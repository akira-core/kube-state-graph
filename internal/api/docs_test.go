package api

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
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

// Spec (design D9): /docs allows its one inline script by hash, not by
// 'unsafe-inline'. The test recomputes the hash from the inline script the page
// actually serves, so the header and the page cannot drift.
func TestDocs_ScriptSrcAllowsTheInlineScriptByHashOnly(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/docs")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	inline := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(body), -1)
	require.Len(t, inline, 1, "exactly one inline script")
	sum := sha256.Sum256([]byte(inline[0][1]))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"

	var scriptSrc string
	for d := range strings.SplitSeq(resp.Header.Get("Content-Security-Policy"), ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "script-src ") {
			scriptSrc = d
		}
	}
	require.NotEmpty(t, scriptSrc)
	assert.Contains(t, strings.Fields(scriptSrc), want, "the inline script's hash is allowed")
	assert.NotContains(t, scriptSrc, "'unsafe-inline'", "no blanket inline allowance for scripts")
	assert.Contains(t, scriptSrc, "https://cdn.jsdelivr.net", "the pinned bundle still loads")
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
