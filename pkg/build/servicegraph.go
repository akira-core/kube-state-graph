package build

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// sentinelUnknown is the literal server-label value the D30 query-layer
// exclusion no longer drops on the server side (resolve-unknown-server-peer-labels
// D1). A no-op everywhere except the two resolveServer branches that route it
// to resolveUnknownServerPeer instead of the generic empty-UID / synth-pod
// fallbacks.
const sentinelUnknown = "unknown"

// Span-link relation marking (add-span-link-logical-edges): a series whose
// edge_relation label is exactly relationLink is a logical producer→consumer
// edge derived from cross-trace span links through a broker. Its emitted edge
// carries labels[relationLabelKey]=relationLink, and the pod→broker network
// edges evidenced by each side's own peer-address labels are marked
// relationTransport. Any other edge_relation value is ignored (exact match).
const (
	relationLabelKey  = "relation"
	relationLink      = "link"
	relationTransport = "transport"
)

// ServiceGraphResult is the typed output of the pod-service-graph reader.
type ServiceGraphResult struct {
	Edges         []*graph.Edge
	ServiceNodes  []*graph.ServiceNode
	ExternalNodes []*graph.ExternalNode
	SynthPods     []*graph.PodNode
}

// ReadServiceGraph fetches service-graph series for the window and joins each
// endpoint against the supplied topology. Per D29, endpoints whose client /
// server label is a "://" connection string are resolved to in-cluster
// service nodes (which fan out service-selects-pod edges to their backing
// pods), falling back to an external node — there is no configurable pattern
// knob. The Renderer is accepted for signature symmetry
// with ReadTopology; the metric-name prefix is NOT applied to
// traces_service_graph_request_total (different exporter family, design.md
// D26), so r is effectively a no-op here today.
//
// resolver is the optional Istio route-resolution engine
// (translate-global-fqdn-to-k8s-service): when non-nil, a pure prescan
// collects the unknown-server endpoints that would fall to external nodes,
// resolves them here — where ctx and the window exist — and hands the answers
// to the parse as a prefetched index, keeping parseServiceGraphRoutes free of
// I/O (design D2). resolveTimeout bounds each engine call (zero = ctx only).
// A nil resolver skips the prescan entirely: pre-change behaviour.
func ReadServiceGraph(
	ctx context.Context,
	q promql.Querier,
	r promql.Renderer,
	window time.Duration,
	end time.Time,
	topology Topology,
	resolver RouteResolver,
	resolveTimeout time.Duration,
) (ServiceGraphResult, error) {
	// Fan the three service-graph queries out under an errgroup (design D3).
	// The total query's error still fails the build; the two OPTIONAL RED
	// queries record their error and yield a nil vector so the parse can
	// degrade field-by-field without failing the request.
	var (
		vec      model.Vector
		totalErr error
		failed   model.Vector
		failErr  error
		duration model.Vector
		durErr   error
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		out, err := q.Instant(gctx,
			string(promql.QServiceGraphTotal),
			r.Render(promql.QServiceGraphTotal, window),
			end,
		)
		vec, totalErr = out, err
		return err // only the total query's error fails the group
	})
	g.Go(func() error {
		out, err := q.Instant(gctx,
			string(promql.QServiceGraphFailedTotal),
			r.Render(promql.QServiceGraphFailedTotal, window),
			end,
		)
		failed, failErr = out, err
		return optionalQueryFatal(ctx, err) // non-fatal except on caller cancellation
	})
	g.Go(func() error {
		out, err := q.Instant(gctx,
			string(promql.QServiceGraphServerSecondsBucket),
			r.Render(promql.QServiceGraphServerSecondsBucket, window),
			end,
		)
		duration, durErr = out, err
		return optionalQueryFatal(ctx, err) // non-fatal except on caller cancellation
	})
	if err := g.Wait(); err != nil {
		// Prefer the total-query error message when present.
		if totalErr != nil {
			return ServiceGraphResult{}, fmt.Errorf("service-graph query: %w", totalErr)
		}
		return ServiceGraphResult{}, fmt.Errorf("service-graph query: %w", err)
	}

	// One aggregated degradation line naming which optional RED query failed
	// and why — never per edge (design D3 / graceful degradation).
	if failErr != nil || durErr != nil {
		attrs := []any{}
		if failErr != nil {
			attrs = append(attrs, "failed_total_error", failErr.Error())
		}
		if durErr != nil {
			attrs = append(attrs, "server_seconds_bucket_error", durErr.Error())
		}
		slog.Warn("service-graph RED query degraded; metrics will be partial", attrs...)
	}

	// ONE resolver per build, shared by the prescan and the parse. Its topology
	// indexes are immutable and building them scans every pod and Service across
	// every cluster (with per-family sorts), so building them twice per request
	// was pure duplicated work — and the prescan consults them read-only.
	res := newSGResolver(topology)

	var routes routeIndex
	if resolver != nil {
		// Range in, instant out: the PromQL side keeps (window, end), the route
		// engine gets ONLY end — the single instant it evaluates the ingress
		// config at (simplify-route-resolution-to-point-in-time D1).
		routes = resolveRouteQueries(ctx, resolver, resolveTimeout,
			collectRouteQueriesWith(vec, res), end)
	}
	return parseWithResolver(vec, res, routes, redInputs{
		Failed:      failed,
		FailedErr:   failErr,
		Duration:    duration,
		DurationErr: durErr,
	}), nil
}

// optionalQueryFatal decides whether an OPTIONAL RED query's error should fail
// the build. The metric's own absence or upstream rejection never does — that
// is the graceful-degradation contract. A failure caused by the CALLER's own
// context (build timeout / client disconnect) MUST still surface: swallowing it
// would let a request that blew --build-timeout return 200 with silently
// degraded metrics instead of the 504 the timeout contract promises. gctx's own
// cancellation (triggered by the total query failing) is deliberately not
// fatal here — the total query's error is the one that gets reported.
func optionalQueryFatal(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return err
	}
	return nil
}

// sgResolver carries the per-build dedupe maps and topology indexes used to
// resolve service-graph endpoints into graph nodes. Service nodes and
// service-selects-pod edges are materialised on demand (only for services a
// "://" connection string actually references) and deduped here.
type sgResolver struct {
	endpointsByService map[serviceKey][]EndpointObs  // service-selects-pod fan-out source
	podByID            map[string]*graph.PodNode     // client side: cluster known from metric
	podByUID           map[string]*graph.PodNode     // server side: cluster recovered via index
	svcCandidates      map[famSvcKey][]svcCandidate  // family → clusters holding (ns, svc), sorted by cluster
	ipIndex            map[ipKey]serviceKey          // (cluster, ClusterIP) → Service deployed there (resolve-unknown-server-ip-peer)
	podIPCandidates    map[famIPKey][]podIPCandidate // family → clusters holding a Pod IP, sorted by cluster (resolve-unknown-server-pod-ip-peer)
	serviceApps        map[serviceKey]string         // (cluster, namespace, service) → ArgoCD Application
	routes             routeIndex                    // prefetched route-engine answers (nil = engine off; non-nil-but-empty = engine on, nothing collected)
	externals          map[string]*graph.ExternalNode
	synthPods          map[string]*graph.PodNode
	services           map[string]*graph.ServiceNode // keyed by service id
	svcEdges           map[string]*graph.Edge        // service-selects-pod, keyed by "svcID|podID"
	routeChainEdges    map[string]routeChainEdge     // synthesized ingress-pod → backend-service pod-calls-service, keyed "srcPodID|backendSvcID"

	// Debug-only evidence accumulators (no effect on the emitted graph). Counted
	// while resolving each endpoint and surfaced as an aggregated summary at the
	// end of parseServiceGraph; per-endpoint detail is emitted at slog.Debug.
	extReasons map[string]int // external-fallback count keyed by reason
	shadowed   int            // "://" labels skipped because a UID was populated
}

// sgTrace carries the identity of the service-graph series currently being
// resolved, threaded into the resolver purely so the debug logs can name WHICH
// upstream metric fell back (and why). It never influences resolution.
type sgTrace struct {
	side                               string // "client" | "server" — which endpoint is being resolved
	label                              string // this side's raw client/server label
	clientLabel, serverLabel           string
	clientUID, serverUID, traceCluster string
}

// noteExternal records (for the aggregated summary) and logs one endpoint that
// fell back to an external node, tagged with the precise reason and the full
// series identity so an operator can grep the offending metric out of the logs.
func (r *sgResolver) noteExternal(reason string, t sgTrace, attrs ...any) {
	r.extReasons[reason]++
	args := append([]any{
		"reason", reason,
		"side", t.side,
		"label", t.label,
		"client", t.clientLabel,
		"server", t.serverLabel,
		"client_uid", t.clientUID,
		"server_uid", t.serverUID,
		"trace_cluster", t.traceCluster,
	}, attrs...)
	slog.Debug("service-graph endpoint fell back to external", args...)
}

// famSvcKey keys the D29 candidate index: the service identity a "://" host
// classifies to, scoped by cluster family. Folding the family into the key
// makes per-endpoint resolution a direct map hit and keeps the family rule
// encoded in exactly one place (the index build). The candidate slice serves
// two roles in resolveServiceLevel: it locates the anchor cluster's own service
// node, and its members' endpoints are unioned for the cross-cluster
// service-selects-pod fan-out.
type famSvcKey struct{ family, namespace, service string }

// svcCandidate is one cluster's deployment of a (namespace, service).
type svcCandidate struct {
	cluster string
	obs     ServiceObs
}

// ipKey keys the resolve-unknown-server-ip-peer reverse index: a Service's
// ClusterIP, scoped to a single cluster. Deliberately NOT family-scoped like
// famSvcKey — a ClusterIP is a per-cluster address assigned from that
// cluster's own (often overlapping) Service CIDR, so matching it across
// clusters risks resolving to the wrong Service. Worse than an overlap: under
// Istio multi-primary the SAME (namespace, service) exists in every cluster
// with a DIFFERENT ClusterIP, so a sibling's ClusterIP identifies a different
// Service instance than the one the caller addressed. The identification
// lookup stays anchor-cluster-only; once a (namespace, service) is identified,
// the existing family-wide resolveServiceLevel still governs the
// service-selects-pod fan-out.
//
// The same key type also stages the per-cluster winner of the Pod IP index
// (see famIPKey), which is grouped into family candidates afterwards.
type ipKey struct{ cluster, ip string }

