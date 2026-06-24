package promql

import (
	"context"
	"testing"
	"time"

	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/internal/testlog"
)

// captureClientLogs is the shared slog-capture helper (pkg/internal/testlog).
func captureClientLogs(t *testing.T) *testlog.Buffer { return testlog.Capture(t) }

// fakeAPI is an in-package stub for the upstream v1.API: only Query is
// implemented; the embedded interface panics on anything else, which is
// exactly what we want — Instant must touch nothing but Query.
type fakeAPI struct {
	v1.API
	val   model.Value
	warns v1.Warnings
	err   error
}

func (f fakeAPI) Query(_ context.Context, _ string, _ time.Time, _ ...v1.Option) (model.Value, v1.Warnings, error) {
	return f.val, f.warns, f.err
}

// TestInstant_UpstreamWarningsLogged guards the fix for silently discarded
// upstream warnings: VictoriaMetrics signals truncated/partial responses via
// the warnings return, so a non-empty warnings slice must surface as a WARN
// log while the value is still returned to the caller unchanged.
func TestInstant_UpstreamWarningsLogged(t *testing.T) {
	buf := captureClientLogs(t)
	vec := model.Vector{
		&model.Sample{Metric: model.Metric{"cluster": "alpha"}, Value: 1},
	}
	c := &Client{api: fakeAPI{
		val:   vec,
		warns: v1.Warnings{"results may be partial: -search.maxSamplesPerQuery exceeded"},
	}}

	got, err := c.Instant(context.Background(), string(QPodInfo), "kube_pod_info", time.Unix(1000, 0))

	require.NoError(t, err, "warnings must not fail the query")
	assert.Equal(t, vec, got, "value is returned unchanged alongside warnings")

	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "upstream query returned warnings")
	assert.Contains(t, out, "query_name="+string(QPodInfo))
	assert.Contains(t, out, "search.maxSamplesPerQuery exceeded")
}

// TestInstant_NoWarnings_NoWarnLog: the happy path stays silent at WARN
// level — no warnings, no warn line.
func TestInstant_NoWarnings_NoWarnLog(t *testing.T) {
	buf := captureClientLogs(t)
	c := &Client{api: fakeAPI{val: model.Vector{}}}

	got, err := c.Instant(context.Background(), string(QNodeInfo), "kube_node_info", time.Unix(1000, 0))

	require.NoError(t, err)
	assert.Empty(t, got)
	assert.NotContains(t, buf.String(), "upstream query returned warnings")
}

// TestInstant_WarningsAlongsideError_StillLogged: an error accompanied by
// warnings keeps both signals — the warn line is emitted and the error is
// still wrapped and returned.
func TestInstant_WarningsAlongsideError_StillLogged(t *testing.T) {
	buf := captureClientLogs(t)
	c := &Client{api: fakeAPI{
		warns: v1.Warnings{"partial response"},
		err:   assert.AnError,
	}}

	_, err := c.Instant(context.Background(), string(QPodInfo), "kube_pod_info", time.Unix(1000, 0))

	require.Error(t, err)
	out := buf.String()
	assert.Contains(t, out, "upstream query returned warnings")
	assert.Contains(t, out, "partial response")
	assert.Contains(t, out, "promql query failed")
}
