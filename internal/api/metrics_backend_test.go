package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/observability"
)

// scrapeMetrics returns the /metrics exposition body of a server built over
// the supplied Metrics bundle.
func scrapeMetrics(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	s.metrics = m
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics") //nolint:noctx // test server URL
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

// The three routing metrics must be present from startup, not only after the
// first reload or the first backend failure — a series that materialises on
// failure gives an alert nothing to compare against.
func TestMetrics_RoutingMetricNamesExposed(t *testing.T) {
	m := observability.NewMetrics()
	m.SetBackends([]string{"zone-a", "zone-b"})
	body := scrapeMetrics(t, m)

	for _, name := range []string{
		"kube_state_graph_upstream_backends",
		"kube_state_graph_backend_config_reload_total",
		"kube_state_graph_backend_query_failures_total",
	} {
		assert.Contains(t, body, name)
	}
	assert.Contains(t, body, "kube_state_graph_upstream_backends 2")
	assert.Contains(t, body, `kube_state_graph_backend_config_reload_total{result="ok"} 0`)
	assert.Contains(t, body, `kube_state_graph_backend_query_failures_total{backend="zone-a"} 0`)
	assert.Contains(t, body, `kube_state_graph_backend_query_failures_total{backend="zone-b"} 0`)
}

func TestMetrics_BackendFailureCountedPerBackend(t *testing.T) {
	m := observability.NewMetrics()
	m.SetBackends([]string{"zone-a", "zone-b"})
	m.IncBackendQueryFailure("zone-b")
	body := scrapeMetrics(t, m)

	assert.Contains(t, body, `kube_state_graph_backend_query_failures_total{backend="zone-b"} 1`)
	assert.Contains(t, body, `kube_state_graph_backend_query_failures_total{backend="zone-a"} 0`)
}

func TestMetrics_ReloadResultCounted(t *testing.T) {
	m := observability.NewMetrics()
	m.IncBackendConfigReload("ok")
	m.IncBackendConfigReload("error")
	body := scrapeMetrics(t, m)

	assert.Contains(t, body, `kube_state_graph_backend_config_reload_total{result="ok"} 1`)
	assert.Contains(t, body, `kube_state_graph_backend_config_reload_total{result="error"} 1`)
	assert.Contains(t, body, `kube_state_graph_backend_config_reload_total{result="unchanged"} 0`)
}

// SetBackends replaces the per-backend series set: a backend a reload removed
// must not linger as a stale zero.
func TestMetrics_SetBackendsReplacesTheSeriesSet(t *testing.T) {
	m := observability.NewMetrics()
	m.SetBackends([]string{"zone-a", "retired"})
	m.SetBackends([]string{"zone-a"})
	body := scrapeMetrics(t, m)

	assert.Contains(t, body, `kube_state_graph_backend_query_failures_total{backend="zone-a"} 0`)
	assert.NotContains(t, body, `backend="retired"`)
	assert.Contains(t, body, "kube_state_graph_upstream_backends 1")
}

// The two established upstream metrics are stable contracts: adding a
// `backend` label to either would break every dashboard and recording rule
// built on them, so per-backend detail lives on the new metrics instead.
func TestMetrics_EstablishedUpstreamMetricsKeepTheirLabels(t *testing.T) {
	m := observability.NewMetrics()
	m.SetBackends([]string{"zone-a", "zone-b"})
	m.ObserveQueryDuration("kube_pod_info", 0.01)
	m.IncQueryFailure("kube_pod_info")
	body := scrapeMetrics(t, m)

	assert.Contains(t, body, `kube_state_graph_upstream_query_failures_total{query="kube_pod_info"} 1`)
	assert.Contains(t, body, `kube_state_graph_upstream_query_duration_seconds_count{query="kube_pod_info"} 1`)

	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "kube_state_graph_upstream_query_duration_seconds") &&
			!strings.HasPrefix(line, "kube_state_graph_upstream_query_failures_total") {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		assert.NotContains(t, line, "backend=",
			"the established upstream metrics must carry no backend label: %s", line)
	}
}

// The result-series histogram is labelled by query only and bucketed on powers
// of two, so an operator can read how close a leg sits to an upstream series
// limit. 70000 series lands above the 65536 bucket — just past a memory-derived
// ~67k VictoriaMetrics cap — which is exactly the reading this metric exists to
// surface before the limit starts rejecting the query.
func TestMetrics_ResultSeriesHistogramExposed(t *testing.T) {
	m := observability.NewMetrics()
	m.ObserveQuerySeries("kube_pod_info", 1200)
	m.ObserveQuerySeries("kube_pod_container_info", 70000)
	body := scrapeMetrics(t, m)

	for _, want := range []string{
		`kube_state_graph_upstream_query_result_series_bucket{query="kube_pod_info",le="1024"} 0`,
		`kube_state_graph_upstream_query_result_series_bucket{query="kube_pod_info",le="2048"} 1`,
		`kube_state_graph_upstream_query_result_series_bucket{query="kube_pod_info",le="+Inf"} 1`,
		`kube_state_graph_upstream_query_result_series_count{query="kube_pod_info"} 1`,
		`kube_state_graph_upstream_query_result_series_bucket{query="kube_pod_container_info",le="65536"} 0`,
		`kube_state_graph_upstream_query_result_series_bucket{query="kube_pod_container_info",le="131072"} 1`,
	} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, `kube_state_graph_upstream_query_result_series_bucket{backend=`,
		"the histogram carries no backend label")
}