// famIPKey keys the resolve-unknown-server-pod-ip-peer index: a Pod IP scoped
// by cluster family, mirroring famSvcKey. Unlike a ClusterIP (see ipKey), a
// Pod IP identifies a real, singular workload wherever it is reachable from —
// cross-cluster pod-to-pod dialling is a NETWORK-layer property (a flat
// routable plane: shared VPC, VPC peering, BGP/native-routing CNI, L3-routed
// on-prem pod CIDRs), independent of any service mesh. Sidecar presence is
// neither necessary (a flat network needs no Istio) nor sufficient (in a
// multi-network mesh the caller's sidecar sees the east-west gateway address,
// never a remote Pod IP), so it is not used as a gate.
//
// The safety condition is that the family's pod CIDRs do not overlap, and that
// is directly observable rather than assumed: if they overlapped, the same IP
// would appear in more than one family cluster. lookupPeerPodIP therefore
// prefers the anchor cluster, accepts a lone family holder, and degrades to
// external on two or more — the uniqueness test IS the CIDR-disjointness test.
type famIPKey struct{ family, ip string }

// podIPCandidate is one cluster's holder of a Pod IP (already reduced to that
// cluster's single winner — see the two-stage build in newSGResolver).
type podIPCandidate struct {
	cluster string
	pod     *graph.PodNode
}

// parseServiceGraph is the route-index-free form used by the ~70 direct test
// call sites. Identical to parseServiceGraphRoutes with a nil index.
//
// NOT a production entry point — ReadServiceGraph always goes through
// parseWithResolver so it can hand over the real RED inputs. This form passes a
// zero redInputs, which by that type's contract reads as "both OPTIONAL queries
// succeeded and returned nothing", so qualifying edges get rate + error_rate=0
// and no p90_server_ms. Any future production caller MUST pass real inputs
// instead, or it will fabricate an error_rate=0 no query ever measured.
func parseServiceGraph(vec model.Vector, topology Topology) ServiceGraphResult {
	return parseServiceGraphRoutes(vec, topology, nil)
}

// newSGResolver builds the per-parse resolver: the immutable topology indexes
// every resolution path consults. Shared by parseServiceGraphRoutes and the
// collectRouteQueries prescan so both classify endpoints over identical
// indexes (design D2).
func newSGResolver(topology Topology) *sgResolver {
	// Reverse Pod IP index for the resolve-unknown-server-pod-ip-peer
	// classification step, built in two stages so the intra-cluster and
	// cross-cluster rules stay separable.
	//
	// Stage 1, in the same pass as podByID: (cluster, pod_ip) -> that cluster's
	// single holder of the address. A pod with no known IP never participates.
	// On a duplicate within ONE cluster — legitimate for hostNetwork pods, which
	// all report their node's address, and transient when an address is
	// reassigned inside the query window — the lexically-smallest pod ID wins.
	podByID := make(map[string]*graph.PodNode, len(topology.Pods))
	perCluster := make(map[ipKey]*graph.PodNode, len(topology.Pods))
	for _, p := range topology.Pods {
		podByID[p.ID()] = p

		ips := p.IPAddress()
		if len(ips) == 0 || ips[0] == "" {
			continue
		}
		// Index the CANONICAL textual form so an IPv6 peer address written
		// differently from the exporter's pod_ip label (case, zero-run
		// compression, IPv4-mapped prefix) still matches — lookupPeerPodIP
		// canonicalises its host the same way. IPv4 is unchanged by this.
		ik := ipKey{cluster: p.Labels()["cluster"], ip: canonicalIP(ips[0])}
		if existing, ok := perCluster[ik]; ok && existing.ID() <= p.ID() {
			continue
		}
		perCluster[ik] = p
	}

	// Stage 2: group the per-cluster winners by (family, ip), sorted by cluster
	// — the same shape as svcCandidates below, so both indexes are pure
	// functions of the data rather than of map-iteration order (D6). The slice
	// length is what lookupPeerPodIP reads as evidence of pod-CIDR overlap
	// within the family.
	podIPCandidates := make(map[famIPKey][]podIPCandidate, len(perCluster))
	for ik, pod := range perCluster {
		fk := famIPKey{family: ClusterFamilyKey(ik.cluster), ip: ik.ip}
		podIPCandidates[fk] = append(podIPCandidates[fk], podIPCandidate{cluster: ik.cluster, pod: pod})
	}
	for _, cands := range podIPCandidates {
		sort.Slice(cands, func(i, j int) bool { return cands[i].cluster < cands[j].cluster })
	}

	// Inverted index for D29 connection-string resolution: (family, namespace,
	// service) → that family's clusters deploying the service, sorted by cluster
	// so resolution is a pure function of the data, independent of map-iteration
	// order (D6). Built once per parse; per-"://"-endpoint resolution is then a
	// single map hit. The candidate slice serves two roles in resolveServiceLevel:
	// it locates the anchor (caller) cluster's own service node, and its members'
	// endpoints are unioned for the cross-cluster service-selects-pod fan-out.
	// "unknown"-bucketed service entries (samples missing their cluster label)
	// land under family "unknown" — its own family-of-one — so they are reachable
	// only by an "unknown"-anchored caller and never unioned into a real cluster's
	// fan-out (ClusterFamilyKey("unknown") == "unknown" != "prod-0").
	svcCandidates := make(map[famSvcKey][]svcCandidate, len(topology.ServicesByNameNS))
	for k, obs := range topology.ServicesByNameNS {
		key := famSvcKey{family: ClusterFamilyKey(k.cluster), namespace: k.namespace, service: k.service}
		svcCandidates[key] = append(svcCandidates[key], svcCandidate{cluster: k.cluster, obs: obs})
	}
	for _, cands := range svcCandidates {
		sort.Slice(cands, func(i, j int) bool { return cands[i].cluster < cands[j].cluster })
	}

	// Reverse ClusterIP index for the resolve-unknown-server-ip-peer bare
	// IP-literal classification step: (cluster, ClusterIP) -> the Service
	// deployed at that address in that cluster. Skips headless ("None") and
	// empty ClusterIP values — neither is a matchable literal. On a same-
	// cluster duplicate ClusterIP (a data anomaly Kubernetes itself prevents,
	// but the index build stays defensive), the lexically-smaller
	// (namespace, service) wins — deterministic, independent of map-
	// iteration order (D6).
	ipIndex := make(map[ipKey]serviceKey, len(topology.ServicesByNameNS))
	for k, obs := range topology.ServicesByNameNS {
		if obs.ClusterIP == "" || obs.ClusterIP == "None" {
			continue
		}
		// Canonical textual form, symmetric with the Pod-IP index above and
		// with classifyPeerHost's lookup: an IPv6 ClusterIP written differently
		// from the peer-address label would otherwise miss. IPv4 is unchanged.
		ik := ipKey{cluster: k.cluster, ip: canonicalIP(obs.ClusterIP)}
		if existing, ok := ipIndex[ik]; ok {
			if k.namespace > existing.namespace ||
				(k.namespace == existing.namespace && k.service >= existing.service) {
				continue
			}
		}
		ipIndex[ik] = k
	}

	return &sgResolver{
		endpointsByService: topology.EndpointsByService,
		podByID:            podByID,
		podByUID:           topology.PodsByUID,
		svcCandidates:      svcCandidates,
		ipIndex:            ipIndex,
		podIPCandidates:    podIPCandidates,
		serviceApps:        topology.ServiceApplications,
		externals:          map[string]*graph.ExternalNode{},
		synthPods:          map[string]*graph.PodNode{},
		services:           map[string]*graph.ServiceNode{},
		svcEdges:           map[string]*graph.Edge{},
		routeChainEdges:    map[string]routeChainEdge{},
		extReasons:         map[string]int{},
	}
}

// parseServiceGraphRoutes is parseServiceGraph plus a prefetched route-engine
// index (nil = engine off). The index is consulted only inside
// resolveUnknownServerPeer, at the points that would otherwise emit an
// external node; resolution stays a pure function of (vec, topology, routes) —
// all I/O happened before this call (design D2).
//
// It builds its own resolver, which is what the direct test call sites want.
// ReadServiceGraph goes through parseWithResolver instead, sharing one resolver
// with the prescan.
func parseServiceGraphRoutes(vec model.Vector, topology Topology, routes routeIndex) ServiceGraphResult {
	if len(vec) == 0 {
		return ServiceGraphResult{}
	}
	return parseWithResolver(vec, newSGResolver(topology), routes, redInputs{})
}

