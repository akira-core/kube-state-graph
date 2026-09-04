package build

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// --- fixture helpers ------------------------------------------------------

const (
	sfCluster = "zone-a-prod-c1"
	sfOC      = "ontap-prod"
	sfCtrl    = "ontap-prod-01"
)

func sfPod(name, uid, nodeName string) *graph.PodNode {
	labels := map[string]string{"cluster": sfCluster, "namespace": "shop"}
	if nodeName != "" {
		labels["node"] = graph.K8sNodeID(sfCluster, nodeName)
	}
	return &graph.PodNode{
		IDValue: graph.PodID(sfCluster, uid), NameValue: name, LabelsValue: labels,
	}
}

func sfNode(name string) *graph.K8sNode {
	return &graph.K8sNode{
		IDValue:     graph.K8sNodeID(sfCluster, name),
		NameValue:   name,
		LabelsValue: map[string]string{"cluster": sfCluster},
	}
}

func sfPVC(claim string) *graph.PVCNode {
	return &graph.PVCNode{
		IDValue:     graph.PVCID(sfCluster, "shop", claim),
		NameValue:   claim,
		LabelsValue: map[string]string{"cluster": sfCluster, "namespace": "shop"},
	}
}

func sfAggr(aggr, owner string) *graph.NetAppAggrNode {
	labels := map[string]string{"ontap_cluster": sfOC}
	if owner != "" {
		labels["node"] = owner
	}
	return &graph.NetAppAggrNode{
		IDValue: graph.NetAppAggrID(sfOC, aggr), NameValue: aggr, LabelsValue: labels,
	}
}

func sfCtrlNode(name string) *graph.NetAppNode {
	return &graph.NetAppNode{
		IDValue: graph.NetAppNodeID(sfOC, name), NameValue: name,
		LabelsValue: map[string]string{"ontap_cluster": sfOC},
	}
}

func sfSVM(svm string) *graph.NetAppSVMNode {
	return &graph.NetAppSVMNode{
		IDValue: graph.NetAppSVMID(sfOC, svm), NameValue: svm,
		LabelsValue: map[string]string{"ontap_cluster": sfOC},
	}
}

func f64(v float64) *float64 { return &v }

// sfIO is a claim measurement with every flow figure set, so a test can see
// which tier the weight landed on.
func sfIO(readOps float64) *graph.IOMetrics {
	return &graph.IOMetrics{
		ReadOps:       f64(readOps),
		WriteOps:      f64(readOps / 2),
		ReadLatencyUs: f64(450),
		MaxIOPS:       f64(5000),
	}
}

// sfEdge finds the storage-flow edge between two ids.
func sfEdge(t *testing.T, edges []*graph.Edge, source, target string) *graph.Edge {
	t.Helper()
	for _, e := range edges {
		if e.Source == source && e.Target == target {
			return e
		}
	}
	return nil
}

// tiersOf renders the emitted edges as sorted "<tier> <source> -> <target>"
// strings, which is the whole shape of a storage-flow body in one assertion.
func tiersOf(edges []*graph.Edge) []string {
	out := make([]string, len(edges))
	for i, e := range edges {
		out[i] = e.Labels["tier"] + " " + e.Source + " -> " + e.Target
	}
	sort.Strings(out)
	return out
}

// oneClaimTopology is the canonical estate: claim shop/orders-data on
// (ontap-prod, ontap-prod-01, aggr1, svm_shop), mounted by pod shop/orders-0
// scheduled on worker-1.
func oneClaimTopology(io *graph.IOMetrics) Topology {
	pvc := sfPVC("orders-data")
	pod := sfPod("orders-0", "uid-1", "worker-1")
	edge := graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, pvc.ID(), graph.NetAppAggrID(sfOC, "aggr1"), nil)
	if io != nil {
		edge = edge.WithIO(*io)
	}
	return Topology{
		Pods:  []*graph.PodNode{pod},
		Nodes: []*graph.K8sNode{sfNode("worker-1")},
		PVCs:  []*graph.PVCNode{pvc},
		NetAppInventory: NetAppInventory{
			Nodes: []*graph.NetAppNode{sfCtrlNode(sfCtrl)},
			Aggrs: []*graph.NetAppAggrNode{sfAggr("aggr1", sfCtrl)},
			SVMs:  []*graph.NetAppSVMNode{sfSVM("svm_shop")},
		},
		SVMByPVC:     map[string]SVMRef{pvc.ID(): {ONTAPCluster: sfOC, SVM: "svm_shop"}},
		StorageEdges: []*graph.Edge{edge},
		PodPVCs:      []PodPVCBinding{{PodID: pod.ID(), PVCID: pvc.ID()}},
	}
}

