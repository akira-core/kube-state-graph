package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stIODelta is stIO with the six change fields set beside their base figures.
func stIODelta(readOps, readOpsDelta float64) *IOMetrics {
	io := stIO(readOps)
	io.ReadOpsDelta = stF64(readOpsDelta)
	io.WriteOpsDelta = stF64(readOpsDelta / 2)
	io.ReadLatencyUsDelta = stF64(25)
	io.WriteLatencyUsDelta = stF64(-5)
	io.ReadBytesPerSecDelta = stF64(readOpsDelta * 10)
	io.WriteBytesPerSecDelta = stF64(readOpsDelta * 4)
	return io
}

func twoClaimsWithDeltas(ioA, ioB *IOMetrics) *Graph {
	ctrl, aggr, svm := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	orders, catalog := stPVC("shop", "orders-data"), stPVC("shop", "catalog-data")
	podA, podB := stPod("shop", "orders-0", "uid-1", "worker-1"), stPod("shop", "catalog-0", "uid-2", "worker-2")
	n1, n2 := stNode("worker-1"), stNode("worker-2")
	edges := stChain(ctrl.ID(), aggr.ID(), svm.ID(), orders.ID(), podA.ID(), n1.ID(), ioA, 1)
	edges = append(edges, stChain(ctrl.ID(), aggr.ID(), svm.ID(), catalog.ID(), podB.ID(), n2.ID(), ioB, 1)...)
	return stGraph([]GraphNode{ctrl, aggr, svm, orders, catalog, podA, podB, n1, n2}, edges)
}

// Spec: "Changes sum up the chain".
func TestProjectStorage_DeltasSumUpTheChain(t *testing.T) {
	v := ProjectStorage(twoClaimsWithDeltas(stIODelta(100, 50), stIODelta(250, -20)),
		scopeRoots(StorageRootONTAPCluster, stOC))
	svm := NetAppSVMID(stOC, "svm_shop")

	nodeAggr := edgeBetween(v, NetAppNodeID(stOC, "ontap-prod-01"), NetAppAggrID(stOC, "aggr1"))
	require.NotNil(t, nodeAggr)
	require.NotNil(t, nodeAggr.IO)
	assert.InDelta(t, 30.0, *nodeAggr.IO.ReadOpsDelta, 1e-12)
	assert.InDelta(t, 15.0, *nodeAggr.IO.WriteOpsDelta, 1e-12)
	assert.InDelta(t, 300.0, *nodeAggr.IO.ReadBytesPerSecDelta, 1e-12)
	assert.InDelta(t, 120.0, *nodeAggr.IO.WriteBytesPerSecDelta, 1e-12)
	assert.Nil(t, nodeAggr.IO.ReadLatencyUsDelta, "latency deltas ride on svm-pvc only")
	assert.Nil(t, nodeAggr.IO.WriteLatencyUsDelta)

	aggrSVM := edgeBetween(v, NetAppAggrID(stOC, "aggr1"), svm)
	assert.InDelta(t, 30.0, *aggrSVM.IO.ReadOpsDelta, 1e-12)

	orders := edgeBetween(v, svm, PVCID(stC, "shop", "orders-data"))
	catalog := edgeBetween(v, svm, PVCID(stC, "shop", "catalog-data"))
	assert.InDelta(t, 50.0, *orders.IO.ReadOpsDelta, 1e-12)
	assert.InDelta(t, -20.0, *catalog.IO.ReadOpsDelta, 1e-12)
	assert.InDelta(t, 25.0, *orders.IO.ReadLatencyUsDelta, 1e-12, "latency delta copied on the claim-level edge")
	assert.InDelta(t, -5.0, *orders.IO.WriteLatencyUsDelta, 1e-12)

	for _, e := range v.Edges {
		if e.Labels["tier"] == StorageTierPVCPod && e.Target == PodID(stC, "uid-2") {
			assert.InDelta(t, -20.0, *e.IO.ReadOpsDelta, 1e-12, "a sole mounter carries the claim's change")
			assert.Nil(t, e.IO.ReadLatencyUsDelta)
		}
	}
}

