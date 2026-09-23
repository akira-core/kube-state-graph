package build

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/kube-state-graph/pkg/clock"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// tracer is obtained from the global provider; it is a no-op until an
// application installs an OpenTelemetry SDK. The instrumentation scope name is
// kept stable ("kube-state-graph") so span dimensions are unchanged.
var tracer = otel.Tracer("kube-state-graph")

// Builder runs the topology + service-graph readers and assembles a
// multi-cluster Graph for one bucketed time window.
type Builder struct {
	q       promql.Querier
	src     promql.QuerierSource
	opts    Options
	metrics Metrics
	clk     clock.Clock
}

// New constructs a Builder. clk may be nil (falls back to clock.System); m may
// be nil (no-op metrics).
//
// When q ALSO satisfies promql.QuerierSource — a *promql.Router does — the
// builder resolves a per-build Querier from it, so the request's `az`
// dimension can select which upstream installations answer. This is an
// OPTIONAL upgrade, deliberately shaped like BuildScopedRouteResolver: a plain
// Querier (a *promql.Client, a mock) leaves src nil and every query is issued
// exactly as it was before backend routing existed.
func New(q promql.Querier, opts Options, m Metrics, clk clock.Clock) *Builder {
	if clk == nil {
		clk = clock.System{}
	}
	b := &Builder{
		q:       q,
		opts:    opts,
		metrics: m,
		clk:     clk,
	}
	if src, ok := q.(promql.QuerierSource); ok && src != nil {
		b.src = src
	}
	return b
}

// querierFor resolves the Querier this build dispatches through. The routing
// snapshot is taken ONCE per build and threaded through every leg — topology,
// service graph, and the retention probe — so a routing-table reload cannot
// change which backends a build in flight reaches, and the build cannot end up
// probing a different set of stores than it read from.
func (b *Builder) querierFor(sel promql.Selector) promql.Querier {
	if b.src == nil {
		return b.q
	}
	return b.src.QuerierFor(sel)
}