// --- the chain ------------------------------------------------------------

// The spec's "One claim draws one path": exactly five edges, one per tier,
// oriented storage → workload, and no edge of any other type.
func TestAssembleStorageFlow_OneClaimDrawsOnePath(t *testing.T) {
	_, edges := assembleStorageFlow(oneClaimTopology(nil))

	assert.Equal(t, []string{
		"aggr-svm netapp/ontap-prod/aggr/aggr1 -> netapp/ontap-prod/svm/svm_shop",
		"node-aggr netapp/ontap-prod/ontap-prod-01 -> netapp/ontap-prod/aggr/aggr1",
		"pod-node " + graph.PodID(sfCluster, "uid-1") + " -> " + graph.K8sNodeID(sfCluster, "worker-1"),
		"pvc-pod " + graph.PVCID(sfCluster, "shop", "orders-data") + " -> " + graph.PodID(sfCluster, "uid-1"),
		"svm-pvc netapp/ontap-prod/svm/svm_shop -> " + graph.PVCID(sfCluster, "shop", "orders-data"),
	}, tiersOf(edges))

	for _, e := range edges {
		assert.Equal(t, graph.EdgeTypeStorageFlow, e.Type)
	}
}

// Every emitted node type is present, and nothing the storage body forbids is.
func TestAssembleStorageFlow_NodeSet(t *testing.T) {
	nodes, _ := assembleStorageFlow(oneClaimTopology(nil))

	kinds := map[graph.NodeType]int{}
	for _, n := range nodes {
		kinds[n.Type()]++
	}
	assert.Equal(t, map[graph.NodeType]int{
		graph.NodeTypePod:        1,
		graph.NodeTypeK8sNode:    1,
		graph.NodeTypePVC:        1,
		graph.NodeTypeNetAppNode: 1,
		graph.NodeTypeNetAppAggr: 1,
		graph.NodeTypeNetAppSVM:  1,
	}, kinds)
	assert.NotContains(t, kinds, graph.NodeTypeService)
	assert.NotContains(t, kinds, graph.NodeTypeExternal)
}

// The spec's "Shared upstream hops are emitted once": two claims on one
// aggregate in one SVM share the node-aggr and aggr-svm edges and get their
// own downstream tiers. The projection then sums both weights onto the shared
// hops.
func TestAssembleStorageFlow_SharedUpstreamHopsEmittedOnce(t *testing.T) {
	pvcA, pvcB := sfPVC("orders-data"), sfPVC("catalog-data")
	podA, podB := sfPod("orders-0", "uid-1", "worker-1"), sfPod("catalog-0", "uid-2", "worker-2")
	aggrID := graph.NetAppAggrID(sfOC, "aggr1")

	tp := Topology{
		Pods:  []*graph.PodNode{podA, podB},
		Nodes: []*graph.K8sNode{sfNode("worker-1"), sfNode("worker-2")},
		PVCs:  []*graph.PVCNode{pvcA, pvcB},
		NetAppInventory: NetAppInventory{
			Nodes: []*graph.NetAppNode{sfCtrlNode(sfCtrl)},
			Aggrs: []*graph.NetAppAggrNode{sfAggr("aggr1", sfCtrl)},
			SVMs:  []*graph.NetAppSVMNode{sfSVM("svm_shop")},
		},
		SVMByPVC: map[string]SVMRef{
			pvcA.ID(): {ONTAPCluster: sfOC, SVM: "svm_shop"},
			pvcB.ID(): {ONTAPCluster: sfOC, SVM: "svm_shop"},
		},
		StorageEdges: []*graph.Edge{
			graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, pvcA.ID(), aggrID, nil),
			graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, pvcB.ID(), aggrID, nil),
		},
		PodPVCs: []PodPVCBinding{
			{PodID: podA.ID(), PVCID: pvcA.ID()},
			{PodID: podB.ID(), PVCID: pvcB.ID()},
		},
	}
	_, edges := assembleStorageFlow(tp)

	byTier := map[string]int{}
	for _, e := range edges {
		byTier[e.Labels["tier"]]++
	}
	assert.Equal(t, map[string]int{
		graph.StorageTierNodeAggr: 1,
		graph.StorageTierAggrSVM:  1,
		graph.StorageTierSVMPVC:   2,
		graph.StorageTierPVCPod:   2,
		graph.StorageTierPodNode:  2,
	}, byTier)
}