// parseWithResolver is the parse body over an already-built resolver.
// red carries the two OPTIONAL RED query results (may be zero-value).
func parseWithResolver(vec model.Vector, res *sgResolver, routes routeIndex, red redInputs) ServiceGraphResult {
	if len(vec) == 0 {
		return ServiceGraphResult{}
	}

	// Per-metric tally of samples missing the `cluster` label; surfaced as one
	// aggregated warn at the end of the parse.
	mc := missingClusterCounts{}

	res.routes = routes

	// Dedup pod-calls-pod by (srcID, tgtID). Multiple upstream series can
	// resolve to the same edge identity — most commonly when `connection_type`
	// differs — and edge IDs are deterministic only by (type, source, target).
	type aggEdge struct {
		srcIsPod   bool
		srcCluster string
	}
	pairs := make(map[pairKey]aggEdge, len(vec))
	// RED join state: series→pair and redKey→pair maps built during the
	// resolution loop; attach pass runs after the loop (design D1/D4).
	// A join whose vector is absent (query errored, or the deployment does not
	// expose the OPTIONAL metric — the common case) is switched off entirely so
	// the loop never pays for a key it cannot use.
	joinFailures := red.FailedErr == nil && len(red.Failed) > 0
	joinDuration := red.DurationErr == nil && len(red.Duration) > 0
	rj := newRedJoin(len(vec), joinFailures, joinDuration)

	// Span-link relation marking (add-span-link-logical-edges D1): two
	// function-local pair sets consulted at edge-build time — linkPairs marks
	// an edge labels.relation="link", transportPairs marks "transport", link
	// wins on collision. Accumulation is insert-only (monotone), so the final
	// marking is a pure function of the accumulated sets regardless of sample
	// arrival order (D6). Function-local like `pairs`: no resolver field, no
	// cross-build state.
	linkPairs := map[pairKey]struct{}{}
	transportPairs := map[pairKey]struct{}{}
	linkNoConsumer := 0 // link series whose consumer was unresolvable — contributed no markers (aggregated debug)

	for _, s := range vec {
		// Drop zero-rate series. Written as !(v > 0) rather than v <= 0 so
		// NaN-valued samples are dropped too — every comparison with NaN is
		// false in Go, so `s.Value <= 0` would let NaN through and materialise
		// nodes/edges for traffic that never happened.
		if !(s.Value > 0) {
			continue
		}

		clientLabel := string(s.Metric["client"])
		serverLabel := string(s.Metric["server"])
		// Single `cluster` label = trace source / client-side cluster.
		traceCluster := mc.bucket(promql.QServiceGraphTotal, string(s.Metric["cluster"]))
		clientUID := string(s.Metric["client_k8s_pod_uid"])
		serverUID := string(s.Metric["server_k8s_pod_uid"])
		clientNS := string(s.Metric["client_k8s_namespace_name"])
		serverNS := string(s.Metric["server_k8s_namespace_name"])
		// Peer dimensions recorded on the CLIENT span — the three peer-address
		// labels (client_server_address / client_network_peer_address /
		// client_net_peer_name; distinct OTel attributes, not three spellings
		// of one — see peerLabels.value) plus the optional client_dns_answers /
		// client_server_port / client_net_peer_port added for route resolution.
		// client_network_peer_port is deliberately NOT read (design.md
		// Non-Goals; do not add it back "for symmetry"). Consulted only when
		// server=="unknown" and the client resolves to a real pod — see
		// resolveUnknownServerPeer.
		peer := peerLabelsOf(s.Metric)

		// seriesKey is a pure function of the RAW series labels (D4): both
		// companion vectors join to it by exact identity, the histogram after
		// dropping `le`. Computed only when at least one of them is present —
		// seriesKeyOf sorts and concatenates every label of every series, so it
		// is not something to pay for when there is nothing to join against.
		var sk string
		if rj.joinKeyed() {
			sk = seriesKeyOf(s.Metric)
		}
		// A span-link series still produces its edge but measures nothing (D1b).
		inScope := redSeriesInScope(s.Metric)

		clientUID, serverUID = normalizeSelfLoopUIDs(clientUID, serverUID, clientLabel, serverLabel)

		// Drop the series BEFORE any resolution when either side is wholly
		// empty (no UID, no label): no edge can exist, and resolving the
		// other side anyway would leak its materialisation side effects
		// (service / external nodes plus service-selects-pod fan-out) as an
		// orphan subgraph — including the cross-cluster service-selects-pod fan-out.
		if (clientUID == "" && clientLabel == "") || (serverUID == "" && serverLabel == "") {
			slog.Debug("service-graph series dropped (one side wholly empty: no UID and no label)",
				"client", clientLabel, "server", serverLabel,
				"client_uid", clientUID, "server_uid", serverUID, "trace_cluster", traceCluster)
			continue
		}

		// Series identity threaded into the resolver for debug logging only.
		base := sgTrace{
			clientLabel: clientLabel, serverLabel: serverLabel,
			clientUID: clientUID, serverUID: serverUID, traceCluster: traceCluster,
		}
		ctClient := base
		ctClient.side, ctClient.label = "client", clientLabel
		ctServer := base
		ctServer.side, ctServer.label = "server", serverLabel

		// Each side resolves to a (possibly empty) slice of node IDs. With the
		// localised model a "://" endpoint resolves to AT MOST ONE service node —
		// in the caller's own (anchor) cluster — or to a single external node.
		// Almost every path yields exactly one ID; the one exception is a
		// chained RouteHit, which returns [ingress, backend] so the caller keeps
		// its direct dependency alongside the entry-point hop (see the two-target
		// case below). An empty slice drops
		// the side (and with it the series — the cross product below is empty).
		srcIDs, srcIsPod := res.resolveClient(clientLabel, traceCluster, clientUID, clientNS, ctClient)

		// Anchor cluster for the server-side "://" path prefers the UID-recovered
		// client-pod cluster over the raw trace label: the label is frequently
		// missing (bucketed to "unknown") or disagrees with topology (see
		// resolveClient), and `.svc` DNS is resolved relative to the CALLER —
		// whose authoritative cluster is the resolved pod's, not the label's.
		// Falls back to the trace label when the client side is not a topology
		// pod. The "://" then resolves to a single service node in THIS cluster
		// (iff it holds the service), per the same-cluster rule. Edge
		// labels.cluster is unaffected (still the raw trace label, per D9).
		//
		// clientPod is also the trigger signal for the unknown-server peer-label
		// enrichment: it is non-nil ONLY when the client side resolved to a REAL
		// topology pod (res.podByID never indexes a synthesised pod), matching
		// this requirement's "not a synthesised pod" trigger condition.
		anchorCluster := traceCluster
		var clientPod *graph.PodNode
		if srcIsPod && len(srcIDs) == 1 {
			if pod, ok := res.podByID[srcIDs[0]]; ok {
				clientPod = pod
				if c := pod.Labels()["cluster"]; c != "" {
					anchorCluster = c
				}
			}
		}
		tgtIDs := res.resolveServer(serverLabel, anchorCluster, serverUID, serverNS, ctServer, clientPod, peer)
		// Index of the RouteHit ingress-chain ENTRY hop within tgtIDs, or -1.
		// Its edge is trace-derived and pod/service on both ends, but it is a
		// second projection of the SAME call as the retained caller→backend
		// edge, so only the backend carries the measurement (design D1).
		chainEntry := chainEntryIndex(tgtIDs)

		// Cross product: any resolved source × any resolved target. Each "://"
		// side now resolves to at most one (local) service node, so a both-"://"
		// series yields a single intra-cluster edge in the anchor cluster. The
		// one two-target case is a chained RouteHit (route-hit-ingress-chain
		// D5): the server side resolves to [ingress service, backend service],
		// yielding both the caller→ingress and the direct caller→backend edge.
		for _, srcID := range srcIDs {
			for ti, tgtID := range tgtIDs {
				// Deterministic dedupe: multiple upstream series can resolve to the
				// same (src, tgt) pair while carrying different trace `cluster`
				// labels (e.g. one missing → "unknown", the client pod recovered via
				// the cluster-agnostic UID index). betterSrcCluster picks the most
				// informative label deterministically so the emitted edge's
				// labels.cluster is a pure function of the data, not vector arrival
				// order (D6). srcIsPod is identical for a given srcID, so only
				// srcCluster needs the tie-break.
				key := pairKey{src: srcID, tgt: tgtID}
				if prev, dup := pairs[key]; !dup || betterSrcCluster(traceCluster, prev.srcCluster) {
					pairs[key] = aggEdge{srcIsPod: srcIsPod, srcCluster: traceCluster}
				}
				// Record RED join keys for every pair this series produced.
				//
				// The structural verdict is the RESOLVED endpoint types plus the
				// chain position (design D1) — never the raw UID labels and never
				// the edge type. Neither of those holds: D33's self-loop
				// normalisation clears the "://" side's UID, so that side can
				// resolve to a service or an external node while the raw UIDs
				// still look fully pod-resolved, and an external target leaves
				// the edge type at pod-calls-pod (only a service target
				// downgrades it), so the type check cannot catch it either.
				rj.recordSeries(key,
					ti != chainEntry && res.isPodOrServiceID(srcID) && res.isPodOrServiceID(tgtID),
					inScope, float64(s.Value), sk)
			}
		}

		// Span-link handling (add-span-link-logical-edges). Exact-match on
		// "link": any other edge_relation value is an ordinary series.
		if string(s.Metric["edge_relation"]) == relationLink {
			// No-consumer guard (design D6): when the server side has no
			// resolvable topology pod AND its raw label is the "unknown"
			// sentinel, the pairs above came out of resolveUnknownServerPeer —
			// the client-side peer labels resolved the BROKER, so the produced
			// edge is producer→broker with no renderable consumer. Such a
			// series contributes NO markers at all: not link (the target is
			// the broker, not the consumer), and not transport either — the
			// frontend contract is "transport = the network hop backing a
			// rendered logical edge", so dashing a network edge that backs no
			// emitted link edge would misrepresent a real dependency. The edge
			// itself stays byte-identical to an ordinary server="unknown"
			// series' outcome. Every other link sample — real pod, synth pod,
			// D27 ghost external — keeps the logical claim and marks its via
			// hops.
			_, serverKnown := res.podByUID[serverUID]
			if serverLabel == sentinelUnknown && (serverUID == "" || !serverKnown) {
				if len(srcIDs) > 0 && len(tgtIDs) > 0 {
					linkNoConsumer++
				}
			} else {
				for _, srcID := range srcIDs {
					for _, tgtID := range tgtIDs {
						linkPairs[pairKey{src: srcID, tgt: tgtID}] = struct{}{}
					}
				}
				// Per-side via marking (design D3/D6): each side whose own pod
				// resolved to a REAL topology pod derives its broker node ID
				// from its OWN peer labels — lookup-only, never materialising —
				// and the (pod, broker) pair is marked transport. An unresolved
				// side simply contributes no marker; the other side is
				// unaffected. Because this branch always records the series'
				// link pair above, a transport marker can only ever originate
				// from a series that also emitted its link-marked logical edge.
				if clientPod != nil {
					if viaID, ok := res.viaNodeID(clientPod, peer); ok {
						transportPairs[pairKey{src: clientPod.ID(), tgt: viaID}] = struct{}{}
					}
				}
				if serverUID != "" && serverKnown {
					serverPod := res.podByUID[serverUID]
					if viaID, ok := res.viaNodeID(serverPod, serverPeerLabelsOf(s.Metric)); ok {
						transportPairs[pairKey{src: serverPod.ID(), tgt: viaID}] = struct{}{}
					}
				}
			}
		}
	}

	// Second pass: join failure rates and duration buckets onto pairs (D4).
	if joinFailures {
		matched, unmatched := rj.accumulateFailures(red.Failed)
		// The failure counter joins the request counter by EXACT series
		// identity. If the two metrics ever disagree on their label set (an
		// extra dimension, a relabel applied to only one of them), NOTHING
		// joins and every edge reports error_rate=0 — indistinguishable from
		// "no failures", which is precisely the conflation the contract
		// forbids. Surface that signature instead of letting it read clean.
		switch {
		case matched == 0 && unmatched > 0:
			slog.Warn("service-graph failure counter joined no request series; error_rate reads 0 on every edge",
				"reason", "failed_total_label_set_mismatch",
				"unmatched_series", unmatched)
		case unmatched > 0:
			slog.Debug("service-graph failure series with no matching request series",
				"unmatched_series", unmatched, "matched_series", matched)
		}
	}
	if joinDuration {
		matched, unmatched, sawNoLe := rj.accumulateDuration(red.Duration)
		// Same failure mode as the failure counter, different symptom: the
		// histogram joins by exact identity minus `le`, so a label-set drift
		// between _total and _bucket matches nothing and omits p90_server_ms on
		// every edge — indistinguishable from a producer that emits no
		// histogram at all unless it is surfaced.
		switch {
		case matched == 0 && unmatched > 0:
			slog.Warn("service-graph duration histogram joined no request series; p90_server_ms omitted on every edge",
				"reason", "server_seconds_bucket_label_set_mismatch",
				"unmatched_series", unmatched)
		case unmatched > 0:
			slog.Debug("service-graph duration series with no matching request series",
				"unmatched_series", unmatched, "matched_series", matched)
		}
		if sawNoLe {
			slog.Warn("service-graph RED duration series carried no usable classic le buckets; p90_server_ms omitted",
				"reason", "no_classic_le_buckets")
		}
	}

	// error_rate is emitted ONLY when the failure counter was actually read:
	// the query succeeded AND returned at least one series. An empty result is
	// "the metric does not exist upstream", which the spec's *RED graceful
	// degradation* requirement puts on the same footing as a query error —
	// `error_rate` is OMITTED, never reported as 0, so an absent measurement is
	// never presented as a measured absence of errors. `error_rate == 0` is
	// reserved for the case the spec defines it for: the counter was read and
	// no series matched THIS edge.
	failedOK := joinFailures
	durationOK := red.DurationErr == nil

	edges := make([]*graph.Edge, 0, len(pairs)+len(res.svcEdges))
	for k, agg := range pairs {
		// Edge `cluster` label is the trace-source / client-side cluster, but
		// only when the client side is a pod (per design D9). A client "://"
		// label resolves to a service or external node (never a pod), so such
		// an edge never carries cluster.
		labels := map[string]string{}
		if agg.srcIsPod {
			labels["cluster"] = agg.srcCluster
		}
		// Span-link relation marking: link wins over transport on collision
		// (checked first), transport otherwise, no key on ordinary edges.
		// Pure set-membership — labels never feed the UUIDv5 edge identity.
		if _, isLink := linkPairs[k]; isLink {
			labels[relationLabelKey] = relationLink
		} else if _, isTransport := transportPairs[k]; isTransport {
			labels[relationLabelKey] = relationTransport
		}
		// Edge type is target-driven: a target that resolved to a service node
		// (via the D29 "://" connection-string rule) yields pod-calls-service;
		// every other target (pod, synth-pod, external) stays pod-calls-pod.
		edgeType := graph.EdgeTypePodCallsPod
		if _, isSvc := res.services[k.tgt]; isSvc {
			edgeType = graph.EdgeTypePodCallsService
		}
		e := graph.NewEdge(edgeType, k.src, k.tgt, labels)
		// RED attach (design D1). Every pair in this map is trace-derived by
		// construction; the accumulator carries the rest of the rule — an
		// external endpoint, the ingress chain's entry hop, and an all-span-link
		// pair are all ineligible there, so no edge-type gate is needed and
		// pod-calls-service edges are measured like any other. The two
		// synthesised edge sets (svcEdges, routeChainEdges) never pass through
		// here at all.
		if m, ok := rj.acc[k].attachMetrics(failedOK, durationOK); ok {
			e = e.WithMetrics(m)
		}
		edges = append(edges, e)
	}
	// Aggregated span-link evidence (per-endpoint detail would be noise): a
	// transport pair with no matching emitted edge is a pure marker — the
	// network series for the pod→broker hop was absent from the window — and
	// is deliberately NOT synthesised (design D3). linkPairs need no check:
	// every link pair was inserted alongside its `pairs` entry.
	if unmatchedTransport := func() int {
		n := 0
		for k := range transportPairs {
			if _, ok := pairs[k]; !ok {
				n++
			}
		}
		return n
	}(); unmatchedTransport > 0 || linkNoConsumer > 0 {
		slog.Debug("span-link relation marking summary",
			"unmatched_transport_pairs", unmatchedTransport,
			"link_series_without_resolvable_consumer", linkNoConsumer)
	}
	for _, e := range res.svcEdges {
		// service-selects-pod edges never carry metrics (synthesised fan-out).
		edges = append(edges, e)
	}
	// Synthesized RouteHit ingress-chain edges (ingress pod → backend service,
	// route-hit-ingress-chain D4). A trace-derived edge for the same (src, tgt)
	// wins — the two would otherwise share one deterministic UUIDv5 edge ID —
	// and skipping keeps pre-existing traced edges byte-identical. Map
	// iteration order is irrelevant: the emitted SET is a pure function of the
	// data, and SortEdges canonicalises downstream (D6).
	for _, ce := range res.routeChainEdges {
		if _, dup := pairs[pairKey{src: ce.src, tgt: ce.tgt}]; dup {
			continue
		}
		edges = append(edges, graph.NewEdge(graph.EdgeTypePodCallsService, ce.src, ce.tgt,
			map[string]string{"cluster": ce.cluster}))
	}

	out := ServiceGraphResult{
		Edges:         edges,
		ServiceNodes:  make([]*graph.ServiceNode, 0, len(res.services)),
		ExternalNodes: make([]*graph.ExternalNode, 0, len(res.externals)),
		SynthPods:     make([]*graph.PodNode, 0, len(res.synthPods)),
	}
	for _, sv := range res.services {
		out.ServiceNodes = append(out.ServiceNodes, sv)
	}
	for _, ext := range res.externals {
		out.ExternalNodes = append(out.ExternalNodes, ext)
	}
	for _, sp := range res.synthPods {
		out.SynthPods = append(out.SynthPods, sp)
	}

	mc.warn()

	// Aggregated, low-volume evidence headline (per-endpoint detail is at Debug).
	// Emitted only when something actually fell back, so a clean parse stays quiet.
	if len(res.externals) > 0 || res.shadowed > 0 {
		slog.Info("service-graph resolution fallbacks",
			"external_nodes", len(res.externals),
			"external_fallback_events", res.extReasons,
			"conn_string_shadowed_by_uid", res.shadowed)
	}

	return out
}

