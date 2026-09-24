package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// instantRecorder returns a mock that answers empty and records every
// evaluation instant it was asked for, the up{} probe excepted.
func instantRecorder(t *testing.T) (*promqlmocks.MockQuerier, func() map[time.Time]bool) {
	t.Helper()
	var mu sync.Mutex
	seen := map[time.Time]bool{}
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name, _ string, ts time.Time) (model.Vector, error) {
			if name == string(promql.QUpProbe) {
				// The retention probe evaluates at the wall clock, not at end.
				return model.Vector{}, nil
			}
			mu.Lock()
			seen[ts.UTC()] = true
			mu.Unlock()
			return model.Vector{}, nil
		}).Maybe()
	return q, func() map[time.Time]bool {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

func TestEndAlign_AppliedOnBothGraphEndpoints(t *testing.T) {
	paths := map[string]string{
		"graph":         "/v1/graph?start=2026-05-02T12:04:17Z&end=2026-05-02T12:19:47Z",
		"storage-graph": "/v1/storage-graph?start=2026-05-02T12:04:17Z&end=2026-05-02T12:19:47Z&az=zone-a&env=prod",
	}
	cases := []struct {
		name  string
		align time.Duration
		want  string
	}{
		{"default grid", -1, "2026-05-02T12:19:30Z"}, // -1: keep config.Defaults()
		{"disabled", 0, "2026-05-02T12:19:47Z"},
	}
	for _, tc := range cases {
		for ep, path := range paths {
			t.Run(tc.name+"/"+ep, func(t *testing.T) {
				q, seen := instantRecorder(t)
				s := newServerWithMocks(t, q, func(c *config.Config) {
					if tc.align >= 0 {
						c.EndAlign = tc.align
					}
				})
				srv := httptest.NewServer(s.Handler())
				t.Cleanup(srv.Close)
				resp, err := http.Get(srv.URL + path) //nolint:noctx // test server URL
				require.NoError(t, err)
				resp.Body.Close()
				require.Equal(t, http.StatusOK, resp.StatusCode)

				want, _ := time.Parse(time.RFC3339, tc.want)
				assert.Equal(t, map[time.Time]bool{want: true}, seen())
			})
		}
	}
}

// Validation runs on the caller's values: a 10s window ending inside one grid
// step is valid and is evaluated, not rejected as an empty range.
func TestEndAlign_ValidationUsesCallerValues(t *testing.T) {
	q, seen := instantRecorder(t)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=2026-05-02T12:19:40Z&end=2026-05-02T12:19:50Z") //nolint:noctx // test server URL
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	want, _ := time.Parse(time.RFC3339, "2026-05-02T12:19:30Z")
	assert.Equal(t, map[time.Time]bool{want: true}, seen())

	resp, err = http.Get(srv.URL + "/v1/graph?start=2026-05-02T12:19:50Z&end=2026-05-02T12:19:40Z") //nolint:noctx // test server URL
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "end before start still rejected")
}
