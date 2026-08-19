package graph

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// netappGraph: two clusters share one filer. shop PVC in alpha joins
// aggr1/n1; db PVC in beta joins the same aggregate. An unused aggregate
// and its controller sit unreferenced.
func idSet(v View) map[string]bool {
	out := map[string]bool{}
	for _, n := range v.Nodes {
		out[n.ID()] = true
	}
	return out
}

func netappGraph() *Graph {
	nodes := []GraphNode{
		&PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}},
		&PodNode{IDValue: "cluster-beta/p2", NameValue: "billing", LabelsValue: map[string]string{"cluster": "cluster-beta", "namespace": "db", "node": "cluster-beta/worker-0"}},
		&K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}},
		&K8sNode{IDValue: "cluster-beta/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-beta"}},
		&PVCNode{IDValue: "cluster-alpha/shop/claim-a", NameValue: "claim-a", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "volumename": "pvc-a"}},
		&PVCNode{IDValue: "cluster-beta/db/claim-b", NameValue: "claim-b", LabelsValue: map[string]string{"cluster": "cluster-beta", "namespace": "db", "volumename": "pvc-b"}},
		&NetAppAggrNode{IDValue: NetAppAggrID("ontap-prod", "aggr1"), NameValue: "aggr1", LabelsValue: map[string]string{"ontap_cluster": "ontap-prod", "node": "ontap-prod-01"}},
		&NetAppAggrNode{IDValue: NetAppAggrID("ontap-prod", "idle"), NameValue: "idle", LabelsValue: map[string]string{"ontap_cluster": "ontap-prod", "node": "ontap-prod-02"}},
		&NetAppNode{IDValue: NetAppNodeID("ontap-prod", "ontap-prod-01"), NameValue: "ontap-prod-01", LabelsValue: map[string]string{"ontap_cluster": "ontap-prod"}},
		&NetAppNode{IDValue: NetAppNodeID("ontap-prod", "ontap-prod-02"), NameValue: "ontap-prod-02", LabelsValue: map[string]string{"ontap_cluster": "ontap-prod"}},
	}
	edges := []*Edge{
		NewEdge(EdgeTypePodCallsPod, "cluster-alpha/p1", "cluster-beta/p2", map[string]string{"cluster": "cluster-alpha"}),
		NewEdge(EdgeTypePodToNode, "cluster-alpha/p1", "cluster-alpha/worker-0", nil),
		NewEdge(EdgeTypePodToNode, "cluster-beta/p2", "cluster-beta/worker-0", nil),
		NewEdge(EdgeTypePodMountsPVC, "cluster-alpha/p1", "cluster-alpha/shop/claim-a", nil),
		NewEdge(EdgeTypePodMountsPVC, "cluster-beta/p2", "cluster-beta/db/claim-b", nil),
		NewEdge(EdgeTypePVCToNetAppAggr, "cluster-alpha/shop/claim-a", NetAppAggrID("ontap-prod", "aggr1"), nil),
		NewEdge(EdgeTypePVCToNetAppAggr, "cluster-beta/db/claim-b", NetAppAggrID("ontap-prod", "aggr1"), nil),
	}
	return NewGraph(nodes, edges, time.Now())
}

func TestProject_DefaultPrunesUnreferencedNetApp(t *testing.T) {
	v := Project(netappGraph(), Scope{})
	ids := idSet(v)
	assert.True(t, ids[NetAppAggrID("ontap-prod", "aggr1")])
	assert.True(t, ids[NetAppNodeID("ontap-prod", "ontap-prod-01")])
	assert.False(t, ids[NetAppAggrID("ontap-prod", "idle")])
	assert.False(t, ids[NetAppNodeID("ontap-prod", "ontap-prod-02")])
}

func TestProject_NamespaceRetainsReferencedNetApp(t *testing.T) {
	v := Project(netappGraph(), Scope{Namespaces: map[string]struct{}{"shop": {}}})
	ids := idSet(v)
	assert.True(t, ids["cluster-alpha/shop/claim-a"])
	assert.True(t, ids[NetAppAggrID("ontap-prod", "aggr1")], "aggregate serving in-scope PVC retained")
	assert.True(t, ids[NetAppNodeID("ontap-prod", "ontap-prod-01")], "owning controller pulled in")
	assert.False(t, ids["cluster-beta/db/claim-b"])
}

func TestProject_SharedFilerVisibleFromEitherCluster(t *testing.T) {
	a := Project(netappGraph(), Scope{Clusters: map[string]struct{}{"cluster-alpha": {}}})
	b := Project(netappGraph(), Scope{Clusters: map[string]struct{}{"cluster-beta": {}}})
	assert.True(t, idSet(a)[NetAppAggrID("ontap-prod", "aggr1")])
	assert.True(t, idSet(a)[NetAppNodeID("ontap-prod", "ontap-prod-01")])
	assert.True(t, idSet(b)[NetAppAggrID("ontap-prod", "aggr1")])
	assert.True(t, idSet(b)[NetAppNodeID("ontap-prod", "ontap-prod-01")])
}

func TestProject_NameFilterSurfacesNetAppAggrAndParent(t *testing.T) {
	v := Project(netappGraph(), Scope{Names: map[string]struct{}{"idle": {}}})
	ids := idSet(v)
	assert.True(t, ids[NetAppAggrID("ontap-prod", "idle")], "named aggregate surfaced")
	assert.True(t, ids[NetAppNodeID("ontap-prod", "ontap-prod-02")], "owning controller pulled as compound parent")
	assert.False(t, ids[NetAppAggrID("ontap-prod", "aggr1")])
}

func TestProject_NameFilterSurfacesNetAppNode(t *testing.T) {
	v := Project(netappGraph(), Scope{Names: map[string]struct{}{"ontap-prod-02": {}}})
	ids := idSet(v)
	assert.True(t, ids[NetAppNodeID("ontap-prod", "ontap-prod-02")])
	assert.False(t, ids[NetAppAggrID("ontap-prod", "idle")], "naming the controller does not pull unused aggregates")
}

func TestProject_NameFilterOnPVCPullsAggregate(t *testing.T) {
	v := Project(netappGraph(), Scope{Names: map[string]struct{}{"claim-a": {}}})
	ids := idSet(v)
	assert.True(t, ids["cluster-alpha/shop/claim-a"])
	assert.True(t, ids[NetAppAggrID("ontap-prod", "aggr1")], "aggregate re-added as edge partner")
	assert.True(t, ids[NetAppNodeID("ontap-prod", "ontap-prod-01")], "controller pulled after partner re-add")
}

func TestProject_EdgeTypeFilterSelectsNetAppEdges(t *testing.T) {
	v := Project(netappGraph(), Scope{EdgeTypes: map[EdgeType]struct{}{EdgeTypePVCToNetAppAggr: {}}})
	for _, e := range v.Edges {
		assert.Equal(t, EdgeTypePVCToNetAppAggr, e.Type)
	}
	assert.Len(t, v.Edges, 2)
}

func TestClusterNames_ExcludesONTAP(t *testing.T) {
	g := netappGraph()
	names := g.ClusterNames()
	for _, c := range names {
		assert.NotEqual(t, "ontap-prod", c)
	}
}
