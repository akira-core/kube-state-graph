package graph

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	stC  = "c1"
	stOC = "ontap-prod"
)

func stPod(ns, name, uid, node string) *PodNode {
	labels := map[string]string{"cluster": stC, "namespace": ns}
	if node != "" {
		labels["node"] = K8sNodeID(stC, node)
	}
	return &PodNode{IDValue: PodID(stC, uid), NameValue: name, LabelsValue: labels}
}

func stNode(name string) *K8sNode {
	return &K8sNode{
		IDValue: K8sNodeID(stC, name), NameValue: name,
		LabelsValue: map[string]string{"cluster": stC},
	}
}

func stPVC(ns, claim string) *PVCNode {
	return &PVCNode{
		IDValue: PVCID(stC, ns, claim), NameValue: claim,
		LabelsValue: map[string]string{"cluster": stC, "namespace": ns},
	}
}

func stAggr(name, owner string) *NetAppAggrNode {
	labels := map[string]string{"ontap_cluster": stOC}
	if owner != "" {
		labels["node"] = owner
	}
	return &NetAppAggrNode{IDValue: NetAppAggrID(stOC, name), NameValue: name, LabelsValue: labels}
}

func stCtrl(name string) *NetAppNode {
	return &NetAppNode{
		IDValue: NetAppNodeID(stOC, name), NameValue: name,
		LabelsValue: map[string]string{"ontap_cluster": stOC},
	}
}

func stSVM(name string) *NetAppSVMNode {
	return &NetAppSVMNode{
		IDValue: NetAppSVMID(stOC, name), NameValue: name,
		LabelsValue: map[string]string{"ontap_cluster": stOC},
	}
}

func stF64(v float64) *float64 { return &v }

func stIO(readOps float64) *IOMetrics {
	return &IOMetrics{
		ReadOps:          stF64(readOps),
		WriteOps:         stF64(readOps / 2),
		ReadLatencyUs:    stF64(450),
		WriteLatencyUs:   stF64(200),
		ReadBytesPerSec:  stF64(readOps * 10),
		WriteBytesPerSec: stF64(readOps * 4),
		MaxIOPS:          stF64(5000),
		MaxBytesPerSec:   stF64(1048576),
	}
}

func stHop(tier, src, tgt string, extra map[string]string, io *IOMetrics) *Edge {
	labels := map[string]string{"tier": tier}
	for k, v := range extra {
		labels[k] = v
	}
	e := NewEdge(EdgeTypeStorageFlow, src, tgt, labels)
	if io != nil {
		e = e.WithIO(*io)
	}
	return e
}

func stChain(ctrl, aggr, svm, pvc, pod, node string, io *IOMetrics, nMounters int) []*Edge {
	var out []*Edge
	if ctrl != "" && aggr != "" {
		out = append(out, stHop(StorageTierNodeAggr, ctrl, aggr, nil, nil))
	}
	if aggr != "" && svm != "" {
		out = append(out, stHop(StorageTierAggrSVM, aggr, svm, nil, nil))
	}
	claimLabels := map[string]string{}
	if aggr != "" {
		claimLabels[ClaimAggrLabel] = aggr
	}
	out = append(out, stHop(StorageTierSVMPVC, svm, pvc, claimLabels, io))
	podLabels := map[string]string{}
	if nMounters > 1 {
		podLabels["attribution"] = AttributionSplit
	}
	out = append(out, stHop(StorageTierPVCPod, pvc, pod, podLabels, nil))
	if node != "" {
		out = append(out, stHop(StorageTierPodNode, pod, node, nil, nil))
	}
	return out
}

func stGraph(nodes []GraphNode, edges []*Edge) *Graph {
	return NewGraph(nodes, edges, time.Time{})
}

func viewIDs(v View) map[string]bool {
	out := map[string]bool{}
	for _, n := range v.Nodes {
		out[n.ID()] = true
	}
	return out
}