// Build runs all upstream queries for [end - window, end] and returns the
// joined multi-cluster Graph.
//
// sel carries the request-scoped selector dimensions (`az`, `env`, `cluster`,
// `namespace`). A zero Selector is the unfiltered build: every query is issued
// exactly as it was before request-scoped selectors existed, and every
// filtered-build rule below stays inert.
func (b *Builder) Build(ctx context.Context, window time.Duration, end time.Time, sel promql.Selector) (*graph.Graph, error) {
	filtered := sel.Active()
	q := b.querierFor(sel)
	ctx, span := tracer.Start(ctx, "kube-state-graph.build",
		trace.WithAttributes(
			attribute.Int64("kube_state_graph.window_seconds", int64(window.Seconds())),
			attribute.Int64("kube_state_graph.end_unix", end.Unix()),
			attribute.Bool("kube_state_graph.selector_active", filtered),
		),
	)
	defer span.End()

	topology, err := ReadTopology(ctx, q, window, end, b.opts, sel)
	if err != nil {
		return nil, classifyReadError(span, "topology read failed", err)
	}

	// Outside-retention check: zero pods + healthy upstream ⇒ retention miss.
	// Only meaningful for an UNFILTERED build. With any selector-level filter
	// active, zero rows means "nothing in scope" — a legitimate empty result,
	// not a client-classifiable retention error — so the classification (and
	// its up{} probe) is skipped entirely.
	if !filtered && len(topology.Pods) == 0 && len(topology.Nodes) == 0 {
		up, probeErr := b.upProbe(ctx, q)
		if probeErr != nil {
			// A failed probe must not fail the build (control flow / status
			// mapping unchanged — that is a spec-level decision), but it must
			// leave a server-side trace: without it a probe error or timeout
			// degrades to a silent 200 empty graph with zero signal.
			slog.WarnContext(ctx, "up probe failed; outside-retention classification skipped",
				"error", probeErr)
		}
		if up {
			startStr := end.Add(-window).UTC().Format(time.RFC3339)
			endStr := end.UTC().Format(time.RFC3339)
			podRaw := topology.RawSeriesCount[string(promql.QPodInfo)]
			nodeRaw := topology.RawSeriesCount[string(promql.QNodeInfo)]
			msg := fmt.Sprintf(
				"no topology rows in window [%s, %s] (window=%s); upstream healthy. "+
					"%s matched %d raw series (parsed to %d pods); "+
					"%s matched %d raw series (parsed to %d nodes) — "+
					"a non-zero raw count with zero parsed means rows were returned but filtered (e.g. empty uid)",
				startStr, endStr, window,
				promql.QPodInfo, podRaw, len(topology.Pods),
				promql.QNodeInfo, nodeRaw, len(topology.Nodes),
			)
			err := NewError(ReasonOutsideRetention, msg, nil)
			// outside_retention maps to HTTP 400 (a client-classifiable no-data
			// condition), so record the event for trace completeness but leave
			// the span status Unset — only 5xx-class failures mark Error.
			span.RecordError(err)
			slog.WarnContext(ctx, "outside_retention",
				"start", startStr,
				"end", endStr,
				"window", window.String(),
				"raw_series_counts", topology.RawSeriesCount,
				"pod_info_query", promql.Render(promql.QPodInfo, window, b.opts.LabelKeys, sel),
				"node_info_query", promql.Render(promql.QNodeInfo, window, b.opts.LabelKeys, sel),
			)
			return nil, err
		}
	}

	// A filtered build that loaded NO topology cannot admit a single
	// service-graph series, so the three traces_service_graph_* queries are
	// skipped entirely. They are the most expensive leg of the fan-out and the
	// one leg no selector narrows (queryDims gives them no dimension), so a
	// mistyped `?namespace=` would otherwise scan the whole estate to build an
	// empty response, on every request, with no cache in front.
	//
	// Provably wasted, not heuristically: admission (design D6) keeps a series
	// only when a resolved endpoint names loaded topology — podByID, built from
	// Pods, or an already-materialised service, which can only come from
	// ServicesByNameNS via anchorHolds. Both empty ⇒ every series is rejected
	// and every side effect rolled back.
	//
	// Gated on `filtered` so the unfiltered empty-topology case stays exactly
	// the outside-retention path above.
	var sg ServiceGraphResult
	if filtered && len(topology.Pods) == 0 && len(topology.ServicesByNameNS) == 0 {
		slog.DebugContext(ctx, "service-graph read skipped: selector matched no topology",
			"reason", "filtered_empty_topology")
	} else {
		sg, err = ReadServiceGraph(ctx, q, window, end, topology,
			b.opts.RouteResolver, b.opts.RouteResolveTimeout, filtered)
		if err != nil {
			return nil, classifyReadError(span, "service-graph read failed", err)
		}
	}

	nodes, edges := assemble(topology, sg)
	// Alerts are baked onto the nodes BEFORE the graph is frozen — the same
	// point PVC Application inheritance uses. It has to run here rather than in
	// the topology parse because matching is against the ASSEMBLED node set,
	// which only exists once the service-graph read has contributed its synth
	// pods; and it has to run in the BUILD rather than in a projection because
	// the attribute must reach every consumer of this graph alike (/v1/graph,
	// /v1/storage-graph, and any embedder walking GraphNode.Alerts()).
	attachAlerts(ctx, nodes, topology)
	attachStatus(nodes)
	g := graph.NewGraph(nodes, edges, b.clk.Now().UTC())
	// The identity table the reader composed. Every cluster-scoped id and label
	// already carries the identity; the graph needs the table so the
	// projection-level `cluster` filter can recover each identity's RAW
	// component — the value the request actually carries and the upstream
	// matcher selected on. Nil for an unstamped estate, which degrades the
	// lookup to the pre-identity comparison.
	g.ClusterIdentities = topology.ClusterIdentities

	// Cross-cluster status is derived from the resolved endpoint nodes'
	// `cluster` labels, since edges only carry the trace-source cluster
	// (Option A: the metric does not stamp server-side cluster; it is
	// recovered via the topology pod-UID index at parse time). Any edge type
	// counts — pod-calls-service edges may cross clusters via the D29
	// cluster-family fan-out. One EdgeCountByType scan feeds both the log/span
	// total (sum of the "true" buckets) and the self-metric gauges.
	edgeCounts := g.EdgeCountByType()
	crossCluster := 0
	for k, n := range edgeCounts {
		if k[1] == "true" {
			crossCluster += n
		}
	}
	leg, legSeries := largestLeg(topology.RawSeriesCount)
	slog.InfoContext(ctx, "graph built",
		"selector_active", filtered,
		"clusters", topology.ClustersObserved,
		"nodes", len(g.NodesByID),
		"edges", len(g.Edges),
		"cross_cluster_edges", crossCluster,
		"largest_leg", leg,
		"largest_leg_series", legSeries,
		"start", end.Add(-window).UTC().Format(time.RFC3339),
		"end", end.UTC().Format(time.RFC3339),
	)
	slog.DebugContext(ctx, "graph built: series per leg", "raw_series_counts", topology.RawSeriesCount)

	// Self-metrics: observational gauges for last build (no-op when unset).
	if b.metrics != nil {
		b.metrics.SetGraphNodeCounts(g.NodeCountByKind())
		b.metrics.SetGraphEdgeCounts(edgeCounts)
		b.metrics.SetClustersObserved(len(topology.ClustersObserved))
	}

	span.SetAttributes(
		attribute.Int("kube_state_graph.cluster_count", len(topology.ClustersObserved)),
		attribute.Int("graph.node.count", len(g.NodesByID)),
		attribute.Int("graph.edge.count", len(g.Edges)),
		attribute.Int("kube_state_graph.cross_cluster_edges", crossCluster),
	)
	return g, nil
}

