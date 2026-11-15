package promql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	promapi "github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
)

// tracer is obtained from the global provider; it is a no-op until an
// application installs an OpenTelemetry SDK. The instrumentation scope name is
// kept stable ("kube-state-graph") so span dimensions are unchanged.
var tracer = otel.Tracer("kube-state-graph")

// Client wraps the Prometheus HTTP API and emits self-metrics for every call.
type Client struct {
	api     v1.API
	metrics Metrics
	base    *http.Transport
	// backend names the routing-table entry this client serves. Empty in a
	// single-upstream deployment. It is a span attribute only — the existing
	// kube_state_graph_upstream_query_* metrics deliberately keep their label
	// sets, so per-backend detail never lands on them.
	backend string
}

// Option configures optional Client behaviour at construction time.
type Option func(*clientOptions)

type clientOptions struct {
	username string
	password string
	backend  string
	// maxResponseBytes is WithMaxResponseBytes' value (0 = unbounded), used
	// only when maxResponseBytesSet; otherwise DefaultMaxResponseBytes.
	maxResponseBytes    int64
	maxResponseBytesSet bool
}

// WithBasicAuth attaches HTTP Basic Auth credentials to outbound upstream
// requests addressed to the configured upstream host. Empty username and
// password is a no-op. The credential values are held on the transport only —
// they are never logged, attached to spans, or included in error messages.
func WithBasicAuth(username, password string) Option {
	return func(o *clientOptions) {
		o.username = username
		o.password = password
	}
}

// WithBackendName labels this client with the routing-table backend it serves,
// so every upstream query span can be attributed to the installation that
// answered it. Empty (the default) omits the attribute entirely, which is what
// a single-upstream deployment emits today.
func WithBackendName(name string) Option {
	return func(o *clientOptions) { o.backend = name }
}

// basicAuthTransport sets an Authorization: Basic header on a clone of each
// request (RoundTrippers must not mutate the caller's request) and delegates
// to the inner transport. The header is added ONLY for requests addressed to
// the configured upstream's scheme AND host: net/http strips Authorization
// when following a cross-host redirect, and stamping per-hop at the transport
// layer would silently re-attach the credentials below that protection — so a
// redirect to a foreign host must pass through unauthenticated. The scheme is
// compared as well because net/http keeps the header on a same-host
// https→http redirect, which would otherwise send the credentials in clear.
type basicAuthTransport struct {
	inner    http.RoundTripper
	scheme   string // only requests on this scheme …
	host     string // … to this host carry credentials
	username string
	password string
}

func (t *basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Scheme, t.scheme) || !strings.EqualFold(req.URL.Host, t.host) {
		return t.inner.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.SetBasicAuth(t.username, t.password)
	return t.inner.RoundTrip(clone)
}

// newTransport assembles the outbound transport chain: otelhttp → basicAuth
// (when credentials are configured) → response-size bound (unless disabled) →
// base. The traced request is the final authenticated one; otelhttp records
// method/URL/status only, never headers. target scopes the credentials to the
// configured upstream's scheme and host (see basicAuthTransport).
func newTransport(o clientOptions, target *url.URL, base http.RoundTripper) http.RoundTripper {
	rt := base
	if bound := o.responseBound(); bound > 0 {
		rt = &responseLimitTransport{inner: rt, max: bound}
	}
	if o.username != "" || o.password != "" {
		rt = &basicAuthTransport{
			inner: rt, scheme: target.Scheme, host: target.Host,
			username: o.username, password: o.password,
		}
	}
	return otelhttp.NewTransport(rt)
}

// New constructs a Client targeting the supplied URL. metrics may be nil
// (no-op). The HTTP transport is wrapped with otelhttp so outbound PromQL
// requests propagate W3C traceparent headers and emit a client span per call.
func New(promURL string, metrics Metrics, opts ...Option) (*Client, error) {
	var o clientOptions
	for _, opt := range opts {
		opt(&o)
	}
	u, err := url.Parse(promURL)
	if err != nil {
		// The cause only: a *url.Error quotes the whole input, userinfo
		// included.
		return nil, fmt.Errorf("prom url is not parseable: %w", parseErrDetail(err))
	}
	// The idle pool is sized to keep one build's scoped second waves warm: a
	// storage build can hold two waves of pkg/build's scopeConcurrency (16)
	// against one store at once. A smaller pool only closes the surplus after
	// each burst for the next build to re-dial; it never caps what is in flight.
	base := &http.Transport{
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     30 * time.Second,
	}
	c, err := promapi.NewClient(promapi.Config{
		Address:      promURL,
		RoundTripper: newTransport(o, u, base),
	})
	if err != nil {
		return nil, fmt.Errorf("prometheus client: %w", err)
	}
	return &Client{api: v1.NewAPI(c), metrics: metrics, base: base, backend: o.backend}, nil
}

// CloseIdleConnections releases the connection pool this client holds. It is
// called when a routing-table reload retires the backend this client served:
// without it every reload would leave a pool behind for an endpoint nothing
// dispatches to any more.
func (c *Client) CloseIdleConnections() {
	if c.base != nil {
		c.base.CloseIdleConnections()
	}
}

