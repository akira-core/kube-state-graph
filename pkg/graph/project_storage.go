package graph

import (
	"maps"
	"slices"
)

// ProjectStorage returns a View of the storage-flow graph constrained by
// scope. It does not mutate g.
//
// The algorithm is a pure function of (g, scope):
//
//  1. Extract flow units — one per (claim, mounting pod) — by walking each
//     svm-pvc edge up (aggr-svm, node-aggr; absent for a FlexGroup) and down
//     (pvc-pod, then that pod's pod-node).
//  2. Resolve roots to node-id sets. Exclusive storage parameters
//     (ontap_cluster / aggr / svm) and exclusive workload parameters (pod)
//     are AND-combined across sides. `node=` is one selector matched against
//     both tiers; its hits are OR-combined with each other (values of one
//     selector) and AND-combined with the exclusive sides. A requested
//     selector that resolved to nothing retains nothing — `?aggr=typo` is
//     empty, not the estate.
//  3. A unit is kept iff it hits every requested selector group and its
//     pod / PVC / K8s node pass the re-applied cluster / namespace filters.
//     Storage-side nodes are never dropped by those filters.
//  4. Nodes = ∪ retained units ∪ resolved root ids ∪ owning controllers of
//     admitted aggregates (pullNetAppParents). Edges = the retained units'
//     hops, weighted over those units (n = mounter count in the *built*
//     graph). SortNodes / SortEdges.
func ProjectStorage(g *Graph, scope StorageScope) View {
	if g == nil {
		return View{}
	}

	units := extractFlowUnits(g)
	storageIDs, workloadIDs, nodeIDs := resolveStorageRoots(g, scope)

	storageExclusive := len(scope.Roots.ONTAPClusters) > 0 ||
		len(scope.Roots.Aggrs) > 0 || len(scope.Roots.SVMs) > 0
	workloadExclusive := len(scope.Roots.Pods) > 0
	nodeRequested := len(scope.Roots.Nodes) > 0

	rawName := g.ClusterRawName
	retained := make([]flowUnit, 0, len(units))
	for _, u := range units {
		if storageExclusive && !u.intersects(storageIDs) {
			continue
		}
		if workloadExclusive && !u.intersects(workloadIDs) {
			continue
		}
		if nodeRequested && !u.intersects(nodeIDs) {
			continue
		}
		if !u.passesFilters(g, scope, rawName) {
			continue
		}
		retained = append(retained, u)
	}

	nodes := make(map[string]GraphNode, len(g.NodesByID))
	for _, u := range retained {
		for _, id := range u.ids {
			if n, ok := g.NodesByID[id]; ok {
				nodes[id] = n
			}
		}
	}
	// Roots always show when the upstream named them, even with no flow.
	// Workload roots still honour cluster / namespace; storage roots do not.
	for id := range storageIDs {
		admitRoot(g, nodes, id, false, scope, rawName)
	}
	for id := range workloadIDs {
		admitRoot(g, nodes, id, true, scope, rawName)
	}
	for id := range nodeIDs {
		n, ok := g.NodesByID[id]
		if !ok {
			continue
		}
		workload := n.Type() == NodeTypeK8sNode || n.Type() == NodeTypePod
		admitRoot(g, nodes, id, workload, scope, rawName)
	}
	pullNetAppParents(g, nodes)

	edges := weightRetained(retained)

	out := View{
		Nodes: make([]GraphNode, 0, len(nodes)),
		Edges: edges,
	}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, n)
	}
	SortNodes(out.Nodes)
	SortEdges(out.Edges)
	return out
}

func admitRoot(g *Graph, nodes map[string]GraphNode, id string, workload bool, scope StorageScope, rawName func(string) string) {
	if _, ok := nodes[id]; ok {
		return
	}
	n, ok := g.NodesByID[id]
	if !ok {
		return
	}
	if workload && !workloadPassesFilters(n, scope, rawName) {
		return
	}
	nodes[id] = n
}

// flowUnit is one (claim, mounting pod) path on the fixed tier chain. Aggr /
// controller are empty for a FlexGroup claim; the K8s node is empty for an
// unscheduled pod. ids lists every node on the path so root intersection and
// the retained node set are one walk.
type flowUnit struct {
	claimID, podID, nodeID string
	svmID, aggrID, ctrlID  string
	n                      int // mounter count in the built graph, not the view
	io                     *IOMetrics
	edges                  []*Edge
	ids                    []string
}

