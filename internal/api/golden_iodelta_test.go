package api

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// ioDeltaStorageEstate is one aggregate serving four mounted claims: orders
// (rising), catalog (falling), cart (created inside the window, so it carries
// measurements and no change field) and shared (an RWX claim mounted by two
// pods, whose change is split between them). It is the body a request with
// io_delta=true returns once the build has attached the claim-level deltas.
func ioDeltaStorageEstate() *graph.Graph {
	const cluster, oc = "cluster-alpha", "ontap-prod"
	f64 := func(v float64) *float64 { return &v }
	hop := func(tier, src, tgt string, extra map[string]string) *graph.Edge {
		l := map[string]string{"tier": tier}
		for k, v := range extra {
			l[k] = v
		}
		return graph.NewEdge(graph.EdgeTypeStorageFlow, src, tgt, l)
	}
	ctrl := &graph.NetAppNode{IDValue: graph.NetAppNodeID(oc, "ontap-prod-01"), NameValue: "ontap-prod-01", LabelsValue: map[string]string{"ontap_cluster": oc}}
	aggr := &graph.NetAppAggrNode{IDValue: graph.NetAppAggrID(oc, "aggr1"), NameValue: "aggr1", LabelsValue: map[string]string{"ontap_cluster": oc, "node": "ontap-prod-01"}}
	svm := &graph.NetAppSVMNode{IDValue: graph.NetAppSVMID(oc, "svm_shop"), NameValue: "svm_shop", LabelsValue: map[string]string{"ontap_cluster": oc}}
	node := &graph.K8sNode{IDValue: graph.K8sNodeID(cluster, "worker-1"), NameValue: "worker-1", LabelsValue: map[string]string{"cluster": cluster}}

	nodes := []graph.GraphNode{ctrl, aggr, svm, node}
	edges := []*graph.Edge{
		hop(graph.StorageTierNodeAggr, ctrl.ID(), aggr.ID(), nil),
		hop(graph.StorageTierAggrSVM, aggr.ID(), svm.ID(), nil),
	}
	type claim struct {
		name string
		pods []string
		io   graph.IOMetrics
	}
	claims := []claim{
		{"orders-data", []string{"orders-0"}, graph.IOMetrics{
			ReadOps: f64(150), WriteOps: f64(40), ReadLatencyUs: f64(650), WriteLatencyUs: f64(300),
			ReadBytesPerSec: f64(6e6), WriteBytesPerSec: f64(2e6),
			ReadOpsDelta: f64(50), WriteOpsDelta: f64(10), ReadLatencyUsDelta: f64(250), WriteLatencyUsDelta: f64(100),
			ReadBytesPerSecDelta: f64(1e6), WriteBytesPerSecDelta: f64(5e5),
		}},
		{"catalog-data", []string{"catalog-0"}, graph.IOMetrics{
			ReadOps: f64(100), WriteOps: f64(20), ReadLatencyUs: f64(200), WriteLatencyUs: f64(150),
			ReadBytesPerSec: f64(1e6), WriteBytesPerSec: f64(5e5),
			ReadOpsDelta: f64(-80), WriteOpsDelta: f64(-30), ReadLatencyUsDelta: f64(-50), WriteLatencyUsDelta: f64(-25),
			ReadBytesPerSecDelta: f64(-3e6), WriteBytesPerSecDelta: f64(-1e6),
		}},
		{"cart-data", []string{"cart-0"}, graph.IOMetrics{
			ReadOps: f64(200), WriteOps: f64(60), ReadLatencyUs: f64(400), WriteLatencyUs: f64(300),
			ReadBytesPerSec: f64(3e6), WriteBytesPerSec: f64(1e6),
		}},
		{"shared-data", []string{"shared-0", "shared-1"}, graph.IOMetrics{
			ReadOps: f64(300), WriteOps: f64(90), ReadLatencyUs: f64(500), WriteLatencyUs: f64(250),
			ReadBytesPerSec: f64(9e6), WriteBytesPerSec: f64(3e6),
			ReadOpsDelta: f64(30), WriteOpsDelta: f64(0), ReadLatencyUsDelta: f64(10), WriteLatencyUsDelta: f64(-4),
			ReadBytesPerSecDelta: f64(1.2e6), WriteBytesPerSecDelta: f64(-3e5),
		}},
	}
	for _, c := range claims {
		pvc := &graph.PVCNode{IDValue: graph.PVCID(cluster, "shop", c.name), NameValue: c.name, LabelsValue: map[string]string{"cluster": cluster, "namespace": "shop", "aggr": aggr.ID()}}
		nodes = append(nodes, pvc)
		edges = append(edges, hop(graph.StorageTierSVMPVC, svm.ID(), pvc.ID(), map[string]string{graph.ClaimAggrLabel: aggr.ID()}).WithIO(c.io))
		for _, p := range c.pods {
			pod := &graph.PodNode{IDValue: graph.PodID(cluster, "uid-"+p), NameValue: p, LabelsValue: map[string]string{"cluster": cluster, "namespace": "shop", "node": node.ID()}}
			nodes = append(nodes, pod)
			var extra map[string]string
			if len(c.pods) > 1 {
				extra = map[string]string{"attribution": graph.AttributionSplit}
			}
			edges = append(edges,
				hop(graph.StorageTierPVCPod, pvc.ID(), pod.ID(), extra),
				hop(graph.StorageTierPodNode, pod.ID(), node.ID(), nil))
		}
	}
	return graph.NewGraph(nodes, edges, time.Time{})
}

func TestGolden_StorageGraphIODelta(t *testing.T) {
	g := ioDeltaStorageEstate()
	stampFixtureStatuses(slices.Collect(maps.Values(g.NodesByID)))
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootAggr, []string{"aggr1"})
	require.NoError(t, err)
	compareGolden(t, "storage-graph-io-delta-cytoscape.json", cytoscape.Serialise(g, graph.ProjectStorage(g, scope)))
}
