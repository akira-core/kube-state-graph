package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
)

func k8sGraphParams(extra url.Values) url.Values {
	q := url.Values{
		"start":   {"1746442800"},
		"end":     {"1746446400"},
		"cluster": {"c1"},
		"az":      {"zone-a"},
		"env":     {"prod"},
	}
	for k, vs := range extra {
		if len(vs) == 0 {
			delete(q, k)
			continue
		}
		q[k] = vs
	}
	return q
}

func k8sGraphURL(base string, extra url.Values) string {
	return base + "/v1/k8s-graph?" + k8sGraphParams(extra).Encode()
}

// k8sFixtures is shop/web-0 (mounting shop/data) and shop/api-0 on worker-1.
// Needles are matched as substrings of the rendered query.
func k8sFixtures() fixtureSet {
	row := func(extra map[string]string) map[string]string {
		m := map[string]string{"cluster": "c1", "az": "zone-a", "env": "prod"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	return fixtureSet{
		"kube_pod_info{": vec(
			row(map[string]string{"namespace": "shop", "pod": "web-0", "uid": "uid-web", "node": "worker-1"}),
			row(map[string]string{"namespace": "shop", "pod": "api-0", "uid": "uid-api", "node": "worker-1"}),
		),
		"kube_node_info{": vec(row(map[string]string{"node": "worker-1"})),
		"kube_pod_spec_volumes_persistentvolumeclaims_info{": vec(
			row(map[string]string{"namespace": "shop", "pod": "web-0", "persistentvolumeclaim": "data", "volume": "v"})),
		"kube_persistentvolumeclaim_info{": vec(
			row(map[string]string{"namespace": "shop", "persistentvolumeclaim": "data", "volumename": "pvc-1"})),
	}
}

func errorReason(t *testing.T, resp *http.Response) (reason, message string) {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	errField, _ := body["error"].(map[string]any)
	reason, _ = errField["reason"].(string)
	message, _ = errField["message"].(string)
	return reason, message
}

func TestK8sGraph_SuccessBodyShape(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, k8sFixtures()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(k8sGraphURL(srv.URL, url.Values{"node": {"worker-1"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var raw map[string]json.RawMessage
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&raw))
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"apiVersion", "clusters", "elements"}, keys)

	var body cytoscape.Body
	buf, err := json.Marshal(raw)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(buf, &body))
	assert.Equal(t, "v1", body.APIVersion)
	assert.Equal(t, []string{"zone-a-prod-c1"}, body.Clusters)
	types := map[string]int{}
	for _, n := range body.Elements.Nodes {
		types[n.Data.Type]++
	}
	assert.Equal(t, 2, types["pod"])
	assert.Equal(t, 1, types["node"])
	assert.Equal(t, 1, types["pvc"])
	for _, e := range body.Elements.Edges {
		assert.Contains(t, []string{"pod-to-node", "pod-mounts-pvc"}, e.Data.Type)
	}
}

func TestK8sGraph_ParameterErrors(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	long := strings.Repeat("x", 254)
	cases := []struct {
		name   string
		extra  url.Values
		reason string
	}{
		{"missing cluster", url.Values{"cluster": nil, "node": {"w"}}, "missing_cluster"},
		{"missing az", url.Values{"az": nil, "node": {"w"}}, "missing_az"},
		{"missing env", url.Values{"env": nil, "node": {"w"}}, "missing_env"},
		{"missing root", nil, "missing_root"},
		{"storage root alone", url.Values{"aggr": {"aggr1"}}, "missing_root"},
		{"both kinds", url.Values{"pod": {"shop/web-0"}, "node": {"worker-1"}}, "invalid_scope"},
		{"malformed pod", url.Values{"pod": {"web-0"}}, "invalid_scope"},
		{"value too long", url.Values{"node": {long}}, "invalid_scope"},
		{"end not after start", url.Values{"node": {"w"}, "end": {"1746442800"}}, "invalid_range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(k8sGraphURL(srv.URL, tc.extra))
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			reason, msg := errorReason(t, resp)
			assert.Equal(t, tc.reason, reason)
			if tc.name == "both kinds" {
				assert.Contains(t, msg, "node")
				assert.Contains(t, msg, "pod")
			}
		})
	}
}

