package observability

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the kube_state_graph_* Prometheus metrics.
type Metrics struct {
	Registry *prometheus.Registry

	BuildDuration     prometheus.Histogram
	ProjectDuration   prometheus.Histogram
	SerialiseDuration *prometheus.HistogramVec
	BuildRejected     *prometheus.CounterVec
	GraphNodeCount    *prometheus.GaugeVec
	GraphEdgeCount    *prometheus.GaugeVec
	ClustersObserved  prometheus.Gauge
	UpstreamQueryDur  *prometheus.HistogramVec
	UpstreamQueryFail *prometheus.CounterVec
	HTTPRequests      *prometheus.CounterVec
	AuthRejected      *prometheus.CounterVec

	// Upstream backend routing. These are SEPARATE metrics rather than a
	// `backend` label on UpstreamQueryDur / UpstreamQueryFail: adding a label
	// to an established self-metric is a contract change that breaks every
	// dashboard and recording rule built on it.
	UpstreamBackends   prometheus.Gauge
	BackendReload      *prometheus.CounterVec
	BackendQueryFailed *prometheus.CounterVec
}

// NewMetrics registers and returns a fresh Metrics bundle.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		BuildDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kube_state_graph_build_duration_seconds",
			Help:    "Time to build a multi-cluster graph snapshot.",
			Buckets: prometheus.DefBuckets,
		}),
		ProjectDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kube_state_graph_project_duration_seconds",
			Help:    "Time spent applying filters and traversal over a built graph.",
			Buckets: []float64{0.0001, 0.001, 0.01, 0.1, 1},
		}),
		SerialiseDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kube_state_graph_serialise_duration_seconds",
			Help:    "Time spent encoding the response.",
			Buckets: []float64{0.0001, 0.001, 0.01, 0.1, 1},
		}, []string{"format"}),
		BuildRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_state_graph_build_rejected_total",
			Help: "Builds rejected by reason.",
		}, []string{"reason"}),
		GraphNodeCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kube_state_graph_graph_node_count",
			Help: "Node count of the most recently built graph (per cluster, per kind).",
		}, []string{"cluster", "kind"}),
		GraphEdgeCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kube_state_graph_graph_edge_count",
			Help: "Edge count of the most recently built graph (per type, per cross_cluster).",
		}, []string{"type", "cross_cluster"}),
		ClustersObserved: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kube_state_graph_clusters_observed",
			Help: "Number of distinct cluster label values observed in the most recent build.",
		}),
		UpstreamQueryDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kube_state_graph_upstream_query_duration_seconds",
			Help:    "Upstream PromQL query duration.",
			Buckets: prometheus.DefBuckets,
		}, []string{"query"}),
		UpstreamQueryFail: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_state_graph_upstream_query_failures_total",
			Help: "Upstream PromQL query failures by query name.",
		}, []string{"query"}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_state_graph_http_requests_total",
			Help: "HTTP requests by path and status.",
		}, []string{"path", "status"}),
		AuthRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_state_graph_auth_rejected_total",
			Help: "API-key authentication rejections by reason (missing | invalid).",
		}, []string{"reason"}),
		UpstreamBackends: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kube_state_graph_upstream_backends",
			Help: "Number of upstream backends in the live routing table.",
		}),
		BackendReload: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_state_graph_backend_config_reload_total",
			Help: "Routing-table reload attempts by result (ok | error | unchanged).",
		}, []string{"result"}),
		BackendQueryFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_state_graph_backend_query_failures_total",
			Help: "Upstream PromQL query failures by backend.",
		}, []string{"backend"}),
	}

	reg.MustRegister(
		m.BuildDuration,
		m.ProjectDuration,
		m.SerialiseDuration,
		m.BuildRejected,
		m.GraphNodeCount,
		m.GraphEdgeCount,
		m.ClustersObserved,
		m.UpstreamQueryDur,
		m.UpstreamQueryFail,
		m.HTTPRequests,
		m.AuthRejected,
		m.UpstreamBackends,
		m.BackendReload,
		m.BackendQueryFailed,
	)
	// The reload results are a closed set, so every one is materialised at
	// zero: a reload counter that appears only after the first failure gives
	// an alert nothing to compare against.
	for _, result := range []string{"ok", "error", "unchanged"} {
		m.BackendReload.WithLabelValues(result)
	}
	return m
}
