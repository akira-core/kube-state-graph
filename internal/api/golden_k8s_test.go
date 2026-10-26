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

// TestGolden_K8sGraphResponses snapshots the /v1/k8s-graph body for a node root
// and a pod root over one hand-built estate, so contract drift is caught on PR.
func TestGolden_K8sGraphResponses(t *testing.T) {
	g := buildK8sGraphEstate()
	stampFixtureStatuses(slices.Collect(maps.Values(g.NodesByID)))

	nodeScope, err := graph.NewStorageScope(nil, nil, graph.StorageRootNode, []string{"worker-1", "worker-9"})
	require.NoError(t, err)
	podScope, err := graph.NewStorageScope(nil, nil, graph.StorageRootPod, []string{"shop/orders-0"})
	require.NoError(t, err)

	scenarios := map[string]graph.View{
		// Both zones' worker-1 and the podless worker-9; every pod on them, the
		// claims those pods mount, and nothing of orders-1 (worker-2), the other
		// mounter of the shared claim, or of the unmounted claim.
		"k8s-graph-node-root": graph.ProjectK8s(g, nodeScope),
		// Both zones' orders-0 with their node and claim; web-0 and nightly-x
		// share worker-1 and are not drawn.
		"k8s-graph-pod-root": graph.ProjectK8s(g, podScope),
	}
	for name, view := range scenarios {
		t.Run(name+"-cytoscape", func(t *testing.T) {
			compareGolden(t, name+"-cytoscape.json", cytoscape.Serialise(g, view))
		})
	}
}

// buildK8sGraphEstate is what /v1/k8s-graph builds from two zones sharing a
// cluster name (c1, so every id is qualified by its identity):
//
//   - zone-a: orders-0 (Deployment orders, Application checkout, firing alert)
//     and orders-1 on two nodes, mounting the shared claim; nightly-x, a Job
//     resolved through its CronJob to Application reports; web-0, mounting
//     nothing; worker-9, a node no pod runs on; orphan-data, a claim nothing
//     mounts.
//   - zone-b: orders-0 on its own worker-1, mounting data.
func buildK8sGraphEstate() *graph.Graph {
	const a, b = "zone-a-prod-c1", "zone-b-prod-c1"
	node := func(cluster, name, ip, ready string) *graph.K8sNode {
		return &graph.K8sNode{
			IDValue: graph.K8sNodeID(cluster, name), NameValue: name,
			LabelsValue:    map[string]string{"cluster": cluster},
			IPAddressValue: []string{ip}, ReadyStatusValue: ready,
		}
	}
	pod := func(cluster, ns, name, uid, onNode string) *graph.PodNode {
		return &graph.PodNode{
			IDValue: graph.PodID(cluster, uid), NameValue: name,
			LabelsValue: map[string]string{"cluster": cluster, "namespace": ns, "node": graph.K8sNodeID(cluster, onNode)},
		}
	}
	pvc := func(cluster, ns, name, app string, used, capacity float64) *graph.PVCNode {
		return &graph.PVCNode{
			IDValue: graph.PVCID(cluster, ns, name), NameValue: name,
			LabelsValue:       map[string]string{"cluster": cluster, "namespace": ns},
			StorageClassValue: "netapp-nas", ApplicationValue: app,
			UsageValue: &graph.UsageBytes{UsedBytes: &used, CapacityBytes: &capacity},
		}
	}

	w1a, w2a, w9a := node(a, "worker-1", "10.0.0.1", "Ready"), node(a, "worker-2", "10.0.0.2", "Ready"), node(a, "worker-9", "10.0.0.9", "NotReady")
	w1b := node(b, "worker-1", "10.1.0.1", "Ready")

	orders0 := pod(a, "shop", "orders-0", "uid-a-orders-0", "worker-1")
	orders0.OwnerValue = &graph.Owner{Kind: "Deployment", Name: "orders"}
	orders0.ApplicationValue = "checkout"
	orders0.IPAddressValue = []string{"10.244.0.10"}
	orders0.AlertsValue = []graph.Alert{{Name: "PodCrashLooping", State: graph.AlertStateFiring, Severity: "warning"}}
	orders1 := pod(a, "shop", "orders-1", "uid-a-orders-1", "worker-2")
	orders1.OwnerValue = &graph.Owner{Kind: "Deployment", Name: "orders"}
	orders1.ApplicationValue = "checkout"
	nightly := pod(a, "shop", "nightly-x", "uid-a-nightly", "worker-1")
	nightly.OwnerValue = &graph.Owner{Kind: "Job", Name: "nightly-28901"}
	nightly.ApplicationValue = "reports"
	web0 := pod(a, "shop", "web-0", "uid-a-web-0", "worker-1")
	ordersB := pod(b, "shop", "orders-0", "uid-b-orders-0", "worker-1")

	shared := pvc(a, "shop", "shared-data", "checkout", 4096, 16384)
	orphan := pvc(a, "shop", "orphan-data", "", 0, 8192)
	dataB := pvc(b, "shop", "data", "", 1024, 8192)

	runs := func(p *graph.PodNode, n *graph.K8sNode) *graph.Edge {
		return graph.NewEdge(graph.EdgeTypePodToNode, p.ID(), n.ID(), nil)
	}
	mounts := func(p *graph.PodNode, c *graph.PVCNode) *graph.Edge {
		return graph.NewEdge(graph.EdgeTypePodMountsPVC, p.ID(), c.ID(), nil)
	}
	nodes := []graph.GraphNode{
		w1a, w2a, w9a, w1b, orders0, orders1, nightly, web0, ordersB, shared, orphan, dataB,
	}
	edges := []*graph.Edge{
		runs(orders0, w1a), runs(orders1, w2a), runs(nightly, w1a), runs(web0, w1a), runs(ordersB, w1b),
		mounts(orders0, shared), mounts(orders1, shared), mounts(ordersB, dataB),
	}
	g := graph.NewGraph(nodes, edges, time.Time{})
	g.ClusterIdentities = map[string]graph.ClusterIdentity{
		a: {AZ: "zone-a", Env: "prod", Name: "c1"},
		b: {AZ: "zone-b", Env: "prod", Name: "c1"},
	}
	g.NamespaceMetadata = map[graph.NamespaceRef]graph.NamespaceMeta{
		{Cluster: a, Namespace: "shop"}: {Owner: "team-shop", Group: "commerce", GroupOwner: "bob", Alias: "Shop"},
	}
	g.ApplicationMetadata = map[graph.ApplicationRef]graph.ApplicationMeta{
		{Cluster: a, Namespace: "shop", Application: "checkout"}: {
			DeployedBy: "alice",
			DeployedAt: time.Date(2025, 10, 8, 13, 14, 15, 0, time.UTC),
		},
	}
	return g
}