func (u flowUnit) intersects(ids map[string]struct{}) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range u.ids {
		if _, ok := ids[id]; ok {
			return true
		}
	}
	return false
}

func (u flowUnit) passesFilters(g *Graph, scope StorageScope, rawName func(string) string) bool {
	for _, id := range []string{u.claimID, u.podID, u.nodeID} {
		if id == "" {
			continue
		}
		n, ok := g.NodesByID[id]
		if !ok {
			return false
		}
		if !workloadPassesFilters(n, scope, rawName) {
			return false
		}
	}
	return true
}

// workloadPassesFilters is the cluster / namespace gate for the Kubernetes
// side of a storage-flow path. NetApp types never reach it — they belong to
// no Kubernetes cluster and carry no namespace, and a storage root is never
// dropped by these filters.
func workloadPassesFilters(n GraphNode, scope StorageScope, rawName func(string) string) bool {
	labels := n.Labels()
	if len(scope.Clusters) > 0 {
		switch n.Type() {
		case NodeTypeNetAppAggr, NodeTypeNetAppNode, NodeTypeNetAppSVM:
			// storage side: not filtered
		default:
			if _, ok := scope.Clusters[rawName(labels["cluster"])]; !ok {
				return false
			}
		}
	}
	if len(scope.Namespaces) > 0 {
		switch n.Type() {
		case NodeTypePod, NodeTypePVC:
			if _, ok := scope.Namespaces[labels["namespace"]]; !ok {
				return false
			}
		default:
			// K8s nodes and NetApp types carry no namespace; they follow the
			// pods that sit on them (units) or survive as roots.
		}
	}
	return true
}

func extractFlowUnits(g *Graph) []flowUnit {
	var svmPVC, pvcPod, podNode, nodeAggr, aggrSVM []*Edge
	for _, e := range g.Edges {
		if e.Type != EdgeTypeStorageFlow {
			continue
		}
		switch e.Labels["tier"] {
		case StorageTierSVMPVC:
			svmPVC = append(svmPVC, e)
		case StorageTierPVCPod:
			pvcPod = append(pvcPod, e)
		case StorageTierPodNode:
			podNode = append(podNode, e)
		case StorageTierNodeAggr:
			nodeAggr = append(nodeAggr, e)
		case StorageTierAggrSVM:
			aggrSVM = append(aggrSVM, e)
		}
	}

	ownerOf := make(map[string]string, len(nodeAggr))
	nodeAggrOf := make(map[string]*Edge, len(nodeAggr))
	for _, e := range nodeAggr {
		ownerOf[e.Target] = e.Source
		nodeAggrOf[e.Target] = e
	}
	aggrSVMOf := make(map[[2]string]*Edge, len(aggrSVM))
	incomingAggr := make(map[string][]*Edge, len(aggrSVM))
	for _, e := range aggrSVM {
		aggrSVMOf[[2]string{e.Source, e.Target}] = e
		incomingAggr[e.Target] = append(incomingAggr[e.Target], e)
	}

	mountersOf := make(map[string][]*Edge)
	for _, e := range pvcPod {
		mountersOf[e.Source] = append(mountersOf[e.Source], e)
	}
	nodeOf := make(map[string]*Edge, len(podNode))
	for _, e := range podNode {
		nodeOf[e.Source] = e
	}

	out := make([]flowUnit, 0, len(svmPVC))
	for _, claim := range svmPVC {
		pvcID, svmID := claim.Target, claim.Source
		mounters := mountersOf[pvcID]
		n := len(mounters)
		if n == 0 {
			// Unmounted claim: the assembler already suppresses these, but a
			// hand-built graph might still carry a dangling svm-pvc. No pod,
			// no flow unit.
			continue
		}
		aggrID := claimAggrOf(claim, incomingAggr[svmID])
		ctrlID := ownerOf[aggrID]
		for _, pe := range mounters {
			u := flowUnit{
				claimID: pvcID,
				podID:   pe.Target,
				svmID:   svmID,
				aggrID:  aggrID,
				ctrlID:  ctrlID,
				n:       n,
				io:      claim.IO,
			}
			u.edges = append(u.edges, claim, pe)
			u.ids = append(u.ids, svmID, pvcID, pe.Target)
			if aggrID != "" {
				u.ids = append(u.ids, aggrID)
				if e := aggrSVMOf[[2]string{aggrID, svmID}]; e != nil {
					u.edges = append(u.edges, e)
				}
			}
			if ctrlID != "" {
				u.ids = append(u.ids, ctrlID)
				if e := nodeAggrOf[aggrID]; e != nil {
					u.edges = append(u.edges, e)
				}
			}
			// The pod-node hop is attached only when its target is a loaded
			// node. kube_pod_info names the node a pod is scheduled on whether
			// or not kube_node_info was read (the `nodes` collector off, or the
			// node family lacking the az/env label every storage build filters
			// by), and the assembler emits the edge from that label alone. A
			// phantom target must read exactly like an unscheduled pod — the
			// path ends at pvc-pod — not delete the whole claim path.
			if ne := nodeOf[pe.Target]; ne != nil {
				if _, loaded := g.NodesByID[ne.Target]; loaded {
					u.nodeID = ne.Target
					u.edges = append(u.edges, ne)
					u.ids = append(u.ids, ne.Target)
				}
			}
			out = append(out, u)
		}
	}
	slices.SortFunc(out, cmpUnit)
	return out
}

