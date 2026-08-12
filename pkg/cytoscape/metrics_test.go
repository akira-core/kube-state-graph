package cytoscape

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

func metricsView(t *testing.T, nodes []graph.GraphNode, edges []*graph.Edge) Body {
	t.Helper()
	// Serialise a synthetic view directly (same pattern as compound_test.cy)
	// so projection prune cannot drop the fixture edge under test.
	byID := make(map[string]graph.GraphNode, len(nodes))
	for _, n := range nodes {
		byID[n.ID()] = n
	}
	return Serialise(&graph.Graph{NodesByID: byID}, graph.View{Nodes: nodes, Edges: edges})
}

func TestSerialise_EdgeWithoutMetricsOmitsKey(t *testing.T) {
	podA := &graph.PodNode{IDValue: "c/a", NameValue: "a", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}}
	podB := &graph.PodNode{IDValue: "c/b", NameValue: "b", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}}
	e := graph.NewEdge(graph.EdgeTypePodCallsPod, podA.ID(), podB.ID(), map[string]string{"cluster": "c"})
	body := metricsView(t, []graph.GraphNode{podA, podB}, []*graph.Edge{e})
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"metrics"`, "edge without metrics must omit the key entirely")
}

func TestSerialise_PartialMetricsOmitsMissingFields(t *testing.T) {
	podA := &graph.PodNode{IDValue: "c/a", NameValue: "a", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}}
	podB := &graph.PodNode{IDValue: "c/b", NameValue: "b", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}}
	er := 0.1
	e := graph.NewEdge(graph.EdgeTypePodCallsPod, podA.ID(), podB.ID(), map[string]string{"cluster": "c"}).
		WithMetrics(graph.EdgeMetrics{Rate: 5, ErrorRate: &er})
	body := metricsView(t, []graph.GraphNode{podA, podB}, []*graph.Edge{e})

	var found *EdgeMetricsDTO
	for _, ed := range body.Elements.Edges {
		if ed.Data.Source == podA.ID() {
			found = ed.Data.Metrics
		}
	}
	require.NotNil(t, found)
	assert.InDelta(t, 5.0, found.Rate, 1e-12)
	require.NotNil(t, found.ErrorRate)
	assert.InDelta(t, 0.1, *found.ErrorRate, 1e-12)
	assert.Nil(t, found.P90ServerMs)

	raw, err := json.Marshal(found)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"rate"`)
	assert.Contains(t, string(raw), `"error_rate"`)
	assert.NotContains(t, string(raw), `"p90_server_ms"`)

	// All values are JSON numbers, never strings.
	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &decoded))
	for k, v := range decoded {
		s := string(v)
		assert.NotEqual(t, '"', s[0], "%s must be a JSON number, not a string", k)
	}

	// labels gain no numeric key.
	for _, ed := range body.Elements.Edges {
		_, ok := ed.Data.Labels["rate"]
		assert.False(t, ok)
	}
}

func TestSerialise_SmallRateExponentFormRoundTrip(t *testing.T) {
	// One request over a 30-day window ≈ 3.86e-7 req/s.
	podA := &graph.PodNode{IDValue: "c/a", NameValue: "a", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}}
	podB := &graph.PodNode{IDValue: "c/b", NameValue: "b", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}}
	tiny := 3.86e-7
	er := 6.7e-8
	e := graph.NewEdge(graph.EdgeTypePodCallsPod, podA.ID(), podB.ID(), map[string]string{"cluster": "c"}).
		WithMetrics(graph.EdgeMetrics{Rate: tiny, ErrorRate: &er})
	body := metricsView(t, []graph.GraphNode{podA, podB}, []*graph.Edge{e})

	b1, err := json.Marshal(body)
	require.NoError(t, err)
	assert.Contains(t, string(b1), "e-", "small rate must serialise in exponent form")
	assert.NotContains(t, string(b1), `"rate":0`)
	assert.NotContains(t, string(b1), `"rate":0,`)

	var round Body
	require.NoError(t, json.Unmarshal(b1, &round))
	b2, err := json.Marshal(round)
	require.NoError(t, err)
	assert.Equal(t, string(b1), string(b2), "Marshal → Unmarshal → Marshal must be byte-identical")

	// Values survived as non-zero.
	var found *EdgeMetricsDTO
	for _, ed := range body.Elements.Edges {
		if ed.Data.Metrics != nil {
			found = ed.Data.Metrics
		}
	}
	require.NotNil(t, found)
	assert.NotZero(t, found.Rate)
	require.NotNil(t, found.ErrorRate)
	assert.NotZero(t, *found.ErrorRate)
}

func TestRound6_SignificantDigits(t *testing.T) {
	assert.InDelta(t, 3.86e-7, round6(3.86e-7), 1e-20)
	assert.InDelta(t, 1.23457, round6(1.23456789), 1e-12) // 6 sig digs
	assert.InDelta(t, 0.0, round6(0), 0)
}
