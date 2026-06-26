package cytoscape

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/graph"
)

// TestProperty_NoDanglingParent — for any serialised view, every emitted node's
// data.parent (when present) MUST reference an id also present in the same
// response. This covers the full synthetic group tier set (cluster, namespace,
// application, controller) across randomised multi-cluster graphs, and confirms
// the corpus actually exercises each workload tier (design.md D6).
func TestProperty_NoDanglingParent(t *testing.T) {
	apps := []string{"", "checkout", "cart"}
	kinds := []string{"", "Deployment", "DaemonSet"}
	tiersSeen := map[string]bool{}

	for seed := int64(1); seed <= 60; seed++ {
		r := rand.New(rand.NewSource(seed))
		clusters := 1 + r.Intn(3)
		var nodes []graph.GraphNode
		for c := range clusters {
			cl := fmt.Sprintf("cluster-%d", c)
			nodeID := graph.K8sNodeID(cl, "worker-0")
			nodes = append(nodes, &graph.K8sNode{IDValue: nodeID, NameValue: "worker-0", LabelsValue: map[string]string{"cluster": cl}})
			nodes = append(nodes, &graph.StorageClassNode{IDValue: graph.StorageClassID(cl, "gp3"), NameValue: "gp3", LabelsValue: map[string]string{"cluster": cl}})
			for p := range r.Intn(4) {
				labels := map[string]string{"cluster": cl, "namespace": fmt.Sprintf("ns-%d", p%2)}
				if r.Intn(2) == 0 {
					labels["node"] = nodeID // sometimes scheduled, sometimes not
				}
				pod := &graph.PodNode{IDValue: graph.PodID(cl, fmt.Sprintf("u-%d-%d", c, p)), NameValue: fmt.Sprintf("pod-%d-%d", c, p), LabelsValue: labels}
				pod.ApplicationValue = apps[r.Intn(len(apps))]
				if k := kinds[r.Intn(len(kinds))]; k != "" {
					pod.OwnerValue = &graph.Owner{Kind: k, Name: "w"}
				}
				nodes = append(nodes, pod)
			}
			for v := range r.Intn(3) {
				nodes = append(nodes, &graph.PVCNode{
					IDValue:           graph.PVCID(cl, "ns-0", fmt.Sprintf("claim-%d-%d", c, v)),
					NameValue:         fmt.Sprintf("claim-%d-%d", c, v),
					LabelsValue:       map[string]string{"cluster": cl, "namespace": "ns-0"},
					StorageClassValue: "gp3",
				})
			}
			nodes = append(nodes, &graph.ServiceNode{IDValue: graph.ServiceID(cl, "ns-0", "svc"), NameValue: "svc", LabelsValue: map[string]string{"cluster": cl, "namespace": "ns-0"}})
		}
		nodes = append(nodes, &graph.ExternalNode{IDValue: graph.ExternalID("admin"), NameValue: "admin", LabelsValue: map[string]string{}})

		body := cy(t, nodes, nil)
		ids := make(map[string]struct{}, len(body.Elements.Nodes))
		for _, n := range body.Elements.Nodes {
			ids[n.Data.ID] = struct{}{}
			switch n.Data.Type {
			case nodeTypeNamespace, nodeTypeApplication, nodeTypeController:
				tiersSeen[n.Data.Type] = true
			}
		}
		for _, n := range body.Elements.Nodes {
			if n.Data.Parent == "" {
				continue
			}
			_, ok := ids[n.Data.Parent]
			require.Truef(t, ok, "seed=%d: node %s has dangling parent %s", seed, n.Data.ID, n.Data.Parent)
		}
	}

	require.True(t, tiersSeen[nodeTypeNamespace], "expected the corpus to exercise namespace group synthesis")
	require.True(t, tiersSeen[nodeTypeApplication], "expected the corpus to exercise application group synthesis")
	require.True(t, tiersSeen[nodeTypeController], "expected the corpus to exercise controller group synthesis")
}
