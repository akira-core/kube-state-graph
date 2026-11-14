package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDebugLastQueries_RouteNotRegistered confirms removal of the debug route.
func TestDebugLastQueries_RouteNotRegistered(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := httpGet(t, srv.URL+"/debug/last-queries")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestGraphEndpoint_MissingStart(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := httpGet(t, srv.URL+"/v1/graph?end=2026-05-01T12:00:00Z")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	errField, _ := body["error"].(map[string]any)
	assert.Equal(t, "missing_start", errField["reason"])
}

func TestGraphEndpoint_InvalidRange(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	now := time.Now().UTC().Add(-time.Hour)
	q := url.Values{}
	q.Set("start", now.Format(time.RFC3339))
	q.Set("end", now.Add(-5*time.Minute).Format(time.RFC3339))
	resp, err := httpGet(t, srv.URL+"/v1/graph?"+q.Encode())
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// GET /v1/edge-types is removed (BREAKING). A removed v1 route must 404 like
// any unknown path — no redirect, no 410, no Cache-Control, and the standard
// error envelope. The edge types a body can carry are fixed by the graph-api
// specification; there is no discovery endpoint for them.
func TestEdgeTypesEndpoint_Removed(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := httpGet(t, srv.URL+"/v1/edge-types")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Cache-Control"))

	var body struct {
		APIVersion string `json:"apiVersion"`
		Error      struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "v1", body.APIVersion)
	assert.Equal(t, "not_found", body.Error.Reason)
}

func TestLivez(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := httpGet(t, srv.URL+"/livez")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestMetricsEndpoint(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := httpGet(t, srv.URL+"/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDebugEndpoint_DisabledByDefault(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := httpGet(t, srv.URL+"/debug/last-queries")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
