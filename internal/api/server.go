package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/marz32one/kube-state-graph/internal/auth"
	"github.com/marz32one/kube-state-graph/internal/config"
	"github.com/marz32one/kube-state-graph/internal/observability"
	"github.com/marz32one/kube-state-graph/internal/telemetry"
	"github.com/marz32one/kube-state-graph/pkg/build"
	"github.com/marz32one/kube-state-graph/pkg/clock"
	"github.com/marz32one/kube-state-graph/pkg/promql"

	"log/slog"
)

// APIVersion is the version stamped into every JSON response and route prefix.
const APIVersion = "v1"

// Server bundles the dependencies needed by the HTTP handlers.
type Server struct {
	cfg     config.Config
	builder *build.Builder
	prom    promql.Querier
	r       promql.Renderer
	metrics *observability.Metrics
	logger  *slog.Logger
	keys    auth.Validator
	clk     clock.Clock
}

// New wires up a Server. keys may be nil to run with API-key authentication
// disabled. clk may be nil; nil falls back to clock.System. The Renderer is
// derived from cfg.MetricPrefix so the cluster-discovery + readiness
// (`up{}`) queries the Server issues on its own (independent of the build
// pipeline) honour the configured upstream metric-name prefix
// (see design.md D26).
func New(cfg config.Config, builder *build.Builder, prom promql.Querier, m *observability.Metrics, logger *slog.Logger, keys auth.Validator, clk clock.Clock) *Server {
	if clk == nil {
		clk = clock.System{}
	}
	if keys == nil {
		keys = auth.NewKeySet()
	}
	return &Server{
		cfg:     cfg,
		builder: builder,
		prom:    prom,
		r:       promql.Renderer{Prefix: cfg.MetricPrefix},
		metrics: m,
		logger:  logger,
		keys:    keys,
		clk:     clk,
	}
}

// Handler returns the Gin engine fully configured with routes + middleware.
//
// otelgin is installed on the /v1/* route group only so kubelet probes,
// Prometheus scrapes, and documentation requests do not generate spans.
// Inbound W3C traceparent is honoured via the global propagator.
func (s *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// Order matters: recovery is innermost so a recovered panic's 500 is still
	// observed by loggingMiddleware's deferred access-log + metric bookkeeping.
	r.Use(s.requestIDMiddleware(), s.loggingMiddleware(), s.recoveryMiddleware())

	v1 := r.Group("/" + APIVersion)
	v1.Use(
		otelgin.Middleware(telemetry.ServiceName),
		s.apiKeyMiddleware(),
		s.spanEnrichMiddleware(),
	)
	v1.GET("/graph", s.handleGraph)
	v1.GET("/clusters", s.handleClusters)
	v1.GET("/edge-types", s.handleEdgeTypes)

	r.GET("/livez", s.handleLivez)
	r.GET("/readyz", s.handleReadyz)
	r.GET("/metrics", gin.WrapH(promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{})))

	r.GET("/openapi.yaml", s.handleOpenAPIYAML)
	r.GET("/openapi.json", s.handleOpenAPIJSON)
	r.GET("/docs", s.handleDocs)

	r.NoRoute(s.handleNotFound)
	r.NoMethod(s.handleNotFound)

	return r
}

// handleNotFound answers any unmatched route/method with the standard error
// body. Its access-log / metric `path` label is bucketed under unmatchedPath by
// loggingMiddleware, so an unauthenticated caller cannot inflate metric series
// cardinality by spraying arbitrary URLs.
func (s *Server) handleNotFound(c *gin.Context) {
	writeError(c, http.StatusNotFound, "not_found", "no such route")
}

// requestIDMiddleware injects a unique X-Request-ID into context and response.
func (s *Server) requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

// quietLogPaths are silent on success: kubelet probes and Prometheus scrape
// fire every few seconds and would otherwise dominate the access log. Any
// status >=400 is still emitted so genuine failures remain visible.
var quietLogPaths = map[string]struct{}{
	"/livez":   {},
	"/readyz":  {},
	"/metrics": {},
}

// unmatchedPath is the bounded label used for any request that matches no
// registered route. gin runs global middleware before its NoRoute handler, so a
// 404's c.FullPath() is "" and the raw, attacker-controlled URL path would
// otherwise become an unbounded `path` metric label (a series-cardinality DoS).
// Bucketing unmatched requests under a fixed sentinel keeps cardinality bounded
// by the registered route set.
const unmatchedPath = "<unmatched>"

// loggingMiddleware emits one slog line per request. The post-Next bookkeeping
// runs in a defer so a panic propagating from inner middleware (recovery is
// innermost and normally converts handler panics to a 500, but a panic raised
// by middleware between logging and recovery would bypass it) can never
// silently skip the access log and the HTTP-requests metric.
func (s *Server) loggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			status := c.Writer.Status()
			path := c.FullPath()
			if path == "" {
				path = unmatchedPath
			}
			// Count every request exactly once, before the quiet-path branch —
			// one Inc site, not a copy per branch.
			s.metrics.HTTPRequests.WithLabelValues(path, statusClass(status)).Inc()
			if _, quiet := quietLogPaths[path]; quiet && status < 400 {
				return
			}
			s.logger.InfoContext(c.Request.Context(), "http",
				"method", c.Request.Method,
				"path", path,
				"status", status,
				"duration_ms", duration.Milliseconds(),
				"request_id", c.GetString("request_id"),
				"clusters", c.Request.URL.Query()["cluster"],
			)
		}()
		c.Next()
	}
}

func statusClass(s int) string {
	switch {
	case s >= 200 && s < 300:
		return "2xx"
	case s >= 300 && s < 400:
		return "3xx"
	case s >= 400 && s < 500:
		return "4xx"
	case s >= 500 && s < 600:
		return "5xx"
	default:
		return "other"
	}
}
