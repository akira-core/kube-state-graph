package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
)

// TestGraphEndpoint_MissingClientUID_PromotesToExternal exercises the
// Section 31 / D27 fallback at the HTTP boundary: a service-graph series
// with empty client_k8s_pod_uid + non-empty client label produces an
// `external` node carrying the human label as `name`, and the resulting
// edge omits `labels.cluster` (the client side is external).
func TestGraphEndpoint_MissingClientUID_PromotesToExternal(t *testing.T) {
	fixtures := fixtureSet{
		// Topology: a single resolvable pod in cluster-alpha so the server
		// side of the service-graph edge anchors to a real node.
		"last_over_time(kube_pod_info": vec(map[string]string{
			"cluster":   "cluster-alpha",
			"namespace": "default",
			"pod":       "checkout",
			"uid":       "uid-checkout",
			"node":      "node-a",
		}),
		"last_over_time(kube_node_info": vec(map[string]string{
			"cluster":                   "cluster-alpha",
			"node":                      "node-a",
			"kernel_version":            "6.1",
			"os_image":                  "linux",
			"container_runtime_version": "containerd",
		}),
		// Service-graph: client UID intentionally empty; client label "admin"
		// names the dependency. Server resolves via UID to the topology pod.
		"traces_service_graph_request_total": vec(map[string]string{
			"client":             "admin",
			"server":             "checkout",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "",
			"server_k8s_pod_uid": "uid-checkout",
		}),
	}

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

	// Locate the external/admin node.
	var extName string
	var extLabels map[string]string
	for _, n := range body.Elements.Nodes {
		if n.Data.ID == "external/admin" {
			extName = n.Data.Name
			extLabels = n.Data.Labels
			assert.Equal(t, "external", n.Data.Type)
		}
	}
	require.Equal(t, "admin", extName, "external/admin node must surface with human label as name")
	assert.NotContains(t, extLabels, "pattern",
		"missing-UID fallback MUST NOT carry labels.pattern (no pattern fired)")
	assert.NotContains(t, extLabels, "cluster")

	// Locate the pod-calls-pod edge from external/admin → cluster-alpha/uid-checkout.
	var sawEdge bool
	for _, e := range body.Elements.Edges {
		if e.Data.Source == "external/admin" && e.Data.Target == "cluster-alpha/uid-checkout" {
			sawEdge = true
			assert.Equal(t, "pod-calls-pod", e.Data.Type)
			assert.NotContains(t, e.Data.Labels, "cluster",
				"edge cluster label MUST be omitted when client side is external (D27)")
		}
	}
	assert.True(t, sawEdge, "expected pod-calls-pod edge from external/admin to cluster-alpha/uid-checkout")
}
