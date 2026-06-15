package cytoscape

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/graph"
)

// A K8s node's Ready status is emitted as the typed data.ready_status string
// (never inside labels), omitted entirely when "", and never carried by
// non-node types.
func TestSerialiseCytoscape_ReadyStatus(t *testing.T) {
	ready := &graph.K8sNode{
		IDValue:          "c1/w0",
		NameValue:        "w0",
		LabelsValue:      map[string]string{"cluster": "c1"},
		ReadyStatusValue: graph.ReadyStatusReady,
	}
	bare := &graph.K8sNode{
		IDValue:     "c1/w1",
		NameValue:   "w1",
		LabelsValue: map[string]string{"cluster": "c1"},
	}
	pod := &graph.PodNode{
		IDValue:     "c1/p1",
		NameValue:   "checkout",
		LabelsValue: map[string]string{"cluster": "c1", "namespace": "shop"},
	}

	body := cy(t, []graph.GraphNode{ready, bare, pod}, nil)
	nodes := cyNodesByID(body)

	assert.Equal(t, graph.ReadyStatusReady, nodes["c1/w0"].ReadyStatus, "node carries data.ready_status")
	assert.Empty(t, nodes["c1/w1"].ReadyStatus, "node with no Ready data omits ready_status")
	assert.Empty(t, nodes["c1/p1"].ReadyStatus, "non-node types never carry ready_status")

	// Must not leak into labels.
	_, hasStatus := nodes["c1/w0"].Labels["ready_status"]
	assert.False(t, hasStatus, "ready_status must not appear inside labels")

	raw, err := json.Marshal(body)
	require.NoError(t, err)
	s := string(raw)
	assert.Contains(t, s, `"ready_status":"Ready"`)
	assert.Equal(t, 1, strings.Count(s, `"ready_status"`),
		"ready_status omitted (omitempty) for the bare node and the pod")
}
