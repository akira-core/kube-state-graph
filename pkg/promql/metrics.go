package promql

// Metrics records upstream PromQL query observations. It is intentionally tiny
// so an embedding application can supply its own recorder (or none). A nil
// Metrics passed to New is treated as a no-op — no upstream-query self-metrics
// are emitted. The concrete kube-state-graph implementation lives in
// internal/observability and satisfies this interface structurally.
type Metrics interface {
	// ObserveQueryDuration records a query's wall-clock duration in seconds,
	// labelled by query name.
	ObserveQueryDuration(name string, seconds float64)
	// IncQueryFailure increments the failure counter for the named query.
	IncQueryFailure(name string)
}

// SeriesMetrics is the OPTIONAL upgrade interface a Metrics implementation may
// satisfy to record how many series each successful upstream query returned.
// It is separate from Metrics for the same reason RouterMetrics is: Metrics is
// exported and an embedder may implement it, so widening it would break every
// such implementation. Client type-asserts for it and no-ops when it is absent.
//
// The observation is the one number an operator needs to see a leg approaching
// an upstream series limit (VictoriaMetrics' -search.maxUniqueTimeseries) before
// that limit starts rejecting the query outright.
type SeriesMetrics interface {
	// ObserveQuerySeries records the series count one successful query
	// returned, labelled by query name.
	ObserveQuerySeries(name string, n int)
}

// seriesMetricsOf returns m as a SeriesMetrics when it satisfies the optional
// upgrade, and a no-op otherwise. A nil Metrics yields the no-op.
func seriesMetricsOf(m Metrics) SeriesMetrics {
	if sm, ok := m.(SeriesMetrics); ok && sm != nil {
		return sm
	}
	return noopSeriesMetrics{}
}

type noopSeriesMetrics struct{}

func (noopSeriesMetrics) ObserveQuerySeries(string, int) {}