// BuildStorage builds the storage-flow graph served by GET /v1/storage-graph.
//
// It reads through Build's topology fan-out under the storage plan
// (storagePlan): the five families its body cannot carry — the container list
// and the four service-side families — are never issued, and kube_pod_info /
// kube_pod_owner are read BY REFERENCE, restricted to the pods a claim binding
// names plus the request's pod roots. Every other leg is issued exactly as
// Build issues it, so the two endpoints cannot disagree about what a claim, a
// pod or an aggregate is. It does NOT read the service graph: the storage body
// uses none of it, and those three legs are the most expensive of the fan-out.
//
// It is a pure function of (window, end, Selector, roots), and roots reach
// several reads. The pod names of pod=<ns>/<name> roots join the pod scope, and
// node=<name> roots join the node scope, because a root mounting no claim (or
// naming a node no pod runs on) is drawable only if it is read; those can only
// ADD a name to a scope, never narrow a read. The ontap_cluster=, aggr= and
// svm= roots restrict the Harvest volume-label topology read to the rooted
// components (scope-volume-labels-by-storage-root) — made output-preserving by
// re-reading each touched aggregate whole for its owner vote and each matched
// claim's whole candidate set in a second phase — and put the build in hub
// mode (read-storage-roots-through-volume-hub): the claim families are read
// FROM the rooted rows, keyed by the PersistentVolume and claim names they
// lead to, under the same az / env matchers and the same zone routing as every
// other storage build — a filer shared across zones is drawn with the claims
// of the requested zone only. Either way which paths
// are drawn stays a projection concern (graph.ProjectStorage). This
// revises the storage-graph design's "roots never reach the build" stance
// (harden-topology-read-cardinality D7): v1 has no result cache whose key it
// would widen, and the alternative was reading the whole estate.
//
// There is deliberately no outside-retention classification and no up{} probe.
// The endpoint requires az and env, so every storage build is a FILTERED build,
// and the existing filtered-build rule already says an empty result is an empty
// 200 rather than a retention error — which is also what makes "a root the
// upstream does not name is simply not drawn" true.
//
// It also deliberately does NOT write the three last-build gauges
// (kube_state_graph_graph_nodes / _edges / clusters observed). Those describe
// the graph Build produces — the adapters reset the vector on every write, so
// a storage build would wipe every pod-calls-* / pvc-to-netapp-aggr series and
// replace them with storage-flow until the next /v1/graph request, making the
// gauges a function of request mix rather than of the estate.
func (b *Builder) BuildStorage(ctx context.Context, window time.Duration, end time.Time, sel promql.Selector, roots graph.StorageRoots) (*graph.Graph, error) {
	return b.buildStorage(ctx, window, end, sel, storagePlan(roots))
}