// betterSrcCluster reports whether next should replace prev as a duplicate
// (src, tgt) pair's edge labels.cluster. An identified cluster always beats
// the "unknown" missing-label bucket — a sibling series that carried the real
// trace-source cluster is strictly more informative, and plain lexical order
// would let "unknown" win against any real name sorting after it ("us-east-1",
// "v…", …). Among real names (or two "unknown"s) the lexically-smaller wins.
// Pure and order-free, so the pick stays a deterministic function of the data
// (D6) regardless of vector arrival order.
func betterSrcCluster(next, prev string) bool {
	if next == prev {
		return false
	}
	if prev == "unknown" {
		return true
	}
	if next == "unknown" {
		return false
	}
	return next < prev
}

// isConnString reports whether a client/server label is a "://" connection
// string (D29) rather than a workload name or pod UID. It is the single
// definition of the connection-string discriminator, shared by resolveEmptyUID
// (Stage 0 routing) and normalizeSelfLoopUIDs (D33) so the two can never drift.
func isConnString(label string) bool { return strings.Contains(label, "://") }

// normalizeSelfLoopUIDs implements the D33 self-loop UID guard. Some
// service-graph exporters stamp BOTH client_k8s_pod_uid and server_k8s_pod_uid
// with the SAME pod UID for a peer they could only identify as a "://"
// connection string (the real remote lives in the client/server label, not in
// a pod UID). A non-empty UID normally short-circuits D29 Stage 0
// (resolveEmptyUID), so the "://" side would collapse onto the caller's own pod
// — a self-loop pod-calls-pod edge — and no service node would ever
// materialise. When the two UIDs collide (non-empty and equal), the UID on any
// "://" side is bogus and is cleared so that side falls through to
// connection-string resolution; the non-"://" side keeps the shared UID and
// resolves to its real pod.
func normalizeSelfLoopUIDs(clientUID, serverUID, clientLabel, serverLabel string) (string, string) {
	if clientUID == "" || clientUID != serverUID {
		return clientUID, serverUID
	}
	if isConnString(clientLabel) {
		clientUID = ""
	}
	if isConnString(serverLabel) {
		serverUID = ""
	}
	return clientUID, serverUID
}

// resolveEmptyUID resolves an endpoint that carries no pod UID — the shared
// prologue for both the client and server sides. Per the D29 resolution order:
//  1. a "://" label runs connection-string resolution (Stage 0: services / external)
//  3. a non-URL label promotes to an external node (D27 fallback)
//  4. a wholly empty endpoint drops
//
// (Step 2, pod-UID resolution, is the caller's responsibility and only runs
// for non-empty UIDs.) A no-UID endpoint resolves to a service or external
// node, never a pod. Every path now yields at most one ID: Stage 0 resolves to
// the single local service node (the caller's own cluster) or one external
// node, the D27 fallback yields one external node, and a wholly empty endpoint
// yields nil (drop).
func (r *sgResolver) resolveEmptyUID(label, anchorCluster string, t sgTrace) []string {
	if isConnString(label) {
		return r.resolveConnString(label, anchorCluster, t) // Stage 0 — service or external, never a pod
	}
	if label != "" {
		r.noteExternal("missing_uid_nonurl_label", t, "anchor_cluster", anchorCluster) // D27 fallback (non-URL only)
		return []string{r.external(label)}
	}
	return nil // drop
}

