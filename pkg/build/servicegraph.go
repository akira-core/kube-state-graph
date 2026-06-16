package build

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/marz32one/kube-state-graph/pkg/graph"
	"github.com/marz32one/kube-state-graph/pkg/promql"
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
func ReadServiceGraph(
	ctx context.Context,
	q promql.Querier,
	r promql.Renderer,
	window time.Duration,
	end time.Time,
	topology Topology,
) (ServiceGraphResult, error) {
	vec, err := q.Instant(ctx,
		string(promql.QServiceGraphTotal),
		r.Render(promql.QServiceGraphTotal, window),
		end,
	)
	if err != nil {
		return ServiceGraphResult{}, fmt.Errorf("service-graph query: %w", err)
	}
	return parseServiceGraph(vec, topology), nil
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
	externals          map[string]*graph.ExternalNode
	synthPods          map[string]*graph.PodNode
	services           map[string]*graph.ServiceNode // keyed by service id
	svcEdges           map[string]*graph.Edge        // service-selects-pod, keyed by "svcID|podID"
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

func parseServiceGraph(vec model.Vector, topology Topology) ServiceGraphResult {
	if len(vec) == 0 {
		return ServiceGraphResult{}
	}

	// Per-metric tally of samples missing the `cluster` label; surfaced as one
	// aggregated warn at the end of the parse.
	mc := missingClusterCounts{}

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
	// fan-out (clusterFamilyKey("unknown") == "unknown" != "prod-0").
	svcCandidates := make(map[famSvcKey][]svcCandidate, len(topology.ServicesByNameNS))
	for k, obs := range topology.ServicesByNameNS {
		key := famSvcKey{family: clusterFamilyKey(k.cluster), namespace: k.namespace, service: k.service}
		svcCandidates[key] = append(svcCandidates[key], svcCandidate{cluster: k.cluster, obs: obs})
	}
	for _, cands := range svcCandidates {
		sort.Slice(cands, func(i, j int) bool { return cands[i].cluster < cands[j].cluster })
	}

	res := &sgResolver{
		endpointsByService: topology.EndpointsByService,
		podByID:            podByID,
		podByUID:           topology.PodsByUID,
		svcCandidates:      svcCandidates,
		externals:          map[string]*graph.ExternalNode{},
		synthPods:          map[string]*graph.PodNode{},
		services:           map[string]*graph.ServiceNode{},
		svcEdges:           map[string]*graph.Edge{},
	}

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

		clientUID, serverUID = normalizeSelfLoopUIDs(clientUID, serverUID, clientLabel, serverLabel)

		// Drop the series BEFORE any resolution when either side is wholly
		// empty (no UID, no label): no edge can exist, and resolving the
		// other side anyway would leak its materialisation side effects
		// (service / external nodes plus service-selects-pod fan-out) as an
		// orphan subgraph — including the cross-cluster service-selects-pod fan-out.
		if (clientUID == "" && clientLabel == "") || (serverUID == "" && serverLabel == "") {
			continue
		}

		// Each side resolves to a (possibly empty) slice of node IDs. With the
		// localised model a "://" endpoint resolves to AT MOST ONE service node —
		// in the caller's own (anchor) cluster — or to a single external node;
		// every other path also yields exactly one ID, and an empty slice drops
		// the side (and with it the series — the cross product below is empty).
		srcIDs, srcIsPod := res.resolveClient(clientLabel, traceCluster, clientUID, clientNS)

		// Anchor cluster for the server-side "://" path prefers the UID-recovered
		// client-pod cluster over the raw trace label: the label is frequently
		// missing (bucketed to "unknown") or disagrees with topology (see
		// resolveClient), and `.svc` DNS is resolved relative to the CALLER —
		// whose authoritative cluster is the resolved pod's, not the label's.
		// Falls back to the trace label when the client side is not a topology
		// pod. The "://" then resolves to a single service node in THIS cluster
		// (iff it holds the service), per the same-cluster rule. Edge
		// labels.cluster is unaffected (still the raw trace label, per D9).
		anchorCluster := traceCluster
		if srcIsPod && len(srcIDs) == 1 {
			if pod, ok := res.podByID[srcIDs[0]]; ok {
				if c := pod.Labels()["cluster"]; c != "" {
					anchorCluster = c
				}
			}
		}
		tgtIDs := res.resolveServer(serverLabel, anchorCluster, serverUID, serverNS)

		// Cross product: any resolved source × any resolved target. Each "://"
		// side now resolves to at most one (local) service node, so a both-"://"
		// series yields a single intra-cluster edge in the anchor cluster.
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
func (r *sgResolver) resolveEmptyUID(label, anchorCluster string) []string {
	if isConnString(label) {
		return r.resolveConnString(label, anchorCluster) // Stage 0 — service or external, never a pod
	}
	if label != "" {
		return []string{r.external(label)} // D27 fallback (non-URL only)
	}
	return nil // drop
}

// resolveClient resolves the client side of a service-graph series. Returns
// (ids, isPod). isPod is true when the resolved endpoint is a pod — real or
// synthesised from a non-empty UID. A "://" connection string resolves to a
// single service node in the anchor cluster, or an external node (never a pod).
// Every path returns at most one ID. The client side knows its cluster from
// the metric's `cluster` label.
func (r *sgResolver) resolveClient(label, traceCluster, podUID, namespace string) ([]string, bool) {
	if podUID == "" {
		return r.resolveEmptyUID(label, traceCluster), false
	}
	id := graph.PodID(traceCluster, podUID)
	if _, ok := r.podByID[id]; ok {
		return []string{id}, true
	}
	// The trace's `cluster` label is frequently missing (bucketed to "unknown")
	// or disagrees with the client pod's real topology cluster, so the
	// cluster-scoped podByID lookup misses even though the pod exists. Recover
	// the real pod via the global UID index — symmetric with resolveServer —
	// before minting a ghost, otherwise every client pod in a no-cluster-label
	// deployment would duplicate as an "unknown/<uid>" synth node. Only
	// synthesise when the UID is unknown to BOTH indexes.
	if pod, ok := r.podByUID[podUID]; ok {
		return []string{pod.ID()}, true
	}
	r.synthPod(id, traceCluster, namespace, podUID)
	return []string{id}, true
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
func (r *sgResolver) resolveServer(label, anchorCluster, podUID, namespace string) []string {
	if podUID == "" {
		return r.resolveEmptyUID(label, anchorCluster)
	}
	if pod, ok := r.podByUID[podUID]; ok {
		return []string{pod.ID()}
	}
	r.synthPod(graph.PodID("", podUID), "", namespace, podUID) // server cluster unknown
	return []string{graph.PodID("", podUID)}
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
func (r *sgResolver) resolveConnString(label, anchorCluster string) []string {
	if host := connStringHost(label); host != "" {
		if svc, ns, ok := classifyK8sDNS(host); ok {
			if ids := r.resolveServiceLevel(anchorCluster, ns, svc); len(ids) > 0 {
				return ids
			}
		}
	}
	// Unresolvable: not a parseable host, not a 2/3-label k8s .svc name, or the
	// anchor cluster does not hold the service in its own family → external node
	// (labels={}, verbatim label as name). Keeps truly-external URLs, unknown
	// in-cluster names, and calls whose own cluster lacks the service visible.
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
//     (clusterFamilyKey("unknown") == "unknown" is a family-of-one, so an
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
	cands := r.svcCandidates[famSvcKey{family: clusterFamilyKey(anchorCluster), namespace: ns, service: svc}]
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
		IDValue:        id,
		NameValue:      svc,
		LabelsValue:    map[string]string{"cluster": cluster, "namespace": ns},
		IPAddressValue: ips,
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
