package cytoscape

import (
	"fmt"
	"testing"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// Synthetic estate for BenchmarkSerialise: enough clusters, namespaces,
// Applications and controllers that the compound-group tiers, not the real
// nodes, decide whether the serialiser scales.
const (
	benchClusters   = 4
	benchNamespaces = 40
	benchApps       = 400
	benchPods       = 8000
	benchPodsPerCtl = 2
)

func benchView() (*graph.Graph, graph.View) {
	nodes := make([]graph.GraphNode, 0, benchPods)
	edges := make([]*graph.Edge, 0, benchPods)
	for i := range benchPods {
		c := fmt.Sprintf("az-env-c%d", i%benchClusters)
		ns := fmt.Sprintf("ns-%02d", i%benchNamespaces)
		app := fmt.Sprintf("app-%03d", i%benchApps)
		pod := &graph.PodNode{
			IDValue:          graph.PodID(c, fmt.Sprintf("uid-%05d", i)),
			NameValue:        fmt.Sprintf("pod-%05d", i),
			LabelsValue:      map[string]string{"cluster": c, "namespace": ns},
			ApplicationValue: app,
			OwnerValue:       &graph.Owner{Kind: "Deployment", Name: fmt.Sprintf("deploy-%05d", i/benchPodsPerCtl)},
			StatusValue:      graph.StatusNormal,
		}
		nodes = append(nodes, pod)
		if i > 0 {
			edges = append(edges, graph.NewEdge(graph.EdgeTypePodCallsPod, nodes[i-1].ID(), pod.ID(), nil))
		}
	}
	g := graph.NewGraph(nodes, edges, time.Time{})
	view := graph.Project(g, graph.NewScope(nil, nil, false))
	return g, view
}

func BenchmarkSerialise(b *testing.B) {
	g, view := benchView()
	if len(view.Nodes) != benchPods {
		b.Fatalf("view holds %d nodes, want %d", len(view.Nodes), benchPods)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = Serialise(g, view)
	}
}