// buildStorage is BuildStorage under an explicit read plan.
//
// The plan is resolved here, before anything is dispatched, because the volume
// hub changes WHICH families the build reads and how they are scoped — never
// where they are read from. Every leg, in hub mode or not, renders the
// request's full selector and is dispatched through the one querier its `az`
// binds, so no query reaches a store of another zone and no series of another
// zone or environment reaches the body.
func (b *Builder) buildStorage(ctx context.Context, window time.Duration, end time.Time, sel promql.Selector, plan topologyPlan) (*graph.Graph, error) {
	plan = plan.resolveVolumeLabelRead(b.opts.volumeKey(), window, b.opts.qosScopeBatchBytes(), b.opts.LabelKeys, sel)
	q := b.querierFor(sel)
	ctx, span := tracer.Start(ctx, "kube-state-graph.build_storage",
		trace.WithAttributes(
			attribute.Int64("kube_state_graph.window_seconds", int64(window.Seconds())),
			attribute.Int64("kube_state_graph.end_unix", end.Unix()),
			attribute.Bool("kube_state_graph.selector_active", sel.Active()),
			attribute.Bool("kube_state_graph.volume_hub", plan.hub),
		),
	)
	defer span.End()

	topology, err := readTopology(ctx, q, window, end, b.opts, sel, plan)
	if err != nil {
		return nil, classifyReadError(span, "topology read failed", err)
	}

	nodes, edges := assembleStorageFlow(topology)
	// Same "bake before freeze" point as Build: the overlay must reach this
	// endpoint identically, since it is resolved onto the graph rather than by
	// a projection.
	attachAlerts(ctx, nodes, topology)
	attachStatus(nodes)
	g := graph.NewGraph(nodes, edges, b.clk.Now().UTC())
	g.ClusterIdentities = topology.ClusterIdentities

	leg, legSeries := largestLeg(topology.RawSeriesCount)
	slog.InfoContext(ctx, "storage graph built",
		"clusters", topology.ClustersObserved,
		"nodes", len(g.NodesByID),
		"edges", len(g.Edges),
		"largest_leg", leg,
		"largest_leg_series", legSeries,
		"start", end.Add(-window).UTC().Format(time.RFC3339),
		"end", end.UTC().Format(time.RFC3339),
		"selector_active", sel.Active(),
		"volume_hub", plan.hub,
	)
	slog.DebugContext(ctx, "storage graph built: series per leg", "raw_series_counts", topology.RawSeriesCount)

	span.SetAttributes(
		attribute.Int("kube_state_graph.cluster_count", len(topology.ClustersObserved)),
		attribute.Int("graph.node.count", len(g.NodesByID)),
		attribute.Int("graph.edge.count", len(g.Edges)),
	)
	return g, nil
}