// resolveClient resolves the client side of a service-graph series. Returns
// (ids, isPod). isPod is true when the resolved endpoint is a pod — real or
// synthesised from a non-empty UID. A "://" connection string resolves to a
// single service node in the anchor cluster, or an external node (never a pod).
// Every path returns at most one ID. The client side knows its cluster from
// the metric's `cluster` label.
func (r *sgResolver) resolveClient(label, traceCluster, podUID, namespace string, t sgTrace) ([]string, bool) {
	if podUID == "" {
		return r.resolveEmptyUID(label, traceCluster, t), false
	}
	// A populated UID short-circuits connection-string resolution (D29 order):
	// a "://" label on this side is therefore SKIPPED and the endpoint resolves
	// to a pod, never the service it names. Surfaced as debug evidence because it
	// is the usual reason a "://" peer resolves on one side (empty UID) but
	// collapses to a pod on the other (populated UID).
	if isConnString(label) {
		r.shadowed++
		slog.Debug("service-graph :// label SHADOWED by populated UID (resolved as pod, not service)",
			"reason", "conn_string_shadowed_by_uid", "side", t.side, "label", label,
			"pod_uid", podUID, "trace_cluster", traceCluster,
			"client", t.clientLabel, "server", t.serverLabel,
			"client_uid", t.clientUID, "server_uid", t.serverUID)
	}
	if pod := r.lookupClientPod(traceCluster, podUID); pod != nil {
		return []string{pod.ID()}, true
	}
	id := graph.PodID(traceCluster, podUID)
	r.synthPod(id, traceCluster, namespace, podUID)
	return []string{id}, true
}

// lookupClientPod finds the REAL topology pod for a client endpoint: first the
// cluster-scoped id (trace cluster + UID), then the global UID index — the
// trace's `cluster` label is frequently missing (bucketed to "unknown") or
// disagrees with the client pod's real topology cluster, so the cluster-scoped
// lookup can miss even though the pod exists; recovering via the UID index
// (symmetric with resolveServer) avoids minting an "unknown/<uid>" ghost for
// every client pod of a no-cluster-label deployment. nil when the UID is empty
// or unknown to BOTH indexes — the synth-pod fallback is the caller's
// (resolveClient's) concern. Shared with the collectRouteQueries prescan so
// the prescan's real-client-pod trigger test cannot drift from resolution's.
func (r *sgResolver) lookupClientPod(traceCluster, podUID string) *graph.PodNode {
	if podUID == "" {
		return nil
	}
	if pod, ok := r.podByID[graph.PodID(traceCluster, podUID)]; ok {
		return pod
	}
	if pod, ok := r.podByUID[podUID]; ok {
		return pod
	}
	return nil
}

// isPodOrServiceID reports whether a resolved endpoint id names a type="pod"
// node (a real topology pod or a synthesised one) or a type="service" node.
//
// This is the RED attachment rule's structural test (design D1): an endpoint the
// reader could actually NAME. How it was identified — a pod UID, a "://"
// connection string, a peer address matched to a ClusterIP or to a Pod IP, a
// route-engine resolution — is irrelevant. What the rule excludes is the
// external node, which collapses every caller-visible destination sharing one
// label string onto one non-cluster-scoped identity.
//
// It cannot be inferred from the edge type: only a service target downgrades
// the type to pod-calls-service, so an external target leaves a pod-calls-pod
// edge looking exactly like a resolved one.
func (r *sgResolver) isPodOrServiceID(id string) bool {
	if _, ok := r.podByID[id]; ok {
		return true
	}
	if _, ok := r.synthPods[id]; ok {
		return true
	}
	_, ok := r.services[id]
	return ok
}

// chainEntryIndex reports the index within a resolved server-target slice that
// holds the RouteHit ingress-chain ENTRY hop, or -1 when the resolution
// produced no chain.
//
// The two-target shape is unique to a chained RouteHit: routeIndexResolve
// appends the backend id to resolveRouteChain's single ingress id, yielding
// [ingress, backend] (route-hit-ingress-chain D5). Every other server-side path
// — pod UID, synth pod, connection string, peer address, route hit without a
// chain, external fallback — resolves to at most one node, because
// resolveServiceLevel materialises exactly one service node per endpoint.
//
// Load-bearing for RED: the entry hop and the retained caller→backend edge are
// two projections of ONE observed call, so measuring both would make a single
// request contribute its rate twice to any sum over the chain (design D1).
// Written as `> 1` rather than `== 2` deliberately: the shape is an invariant
// of today's resolution paths, not something the type system enforces, and if a
// future path ever returns three targets the safe failure is "the first hop
// goes unmeasured", not "the entry hop is measured and the call is counted
// twice".
func chainEntryIndex(tgtIDs []string) int {
	if len(tgtIDs) > 1 {
		return 0
	}
	return -1
}

// classifyPeerHost runs the in-cluster classification ladder of the
// unknown-server enrichment over a port-stripped host: the Kubernetes .svc DNS
// grammar, the bare short Service name (resolved in the client pod's own
// namespace), and the anchor-cluster ClusterIP literal. classified=false means
// no grammar produced a service identity. Pure over the resolver's immutable
// indexes — no materialisation — so the collectRouteQueries prescan shares it
// with resolveUnknownServerPeer (design D2's anti-drift extraction).
func (r *sgResolver) classifyPeerHost(host, clientNamespace, anchorCluster string) (ns, svc string, classified bool) {
	if s, n, ok := classifyK8sDNS(host); ok {
		return n, s, true
	}
	if s, ok := classifyBareShortName(host); ok {
		return clientNamespace, s, true
	}
	// resolve-unknown-server-ip-peer: a bare IP literal is looked up against
	// the anchor cluster's OWN ClusterIP set only — never a family sibling,
	// since a ClusterIP is a per-cluster address that can legitimately collide
	// across unrelated clusters' Service CIDRs (see ipKey doc comment).
	if ip := net.ParseIP(host); ip != nil {
		if sk, hit := r.ipIndex[ipKey{cluster: anchorCluster, ip: ip.String()}]; hit {
			return sk.namespace, sk.service, true
		}
	}
	return "", "", false
}

// lookupPeerPodIP resolves a peer host to the pod holding that Pod IP within
// the anchor cluster's FAMILY — the resolve-unknown-server-pod-ip-peer step,
// tried only after the Service ClusterIP lookup inside classifyPeerHost has
// missed, so a ClusterIP always takes priority over a Pod IP for the same
// address. Pure over the resolver's immutable indexes — no materialisation —
// so the collectRouteQueries prescan shares it with resolveUnknownServerPeer.
//
// Three rules, in order (see famIPKey for why cross-cluster matching is sound
// and why no mesh gate is applied):
//
//  1. The anchor cluster's own holder always wins. Byte-for-byte the
//     pre-widening behaviour, and the right call even under real overlap: a
//     caller most plausibly reached a pod in its own cluster.
//  2. Otherwise, a LONE family holder resolves. Being the only cluster in the
//     family carrying the address is direct evidence that the family's pod
//     CIDRs do not overlap at this address, so the match is unambiguous.
//  3. Two or more family holders means the address is genuinely ambiguous:
//     return no pod and report ambiguous=true, so the caller degrades to an
//     external node rather than guessing (the repo's pickIngressCluster /
//     RouteAmbiguousIngressService convention).
//
// ambiguous is only ever true alongside a nil pod, and distinguishes rule 3
// from a plain miss for the debug tally. A different-family cluster is
// excluded by the famIPKey lookup itself — no extra check.
func (r *sgResolver) lookupPeerPodIP(anchorCluster, host string) (pod *graph.PodNode, ambiguous bool) {
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, false
	}
	cands := r.podIPCandidates[famIPKey{family: ClusterFamilyKey(anchorCluster), ip: ip.String()}]
	for _, c := range cands {
		if c.cluster == anchorCluster {
			return c.pod, false
		}
	}
	if len(cands) == 1 {
		return cands[0].pod, false
	}
	return nil, len(cands) > 1
}

// canonicalIP normalises an IP literal to net.IP's canonical text form, so an
// index built from one spelling is hit by any other spelling of the same
// address. A non-IP value is returned verbatim (it can then only be matched by
// an identical non-IP value, which is the pre-existing behaviour).
func canonicalIP(s string) string {
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return s
}

// anchorHolds reports whether the anchor cluster itself deploys (ns, svc) —
// the same membership test resolveServiceLevel applies before materialising a
// service node. Used by the prescan to skip endpoints the in-cluster ladder
// already resolves (route resolution runs ONLY where the parse would fall to
// an external node).
func (r *sgResolver) anchorHolds(anchorCluster, ns, svc string) bool {
	for _, cand := range r.svcCandidates[famSvcKey{family: ClusterFamilyKey(anchorCluster), namespace: ns, service: svc}] {
		if cand.cluster == anchorCluster {
			return true
		}
	}
	return false
}