// The spec's "FlexGroup claim starts at the SVM": a claim whose matched series
// carries an SVM but an empty aggr has no aggregate to hang off, so its path
// begins at svm-pvc and no node-aggr / aggr-svm edge is emitted for it.
func TestAssembleStorageFlow_FlexGroupStartsAtTheSVM(t *testing.T) {
	pvc, pod := sfPVC("big-data"), sfPod("big-0", "uid-1", "worker-1")
	tp := Topology{
		Pods:  []*graph.PodNode{pod},
		Nodes: []*graph.K8sNode{sfNode("worker-1")},
		PVCs:  []*graph.PVCNode{pvc},
		NetAppInventory: NetAppInventory{
			SVMs: []*graph.NetAppSVMNode{sfSVM("svm_big")},
		},
		SVMByPVC: map[string]SVMRef{pvc.ID(): {ONTAPCluster: sfOC, SVM: "svm_big"}},
		// No pvc-to-netapp-aggr edge at all — the FlexGroup shape.
		PodPVCs: []PodPVCBinding{{PodID: pod.ID(), PVCID: pvc.ID()}},
	}
	_, edges := assembleStorageFlow(tp)

	assert.Equal(t, []string{
		"pod-node " + graph.PodID(sfCluster, "uid-1") + " -> " + graph.K8sNodeID(sfCluster, "worker-1"),
		"pvc-pod " + graph.PVCID(sfCluster, "shop", "big-data") + " -> " + graph.PodID(sfCluster, "uid-1"),
		"svm-pvc netapp/ontap-prod/svm/svm_big -> " + graph.PVCID(sfCluster, "shop", "big-data"),
	}, tiersOf(edges))
}

// A claim with no resolved SVM contributes NO path — not even the aggregate
// hop it did resolve. The tier chain is fixed and an aggr → pvc shortcut is not
// permitted, so the claim is counted like a topology miss for this graph.
func TestAssembleStorageFlow_EmptySVMDrawsNoPath(t *testing.T) {
	tp := oneClaimTopology(nil)
	pvcID := graph.PVCID(sfCluster, "shop", "orders-data")
	tp.SVMByPVC = map[string]SVMRef{pvcID: {ONTAPCluster: sfOC, SVM: ""}}

	_, edges := assembleStorageFlow(tp)
	assert.Empty(t, edges, "no SVM, no path — the chain has no aggr -> pvc shortcut")

	// The nodes are still materialised: an aggregate with no drawable path is
	// exactly the flowless entity ProjectStorage drops unless it is a root.
	nodes, _ := assembleStorageFlow(tp)
	assert.NotEmpty(t, nodes)
}

// An unmounted claim draws no path: with no pod there is no Sankey flow, and a
// storage-side stub ending at the PVC would be a dangling half-path.
func TestAssembleStorageFlow_UnmountedClaimDrawsNoPath(t *testing.T) {
	tp := oneClaimTopology(nil)
	tp.PodPVCs = nil

	_, edges := assembleStorageFlow(tp)
	assert.Empty(t, edges)
}

// An unscheduled pod ends its path at pvc-pod: it carries no labels.node, so
// there is no Kubernetes node to reach.
func TestAssembleStorageFlow_UnscheduledPodEndsAtPVCPod(t *testing.T) {
	pvc, pod := sfPVC("orders-data"), sfPod("orders-0", "uid-1", "")
	tp := oneClaimTopology(nil)
	tp.Pods = []*graph.PodNode{pod}
	tp.Nodes = nil
	tp.PodPVCs = []PodPVCBinding{{PodID: pod.ID(), PVCID: pvc.ID()}}

	_, edges := assembleStorageFlow(tp)

	tiers := map[string]int{}
	for _, e := range edges {
		tiers[e.Labels["tier"]]++
	}
	assert.Equal(t, 1, tiers[graph.StorageTierPVCPod])
	assert.Zero(t, tiers[graph.StorageTierPodNode], "an unscheduled pod has no node hop")
}

