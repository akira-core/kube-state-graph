package graph

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// scGraph builds a small graph with a pod, its node, two PVCs in different
// namespaces, their StorageClasses, and the pod-to-node / pvc-to-storageclass /
// pod-mounts-pvc edges that wire them.
func scGraph() *Graph {
	nodes := []GraphNode{
		&PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}},
		&K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}},
		&PVCNode{IDValue: "cluster-alpha/shop/claim-a", NameValue: "claim-a", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}, StorageClassValue: "gp3"},
		&PVCNode{IDValue: "cluster-alpha/db/claim-b", NameValue: "claim-b", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "db"}, StorageClassValue: "gp2"},
		&StorageClassNode{IDValue: StorageClassID("cluster-alpha", "gp3"), NameValue: "gp3", LabelsValue: map[string]string{"cluster": "cluster-alpha"}, InfoValue: &StorageClassInfo{Provisioner: "ebs.csi.aws.com"}},
		&StorageClassNode{IDValue: StorageClassID("cluster-alpha", "gp2"), NameValue: "gp2", LabelsValue: map[string]string{"cluster": "cluster-alpha"}},
	}
	edges := []*Edge{
		NewEdge(EdgeTypePodToNode, "cluster-alpha/p1", "cluster-alpha/worker-0", nil),
		NewEdge(EdgeTypePodMountsPVC, "cluster-alpha/p1", "cluster-alpha/shop/claim-a", nil),
		NewEdge(EdgeTypePVCToStorageClass, "cluster-alpha/shop/claim-a", StorageClassID("cluster-alpha", "gp3"), nil),
		NewEdge(EdgeTypePVCToStorageClass, "cluster-alpha/db/claim-b", StorageClassID("cluster-alpha", "gp2"), nil),
	}
	return NewGraph(nodes, edges, time.Now())
}

func idSet(v View) map[string]bool {
	out := map[string]bool{}
	for _, n := range v.Nodes {
		out[n.ID()] = true
	}
	return out
}

// TestProject_NamespaceFilterRetainsReferencedStorageClass — under ?namespace,
// a StorageClass (which carries no namespace) is retained iff some in-scope PVC
// references it; an unreferenced one is dropped (design.md D6).
func TestProject_NamespaceFilterRetainsReferencedStorageClass(t *testing.T) {
	v := Project(scGraph(), Scope{Namespaces: map[string]struct{}{"shop": {}}})
	ids := idSet(v)

	assert.True(t, ids["cluster-alpha/p1"], "in-scope pod retained")
	assert.True(t, ids["cluster-alpha/shop/claim-a"], "in-scope PVC retained")
	assert.True(t, ids[StorageClassID("cluster-alpha", "gp3")], "StorageClass referenced by in-scope PVC retained")
	assert.True(t, ids["cluster-alpha/worker-0"], "node hosting in-scope pod retained")

	assert.False(t, ids["cluster-alpha/db/claim-b"], "out-of-namespace PVC dropped")
	assert.False(t, ids[StorageClassID("cluster-alpha", "gp2")], "StorageClass referenced only by out-of-scope PVC dropped")
}

// TestProject_NamespaceFilterDropsAllStorageClassesWhenNoPVCInScope — a
// namespace with no PVCs drops every StorageClass (none referenced).
func TestProject_NamespaceFilterDropsUnreferencedStorageClasses(t *testing.T) {
	// "empty-ns" has no PVCs at all → both StorageClasses unreferenced.
	v := Project(scGraph(), Scope{Namespaces: map[string]struct{}{"empty-ns": {}}})
	ids := idSet(v)
	assert.False(t, ids[StorageClassID("cluster-alpha", "gp3")])
	assert.False(t, ids[StorageClassID("cluster-alpha", "gp2")])
}

// TestProject_NameFilterMatchesStorageClass — ?name=<sc> matches a StorageClass
// node by exact Name(); its incident pvc-to-storageclass partner re-hydrates.
func TestProject_NameFilterMatchesStorageClass(t *testing.T) {
	v := Project(scGraph(), Scope{Names: map[string]struct{}{"gp3": {}}})
	ids := idSet(v)
	assert.True(t, ids[StorageClassID("cluster-alpha", "gp3")], "StorageClass matched by name")
	assert.True(t, ids["cluster-alpha/shop/claim-a"], "PVC re-added as the pvc-to-storageclass partner")
	assert.False(t, ids[StorageClassID("cluster-alpha", "gp2")], "non-matching StorageClass excluded")
}

// TestProject_NameFilterOnNodePullsScheduledPodsViaEdge — with pod→node now an
// edge, ?name=<node> pulls the pods scheduled on it via pod-to-node re-add.
func TestProject_NameFilterOnNodePullsScheduledPodsViaEdge(t *testing.T) {
	v := Project(scGraph(), Scope{Names: map[string]struct{}{"worker-0": {}}})
	ids := idSet(v)
	assert.True(t, ids["cluster-alpha/worker-0"], "node matched by name")
	assert.True(t, ids["cluster-alpha/p1"], "pod scheduled on the node re-added via pod-to-node edge")
}

// TestProject_EdgeTypeFilterSelectsTopologyEdges — ?edge_type=pvc-to-storageclass
// keeps only those edges (registry-routed like any other type).
func TestProject_EdgeTypeFilterSelectsTopologyEdges(t *testing.T) {
	v := Project(scGraph(), Scope{EdgeTypes: map[EdgeType]struct{}{EdgeTypePVCToStorageClass: {}}})
	for _, e := range v.Edges {
		assert.Equal(t, EdgeTypePVCToStorageClass, e.Type)
	}
	assert.Len(t, v.Edges, 2, "both pvc-to-storageclass edges retained")
}
