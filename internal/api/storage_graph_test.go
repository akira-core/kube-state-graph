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

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
)

func storageGraphURL(base string, extra url.Values) string {
	q := url.Values{
		"start": {"1746442800"},
		"end":   {"1746446400"},
		"az":    {"zone-a"},
		"env":   {"prod"},
	}
	for k, vs := range extra {
		q[k] = vs
	}
	return base + "/v1/storage-graph?" + q.Encode()
}

func TestStorageGraph_SuccessBodyShape(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(storageGraphURL(srv.URL, nil))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body cytoscape.Body
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "v1", body.APIVersion)
	assert.Empty(t, body.Clusters)
	assert.Empty(t, body.Elements.Nodes)
	assert.Empty(t, body.Elements.Edges)
}

func TestStorageGraph_MissingAZ(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/storage-graph?start=1746442800&end=1746446400&env=prod")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	errField, _ := body["error"].(map[string]any)
	assert.Equal(t, "missing_az", errField["reason"])
}

func TestStorageGraph_RepeatedEnv(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/storage-graph?start=1746442800&end=1746446400&az=zone-a&env=prod&env=dev")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	errField, _ := body["error"].(map[string]any)
	assert.Equal(t, "invalid_scope", errField["reason"])
	assert.Contains(t, errField["message"], "env")
}

func TestStorageGraph_RequiresKey(t *testing.T) {
	srv := authServer(t, "k1")
	resp, err := http.Get(storageGraphURL(srv.URL, nil))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestStorageGraph_SelectorQueriesCaptured(t *testing.T) {
	q, captured := recordingQuerier(t)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{"namespace": {"shop"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	seen := captured()
	assert.Equal(t,
		`last_over_time(kube_pod_info{az="zone-a",env="prod",namespace="shop"}[1h])`,
		seen["kube_pod_info"])
	assert.Equal(t,
		`last_over_time(volume_labels[1h])`,
		seen["volume_labels"], "Harvest takes no request matcher — az only routes it")
	assert.Equal(t,
		`last_over_time(ALERTS{alertstate="firing",az="zone-a",env="prod",namespace=~"shop|"}[1h])`,
		seen["ALERTS"], "ALERTS takes az/env and namespace-or-absent, never cluster")
	assert.NotContains(t, seen, "traces_service_graph_request_total",
		"storage-graph never reads the service graph")
	assert.NotContains(t, seen, "up", "no retention probe on a filtered build")
}

func TestStorageGraph_Timeout504(t *testing.T) {
	s := newServerWithMocks(t, newStallQuerier(t), func(c *config.Config) { c.BuildTimeout = 20 * time.Millisecond })
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(storageGraphURL(srv.URL, nil))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	errField, _ := body["error"].(map[string]any)
	assert.Equal(t, "timeout", errField["reason"])
}

func TestStorageGraph_EmbedderAndServerAgree(t *testing.T) {
	q := newMockQuerier(t, nil)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	vals := url.Values{
		"start": {"1746442800"},
		"end":   {"1746446400"},
		"az":    {"zone-a"},
		"env":   {"prod"},
		"aggr":  {"aggr1"},
	}
	resp, err := http.Get(srv.URL + "/v1/storage-graph?" + vals.Encode())
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var httpBody cytoscape.Body
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&httpBody))

	eng := kubegraph.New(q, kubegraph.Options{APITimeout: 5 * time.Second})
	facade, err := eng.BuildStorageFromValues(t.Context(), vals)
	require.NoError(t, err)
	assert.Equal(t, httpBody, facade)
}
