package build

import (
	"testing"

	"github.com/marz32one/kube-state-graph/pkg/graph"
)

func TestTopologyEdges_PVCMountWithBinding(t *testing.T) {
	t.Parallel()
	pvc := &graph.PVCNode{
		IDValue:     "test/default/data-1",
		NameValue:   "data-1",
		LabelsValue: map[string]string{},
	}
	pod := &graph.PodNode{
		IDValue:     "test/uid-3",
		NameValue:   "db",
		LabelsValue: map[string]string{"node": "test/node-a"},
	}
	top := Topology{
		Pods: []*graph.PodNode{pod},
		PVCs: []*graph.PVCNode{pvc},
		PodPVCs: []PodPVCBinding{
			{PodID: pod.ID(), PVCID: pvc.ID()},
		},
	}

	edges := TopologyEdges(top)
	// One pod-mounts-pvc edge plus one pod-to-node edge (the pod is scheduled).
	if len(edges) != 2 {
		t.Fatalf("len(edges)=%d, want 2", len(edges))
	}
	var pvcEdge, nodeEdge *graph.Edge
	for _, e := range edges {
		switch e.Type {
		case graph.EdgeTypePodMountsPVC:
			pvcEdge = e
		case graph.EdgeTypePodToNode:
			nodeEdge = e
		default:
		}
	}
	if pvcEdge == nil {
		t.Fatalf("missing pod-mounts-pvc edge")
	}
	if pvcEdge.Source != pod.ID() || pvcEdge.Target != pvc.ID() {
		t.Errorf("pvc edge endpoints src=%q tgt=%q", pvcEdge.Source, pvcEdge.Target)
	}
	if pvcEdge.Labels["claim_name"] != "data-1" {
		t.Errorf("claim_name label=%q want data-1", pvcEdge.Labels["claim_name"])
	}
	if nodeEdge == nil {
		t.Fatalf("missing pod-to-node edge")
	}
	if nodeEdge.Source != pod.ID() || nodeEdge.Target != "test/node-a" {
		t.Errorf("pod-to-node edge endpoints src=%q tgt=%q", nodeEdge.Source, nodeEdge.Target)
	}
}

func TestTopologyEdges_SkipsBindingForMissingPVC(t *testing.T) {
	t.Parallel()
	pod := &graph.PodNode{
		IDValue:     "test/uid-4",
		NameValue:   "ghost",
		LabelsValue: map[string]string{"node": "test/node-a"},
	}
	top := Topology{
		Pods: []*graph.PodNode{pod},
		PodPVCs: []PodPVCBinding{
			{PodID: pod.ID(), PVCID: "test/default/missing"},
		},
	}

	edges := TopologyEdges(top)
	for _, e := range edges {
		if e.Type == graph.EdgeTypePodMountsPVC {
			t.Fatalf("unexpected pvc edge for missing PVC: %+v", e)
		}
	}
}