// Instant runs an instant PromQL query at ts, recording duration / failure
// metrics labelled with the supplied query name and emitting a `prometheus.query`
// span carrying the rendered statement.
func (c *Client) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	stmt := boundStatement(query)
	attrs := []attribute.KeyValue{
		semconv.DBSystemKey.String("prometheus"),
		attribute.String("db.statement", stmt.text),
		attribute.Int("kube_state_graph.query_bytes", stmt.bytes),
		attribute.String("kube_state_graph.query_sha256", stmt.sha256),
		attribute.String("kube_state_graph.query_name", name),
	}
	if c.backend != "" {
		attrs = append(attrs, attribute.String("kube_state_graph.backend", c.backend))
	}
	if wait := slotWaitFrom(ctx); wait > 0 {
		// Set only when the query queued behind its store's concurrency limit
		// (see guard), so an unqueued query's span is unchanged.
		attrs = append(attrs, attribute.Int64("kube_state_graph.slot_wait_ms", wait.Milliseconds()))
	}
	ctx, span := tracer.Start(ctx, "prometheus.query",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	defer span.End()

	slog.DebugContext(ctx, "promql query",
		"name", name,
		"query", stmt.text,
		"query_bytes", stmt.bytes,
		"query_sha256", stmt.sha256,
		"ts", ts.UTC().Format(time.RFC3339),
	)

	start := time.Now()
	defer func() {
		if c.metrics != nil {
			c.metrics.ObserveQueryDuration(name, time.Since(start).Seconds())
		}
	}()
	var qopts []v1.Option
	deadline, bounded := ctx.Deadline()
	if t, ok := evaluationTimeout(deadline, bounded, time.Now()); ok {
		qopts = append(qopts, v1.WithTimeout(t))
	}
	// The third return is Infos (client_golang v1.25+): advisory notes such as
	// "metric might not be a counter", not data loss. Warnings stay the signal
	// for truncation, so Infos are deliberately dropped.
	val, warns, _, err := c.api.Query(ctx, query, ts, qopts...)
	if len(warns) > 0 {
		// VictoriaMetrics signals truncated / partial responses (e.g. a
		// -search.* limit hit) via the warnings return; dropping them hides
		// silent data truncation. Logged regardless of err so a partial
		// response that still errors keeps both signals. Only the query name
		// and upstream warning text are logged — never credentials or the
		// upstream URL.
		slog.WarnContext(ctx, "upstream query returned warnings",
			"query_name", name,
			"warnings", warns,
		)
	}
	if err != nil {
		if c.metrics != nil {
			c.metrics.IncQueryFailure(name)
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "promql query failed",
			"name", name,
			"query", stmt.text,
			"query_bytes", stmt.bytes,
			"query_sha256", stmt.sha256,
			"ts", ts.UTC().Format(time.RFC3339),
			"err", err,
		)
		return nil, fmt.Errorf("prom query %s: %w", name, err)
	}
	vec, ok := val.(model.Vector)
	if !ok {
		if c.metrics != nil {
			c.metrics.IncQueryFailure(name)
		}
		err := fmt.Errorf("unexpected result type %T", val)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("prom query %s: %w", name, err)
	}
	span.SetAttributes(attribute.Int("kube_state_graph.result_series_count", len(vec)))
	// Recorded beside the span attribute, from the same len(vec), so the two
	// signals cannot disagree. A failed query returned above and contributes
	// no observation.
	seriesMetricsOf(c.metrics).ObserveQuerySeries(name, len(vec))
	slog.DebugContext(ctx, "promql result",
		"name", name,
		"series", len(vec),
		"ts", ts.UTC().Format(time.RFC3339),
	)
	return vec, nil
}

// maxStatementBytes bounds the rendered query a span or log record carries. A
// request matcher is caller-controlled, so an unbounded statement let one cheap
// request write megabytes of span attributes and Error logs — on every leg, and
// on every repetition, since a failed query is never cached.
const maxStatementBytes = 2048

// statement is the bounded form of a rendered query for spans and logs: the
// text truncated to maxStatementBytes, the full length, and a short SHA-256
// prefix, so a truncated statement can still be told apart from another and
// matched against an upstream log line.
type statement struct {
	text   string
	bytes  int
	sha256 string
}

func boundStatement(query string) statement {
	sum := sha256.Sum256([]byte(query))
	return statement{
		text:   truncateUTF8(query, maxStatementBytes),
		bytes:  len(query),
		sha256: hex.EncodeToString(sum[:6]),
	}
}

// truncateUTF8 returns at most n bytes of s, cut on a character boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// evaluationTimeoutMargin is kept back from the caller's remaining time when
// the upstream evaluation timeout is derived from it, so the store gives up
// (and the query's slot frees) before the caller's own deadline fires.
const evaluationTimeoutMargin = 500 * time.Millisecond

// evaluationTimeout derives the upstream API's `timeout` parameter from the
// query context's deadline: the remaining time less evaluationTimeoutMargin,
// truncated to whole seconds. ok is false when there is no deadline or less
// than one whole second would remain; the context deadline alone then bounds
// the query.
//
// Whole seconds because the parameter is sent as time.Duration.String() and
// Prometheus' duration parser rejects a fractional form such as "14.75s";
// "14s" and "2m0s" parse in Prometheus and VictoriaMetrics alike. Without it a
// store keeps evaluating a query the build has already abandoned, holding its
// concurrency slot until the store's own -search.maxQueryDuration.
func evaluationTimeout(deadline time.Time, bounded bool, now time.Time) (time.Duration, bool) {
	if !bounded {
		return 0, false
	}
	t := (deadline.Sub(now) - evaluationTimeoutMargin).Truncate(time.Second)
	if t < time.Second {
		return 0, false
	}
	return t, true
}

// FormatDuration renders d as a PromQL duration literal (e.g., 90s, 5m, 3h).
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	switch {
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	default:
		// Fall back to seconds, truncated. A positive sub-second window would
		// truncate to 0 and render a zero-width range selector (`...[0s]`),
		// which is at best a no-op and at worst rejected by PromQL/MetricsQL.
		// Floor any positive duration to 1s so a valid (end > start) window
		// never produces a degenerate `[0s]` selector.
		secs := int64(d.Seconds())
		if secs < 1 {
			secs = 1
		}
		return strconv.FormatInt(secs, 10) + "s"
	}
}
