package graph

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func sampleGraph() *Graph {
	pods := []GraphNode{
		&PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}},
		&PodNode{IDValue: "cluster-alpha/p2", NameValue: "cart", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}},
		&PodNode{IDValue: "cluster-beta/p3", NameValue: "payments", LabelsValue: map[string]string{"cluster": "cluster-beta", "namespace": "billing", "node": "cluster-beta/worker-0"}},
	}
	nodes := []GraphNode{
		&K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}},
		&K8sNode{IDValue: "cluster-beta/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-beta"}},
	}
	all := append([]GraphNode{}, pods...)
	all = append(all, nodes...)

	edges := []*Edge{
		NewEdge(EdgeTypePodCallsPod, "cluster-alpha/p1", "cluster-alpha/p2", map[string]string{"cluster": "cluster-alpha"}),
		NewEdge(EdgeTypePodCallsPod, "cluster-alpha/p1", "cluster-beta/p3", map[string]string{"cluster": "cluster-alpha"}),
	}
	return NewGraph(all, edges, time.Now())
}

func TestProject_NoFilter(t *testing.T) {
	v := Project(sampleGraph(), Scope{})
	assert.Len(t, v.Nodes, 5)
	assert.Len(t, v.Edges, 2)
}

func TestProject_ClusterFilter(t *testing.T) {
	v := Project(sampleGraph(), Scope{Clusters: map[string]struct{}{"cluster-alpha": {}}})

	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	// All cluster-alpha nodes present.
	for _, want := range []string{"cluster-alpha/p1", "cluster-alpha/p2", "cluster-alpha/worker-0"} {
		assert.Truef(t, ids[want], "expected %s in result", want)
	}
	// Cross-cluster partner cluster-beta/p3 preserved (graph-api spec
	// §"Cross-cluster edge representation"); the K8s node cluster-beta/worker-0
	// is not on a cross-cluster edge so MUST stay out.
	assert.True(t, ids["cluster-beta/p3"], "cross-cluster pod partner must be preserved")
	assert.False(t, ids["cluster-beta/worker-0"], "intra-cluster cluster-beta node must be filtered out")
}

func TestProject_ClusterFilter_PreservesCrossClusterEdge(t *testing.T) {
	g := sampleGraph()
	v := Project(g, Scope{Clusters: map[string]struct{}{"cluster-alpha": {}}})

	// Cross-cluster status is derived from the resolved endpoint nodes'
	// cluster labels (the edge only carries the trace-source cluster).
	var crossEdges int
	for _, e := range v.Edges {
		if e.Type != EdgeTypePodCallsPod {
			continue
		}
		src := g.NodesByID[e.Source]
		tgt := g.NodesByID[e.Target]
		if src.Labels()["cluster"] != tgt.Labels()["cluster"] {
			crossEdges++
			assert.Equal(t, "cluster-alpha/p1", e.Source)
			assert.Equal(t, "cluster-beta/p3", e.Target)
			assert.Equal(t, "cluster-alpha", e.Labels["cluster"])
		}
	}
	assert.Equal(t, 1, crossEdges, "cross-cluster edge must survive cluster filter")
}

func TestProject_ClusterFilter_NamespaceStillStrict(t *testing.T) {
	// Namespace filter is AND-combined: cross-cluster partner whose namespace
	// does not match the filter MUST be dropped (and so must the edge).
	g := sampleGraph()
	v := Project(g, Scope{
		Clusters:   map[string]struct{}{"cluster-alpha": {}},
		Namespaces: map[string]struct{}{"shop": {}},
	})

	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	assert.False(t, ids["cluster-beta/p3"], "partner with namespace=billing must not be re-added when namespace filter excludes it")
	// Cross-cluster status derived from the resolved endpoints' cluster
	// labels via the original graph (the projection only includes pods that
	// passed all filters).
	for _, e := range v.Edges {
		if e.Type != EdgeTypePodCallsPod {
			continue
		}
		src := g.NodesByID[e.Source]
		tgt := g.NodesByID[e.Target]
		assert.Equal(t, src.Labels()["cluster"], tgt.Labels()["cluster"], "no cross-cluster edge should survive namespace mismatch")
	}
}

func TestProject_NamespaceFilter(t *testing.T) {
	v := Project(sampleGraph(), Scope{Namespaces: map[string]struct{}{"shop": {}}})
	for _, n := range v.Nodes {
		if n.Type() == NodeTypePod {
			assert.Equal(t, "shop", n.Labels()["namespace"])
		}
	}
}

func TestProject_EdgeTypeFilter(t *testing.T) {
	v := Project(sampleGraph(), Scope{EdgeTypes: map[EdgeType]struct{}{EdgeTypePodCallsPod: {}}})
	for _, e := range v.Edges {
		assert.Equal(t, EdgeTypePodCallsPod, e.Type)
	}
	assert.Len(t, v.Edges, 2)
}