func cmpUnit(a, b flowUnit) int {
	if a.claimID != b.claimID {
		if a.claimID < b.claimID {
			return -1
		}
		return 1
	}
	if a.podID < b.podID {
		return -1
	}
	if a.podID > b.podID {
		return 1
	}
	return 0
}

// claimAggrOf recovers the claim's aggregate. The assembler stamps it on the
// svm-pvc edge; a unique incoming aggr-svm is the fallback for hand-built
// graphs that omitted the key. Several incoming hops with no stamp cannot be
// disambiguated — the claim is treated as FlexGroup-shaped (no aggr) rather
// than guessed.
func claimAggrOf(claim *Edge, incoming []*Edge) string {
	if id := claim.Labels[ClaimAggrLabel]; id != "" {
		return id
	}
	if len(incoming) == 1 {
		return incoming[0].Source
	}
	return ""
}

func resolveStorageRoots(g *Graph, scope StorageScope) (storage, workload, nodeHits map[string]struct{}) {
	storage = map[string]struct{}{}
	workload = map[string]struct{}{}
	nodeHits = map[string]struct{}{}
	ocSet := scope.Roots.ONTAPClusters
	namedAggrOrSVM := len(scope.Roots.Aggrs) > 0 || len(scope.Roots.SVMs) > 0

	inOC := func(n GraphNode) bool {
		if len(ocSet) == 0 {
			return true
		}
		_, ok := ocSet[n.Labels()["ontap_cluster"]]
		return ok
	}

	for _, n := range g.NodesByID {
		switch n.Type() {
		case NodeTypeNetAppAggr:
			if _, ok := scope.Roots.Aggrs[n.Name()]; ok && inOC(n) {
				storage[n.ID()] = struct{}{}
			}
		case NodeTypeNetAppSVM:
			if _, ok := scope.Roots.SVMs[n.Name()]; ok && inOC(n) {
				storage[n.ID()] = struct{}{}
			}
		case NodeTypeNetAppNode:
			if _, ok := scope.Roots.Nodes[n.Name()]; ok && inOC(n) {
				nodeHits[n.ID()] = struct{}{}
			}
		case NodeTypeK8sNode:
			if _, ok := scope.Roots.Nodes[n.Name()]; ok {
				nodeHits[n.ID()] = struct{}{}
			}
		case NodeTypePod:
			ref := PodRef{Namespace: n.Labels()["namespace"], Name: n.Name()}
			if _, ok := scope.Roots.Pods[ref]; ok {
				workload[n.ID()] = struct{}{}
			}
		default:
			// service / external / PVC are never storage or workload roots
		}
	}

	// ontap_cluster used alone (no named aggr/svm) roots every NetApp entity
	// in those clusters. Combined with aggr=/svm= it only NARROWS those
	// named roots — "unless ontap_cluster narrows it".
	if len(ocSet) > 0 && !namedAggrOrSVM {
		for _, n := range g.NodesByID {
			switch n.Type() {
			case NodeTypeNetAppAggr, NodeTypeNetAppSVM, NodeTypeNetAppNode:
				if inOC(n) {
					storage[n.ID()] = struct{}{}
				}
			default:
				// Kubernetes types are not ONTAP-cluster-scoped
			}
		}
	}
	return storage, workload, nodeHits
}

