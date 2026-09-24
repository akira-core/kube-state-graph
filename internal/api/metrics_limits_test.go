package api

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/akira-core/kube-state-graph/internal/observability"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// The server's recorder must satisfy both optional upgrades, or the Router
// would silently no-op every limiter and cache observation.
var (
	_ promql.LimiterMetrics = (*observability.Metrics)(nil)
	_ promql.CacheMetrics   = (*observability.Metrics)(nil)
)

func TestMetrics_LimitAndCacheMetricNamesExposed(t *testing.T) {
	m := observability.NewMetrics()
	m.AddInflight("zone-a", 1)
	m.ObserveSlotWait("zone-a", 0.2)
	m.IncCacheHit()
	m.IncCacheMiss()
	m.IncCacheMiss()
	m.IncCacheCoalesced()
	m.IncCacheEviction()
	m.SetCacheSeries(42)
	body := scrapeMetrics(t, m)

	assert.Contains(t, body, `kube_state_graph_upstream_inflight{backend="zone-a"} 1`)
	assert.Contains(t, body, `kube_state_graph_upstream_slot_wait_seconds_count{backend="zone-a"} 1`)
	assert.Contains(t, body, "kube_state_graph_query_cache_hits_total 1")
	assert.Contains(t, body, "kube_state_graph_query_cache_misses_total 2")
	assert.Contains(t, body, "kube_state_graph_query_cache_coalesced_total 1")
	assert.Contains(t, body, "kube_state_graph_query_cache_evictions_total 1")
	assert.Contains(t, body, "kube_state_graph_query_cache_series 42")
}

// The established query metrics keep their `query`-only label sets.
func TestMetrics_UpstreamQueryMetricsKeepLabels(t *testing.T) {
	m := observability.NewMetrics()
	m.ObserveQueryDuration("kube_pod_info", 0.1)
	m.IncQueryFailure("kube_pod_info")
	body := scrapeMetrics(t, m)
	assert.Contains(t, body, `kube_state_graph_upstream_query_duration_seconds_count{query="kube_pod_info"} 1`)
	assert.Contains(t, body, `kube_state_graph_upstream_query_failures_total{query="kube_pod_info"} 1`)
}
