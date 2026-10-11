package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimEstate is the pvc / pv root corpus, all on aggr1 / svm_shop:
//
//	orders-data   PV pvc-orders   mounted by shop/orders-0 on worker-1   read_ops 100
//	catalog-data  PV pvc-catalog  mounted by shop/catalog-0 on worker-2  read_ops 250
//	orphan-data   PV pvc-orphan   mounted by nothing (a sink)            read_ops 40
//	stray-data    PV pvc-stray    mounted by nothing (a sink)            read_ops 7
//
// The storage-flow edges of the two unmounted claims are what a claim-seeded
// build emits; a build of any other root kind emits neither.
func claimEstate() *Graph {
	ctrl, aggr, svm := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	withPV := func(p *PVCNode, pv string) *PVCNode { p.LabelsValue["volumename"] = pv; return p }
	orders := withPV(stPVC("shop", "orders-data"), "pvc-orders")
	catalog := withPV(stPVC("shop", "catalog-data"), "pvc-catalog")
	orphan := withPV(stPVC("shop", "orphan-data"), "pvc-orphan")
	stray := withPV(stPVC("shop", "stray-data"), "pvc-stray")
	podA, podB := stPod("shop", "orders-0", "uid-1", "worker-1"), stPod("shop", "catalog-0", "uid-2", "worker-2")
	n1, n2 := stNode("worker-1"), stNode("worker-2")

	edges := stChain(ctrl.ID(), aggr.ID(), svm.ID(), orders.ID(), podA.ID(), n1.ID(), stIO(100), 1)
	edges = append(edges, stChain(ctrl.ID(), aggr.ID(), svm.ID(), catalog.ID(), podB.ID(), n2.ID(), stIO(250), 1)...)
	edges = append(edges, sinkChain(ctrl.ID(), aggr.ID(), svm.ID(), orphan.ID(), stIO(40))...)
	edges = append(edges, sinkChain(ctrl.ID(), aggr.ID(), svm.ID(), stray.ID(), stIO(7))...)
	return stGraph([]GraphNode{ctrl, aggr, svm, orders, catalog, orphan, stray, podA, podB, n1, n2}, edges)
}

// sinkChain is the storage-side path of a claim no pod mounts: the chain ends at
// the claim.
func sinkChain(ctrl, aggr, svm, pvc string, io *IOMetrics) []*Edge {
	return []*Edge{
		stHop(StorageTierNodeAggr, ctrl, aggr, nil, nil),
		stHop(StorageTierAggrSVM, aggr, svm, nil, nil),
		stHop(StorageTierSVMPVC, svm, pvc, map[string]string{ClaimAggrLabel: aggr}, io),
	}
}

func claimIDs(names ...string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = PVCID(stC, "shop", n)
	}
	return out
}

func idsOf(v View) []string { return nodeIDList(v) }

// Spec: "Claim root finds its storage and its consumers" — exactly the chain of
// the root claim, and no other claim sharing its aggregate or SVM.
func TestProjectStorage_ClaimRootFindsItsStorageAndConsumers(t *testing.T) {
	v := ProjectStorage(claimEstate(), scopeRoots(StorageRootPVC, "shop/orders-data"))

	ids := viewIDs(v)
	for _, id := range []string{
		NetAppNodeID(stOC, "ontap-prod-01"), NetAppAggrID(stOC, "aggr1"), NetAppSVMID(stOC, "svm_shop"),
		PVCID(stC, "shop", "orders-data"), PodID(stC, "uid-1"), K8sNodeID(stC, "worker-1"),
	} {
		assert.True(t, ids[id], "%s is on the root claim's path", id)
	}
	for _, id := range []string{
		PVCID(stC, "shop", "catalog-data"), PodID(stC, "uid-2"), K8sNodeID(stC, "worker-2"),
		PVCID(stC, "shop", "orphan-data"), PVCID(stC, "shop", "stray-data"),
	} {
		assert.False(t, ids[id], "%s shares the aggregate or SVM but is not the root claim", id)
	}
	assert.Equal(t, []string{
		"aggr-svm " + NetAppAggrID(stOC, "aggr1") + " -> " + NetAppSVMID(stOC, "svm_shop"),
		"node-aggr " + NetAppNodeID(stOC, "ontap-prod-01") + " -> " + NetAppAggrID(stOC, "aggr1"),
		"pod-node " + PodID(stC, "uid-1") + " -> " + K8sNodeID(stC, "worker-1"),
		"pvc-pod " + PVCID(stC, "shop", "orders-data") + " -> " + PodID(stC, "uid-1"),
		"svm-pvc " + NetAppSVMID(stOC, "svm_shop") + " -> " + PVCID(stC, "shop", "orders-data"),
	}, sortedTiers(v))

	// The weights are the ones a pod root on the same claim reports.
	pod := ProjectStorage(claimEstate(), scopeRoots(StorageRootPod, "shop/orders-0"))
	assert.Equal(t, sortedTiers(pod), sortedTiers(v), "?pvc= draws the paths ?pod= draws for that claim")
	assert.Equal(t, edgeIDList(pod), edgeIDList(v))
	assert.Equal(t, idsOf(pod), idsOf(v))
	nodeAggr := edgeBetween(v, NetAppNodeID(stOC, "ontap-prod-01"), NetAppAggrID(stOC, "aggr1"))
	require.NotNil(t, nodeAggr.IO)
	assert.InDelta(t, 100.0, *nodeAggr.IO.ReadOps, 1e-12, "only the root claim's measurement is summed")
}

