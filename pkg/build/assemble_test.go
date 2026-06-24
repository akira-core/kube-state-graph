package build

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/graph"
)

// assemble's append order is load-bearing: graph.NewGraph dedupes colliding
// node IDs keep-first (ServiceID mirrors PVCID keying), so the authoritative
// topology node must win against an on-demand service-graph node minting the
// same ID. This pins the contract THROUGH the real assemble — the pkg/graph
// collision tests hand-order their input and cannot catch an assemble
// reordering.
func TestAssemble_TopologyWinsIDCollision(t *testing.T) {
	sharedID := graph.PVCID("prod-1", "data", "shared")
	require.Equal(t, sharedID, graph.ServiceID("prod-1", "data", "shared"),
		"test premise: PVC and Service IDs share the grammar")

	topo := Topology{
		PVCs: []*graph.PVCNode{{
			IDValue:     sharedID,
			NameValue:   "shared",
			LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "data"},
		}},
	}
	sg := ServiceGraphResult{
		ServiceNodes: []*graph.ServiceNode{{
			IDValue:     sharedID,
			NameValue:   "shared",
			LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "data"},
		}},
	}

	nodes, edges := assemble(topo, sg)
	g := graph.NewGraph(nodes, edges, time.Unix(0, 0).UTC())

	got, ok := g.NodesByID[sharedID]
	require.True(t, ok)
	assert.Equal(t, graph.NodeTypePVC, got.Type(),
		"authoritative topology PVC must win the keep-first dedupe over the on-demand service node")
}