// resolveServer mirrors resolveClient. The metric does not carry server-side
// cluster, so pod-UID resolution recovers it via the global UID index; the
// connection-string path resolves to a SINGLE service node in anchorCluster
// (the caller's own cluster), iff that cluster holds the service, then fans out
// service-selects-pod edges that MAY cross clusters (the same-family endpoint
// union — `.svc` names route to backing pods in any family member under mesh
// routing). anchorCluster is the caller's authoritative cluster: the
// UID-recovered client-pod cluster when the client side resolved to a topology
// pod, else the raw trace label (bucketed to "unknown" when missing).
//
// clientPod and peer feed ONLY the unknown-server peer-label enrichment:
// whenever the raw server label is literally "unknown" AND no real topology
// pod is found for it (UID empty, or UID present but absent from the global
// pod-UID index), resolution routes to resolveUnknownServerPeer instead of
// resolveEmptyUID or the synth-pod fallback below — D1's explicit carve-out,
// so the loosened server!~"user" selector does not leak external/unknown noise
// via the generic paths. peer carries the three precedence-ordered
// peer-address labels (see peerLabels.value) plus the optional route-resolution
// dimensions.
func (r *sgResolver) resolveServer(label, anchorCluster, podUID, namespace string, t sgTrace, clientPod *graph.PodNode, peer peerLabels) []string {
	if podUID == "" {
		if label == sentinelUnknown {
			return r.resolveUnknownServerPeer(clientPod, peer, t)
		}
		return r.resolveEmptyUID(label, anchorCluster, t)
	}
	// As in resolveClient: a populated server_k8s_pod_uid SKIPS connection-string
	// resolution, so a "://" server label never maps to its service node (it
	// collapses onto the UID's pod, or a synth pod when the UID is unknown to
	// topology). This is the most common cause of a "://" peer resolving as a
	// service on the client side yet falling through on the server side. Never
	// fires for label == "unknown" — isConnString("unknown") is false — so this
	// evidence path and the enrichment branch below are mutually exclusive.
	if isConnString(label) {
		r.shadowed++
		_, known := r.podByUID[podUID]
		slog.Debug("service-graph :// label SHADOWED by populated UID (resolved as pod, not service)",
			"reason", "conn_string_shadowed_by_uid", "side", t.side, "label", label,
			"pod_uid", podUID, "uid_known_to_topology", known, "anchor_cluster", anchorCluster,
			"client", t.clientLabel, "server", t.serverLabel,
			"client_uid", t.clientUID, "server_uid", t.serverUID, "trace_cluster", t.traceCluster)
	}
	if pod, ok := r.podByUID[podUID]; ok {
		return []string{pod.ID()}
	}
	if label == sentinelUnknown {
		return r.resolveUnknownServerPeer(clientPod, peer, t)
	}
	r.synthPod(graph.PodID("", podUID), "", namespace, podUID) // server cluster unknown
	return []string{graph.PodID("", podUID)}
}

// resolveUnknownServerPeer implements the "Unknown-server peer-label
// enrichment" requirement (resolve-unknown-server-peer-labels D1-D3, extended
// by resolve-unknown-server-ip-peer and resolve-unknown-server-network-peer-address):
// the narrow carve-out that lets a literal server="unknown" endpoint resolve
// via three client-recorded peer-address labels instead of being
// unconditionally dropped — but only when the client side already resolved to
// a REAL (non-synthesised) topology pod, so the anchor cluster is unambiguous.
// clientPod nil (client unresolved, or resolved only to a synth pod) and "all
// three labels empty" both drop the endpoint (nil), byte-for-byte identical to
// the outcome under the old server!~"user|unknown" query-layer exclusion.
//
// The three peer-address labels are precedence-ordered in peerLabels.value
// (D1): the first non-empty wins outright and is never merged with a
// lower-precedence label, and a non-empty higher-precedence label that fails
// to classify does NOT fall through to a lower-precedence one.
// client_server_address (server.address, stable, name-valued) leads;
// client_network_peer_address (network.peer.address, stable, IP-valued)
// follows; client_net_peer_name (net.peer.name, deprecated in favour of
// server.address) trails — ranked by what the classification chain below can
// resolve (strong on names, weak on IP literals), not by recency of spelling.
//
// resolve-unknown-server-pod-ip-peer adds ONE step to the IP-literal branch:
// when the host parses as an IP but matches no Service ClusterIP in the anchor
// cluster, it is looked up against the Pod IP set of the anchor cluster's whole
// FAMILY before the external fallback (and therefore before route resolution) —
// cross-cluster pod-to-pod dialling over a flat network is real traffic. A hit
// resolves to the topology pod itself — no service node, no service-selects-pod
// fan-out — yielding the ordinary pod-calls-pod edge, which MAY cross clusters.
// The anchor cluster wins when it holds the address; otherwise a lone family
// holder resolves and two or more degrade to external. See lookupPeerPodIP.
//
// translate-global-fqdn-to-k8s-service adds ONE step: at EVERY point below
// that would emit an external node — no grammar matched, an IP literal with
// no ClusterIP hit, AND a classified (ns, svc) the anchor cluster does not
// hold (the branch a global FQDN like api.example.com actually takes, design
// D3) — the prefetched route-engine index is consulted first via
// routeExternal. A route hit resolves through the same resolveServiceLevel as
// every other path; a miss (or the engine being off) falls to the external
// node exactly as before.
func (r *sgResolver) resolveUnknownServerPeer(clientPod *graph.PodNode, peer peerLabels, t sgTrace) []string {
	if clientPod == nil {
		return nil // client side did not resolve to a real topology pod
	}
	value := peer.value()
	if value == "" {
		return nil // none of the three peer-address labels present
	}

	anchorCluster := clientPod.Labels()["cluster"]
	clientNamespace := clientPod.Labels()["namespace"]

	// Truncate + port-split matches peerRouteKey so classify host and route-key
	// host cannot drift (resolve-unknown-server-network-peer-address D2;
	// translate-global-fqdn-to-k8s-service anti-drift).
	host, _ := splitPeerAddressPort(truncateBracketSuffix(value))
	// The same key derivation the collectRouteQueries prescan used, so the
	// index lookup can never miss for key-derivation reasons (ok is guaranteed
	// here: value is non-empty).
	key, _ := peerRouteKey(anchorCluster, peer)

	// routeExternal is the shared external fallback: consult the route index
	// first; only when it does not save the endpoint, record the classify
	// reason (unless the index already recorded a route_engine_* reason for
	// this endpoint) and emit the external node.
	routeExternal := func(reason string, attrs ...any) []string {
		ids, noted := r.routeIndexResolve(key, value, reason, t)
		if len(ids) > 0 {
			return ids
		}
		if !noted {
			r.noteExternal(reason, t, attrs...)
		}
		return []string{r.external(value)}
	}

	ns, svc, classified := r.classifyPeerHost(host, clientNamespace, anchorCluster)
	if !classified {
		if net.ParseIP(host) != nil {
			// resolve-unknown-server-pod-ip-peer: an IP literal that matched no
			// Service ClusterIP may still be a pod's own address — a caller that
			// dialled the pod directly, bypassing any Service, possibly across a
			// flat cluster boundary. Scoped to the anchor cluster's family, with
			// the anchor preferred and an ambiguous family degrading (see
			// lookupPeerPodIP). A hit is a POD, so it deliberately does NOT go
			// through resolveServiceLevel: no service node is materialised and
			// no service-selects-pod edge is emitted, and the generic
			// target-driven rule makes the edge pod-calls-pod.
			pod, ambiguous := r.lookupPeerPodIP(anchorCluster, host)
			if pod != nil {
				slog.Debug("service-graph unknown-server peer IP resolved to pod",
					"side", t.side, "peer_address", value, "host", host,
					"anchor_cluster", anchorCluster, "pod_cluster", pod.Labels()["cluster"],
					"pod_id", pod.ID(),
					"client", t.clientLabel, "server", t.serverLabel)
				return []string{pod.ID()}
			}
			if ambiguous {
				// Two or more family clusters hold the address: their pod CIDRs
				// overlap here, so picking one would fabricate a dependency.
				return routeExternal("unknown_server_peer_pod_ip_ambiguous",
					"host", host, "peer_address", value, "anchor_cluster", anchorCluster,
					"anchor_family", ClusterFamilyKey(anchorCluster))
			}
			return routeExternal("unknown_server_peer_ip_literal_no_match",
				"host", host, "peer_address", value, "anchor_cluster", anchorCluster)
		}
		return routeExternal("unknown_server_peer_not_k8s_dns",
			"host", host, "peer_address", value, "anchor_cluster", anchorCluster)
	}
	if ids := r.resolveServiceLevel(anchorCluster, ns, svc); len(ids) > 0 {
		slog.Debug("service-graph unknown-server peer-label resolved to service node",
			"side", t.side, "peer_address", value, "service", svc, "namespace", ns,
			"anchor_cluster", anchorCluster, "service_id", ids[0],
			"client", t.clientLabel, "server", t.serverLabel)
		return ids
	}
	// Host classified to (ns, svc) fine, but the anchor cluster does not itself
	// hold that Service in its own family — external, not dropped (mirrors
	// resolveConnString's anchor_cluster_lacks_service outcome).
	return routeExternal("unknown_server_peer_anchor_lacks_service",
		"service", svc, "namespace", ns, "host", host, "peer_address", value,
		"anchor_cluster", anchorCluster, "anchor_family", ClusterFamilyKey(anchorCluster))
}

// viaNodeID is the LOOKUP-ONLY mirror of resolveUnknownServerPeer's
// classification chain (add-span-link-logical-edges D3): it derives the graph
// node ID a peer address WOULD resolve to — anchored on the given side's own
// already-resolved topology pod — without materialising any node, emitting any
// edge, or recording any external-fallback reason. Used to mark the
// pod→broker transport pair of a span-link series; if the network edge for
// the pair was never produced by a real series, the pair is a pure marker
// (resolveRouteChain's orphan-protection precedent: service nodes are never
// pruned by projection, so a marker lookup that materialised would leak
// orphan broker nodes). ok=false when the side carries no peer-address value.
//
// Anti-drift: every step calls the same pure helpers the materialising path
// calls (truncateBracketSuffix, splitPeerAddressPort, classifyPeerHost,
// anchorHolds, lookupPeerPodIP, peerRouteKey), so the derived ID can never
// diverge from the ID resolveUnknownServerPeer would materialise for the same
// (pod, peer) input. An ambiguous Pod-IP family degrades through routeNodeID
// exactly like the materialising path's routeExternal.
func (r *sgResolver) viaNodeID(ownPod *graph.PodNode, peer peerLabels) (string, bool) {
	value := peer.value()
	if value == "" {
		return "", false // none of the peer-address labels present — no marker
	}
	anchorCluster := ownPod.Labels()["cluster"]
	host, _ := splitPeerAddressPort(truncateBracketSuffix(value))
	// ok is guaranteed: value is non-empty (same reasoning as
	// resolveUnknownServerPeer's key derivation).
	key, _ := peerRouteKey(anchorCluster, peer)

	if ns, svc, classified := r.classifyPeerHost(host, ownPod.Labels()["namespace"], anchorCluster); classified {
		if r.anchorHolds(anchorCluster, ns, svc) {
			return graph.ServiceID(anchorCluster, ns, svc), true
		}
		return r.routeNodeID(key, value), true
	}
	if pod, _ := r.lookupPeerPodIP(anchorCluster, host); pod != nil {
		return pod.ID(), true
	}
	return r.routeNodeID(key, value), true
}