func TestProject_TraversalBoundedByDepth(t *testing.T) {
	v := Project(sampleGraph(), Scope{Root: "cluster-alpha/p1", Depth: 1, Direction: DirectionOut})
	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	for _, want := range []string{"cluster-alpha/p1", "cluster-alpha/p2", "cluster-beta/p3"} {
		assert.Truef(t, ids[want], "expected %s in traversal result", want)
	}
	assert.False(t, ids["cluster-alpha/worker-0"], "host node unreachable without a pod-runs-on-node edge")
}

func TestProject_UnknownRootEmpty(t *testing.T) {
	v := Project(sampleGraph(), Scope{Root: "does-not-exist", Depth: 2, Direction: DirectionBoth})
	assert.Empty(t, v.Nodes)
	assert.Empty(t, v.Edges)
}

func TestProject_DepthZero(t *testing.T) {
	v := Project(sampleGraph(), Scope{Root: "cluster-alpha/p1", Depth: 0, Direction: DirectionBoth})
	assert.Len(t, v.Nodes, 1)
	assert.Equal(t, "cluster-alpha/p1", v.Nodes[0].ID())
}

// Namespace filter retains a K8sNode iff an in-scope pod is scheduled on it
// (design.md D31). K8s nodes carry no namespace label of their own, so rather
// than dropping every node under a namespace filter, a node is kept when some
// pod that survived the namespace filter is hosted on it (labels.node). This
// restores the cluster>node>pod nesting for nodes relevant to the filtered
// pods, without surfacing nodes that host none of them.
func TestProject_NamespaceFilter_KeepsHostingK8sNode(t *testing.T) {
	g := sampleGraph()
	v := Project(g, Scope{Namespaces: map[string]struct{}{"shop": {}}})

	ids := map[string]bool{}
	var k8sCount, podCount int
	for _, n := range v.Nodes {
		ids[n.ID()] = true
		switch n.Type() {
		case NodeTypeK8sNode:
			k8sCount++
		case NodeTypePod:
			podCount++
		default:
		}
	}
	assert.Equal(t, 2, podCount, "expected 2 pods in shop namespace")
	// cluster-alpha/worker-0 hosts p1+p2 (both in shop) → retained.
	assert.True(t, ids["cluster-alpha/worker-0"], "host node of an in-scope pod must be retained")
	// cluster-beta/worker-0 hosts only p3 (billing) → no in-scope pod → dropped.
	assert.False(t, ids["cluster-beta/worker-0"], "node hosting no in-scope pod must drop")
	assert.Equal(t, 1, k8sCount, "only nodes hosting an in-scope pod survive a namespace filter")
}

// A K8sNode hosting no pod at all can never have an in-scope pod, so it drops
// under a namespace filter (and, per the generalised D6 rule, under no filter
// too — see TestProject_NoFilter_DropsUnreferencedInfraNodes).
func TestProject_NamespaceFilter_DropsPodlessK8sNode(t *testing.T) {
	all := []GraphNode{
		&PodNode{IDValue: "c/p1", NameValue: "web", LabelsValue: map[string]string{"cluster": "c", "namespace": "shop", "node": "c/worker-0"}},
		&K8sNode{IDValue: "c/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c"}},
		&K8sNode{IDValue: "c/worker-1", NameValue: "worker-1", LabelsValue: map[string]string{"cluster": "c"}}, // hosts nothing
	}
	g := NewGraph(all, nil, time.Now())

	v := Project(g, Scope{Namespaces: map[string]struct{}{"shop": {}}})
	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["c/worker-0"], "node hosting the in-scope pod is retained")
	assert.False(t, ids["c/worker-1"], "podless node drops under a namespace filter")
}

// Generalised D6: an infra node (K8s node or StorageClass) referenced by NO
// in-scope element is dropped from EVERY request shape — including the
// default no-filter view. A node only appears as the host of a pod in the graph.
func TestProject_NoFilter_DropsUnreferencedInfraNodes(t *testing.T) {
	all := []GraphNode{
		&PodNode{IDValue: "c/p1", NameValue: "web", LabelsValue: map[string]string{"cluster": "c", "namespace": "shop", "node": "c/worker-0"}},
		&K8sNode{IDValue: "c/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c"}}, // hosts p1
		&K8sNode{IDValue: "c/worker-1", NameValue: "worker-1", LabelsValue: map[string]string{"cluster": "c"}}, // hosts nothing
		&PVCNode{IDValue: "c/shop/data", NameValue: "data", LabelsValue: map[string]string{"cluster": "c", "namespace": "shop"}, StorageClassValue: "gp3"},
		&StorageClassNode{IDValue: StorageClassID("c", "gp3"), NameValue: "gp3", LabelsValue: map[string]string{"cluster": "c"}},       // backs data
		&StorageClassNode{IDValue: StorageClassID("c", "unused"), NameValue: "unused", LabelsValue: map[string]string{"cluster": "c"}}, // backs nothing
	}
	edges := []*Edge{
		NewEdge(EdgeTypePodToNode, "c/p1", "c/worker-0", nil),
		NewEdge(EdgeTypePVCToStorageClass, "c/shop/data", StorageClassID("c", "gp3"), nil),
	}
	g := NewGraph(all, edges, time.Now())

	v := Project(g, Scope{})
	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["c/worker-0"], "node hosting an in-graph pod is retained")
	assert.True(t, ids[StorageClassID("c", "gp3")], "StorageClass backing an in-graph PVC is retained")
	assert.False(t, ids["c/worker-1"], "podless node dropped from the no-filter view")
	assert.False(t, ids[StorageClassID("c", "unused")], "PVC-less StorageClass dropped from the no-filter view")
}

