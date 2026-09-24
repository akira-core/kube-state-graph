package observability

// These methods let *Metrics satisfy the small, no-op-tolerant metrics
// interfaces declared by the reusable pkg/ packages (pkg/promql.Metrics and
// pkg/build.Metrics) without those packages importing internal/observability.
// The interfaces are structural, so only the method set must line up.

// ObserveQueryDuration records an upstream PromQL query duration in seconds,
// labelled by query name. Satisfies pkg/promql.Metrics.
func (m *Metrics) ObserveQueryDuration(name string, seconds float64) {
	m.UpstreamQueryDur.WithLabelValues(name).Observe(seconds)
}

// IncQueryFailure increments the upstream PromQL query failure counter for the
// named query. Satisfies pkg/promql.Metrics.
func (m *Metrics) IncQueryFailure(name string) {
	m.UpstreamQueryFail.WithLabelValues(name).Inc()
}

// ObserveQuerySeries records how many series one successful upstream query
// returned, labelled by query name. Satisfies pkg/promql.SeriesMetrics — the
// OPTIONAL upgrade over pkg/promql.Metrics.
func (m *Metrics) ObserveQuerySeries(name string, n int) {
	m.UpstreamQuerySeries.WithLabelValues(name).Observe(float64(n))
}

// SetGraphNodeCounts replaces the last-build node-count gauge with counts keyed
// by [cluster, kind]. Satisfies pkg/build.Metrics.
func (m *Metrics) SetGraphNodeCounts(counts map[[2]string]int) {
	m.GraphNodeCount.Reset()
	for k, c := range counts {
		m.GraphNodeCount.WithLabelValues(k[0], k[1]).Set(float64(c))
	}
}

// SetGraphEdgeCounts replaces the last-build edge-count gauge with counts keyed
// by [type, cross_cluster]. Satisfies pkg/build.Metrics.
func (m *Metrics) SetGraphEdgeCounts(counts map[[2]string]int) {
	m.GraphEdgeCount.Reset()
	for k, c := range counts {
		m.GraphEdgeCount.WithLabelValues(k[0], k[1]).Set(float64(c))
	}
}

// SetClustersObserved records the distinct cluster count from the most recent
// build. Satisfies pkg/build.Metrics.
func (m *Metrics) SetClustersObserved(n int) {
	m.ClustersObserved.Set(float64(n))
}

// SetBackends records the live routing table's backends. It sets the backend
// gauge and pre-creates each backend's failure counter at zero, so a healthy
// backend is a visible zero series rather than an absent one — a counter that
// materialises only on the first failure is a dashboard trap.
//
// Satisfies pkg/promql.RouterMetrics — the OPTIONAL upgrade over
// pkg/promql.Metrics, so an embedder supplying only the two required methods
// keeps working unchanged.
func (m *Metrics) SetBackends(names []string) {
	m.UpstreamBackends.Set(float64(len(names)))
	m.BackendQueryFailed.Reset()
	for _, n := range names {
		m.BackendQueryFailed.WithLabelValues(n)
	}
}

// IncBackendQueryFailure increments the per-backend upstream failure counter.
// It is deliberately a SEPARATE metric from IncQueryFailure's: the established
// kube_state_graph_upstream_query_failures_total keeps its `query`-only label
// set. Satisfies pkg/promql.RouterMetrics.
func (m *Metrics) IncBackendQueryFailure(backend string) {
	m.BackendQueryFailed.WithLabelValues(backend).Inc()
}

// IncBackendConfigReload increments the routing-table reload counter, labelled
// by result. Satisfies pkg/promql.RouterMetrics.
func (m *Metrics) IncBackendConfigReload(result string) {
	m.BackendReload.WithLabelValues(result).Inc()
}

// AddInflight moves the per-backend in-flight gauge. Satisfies
// pkg/promql.LimiterMetrics.
func (m *Metrics) AddInflight(backend string, delta int) {
	m.UpstreamInflight.WithLabelValues(backend).Add(float64(delta))
}

// ObserveSlotWait records a query's wait for its store's concurrency slot.
// Satisfies pkg/promql.LimiterMetrics.
func (m *Metrics) ObserveSlotWait(backend string, seconds float64) {
	m.UpstreamSlotWait.WithLabelValues(backend).Observe(seconds)
}

// IncCacheHit / IncCacheMiss / IncCacheCoalesced / IncCacheEviction /
// SetCacheSeries satisfy pkg/promql.CacheMetrics.
func (m *Metrics) IncCacheHit()         { m.QueryCacheHits.Inc() }
func (m *Metrics) IncCacheMiss()        { m.QueryCacheMisses.Inc() }
func (m *Metrics) IncCacheCoalesced()   { m.QueryCacheShared.Inc() }
func (m *Metrics) IncCacheEviction()    { m.QueryCacheEvicted.Inc() }
func (m *Metrics) SetCacheSeries(n int) { m.QueryCacheSeries.Set(float64(n)) }
