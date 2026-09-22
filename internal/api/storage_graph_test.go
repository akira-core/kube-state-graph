package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/common/model"
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
		`last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info{az="zone-a",env="prod",namespace="shop"}[1h])`,
		seen["kube_pod_spec_volumes_persistentvolumeclaims_info"])
	assert.NotContains(t, seen, "kube_pod_info",
		"no claim binding and no pod root: the pod read has an empty scope and is not issued")
	for _, skipped := range []string{
		"kube_pod_container_info", "kube_service_info", "kube_endpointslice_endpoints",
		"kube_endpointslice_labels", "kube_service_annotations",
	} {
		assert.NotContains(t, seen, skipped, "the storage body cannot carry %s, so it is never read", skipped)
	}
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

// Pod-only roots push their namespaces into every namespaced leg, and their
// names into the pod scope; Harvest is never narrowed by a root. The loaded
// pod's owner also drives the controller wave, so a StatefulSet-owned root
// carries BOTH the derived namespace matcher and the by-reference
// `statefulset=` scope on the same query.
func TestStorageGraph_PodOnlyRootsNarrowTheRead(t *testing.T) {
	q, captured := recordingQuerierWith(t, map[string]model.Vector{
		"kube_pod_owner": {&model.Sample{Metric: model.Metric{
			"namespace": "shop", "pod": "orders-0",
			"owner_kind": "StatefulSet", "owner_name": "orders", "owner_is_controller": "true",
		}, Value: 1}},
	})
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{"pod": {"shop/orders-0", "platform/redis-0"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	seen := captured()
	assert.Equal(t,
		`last_over_time(kube_pod_info{az="zone-a",env="prod",namespace=~"platform|shop",pod=~"orders-0|redis-0"}[1h])`,
		seen["kube_pod_info"])
	assert.Equal(t,
		`last_over_time(ALERTS{alertstate="firing",az="zone-a",env="prod",namespace=~"platform|shop|"}[1h])`,
		seen["ALERTS"])
	assert.Equal(t, `last_over_time(volume_labels[1h])`, seen["volume_labels"])
	assert.Equal(t,
		`last_over_time(kube_statefulset_annotations{annotation_argocd_argoproj_io_tracking_id!="",az="zone-a",env="prod",namespace=~"platform|shop",statefulset="orders"}[1h])`,
		seen["kube_statefulset_annotations"],
		"the derived namespace matcher and the by-reference controller scope both reach this query")
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

func TestStorageGraph_ApplicationRootIssuesRecovery(t *testing.T) {
	q, captured := recordingQuerier(t)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{"application": {"checkout"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	seen := captured()
	got := seen["kube_deployment_annotations"]
	assert.Contains(t, got, `annotation_argocd_argoproj_io_tracking_id!=""`)
	assert.Contains(t, got, `az="zone-a"`)
	assert.Contains(t, got, `env="prod"`)
	assert.Contains(t, got, `annotation_argocd_argoproj_io_tracking_id=~"(?:checkout)(?::.*)?"`)
}

func TestStorageGraph_ApplicationRootSuppressesNamespaceDerivation(t *testing.T) {
	q, captured := recordingQuerier(t)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{
		"pod":         {"shop/x"},
		"application": {"y"},
	}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	for name, query := range captured() {
		assert.NotContains(t, query, "namespace=", "%s must not carry a derived namespace matcher", name)
	}
}

func TestStorageGraph_EmbedderAndServerAgree(t *testing.T) {
	q := newMockQuerier(t, nil)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	vals := url.Values{
		"start":       {"1746442800"},
		"end":         {"1746446400"},
		"az":          {"zone-a"},
		"env":         {"prod"},
		"aggr":        {"aggr1"},
		"application": {"checkout"},
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