// classifyReadError maps an upstream read failure to a typed build error and
// records it on the build span. context.Canceled (client disconnect) is NOT a
// server/upstream fault: it is recorded as a span event but does not set the
// span Error status, and downstream maps to a 4xx rather than a 5xx so it does
// not pollute error-rate metrics/traces. DeadlineExceeded (build timeout) and
// any other upstream error are genuine failures and mark the span Error.
func classifyReadError(span trace.Span, what string, err error) error {
	if errors.Is(err, context.Canceled) {
		span.RecordError(err)
		return NewError(ReasonCanceled, "request canceled", err)
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	if errors.Is(err, context.DeadlineExceeded) {
		return NewError(ReasonTimeout, "build timeout", err)
	}
	be := &Error{Reason: ReasonUpstream, Message: what, Err: err}
	if qe, ok := errors.AsType[*QueryError](err); ok {
		be.Query = qe.Query
	}
	return be
}

func assemble(topology Topology, sg ServiceGraphResult) ([]graph.GraphNode, []*graph.Edge) {
	// Nodes: pods + k8s nodes + pvcs + synthesised pods + services + externals.
	// ORDER IS LOAD-BEARING: graph.NewGraph dedupes colliding node IDs
	// keep-first (ServiceID mirrors PVCID keying, so a Service and a PVC
	// sharing (cluster, namespace, name) mint byte-identical IDs), so the
	// authoritative topology nodes MUST be appended before the on-demand
	// service-graph nodes. Reordering these appends silently flips the
	// collision winner — see TestAssemble_TopologyWinsIDCollision.
	total := len(topology.Pods) + len(topology.Nodes) + len(topology.PVCs) +
		len(topology.NetAppAggrs) + len(topology.NetAppNodes) +
		len(sg.SynthPods) + len(sg.ServiceNodes) + len(sg.ExternalNodes)
	nodes := make([]graph.GraphNode, 0, total)
	for _, p := range topology.Pods {
		nodes = append(nodes, p)
	}
	for _, n := range topology.Nodes {
		nodes = append(nodes, n)
	}
	for _, pv := range topology.PVCs {
		nodes = append(nodes, pv)
	}
	for _, a := range topology.NetAppAggrs {
		nodes = append(nodes, a)
	}
	for _, n := range topology.NetAppNodes {
		nodes = append(nodes, n)
	}
	for _, p := range sg.SynthPods {
		nodes = append(nodes, p)
	}
	for _, sv := range sg.ServiceNodes {
		nodes = append(nodes, sv)
	}
	for _, e := range sg.ExternalNodes {
		nodes = append(nodes, e)
	}

	edges := make([]*graph.Edge, 0,
		len(sg.Edges)+len(topology.Pods)+len(topology.PodPVCs))
	edges = append(edges, TopologyEdges(topology)...)
	edges = append(edges, topology.StorageEdges...)
	edges = append(edges, sg.Edges...)
	return nodes, edges
}

// upProbe measures store health through the SAME per-build querier the
// topology read used. Routing it any other way would let the classification
// consult a different set of backends than the build read from — and the probe
// family accepts no request dimension, so it still reaches every backend
// serving it regardless of the request's zones.
func (b *Builder) upProbe(ctx context.Context, q promql.Querier) (bool, error) {
	// Honour the documented contract (Options.APITimeout): zero means inherit
	// the caller's context deadline. context.WithTimeout(ctx, 0) would otherwise
	// produce an immediately-expired context, silently failing the probe (and
	// skipping outside-retention classification) for a zero-value embedder.
	if b.opts.APITimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, b.opts.APITimeout, errProbeTimeout)
		defer cancel()
	}
	vec, err := q.Instant(ctx, string(promql.QUpProbe),
		promql.Render(promql.QUpProbe, 0, promql.LabelKeys{}, promql.Selector{}), b.clk.Now().UTC())
	if err != nil {
		// Name the budget that ran out — the probe's own, or an ancestor's
		// whose cause propagated down — since both surface as DeadlineExceeded.
		if cause := context.Cause(ctx); cause != nil && !errors.Is(err, cause) {
			err = fmt.Errorf("%w: %w", cause, err)
		}
		return false, err
	}
	return len(vec) > 0, nil
}

// errProbeTimeout is the cause stamped on the up{} probe's own context.
var errProbeTimeout = errors.New("up probe exceeded Options.APITimeout")