// routeNodeID is the lookup-only twin of routeIndexResolve (design D5): it
// consults the prefetched route index for the node ID an endpoint would
// resolve to, without materialisation, logging, or extReasons tallies. The
// deliberate divergences from the materialising path: a chained RouteHit
// yields only the BACKEND service ID (the marker targets the routed service,
// never the ingress hop), no role marking, no chain synthesis. Every miss —
// engine off, no destination IPs, an uncollected/truncated key (maxRouteKeys:
// a truncated key has no index entry, so both paths degrade identically), a
// failed or non-hit entry, or a dest cluster that does not hold the service —
// derives the external node ID of the RAW peer value, exactly the ID the
// materialising path's external fallback produces.
func (r *sgResolver) routeNodeID(key routeKey, raw string) string {
	ext := graph.ExternalID(raw)
	if r.routes == nil || key.ips == "" {
		return ext
	}
	entry, ok := r.routes[key]
	if !ok || entry.failed {
		return ext
	}
	if entry.outcome == RouteHit || entry.outcome == RouteIngressLBService {
		// Same membership test resolveServiceLevel / resolveServiceLevelInCluster
		// apply before materialising: the engine-selected cluster must itself
		// hold the destination service in topology. Every other outcome (the
		// route_engine_* miss family) degrades to the external ID below.
		if r.anchorHolds(entry.dest.Cluster, entry.dest.Namespace, entry.dest.Service) {
			return graph.ServiceID(entry.dest.Cluster, entry.dest.Namespace, entry.dest.Service)
		}
	}
	return ext
}

// classifyBareShortName reports whether host is a bare, dot-free Service short
// name — the resolve-unknown-server-peer-labels D2 step-3 grammar extension
// over classifyK8sDNS, scoped to the unknown-server peer-label enrichment
// trigger only (connection-string resolution does NOT gain this grammar). A
// multi-label host or an IP literal (IPv4 or IPv6) is never a bare short name.
func classifyBareShortName(host string) (service string, ok bool) {
	if host == "" || strings.Contains(host, ".") {
		return "", false
	}
	if net.ParseIP(host) != nil {
		return "", false
	}
	return host, true
}

// resolveConnString implements D29 Stage 0 for a label containing "://". Every
// recognised in-cluster reference resolves to a SINGLE Service node in the
// caller's own (anchor) cluster — both the <service>.<namespace> form and the
// headless <pod-hostname>.<service>.<namespace> form resolve to the same
// (service, namespace) — provided the anchor cluster itself holds that Service
// (see resolveServiceLevel). The resolved node fans out service-selects-pod
// edges that MAY cross clusters (the cross-cluster endpoint union over the
// caller's family). An unparseable host, a non-2/3-label name, or an anchor
// cluster that does not hold the service falls back to an external node. The
// result is therefore never a pod — Stage 0 yields a single service node or a
// single external node.
//
// Deliberately NOT memoised: resolution is a url.Parse plus a couple of map
// hits (µs-scale, dwarfed by the upstream fetch), materialisation is
// idempotent (services / externals / svcEdges all dedupe), and a cache keyed
// on (anchorCluster, label) is exactly the kind of collision-prone state a
// pure per-parse function does not need.
func (r *sgResolver) resolveConnString(label, anchorCluster string, t sgTrace) []string {
	// Each early return below is a distinct, separately-logged external-fallback
	// reason so an operator can tell exactly why a "://" peer did not resolve.
	host := connStringHost(label)
	if host == "" {
		// url.Parse produced no host: opaque double-scheme (e.g. "jdbc:postgresql://…")
		// or otherwise unparseable authority.
		r.noteExternal("conn_host_unparseable", t, "anchor_cluster", anchorCluster)
		return []string{r.external(label)}
	}
	svc, ns, ok := classifyK8sDNS(host)
	if !ok {
		// Host is not a 2/3-label Kubernetes .svc DNS name (single-label short
		// name, 4+-label custom/mesh domain, IP literal, comma-joined multi-host…).
		r.noteExternal("conn_host_not_k8s_dns", t, "host", host, "anchor_cluster", anchorCluster)
		return []string{r.external(label)}
	}
	if ids := r.resolveServiceLevel(anchorCluster, ns, svc); len(ids) > 0 {
		slog.Debug("service-graph :// resolved to service node",
			"side", t.side, "label", label, "service", svc, "namespace", ns,
			"anchor_cluster", anchorCluster, "service_id", ids[0],
			"client", t.clientLabel, "server", t.serverLabel)
		return ids
	}
	// Host classified to (ns, svc) fine, but the anchor cluster does not itself
	// hold that Service in its own family (same-cluster precondition; a family
	// sibling holding it is not enough) → external node (labels={}, verbatim
	// label as name).
	r.noteExternal("anchor_cluster_lacks_service", t,
		"service", svc, "namespace", ns, "host", host,
		"anchor_cluster", anchorCluster, "anchor_family", ClusterFamilyKey(anchorCluster))
	return []string{r.external(label)}
}

// resolveServiceLevel resolves a `<service>.<namespace>` record to a SINGLE
// service node in the caller's OWN (anchor) cluster, per the localised
// connection-string model: pod→service is intra-cluster, service→pod MAY cross
// clusters. The endpoint resolves iff the anchor cluster itself holds the
// (namespace, service) — i.e. the anchor is one of the family candidates. Steps:
//
//  1. cands = svcCandidates[famSvcKey{family(anchor), ns, svc}] — every
//     same-family cluster holding the service, sorted by cluster (D6).
//  2. The anchor cluster must appear among cands; if not, return nil and the
//     caller falls back to an external node. This single membership test
//     uniformly covers an anchor whose own cluster lacks the service (a family
//     sibling holding it is NOT enough — a same-named local Service is a mesh
//     precondition), an "unknown"/empty/bogus anchor naming no holder in its
//     own family, AND preserves the fully-unlabelled single-cluster case
//     (ClusterFamilyKey("unknown") == "unknown" is a family-of-one, so an
//     "unknown"-bucketed service makes "unknown" a legitimate holder). There is
//     NO cross-family fallback.
//  3. Materialise ONE service node, in the anchor cluster, from its OWN
//     ServiceObs (anchor's cluster_ip / headless status).
//  4. Fan out service-selects-pod edges from that single node to the UNION of
//     backing pods across ALL cands' EndpointsByService entries — every
//     same-family cluster holding the service. These edges MAY cross clusters
//     (a local service node selecting a backing pod in a family sibling),
//     reflecting service-mesh endpoint aggregation where each cluster's KSM
//     observes only its OWN EndpointSlices. There is NO endpoint-backed pruning:
//     a sibling with the Service but zero endpoints contributes no edge, and a
//     service with zero endpoints anywhere still materialises its (single) node
//     — an operator signal.
//
// Returns the single-element slice [anchorSvcID], or nil (→ external).
func (r *sgResolver) resolveServiceLevel(anchorCluster, ns, svc string) []string {
	cands := r.svcCandidates[famSvcKey{family: ClusterFamilyKey(anchorCluster), namespace: ns, service: svc}]
	var anchor *svcCandidate
	for i := range cands {
		if cands[i].cluster == anchorCluster {
			anchor = &cands[i]
			break
		}
	}
	if anchor == nil {
		return nil // anchor cluster does not hold the service → external
	}
	id := r.materializeServiceNode(anchorCluster, ns, svc, anchor.obs)
	// Cross-cluster service-selects-pod fan-out: union backing pods across every
	// same-family cluster holding the service. cands is sorted, addServiceEdge
	// dedupes by (svcID, podID), and SortEdges canonicalises the final order, so
	// the result is a pure function of the data regardless of arrival order (D6).
	for _, cand := range cands {
		for _, ep := range r.endpointsByService[serviceKey{cand.cluster, ns, svc}] {
			r.addServiceEdge(id, ep.Pod.ID(), ns)
		}
	}
	return []string{id}
}

// resolveServiceLevelInCluster is resolveServiceLevel with the
// service-selects-pod fan-out LOCKED to one cluster's own endpoints — no
// family union (route-hit-ingress-chain D2). Used for ingress LB Services
// (the RouteHit chain's entry hop and the RouteIngressLBService fallback):
// an LB IP is a per-cluster address, so the pods behind it are the locked
// cluster's own endpoints — a family sibling's same-named Service (e.g.
// istio-system/istio-ingressgateway, present in nearly every mesh cluster)
// is NOT behind this IP and must not contribute pods. Same anchor-membership
// test, same idempotent materializeServiceNode, same
// no-endpoint-backed-pruning rule (a held service with zero endpoints still
// materialises its node). Returns [svcID] or nil (cluster does not hold the
// service).
func (r *sgResolver) resolveServiceLevelInCluster(cluster, ns, svc string) []string {
	cands := r.svcCandidates[famSvcKey{family: ClusterFamilyKey(cluster), namespace: ns, service: svc}]
	var anchor *svcCandidate
	for i := range cands {
		if cands[i].cluster == cluster {
			anchor = &cands[i]
			break
		}
	}
	if anchor == nil {
		return nil // cluster does not hold the service
	}
	id := r.materializeServiceNode(cluster, ns, svc, anchor.obs)
	for _, ep := range r.endpointsByService[serviceKey{cluster, ns, svc}] {
		r.addServiceEdge(id, ep.Pod.ID(), ns)
	}
	return []string{id}
}