// Spec: "Volume root resolves to its claim" — identical to the claim root.
func TestProjectStorage_VolumeRootEqualsItsClaimRoot(t *testing.T) {
	pv := ProjectStorage(claimEstate(), scopeRoots(StorageRootPV, "pvc-orders"))
	pvc := ProjectStorage(claimEstate(), scopeRoots(StorageRootPVC, "shop/orders-data"))
	assert.Equal(t, idsOf(pvc), idsOf(pv))
	assert.Equal(t, edgeIDList(pvc), edgeIDList(pv))
	assert.Equal(t, sortedTiers(pvc), sortedTiers(pv))
}

// Spec: "Unmounted root claim keeps its storage-side path".
func TestProjectStorage_UnmountedRootClaimKeepsItsStoragePath(t *testing.T) {
	for name, scope := range map[string]StorageScope{
		"pvc": scopeRoots(StorageRootPVC, "shop/orphan-data"),
		"pv":  scopeRoots(StorageRootPV, "pvc-orphan"),
	} {
		t.Run(name, func(t *testing.T) {
			v := ProjectStorage(claimEstate(), scope)
			assert.Equal(t, []string{
				"aggr-svm " + NetAppAggrID(stOC, "aggr1") + " -> " + NetAppSVMID(stOC, "svm_shop"),
				"node-aggr " + NetAppNodeID(stOC, "ontap-prod-01") + " -> " + NetAppAggrID(stOC, "aggr1"),
				"svm-pvc " + NetAppSVMID(stOC, "svm_shop") + " -> " + PVCID(stC, "shop", "orphan-data"),
			}, sortedTiers(v), "no pvc-pod or pod-node edge: the claim is the sink")
			assert.ElementsMatch(t, append(claimIDs("orphan-data"),
				NetAppNodeID(stOC, "ontap-prod-01"), NetAppAggrID(stOC, "aggr1"), NetAppSVMID(stOC, "svm_shop")),
				idsOf(v))

			// The claim's whole measurement rides its edge and every upstream hop.
			for _, e := range v.Edges {
				require.NotNil(t, e.IO, "%s carries the sink's measurement", e.Labels["tier"])
				assert.InDelta(t, 40.0, *e.IO.ReadOps, 1e-12)
			}
			claim := edgeBetween(v, NetAppSVMID(stOC, "svm_shop"), PVCID(stC, "shop", "orphan-data"))
			assert.NotNil(t, claim.IO.ReadLatencyUs)
			assert.NotNil(t, claim.IO.MaxIOPS, "latency and ceiling stay on the claim-level edge")
			upstream := edgeBetween(v, NetAppAggrID(stOC, "aggr1"), NetAppSVMID(stOC, "svm_shop"))
			assert.Nil(t, upstream.IO.ReadLatencyUs)
			assert.NotContains(t, claim.Labels, ClaimAggrLabel, "the internal label never leaves the package")
		})
	}
}

// Spec: "Unmounted root claim is a sink" — the mixed-request weights.
func TestProjectStorage_SinkAndMountedClaimWeightsInOneRequest(t *testing.T) {
	v := ProjectStorage(claimEstate(), scopeRoots(StorageRootPVC, "shop/orphan-data", "shop/orders-data"))
	svm := NetAppSVMID(stOC, "svm_shop")

	read := func(src, tgt string) float64 {
		e := edgeBetween(v, src, tgt)
		require.NotNil(t, e, "%s -> %s", src, tgt)
		require.NotNil(t, e.IO)
		return *e.IO.ReadOps
	}
	assert.InDelta(t, 140.0, read(NetAppNodeID(stOC, "ontap-prod-01"), NetAppAggrID(stOC, "aggr1")), 1e-12)
	assert.InDelta(t, 140.0, read(NetAppAggrID(stOC, "aggr1"), svm), 1e-12)
	assert.InDelta(t, 40.0, read(svm, PVCID(stC, "shop", "orphan-data")), 1e-12)
	assert.InDelta(t, 100.0, read(svm, PVCID(stC, "shop", "orders-data")), 1e-12)
	assert.InDelta(t, 100.0, read(PVCID(stC, "shop", "orders-data"), PodID(stC, "uid-1")), 1e-12)
	for _, e := range v.Edges {
		assert.NotEqual(t, PVCID(stC, "shop", "orphan-data"), e.Source, "no edge leaves the sink")
	}
}

