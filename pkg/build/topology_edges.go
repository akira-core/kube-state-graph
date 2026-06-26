package build

import "github.com/marz32one/kube-state-graph/pkg/graph"

// TopologyEdges synthesises the topology relationship edges from a parsed
// Topology: pod-mounts-pvc, pod-to-node, and pvc-to-storageclass. Edge IDs are
// deterministic UUIDv5 — see graph.NewEdge. The pod→node and pvc→storageclass
// relationships are explicit edges (this supersedes the D31 compound-nesting
// representation); the workload hierarchy nesting is presentation-only in the
// Cytoscape serialiser.
func TopologyEdges(t Topology) []*graph.Edge {
	edges := make([]*graph.Edge, 0, len(t.PodPVCs)+len(t.Pods)+len(t.PVCs))

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

	// pvc-to-storageclass: one edge per PVC with a resolved StorageClass, to the
	// (real, possibly bare) StorageClass node in the PVC's own cluster. Always
	// intra-cluster. Dedupe defensively by (pvcID, scID).
	seenPVCSC := make(map[[2]string]bool, len(t.PVCs))
	for _, pv := range t.PVCs {
		sc := pv.StorageClass()
		if sc == "" {
			continue
		}
		scID := graph.StorageClassID(pv.Labels()["cluster"], sc)
		key := [2]string{pv.ID(), scID}
		if seenPVCSC[key] {
			continue
		}
		seenPVCSC[key] = true
		edges = append(edges, graph.NewEdge(graph.EdgeTypePVCToStorageClass, pv.ID(), scID, nil))
	}

	return edges
}
