package cytoscape

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

func cyMeta(t *testing.T, nodes []graph.GraphNode, owners map[graph.NamespaceRef]string, deployers map[graph.ApplicationRef]string) Body {
	t.Helper()
	byID := make(map[string]graph.GraphNode, len(nodes))
	for _, n := range nodes {
		byID[n.ID()] = n
	}
	return Serialise(&graph.Graph{
		NodesByID:            byID,
		NamespaceOwners:      owners,
		ApplicationDeployers: deployers,
	}, graph.View{Nodes: nodes})
}

func assertOmitsGroupFields(t *testing.T, data NodeData) {
	t.Helper()
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "namespace_owner")
	assert.NotContains(t, string(raw), "deployed_by")
}

func TestSerialise_NamespaceGroupCarriesOwner(t *testing.T) {
	pod := &graph.PodNode{
		IDValue:     "us-dev-c1/p1",
		NameValue:   "web",
		LabelsValue: map[string]string{"cluster": "us-dev-c1", "namespace": "shop"},
	}
	nodes := cyNodesByID(cyMeta(t, []graph.GraphNode{pod}, map[graph.NamespaceRef]string{
		{Cluster: "us-dev-c1", Namespace: "shop"}: "team-a",
		{Cluster: "us-dev-c1", Namespace: "idle"}: "team-z",
	}, nil))

	ns := nodes[namespaceParentID("us-dev-c1", "shop")]
	assert.Equal(t, "team-a", ns.NamespaceOwner)
	assert.Equal(t, map[string]string{}, ns.Labels)
	assert.NotContains(t, nodes, namespaceParentID("us-dev-c1", "idle"), "a namespace outside the view is not emitted")
	assertOmitsGroupFields(t, nodes[pod.ID()])
}

func TestSerialise_ApplicationGroupCarriesDeployer(t *testing.T) {
	// The pod's Application came from the controller chain. The deployer is
	// still the metric's row for that Application, looked up by the group key.
	pod := &graph.PodNode{
		IDValue:          "us-dev-c1/p1",
		NameValue:        "web",
		LabelsValue:      map[string]string{"cluster": "us-dev-c1", "namespace": "shop"},
		ApplicationValue: "checkout",
		OwnerValue:       &graph.Owner{Kind: "Deployment", Name: "web"},
	}
	nodes := cyNodesByID(cyMeta(t, []graph.GraphNode{pod}, nil, map[graph.ApplicationRef]string{
		{Cluster: "us-dev-c1", Namespace: "shop", Application: "checkout"}: "alice",
	}))

	app := nodes[applicationParentID("us-dev-c1", "shop", "checkout")]
	assert.Equal(t, "alice", app.DeployedBy)
	assert.Equal(t, map[string]string{}, app.Labels)
	assertOmitsGroupFields(t, nodes[pod.ID()])
	assertOmitsGroupFields(t, nodes[namespaceParentID("us-dev-c1", "shop")])
	ctrlID := controllerParentID("us-dev-c1", "shop", "checkout", "Deployment", "web")
	assertOmitsGroupFields(t, nodes[ctrlID])
}

func TestSerialise_UnresolvedGroupMetadataOmitted(t *testing.T) {
	pod := &graph.PodNode{
		IDValue:          "us-dev-c1/p1",
		NameValue:        "web",
		LabelsValue:      map[string]string{"cluster": "us-dev-c1", "namespace": "shop"},
		ApplicationValue: "checkout",
	}
	nodes := cyNodesByID(cyMeta(t, []graph.GraphNode{pod}, nil, nil))
	assertOmitsGroupFields(t, nodes[namespaceParentID("us-dev-c1", "shop")])
	assertOmitsGroupFields(t, nodes[applicationParentID("us-dev-c1", "shop", "checkout")])

	// A populated table that simply does not name this namespace or Application
	// omits the fields the same way a nil table does.
	nodes = cyNodesByID(cyMeta(t, []graph.GraphNode{pod}, map[graph.NamespaceRef]string{
		{Cluster: "us-dev-c1", Namespace: "other"}: "team-b",
	}, map[graph.ApplicationRef]string{
		{Cluster: "us-dev-c1", Namespace: "shop", Application: "other"}: "bob",
	}))
	assertOmitsGroupFields(t, nodes[namespaceParentID("us-dev-c1", "shop")])
	assertOmitsGroupFields(t, nodes[applicationParentID("us-dev-c1", "shop", "checkout")])
}

func TestSerialise_RealNodesNeverCarryGroupFields(t *testing.T) {
	labels := map[string]string{"cluster": "us-dev-c1", "namespace": "shop"}
	nodes := []graph.GraphNode{
		&graph.PodNode{IDValue: "us-dev-c1/p1", NameValue: "web", LabelsValue: labels, ApplicationValue: "checkout"},
		&graph.ServiceNode{IDValue: "us-dev-c1/shop/api", NameValue: "api", LabelsValue: labels, ApplicationValue: "checkout"},
		&graph.PVCNode{IDValue: "us-dev-c1/shop/data", NameValue: "data", LabelsValue: labels, ApplicationValue: "checkout"},
		&graph.K8sNode{IDValue: "us-dev-c1/n1", NameValue: "n1", LabelsValue: map[string]string{"cluster": "us-dev-c1"}},
		&graph.ExternalNode{IDValue: "external/x", NameValue: "x", LabelsValue: map[string]string{}},
		&graph.NetAppAggrNode{IDValue: "netapp/oc/aggr/a1", NameValue: "a1", LabelsValue: map[string]string{"ontap_cluster": "oc", "node": "n"}},
		&graph.NetAppNode{IDValue: "netapp/oc/n", NameValue: "n", LabelsValue: map[string]string{"ontap_cluster": "oc"}},
	}
	body := cyMeta(t, nodes, map[graph.NamespaceRef]string{
		{Cluster: "us-dev-c1", Namespace: "shop"}: "team-a",
	}, map[graph.ApplicationRef]string{
		{Cluster: "us-dev-c1", Namespace: "shop", Application: "checkout"}: "alice",
	})
	byID := cyNodesByID(body)
	for _, n := range nodes {
		assertOmitsGroupFields(t, byID[n.ID()])
	}
	assert.Equal(t, "team-a", byID[namespaceParentID("us-dev-c1", "shop")].NamespaceOwner)
	assert.Equal(t, "alice", byID[applicationParentID("us-dev-c1", "shop", "checkout")].DeployedBy)
	assertOmitsGroupFields(t, byID["cluster/us-dev-c1"])
}
