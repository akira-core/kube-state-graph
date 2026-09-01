package cytoscape

import (
	"encoding/json"
	"strings"
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
	require.NotNil(t, found.Rate)
	assert.InDelta(t, 5.0, *found.Rate, 1e-12)
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
		// Compare byte-to-byte: assert.NotEqual on an untyped rune constant and
		// a byte compares int32 against uint8, which reflect.DeepEqual reports
		// as unequal for ANY input — the assertion would never fail.
		assert.False(t, strings.HasPrefix(s, `"`), "%s must be a JSON number, not a string", k)
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
	require.NotNil(t, found.Rate)
	assert.NotZero(t, *found.Rate)
	require.NotNil(t, found.ErrorRate)
	assert.NotZero(t, *found.ErrorRate)
}

func TestSerialise_IOMetricsOnly(t *testing.T) {
	pvc := &graph.PVCNode{IDValue: "c/ns/claim", NameValue: "claim", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}}
	aggr := &graph.NetAppAggrNode{IDValue: "netapp/oc/aggr/a1", NameValue: "a1", LabelsValue: map[string]string{"ontap_cluster": "oc", "node": "n1"}}
	readOps, writeOps, readBps, writeBps := 150.0, 40.0, 5242880.0, 1000000.0
	maxIOPS, maxBps := 5000.0, 262144000.0
	e := graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, pvc.ID(), aggr.ID(), nil).
		WithIO(graph.IOMetrics{
			ReadOps: &readOps, WriteOps: &writeOps,
			ReadBytesPerSec: &readBps, WriteBytesPerSec: &writeBps,
			MaxIOPS: &maxIOPS, MaxBytesPerSec: &maxBps,
		})
	body := metricsView(t, []graph.GraphNode{pvc, aggr}, []*graph.Edge{e})
	var found *EdgeMetricsDTO
	for _, ed := range body.Elements.Edges {
		found = ed.Data.Metrics
	}
	require.NotNil(t, found)
	require.NotNil(t, found.ReadOps)
	assert.InDelta(t, 150.0, *found.ReadOps, 1e-12)
	require.NotNil(t, found.WriteOps)
	require.NotNil(t, found.ReadBytesPerSec)
	assert.InDelta(t, 5242880.0, *found.ReadBytesPerSec, 1e-12)
	require.NotNil(t, found.WriteBytesPerSec)
	assert.InDelta(t, 1000000.0, *found.WriteBytesPerSec, 1e-12)
	require.NotNil(t, found.MaxIOPS)
	assert.InDelta(t, 5000.0, *found.MaxIOPS, 1e-12)
	require.NotNil(t, found.MaxBytesPerSec)
	assert.InDelta(t, 262144000.0, *found.MaxBytesPerSec, 1e-12)
	assert.Nil(t, found.Rate)
	assert.Nil(t, found.ErrorRate)
	raw, err := json.Marshal(found)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"read_ops"`)
	assert.Contains(t, string(raw), `"read_bytes_per_sec"`)
	assert.Contains(t, string(raw), `"write_bytes_per_sec"`)
	assert.Contains(t, string(raw), `"max_iops"`)
	assert.Contains(t, string(raw), `"max_bytes_per_sec"`)
	assert.NotContains(t, string(raw), `"rate"`)
}

// A ceiling never makes a metrics object exist on its own. The builder cannot
// produce this shape (the ceiling's policy group is recovered FROM a matched
// workload series, and it is attached only inside that branch), and the serialiser must not invent one if it ever
// could.
func TestMetricsDTO_CeilingAloneOmitsMetrics(t *testing.T) {
	maxIOPS := 5000.0
	assert.Nil(t, metricsDTO(nil, &graph.IOMetrics{MaxIOPS: &maxIOPS}))
}

func TestSerialise_NeitherFamilyOmitsMetrics(t *testing.T) {
	e := graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, "a", "b", nil)
	body := metricsView(t, nil, []*graph.Edge{e})
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"metrics"`)
}

func TestMetricsDTO_REDPrecedence(t *testing.T) {
	readOps, readBps := 1.0, 5242880.0
	maxIOPS := 5000.0
	dto := metricsDTO(&graph.EdgeMetrics{Rate: 5}, &graph.IOMetrics{
		ReadOps: &readOps, ReadBytesPerSec: &readBps, MaxIOPS: &maxIOPS,
	})
	require.NotNil(t, dto)
	require.NotNil(t, dto.Rate)
	assert.InDelta(t, 5.0, *dto.Rate, 1e-12)
	assert.Nil(t, dto.ReadOps, "RED wins the impossible both-set case")
	assert.Nil(t, dto.ReadBytesPerSec, "RED wins the impossible both-set case")
	assert.Nil(t, dto.MaxIOPS, "RED wins the impossible both-set case")
}

func TestRound6_SignificantDigits(t *testing.T) {
	assert.InDelta(t, 3.86e-7, round6(3.86e-7), 1e-20)
	assert.InDelta(t, 1.23457, round6(1.23456789), 1e-12) // 6 sig digs
	assert.InDelta(t, 0.0, round6(0), 0)
}