// The name-filter exception: ?name=<infra-node> surfaces a referenced-by-nothing
// node directly, even though the default view hides it.
func TestProject_NameFilter_MatchesUnreferencedInfraNode(t *testing.T) {
	all := []GraphNode{
		&PodNode{IDValue: "c/p1", NameValue: "web", LabelsValue: map[string]string{"cluster": "c", "namespace": "shop", "node": "c/worker-0"}},
		&K8sNode{IDValue: "c/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c"}},
		&K8sNode{IDValue: "c/worker-1", NameValue: "worker-1", LabelsValue: map[string]string{"cluster": "c"}}, // hosts nothing
	}
	g := NewGraph(all, []*Edge{NewEdge(EdgeTypePodToNode, "c/p1", "c/worker-0", nil)}, time.Now())

	v := Project(g, Scope{Names: map[string]struct{}{"worker-1": {}}})
	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["c/worker-1"], "explicit ?name=<podless-node> still returns the node")
	assert.False(t, ids["c/worker-0"], "non-named node not pulled in")
}

// multiClusterPodSampleGraph extends sampleGraph with a `payments` pod in
// cluster-alpha so the `pod=payments` filter can match across clusters
// (cluster-alpha and cluster-beta) and we can assert AND/OR semantics with
// the cluster filter.
func multiClusterPodSampleGraph() *Graph {
	pods := []GraphNode{
		&PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}},
		&PodNode{IDValue: "cluster-alpha/p2", NameValue: "cart", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}},
		&PodNode{IDValue: "cluster-alpha/p4", NameValue: "payments", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "billing", "node": "cluster-alpha/worker-0"}},
		&PodNode{IDValue: "cluster-beta/p3", NameValue: "payments", LabelsValue: map[string]string{"cluster": "cluster-beta", "namespace": "billing", "node": "cluster-beta/worker-0"}},
	}
	nodes := []GraphNode{
		&K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}},
		&K8sNode{IDValue: "cluster-beta/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-beta"}},
	}
	all := append([]GraphNode{}, pods...)
	all = append(all, nodes...)

	edges := []*Edge{
		NewEdge(EdgeTypePodCallsPod, "cluster-alpha/p1", "cluster-alpha/p2", map[string]string{"cluster": "cluster-alpha"}),
		NewEdge(EdgeTypePodCallsPod, "cluster-alpha/p1", "cluster-beta/p3", map[string]string{"cluster": "cluster-alpha"}),
	}
	return NewGraph(all, edges, time.Now())
}

// crossTypeSampleGraph contains:
//   - a pod named "worker-0" in cluster-alpha (collision with K8s node name)
//   - a K8s node named "worker-0" in cluster-alpha and cluster-beta
//   - a PVC named "checkout-data" in cluster-alpha
//   - a single pod-mounts-pvc edge (K8s nodes are edgeless; they are present
//     only to exercise the pod/node name collision)
func crossTypeSampleGraph() *Graph {
	all := []GraphNode{
		&PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}},
		&PodNode{IDValue: "cluster-alpha/p2", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}},
		&K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}},
		&K8sNode{IDValue: "cluster-beta/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-beta"}},
		&PVCNode{IDValue: "cluster-alpha/shop/checkout-data", NameValue: "checkout-data", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}},
	}
	edges := []*Edge{
		NewEdge(EdgeTypePodMountsPVC, "cluster-alpha/p1", "cluster-alpha/shop/checkout-data", nil),
	}
	return NewGraph(all, edges, time.Now())
}

