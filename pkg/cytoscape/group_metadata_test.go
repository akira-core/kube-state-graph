package cytoscape

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

func cyMeta(t *testing.T, nodes []graph.GraphNode, namespaces map[graph.NamespaceRef]graph.NamespaceMeta, applications map[graph.ApplicationRef]graph.ApplicationMeta) Body {
	t.Helper()
	byID := make(map[string]graph.GraphNode, len(nodes))
	for _, n := range nodes {
		byID[n.ID()] = n
	}
	return Serialise(&graph.Graph{
		NodesByID:           byID,
		NamespaceMetadata:   namespaces,
		ApplicationMetadata: applications,
	}, graph.View{Nodes: nodes})
}

func assertOmitsGroupFields(t *testing.T, data NodeData) {
	t.Helper()
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	for _, key := range []string{
		"namespace_owner", "namespace_group", "namespace_group_owner",
		"namespace_alias", "deployed_by", "deployed_at",
	} {
		assert.NotContains(t, string(raw), key)
	}
}

func TestSerialise_NamespaceGroupCarriesOwner(t *testing.T) {
	pod := &graph.PodNode{
		IDValue:     "us-dev-c1/p1",
		NameValue:   "web",
		LabelsValue: map[string]string{"cluster": "us-dev-c1", "namespace": "shop"},
	}
	nodes := cyNodesByID(cyMeta(t, []graph.GraphNode{pod}, map[graph.NamespaceRef]graph.NamespaceMeta{
		{Cluster: "us-dev-c1", Namespace: "shop"}: {Owner: "team-a"},
		{Cluster: "us-dev-c1", Namespace: "idle"}: {Owner: "team-z"},
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
	nodes := cyNodesByID(cyMeta(t, []graph.GraphNode{pod}, nil, map[graph.ApplicationRef]graph.ApplicationMeta{
		{Cluster: "us-dev-c1", Namespace: "shop", Application: "checkout"}: {DeployedBy: "alice"},
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
	nodes = cyNodesByID(cyMeta(t, []graph.GraphNode{pod}, map[graph.NamespaceRef]graph.NamespaceMeta{
		{Cluster: "us-dev-c1", Namespace: "other"}: {Owner: "team-b"},
	}, map[graph.ApplicationRef]graph.ApplicationMeta{
		{Cluster: "us-dev-c1", Namespace: "shop", Application: "other"}: {DeployedBy: "bob"},
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
	body := cyMeta(t, nodes, map[graph.NamespaceRef]graph.NamespaceMeta{
		{Cluster: "us-dev-c1", Namespace: "shop"}: {Owner: "team-a"},
	}, map[graph.ApplicationRef]graph.ApplicationMeta{
		{Cluster: "us-dev-c1", Namespace: "shop", Application: "checkout"}: {DeployedBy: "alice"},
	})
	byID := cyNodesByID(body)
	for _, n := range nodes {
		assertOmitsGroupFields(t, byID[n.ID()])
	}
	assert.Equal(t, "team-a", byID[namespaceParentID("us-dev-c1", "shop")].NamespaceOwner)
	assert.Equal(t, "alice", byID[applicationParentID("us-dev-c1", "shop", "checkout")].DeployedBy)
	assertOmitsGroupFields(t, byID["cluster/us-dev-c1"])
}

func TestSerialise_NamespaceGroupCarriesGroupFields(t *testing.T) {
	pod := &graph.PodNode{
		IDValue:     "us-dev-c1/p1",
		NameValue:   "web",
		LabelsValue: map[string]string{"cluster": "us-dev-c1", "namespace": "shop"},
	}
	nodes := cyNodesByID(cyMeta(t, []graph.GraphNode{pod}, map[graph.NamespaceRef]graph.NamespaceMeta{
		{Cluster: "us-dev-c1", Namespace: "shop"}: {Group: "commerce", GroupOwner: "bob", Alias: "Shop"},
	}, nil))
	ns := nodes[namespaceParentID("us-dev-c1", "shop")]
	assert.Equal(t, "commerce", ns.NamespaceGroup)
	assert.Equal(t, "bob", ns.NamespaceGroupOwner)
	assert.Equal(t, "Shop", ns.NamespaceAlias)
	assert.Equal(t, map[string]string{}, ns.Labels)
	assert.Empty(t, ns.NamespaceOwner)
	assertOmitsGroupFields(t, nodes[pod.ID()])
}

func TestSerialise_ApplicationGroupCarriesDeployTime(t *testing.T) {
	pod := &graph.PodNode{
		IDValue:          "us-dev-c1/p1",
		NameValue:        "web",
		LabelsValue:      map[string]string{"cluster": "us-dev-c1", "namespace": "shop"},
		ApplicationValue: "checkout",
	}
	when := time.Date(2025, 10, 8, 13, 14, 15, 0, time.UTC)
	nodes := cyNodesByID(cyMeta(t, []graph.GraphNode{pod}, nil, map[graph.ApplicationRef]graph.ApplicationMeta{
		{Cluster: "us-dev-c1", Namespace: "shop", Application: "checkout"}: {DeployedBy: "alice", DeployedAt: when},
	}))
	app := nodes[applicationParentID("us-dev-c1", "shop", "checkout")]
	assert.Equal(t, "alice", app.DeployedBy)
	assert.Equal(t, "2025-10-08T13:14:15Z", app.DeployedAt)
	assertOmitsGroupFields(t, nodes[pod.ID()])

	nodes = cyNodesByID(cyMeta(t, []graph.GraphNode{pod}, nil, map[graph.ApplicationRef]graph.ApplicationMeta{
		{Cluster: "us-dev-c1", Namespace: "shop", Application: "checkout"}: {DeployedBy: "alice"},
	}))
	assert.Empty(t, nodes[applicationParentID("us-dev-c1", "shop", "checkout")].DeployedAt)
}
