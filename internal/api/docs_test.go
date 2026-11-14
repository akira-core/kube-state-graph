package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDocs_ServesScalarUI asserts /docs serves the Scalar API Reference HTML:
// it loads the Scalar bundle from the CDN and points the reference at our
// same-origin OpenAPI spec.
func TestDocs_ServesScalarUI(t *testing.T) {
	q := newMockQuerier(t, nil)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := httpGet(t, srv.URL+"/docs")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "cdn.jsdelivr.net/npm/@scalar/api-reference",
		"/docs must load the Scalar bundle from the CDN")
	assert.Contains(t, string(body), "Scalar.createApiReference",
		"/docs must initialise Scalar via createApiReference")
	assert.Contains(t, string(body), "/openapi.json",
		"/docs must point the reference at the same-origin spec")
}

func TestOpenAPIYAMLEndpoint(t *testing.T) {
	q := newMockQuerier(t, nil)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := httpGet(t, srv.URL+"/openapi.yaml")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/yaml")
	assert.Contains(t, resp.Header.Get("Cache-Control"), "max-age=3600")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "openapi:")
}

func TestOpenAPIJSONEndpoint(t *testing.T) {
	q := newMockQuerier(t, nil)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := httpGet(t, srv.URL+"/openapi.json")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	assert.Contains(t, resp.Header.Get("Cache-Control"), "max-age=3600")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.NotEmpty(t, body)
}
