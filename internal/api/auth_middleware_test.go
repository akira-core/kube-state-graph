package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/auth"
)

// authServer constructs a handler with the supplied API keys loaded into a
// real auth.KeySet (the production validator — pure in-memory, no I/O).
func authServer(t *testing.T, keys ...string) *httptest.Server {
	t.Helper()
	ks := auth.NewKeySet()
	if len(keys) > 0 {
		ks.LoadCSV(strings.Join(keys, ","))
	}
	s := newServerWithMocksAndKeys(t, newMockQuerier(t, nil), ks, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestAuth_Disabled_AllRoutesPassThrough(t *testing.T) {
	srv := authServer(t) // no keys = auth disabled

	for _, path := range []string{"/livez", "/v1/edge-types", "/metrics"} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equalf(t, http.StatusOK, resp.StatusCode, "path %s", path)
	}
}

func TestAuth_MissingHeader_Returns401(t *testing.T) {
	srv := authServer(t, "k1")

	resp, err := http.Get(srv.URL + "/v1/edge-types")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	errField, _ := body["error"].(map[string]any)
	assert.Equal(t, "unauthorized", errField["reason"])
}

func TestAuth_WrongKey_Returns401(t *testing.T) {
	srv := authServer(t, "k1")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/edge-types", nil)
	req.Header.Set(APIKeyHeader, "wrong-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAuth_ValidKey_Passes(t *testing.T) {
	srv := authServer(t, "k1", "k2")

	for _, key := range []string{"k1", "k2"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/edge-types", nil)
		req.Header.Set(APIKeyHeader, key)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equalf(t, http.StatusOK, resp.StatusCode, "key %s rejected", key)
	}
}

func TestAuth_OpenPaths_BypassWithoutKey(t *testing.T) {
	srv := authServer(t, "k1")

	for _, path := range []string{"/livez", "/metrics", "/openapi.yaml", "/openapi.json", "/docs"} {
		resp, err := http.Get(srv.URL + path)
		require.NoErrorf(t, err, "path %s", path)
		_ = resp.Body.Close()
		assert.Equalf(t, http.StatusOK, resp.StatusCode, "open path %s should not require key", path)
	}
}

func TestAuth_GraphRoute_RequiresKey(t *testing.T) {
	srv := authServer(t, "k1")

	resp, err := http.Get(srv.URL + "/v1/graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
