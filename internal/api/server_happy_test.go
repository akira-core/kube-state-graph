package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// happyFixtures returns one pod and one node, plus a service-graph series in
// which the pod calls an external endpoint. The default projection retains only
// pods that sit on a connectivity edge, so without that edge web-1 (and its host
// node) would be pruned and the serialiser would see an empty graph; the
// pod-calls-pod → external edge keeps the pod connected. The service-graph
// metric is never metric-prefixed (different exporter family — D26).
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
		"traces_service_graph_request_total": serviceGraphConnectsWeb1(),
	}
}

// serviceGraphConnectsWeb1 is a single service-graph series whose client is the
// happy-fixture pod (uid-web-1) and whose server is an external label with no
// UID — resolving (missing-UID fallback, D27) to an external node and a
// pod-calls-pod edge that makes web-1 connectivity-connected.
func serviceGraphConnectsWeb1() model.Vector {
	return vec(map[string]string{
		"cluster":            "test",
		"client_k8s_pod_uid": "uid-web-1",
		"server":             "external-api",
	})
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

// --- removed endpoint ----------------------------------------------------

// /v1/clusters is removed (BREAKING). A removed v1 route must 404 like any
// unknown path — no redirect, no 410, and no upstream call.
func TestClustersEndpoint_Removed(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _ time.Time) (model.Vector, error) {
			t.Error("a removed route must not query upstream")
			return model.Vector{}, nil
		}).
		Maybe()

	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/clusters")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// The clusters list now lives in the graph body, derived from the built
// graph's node labels and sorted.
func TestGraphEndpoint_ClustersFieldListsObservedClusters(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, happyFixtures()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=1746442800&end=1746446400&prune=false")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Clusters []string `json:"clusters"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, []string{"test"}, body.Clusters, "derived from the built graph's node labels")
}
