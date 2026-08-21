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
	// p1 and p2 are connectivity-connected (pod-calls-pod) so they survive the
	// default prune; the test isolates the D6 podless-node rule.
	all := []GraphNode{
		&PodNode{IDValue: "c/p1", NameValue: "web", LabelsValue: map[string]string{"cluster": "c", "namespace": "shop", "node": "c/worker-0"}},
		&PodNode{IDValue: "c/p2", NameValue: "api", LabelsValue: map[string]string{"cluster": "c", "namespace": "shop", "node": "c/worker-0"}},
		&K8sNode{IDValue: "c/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c"}},
		&K8sNode{IDValue: "c/worker-1", NameValue: "worker-1", LabelsValue: map[string]string{"cluster": "c"}}, // hosts nothing
	}
	g := NewGraph(all, []*Edge{NewEdge(EdgeTypePodCallsPod, "c/p1", "c/p2", map[string]string{"cluster": "c"})}, time.Now())

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
	// p1 is connectivity-connected (pod-calls-pod with p2) and mounts the `data`
	// PVC, so both survive the default prune; the test isolates the D6
	// unreferenced-infra rule (worker-1 hosts nothing, `unused` backs nothing).
	all := []GraphNode{
		&PodNode{IDValue: "c/p1", NameValue: "web", LabelsValue: map[string]string{"cluster": "c", "namespace": "shop", "node": "c/worker-0"}},
		&PodNode{IDValue: "c/p2", NameValue: "api", LabelsValue: map[string]string{"cluster": "c", "namespace": "shop", "node": "c/worker-0"}},
		&K8sNode{IDValue: "c/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c"}}, // hosts p1+p2
		&K8sNode{IDValue: "c/worker-1", NameValue: "worker-1", LabelsValue: map[string]string{"cluster": "c"}}, // hosts nothing
		&PVCNode{IDValue: "c/shop/data", NameValue: "data", LabelsValue: map[string]string{"cluster": "c", "namespace": "shop"}, StorageClassValue: "gp3"},
		&NetAppAggrNode{IDValue: NetAppAggrID("oc", "used"), NameValue: "used", LabelsValue: map[string]string{"ontap_cluster": "oc", "node": "n1"}},
		&NetAppAggrNode{IDValue: NetAppAggrID("oc", "unused"), NameValue: "unused", LabelsValue: map[string]string{"ontap_cluster": "oc", "node": "n2"}},
		&NetAppNode{IDValue: NetAppNodeID("oc", "n1"), NameValue: "n1", LabelsValue: map[string]string{"ontap_cluster": "oc"}},
		&NetAppNode{IDValue: NetAppNodeID("oc", "n2"), NameValue: "n2", LabelsValue: map[string]string{"ontap_cluster": "oc"}},
	}
	edges := []*Edge{
		NewEdge(EdgeTypePodCallsPod, "c/p1", "c/p2", map[string]string{"cluster": "c"}),
		NewEdge(EdgeTypePodToNode, "c/p1", "c/worker-0", nil),
		NewEdge(EdgeTypePodToNode, "c/p2", "c/worker-0", nil),
		NewEdge(EdgeTypePodMountsPVC, "c/p1", "c/shop/data", nil),
		NewEdge(EdgeTypePVCToNetAppAggr, "c/shop/data", NetAppAggrID("oc", "used"), nil),
	}
	g := NewGraph(all, edges, time.Now())

	v := Project(g, Scope{})
	ids := map[string]bool{}
	for _, n := range v.Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["c/worker-0"], "node hosting an in-graph pod is retained")
	assert.True(t, ids[NetAppAggrID("oc", "used")], "aggregate serving an in-graph PVC is retained")
	assert.True(t, ids[NetAppNodeID("oc", "n1")], "controller of the kept aggregate is retained")
	assert.False(t, ids["c/worker-1"], "podless node dropped from the no-filter view")
	assert.False(t, ids[NetAppAggrID("oc", "unused")], "unreferenced aggregate dropped from the no-filter view")
	assert.False(t, ids[NetAppNodeID("oc", "n2")], "controller of an unreferenced aggregate dropped")
}

// prune=false with no namespace filter admits a podless K8s node — the
// `?prune=false` inventory view, which replaces the withdrawn `?name=` and
// `?root=` escape hatches.
func TestProject_Inventory_AdmitsPodlessK8sNode(t *testing.T) {
	all := []GraphNode{
		&PodNode{IDValue: "c/p1", NameValue: "web", LabelsValue: map[string]string{"cluster": "c", "namespace": "shop", "node": "c/worker-0"}},
		&K8sNode{IDValue: "c/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c"}},
		&K8sNode{IDValue: "c/worker-1", NameValue: "worker-1", LabelsValue: map[string]string{"cluster": "c"}}, // hosts nothing
	}
	g := NewGraph(all, []*Edge{NewEdge(EdgeTypePodToNode, "c/p1", "c/worker-0", nil)}, time.Now())

	ids := map[string]bool{}
	for _, n := range Project(g, Scope{Inventory: true}).Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["c/worker-1"], "prune=false admits the podless node")
	assert.True(t, ids["c/worker-0"], "and keeps the referenced one")

	// A cluster filter still applies to a K8s node's own labels.
	scoped := map[string]bool{}
	for _, n := range Project(g, Scope{Inventory: true, Clusters: map[string]struct{}{"other": {}}}).Nodes {
		scoped[n.ID()] = true
	}
	assert.False(t, scoped["c/worker-1"], "cluster filter excludes the node by its own label")
}

// Under a namespace filter the K8s-node lift is suppressed: the node carries no
// namespace, so its only meaningful admission stays "an in-scope pod runs here".
func TestProject_Inventory_NamespaceFilterKeepsK8sNodeReferenceDriven(t *testing.T) {
	all := []GraphNode{
		&PodNode{IDValue: "c/p1", NameValue: "web", LabelsValue: map[string]string{"cluster": "c", "namespace": "shop", "node": "c/worker-0"}},
		&K8sNode{IDValue: "c/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c"}},
		&K8sNode{IDValue: "c/worker-1", NameValue: "worker-1", LabelsValue: map[string]string{"cluster": "c"}}, // hosts nothing
	}
	g := NewGraph(all, []*Edge{NewEdge(EdgeTypePodToNode, "c/p1", "c/worker-0", nil)}, time.Now())

	ids := map[string]bool{}
	for _, n := range Project(g, Scope{Inventory: true, Namespaces: map[string]struct{}{"shop": {}}}).Nodes {
		ids[n.ID()] = true
	}
	assert.True(t, ids["c/worker-0"], "host of the in-scope pod kept")
	assert.False(t, ids["c/worker-1"], "podless node NOT lifted under a namespace filter")
}