// An unmounted claim that is not a root stays dropped under EVERY root kind,
// claim roots included, so no existing body changes.
func TestProjectStorage_UnmountedClaimThatIsNotARootStaysDropped(t *testing.T) {
	scopes := map[string]StorageScope{
		"aggr":        scopeRoots(StorageRootAggr, "aggr1"),
		"svm":         scopeRoots(StorageRootSVM, "svm_shop"),
		"ontap_node":  scopeRoots(StorageRootONTAPNode, "ontap-prod-01"),
		"ontap_cl":    scopeRoots(StorageRootONTAPCluster, stOC),
		"node":        scopeRoots(StorageRootNode, "worker-1"),
		"pod":         scopeRoots(StorageRootPod, "shop/orders-0"),
		"pvc (other)": scopeRoots(StorageRootPVC, "shop/orders-data"),
		"pv (other)":  scopeRoots(StorageRootPV, "pvc-catalog"),
	}
	for name, scope := range scopes {
		t.Run(name, func(t *testing.T) {
			ids := viewIDs(ProjectStorage(claimEstate(), scope))
			assert.False(t, ids[PVCID(stC, "shop", "orphan-data")], "orphan-data is a sink no root names")
			assert.False(t, ids[PVCID(stC, "shop", "stray-data")], "stray-data is a sink no root names")
		})
	}
}

// A hand-built graph carrying an unmounted chain still projects as it did before
// this change under every other root kind: the sink is retained through a claim
// root alone.
func TestProjectStorage_UnmountedChainAloneIsDroppedWithoutAClaimRoot(t *testing.T) {
	ctrl, aggr, svm := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	orphan := stPVC("shop", "orphan-data")
	g := stGraph([]GraphNode{ctrl, aggr, svm, orphan}, sinkChain(ctrl.ID(), aggr.ID(), svm.ID(), orphan.ID(), stIO(40)))

	for name, scope := range map[string]StorageScope{
		"aggr": scopeRoots(StorageRootAggr, "aggr1"),
		"svm":  scopeRoots(StorageRootSVM, "svm_shop"),
		"none": {},
	} {
		v := ProjectStorage(g, scope)
		assert.NotContains(t, viewIDs(v), orphan.ID(), name)
		assert.Empty(t, v.Edges, "%s: no storage-flow edge of an unmounted claim", name)
	}
	kept := ProjectStorage(g, scopeRoots(StorageRootPVC, "shop/orphan-data"))
	assert.Contains(t, viewIDs(kept), orphan.ID())
	assert.Len(t, kept.Edges, 3)
}

// A FlexGroup claim no pod mounts is a sink that enters the chain at its SVM.
func TestProjectStorage_UnmountedFlexGroupRootClaimStartsAtTheSVM(t *testing.T) {
	svm := stSVM("svm_big")
	big := stPVC("shop", "big-data")
	g := stGraph([]GraphNode{svm, big},
		[]*Edge{stHop(StorageTierSVMPVC, svm.ID(), big.ID(), nil, stIO(60))})

	v := ProjectStorage(g, scopeRoots(StorageRootPVC, "shop/big-data"))
	assert.Equal(t, []string{"svm-pvc " + svm.ID() + " -> " + big.ID()}, sortedTiers(v))
	assert.ElementsMatch(t, []string{svm.ID(), big.ID()}, idsOf(v))
}

// Spec: "Non-NetApp claim root still shows" — the claim alone, with no edge and
// no mounting pod.
func TestProjectStorage_NonNetAppClaimRootShowsAlone(t *testing.T) {
	cache := stPVC("shop", "cache")
	redis := stPod("shop", "redis-0", "uid-r", "worker-1")
	w1 := stNode("worker-1")
	g := stGraph([]GraphNode{cache, redis, w1}, nil)

	v := ProjectStorage(g, scopeRoots(StorageRootPVC, "shop/cache"))
	assert.Equal(t, []string{cache.ID()}, idsOf(v), "the mounting pod is not drawn")
	assert.Empty(t, v.Edges)

	pv := stPVC("shop", "cache2")
	pv.LabelsValue["volumename"] = "pv-static"
	g2 := stGraph([]GraphNode{pv}, nil)
	assert.Equal(t, []string{pv.ID()}, idsOf(ProjectStorage(g2, scopeRoots(StorageRootPV, "pv-static"))))
}