// weightRetained sums each retained edge's I/O over the retained units that
// pass through it. A unit's share is the claim's four flow figures divided
// by n (mounter count in the built graph). svm-pvc keeps the claim's latency
// and ceiling verbatim. Unmeasured claims contribute nothing; an edge whose
// every unit is unmeasured carries no IO. Contributions are added in
// ascending (claim id, pod id) order so the sum is order-free.
//
// Edges are returned as new values (NewEdge + WithIO) so the built graph is
// never mutated and the internal claim_aggr label never leaves this package.
func weightRetained(retained []flowUnit) []*Edge {
	type acc struct {
		edge    *Edge
		contrib []flowUnit
	}
	byPair := map[[2]string]*acc{}
	for _, u := range retained {
		for _, e := range u.edges {
			key := [2]string{e.Source, e.Target}
			a, ok := byPair[key]
			if !ok {
				a = &acc{edge: e}
				byPair[key] = a
			}
			a.contrib = append(a.contrib, u)
		}
	}
	out := make([]*Edge, 0, len(byPair))
	for _, a := range byPair {
		slices.SortFunc(a.contrib, cmpUnit)
		io := sumUnitShares(a.edge, a.contrib)
		out = append(out, projectedEdge(a.edge, io))
	}
	return out
}

func sumUnitShares(edge *Edge, units []flowUnit) *IOMetrics {
	var acc IOMetrics
	filled := false
	var claimIO *IOMetrics
	claimTier := edge.Labels["tier"] == StorageTierSVMPVC
	for _, u := range units {
		share, ok := scaleFlow(u.io, u.n)
		if !ok {
			// A claim measured only for latency (the QoS ops / data chunks
			// degraded while the latency chunks answered) has no flow to
			// share out, but its own svm-pvc edge still carries the latency —
			// the same claim reports it on /v1/graph's pvc-to-netapp-aggr edge.
			if claimTier && hasLatency(u.io) {
				filled = true
				if claimIO == nil {
					claimIO = u.io
				}
			}
			continue
		}
		acc.ReadOps = addPtr(acc.ReadOps, share.ReadOps)
		acc.WriteOps = addPtr(acc.WriteOps, share.WriteOps)
		acc.ReadBytesPerSec = addPtr(acc.ReadBytesPerSec, share.ReadBytesPerSec)
		acc.WriteBytesPerSec = addPtr(acc.WriteBytesPerSec, share.WriteBytesPerSec)
		filled = true
		if claimIO == nil {
			claimIO = u.io
		}
	}
	if !filled {
		return nil
	}
	if claimTier && claimIO != nil {
		acc.ReadLatencyUs = claimIO.ReadLatencyUs
		acc.WriteLatencyUs = claimIO.WriteLatencyUs
		acc.MaxIOPS = claimIO.MaxIOPS
		acc.MaxBytesPerSec = claimIO.MaxBytesPerSec
	}
	return &acc
}

func hasLatency(io *IOMetrics) bool {
	return io != nil && (io.ReadLatencyUs != nil || io.WriteLatencyUs != nil)
}

func scaleFlow(io *IOMetrics, n int) (IOMetrics, bool) {
	var out IOMetrics
	if io == nil || n <= 0 {
		return out, false
	}
	f := float64(n)
	ok := false
	if io.ReadOps != nil {
		v := *io.ReadOps / f
		out.ReadOps = &v
		ok = true
	}
	if io.WriteOps != nil {
		v := *io.WriteOps / f
		out.WriteOps = &v
		ok = true
	}
	if io.ReadBytesPerSec != nil {
		v := *io.ReadBytesPerSec / f
		out.ReadBytesPerSec = &v
		ok = true
	}
	if io.WriteBytesPerSec != nil {
		v := *io.WriteBytesPerSec / f
		out.WriteBytesPerSec = &v
		ok = true
	}
	return out, ok
}

func addPtr(dst, src *float64) *float64 {
	if src == nil {
		return dst
	}
	if dst == nil {
		v := *src
		return &v
	}
	v := *dst + *src
	return &v
}

func projectedEdge(e *Edge, io *IOMetrics) *Edge {
	labels := maps.Clone(e.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	delete(labels, ClaimAggrLabel)
	out := NewEdge(e.Type, e.Source, e.Target, labels)
	if io != nil {
		out = out.WithIO(*io)
	}
	return out
}
