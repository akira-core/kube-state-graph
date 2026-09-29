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
//     (pvc-pod, then that pod's pod-node). A claim no pod mounts yields one
//     SINK unit that ends at the claim, so its whole measurement rides the
//     path above it.
//  2. Resolve the one root kind to node-id sets. ontap_cluster roots every
//     NetApp entity of the filer; ontap_node roots the controller; aggr and
//     svm root that name on every filer; node roots the Kubernetes node;
//     pod roots the pod ref; pvc roots the claim by (namespace, name) and pv
//     the claim bound to that PersistentVolume (both materialised, and both
//     the ONLY roots that retain a sink unit); application roots pods
//     (materialised) and claims (retention only). A kind that resolved to
//     nothing retains nothing — `?aggr=typo` is empty, not the estate.
//  3. A unit is kept iff it touches a resolved root (or, for an application
//     root, a claim hit) and its pod / PVC / K8s node pass the re-applied
//     cluster / namespace filters. Storage-side nodes are never dropped by
//     those filters. A claim hit retains the unit; it does not materialise
//     the claim on its own. A sink unit is kept only when its claim IS a
//     pvc / pv root — never through a storage, workload or application root
//     it merely intersects — so an unmounted claim that is not a root stays
//     dropped under every kind.
//  4. Nodes = ∪ retained units ∪ resolved root ids ∪ owning controllers of
//     admitted aggregates (pullNetAppParents). Edges = the retained units'
//     hops, weighted over those units (n = mounter count in the *built*
//     graph). SortNodes / SortEdges.
func ProjectStorage(g *Graph, scope StorageScope) View {
	if g == nil {
		return View{}
	}

	units := extractFlowUnits(g)
	storageIDs, workloadIDs, claimHits, claimRoots := resolveStorageRoots(g, scope)

	rawName := g.ClusterRawName
	retained := make([]flowUnit, 0, len(units))
	for _, u := range units {
		if u.sink {
			// An unmounted claim is drawn only as the sink of a pvc / pv root
			// that names it.
			if _, ok := claimRoots[u.claimID]; !ok {
				continue
			}
		}
		if !u.intersects(storageIDs) && !u.intersects(workloadIDs) && !u.intersects(claimHits) {
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
//
// A sink unit is the path of a claim no pod mounts: podID and nodeID are empty
// and n is 1, so scaleFlow hands the claim's whole measurement to every hop
// above it. It is retained only through a claim root (see ProjectStorage).
type flowUnit struct {
	claimID, podID, nodeID string
	svmID, aggrID, ctrlID  string
	n                      int // mounter count in the built graph, not the view
	sink                   bool
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

	// The assembler stamps claim_aggr on every claim that has an aggregate, so
	// once one claim carries it an unstamped claim is FlexGroup-shaped, not a
	// hand-built graph that omitted the key.
	stamped := slices.ContainsFunc(svmPVC, func(e *Edge) bool {
		return e.Labels[ClaimAggrLabel] != ""
	})

	out := make([]flowUnit, 0, len(svmPVC))
	for _, claim := range svmPVC {
		pvcID, svmID := claim.Target, claim.Source
		mounters := mountersOf[pvcID]
		n := len(mounters)
		aggrID := claimAggrOf(claim, incomingAggr[svmID], stamped)
		ctrlID := ownerOf[aggrID]
		// addUpstream appends the hops above the claim: aggr-svm and node-aggr,
		// each only when the claim resolved an aggregate / its owner.
		addUpstream := func(u *flowUnit) {
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
		}
		if n == 0 {
			// Unmounted claim: no pod, so no flow through it — but a claim that
			// is itself a pvc / pv root keeps its storage-side path, ending at
			// the claim. The unit is a sink; ProjectStorage retains it only
			// through such a root, so a hand-built graph carrying a dangling
			// svm-pvc projects exactly as before under every other kind.
			u := flowUnit{
				claimID: pvcID,
				svmID:   svmID,
				aggrID:  aggrID,
				ctrlID:  ctrlID,
				n:       1,
				sink:    true,
				io:      claim.IO,
			}
			u.edges = append(u.edges, claim)
			u.ids = append(u.ids, svmID, pvcID)
			addUpstream(&u)
			out = append(out, u)
			continue
		}
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
			addUpstream(&u)
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
// svm-pvc edge of every claim that has one, so in a stamped graph an unstamped
// claim is FlexGroup-shaped (no aggr) — even when its SVM has a single incoming
// aggr-svm, which then belongs to another claim in the same SVM. A unique
// incoming aggr-svm is the fallback only for a hand-built graph that stamps no
// claim at all. Several incoming hops with no stamp cannot be disambiguated —
// the claim is treated as FlexGroup-shaped rather than guessed.
func claimAggrOf(claim *Edge, incoming []*Edge, stamped bool) string {
	if id := claim.Labels[ClaimAggrLabel]; id != "" {
		return id
	}
	if !stamped && len(incoming) == 1 {
		return incoming[0].Source
	}
	return ""
}

// resolveStorageRoots resolves the request's one root kind to node-id sets.
// storage and workload are the roots that are materialised (storage roots
// ignore the cluster / namespace filters, workload roots honour them);
// claimHits are claims retained through an application root and never
// materialised; claimRoots are the claims named by a pvc / pv root, a subset of
// workload, and the only ones whose sink unit a body retains.
func resolveStorageRoots(g *Graph, scope StorageScope) (storage, workload, claimHits, claimRoots map[string]struct{}) {
	storage = map[string]struct{}{}
	workload = map[string]struct{}{}
	claimHits = map[string]struct{}{}
	claimRoots = map[string]struct{}{}
	roots := scope.Roots
	if !roots.Any() {
		return storage, workload, claimHits, claimRoots
	}
	names := make(map[string]struct{}, len(roots.Names))
	for _, name := range roots.Names {
		names[name] = struct{}{}
	}
	pods := make(map[PodRef]struct{}, len(roots.Pods))
	for _, ref := range roots.Pods {
		pods[ref] = struct{}{}
	}
	claims := make(map[ClaimRef]struct{}, len(roots.Claims))
	for _, ref := range roots.Claims {
		claims[ref] = struct{}{}
	}
	named := func(n GraphNode) bool {
		_, ok := names[n.Name()]
		return ok
	}
	for _, n := range g.NodesByID {
		switch roots.Kind {
		case StorageRootONTAPCluster:
			switch n.Type() {
			case NodeTypeNetAppAggr, NodeTypeNetAppSVM, NodeTypeNetAppNode:
				if _, ok := names[n.Labels()["ontap_cluster"]]; ok {
					storage[n.ID()] = struct{}{}
				}
			default:
			}
		case StorageRootONTAPNode:
			if n.Type() == NodeTypeNetAppNode && named(n) {
				storage[n.ID()] = struct{}{}
			}
		case StorageRootAggr:
			if n.Type() == NodeTypeNetAppAggr && named(n) {
				storage[n.ID()] = struct{}{}
			}
		case StorageRootSVM:
			if n.Type() == NodeTypeNetAppSVM && named(n) {
				storage[n.ID()] = struct{}{}
			}
		case StorageRootNode:
			if n.Type() == NodeTypeK8sNode && named(n) {
				workload[n.ID()] = struct{}{}
			}
		case StorageRootPod:
			if n.Type() == NodeTypePod {
				ref := PodRef{Namespace: n.Labels()["namespace"], Name: n.Name()}
				if _, ok := pods[ref]; ok {
					workload[n.ID()] = struct{}{}
				}
			}
		case StorageRootPVC:
			if n.Type() == NodeTypePVC {
				ref := ClaimRef{Namespace: n.Labels()["namespace"], Name: n.Name()}
				if _, ok := claims[ref]; ok {
					workload[n.ID()] = struct{}{}
					claimRoots[n.ID()] = struct{}{}
				}
			}
		case StorageRootPV:
			// The claim bound to the named PersistentVolume: its volumename
			// label, never its own name, so a bare claim name matches nothing.
			if n.Type() == NodeTypePVC {
				if vn := n.Labels()["volumename"]; vn != "" {
					if _, ok := names[vn]; ok {
						workload[n.ID()] = struct{}{}
						claimRoots[n.ID()] = struct{}{}
					}
				}
			}
		case StorageRootApplication:
			app := n.Application()
			if app == "" {
				continue
			}
			if _, ok := names[app]; !ok {
				continue
			}
			switch n.Type() {
			case NodeTypePod:
				workload[n.ID()] = struct{}{}
			case NodeTypePVC:
				// A claim hit retains the path and is never materialised on
				// its own — admitRoot runs over storage and workload only.
				claimHits[n.ID()] = struct{}{}
			default:
			}
		}
	}
	return storage, workload, claimHits, claimRoots
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
		out.ReadOps = new(*io.ReadOps / f)
		ok = true
	}
	if io.WriteOps != nil {
		out.WriteOps = new(*io.WriteOps / f)
		ok = true
	}
	if io.ReadBytesPerSec != nil {
		out.ReadBytesPerSec = new(*io.ReadBytesPerSec / f)
		ok = true
	}
	if io.WriteBytesPerSec != nil {
		out.WriteBytesPerSec = new(*io.WriteBytesPerSec / f)
		ok = true
	}
	return out, ok
}

func addPtr(dst, src *float64) *float64 {
	if src == nil {
		return dst
	}
	if dst == nil {
		return new(*src)
	}
	return new(*dst + *src)
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