// Spec: "Unknown claim root is not drawn" — and a bare PV name never matches a
// claim's own name.
func TestProjectStorage_UnknownClaimRootIsNotDrawn(t *testing.T) {
	for name, scope := range map[string]StorageScope{
		"pvc typo":         scopeRoots(StorageRootPVC, "shop/typo"),
		"pvc wrong ns":     scopeRoots(StorageRootPVC, "platform/orders-data"),
		"pv typo":          scopeRoots(StorageRootPV, "pvc-typo"),
		"pv is not a name": scopeRoots(StorageRootPV, "orders-data"),
	} {
		t.Run(name, func(t *testing.T) {
			v := ProjectStorage(claimEstate(), scope)
			assert.Empty(t, v.Nodes)
			assert.Empty(t, v.Edges)
		})
	}
}

// Spec: "A claim root matches the claim in every cluster" — and `cluster` narrows
// it, exactly as it does every workload root.
func TestProjectStorage_ClaimRootMatchesEveryClusterAndNarrowsByCluster(t *testing.T) {
	const c2 = "c2"
	ctrl, aggr, svm := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	c1Claim := stPVC("shop", "data")
	c2Claim := &PVCNode{IDValue: PVCID(c2, "shop", "data"), NameValue: "data",
		LabelsValue: map[string]string{"cluster": c2, "namespace": "shop"}}
	g := stGraph([]GraphNode{ctrl, aggr, svm, c1Claim, c2Claim}, append(
		sinkChain(ctrl.ID(), aggr.ID(), svm.ID(), c1Claim.ID(), stIO(10)),
		sinkChain(ctrl.ID(), aggr.ID(), svm.ID(), c2Claim.ID(), stIO(20))...))

	both := ProjectStorage(g, scopeRoots(StorageRootPVC, "shop/data"))
	assert.Contains(t, viewIDs(both), c1Claim.ID())
	assert.Contains(t, viewIDs(both), c2Claim.ID())

	narrowed, err := NewStorageScope([]string{c2}, nil, StorageRootPVC, []string{"shop/data"})
	require.NoError(t, err)
	v := ProjectStorage(g, narrowed)
	assert.Contains(t, viewIDs(v), c2Claim.ID())
	assert.NotContains(t, viewIDs(v), c1Claim.ID())
}

// A namespace filter still narrows the workload side: a claim root outside it is
// not drawn.
func TestProjectStorage_ClaimRootHonoursTheNamespaceFilter(t *testing.T) {
	scope, err := NewStorageScope(nil, []string{"platform"}, StorageRootPVC, []string{"shop/orders-data"})
	require.NoError(t, err)
	v := ProjectStorage(claimEstate(), scope)
	assert.Empty(t, v.Nodes)
	assert.Empty(t, v.Edges)
}

// The order of roots and the order of the built graph's nodes never change the
// body.
func TestProjectStorage_ClaimRootOrderDoesNotMatter(t *testing.T) {
	a := ProjectStorage(claimEstate(), scopeRoots(StorageRootPVC, "shop/orders-data", "shop/orphan-data"))
	b := ProjectStorage(claimEstate(), scopeRoots(StorageRootPVC, "shop/orphan-data", "shop/orders-data"))
	assert.Equal(t, nodeIDList(a), nodeIDList(b))
	assert.Equal(t, edgeIDList(a), edgeIDList(b))
}

// The sink adds no node the view did not already hold as a claim on its path:
// every retained edge endpoint is a retained node.
func TestProjectStorage_ClaimRootViewHasNoDanglingEdge(t *testing.T) {
	for _, scope := range []StorageScope{
		scopeRoots(StorageRootPVC, "shop/orphan-data"),
		scopeRoots(StorageRootPVC, "shop/orphan-data", "shop/orders-data", "shop/catalog-data"),
		scopeRoots(StorageRootPV, "pvc-orphan", "pvc-stray"),
	} {
		v := ProjectStorage(claimEstate(), scope)
		ids := viewIDs(v)
		for _, e := range v.Edges {
			assert.True(t, ids[e.Source] && ids[e.Target], "%s -> %s", e.Source, e.Target)
		}
	}
}
