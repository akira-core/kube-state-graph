package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/cytoscape"
	"github.com/marz32one/kube-state-graph/pkg/graph"
)

// TestGraphEndpoint_NodeReadyStatus — end-to-end the K8s node carries
// data.ready_status (from the active kube_node_status_condition{condition="Ready"}
// row); the status never leaks into labels, and a non-node (pod) omits it.
func TestGraphEndpoint_NodeReadyStatus(t *testing.T) {
	fixtures := happyFixtures()
	// Only the active Ready row — vec() stamps Value=1 (the active combination).
	// cluster/node match happyFixtures' node-a.
	fixtures["kube_node_status_condition"] = vec(map[string]string{
		"cluster":   "test",
		"node":      "node-a",
		"condition": "Ready",
		"status":    "true",
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

	var podSeen, nodeSeen bool
	for _, n := range body.Elements.Nodes {
		switch n.Data.Type {
		case "node":
			nodeSeen = true
			assert.Equal(t, graph.ReadyStatusReady, n.Data.ReadyStatus,
				"data.ready_status = Ready from the active condition=Ready,status=true row")
			_, hasReadyStatus := n.Data.Labels["ready_status"]
			_, hasCondition := n.Data.Labels["condition"]
			_, hasStatus := n.Data.Labels["status"]
			assert.False(t, hasReadyStatus || hasCondition || hasStatus,
				"ready status must not appear inside labels")
		case "pod":
			podSeen = true
			assert.Empty(t, n.Data.ReadyStatus, "non-node (pod) omits ready_status")
		}
	}
	require.True(t, nodeSeen, "expected a K8s node")
	require.True(t, podSeen, "expected a pod node")
}
