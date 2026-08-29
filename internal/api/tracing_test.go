package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/akira-core/kube-state-graph/internal/auth"
	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/internal/observability"
	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/clock"
)

// installInMemoryTracer registers an in-memory exporter so tests can inspect
// emitted spans without contacting an OTLP collector. Also installs the W3C
// TraceContext propagator so otelgin extracts inbound traceparent headers.
func installInMemoryTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prev := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
		otel.SetTextMapPropagator(prevProp)
	})
	return exporter
}

// waitForSpans returns the exporter's spans once at least one has arrived.
//
// Reading the exporter straight after the response is a race the test can lose:
// otelgin ends the server span in a DEFERRED call, after the handler returns,
// while net/http flushes a response larger than its 4 KB write buffer to the
// client mid-handler. /v1/edge-types is well past that, so the client can have
// the whole body in hand before the span exists. Only the positive assertions
// need this — a test asserting NO span has nothing to wait for.
func waitForSpans(t *testing.T, exporter *tracetest.InMemoryExporter, msg string) tracetest.SpanStubs {
	t.Helper()
	var spans tracetest.SpanStubs
	require.Eventually(t, func() bool {
		spans = exporter.GetSpans()
		return len(spans) > 0
	}, 5*time.Second, 5*time.Millisecond, msg)
	return spans
}

func TestTracing_LivezProbeEmitsNoSpan(t *testing.T) {
	exporter := installInMemoryTracer(t)
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/livez")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, exporter.GetSpans(), "/livez must not generate spans")
}

func TestTracing_MetricsScrapeEmitsNoSpan(t *testing.T) {
	exporter := installInMemoryTracer(t)
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, exporter.GetSpans(), "/metrics must not generate spans")
}

func TestTracing_EdgeTypesEmitsServerSpan(t *testing.T) {
	exporter := installInMemoryTracer(t)
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/edge-types")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	spans := waitForSpans(t, exporter, "/v1/edge-types must emit at least one server span")

	var found bool
	for _, span := range spans {
		if span.Name == "GET /v1/edge-types" {
			found = true
		}
	}
	assert.True(t, found, "expected server span named GET /v1/edge-types")
}

// TestTracing_InboundTraceparentBecomesParent asserts otelgin extracts the
// W3C traceparent header so the server span chains under the caller's trace.
func TestTracing_InboundTraceparentBecomesParent(t *testing.T) {
	exporter := installInMemoryTracer(t)
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	const wantTraceID = "0af7651916cd43dd8448eb211c80319c"
	const wantParent = "b7ad6b7169203331"
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/edge-types", nil)
	require.NoError(t, err)
	req.Header.Set("traceparent", "00-"+wantTraceID+"-"+wantParent+"-01")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	spans := waitForSpans(t, exporter, "/v1/edge-types must emit a server span")
	var serverSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "GET /v1/edge-types" {
			serverSpan = &spans[i]
			break
		}
	}
	require.NotNil(t, serverSpan, "server span missing")
	assert.Equal(t, wantTraceID, serverSpan.SpanContext.TraceID().String())
	assert.Equal(t, wantParent, serverSpan.Parent.SpanID().String())
}

// TestTracing_FailedGraphRecordsErrorSpan asserts a failed /v1/graph build
// stamps Error status on the server span via mapBuildError.
func TestTracing_FailedGraphRecordsErrorSpan(t *testing.T) {
	exporter := installInMemoryTracer(t)
	// Querier mock surfaces an upstream error so build → 502.
	s := newServerWithMocks(t, newErrQuerier(t, errors.New("upstream 500: boom")), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)

	spans := waitForSpans(t, exporter, "/v1/graph must emit a server span")
	var serverSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "GET /v1/graph" {
			serverSpan = &spans[i]
			break
		}
	}
	require.NotNil(t, serverSpan, "server span missing")
	assert.Equal(t, codes.Error, serverSpan.Status.Code,
		"server span must be marked Error when build returns 502")
	// Description from mapBuildError ("upstream") may be overwritten by
	// otelgin's post-handler status hook. The span events list still carries
	// the original error via span.RecordError — assert that's present.
	var sawErrorEvent bool
	for _, ev := range serverSpan.Events {
		if ev.Name == "exception" {
			sawErrorEvent = true
			break
		}
	}
	assert.True(t, sawErrorEvent, "server span must carry an exception event recording the build error")
}

// TestTracing_OutsideRetention400_LeavesServerSpanUnset guards F17: a 400
// outside_retention response is a client-classifiable condition and MUST NOT
// stamp the server span Error — only 5xx-class failures do. Pairs with the 502
// case above which DOES mark Error.
func TestTracing_OutsideRetention400_LeavesServerSpanUnset(t *testing.T) {
	exporter := installInMemoryTracer(t)
	// Empty topology + a healthy up{} probe → outside_retention (400).
	s := newServerWithMocks(t, newMockQuerier(t, fixtureSet{"up": vec(map[string]string{"job": "vm"})}), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	spans := waitForSpans(t, exporter, "/v1/graph must emit a server span")
	var serverSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "GET /v1/graph" {
			serverSpan = &spans[i]
			break
		}
	}
	require.NotNil(t, serverSpan, "server span missing")
	assert.NotEqual(t, codes.Error, serverSpan.Status.Code,
		"a 400 outside_retention must NOT mark the server span Error")
}

// TestAuth_NoAPIKeyInLogs asserts authentication failure does not leak the
// presented X-API-Key value into log output.
func TestAuth_NoAPIKeyInLogs(t *testing.T) {
	const sentinelKey = "SENTINEL-NEVER-LOG-ME-1234"

	cfg := config.Defaults()
	cfg.PromURL = "http://unused"
	require.NoError(t, cfg.Validate())

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)

	q := newMockQuerier(t, nil)
	metrics := observability.NewMetrics()
	ks := auth.NewKeySet()
	ks.LoadCSV("real-key-1,real-key-2")
	builder := build.New(q, build.Options{APITimeout: cfg.APITimeout}, metrics, clock.System{})
	server := New(cfg, builder, q, metrics, logger, ks, clock.System{})

	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/edge-types", nil)
	require.NoError(t, err)
	req.Header.Set(APIKeyHeader, sentinelKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	out := buf.String()
	assert.NotContains(t, out, sentinelKey, "log output must never contain the presented API key value")
	// Sanity: a log line did get emitted (so the assertion above is meaningful).
	assert.Contains(t, out, "\"http\"", "expected an http log line: %s", out)
}

// TestTracing_BodyStableAcrossTracingState asserts enabling tracing does not
// change the response body — resource attributes and span IDs must NOT leak
// into the JSON output.
func TestTracing_BodyStableAcrossTracingState(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	url := srv.URL + "/v1/edge-types"

	// First fetch with no tracer overrides (default is whatever the test
	// process has installed — typically the noop tracer).
	r1, err := http.Get(url)
	require.NoError(t, err)
	body1, err := io.ReadAll(r1.Body)
	r1.Body.Close()
	require.NoError(t, err)

	// Install a recording tracer and refetch.
	prevTP := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
	})

	r2, err := http.Get(url)
	require.NoError(t, err)
	body2, err := io.ReadAll(r2.Body)
	r2.Body.Close()
	require.NoError(t, err)

	assert.Equal(t, body1, body2, "response body must be byte-identical regardless of tracing state")
}
