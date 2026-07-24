package graph

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// prunableGraph builds a graph that mixes connectivity-connected workload with
// edgeless workload so the default-projection prune can be exercised:
//
//	connected:  p1 --pod-calls-pod--> p2          (both on worker-0)
//	            p2 --pod-mounts-pvc--> pvc-bound   (pvc-bound -> sc-fast)
//	edgeless:   p9 (only pod-to-node to worker-1, no connectivity edge)
//	            pvc-orphan mounted only by p9      (pvc-orphan -> sc-slow)
//
// worker-0 hosts connected pods; worker-1 hosts only the edgeless p9.
// sc-fast backs a kept pvc; sc-slow backs only the orphan pvc.
func prunableGraph() *Graph {
	nodes := []GraphNode{
		&PodNode{IDValue: "c/p1", NameValue: "p1", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns", "node": "c/worker-0"}},
		&PodNode{IDValue: "c/p2", NameValue: "p2", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns", "node": "c/worker-0"}},
		&PodNode{IDValue: "c/p9", NameValue: "p9", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns", "node": "c/worker-1"}},
		&K8sNode{IDValue: "c/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c"}},
		&K8sNode{IDValue: "c/worker-1", NameValue: "worker-1", LabelsValue: map[string]string{"cluster": "c"}},
		&PVCNode{IDValue: "c/ns/pvc-bound", NameValue: "pvc-bound", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}, StorageClassValue: "sc-fast"},
		&PVCNode{IDValue: "c/ns/pvc-orphan", NameValue: "pvc-orphan", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}, StorageClassValue: "sc-slow"},
		&StorageClassNode{IDValue: StorageClassID("c", "sc-fast"), NameValue: "sc-fast", LabelsValue: map[string]string{"cluster": "c"}},
		&StorageClassNode{IDValue: StorageClassID("c", "sc-slow"), NameValue: "sc-slow", LabelsValue: map[string]string{"cluster": "c"}},
	}
	edges := []*Edge{
		NewEdge(EdgeTypePodCallsPod, "c/p1", "c/p2", map[string]string{"cluster": "c"}),
		NewEdge(EdgeTypePodMountsPVC, "c/p2", "c/ns/pvc-bound", nil),
		NewEdge(EdgeTypePodMountsPVC, "c/p9", "c/ns/pvc-orphan", nil),
		NewEdge(EdgeTypePodToNode, "c/p1", "c/worker-0", nil),
		NewEdge(EdgeTypePodToNode, "c/p2", "c/worker-0", nil),
		NewEdge(EdgeTypePodToNode, "c/p9", "c/worker-1", nil),
		NewEdge(EdgeTypePVCToStorageClass, "c/ns/pvc-bound", StorageClassID("c", "sc-fast"), nil),
		NewEdge(EdgeTypePVCToStorageClass, "c/ns/pvc-orphan", StorageClassID("c", "sc-slow"), nil),
	}
	return NewGraph(nodes, edges, time.Now())
}

// Default view: edgeless pod p9 and everything that hangs only off it
// (worker-1, pvc-orphan, sc-slow) are pruned. Connected workload stays.
func TestProject_DefaultPrunesEdgelessSubgraph(t *testing.T) {
	v := Project(prunableGraph(), Scope{})
	ids := idSet(v)

	// Connected workload retained.
	assert.True(t, ids["c/p1"], "connected pod p1 kept")
	assert.True(t, ids["c/p2"], "connected pod p2 kept")
	assert.True(t, ids["c/worker-0"], "node hosting connected pods kept")
	assert.True(t, ids["c/ns/pvc-bound"], "pvc mounted by connected pod kept")
	assert.True(t, ids[StorageClassID("c", "sc-fast")], "storageclass backing kept pvc kept")

	// Edgeless subgraph pruned.
	assert.False(t, ids["c/p9"], "edgeless pod must be pruned")
	assert.False(t, ids["c/worker-1"], "node hosting only edgeless pod must be pruned")
	assert.False(t, ids["c/ns/pvc-orphan"], "pvc mounted only by edgeless pod must be pruned")
	assert.False(t, ids[StorageClassID("c", "sc-slow")], "storageclass backing only orphan pvc must be pruned")
}

// The pod-to-node edge from the pruned pod must NOT resurrect that pod via the
// filterEdges partner re-add (worker-0 is kept, so the re-add path is live).
func TestProject_PrunedPodNotResurrectedByEdgeReadd(t *testing.T) {
	g := prunableGraph()
	// Add an edgeless pod p8 sharing the kept node worker-0, so its pod-to-node
	// edge has an in-scope endpoint (worker-0) and triggers the re-add path.
	g = withExtraEdgelessPodOnKeptNode(g)
	v := Project(g, Scope{})
	ids := idSet(v)
	assert.False(t, ids["c/p8"], "edgeless pod sharing a kept node must not be re-added as edge partner")
}

