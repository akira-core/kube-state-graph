package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
)

// errReason decodes the {error:{reason,message}} envelope.
type errReason struct {
	Error struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestReadyz_Healthy200 (F6): a successful up{} probe returns 200 "ok".
func TestReadyz_Healthy200(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, fixtureSet{"up": vec(map[string]string{"job": "vm"})}), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "ok", string(body))
}

// TestReadyz_UpstreamUnreachable503 (F6 + F14): an upstream probe failure
// returns 503 upstream_unreachable, and the raw error (carrying the internal VM
// host/IP) must NOT leak into the unauthenticated response body.
func TestReadyz_UpstreamUnreachable503(t *testing.T) {
	s := newServerWithMocks(t, newErrQuerier(t, errors.New("dial tcp 10.9.8.7:8428: connection refused")), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	var body errReason
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "upstream_unreachable", body.Error.Reason)
	assert.Equal(t, "upstream probe failed", body.Error.Message)
	assert.NotContains(t, body.Error.Message, "10.9.8.7", "internal upstream host must not leak")
}

// TestReadyz_Timeout503 (F6): a stalled upstream probe times out under
// --api-timeout and returns 503.
func TestReadyz_Timeout503(t *testing.T) {
	s := newServerWithMocks(t, newStallQuerier(t), func(c *config.Config) { c.APITimeout = 20 * time.Millisecond })
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// TestGraph_BuildTimeout504 (F7): a build that exceeds --build-timeout returns
// 504 reason:"timeout" and increments the BuildRejected{timeout} metric.
func TestGraph_BuildTimeout504(t *testing.T) {
	s := newServerWithMocks(t, newStallQuerier(t), func(c *config.Config) { c.BuildTimeout = 20 * time.Millisecond })
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)

	var body errReason
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "timeout", body.Error.Reason)

	mresp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer mresp.Body.Close()
	mbody, _ := io.ReadAll(mresp.Body)
	assert.Contains(t, string(mbody), `kube_state_graph_build_rejected_total{reason="timeout"}`,
		"a build timeout must increment BuildRejected{timeout}")
}
