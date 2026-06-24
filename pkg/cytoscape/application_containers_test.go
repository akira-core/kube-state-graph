package cytoscape

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/graph"
)

// A pod's ArgoCD Application and container list are emitted as the typed
// data.application string and data.containers array (never inside labels), and
// both are omitted entirely for a pod that has neither.
func TestSerialiseCytoscape_ApplicationAndContainers(t *testing.T) {
	enriched := &graph.PodNode{
		IDValue:          "c1/p1",
		NameValue:        "checkout",
		LabelsValue:      map[string]string{"cluster": "c1", "namespace": "shop"},
		ApplicationValue: "checkout",
		ContainersValue: []graph.Container{
			{Name: "app", Image: "reg/app:1.2"},
			{Name: "sidecar", Image: "reg/proxy:0.9"},
		},
	}
	bare := &graph.PodNode{
		IDValue:     "c1/p2",
		NameValue:   "adhoc",
		LabelsValue: map[string]string{"cluster": "c1", "namespace": "shop"},
	}

	body := cy(t, []graph.GraphNode{enriched, bare}, nil)
	nodes := cyNodesByID(body)

	assert.Equal(t, "checkout", nodes["c1/p1"].Application, "enriched pod carries data.application")
	require.Equal(t, []graph.Container{
		{Name: "app", Image: "reg/app:1.2"},
		{Name: "sidecar", Image: "reg/proxy:0.9"},
	}, nodes["c1/p1"].Containers, "enriched pod carries ordered data.containers")

	assert.Empty(t, nodes["c1/p2"].Application, "pod with no Application omits it")
	assert.Nil(t, nodes["c1/p2"].Containers, "pod with no containers omits them")

	// Neither must leak into labels.
	_, hasApp := nodes["c1/p1"].Labels["application"]
	_, hasCtr := nodes["c1/p1"].Labels["containers"]
	assert.False(t, hasApp, "application must not appear inside labels")
	assert.False(t, hasCtr, "containers must not appear inside labels")

	raw, err := json.Marshal(body)
	require.NoError(t, err)
	s := string(raw)
	assert.Contains(t, s, `"application":"checkout"`)
	assert.Contains(t, s, `"containers":[{"name":"app","image":"reg/app:1.2"},{"name":"sidecar","image":"reg/proxy:0.9"}]`,
		"containers serialise as an ordered array of {name, image} objects")
	assert.Equal(t, 1, strings.Count(s, `"application"`), "application omitted (omitempty) for the bare pod")
	assert.Equal(t, 1, strings.Count(s, `"containers"`), "containers omitted (omitempty) for the bare pod")
}