// --- weights and attribution ----------------------------------------------

// The build bakes exactly ONE weight, on the claim-level svm-pvc edge. Every
// other tier is weightless here; the projection sums them over the RETAINED
// units, which is what makes conservation true in every view.
func TestAssembleStorageFlow_WeightOnlyOnTheClaimEdge(t *testing.T) {
	_, edges := assembleStorageFlow(oneClaimTopology(sfIO(300)))

	pvcID := graph.PVCID(sfCluster, "shop", "orders-data")
	claim := sfEdge(t, edges, graph.NetAppSVMID(sfOC, "svm_shop"), pvcID)
	require.NotNil(t, claim)
	require.NotNil(t, claim.IO)
	assert.InDelta(t, 300.0, *claim.IO.ReadOps, 1e-12)
	assert.InDelta(t, 450.0, *claim.IO.ReadLatencyUs, 1e-12)
	assert.InDelta(t, 5000.0, *claim.IO.MaxIOPS, 1e-12)

	for _, e := range edges {
		if e == claim {
			continue
		}
		assert.Nilf(t, e.IO, "tier %q must be weightless at build time", e.Labels["tier"])
	}
}

// An unmeasured claim draws its whole path weightless.
func TestAssembleStorageFlow_UnmeasuredClaimIsWeightless(t *testing.T) {
	_, edges := assembleStorageFlow(oneClaimTopology(nil))
	require.NotEmpty(t, edges)
	for _, e := range edges {
		assert.Nil(t, e.IO)
	}
}

// A claim mounted by several pods gets one pvc-pod edge per mounter, each
// marked attribution="split" — per-pod I/O is not observable, so the split is
// an attribution rather than a measurement and must say so.
func TestAssembleStorageFlow_RWXMountersAreMarkedSplit(t *testing.T) {
	pvc := sfPVC("shared-data")
	pods := []*graph.PodNode{
		sfPod("web-0", "uid-1", "worker-1"),
		sfPod("web-1", "uid-2", "worker-1"),
		sfPod("web-2", "uid-3", "worker-2"),
	}
	tp := oneClaimTopology(sfIO(300))
	tp.Pods = pods
	tp.Nodes = []*graph.K8sNode{sfNode("worker-1"), sfNode("worker-2")}
	tp.PVCs = []*graph.PVCNode{pvc}
	tp.SVMByPVC = map[string]SVMRef{pvc.ID(): {ONTAPCluster: sfOC, SVM: "svm_shop"}}
	tp.StorageEdges = []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, pvc.ID(), graph.NetAppAggrID(sfOC, "aggr1"), nil).
			WithIO(*sfIO(300)),
	}
	tp.PodPVCs = nil
	for _, p := range pods {
		tp.PodPVCs = append(tp.PodPVCs, PodPVCBinding{PodID: p.ID(), PVCID: pvc.ID()})
	}

	_, edges := assembleStorageFlow(tp)

	split := 0
	for _, e := range edges {
		if e.Labels["tier"] != graph.StorageTierPVCPod {
			assert.NotContainsf(t, e.Labels, "attribution",
				"tier %q must never be marked split", e.Labels["tier"])
			continue
		}
		split++
		assert.Equal(t, graph.AttributionSplit, e.Labels["attribution"])
	}
	assert.Equal(t, 3, split, "one pvc-pod edge per mounter")
}

// A singly-mounted claim carries NO attribution key at all — absent, not
// "single". The key means "this weight was attributed, not measured".
func TestAssembleStorageFlow_SingleMounterHasNoAttribution(t *testing.T) {
	_, edges := assembleStorageFlow(oneClaimTopology(sfIO(300)))
	for _, e := range edges {
		assert.NotContains(t, e.Labels, "attribution")
	}
}

// --- determinism ----------------------------------------------------------

// Edge ids are UUIDv5 over (type|source|target), so the same estate built twice
// yields byte-identical ids — and shuffling the topology's slices cannot change
// the output, since every loop is over a sorted or set-keyed collection.
func TestAssembleStorageFlow_DeterministicAcrossRuns(t *testing.T) {
	build := func() ([]string, []string) {
		nodes, edges := assembleStorageFlow(oneClaimTopology(sfIO(300)))
		nodeIDs := make([]string, len(nodes))
		for i, n := range nodes {
			nodeIDs[i] = n.ID()
		}
		edgeIDs := make([]string, len(edges))
		for i, e := range edges {
			edgeIDs[i] = e.ID + "|" + e.Labels["tier"]
		}
		return nodeIDs, edgeIDs
	}
	n1, e1 := build()
	n2, e2 := build()
	assert.Equal(t, n1, n2)
	assert.Equal(t, e1, e2)
	assert.NotEmpty(t, e1)
}