// materializeServiceNode creates (once) a ServiceNode for the resolved service
// in its own cluster. Idempotent and edge-free: the service-selects-pod fan-out
// is driven by the caller (resolveServiceLevel) so a single local node can fan
// out across same-family clusters.
func (r *sgResolver) materializeServiceNode(cluster, ns, svc string, obs ServiceObs) string {
	id := graph.ServiceID(cluster, ns, svc)
	if _, ok := r.services[id]; ok {
		return id
	}
	var ips []string
	if obs.ClusterIP != "" && obs.ClusterIP != "None" {
		ips = []string{obs.ClusterIP}
	}
	r.services[id] = &graph.ServiceNode{
		IDValue:          id,
		NameValue:        svc,
		LabelsValue:      map[string]string{"cluster": cluster, "namespace": ns},
		IPAddressValue:   ips,
		ApplicationValue: r.serviceApps[serviceKey{cluster, ns, svc}],
	}
	return id
}

func (r *sgResolver) addServiceEdge(svcID, podID, ns string) {
	key := svcID + "|" + podID
	if _, ok := r.svcEdges[key]; ok {
		return
	}
	labels := map[string]string{}
	if ns != "" {
		labels["namespace"] = ns
	}
	r.svcEdges[key] = graph.NewEdge(graph.EdgeTypeServiceSelectsPod, svcID, podID, labels)
}

// Ingress-role marker values (mark-ingress-route-path D3). Exactly two, each
// mirroring the engine outcome that materialises an ingress service node:
// roleIngressGateway for the RouteHit chain's entry hop (gateway pods and a
// synthesized pod-calls-service edge to the backend exist behind it),
// roleIngressLB for the RouteIngressLBService (nginx) fallback destination
// (no routed backend).
const (
	roleIngressGateway = "ingress-gateway"
	roleIngressLB      = "ingress-lb"
)

// markIngressService sets the `role` key on an already-materialised ingress
// service node's labels. Assignment is set-only and MONOTONE (design D3): one
// Service can be reached by BOTH paths within a single build — one endpoint's
// routed hit chains through it as the entry hop while another endpoint's
// resolution LB-falls-back to the same Service — and a first-write-wins rule
// would make the emitted value depend on vector arrival order. Rule:
// ingress-gateway always overwrites (the more informative claim — a chain
// provably exists behind the node); ingress-lb writes only into an unset
// value. Never cleared, never downgraded, so it is idempotent under repeated
// series sharing one route key and order-free (D6).
func (r *sgResolver) markIngressService(id, role string) {
	sv, ok := r.services[id]
	if !ok {
		return
	}
	if role == roleIngressGateway || sv.LabelsValue["role"] == "" {
		sv.LabelsValue["role"] = role
	}
}

// routeChainEdge is one synthesized (not trace-derived) pod-calls-service
// edge of the RouteHit ingress chain: an ingress gateway pod routing to the
// routed backend service (route-hit-ingress-chain D4). cluster is the locked
// ingress cluster — the source pod's own cluster, so the emitted edge's
// labels.cluster follows the D9 client-side-cluster rule.
type routeChainEdge struct{ src, tgt, cluster string }

// addRouteChainEdge accumulates one synthesized ingress-pod → backend-service
// edge, deduped by (src, tgt). Emission (with the traced-edge-wins check
// against the parse's pairs map) happens in parseServiceGraphRoutes.
func (r *sgResolver) addRouteChainEdge(srcPodID, backendSvcID, cluster string) {
	key := srcPodID + "|" + backendSvcID
	if _, ok := r.routeChainEdges[key]; ok {
		return
	}
	r.routeChainEdges[key] = routeChainEdge{src: srcPodID, tgt: backendSvcID, cluster: cluster}
}

// resolveRouteChain attempts the full ingress chain for a RouteHit whose
// destination carries an ingress LB Service identity (route-hit-ingress-chain
// D3): caller → ingress service → ingress pods → backend service. Every
// precondition is checked PURELY (no materialisation) first, so a degrade
// leaves zero stray nodes/edges — service nodes are never pruned by
// projection, so a materialise-then-bail would leak an orphan ingress node.
// ok=false ⇒ the caller emits today's direct caller→backend shape (never an
// external, never a build failure); the degrade is observable at Debug only
// and deliberately NOT counted in extReasons (whose invariant is "events
// that produced external nodes").
//
// On success the ingress service materialises with the LOCKED-CLUSTER
// service-selects-pod fan-out (D2), one synthesized pod-calls-service
// edge per locked-cluster ingress pod → the backend service, and the returned
// [ingressSvcID] joins the backend id as the endpoint's resolution targets
// (routeIndexResolve appends the backend) — the main loop's cross product
// then emits caller→ingress-service AND the direct caller→backend edge, so
// the caller's dependency on the backend is never lost behind the shared
// ingress funnel (D5).
func (r *sgResolver) resolveRouteChain(dest RouteDestination, backendSvcID string, t sgTrace) ([]string, bool) {
	degrade := func(reason string) ([]string, bool) {
		slog.Debug("route chain degraded to direct edge",
			"chain_degrade_reason", reason,
			"ingress_cluster", dest.Cluster,
			"ingress_namespace", dest.IngressNamespace, "ingress_service", dest.IngressService,
			"namespace", dest.Namespace, "service", dest.Service,
			"client", t.clientLabel, "server", t.serverLabel)
		return nil, false
	}
	if dest.IngressService == "" {
		// No unique ingress identity in the window (ambiguous or absent) —
		// quiet: this is the common non-ingress-fronted case, not an anomaly.
		return nil, false
	}
	if dest.IngressNamespace == dest.Namespace && dest.IngressService == dest.Service {
		return degrade("destination_is_ingress_service")
	}
	if !r.anchorHolds(dest.Cluster, dest.IngressNamespace, dest.IngressService) {
		return degrade("ingress_cluster_lacks_ingress_service")
	}
	eps := r.endpointsByService[serviceKey{dest.Cluster, dest.IngressNamespace, dest.IngressService}]
	if len(eps) == 0 {
		return degrade("ingress_service_has_no_endpoints")
	}

	// Preconditions hold — materialise. Non-nil by construction: membership
	// was pre-checked via anchorHolds over the same index. Every precondition
	// degrade returned before this point, so marking happens only on a
	// successfully materialised ingress node (mark-ingress-route-path).
	ids := r.resolveServiceLevelInCluster(dest.Cluster, dest.IngressNamespace, dest.IngressService)
	r.markIngressService(ids[0], roleIngressGateway)
	podIDs := make([]string, 0, len(eps))
	for _, ep := range eps {
		podIDs = append(podIDs, ep.Pod.ID())
	}
	sort.Strings(podIDs)
	for i, podID := range podIDs {
		if i > 0 && podIDs[i-1] == podID {
			continue // sorted-unique; addRouteChainEdge dedupes anyway (D6)
		}
		r.addRouteChainEdge(podID, backendSvcID, dest.Cluster)
	}
	return ids, true
}

func (r *sgResolver) external(label string) string {
	id := graph.ExternalID(label)
	if _, ok := r.externals[id]; !ok {
		r.externals[id] = &graph.ExternalNode{
			IDValue:     id,
			NameValue:   label,
			LabelsValue: map[string]string{},
		}
	}
	return id
}

func (r *sgResolver) synthPod(id, cluster, namespace, podUID string) {
	if existing, ok := r.synthPods[id]; ok {
		// Deterministic dedupe: the same synth-pod id can arrive again with a
		// different namespace label (conflicting upstream series in arbitrary
		// vector order). Keep the lexically-smaller namespace so the node's
		// content is a pure function of the data, not arrival order (D6). The
		// node is build-local and unpublished, so mutating its label map is safe.
		existingNS := existing.LabelsValue["namespace"]
		if namespace != "" && (existingNS == "" || namespace < existingNS) {
			existing.LabelsValue["namespace"] = namespace
		}
		return
	}
	labels := map[string]string{"cluster": cluster}
	if namespace != "" {
		labels["namespace"] = namespace
	}
	r.synthPods[id] = &graph.PodNode{IDValue: id, NameValue: podUID, LabelsValue: labels}
}

// connStringHost extracts the host of a "://" connection string (scheme,
// userinfo, port, and path stripped). Returns "" when unparseable.
func connStringHost(label string) string {
	u, err := url.Parse(label)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// classifyK8sDNS matches a host against Kubernetes Service DNS grammar and
// returns the addressed (service, namespace). It strips an optional trailing
// ".svc.<cluster-domain>" (or ".svc") and resolves BOTH the service form
// <service>.<namespace> and the headless pod form
// <pod-hostname>.<service>.<namespace> to the same (service, namespace): every
// recognised "://" in-cluster reference resolves to its Service node, which
// fans out to all backing pods (D29). ok is false when the service-relative
// part is not 2 or 3 dotted labels.
func classifyK8sDNS(host string) (service, namespace string, ok bool) {
	rel := host
	// "svc" is a legal DNS-1123 label, so a namespace or service literally
	// named "svc" must not confuse the suffix strip. The bare-suffix check
	// runs FIRST: an FQDN never ends in ".svc", but a bare form like
	// "myservice.svc.svc" (service in a namespace named "svc") contains an
	// interior ".svc." that would otherwise truncate the name too early. For
	// FQDNs the cluster-domain suffix is then the LAST ".svc." occurrence
	// (e.g. "myservice.svc.svc.cluster.local") — a first-occurrence
	// strings.Index would truncate those too early as well.
	if strings.HasSuffix(host, ".svc") {
		rel = strings.TrimSuffix(host, ".svc")
	} else if i := strings.LastIndex(host, ".svc."); i >= 0 {
		rel = host[:i]
	}
	parts := strings.Split(rel, ".")
	switch len(parts) {
	case 2: // <service>.<namespace>
		return parts[0], parts[1], true
	case 3: // <pod-hostname>.<service>.<namespace> → resolve to its service
		return parts[1], parts[2], true
	default:
		return "", "", false
	}
}
