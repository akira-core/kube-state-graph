package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	authmocks "github.com/akira-core/kube-state-graph/internal/auth/mocks"
	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/internal/observability"
	"github.com/akira-core/kube-state-graph/pkg/build"
	clockmocks "github.com/akira-core/kube-state-graph/pkg/clock/mocks"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// TestHandleReadyz_UsesInjectedClock proves the readiness handler queries
// upstream at the injected Clock's Now(), not at wall-clock time. Demonstrates
// the mockery-generated MockQuerier + MockClock + MockValidator working
// together against the production handler with no httptest fixtures.
func TestHandleReadyz_UsesInjectedClock(t *testing.T) {
	pinned := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	clk := clockmocks.NewMockClock(t)
	clk.EXPECT().Now().Return(pinned).Once()

	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(
			mock.Anything,
			string(promql.QUpProbe),
			mock.AnythingOfType("string"),
			pinned,
		).
		Return(model.Vector{&model.Sample{Metric: model.Metric{"job": "vm"}, Value: 1}}, nil).
		Once()

	keys := authmocks.NewMockValidator(t)
	keys.EXPECT().Empty().Return(true).Maybe()

	cfg := config.Defaults()
	cfg.PromURL = "http://unused"
	require.NoError(t, cfg.Validate())

	logger := observability.NewLogger("error")
	metrics := observability.NewMetrics()
	builder := build.New(q, build.Options{APITimeout: cfg.APITimeout}, metrics, clk)
	srv := New(cfg, builder, q, metrics, logger, keys, clk)

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	resp, err := http.Get(httpSrv.URL + "/readyz") //nolint:noctx,gosec // test server URL
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(body))
}
