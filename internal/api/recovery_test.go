package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/internal/auth"
	"github.com/marz32one/kube-state-graph/internal/config"
	"github.com/marz32one/kube-state-graph/internal/observability"
	"github.com/marz32one/kube-state-graph/pkg/build"
	"github.com/marz32one/kube-state-graph/pkg/clock"
	promqlmocks "github.com/marz32one/kube-state-graph/pkg/promql/mocks"
)

// newPanicQuerier returns a Querier mock whose every Instant call panics with
// the given value. /v1/clusters calls Instant on the handler goroutine, so the
// panic propagates straight up the gin middleware chain (unlike /v1/graph,
// whose fan-out runs Instant on errgroup goroutines).
func newPanicQuerier(t *testing.T, v any) *promqlmocks.MockQuerier {
	t.Helper()
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _ time.Time) (model.Vector, error) {
			panic(v)
		}).
		Maybe()
	return q
}

// syncLogBuffer is a goroutine-safe log sink: the server goroutine may still
// be writing (e.g. unwinding after an aborted connection) when the test
// goroutine reads.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncLogBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// newServerWithLogBuffer builds a Server whose slog output is captured in the
// returned buffer, so tests can assert on the access log and recovery log.
func newServerWithLogBuffer(t *testing.T, q *promqlmocks.MockQuerier) (*Server, *syncLogBuffer) {
	t.Helper()
	cfg := config.Defaults()
	cfg.PromURL = "http://unused"
	require.NoError(t, cfg.Validate())

	buf := &syncLogBuffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	metrics := observability.NewMetrics()
	builder := build.New(q, build.Options{MetricPrefix: cfg.MetricPrefix, APITimeout: cfg.APITimeout}, metrics, clock.System{})
	return New(cfg, builder, q, metrics, logger, auth.NewKeySet(), clock.System{}), buf
}

// TestPanicRecovery_Returns500Envelope asserts a handler panic does NOT reset
// the TCP connection: the client receives the standard 500 JSON envelope with
// reason "internal" and a static message that leaks no panic detail.
func TestPanicRecovery_Returns500Envelope(t *testing.T) {
	const panicSentinel = "PANIC-SENTINEL-do-not-leak"
	s, logs := newServerWithLogBuffer(t, newPanicQuerier(t, panicSentinel))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// Without recovery middleware this Get fails with a connection reset (EOF).
	resp, err := http.Get(srv.URL + "/v1/clusters")
	require.NoError(t, err, "a handler panic must not reset the connection")
	defer resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	var body errReason
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "internal", body.Error.Reason)
	assert.Equal(t, "internal error", body.Error.Message)
	assert.NotContains(t, body.Error.Message, panicSentinel, "panic value must not leak into the response body")

	// The panic itself must be logged server-side, with a stack trace.
	out := logs.String()
	assert.Contains(t, out, panicSentinel, "recovery log must carry the panic value")
	assert.Contains(t, out, "goroutine", "recovery log must carry the stack trace")
}

// TestPanicRecovery_ErrAbortHandlerPassesThrough asserts the middleware
// re-panics net/http's silent-abort sentinel instead of converting it into an
// ERROR log + 500: the connection is torn down quietly by conn.serve (the
// client sees a transport error, not a JSON envelope) and no "panic recovered"
// line is emitted.
func TestPanicRecovery_ErrAbortHandlerPassesThrough(t *testing.T) {
	s, logs := newServerWithLogBuffer(t, newPanicQuerier(t, http.ErrAbortHandler))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/clusters") //nolint:bodyclose // the request must fail without a response
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected an aborted connection, got HTTP %d", resp.StatusCode)
	}

	// The client error can return while the server goroutine is still
	// unwinding; the deferred access log is the last write of that unwind, so
	// once it appears the recovery log (which would precede it) is settled.
	require.Eventually(t, func() bool {
		return bytes.Contains(logs.Bytes(), []byte(`"msg":"http"`))
	}, 2*time.Second, 10*time.Millisecond, "access log line never appeared")
	assert.NotContains(t, logs.String(), "panic recovered",
		"a deliberate silent abort must not be logged as a recovered panic")
}

// TestPanicRecovery_AccessLogAndMetricStillRecorded asserts the access log line
// and the kube_state_graph_http_requests_total metric still record the 500 —
// the recovery middleware sits inside requestID + logging, and the logging
// bookkeeping is deferred, so neither is skipped by a propagating panic.
func TestPanicRecovery_AccessLogAndMetricStillRecorded(t *testing.T) {
	s, logs := newServerWithLogBuffer(t, newPanicQuerier(t, "boom"))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/clusters")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// Access log line with the 500 status.
	type logLine struct {
		Msg    string `json:"msg"`
		Path   string `json:"path"`
		Status int    `json:"status"`
	}
	var sawAccessLog bool
	for _, raw := range bytes.Split(logs.Bytes(), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var line logLine
		if json.Unmarshal(raw, &line) != nil {
			continue
		}
		if line.Msg == "http" && line.Path == "/v1/clusters" && line.Status == http.StatusInternalServerError {
			sawAccessLog = true
		}
	}
	assert.True(t, sawAccessLog, "access log must record the 500 despite the panic: %s", logs.String())

	// HTTP requests metric with the 5xx class.
	mresp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer mresp.Body.Close()
	mbody, _ := io.ReadAll(mresp.Body)
	assert.Contains(t, string(mbody),
		`kube_state_graph_http_requests_total{path="/v1/clusters",status="5xx"}`,
		"HTTP metric must record the 500 despite the panic")
}
