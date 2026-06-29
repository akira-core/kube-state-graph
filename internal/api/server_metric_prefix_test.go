package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// TestServer_MetricPrefix_AppliedToTopologyQueries asserts that when the
// server is constructed with cfg.MetricPrefix = "o11y_", the build pipeline
// issues PromQL strings that reference the prefixed metric names
// (o11y_kube_pod_info, o11y_kube_node_info, ...) instead of the stock
// kube-state-metrics names. This is the end-to-end wiring contract for
// design.md D26.
func TestServer_MetricPrefix_AppliedToTopologyQueries(t *testing.T) {
	prefixFixtures := fixtureSet{
		"last_over_time(o11y_kube_pod_info": vec(map[string]string{
			"cluster":   "test",
			"namespace": "default",
			"pod":       "web-1",
			"uid":       "uid-web-1",
			"node":      "node-a",
		}),
		"last_over_time(o11y_kube_node_info": vec(map[string]string{
			"cluster": "test",
			"node":    "node-a",
		}),
		// The service-graph metric is never prefixed (D26); this edge keeps
		// web-1 connectivity-connected so it survives the default prune.
		"traces_service_graph_request_total": serviceGraphConnectsWeb1(),
	}
	s := newServerWithMocks(t, newMockQuerier(t, prefixFixtures), func(c *config.Config) {
		c.MetricPrefix = "o11y_"
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	end := time.Now().UTC()
	start := end.Add(-15 * time.Minute)
	resp, err := http.Get(graphURL(srv.URL+"/v1/graph", start, end))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"build should produce non-empty topology against o11y_-prefixed fixtures")

	var body cytoscape.Body
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.NotEmpty(t, body.Elements.Nodes,
		"prefixed fixtures should resolve to at least one node")
}

// TestServer_MetricPrefix_ClusterDiscovery captures every PromQL string the
// /v1/clusters handler issues and asserts the configured prefix lands on the
// cluster-discovery query.
func TestServer_MetricPrefix_ClusterDiscovery(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)

	var mu sync.Mutex
	var observedQueries []string
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, query string, _ time.Time) (model.Vector, error) {
			mu.Lock()
			observedQueries = append(observedQueries, query)
			mu.Unlock()
			if strings.Contains(query, "o11y_kube_node_info") {
				return vec(map[string]string{"cluster": "alpha"}), nil
			}
			return model.Vector{}, nil
		}).
		Maybe()

	s := newServerWithMocks(t, q, func(c *config.Config) {
		c.MetricPrefix = "o11y_"
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/clusters")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	mu.Lock()
	defer mu.Unlock()
	var sawPrefixed bool
	for _, q := range observedQueries {
		if strings.Contains(q, "o11y_kube_node_info") {
			sawPrefixed = true
			break
		}
	}
	assert.True(t, sawPrefixed,
		"cluster-discovery query should contain prefixed metric name; saw queries: %v", observedQueries)
}
