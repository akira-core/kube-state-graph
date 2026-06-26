package cytoscape

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/graph"
)

// cy serialises a view into Cytoscape shape, building the *graph.Graph the
// serialiser needs from the supplied nodes.
func cy(t *testing.T, nodes []graph.GraphNode, edges []*graph.Edge) Body {
	t.Helper()
	byID := make(map[string]graph.GraphNode, len(nodes))
	for _, n := range nodes {
		byID[n.ID()] = n
	}
	return Serialise(&graph.Graph{NodesByID: byID}, graph.View{Nodes: nodes, Edges: edges})
}

// cyNodesByID indexes the Cytoscape node data by node id for assertions.
func cyNodesByID(b Body) map[string]NodeData {
	m := make(map[string]NodeData, len(b.Elements.Nodes))
	for _, n := range b.Elements.Nodes {
		m[n.Data.ID] = n.Data
	}
	return m
}

// assertNoClusterGroup fails if any emitted node is a synthetic cluster group.
func assertNoClusterGroup(t *testing.T, nodes map[string]NodeData) {
	t.Helper()
	for id := range nodes {
		assert.NotContains(t, id, "cluster/", "no cluster group node expected")
	}
}

// Full workload hierarchy: cluster > namespace > application > controller > pod.
// The pod→node relationship is an edge now (the node parents to the cluster, not
// the pod to the node). Supersedes the D31 cluster > node > pod nesting.
func TestSerialiseCytoscape_WorkloadHierarchy(t *testing.T) {
	pod := &graph.PodNode{
		IDValue:          "c1/p1",
		NameValue:        "checkout",
		LabelsValue:      map[string]string{"cluster": "c1", "namespace": "shop", "node": "c1/worker-0"},
		ApplicationValue: "checkout",
		OwnerValue:       &graph.Owner{Kind: "Deployment", Name: "checkout"},
	}
	node := &graph.K8sNode{IDValue: "c1/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c1"}}

	nodes := cyNodesByID(cy(t, []graph.GraphNode{node, pod}, nil))

	require.Contains(t, nodes, "cluster/c1")
	assert.Equal(t, "cluster", nodes["cluster/c1"].Type)
	assert.Empty(t, nodes["cluster/c1"].Parent, "cluster group is top-level")

	nsID := "c1/namespace/shop"
	appID := "c1/namespace/shop/application/checkout"
	ctrlID := "c1/namespace/shop/application/checkout/controller/Deployment/checkout"

	require.Contains(t, nodes, nsID)
	assert.Equal(t, "namespace", nodes[nsID].Type)
	assert.Equal(t, "cluster/c1", nodes[nsID].Parent)
	require.Contains(t, nodes, appID)
	assert.Equal(t, "application", nodes[appID].Type)
	assert.Equal(t, nsID, nodes[appID].Parent)
	require.Contains(t, nodes, ctrlID)
	assert.Equal(t, "controller", nodes[ctrlID].Type)
	assert.Equal(t, appID, nodes[ctrlID].Parent)

	assert.Equal(t, ctrlID, nodes["c1/p1"].Parent, "pod nests under its controller")
	assert.Equal(t, "cluster/c1", nodes["c1/worker-0"].Parent, "node parents to cluster (pod→node is an edge)")
}

// Parent assignment across the remaining cases: skip-absent-levels for pods,
// service/pvc → namespace, node/storageclass → cluster, and the label-less
// endpoints that get no parent and synthesise no group.
func TestSerialiseCytoscape_Parents(t *testing.T) {
	cases := []struct {
		name              string
		nodes             []graph.GraphNode
		wantParent        map[string]string
		wantNoClusterNode bool
	}{
		{
			name: "pod with controller but no application nests under controller (skip application)",
			nodes: []graph.GraphNode{
				&graph.PodNode{IDValue: "c1/p1", NameValue: "fluentd-x", LabelsValue: map[string]string{"cluster": "c1", "namespace": "shop"}, OwnerValue: &graph.Owner{Kind: "DaemonSet", Name: "fluentd"}},
			},
			wantParent: map[string]string{"c1/p1": "c1/namespace/shop/controller/DaemonSet/fluentd"},
		},
		{
			name: "pod with neither application nor controller nests under namespace",
			nodes: []graph.GraphNode{
				&graph.PodNode{IDValue: "c1/p1", NameValue: "bare", LabelsValue: map[string]string{"cluster": "c1", "namespace": "shop"}},
			},
			wantParent: map[string]string{"c1/p1": "c1/namespace/shop"},
		},
		{
			name: "service and pvc parented to namespace (never pod containers)",
			nodes: []graph.GraphNode{
				&graph.ServiceNode{IDValue: "c1/shop/payments", NameValue: "payments", LabelsValue: map[string]string{"cluster": "c1", "namespace": "shop"}},
				&graph.PVCNode{IDValue: "c1/shop/data", NameValue: "data", LabelsValue: map[string]string{"cluster": "c1", "namespace": "shop"}},
			},
			wantParent: map[string]string{"c1/shop/payments": "c1/namespace/shop", "c1/shop/data": "c1/namespace/shop"},
		},
		{
			name: "node and storageclass parented to cluster",
			nodes: []graph.GraphNode{
				&graph.K8sNode{IDValue: "c1/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c1"}},
				&graph.StorageClassNode{IDValue: "c1/storageclass/gp3", NameValue: "gp3", LabelsValue: map[string]string{"cluster": "c1"}},
			},
			wantParent: map[string]string{"c1/worker-0": "cluster/c1", "c1/storageclass/gp3": "cluster/c1"},
		},
		{
			name: "external has no parent and no cluster group",
			nodes: []graph.GraphNode{
				&graph.ExternalNode{IDValue: "external/http://api.example.com", NameValue: "http://api.example.com", LabelsValue: map[string]string{}},
				&graph.ExternalNode{IDValue: "external/admin", NameValue: "admin", LabelsValue: map[string]string{}},
			},
			wantParent:        map[string]string{"external/http://api.example.com": "", "external/admin": ""},
			wantNoClusterNode: true,
		},
		{
			name: "synth pod with unknown cluster has no parent and no cluster group",
			nodes: []graph.GraphNode{
				&graph.PodNode{IDValue: "/orphan-uid", NameValue: "orphan-uid", LabelsValue: map[string]string{"cluster": ""}},
			},
			wantParent:        map[string]string{"/orphan-uid": ""},
			wantNoClusterNode: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes := cyNodesByID(cy(t, tc.nodes, nil))
			for id, want := range tc.wantParent {
				assert.Equal(t, want, nodes[id].Parent, "parent of %s", id)
			}
			if tc.wantNoClusterNode {
				assertNoClusterGroup(t, nodes)
			}
		})
	}
}

