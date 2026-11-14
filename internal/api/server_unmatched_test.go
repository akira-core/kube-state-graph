package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnmatchedRoutes_BucketedMetricLabel guards N1: an unauthenticated caller
// must not be able to inflate kube_state_graph_http_requests_total cardinality
// by spraying arbitrary URLs. Every unmatched route is bucketed under the fixed
// "<unmatched>" path label, and the raw attacker path never appears as a label.
func TestUnmatchedRoutes_BucketedMetricLabel(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	attackerPaths := []string{"/aaa-attacker", "/bbb-attacker", "/v1/ccc-attacker"}
	for _, p := range attackerPaths {
		resp, err := httpGet(t, srv.URL+p)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)

		var body map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		resp.Body.Close()
		errField, _ := body["error"].(map[string]any)
		assert.Equal(t, "not_found", errField["reason"])
	}

	metricsResp, err := httpGet(t, srv.URL+"/metrics")
	require.NoError(t, err)
	defer metricsResp.Body.Close()
	raw, err := io.ReadAll(metricsResp.Body)
	require.NoError(t, err)
	metrics := string(raw)

	assert.Contains(t, metrics, `kube_state_graph_http_requests_total{path="<unmatched>",status="4xx"}`,
		"unmatched requests must be bucketed under the <unmatched> sentinel")
	for _, p := range attackerPaths {
		assert.NotContainsf(t, metrics, `path="`+p+`"`,
			"raw attacker path %q must never become a metric label", p)
	}
	// Sanity: exactly one unmatched series exists despite three distinct URLs.
	assert.Equal(t, 1, strings.Count(metrics, `kube_state_graph_http_requests_total{path="<unmatched>"`))
}
