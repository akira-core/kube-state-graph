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

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// sentinelUnknown is the literal server-label value the D30 query-layer
// exclusion no longer drops on the server side (resolve-unknown-server-peer-labels
// D1). A no-op everywhere except the two resolveServer branches that route it
// to resolveUnknownServerPeer instead of the generic empty-UID / synth-pod
// fallbacks.
const sentinelUnknown = "unknown"

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
	vec, err := q.Instant(ctx,
		string(promql.QServiceGraphTotal),
		r.Render(promql.QServiceGraphTotal, window),
		end,
	)
	if err != nil {
		return ServiceGraphResult{}, fmt.Errorf("service-graph query: %w", err)
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
	return parseWithResolver(vec, res, routes), nil
}

// sgResolver carries the per-build dedupe maps and topology indexes used to
// resolve service-graph endpoints into graph nodes. Service nodes and
// service-selects-pod edges are materialised on demand (only for services a
// "://" connection string actually references) and deduped here.
type sgResolver struct {
	endpointsByService map[serviceKey][]EndpointObs // service-selects-pod fan-out source
	podByID            map[string]*graph.PodNode    // client side: cluster known from metric
	podByUID           map[string]*graph.PodNode    // server side: cluster recovered via index
	svcCandidates      map[famSvcKey][]svcCandidate // family → clusters holding (ns, svc), sorted by cluster
	ipIndex            map[ipKey]serviceKey         // (cluster, ClusterIP) → Service deployed there (resolve-unknown-server-ip-peer)
	serviceApps        map[serviceKey]string        // (cluster, namespace, service) → ArgoCD Application
	routes             routeIndex                   // prefetched route-engine answers (nil = engine off; non-nil-but-empty = engine on, nothing collected)
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
// clusters risks resolving to the wrong Service. The identification lookup
// is anchor-cluster-only; once a (namespace, service) is identified, the
// existing family-wide resolveServiceLevel still governs the
// service-selects-pod fan-out.
type ipKey struct{ cluster, ip string }

// parseServiceGraph is the route-index-free form: the ~70 direct test call
// sites and any parse without a RouteResolver go through here. Identical to
// parseServiceGraphRoutes with a nil index — i.e. the pre-change behaviour,
// byte for byte.
func parseServiceGraph(vec model.Vector, topology Topology) ServiceGraphResult {
	return parseServiceGraphRoutes(vec, topology, nil)
}

// newSGResolver builds the per-parse resolver: the immutable topology indexes
// every resolution path consults. Shared by parseServiceGraphRoutes and the
// collectRouteQueries prescan so both classify endpoints over identical
// indexes (design D2).
func newSGResolver(topology Topology) *sgResolver {
	podByID := make(map[string]*graph.PodNode, len(topology.Pods))
	for _, p := range topology.Pods {
		podByID[p.ID()] = p
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
		ik := ipKey{cluster: k.cluster, ip: obs.ClusterIP}
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
	return parseWithResolver(vec, newSGResolver(topology), routes)
}

// parseWithResolver is the parse body over an already-built resolver.
func parseWithResolver(vec model.Vector, res *sgResolver, routes routeIndex) ServiceGraphResult {
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
	type pairKey struct{ src, tgt string }
	pairs := make(map[pairKey]aggEdge, len(vec))

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
		// Peer dimensions recorded on the CLIENT span (OTel semconv
		// client.net.peer.name / client.server.address, plus the optional
		// client_dns_answers / client_server_port / client_net_peer_port added
		// for route resolution), consulted only when server=="unknown" and the
		// client resolves to a real pod — see resolveUnknownServerPeer.
		peer := peerLabelsOf(s.Metric)

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
		// in the caller's own (anchor) cluster — or to a single external node;
		// every other path also yields exactly one ID, and an empty slice drops
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

		// Cross product: any resolved source × any resolved target. Each "://"
		// side now resolves to at most one (local) service node, so a both-"://"
		// series yields a single intra-cluster edge in the anchor cluster. The
		// one two-target case is a chained RouteHit (route-hit-ingress-chain
		// D5): the server side resolves to [ingress service, backend service],
		// yielding both the caller→ingress and the direct caller→backend edge.
		for _, srcID := range srcIDs {
			for _, tgtID := range tgtIDs {
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
			}
		}
	}

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
		// Edge type is target-driven: a target that resolved to a service node
		// (via the D29 "://" connection-string rule) yields pod-calls-service;
		// every other target (pod, synth-pod, external) stays pod-calls-pod.
		edgeType := graph.EdgeTypePodCallsPod
		if _, isSvc := res.services[k.tgt]; isSvc {
			edgeType = graph.EdgeTypePodCallsService
		}
		edges = append(edges, graph.NewEdge(edgeType, k.src, k.tgt, labels))
	}
	for _, e := range res.svcEdges {
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
	if net.ParseIP(host) != nil {
		if sk, hit := r.ipIndex[ipKey{cluster: anchorCluster, ip: host}]; hit {
			return sk.namespace, sk.service, true
		}
	}
	return "", "", false
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
// clientPod and peer feed ONLY the resolve-unknown-server-peer-labels
// enrichment: whenever the raw server label is literally "unknown" AND no real
// topology pod is found for it (UID empty, or UID present but absent from the
// global pod-UID index), resolution routes to resolveUnknownServerPeer instead
// of resolveEmptyUID or the synth-pod fallback below — D1's explicit
// carve-out, so the loosened server!~"user" selector does not leak
// external/unknown noise via the generic paths.
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
// enrichment" requirement (resolve-unknown-server-peer-labels D1-D3): the
// narrow carve-out that lets a literal server="unknown" endpoint resolve via
// the client-recorded peer-address labels client_net_peer_name /
// client_server_address instead of being unconditionally dropped — but only
// when the client side already resolved to a REAL (non-synthesised) topology
// pod, so the anchor cluster is unambiguous. clientPod nil (client
// unresolved, or resolved only to a synth pod) and "neither label present"
// both drop the endpoint (nil), byte-for-byte identical to the outcome under
// the old server!~"user|unknown" query-layer exclusion.
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
		return nil // neither client_net_peer_name nor client_server_address present
	}

	anchorCluster := clientPod.Labels()["cluster"]
	clientNamespace := clientPod.Labels()["namespace"]

	host, _ := splitPeerAddressPort(value)
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
