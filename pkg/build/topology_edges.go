package build

import "github.com/akira-core/kube-state-graph/pkg/graph"

// TopologyEdges synthesises the topology relationship edges from a parsed
// Topology: pod-mounts-pvc and pod-to-node. Edge IDs are deterministic
// UUIDv5 — see graph.NewEdge. The pvc-to-netapp-aggr edge is produced by
// the Harvest join (Topology.StorageEdges), not here.
func TopologyEdges(t Topology) []*graph.Edge {
	edges := make([]*graph.Edge, 0, len(t.PodPVCs)+len(t.Pods))

	pvcByID := map[string]*graph.PVCNode{}
	for _, pv := range t.PVCs {
		pvcByID[pv.ID()] = pv
	}
	for _, b := range t.PodPVCs {
		pv, ok := pvcByID[b.PVCID]
		if !ok {
			continue
		}
		edges = append(edges, graph.NewEdge(
			graph.EdgeTypePodMountsPVC,
			b.PodID,
			b.PVCID,
			map[string]string{"claim_name": pv.Name()},
		))
	}

	// pod-to-node: one edge per scheduled pod. labels.node is already the
	// cluster-scoped node ID (set at pod construction only when scheduled).
	// Always intra-cluster. Dedupe defensively by (podID, nodeID).
	seenPodNode := make(map[[2]string]bool, len(t.Pods))
	for _, p := range t.Pods {
		nodeID := p.Labels()["node"]
		if nodeID == "" {
			continue
		}
		key := [2]string{p.ID(), nodeID}
		if seenPodNode[key] {
			continue
		}
		seenPodNode[key] = true
		edges = append(edges, graph.NewEdge(graph.EdgeTypePodToNode, p.ID(), nodeID, nil))
	}

	return edges
}