func TestProject_NameFilter_MatchesPod(t *testing.T) {
	g := sampleGraph()
	v := Project(g, Scope{Names: map[string]struct{}{"checkout": {}}})

	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["cluster-alpha/p1"], "checkout pod must match name")
	// Endpoints of checkout's incident edges are re-added by the unified rule:
	// pod-calls-pod → cart (p2); pod-calls-pod → cross-cluster payments (p3).
	assert.True(t, ids["cluster-alpha/p2"], "cart re-added via pod-calls-pod")
	assert.True(t, ids["cluster-beta/p3"], "cross-cluster payments re-added via pod-calls-pod")
	// Host K8s nodes carry no edge to the pod, so they are not pulled in.
	assert.False(t, ids["cluster-alpha/worker-0"], "host node not re-added (no pod-runs-on-node edge)")
	assert.False(t, ids["cluster-beta/worker-0"], "unrelated K8s node must drop")
}

func TestProject_NameFilter_MatchesK8sNode(t *testing.T) {
	g := sampleGraph()
	v := Project(g, Scope{Names: map[string]struct{}{"worker-0": {}}})

	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	// Both worker-0 K8s nodes match by name (cluster-alpha and cluster-beta).
	assert.True(t, ids["cluster-alpha/worker-0"])
	assert.True(t, ids["cluster-beta/worker-0"])
	// K8s nodes carry no edges, so a name match pulls in no pods.
	assert.False(t, ids["cluster-alpha/p1"], "no edge re-adds a pod onto a name-matched K8s node")
	assert.False(t, ids["cluster-beta/p3"], "no edge re-adds a pod onto a name-matched K8s node")
}

func TestProject_NameFilter_MatchesPVC(t *testing.T) {
	g := crossTypeSampleGraph()
	v := Project(g, Scope{Names: map[string]struct{}{"checkout-data": {}}})

	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["cluster-alpha/shop/checkout-data"], "matching PVC must be present")
	// Pod that mounts the PVC re-added via pod-mounts-pvc edge.
	assert.True(t, ids["cluster-alpha/p1"], "pod mounting checkout-data must be re-added")
	assert.False(t, ids["cluster-alpha/p2"], "unrelated pod must NOT be re-added")
}

func TestProject_NameFilter_AcrossTypes(t *testing.T) {
	g := crossTypeSampleGraph()
	// "worker-0" is both a pod name (cluster-alpha/p2) and a K8s node name
	// (in both clusters). All three matches must surface in the result.
	v := Project(g, Scope{Names: map[string]struct{}{"worker-0": {}}})

	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["cluster-alpha/p2"], "pod named worker-0 must match")
	assert.True(t, ids["cluster-alpha/worker-0"], "K8s node named worker-0 in alpha must match")
	assert.True(t, ids["cluster-beta/worker-0"], "K8s node named worker-0 in beta must match")
}

func TestProject_NameFilter_DuplicatesAcrossClusters(t *testing.T) {
	g := multiClusterPodSampleGraph()
	v := Project(g, Scope{Names: map[string]struct{}{"payments": {}}})

	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["cluster-alpha/p4"], "alpha payments must match")
	assert.True(t, ids["cluster-beta/p3"], "beta payments must match")
	// p2 (cart) has no incident edge to either payments pod, so it must drop.
	assert.False(t, ids["cluster-alpha/p2"], "cart not incident on payments must drop")
}

func TestProject_NameFilter_AndedWithCluster(t *testing.T) {
	g := multiClusterPodSampleGraph()
	v := Project(g, Scope{
		Names:    map[string]struct{}{"payments": {}},
		Clusters: map[string]struct{}{"cluster-alpha": {}},
	})

	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["cluster-alpha/p4"], "alpha payments matches")
	assert.False(t, ids["cluster-beta/p3"], "beta payments excluded by cluster filter")
}

func TestProject_NameFilter_RetainsCrossClusterEdgeWithRehydratedPartner(t *testing.T) {
	g := sampleGraph()
	// Anchor on cluster-alpha/p1 (pod name "checkout"); its outgoing
	// pod-calls-pod to cluster-beta/p3 must retain the edge AND re-add p3
	// via the unified edge-endpoint partner rule.
	v := Project(g, Scope{Names: map[string]struct{}{"checkout": {}}})

	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["cluster-alpha/p1"])
	assert.True(t, ids["cluster-beta/p3"], "cross-cluster partner pod must be re-added as edge endpoint")
	var crossEdges int
	for _, e := range v.Edges {
		if e.Type == EdgeTypePodCallsPod && e.Source == "cluster-alpha/p1" && e.Target == "cluster-beta/p3" {
			crossEdges++
		}
	}
	assert.Equal(t, 1, crossEdges, "cross-cluster pod-calls-pod edge must survive name anchor")
}

func TestProject_NameFilter_UnknownReturnsEmpty(t *testing.T) {
	g := sampleGraph()
	v := Project(g, Scope{Names: map[string]struct{}{"does-not-exist": {}}})
	assert.Empty(t, v.Nodes)
	assert.Empty(t, v.Edges)
}
