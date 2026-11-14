package graph

import (
	"fmt"
	"testing"
)

// Synthetic estate sizes for the projection benchmarks. They are large enough
// that an accidental quadratic step dominates the profile, and small enough
// that -count=5 stays well under a minute.
const (
	benchPods       = 4000
	benchNamespaces = 40
	benchNodes      = 80
	benchAggrs      = 16
	benchCtrls      = 8
	benchSVMs       = 4
)

// benchInventoryGraph is a /v1/graph-shaped estate: every pod runs on a node
// and mounts its own claim, every claim sits on an aggregate, and every pod
// calls its successor, so the default prune keeps the whole estate and the
// projection walks every node and edge.
func benchInventoryGraph() *Graph {
	nodes := make([]GraphNode, 0, benchPods*2+benchNodes+benchAggrs+benchCtrls)
	edges := make([]*Edge, 0, benchPods*4)
	for i := range benchCtrls {
		nodes = append(nodes, stCtrl(fmt.Sprintf("ctrl-%02d", i)))
	}
	for i := range benchAggrs {
		nodes = append(nodes, stAggr(fmt.Sprintf("aggr%02d", i), fmt.Sprintf("ctrl-%02d", i%benchCtrls)))
	}
	for i := range benchNodes {
		nodes = append(nodes, stNode(fmt.Sprintf("worker-%03d", i)))
	}
	podIDs := make([]string, benchPods)
	for i := range benchPods {
		ns := fmt.Sprintf("ns-%02d", i%benchNamespaces)
		node := fmt.Sprintf("worker-%03d", i%benchNodes)
		pod := stPod(ns, fmt.Sprintf("pod-%05d", i), fmt.Sprintf("uid-%05d", i), node)
		pvc := stPVC(ns, fmt.Sprintf("data-%05d", i))
		aggr := NetAppAggrID(stOC, fmt.Sprintf("aggr%02d", i%benchAggrs))
		nodes = append(nodes, pod, pvc)
		podIDs[i] = pod.ID()
		edges = append(edges,
			NewEdge(EdgeTypePodToNode, pod.ID(), K8sNodeID(stC, node), nil),
			NewEdge(EdgeTypePodMountsPVC, pod.ID(), pvc.ID(), nil),
			NewEdge(EdgeTypePVCToNetAppAggr, pvc.ID(), aggr, nil),
		)
	}
	for i := range benchPods {
		edges = append(edges, NewEdge(EdgeTypePodCallsPod, podIDs[i], podIDs[(i+1)%benchPods], nil))
	}
	return stGraph(nodes, edges)
}

// benchStorageGraph is a /v1/storage-graph-shaped estate: one storage-flow
// chain per claim, the shared node-aggr / aggr-svm hops emitted once each.
func benchStorageGraph() *Graph {
	nodes := make([]GraphNode, 0, benchPods*2+benchNodes+benchAggrs+benchCtrls+benchSVMs)
	edges := make([]*Edge, 0, benchPods*3+benchAggrs*(1+benchSVMs))
	for i := range benchCtrls {
		nodes = append(nodes, stCtrl(fmt.Sprintf("ctrl-%02d", i)))
	}
	for i := range benchSVMs {
		nodes = append(nodes, stSVM(fmt.Sprintf("svm_%d", i)))
	}
	for i := range benchAggrs {
		ctrl := fmt.Sprintf("ctrl-%02d", i%benchCtrls)
		aggr := stAggr(fmt.Sprintf("aggr%02d", i), ctrl)
		nodes = append(nodes, aggr)
		edges = append(edges, stHop(StorageTierNodeAggr, NetAppNodeID(stOC, ctrl), aggr.ID(), nil, nil))
		for s := range benchSVMs {
			edges = append(edges, stHop(StorageTierAggrSVM, aggr.ID(), NetAppSVMID(stOC, fmt.Sprintf("svm_%d", s)), nil, nil))
		}
	}
	for i := range benchNodes {
		nodes = append(nodes, stNode(fmt.Sprintf("worker-%03d", i)))
	}
	for i := range benchPods {
		ns := fmt.Sprintf("ns-%02d", i%benchNamespaces)
		node := K8sNodeID(stC, fmt.Sprintf("worker-%03d", i%benchNodes))
		pod := stPod(ns, fmt.Sprintf("pod-%05d", i), fmt.Sprintf("uid-%05d", i), fmt.Sprintf("worker-%03d", i%benchNodes))
		pvc := stPVC(ns, fmt.Sprintf("data-%05d", i))
		aggr := NetAppAggrID(stOC, fmt.Sprintf("aggr%02d", i%benchAggrs))
		svm := NetAppSVMID(stOC, fmt.Sprintf("svm_%d", i%benchSVMs))
		nodes = append(nodes, pod, pvc)
		edges = append(edges,
			stHop(StorageTierSVMPVC, svm, pvc.ID(), map[string]string{ClaimAggrLabel: aggr}, stIO(float64(i%97+1))),
			stHop(StorageTierPVCPod, pvc.ID(), pod.ID(), nil, nil),
			stHop(StorageTierPodNode, pod.ID(), node, nil, nil),
		)
	}
	return stGraph(nodes, edges)
}

