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

// TestGraphEndpoint_PodApplicationAndContainers — end-to-end the pod node
// carries data.application (parsed from the kube_pod_owner argocd_tracking_id
// label) and data.containers (from kube_pod_container_info, ordered by
// (name, image)); neither leaks into labels, and a non-pod node omits both.
func TestGraphEndpoint_PodApplicationAndContainers(t *testing.T) {
	fixtures := happyFixtures()
	fixtures["last_over_time(kube_pod_owner"] = vec(map[string]string{
		"cluster":             "test",
		"namespace":           "default",
		"pod":                 "web-1",
		"owner_kind":          "ReplicaSet",
		"owner_name":          "web-7f9c",
		"owner_is_controller": "true",
		"argocd_tracking_id":  "storefront:apps/Deployment:default/web",
	})
	fixtures["tlast_over_time(kube_pod_container_info"] = vec(
		map[string]string{
			"cluster": "test", "namespace": "default", "pod": "web-1",
			"container": "app", "image": "reg/web:1.4",
		},
		map[string]string{
			"cluster": "test", "namespace": "default", "pod": "web-1",
			"container": "istio-proxy", "image": "reg/proxy:0.9",
		},
	)

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
		case "pod":
			podSeen = true
			assert.Equal(t, "storefront", n.Data.Application,
				"data.application = ArgoCD app (segment before the first ':')")
			assert.Equal(t, []graph.Container{
				{Name: "app", Image: "reg/web:1.4"},
				{Name: "istio-proxy", Image: "reg/proxy:0.9"},
			}, n.Data.Containers, "data.containers ordered by (name, image)")
			_, hasApp := n.Data.Labels["application"]
			_, hasTrack := n.Data.Labels["argocd_tracking_id"]
			_, hasCtr := n.Data.Labels["containers"]
			assert.False(t, hasApp || hasTrack, "application must not appear in labels")
			assert.False(t, hasCtr, "containers must not appear in labels")
		case "node":
			nodeSeen = true
			assert.Empty(t, n.Data.Application, "non-pod node omits application")
			assert.Nil(t, n.Data.Containers, "non-pod node omits containers")
		}
	}
	require.True(t, podSeen, "expected a pod node")
	require.True(t, nodeSeen, "expected a K8s node")
}