func viewTiers(v View) []string {
	out := make([]string, 0, len(v.Edges))
	for _, e := range v.Edges {
		out = append(out, e.Labels["tier"]+" "+e.Source+" -> "+e.Target)
	}
	return out
}

func edgeBetween(v View, src, tgt string) *Edge {
	for _, e := range v.Edges {
		if e.Source == src && e.Target == tgt {
			return e
		}
	}
	return nil
}

func scopeRoots(ontap, nodes, aggrs, svms, pods []string) StorageScope {
	s, err := NewStorageScope(nil, nil, ontap, nodes, aggrs, svms, pods)
	if err != nil {
		panic(err)
	}
	return s
}

// twoClaimsOnAggr1 is the spec's "Storage root finds its consumers" estate:
// aggr1 serves orders-data (read_ops 100, shop/orders-0 on worker-1) and
// catalog-data (read_ops 250, shop/catalog-0 on worker-2), both in svm_shop.
func twoClaimsOnAggr1() *Graph {
	ctrl, aggr, svm := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	orders, catalog := stPVC("shop", "orders-data"), stPVC("shop", "catalog-data")
	podA, podB := stPod("shop", "orders-0", "uid-1", "worker-1"), stPod("shop", "catalog-0", "uid-2", "worker-2")
	n1, n2 := stNode("worker-1"), stNode("worker-2")
	edges := stChain(ctrl.ID(), aggr.ID(), svm.ID(), orders.ID(), podA.ID(), n1.ID(), stIO(100), 1)
	edges = append(edges, stChain(ctrl.ID(), aggr.ID(), svm.ID(), catalog.ID(), podB.ID(), n2.ID(), stIO(250), 1)...)
	return stGraph([]GraphNode{ctrl, aggr, svm, orders, catalog, podA, podB, n1, n2}, edges)
}

func TestProjectStorage_StorageRootFindsItsConsumers(t *testing.T) {
	v := ProjectStorage(twoClaimsOnAggr1(), scopeRoots(nil, nil, []string{"aggr1"}, nil, nil))
	ids := viewIDs(v)
	assert.True(t, ids[NetAppNodeID(stOC, "ontap-prod-01")])
	assert.True(t, ids[NetAppAggrID(stOC, "aggr1")])
	assert.True(t, ids[NetAppSVMID(stOC, "svm_shop")])
	assert.True(t, ids[PVCID(stC, "shop", "orders-data")])
	assert.True(t, ids[PVCID(stC, "shop", "catalog-data")])
	assert.True(t, ids[PodID(stC, "uid-1")])
	assert.True(t, ids[PodID(stC, "uid-2")])
	assert.True(t, ids[K8sNodeID(stC, "worker-1")])
	assert.True(t, ids[K8sNodeID(stC, "worker-2")])
	assert.Len(t, v.Nodes, 9)
}

func TestProjectStorage_WorkloadRootFindsItsStorage(t *testing.T) {
	v := ProjectStorage(twoClaimsOnAggr1(), scopeRoots(nil, nil, nil, nil, []string{"shop/orders-0"}))
	ids := viewIDs(v)
	assert.True(t, ids[NetAppNodeID(stOC, "ontap-prod-01")])
	assert.True(t, ids[NetAppAggrID(stOC, "aggr1")])
	assert.True(t, ids[NetAppSVMID(stOC, "svm_shop")])
	assert.True(t, ids[PVCID(stC, "shop", "orders-data")])
	assert.True(t, ids[PodID(stC, "uid-1")])
	assert.True(t, ids[K8sNodeID(stC, "worker-1")])
	assert.False(t, ids[PodID(stC, "uid-2")], "catalog-0 is not on this path")
	assert.Equal(t, []string{
		"aggr-svm " + NetAppAggrID(stOC, "aggr1") + " -> " + NetAppSVMID(stOC, "svm_shop"),
		"node-aggr " + NetAppNodeID(stOC, "ontap-prod-01") + " -> " + NetAppAggrID(stOC, "aggr1"),
		"pod-node " + PodID(stC, "uid-1") + " -> " + K8sNodeID(stC, "worker-1"),
		"pvc-pod " + PVCID(stC, "shop", "orders-data") + " -> " + PodID(stC, "uid-1"),
		"svm-pvc " + NetAppSVMID(stOC, "svm_shop") + " -> " + PVCID(stC, "shop", "orders-data"),
	}, sortedTiers(v))
}