func TestK8sGraph_OverCapNodeRootIssuesNothing(t *testing.T) {
	q := newMockQuerier(t, nil)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	names := make([]string, 513)
	pad := strings.Repeat("x", 240)
	for i := range names {
		names[i] = fmt.Sprintf("node%04d%s", i, pad)
	}
	resp, err := http.Get(k8sGraphURL(srv.URL, url.Values{"node": names}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	reason, msg := errorReason(t, resp)
	assert.Equal(t, "invalid_scope", reason)
	assert.Equal(t, build.RootScopeCapMessage, msg)
	q.AssertNumberOfCalls(t, "Instant", 0)
}

func TestK8sGraph_RequiresKey(t *testing.T) {
	srv := authServer(t, "k1")
	resp, err := http.Get(k8sGraphURL(srv.URL, url.Values{"node": {"worker-1"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestK8sGraph_Timeout504(t *testing.T) {
	s := newServerWithMocks(t, newStallQuerier(t), func(c *config.Config) { c.BuildTimeout = 20 * time.Millisecond })
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(k8sGraphURL(srv.URL, url.Values{"node": {"worker-1"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
	reason, _ := errorReason(t, resp)
	assert.Equal(t, "timeout", reason)
}

// Any family but ALERTS fails the request, naming it and nothing of the
// upstream error; an ALERTS error degrades to a 200.
func TestK8sGraph_FailsClosedNamingFamily(t *testing.T) {
	for _, family := range []string{"kube_pod_info", "kube_node_info"} {
		t.Run(family, func(t *testing.T) {
			s := newServerWithMocks(t, nameFailQuerier(t, family, upstreamDialErr()), nil)
			srv := httptest.NewServer(s.Handler())
			t.Cleanup(srv.Close)

			resp, err := http.Get(k8sGraphURL(srv.URL, url.Values{"node": {"worker-1"}}))
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusBadGateway, resp.StatusCode)
			reason, msg := errorReason(t, resp)
			assert.Equal(t, "upstream", reason)
			assert.Equal(t, "upstream query failed: "+family, msg)
			assertNoUpstreamLeak(t, msg)
		})
	}

	t.Run("ALERTS degrades", func(t *testing.T) {
		s := newServerWithMocks(t, nameFailQuerier(t, "ALERTS", upstreamDialErr()), nil)
		srv := httptest.NewServer(s.Handler())
		t.Cleanup(srv.Close)

		resp, err := http.Get(k8sGraphURL(srv.URL, url.Values{"node": {"worker-1"}}))
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

// A root the upstream does not name is an empty 200: no outside_retention, and
// no up{} probe to classify one.
func TestK8sGraph_UnknownRootIsAnEmptyOK(t *testing.T) {
	q, captured := recordingQuerier(t)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(k8sGraphURL(srv.URL, url.Values{"pod": {"shop/nope"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body cytoscape.Body
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Empty(t, body.Clusters)
	assert.Empty(t, body.Elements.Nodes)
	assert.Empty(t, body.Elements.Edges)
	assert.NotContains(t, captured(), "up")
}

// The Kubernetes-only read: matchers on every query, nothing Harvest,
// service-side or service-graph.
func TestK8sGraph_QuerySet(t *testing.T) {
	q, captured := recordingQuerier(t)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(k8sGraphURL(srv.URL, url.Values{"namespace": {"shop"}, "node": {"worker-1"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	seen := captured()
	assert.Equal(t,
		`last_over_time(kube_pod_info{az="zone-a",env="prod",cluster="c1",namespace="shop",node="worker-1"}[1h])`,
		seen["kube_pod_info"])
	assert.Equal(t,
		`tlast_over_time(ALERTS{alertstate="firing",az="zone-a",env="prod",namespace=~"shop|"}[1h])`,
		seen["ALERTS"], "ALERTS takes az/env and namespace-or-absent, never cluster")
	for _, forbidden := range []string{
		"volume_labels", "aggr_space_used", "kube_pod_container_info", "kube_service_info",
		"kube_endpointslice_endpoints", "kube_service_annotations",
		"traces_service_graph_request_total", "up",
	} {
		assert.NotContains(t, seen, forbidden)
	}
}

func TestK8sGraph_PruneIsIgnored(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, k8sFixtures()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	get := func(extra url.Values) string {
		resp, err := http.Get(k8sGraphURL(srv.URL, extra))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(raw)
	}
	plain := get(url.Values{"node": {"worker-1"}})
	assert.Equal(t, plain, get(url.Values{"node": {"worker-1"}, "prune": {"false"}}))
	assert.Equal(t, plain, get(url.Values{"node": {"worker-1"}, "prune": {"true"}}))
	assert.Equal(t, get(url.Values{"cluster": {"c1", "c2"}, "node": {"worker-1"}}),
		get(url.Values{"cluster": {"c2", "c1"}, "node": {"worker-1"}}), "value order is irrelevant")
}

// A /v1/k8s-graph build leaves the last-build gauges at the /v1/graph values.
func TestK8sGraph_LeavesLastBuildGaugesAlone(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, k8sFixtures()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	gauges := func() map[string]float64 {
		families, err := s.metrics.Registry.Gather()
		require.NoError(t, err)
		out := map[string]float64{}
		for _, mf := range families {
			switch mf.GetName() {
			case "kube_state_graph_graph_node_count", "kube_state_graph_graph_edge_count", "kube_state_graph_clusters_observed":
				for _, m := range mf.GetMetric() {
					key := mf.GetName()
					for _, l := range m.GetLabel() {
						key += "|" + l.GetName() + "=" + l.GetValue()
					}
					out[key] = m.GetGauge().GetValue()
				}
			}
		}
		return out
	}

	resp, err := http.Get(srv.URL + "/v1/graph?start=1746442800&end=1746446400&prune=false")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	afterGraph := gauges()
	require.NotEmpty(t, afterGraph, "the /v1/graph build wrote the gauges")

	resp, err = http.Get(k8sGraphURL(srv.URL, url.Values{"node": {"worker-1"}}))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, afterGraph, gauges())
}

func TestK8sGraph_EmbedderAndServerAgree(t *testing.T) {
	q := newMockQuerier(t, k8sFixtures())
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	vals := k8sGraphParams(url.Values{"node": {"worker-1"}})
	resp, err := http.Get(srv.URL + "/v1/k8s-graph?" + vals.Encode())
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	eng := kubegraph.New(q, kubegraph.Options{APITimeout: 5 * time.Second})
	facade, err := eng.BuildK8sFromValues(t.Context(), vals)
	require.NoError(t, err)
	want, err := json.Marshal(facade)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(raw), "the endpoint and the engine return the same body")

	for _, tc := range []struct {
		name   string
		values url.Values
		reason string
	}{
		{"missing root", k8sGraphParams(nil), "missing_root"},
		{"both kinds", k8sGraphParams(url.Values{"pod": {"shop/x"}, "node": {"w"}}), "invalid_scope"},
		{"missing cluster", k8sGraphParams(url.Values{"cluster": nil, "node": {"w"}}), "missing_cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ferr := eng.BuildK8sFromValues(t.Context(), tc.values)
			var pe *kubegraph.ParseError
			require.ErrorAs(t, ferr, &pe)
			assert.Equal(t, tc.reason, pe.Reason)
		})
	}
}
