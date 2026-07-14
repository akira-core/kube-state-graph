package cytoscape

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// A pod's controller owner is emitted as the nullable top-level data.owner
// object (never inside labels), and is omitted entirely for a pod with no
// controller owner.
func TestSerialiseCytoscape_OwnerAttribute(t *testing.T) {
	owned := &graph.PodNode{
		IDValue:     "c1/p1",
		NameValue:   "checkout",
		LabelsValue: map[string]string{"cluster": "c1", "namespace": "shop"},
		OwnerValue:  &graph.Owner{Kind: "Deployment", Name: "checkout"},
	}
	bare := &graph.PodNode{
		IDValue:     "c1/p2",
		NameValue:   "adhoc",
		LabelsValue: map[string]string{"cluster": "c1", "namespace": "shop"},
	}

	body := cy(t, []graph.GraphNode{owned, bare}, nil)
	nodes := cyNodesByID(body)

	require.NotNil(t, nodes["c1/p1"].Owner, "owned pod must carry data.owner")
	assert.Equal(t, "Deployment", nodes["c1/p1"].Owner.Kind)
	assert.Equal(t, "checkout", nodes["c1/p1"].Owner.Name)
	assert.Nil(t, nodes["c1/p2"].Owner, "pod with no controller owner must omit data.owner")

	_, hasKind := nodes["c1/p1"].Labels["owner_kind"]
	_, hasName := nodes["c1/p1"].Labels["owner_name"]
	assert.False(t, hasKind, "owner must not appear inside labels")
	assert.False(t, hasName, "owner must not appear inside labels")

	raw, err := json.Marshal(body)
	require.NoError(t, err)
	s := string(raw)
	assert.Contains(t, s, `"owner":{"kind":"Deployment","name":"checkout"}`,
		"owner must serialise as a nested object")
	assert.Equal(t, 1, strings.Count(s, `"owner"`),
		"owner must be omitted (omitempty) for the pod with no owner")
}