func withExtraEdgelessPodOnKeptNode(g *Graph) *Graph {
	nodes := make([]GraphNode, 0, len(g.NodesByID)+1)
	for _, n := range g.NodesByID {
		nodes = append(nodes, n)
	}
	nodes = append(nodes, &PodNode{IDValue: "c/p8", NameValue: "p8", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns", "node": "c/worker-0"}})
	edges := append([]*Edge{}, g.Edges...)
	edges = append(edges, NewEdge(EdgeTypePodToNode, "c/p8", "c/worker-0", nil))
	return NewGraph(nodes, edges, time.Now())
}

// A topology pod that is ONLY the target of a service-selects-pod edge (svc->pod)
// counts as connected and is retained.
func TestProject_ServiceSelectsPodBackingPodKept(t *testing.T) {
	nodes := []GraphNode{
		&ServiceNode{IDValue: "c/ns/svc", NameValue: "svc", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}},
		&PodNode{IDValue: "c/backing", NameValue: "backing", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns", "node": "c/worker-0"}},
		&K8sNode{IDValue: "c/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c"}},
	}
	edges := []*Edge{
		NewEdge(EdgeTypeServiceSelectsPod, "c/ns/svc", "c/backing", map[string]string{"cluster": "c"}),
		NewEdge(EdgeTypePodToNode, "c/backing", "c/worker-0", nil),
	}
	v := Project(NewGraph(nodes, edges, time.Now()), Scope{})
	ids := idSet(v)
	assert.True(t, ids["c/backing"], "service-selects-pod backing pod is connected and kept")
	assert.True(t, ids["c/ns/svc"], "service node kept")
	assert.True(t, ids["c/worker-0"], "node hosting backing pod kept")
}

// A pod connected ONLY by a pod-routes-to-service edge (the config-derived
// ingress-chain hop) counts as connected and survives the default prune.
func TestProject_PodRoutesToServiceSourcePodKept(t *testing.T) {
	nodes := []GraphNode{
		&PodNode{IDValue: "c/igw0", NameValue: "igw0", LabelsValue: map[string]string{"cluster": "c", "namespace": "istio-system", "node": "c/worker-0"}},
		&ServiceNode{IDValue: "c/ns/backend", NameValue: "backend", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}},
		&K8sNode{IDValue: "c/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "c"}},
	}
	edges := []*Edge{
		NewEdge(EdgeTypePodRoutesToService, "c/igw0", "c/ns/backend", map[string]string{"cluster": "c"}),
		NewEdge(EdgeTypePodToNode, "c/igw0", "c/worker-0", nil),
	}
	v := Project(NewGraph(nodes, edges, time.Now()), Scope{})
	ids := idSet(v)
	assert.True(t, ids["c/igw0"], "pod connected only by a pod-routes-to-service edge is connectivity-connected and kept")
	assert.True(t, ids["c/ns/backend"], "backend service node kept")
	assert.True(t, ids["c/worker-0"], "node hosting the gateway pod kept")
}

// Escape hatch: an explicit ?name= surfaces an otherwise-pruned edgeless pod
// (symmetric with the D6 infra-node name exception).
func TestProject_NameFilterSurfacesEdgelessPod(t *testing.T) {
	v := Project(prunableGraph(), Scope{Names: map[string]struct{}{"p9": {}}})
	ids := idSet(v)
	assert.True(t, ids["c/p9"], "explicit name filter must surface the edgeless pod on demand")
}

// Escape hatch: a root-anchored traversal still includes reachable nodes even if
// they have no connectivity edge (the prune is off under traversal).
func TestProject_TraversalSurfacesEdgelessNeighbour(t *testing.T) {
	// Root on worker-1 (the otherwise-pruned node), depth 1 over pod-to-node
	// reaches the edgeless pod p9.
	v := Project(prunableGraph(), Scope{Root: "c/worker-1", Depth: 1, Direction: DirectionBoth})
	ids := idSet(v)
	assert.True(t, ids["c/worker-1"], "traversal root kept")
	assert.True(t, ids["c/p9"], "edgeless pod reachable from root kept under traversal")
}

// Namespace filter still prunes edgeless pods (prune is active for cluster/ns
// filters, only name/traversal disable it).
func TestProject_NamespaceFilterStillPrunes(t *testing.T) {
	v := Project(prunableGraph(), Scope{Namespaces: map[string]struct{}{"ns": {}}})
	ids := idSet(v)
	assert.True(t, ids["c/p1"], "connected pod kept under namespace filter")
	assert.False(t, ids["c/p9"], "edgeless pod still pruned under namespace filter")
}