func sortedTiers(v View) []string {
	out := viewTiers(v)
	// viewTiers follows edge-id order (SortEdges); pin the contract as a set
	// via the same sort the assembler tests use.
	return sortedCopy(out)
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestProjectStorage_NodeMatchesBothTiers(t *testing.T) {
	// Controller n1 owns aggr-n; k8s node n1 hosts dual-0. A second path
	// through worker-1 must survive via the controller root OR the k8s root
	// — values of one selector are OR-combined.
	ctrlN, aggrN, svmN := stCtrl("n1"), stAggr("aggr-n", "n1"), stSVM("svm_n")
	pvcN, podN, knN := stPVC("shop", "n-data"), stPod("shop", "dual-0", "uid-n", "n1"), stNode("n1")
	ctrl1, aggr1, svm1 := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	pvcA, podA, knA := stPVC("shop", "orders-data"), stPod("shop", "orders-0", "uid-1", "worker-1"), stNode("worker-1")
	edges := stChain(ctrlN.ID(), aggrN.ID(), svmN.ID(), pvcN.ID(), podN.ID(), knN.ID(), stIO(1), 1)
	edges = append(edges, stChain(ctrl1.ID(), aggr1.ID(), svm1.ID(), pvcA.ID(), podA.ID(), knA.ID(), stIO(2), 1)...)
	g := stGraph([]GraphNode{ctrlN, aggrN, svmN, pvcN, podN, knN, ctrl1, aggr1, svm1, pvcA, podA, knA}, edges)

	v := ProjectStorage(g, scopeRoots(nil, []string{"n1"}, nil, nil, nil))
	ids := viewIDs(v)
	assert.True(t, ids[ctrlN.ID()], "controller n1 is a storage root")
	assert.True(t, ids[knN.ID()], "k8s node n1 is a workload root")
	assert.True(t, ids[podN.ID()], "path through controller n1")
	assert.False(t, ids[podA.ID()], "orders-0 touches neither n1")
}

func TestProjectStorage_RootsOnBothSidesIntersect(t *testing.T) {
	ctrl := stCtrl("ontap-prod-01")
	aggr1, aggr2 := stAggr("aggr1", "ontap-prod-01"), stAggr("aggr2", "ontap-prod-01")
	svm1, svm2 := stSVM("svm_shop"), stSVM("svm_other")
	orders, extra := stPVC("shop", "orders-data"), stPVC("shop", "extra-data")
	pod := stPod("shop", "orders-0", "uid-1", "worker-1")
	other := stPod("shop", "catalog-0", "uid-2", "worker-2")
	n1, n2 := stNode("worker-1"), stNode("worker-2")
	catalog := stPVC("shop", "catalog-data")
	edges := stChain(ctrl.ID(), aggr1.ID(), svm1.ID(), orders.ID(), pod.ID(), n1.ID(), stIO(100), 1)
	edges = append(edges, stChain(ctrl.ID(), aggr2.ID(), svm2.ID(), extra.ID(), pod.ID(), n1.ID(), stIO(10), 1)...)
	edges = append(edges, stChain(ctrl.ID(), aggr1.ID(), svm1.ID(), catalog.ID(), other.ID(), n2.ID(), stIO(250), 1)...)
	g := stGraph([]GraphNode{ctrl, aggr1, aggr2, svm1, svm2, orders, extra, catalog, pod, other, n1, n2}, edges)

	v := ProjectStorage(g, scopeRoots(nil, nil, []string{"aggr1"}, nil, []string{"shop/orders-0"}))
	ids := viewIDs(v)
	assert.True(t, ids[aggr1.ID()])
	assert.True(t, ids[orders.ID()])
	assert.True(t, ids[pod.ID()])
	assert.False(t, ids[extra.ID()], "aggr2 chain dropped by the intersection")
	assert.False(t, ids[aggr2.ID()])
	assert.False(t, ids[other.ID()], "other pods on aggr1 dropped")
}

func TestProjectStorage_NoRootReturnsTheEstate(t *testing.T) {
	v := ProjectStorage(twoClaimsOnAggr1(), StorageScope{})
	assert.Len(t, v.Edges, 8) // shared node-aggr + aggr-svm, then 2× (svm-pvc, pvc-pod, pod-node)
	assert.True(t, viewIDs(v)[PodID(stC, "uid-1")])
	assert.True(t, viewIDs(v)[PodID(stC, "uid-2")])
}

func TestProjectStorage_AggregateWithNoClaimsStillShows(t *testing.T) {
	ctrl := stCtrl("ontap-prod-02")
	aggr := stAggr("aggr9", "ontap-prod-02")
	g := stGraph([]GraphNode{ctrl, aggr, stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01")}, nil)

	v := ProjectStorage(g, scopeRoots(nil, nil, []string{"aggr9"}, nil, nil))
	ids := viewIDs(v)
	assert.True(t, ids[aggr.ID()])
	assert.True(t, ids[ctrl.ID()], "owning controller pulled so data.parent cannot dangle")
	assert.Empty(t, v.Edges)
	assert.False(t, ids[NetAppAggrID(stOC, "aggr1")])
}

func TestProjectStorage_PodWithNoNetAppClaimStillShows(t *testing.T) {
	pod := stPod("shop", "web-0", "uid-web", "worker-1")
	node := stNode("worker-1")
	g := stGraph([]GraphNode{pod, node, stCtrl("ontap-prod-01")}, nil)

	v := ProjectStorage(g, scopeRoots(nil, nil, nil, nil, []string{"shop/web-0"}))
	ids := viewIDs(v)
	assert.True(t, ids[pod.ID()])
	assert.False(t, ids[node.ID()], "an unscheduled-looking isolated pod pulls no node without a path")
	assert.Empty(t, v.Edges)
}

func TestProjectStorage_UnknownRootIsNotDrawn(t *testing.T) {
	v := ProjectStorage(twoClaimsOnAggr1(), scopeRoots(nil, nil, []string{"typo"}, nil, nil))
	assert.Empty(t, v.Nodes)
	assert.Empty(t, v.Edges)
}

func TestProjectStorage_UnknownPodRootIsNotDrawn(t *testing.T) {
	v := ProjectStorage(twoClaimsOnAggr1(), scopeRoots(nil, nil, nil, nil, []string{"shop/missing-0"}))
	assert.Empty(t, v.Nodes)
	assert.Empty(t, v.Edges)
}

func TestProjectStorage_UnmountedClaimDroppedWithLonelyAggregate(t *testing.T) {
	ctrl := stCtrl("ontap-prod-02")
	aggr := stAggr("aggr7", "ontap-prod-02")
	pvc := stPVC("shop", "idle-data")
	g := stGraph([]GraphNode{ctrl, aggr, pvc}, nil) // no edges: unmounted

	v := ProjectStorage(g, StorageScope{})
	assert.Empty(t, v.Nodes, "unmounted claim and its lonely aggregate are not roots")
}

func TestProjectStorage_NamespaceFilterNarrowsWorkloadSideOnly(t *testing.T) {
	ctrl := stCtrl("ontap-prod-01")
	aggr := stAggr("aggr1", "ontap-prod-01")
	svmShop, svmPlat := stSVM("svm_shop"), stSVM("svm_plat")
	orders, db := stPVC("shop", "orders-data"), stPVC("platform", "db-data")
	podS, podP := stPod("shop", "orders-0", "uid-1", "worker-1"), stPod("platform", "db-0", "uid-5", "worker-3")
	n1, n3 := stNode("worker-1"), stNode("worker-3")
	edges := stChain(ctrl.ID(), aggr.ID(), svmShop.ID(), orders.ID(), podS.ID(), n1.ID(), stIO(100), 1)
	edges = append(edges, stChain(ctrl.ID(), aggr.ID(), svmPlat.ID(), db.ID(), podP.ID(), n3.ID(), stIO(50), 1)...)
	g := stGraph([]GraphNode{ctrl, aggr, svmShop, svmPlat, orders, db, podS, podP, n1, n3}, edges)

	scope := scopeRoots(nil, nil, []string{"aggr1"}, nil, nil)
	scope.Namespaces = map[string]struct{}{"shop": {}}
	v := ProjectStorage(g, scope)
	ids := viewIDs(v)
	assert.True(t, ids[aggr.ID()], "storage root is never dropped by namespace")
	assert.True(t, ids[ctrl.ID()])
	assert.True(t, ids[orders.ID()])
	assert.True(t, ids[podS.ID()])
	assert.False(t, ids[db.ID()])
	assert.False(t, ids[podP.ID()])
	assert.False(t, ids[svmPlat.ID()])
}

func TestProjectStorage_FlexGroupClaimStartsAtSVM(t *testing.T) {
	svm := stSVM("svm_big")
	pvc := stPVC("shop", "big-data")
	pod := stPod("shop", "big-0", "uid-4", "worker-1")
	node := stNode("worker-1")
	edges := stChain("", "", svm.ID(), pvc.ID(), pod.ID(), node.ID(), nil, 1)
	g := stGraph([]GraphNode{svm, pvc, pod, node, stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01")}, edges)

	v := ProjectStorage(g, scopeRoots(nil, nil, nil, nil, []string{"shop/big-0"}))
	ids := viewIDs(v)
	assert.True(t, ids[svm.ID()])
	assert.True(t, ids[pvc.ID()])
	assert.True(t, ids[pod.ID()])
	assert.False(t, ids[NetAppAggrID(stOC, "aggr1")])
	for _, e := range v.Edges {
		assert.NotEqual(t, StorageTierNodeAggr, e.Labels["tier"])
		assert.NotEqual(t, StorageTierAggrSVM, e.Labels["tier"])
		assert.NotContains(t, e.Labels, ClaimAggrLabel)
	}
}

func TestProjectStorage_ClaimAggrLabelStrippedFromView(t *testing.T) {
	v := ProjectStorage(twoClaimsOnAggr1(), StorageScope{})
	for _, e := range v.Edges {
		assert.NotContains(t, e.Labels, ClaimAggrLabel)
	}
}

func TestProjectStorage_K8sNodeRootFindsStorageChains(t *testing.T) {
	v := ProjectStorage(twoClaimsOnAggr1(), scopeRoots(nil, []string{"worker-1"}, nil, nil, nil))
	ids := viewIDs(v)
	assert.True(t, ids[PodID(stC, "uid-1")])
	assert.True(t, ids[NetAppAggrID(stOC, "aggr1")])
	assert.False(t, ids[PodID(stC, "uid-2")])
}

func TestProjectStorage_RootOrderDoesNotMatter(t *testing.T) {
	g := twoClaimsOnAggr1()
	a := ProjectStorage(g, scopeRoots(nil, nil, []string{"aggr1", "missing"}, nil, nil))
	b := ProjectStorage(g, scopeRoots(nil, nil, []string{"missing", "aggr1", "aggr1"}, nil, nil))
	require.Equal(t, nodeIDList(a), nodeIDList(b))
	require.Equal(t, edgeIDList(a), edgeIDList(b))
}

func nodeIDList(v View) []string {
	out := make([]string, len(v.Nodes))
	for i, n := range v.Nodes {
		out[i] = n.ID()
	}
	return out
}

func edgeIDList(v View) []string {
	out := make([]string, len(v.Edges))
	for i, e := range v.Edges {
		out[i] = e.ID
	}
	return out
}

// --- weights (task 6.2) ---------------------------------------------------

func TestProjectStorage_WeightsConserveThroughTheChain(t *testing.T) {
	v := ProjectStorage(twoClaimsOnAggr1(), StorageScope{})
	nodeAggr := edgeBetween(v, NetAppNodeID(stOC, "ontap-prod-01"), NetAppAggrID(stOC, "aggr1"))
	require.NotNil(t, nodeAggr)
	require.NotNil(t, nodeAggr.IO)
	assert.InDelta(t, 350.0, *nodeAggr.IO.ReadOps, 1e-12)

	svm := NetAppSVMID(stOC, "svm_shop")
	a := edgeBetween(v, NetAppAggrID(stOC, "aggr1"), svm)
	require.NotNil(t, a.IO)
	assert.InDelta(t, 350.0, *a.IO.ReadOps, 1e-12)

	orders := edgeBetween(v, svm, PVCID(stC, "shop", "orders-data"))
	catalog := edgeBetween(v, svm, PVCID(stC, "shop", "catalog-data"))
	require.NotNil(t, orders.IO)
	require.NotNil(t, catalog.IO)
	assert.InDelta(t, 100.0, *orders.IO.ReadOps, 1e-12)
	assert.InDelta(t, 250.0, *catalog.IO.ReadOps, 1e-12)
}

func TestProjectStorage_RWXClaimSplitAcrossItsMounters(t *testing.T) {
	ctrl, aggr, svm := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	pvc := stPVC("shop", "shared-data")
	pods := []*PodNode{
		stPod("shop", "rwx-0", "uid-10", "worker-1"),
		stPod("shop", "rwx-1", "uid-11", "worker-1"),
		stPod("shop", "rwx-2", "uid-12", "worker-2"),
	}
	n1, n2 := stNode("worker-1"), stNode("worker-2")
	edges := make([]*Edge, 0, 3+2*len(pods))
	edges = append(edges, stHop(StorageTierNodeAggr, ctrl.ID(), aggr.ID(), nil, nil))
	edges = append(edges, stHop(StorageTierAggrSVM, aggr.ID(), svm.ID(), nil, nil))
	edges = append(edges, stHop(StorageTierSVMPVC, svm.ID(), pvc.ID(), map[string]string{ClaimAggrLabel: aggr.ID()}, stIO(300)))
	for _, p := range pods {
		edges = append(edges, stHop(StorageTierPVCPod, pvc.ID(), p.ID(), map[string]string{"attribution": AttributionSplit}, nil))
		nodeID := p.Labels()["node"]
		edges = append(edges, stHop(StorageTierPodNode, p.ID(), nodeID, nil, nil))
	}
	g := stGraph([]GraphNode{ctrl, aggr, svm, pvc, pods[0], pods[1], pods[2], n1, n2}, edges)

	v := ProjectStorage(g, StorageScope{})
	claim := edgeBetween(v, svm.ID(), pvc.ID())
	require.NotNil(t, claim.IO)
	assert.InDelta(t, 300.0, *claim.IO.ReadOps, 1e-12)
	assert.NotContains(t, claim.Labels, "attribution")

	splits := 0
	for _, e := range v.Edges {
		if e.Labels["tier"] != StorageTierPVCPod {
			continue
		}
		splits++
		assert.Equal(t, AttributionSplit, e.Labels["attribution"])
		require.NotNil(t, e.IO)
		assert.InDelta(t, 100.0, *e.IO.ReadOps, 1e-12)
	}
	assert.Equal(t, 3, splits)
}

func TestProjectStorage_RWXPodRootShowsHonestShare(t *testing.T) {
	ctrl, aggr, svm := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	pvc := stPVC("shop", "shared-data")
	pods := []*PodNode{
		stPod("shop", "rwx-0", "uid-10", "worker-1"),
		stPod("shop", "rwx-1", "uid-11", "worker-1"),
		stPod("shop", "rwx-2", "uid-12", "worker-2"),
	}
	n1, n2 := stNode("worker-1"), stNode("worker-2")
	edges := make([]*Edge, 0, 3+2*len(pods))
	edges = append(edges, stHop(StorageTierNodeAggr, ctrl.ID(), aggr.ID(), nil, nil))
	edges = append(edges, stHop(StorageTierAggrSVM, aggr.ID(), svm.ID(), nil, nil))
	edges = append(edges, stHop(StorageTierSVMPVC, svm.ID(), pvc.ID(), map[string]string{ClaimAggrLabel: aggr.ID()}, stIO(300)))
	for _, p := range pods {
		edges = append(edges, stHop(StorageTierPVCPod, pvc.ID(), p.ID(), map[string]string{"attribution": AttributionSplit}, nil))
		edges = append(edges, stHop(StorageTierPodNode, p.ID(), p.Labels()["node"], nil, nil))
	}
	g := stGraph([]GraphNode{ctrl, aggr, svm, pvc, pods[0], pods[1], pods[2], n1, n2}, edges)

	v := ProjectStorage(g, scopeRoots(nil, nil, nil, nil, []string{"shop/rwx-0"}))
	claim := edgeBetween(v, svm.ID(), pvc.ID())
	require.NotNil(t, claim.IO)
	assert.InDelta(t, 100.0, *claim.IO.ReadOps, 1e-12, "pod= root shows this pod's 1/n, carried up-tier")
	podEdge := edgeBetween(v, pvc.ID(), pods[0].ID())
	require.NotNil(t, podEdge.IO)
	assert.InDelta(t, 100.0, *podEdge.IO.ReadOps, 1e-12)
}

func TestProjectStorage_LatencyAndCeilingOnlyOnTheClaimLevelEdge(t *testing.T) {
	v := ProjectStorage(twoClaimsOnAggr1(), StorageScope{})
	for _, e := range v.Edges {
		require.NotNil(t, e.IO, "every hop of a measured path carries flow figures")
		if e.Labels["tier"] == StorageTierSVMPVC {
			require.NotNil(t, e.IO.ReadLatencyUs)
			require.NotNil(t, e.IO.MaxIOPS)
			assert.InDelta(t, 450.0, *e.IO.ReadLatencyUs, 1e-12)
			assert.InDelta(t, 5000.0, *e.IO.MaxIOPS, 1e-12)
			continue
		}
		assert.Nil(t, e.IO.ReadLatencyUs, e.Labels["tier"])
		assert.Nil(t, e.IO.WriteLatencyUs, e.Labels["tier"])
		assert.Nil(t, e.IO.MaxIOPS, e.Labels["tier"])
		assert.Nil(t, e.IO.MaxBytesPerSec, e.Labels["tier"])
	}
}

func TestProjectStorage_UnmeasuredClaimDrawsAWeightlessPath(t *testing.T) {
	ctrl, aggr, svm := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	plain, measured := stPVC("shop", "plain-data"), stPVC("shop", "orders-data")
	podP, podM := stPod("shop", "plain-0", "uid-p", "worker-1"), stPod("shop", "orders-0", "uid-1", "worker-1")
	node := stNode("worker-1")
	edges := stChain(ctrl.ID(), aggr.ID(), svm.ID(), plain.ID(), podP.ID(), node.ID(), nil, 1)
	edges = append(edges, stChain(ctrl.ID(), aggr.ID(), svm.ID(), measured.ID(), podM.ID(), node.ID(), stIO(100), 1)...)
	g := stGraph([]GraphNode{ctrl, aggr, svm, plain, measured, podP, podM, node}, edges)

	v := ProjectStorage(g, StorageScope{})
	plainEdge := edgeBetween(v, svm.ID(), plain.ID())
	require.NotNil(t, plainEdge)
	assert.Nil(t, plainEdge.IO, "unmeasured claim's own edge carries no metrics")
	shared := edgeBetween(v, ctrl.ID(), aggr.ID())
	require.NotNil(t, shared.IO)
	assert.InDelta(t, 100.0, *shared.IO.ReadOps, 1e-12, "shared hop carries only the measured claim")
}

func TestProjectStorage_NilGraph(t *testing.T) {
	v := ProjectStorage(nil, StorageScope{})
	assert.Empty(t, v.Nodes)
	assert.Empty(t, v.Edges)
}

// kube_pod_info names the node a pod runs on whether or not kube_node_info was
// read, so the assembler can emit a pod-node hop to a node the build never
// loaded. That must read like an unscheduled pod — the path ends at pvc-pod —
// and never delete the claim's whole chain.
func TestProjectStorage_UnloadedNodeEndsThePathAtThePod(t *testing.T) {
	ctrl, aggr, svm := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	pvc := stPVC("shop", "orders-data")
	pod := stPod("shop", "orders-0", "uid-1", "worker-9")
	phantom := K8sNodeID(stC, "worker-9") // no K8sNode for it in the graph
	edges := stChain(ctrl.ID(), aggr.ID(), svm.ID(), pvc.ID(), pod.ID(), phantom, stIO(100), 1)
	g := stGraph([]GraphNode{ctrl, aggr, svm, pvc, pod}, edges)

	v := ProjectStorage(g, StorageScope{})
	ids := viewIDs(v)
	for _, id := range []string{ctrl.ID(), aggr.ID(), svm.ID(), pvc.ID(), pod.ID()} {
		assert.True(t, ids[id], id)
	}
	assert.False(t, ids[phantom], "a node the build did not load is not drawn")
	require.Len(t, v.Edges, 4, "the path ends at pvc-pod")
	for _, e := range v.Edges {
		assert.NotEqual(t, StorageTierPodNode, e.Labels["tier"], "no hop to the phantom node")
	}

	rooted := ProjectStorage(g, scopeRoots(nil, nil, nil, nil, []string{"shop/orders-0"}))
	assert.True(t, viewIDs(rooted)[aggr.ID()], "a pod root still finds its storage")
}

// A claim whose QoS ops / data chunks degraded while its latency chunks
// answered has no flow to share out, but its own svm-pvc edge still carries
// the latency — exactly as /v1/graph's pvc-to-netapp-aggr edge does.
func TestProjectStorage_LatencyOnlyClaimKeepsLatencyOnItsEdge(t *testing.T) {
	ctrl, aggr, svm := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	pvc := stPVC("shop", "orders-data")
	pod := stPod("shop", "orders-0", "uid-1", "worker-1")
	node := stNode("worker-1")
	latencyOnly := &IOMetrics{ReadLatencyUs: stF64(450), WriteLatencyUs: stF64(200)}
	edges := stChain(ctrl.ID(), aggr.ID(), svm.ID(), pvc.ID(), pod.ID(), node.ID(), latencyOnly, 1)
	g := stGraph([]GraphNode{ctrl, aggr, svm, pvc, pod, node}, edges)

	v := ProjectStorage(g, StorageScope{})
	claim := edgeBetween(v, svm.ID(), pvc.ID())
	require.NotNil(t, claim)
	require.NotNil(t, claim.IO, "the claim-level edge keeps its latency")
	assert.InDelta(t, 450.0, *claim.IO.ReadLatencyUs, 1e-12)
	assert.InDelta(t, 200.0, *claim.IO.WriteLatencyUs, 1e-12)
	assert.Nil(t, claim.IO.ReadOps)
	assert.Nil(t, claim.IO.MaxIOPS)
	shared := edgeBetween(v, ctrl.ID(), aggr.ID())
	require.NotNil(t, shared)
	assert.Nil(t, shared.IO, "no flow figure exists to share up the chain")
}