func BenchmarkProject(b *testing.B) {
	g := benchInventoryGraph()
	scope := NewScope(nil, nil, false)
	b.ReportAllocs()
	for b.Loop() {
		_ = Project(g, scope)
	}
}

func BenchmarkProjectStorage(b *testing.B) {
	g := benchStorageGraph()
	roots := make([]string, benchNodes)
	for i := range benchNodes {
		roots[i] = fmt.Sprintf("worker-%03d", i)
	}
	scope, err := NewStorageScope(nil, nil, StorageRootNode, roots)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = ProjectStorage(g, scope)
	}
}

// BenchmarkNewStorageScope_ONTAPRoots parses an aggr root set mixing bare
// names and qualified refs, half of which the bare set subsumes — the
// de-duplication step that used to scan the bare list once per qualified ref.
func BenchmarkNewStorageScope_ONTAPRoots(b *testing.B) {
	const n = 4000
	values := make([]string, 0, 2*n)
	for i := range n {
		values = append(values, fmt.Sprintf("aggr%05d", i))
		values = append(values, fmt.Sprintf("%s/aggr%05d", stOC, i+n/2))
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := NewStorageScope(nil, nil, StorageRootAggr, values); err != nil {
			b.Fatal(err)
		}
	}
}

// TestBenchEstates_ProjectTheWholeEstate keeps the benchmarks honest: a
// generator change that made either projection drop the estate would leave the
// benchmark timing an empty view.
func TestBenchEstates_ProjectTheWholeEstate(t *testing.T) {
	inv := Project(benchInventoryGraph(), NewScope(nil, nil, false))
	if got, want := len(inv.Nodes), benchPods*2+benchNodes+benchAggrs+benchCtrls; got != want {
		t.Fatalf("Project kept %d nodes, want %d", got, want)
	}
	if got, want := len(inv.Edges), benchPods*4; got != want {
		t.Fatalf("Project kept %d edges, want %d", got, want)
	}

	roots := make([]string, benchNodes)
	for i := range benchNodes {
		roots[i] = fmt.Sprintf("worker-%03d", i)
	}
	scope, err := NewStorageScope(nil, nil, StorageRootNode, roots)
	if err != nil {
		t.Fatal(err)
	}
	st := ProjectStorage(benchStorageGraph(), scope)
	if got, want := len(st.Nodes), benchPods*2+benchNodes+benchAggrs+benchCtrls+benchSVMs; got != want {
		t.Fatalf("ProjectStorage kept %d nodes, want %d", got, want)
	}
	if len(st.Edges) < benchPods*3 {
		t.Fatalf("ProjectStorage kept %d edges, want at least %d", len(st.Edges), benchPods*3)
	}
}