// Two claims reaching the same shared hop in either order produce the identical
// edge set — the de-duplication is an insert-only set, not a first-wins race.
func TestAssembleStorageFlow_ChainOrderDoesNotMatter(t *testing.T) {
	makeTopology := func(reverse bool) Topology {
		pvcA, pvcB := sfPVC("a-data"), sfPVC("b-data")
		podA, podB := sfPod("a-0", "uid-1", "worker-1"), sfPod("b-0", "uid-2", "worker-1")
		aggrID := graph.NetAppAggrID(sfOC, "aggr1")
		tp := Topology{
			Pods:  []*graph.PodNode{podA, podB},
			Nodes: []*graph.K8sNode{sfNode("worker-1")},
			PVCs:  []*graph.PVCNode{pvcA, pvcB},
			NetAppInventory: NetAppInventory{
				Nodes: []*graph.NetAppNode{sfCtrlNode(sfCtrl)},
				Aggrs: []*graph.NetAppAggrNode{sfAggr("aggr1", sfCtrl)},
				SVMs:  []*graph.NetAppSVMNode{sfSVM("svm_shop")},
			},
			SVMByPVC: map[string]SVMRef{
				pvcA.ID(): {ONTAPCluster: sfOC, SVM: "svm_shop"},
				pvcB.ID(): {ONTAPCluster: sfOC, SVM: "svm_shop"},
			},
			StorageEdges: []*graph.Edge{
				graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, pvcA.ID(), aggrID, nil),
				graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, pvcB.ID(), aggrID, nil),
			},
			PodPVCs: []PodPVCBinding{
				{PodID: podA.ID(), PVCID: pvcA.ID()},
				{PodID: podB.ID(), PVCID: pvcB.ID()},
			},
		}
		if reverse {
			tp.StorageEdges[0], tp.StorageEdges[1] = tp.StorageEdges[1], tp.StorageEdges[0]
			tp.PodPVCs[0], tp.PodPVCs[1] = tp.PodPVCs[1], tp.PodPVCs[0]
		}
		return tp
	}

	_, fwd := assembleStorageFlow(makeTopology(false))
	_, rev := assembleStorageFlow(makeTopology(true))
	assert.Equal(t, tiersOf(fwd), tiersOf(rev))
}

// The node-aggr tier names the SAME controller the aggregate's own compound
// parent does, because both read the aggregate's labels.node. After an HA
// takeover the Sankey path and the compound hierarchy must not disagree.
func TestAssembleStorageFlow_NodeAggrFollowsTheAggregateOwnerLabel(t *testing.T) {
	tp := oneClaimTopology(nil)
	tp.NetAppInventory.Aggrs = []*graph.NetAppAggrNode{sfAggr("aggr1", "ontap-prod-02")}
	tp.NetAppInventory.Nodes = []*graph.NetAppNode{sfCtrlNode("ontap-prod-02")}

	_, edges := assembleStorageFlow(tp)
	e := sfEdge(t, edges, graph.NetAppNodeID(sfOC, "ontap-prod-02"), graph.NetAppAggrID(sfOC, "aggr1"))
	require.NotNil(t, e, "the node-aggr hop follows the current owner")
	assert.Equal(t, graph.StorageTierNodeAggr, e.Labels["tier"])
}

// An owner-less aggregate emits no node-aggr hop rather than an edge from an
// empty id — the claim's path simply starts at the aggregate.
func TestAssembleStorageFlow_OwnerlessAggrHasNoNodeHop(t *testing.T) {
	tp := oneClaimTopology(nil)
	tp.NetAppInventory.Aggrs = []*graph.NetAppAggrNode{sfAggr("aggr1", "")}

	_, edges := assembleStorageFlow(tp)
	for _, e := range edges {
		assert.NotEqual(t, graph.StorageTierNodeAggr, e.Labels["tier"])
		assert.NotEmpty(t, e.Source)
		assert.NotEmpty(t, e.Target)
	}
}
