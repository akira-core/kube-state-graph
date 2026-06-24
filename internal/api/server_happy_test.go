package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/cytoscape"
	promqlmocks "github.com/marz32one/kube-state-graph/pkg/promql/mocks"
)

// happyFixtures returns one pod and one node. Builder.Build emits a non-empty
// topology and the request reaches the serialiser.
func happyFixtures() fixtureSet {
	return fixtureSet{
		"last_over_time(kube_pod_info": vec(map[string]string{
			"cluster":       "test",
			"namespace":     "default",
			"pod":           "web-1",
			"uid":           "uid-web-1",
			"node":          "node-a",
			"host_ip":       "10.0.0.1",
			"pod_ip":        "10.244.0.10",
			"pod_ip_family": "IPv4",
		}),
		"last_over_time(kube_node_info": vec(map[string]string{
			"cluster":                   "test",
			"node":                      "node-a",
			"kernel_version":            "6.1",
			"os_image":                  "linux",
			"container_runtime_version": "containerd",
		}),
	}
}

func graphURL(base string, start, end time.Time) string {
	q := url.Values{}
	q.Set("start", start.Format(time.RFC3339))
	q.Set("end", end.Format(time.RFC3339))
	return base + "?" + q.Encode()
}

func TestGraphEndpoint_HappyPath(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, happyFixtures()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	end := time.Now().UTC()
	start := end.Add(-15 * time.Minute)
	resp, err := http.Get(graphURL(srv.URL+"/v1/graph", start, end))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body cytoscape.Body
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "v1", body.APIVersion)
	assert.NotEmpty(t, body.Elements.Nodes, "expected at least one node in cytoscape body")

	// Validate the new top-level IPAddress attribute on pod and node entries.
	var podIPs, nodeIPs []string
	for _, n := range body.Elements.Nodes {
		switch n.Data.Type {
		case "pod":
			podIPs = n.Data.IPAddress
			_, hasPodIP := n.Data.Labels["pod_ip"]
			_, hasHostIP := n.Data.Labels["host_ip"]
			assert.False(t, hasPodIP, "labels.pod_ip must not be emitted")
			assert.False(t, hasHostIP, "labels.host_ip must not be emitted")
		case "node":
			nodeIPs = n.Data.IPAddress
			_, hasExternalIP := n.Data.Labels["external_ip"]
			assert.False(t, hasExternalIP, "labels.external_ip must not be emitted")
		}
	}
	assert.Equal(t, []string{"10.244.0.10"}, podIPs, "pod ipaddress must carry pod_ip")
	assert.Empty(t, nodeIPs, "happy fixture provides no ExternalIP for the node")
}

// TestGraphEndpoint_NodeInternalIPFallback — a node whose only
// kube_node_status_addresses rows are InternalIP surfaces that address on
// data.ipaddress; no IP ever appears in labels.
func TestGraphEndpoint_NodeInternalIPFallback(t *testing.T) {
	fixtures := happyFixtures()
	fixtures["last_over_time(kube_node_status_addresses"] = vec(map[string]string{
		"cluster": "test",
		"node":    "node-a",
		"type":    "InternalIP",
		"address": "10.0.0.7",
	})
	s := newServerWithMocks(t, newMockQuerier(t, fixtures), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	end := time.Now().UTC()
	start := end.Add(-15 * time.Minute)
	resp, err := http.Get(graphURL(srv.URL+"/v1/graph", start, end))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body cytoscape.Body
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	var nodeIPs []string
	for _, n := range body.Elements.Nodes {
		if n.Data.Type == "node" {
			nodeIPs = n.Data.IPAddress
			_, hasInternalIP := n.Data.Labels["internal_ip"]
			_, hasExternalIP := n.Data.Labels["external_ip"]
			assert.False(t, hasInternalIP, "labels.internal_ip must not be emitted")
			assert.False(t, hasExternalIP, "labels.external_ip must not be emitted")
		}
	}
	assert.Equal(t, []string{"10.0.0.7"}, nodeIPs,
		"InternalIP must surface on data.ipaddress when no ExternalIP exists")
}

func TestGraphEndpoint_OutsideRetention_ReturnsError(t *testing.T) {
	// All topology queries return empty + up probe returns one sample
	// → outside_retention.
	s := newServerWithMocks(t, newMockQuerier(t, fixtureSet{
		"up": vec(map[string]string{"job": "vm"}),
	}), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	end := time.Now().UTC()
	start := end.Add(-15 * time.Minute)
	resp, err := http.Get(graphURL(srv.URL+"/v1/graph", start, end))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	errField, _ := body["error"].(map[string]any)
	assert.Equal(t, "outside_retention", errField["reason"])
}

func TestGraphEndpoint_UpstreamError_Returns502(t *testing.T) {
	s := newServerWithMocks(t, newErrQuerier(t, errors.New("upstream 500: boom")), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	end := time.Now().UTC()
	start := end.Add(-15 * time.Minute)
	resp, err := http.Get(graphURL(srv.URL+"/v1/graph", start, end))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// --- /v1/clusters --------------------------------------------------------

// clustersFixture returns three sample series matching the discovery query.
func clustersFixture() fixtureSet {
	return fixtureSet{
		"group by (cluster)": vec(
			map[string]string{"cluster": "prod-east"},
			map[string]string{"cluster": "prod-west"},
			map[string]string{"cluster": "stg"},
		),
	}
}

func TestClustersEndpoint_SortedOutput(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, clustersFixture()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/clusters")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body clustersBody
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Clusters, 3)
	assert.Equal(t, "prod-east", body.Clusters[0].Name)
	assert.Equal(t, "prod-west", body.Clusters[1].Name)
	assert.Equal(t, "stg", body.Clusters[2].Name)
}

func TestClustersEndpoint_HitsUpstreamPerRequest(t *testing.T) {
	// Counting variant: a thin RunAndReturn around the substring matcher
	// records how many times the discovery query was dispatched. Mockery's
	// .Times(n) covers this too, but here we want to assert the *minimum*
	// independent-request count without coupling to call ordering.
	var calls atomic.Int32
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, query string, _ time.Time) (model.Vector, error) {
			if strings.Contains(query, "group by (cluster)") {
				calls.Add(1)
				return vec(
					map[string]string{"cluster": "prod-east"},
					map[string]string{"cluster": "prod-west"},
					map[string]string{"cluster": "stg"},
				), nil
			}
			return model.Vector{}, nil
		}).
		Times(3)

	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	for range 3 {
		resp, err := http.Get(srv.URL + "/v1/clusters")
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	assert.Equal(t, int32(3), calls.Load(), "each /v1/clusters request must hit upstream (no in-process cache)")
}

func TestClustersEndpoint_UpstreamError_Returns502(t *testing.T) {
	s := newServerWithMocks(t, newErrQuerier(t, errors.New("upstream 500: boom")), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/clusters")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}