// Spec: "A multiply-mounted claim's change is split".
func TestProjectStorage_RWXClaimDeltaIsSplitAcrossItsMounters(t *testing.T) {
	ctrl, aggr, svm := stCtrl("ontap-prod-01"), stAggr("aggr1", "ontap-prod-01"), stSVM("svm_shop")
	pvc := stPVC("shop", "shared-data")
	pods := []*PodNode{
		stPod("shop", "rwx-0", "uid-10", "worker-1"),
		stPod("shop", "rwx-1", "uid-11", "worker-1"),
		stPod("shop", "rwx-2", "uid-12", "worker-2"),
	}
	n1, n2 := stNode("worker-1"), stNode("worker-2")
	io := stIO(300)
	io.ReadOpsDelta = stF64(30)
	edges := make([]*Edge, 0, 3+2*len(pods))
	edges = append(edges,
		stHop(StorageTierNodeAggr, ctrl.ID(), aggr.ID(), nil, nil),
		stHop(StorageTierAggrSVM, aggr.ID(), svm.ID(), nil, nil),
		stHop(StorageTierSVMPVC, svm.ID(), pvc.ID(), map[string]string{ClaimAggrLabel: aggr.ID()}, io),
	)
	for _, p := range pods {
		edges = append(edges, stHop(StorageTierPVCPod, pvc.ID(), p.ID(), map[string]string{"attribution": AttributionSplit}, nil))
		edges = append(edges, stHop(StorageTierPodNode, p.ID(), p.Labels()["node"], nil, nil))
	}
	g := stGraph([]GraphNode{ctrl, aggr, svm, pvc, pods[0], pods[1], pods[2], n1, n2}, edges)

	v := ProjectStorage(g, scopeRoots(StorageRootONTAPCluster, stOC))
	claim := edgeBetween(v, svm.ID(), pvc.ID())
	assert.InDelta(t, 30.0, *claim.IO.ReadOpsDelta, 1e-12)
	splits := 0
	for _, e := range v.Edges {
		if e.Labels["tier"] != StorageTierPVCPod {
			continue
		}
		splits++
		assert.InDelta(t, 10.0, *e.IO.ReadOpsDelta, 1e-12)
		assert.Nil(t, e.IO.WriteOpsDelta, "a field with no delta gains none from the split")
	}
	assert.Equal(t, 3, splits)
}

// Spec: "A tier change covers the claims measured at both ends".
func TestProjectStorage_TierDeltaSumsOnlyTheClaimsThatCarryOne(t *testing.T) {
	a := stIO(100)
	a.ReadOpsDelta = stF64(50)
	b := stIO(200) // created inside the window: no start value
	v := ProjectStorage(twoClaimsWithDeltas(a, b), scopeRoots(StorageRootONTAPCluster, stOC))

	nodeAggr := edgeBetween(v, NetAppNodeID(stOC, "ontap-prod-01"), NetAppAggrID(stOC, "aggr1"))
	assert.InDelta(t, 300.0, *nodeAggr.IO.ReadOps, 1e-12)
	assert.InDelta(t, 50.0, *nodeAggr.IO.ReadOpsDelta, 1e-12)
	assert.Nil(t, nodeAggr.IO.WriteOpsDelta, "no claim carries it")

	catalog := edgeBetween(v, NetAppSVMID(stOC, "svm_shop"), PVCID(stC, "shop", "catalog-data"))
	assert.InDelta(t, 200.0, *catalog.IO.ReadOps, 1e-12)
	assert.Nil(t, catalog.IO.ReadOpsDelta)
}

// Spec: "A change field never appears without its base field".
func TestProjectStorage_NeverEmitsADeltaWithoutItsBaseField(t *testing.T) {
	stray := &IOMetrics{ // hand-built: deltas but no measurement
		ReadOpsDelta: stF64(5), ReadLatencyUsDelta: stF64(1), WriteBytesPerSecDelta: stF64(9),
	}
	partial := &IOMetrics{ReadOps: stF64(10), WriteOpsDelta: stF64(3), ReadLatencyUsDelta: stF64(2)}
	v := ProjectStorage(twoClaimsWithDeltas(stray, partial), scopeRoots(StorageRootONTAPCluster, stOC))
	for _, e := range v.Edges {
		if e.IO == nil {
			continue
		}
		io := e.IO
		for name, pair := range map[string][2]*float64{
			"read_ops":            {io.ReadOps, io.ReadOpsDelta},
			"write_ops":           {io.WriteOps, io.WriteOpsDelta},
			"read_latency_us":     {io.ReadLatencyUs, io.ReadLatencyUsDelta},
			"write_latency_us":    {io.WriteLatencyUs, io.WriteLatencyUsDelta},
			"read_bytes_per_sec":  {io.ReadBytesPerSec, io.ReadBytesPerSecDelta},
			"write_bytes_per_sec": {io.WriteBytesPerSec, io.WriteBytesPerSecDelta},
		} {
			if pair[1] != nil {
				assert.NotNilf(t, pair[0], "%s carries %s_delta without its base field", e.Labels["tier"], name)
			}
		}
	}
}
