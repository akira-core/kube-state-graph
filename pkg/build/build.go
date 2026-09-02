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
	slog.InfoContext(ctx, "graph built",
		"selector_active", filtered,
		"clusters", topology.ClustersObserved,
		"nodes", len(g.NodesByID),
		"edges", len(g.Edges),
		"cross_cluster_edges", crossCluster,
		"start", end.Add(-window).UTC().Format(time.RFC3339),
		"end", end.UTC().Format(time.RFC3339),
	)

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
	return NewError(ReasonUpstream, what, err)
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
		ctx, cancel = context.WithTimeout(ctx, b.opts.APITimeout)
		defer cancel()
	}
	vec, err := q.Instant(ctx, string(promql.QUpProbe),
		promql.Render(promql.QUpProbe, 0, promql.LabelKeys{}, promql.Selector{}), b.clk.Now().UTC())
	if err != nil {
		return false, err
	}
	return len(vec) > 0, nil
}
