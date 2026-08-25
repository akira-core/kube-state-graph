package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/auth"
	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/internal/observability"
	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/clock"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// probeFake answers or refuses, standing in for one upstream installation.
type probeFake struct{ err error }

func (p probeFake) Instant(context.Context, string, string, time.Time) (model.Vector, error) {
	if p.err != nil {
		return nil, p.err
	}
	return model.Vector{&model.Sample{Metric: model.Metric{"__name__": "up"}, Value: 1}}, nil
}

func routedTable(t *testing.T, names ...string) *promql.Table {
	t.Helper()
	bs := make([]promql.Backend, 0, len(names))
	for i, n := range names {
		bs = append(bs, promql.NewBackend(n, "http://vm-"+n+":8428", promql.Families,
			[]string{"zone-" + string(rune('a'+i))}, "", ""))
	}
	tbl, err := promql.NewTable(bs)
	require.NoError(t, err)
	return tbl
}

// newRoutedServer builds a Server whose upstream is a Router over the supplied
// per-backend fakes — the production wiring, with the clients faked.
func newRoutedServer(t *testing.T, fakes map[string]probeFake, names ...string) *Server {
	t.Helper()
	r, err := promql.NewRouter(routedTable(t, names...), nil, func(b promql.Backend) (promql.Querier, error) {
		f, ok := fakes[b.Name()]
		require.True(t, ok, "no fake for backend %q", b.Name())
		return f, nil
	})
	require.NoError(t, err)

	cfg := config.Defaults()
	cfg.PromURL = "http://unused"
	require.NoError(t, cfg.Validate())

	logger := observability.NewLogger("error")
	metrics := observability.NewMetrics()
	builder := build.New(r, build.Options{APITimeout: cfg.APITimeout}, metrics, clock.System{})
	return New(cfg, builder, r, metrics, logger, auth.NewKeySet(), clock.System{})
}

func getReadyz(t *testing.T, s *Server) (int, string) {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/readyz") //nolint:noctx // test server URL
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

func TestReadyz_AllBackendsAnswer(t *testing.T) {
	s := newRoutedServer(t, map[string]probeFake{
		"zone-a": {}, "zone-b": {}, "zone-c": {},
	}, "zone-a", "zone-b", "zone-c")

	code, body := getReadyz(t, s)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "ok", body)
}

// A single unreachable backend makes the server not-ready, and the body names
// it: with several upstreams, "upstream unreachable" is not actionable.
func TestReadyz_NamesTheFailingBackend(t *testing.T) {
	s := newRoutedServer(t, map[string]probeFake{
		"zone-a": {}, "zone-b": {err: errors.New("connection refused")}, "zone-c": {},
	}, "zone-a", "zone-b", "zone-c")

	code, body := getReadyz(t, s)
	require.Equal(t, http.StatusServiceUnavailable, code)

	var parsed struct {
		Error struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	assert.Equal(t, "upstream_unreachable", parsed.Error.Reason)
	assert.Contains(t, parsed.Error.Message, "zone-b")
	assert.NotContains(t, parsed.Error.Message, "zone-a")
	assert.NotContains(t, parsed.Error.Message, "zone-c")
}

// The probe must not stop at the first failure: naming one of three down
// backends would send the operator chasing a single store.
func TestReadyz_NamesEveryFailingBackend(t *testing.T) {
	s := newRoutedServer(t, map[string]probeFake{
		"zone-a": {err: errors.New("refused")},
		"zone-b": {},
		"zone-c": {err: errors.New("refused")},
	}, "zone-a", "zone-b", "zone-c")

	code, body := getReadyz(t, s)
	require.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, "zone-a")
	assert.Contains(t, body, "zone-c")
}

// /readyz is unauthenticated, so the body must never disclose the internal
// upstream topology — only the operator-chosen backend names.
func TestReadyz_BodyNeverLeaksUpstreamEndpoints(t *testing.T) {
	s := newRoutedServer(t, map[string]probeFake{
		"zone-a": {}, "zone-b": {err: errors.New("dial tcp 10.1.2.3:8428: connection refused")},
	}, "zone-a", "zone-b")

	_, body := getReadyz(t, s)
	assert.NotContains(t, body, "10.1.2.3")
	assert.NotContains(t, body, "vm-zone-b")
	assert.NotContains(t, body, "8428")
	assert.Contains(t, body, "zone-b")
}

// An unrouted deployment produces exactly the response it produced before
// backend routing existed: a plain mock Querier does not satisfy
// promql.Prober, so the single-query fallback runs.
func TestReadyz_UnroutedBodyUnchanged(t *testing.T) {
	s := newServerWithMocks(t, newErrQuerier(t, errors.New("connection refused")), nil)
	code, body := getReadyz(t, s)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.JSONEq(t,
		`{"apiVersion":"v1","error":{"reason":"upstream_unreachable","message":"upstream probe failed"}}`,
		body)
}

// The probes share one budget rather than consuming it in series, so
// readiness latency does not grow with the number of backends.
func TestReadyz_ProbesRunConcurrently(t *testing.T) {
	const backends = 6
	names := make([]string, backends)
	fakes := map[string]probeFake{}
	for i := range names {
		names[i] = string(rune('a'+i)) + "-backend"
		fakes[names[i]] = probeFake{}
	}

	r, err := promql.NewRouter(routedTable(t, names...), nil, func(b promql.Backend) (promql.Querier, error) {
		return slowProbe{delay: 80 * time.Millisecond}, nil
	})
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, r.ProbeAll(context.Background(), time.Unix(0, 0)))
	elapsed := time.Since(start)

	assert.Less(t, elapsed, backends*80*time.Millisecond/2,
		"probes must run concurrently, not in series (took %s for %d backends)", elapsed, backends)
}

type slowProbe struct{ delay time.Duration }

func (s slowProbe) Instant(ctx context.Context, _, _ string, _ time.Time) (model.Vector, error) {
	select {
	case <-time.After(s.delay):
		return model.Vector{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
