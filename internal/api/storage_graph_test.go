package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/build"
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

	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{"aggr": {"aggr1"}}))
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
	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{"aggr": {"aggr1"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestStorageGraph_SelectorQueriesCaptured(t *testing.T) {
	q, captured := recordingQuerier(t)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{"namespace": {"shop"}, "node": {"worker-1"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	seen := captured()
	assert.NotContains(t, seen, "kube_pod_spec_volumes_persistentvolumeclaims_info",
		"the node seed found no pod, so no binding is fetched")
	assert.Equal(t,
		`last_over_time(kube_pod_info{az="zone-a",env="prod",namespace="shop",node="worker-1"}[1h])`,
		seen["kube_pod_info"], "the node seed carries the request matchers, then the node")
	for _, skipped := range []string{
		"kube_pod_container_info", "kube_service_info", "kube_endpointslice_endpoints",
		"kube_endpointslice_labels", "kube_service_annotations",
	} {
		assert.NotContains(t, seen, skipped, "the storage body cannot carry %s, so it is never read", skipped)
	}
	assert.NotContains(t, seen, "volume_labels",
		"a Kubernetes node with no pod tracks no claim, so volume_labels is not read")
	assert.Equal(t,
		`last_over_time(ALERTS{alertstate="firing",az="zone-a",env="prod",namespace=~"shop|"}[1h])`,
		seen["ALERTS"], "ALERTS takes az/env and namespace-or-absent, never cluster")
	assert.NotContains(t, seen, "traces_service_graph_request_total",
		"storage-graph never reads the service graph")
	assert.NotContains(t, seen, "up", "no retention probe on a filtered build")
}

// A pod root reads kube_pod_info for the root (namespace, pod) pairs, one query
// per namespace, and does not derive a namespace matcher onto the controller
// wave. Harvest is read only for the claims the roots mount; this fixture
// mounts none.
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
	// The recorder keeps the last query per family, and the two namespaces'
	// queries run concurrently, so either may be the one kept.
	assert.Contains(t, []string{
		`last_over_time(kube_pod_info{az="zone-a",env="prod",namespace="platform",pod="redis-0"}[1h])`,
		`last_over_time(kube_pod_info{az="zone-a",env="prod",namespace="shop",pod="orders-0"}[1h])`,
	}, seen["kube_pod_info"], "each namespace's pods are read by their own query")
	assert.Equal(t,
		`last_over_time(ALERTS{alertstate="firing",az="zone-a",env="prod"}[1h])`,
		seen["ALERTS"])
	assert.NotContains(t, seen, "volume_labels",
		"the pod mounts no claim in this fixture, so candidate completion is not issued")
	assert.Equal(t,
		`last_over_time(kube_statefulset_annotations{annotation_argocd_argoproj_io_tracking_id!="",az="zone-a",env="prod",statefulset="orders"}[1h])`,
		seen["kube_statefulset_annotations"],
		"the by-reference controller scope reaches this query; a pod root does not derive a namespace matcher")
}

func TestStorageGraph_Timeout504(t *testing.T) {
	s := newServerWithMocks(t, newStallQuerier(t), func(c *config.Config) { c.BuildTimeout = 20 * time.Millisecond })
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{"aggr": {"aggr1"}}))
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

func TestStorageGraph_OverCapAggregateRootIssuesNothing(t *testing.T) {
	q := newMockQuerier(t, nil)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// 513 names of ~248 bytes are 17 chunks at the default 8192-byte budget,
	// one past the 16-chunk cap. The rejection is a pure function of the
	// values, so the querier is never asked.
	names := make([]string, 513)
	pad := strings.Repeat("x", 240)
	for i := range names {
		names[i] = fmt.Sprintf("aggr%04d%s", i, pad)
	}
	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{"aggr": names}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	errField, _ := body["error"].(map[string]any)
	assert.Equal(t, "invalid_scope", errField["reason"])
	assert.Equal(t, build.RootScopeCapMessage, errField["message"])
	q.AssertNumberOfCalls(t, "Instant", 0)

	eng := kubegraph.New(q, kubegraph.Options{APITimeout: 5 * time.Second})
	_, ferr := eng.BuildStorageFromValues(t.Context(), url.Values{
		"start": {"1746442800"},
		"end":   {"1746446400"},
		"az":    {"zone-a"},
		"env":   {"prod"},
		"aggr":  names,
	})
	require.Equal(t, build.ReasonInvalidScope, build.AsReason(ferr))
	q.AssertNumberOfCalls(t, "Instant", 0)
}

func TestStorageGraph_MixedRootKindsRejected(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{
		"pod":         {"shop/x"},
		"application": {"y"},
	}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	var mixed map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&mixed))
	errField, _ := mixed["error"].(map[string]any)
	assert.Equal(t, "invalid_scope", errField["reason"])
	assert.Contains(t, errField["message"], "application")
	assert.Contains(t, errField["message"], "pod")
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

	for _, tc := range []struct {
		name   string
		values url.Values
		reason string
	}{
		{"missing root", url.Values{"start": {"1746442800"}, "end": {"1746446400"}, "az": {"zone-a"}, "env": {"prod"}}, "missing_root"},
		{"mixed kinds", url.Values{"start": {"1746442800"}, "end": {"1746446400"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {"aggr1"}, "pod": {"shop/orders-0"}}, "invalid_scope"},
		{"empty root", url.Values{"start": {"1746442800"}, "end": {"1746446400"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {""}}, "missing_root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + "/v1/storage-graph?" + tc.values.Encode())
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			var httpErr map[string]any
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&httpErr))
			errField, _ := httpErr["error"].(map[string]any)
			assert.Equal(t, tc.reason, errField["reason"])

			_, ferr := eng.BuildStorageFromValues(t.Context(), tc.values)
			var pe *kubegraph.ParseError
			require.ErrorAs(t, ferr, &pe)
			assert.Equal(t, tc.reason, pe.Reason)
		})
	}
}
