package cytoscape

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

func deltaIO() *graph.IOMetrics {
	f := func(v float64) *float64 { return &v }
	return &graph.IOMetrics{
		ReadOps: f(100), WriteOps: f(50), ReadLatencyUs: f(650), WriteLatencyUs: f(300),
		ReadBytesPerSec: f(5e6), WriteBytesPerSec: f(1e6),
		ReadOpsDelta: f(50), WriteOpsDelta: f(0), ReadLatencyUsDelta: f(250),
		WriteLatencyUsDelta: f(-12.3456789), ReadBytesPerSecDelta: f(1e6),
		WriteBytesPerSecDelta: f(-3_000_000),
	}
}

// Spec: the six fields serialise under their wire names, a negative value
// keeps its sign through round6, and a zero delta is emitted as 0.
func TestMetricsDTO_DeltaFields(t *testing.T) {
	dto := metricsDTO(nil, deltaIO())
	require.NotNil(t, dto)
	raw, err := json.Marshal(dto)
	require.NoError(t, err)

	var got map[string]float64
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.InDelta(t, 50, got["read_ops_delta"], 1e-12)
	assert.Contains(t, got, "write_ops_delta", "a zero delta is present, not omitted")
	assert.InDelta(t, 0, got["write_ops_delta"], 0)
	assert.InDelta(t, 250, got["read_latency_us_delta"], 1e-12)
	assert.InDelta(t, -12.3457, got["write_latency_us_delta"], 1e-12, "sign kept, rounded to 6 significant digits")
	assert.InDelta(t, 1e6, got["read_bytes_per_sec_delta"], 1e-6)
	assert.InDelta(t, -3e6, got["write_bytes_per_sec_delta"], 1e-6)
	assert.Contains(t, string(raw), `"write_ops_delta":0`)
}

func TestMetricsDTO_NoDeltaFieldsWithoutDeltas(t *testing.T) {
	io := deltaIO()
	*io = graph.IOMetrics{ReadOps: io.ReadOps, WriteOps: io.WriteOps}
	raw, err := json.Marshal(metricsDTO(nil, io))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "_delta")
}

// Deltas never set `filled`: an IOMetrics holding only change fields yields no
// metrics key, and a change field is never serialised without its base field.
func TestMetricsDTO_DeltaNeverCountsAsAMeasurement(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	assert.Nil(t, metricsDTO(nil, &graph.IOMetrics{
		ReadOpsDelta: f(1), WriteOpsDelta: f(2), ReadLatencyUsDelta: f(3),
		WriteLatencyUsDelta: f(4), ReadBytesPerSecDelta: f(5), WriteBytesPerSecDelta: f(6),
	}))

	dto := metricsDTO(nil, &graph.IOMetrics{ReadOps: f(10), WriteOpsDelta: f(2), ReadLatencyUsDelta: f(3)})
	require.NotNil(t, dto)
	assert.NotNil(t, dto.ReadOps)
	assert.Nil(t, dto.WriteOpsDelta, "no write_ops, so no write_ops_delta")
	assert.Nil(t, dto.ReadLatencyUsDelta)
}

func TestSerialise_DeltaFieldsStayOutOfLabels(t *testing.T) {
	e := graph.NewEdge(graph.EdgeTypeStorageFlow, "a", "b", map[string]string{"tier": graph.StorageTierSVMPVC}).WithIO(*deltaIO())
	body := metricsView(t, nil, []*graph.Edge{e})
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"read_ops_delta":50`)
	assert.Contains(t, string(raw), `"labels":{"tier":"svm-pvc"}`)
}