// Service and PVC nest under their application group when an ArgoCD Application
// is resolved (skip-absent → namespace otherwise); the application group is
// synthesised even when no pod carries that Application, and neither nests under
// a controller group.
func TestSerialiseCytoscape_ServicePVCUnderApplication(t *testing.T) {
	svc := &graph.ServiceNode{
		IDValue: "c1/shop/payments", NameValue: "payments",
		LabelsValue:      map[string]string{"cluster": "c1", "namespace": "shop"},
		ApplicationValue: "checkout",
	}
	pvc := &graph.PVCNode{
		IDValue: "c1/shop/data", NameValue: "data",
		LabelsValue:      map[string]string{"cluster": "c1", "namespace": "shop"},
		ApplicationValue: "checkout",
	}

	nodes := cyNodesByID(cy(t, []graph.GraphNode{svc, pvc}, nil))

	nsID := "c1/namespace/shop"
	appID := "c1/namespace/shop/application/checkout"

	// The application group is synthesised from the service/pvc (no pod present).
	require.Contains(t, nodes, appID)
	assert.Equal(t, "application", nodes[appID].Type)
	assert.Equal(t, nsID, nodes[appID].Parent)

	assert.Equal(t, appID, nodes["c1/shop/payments"].Parent, "service nests under its application group")
	assert.Equal(t, appID, nodes["c1/shop/data"].Parent, "pvc nests under its application group")

	// data.application surfaces on the service/pvc nodes (via the omitempty DTO).
	assert.Equal(t, "checkout", nodes["c1/shop/payments"].Application)
	assert.Equal(t, "checkout", nodes["c1/shop/data"].Application)

	// No controller group is synthesised under the application for a service/pvc.
	for id := range nodes {
		assert.NotContains(t, id, "/controller/", "service/pvc must not produce a controller group")
	}
}

// A service with an Application but no namespace falls back to the cluster group
// (no namespace ⇒ no application group can be path-encoded).
func TestSerialiseCytoscape_ServiceApplicationWithoutNamespaceFallsBack(t *testing.T) {
	svc := &graph.ServiceNode{
		IDValue: "c1/x", NameValue: "x",
		LabelsValue:      map[string]string{"cluster": "c1"},
		ApplicationValue: "checkout",
	}
	nodes := cyNodesByID(cy(t, []graph.GraphNode{svc}, nil))
	assert.Equal(t, "cluster/c1", nodes["c1/x"].Parent)
	for id := range nodes {
		assert.NotContains(t, id, "/application/", "no application group without a namespace")
	}
}

// End-to-end (project → serialise): under a namespace filter the host K8s node
// is retained because it hosts an in-scope pod (D6), and the pod nests under its
// namespace group while the node parents to the cluster group.
func TestSerialiseCytoscape_NamespaceFilterKeepsHostNode(t *testing.T) {
	pod := &graph.PodNode{IDValue: "c1/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "c1", "namespace": "shop", "node": "c1/worker-0"}}
	node := &graph.K8sNode{IDValue: "c1/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c1"}}
	g := graph.NewGraph([]graph.GraphNode{pod, node}, nil, time.Now())

	view := graph.Project(g, graph.Scope{Namespaces: map[string]struct{}{"shop": {}}})
	nodes := cyNodesByID(Serialise(g, view))

	require.Contains(t, nodes, "c1/worker-0", "host node retained under namespace filter")
	assert.Equal(t, "cluster/c1", nodes["c1/worker-0"].Parent, "node parents to cluster")
	assert.Equal(t, "c1/namespace/shop", nodes["c1/p1"].Parent, "pod nests under its namespace group")
}

// Cluster group nodes are emitted first, sorted by cluster name, so the body
// stays byte-deterministic (D6).
func TestSerialiseCytoscape_ClusterNodesSortedFirst(t *testing.T) {
	a := &graph.PodNode{IDValue: "c-beta/p1", NameValue: "p", LabelsValue: map[string]string{"cluster": "c-beta"}}
	b := &graph.PodNode{IDValue: "c-alpha/p2", NameValue: "p", LabelsValue: map[string]string{"cluster": "c-alpha"}}

	body := cy(t, []graph.GraphNode{a, b}, nil)
	require.GreaterOrEqual(t, len(body.Elements.Nodes), 2)
	assert.Equal(t, "cluster/c-alpha", body.Elements.Nodes[0].Data.ID)
	assert.Equal(t, "cluster/c-beta", body.Elements.Nodes[1].Data.ID)
}
