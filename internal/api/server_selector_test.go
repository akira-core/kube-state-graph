package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// recordingQuerier captures every query string the build issues, keyed by
// query name. The MockQuerier cannot itself honour a label matcher, so the
// captured strings — not the response body — are what prove the push-down
// reached the wire.
//
// podInfo, when supplied, is what the kube_pod_info leg answers with. A
// filtered build that loads NO topology skips the service-graph read entirely
// (no series could survive admission), so a test asserting what those queries
// look like has to keep the topology non-empty.
func recordingQuerier(t *testing.T, podInfo ...*model.Sample) (*promqlmocks.MockQuerier, func() map[string]string) {
	t.Helper()
	fixtures := map[string]model.Vector{}
	if len(podInfo) > 0 {
		fixtures["kube_pod_info"] = model.Vector(podInfo)
	}
	return recordingQuerierWith(t, fixtures)
}

// recordingQuerierWith is recordingQuerier with a per-leg fixture table. The
// six QoS workload legs are issued only for FlexVol names a loaded claim
// already matched, so a test asserting what THOSE queries look like must answer
// kube_persistentvolumeclaim_info and volume_labels too.
func recordingQuerierWith(t *testing.T, fixtures map[string]model.Vector) (*promqlmocks.MockQuerier, func() map[string]string) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]string{}
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name, query string, _ time.Time) (model.Vector, error) {
			mu.Lock()
			seen[name] = query
			out := fixtures[name]
			mu.Unlock()
			if out != nil {
				return out, nil
			}
			return model.Vector{}, nil
		}).Maybe()
	return q, func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]string, len(seen))
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

// TestGraph_SelectorQueriesCaptured is the end-to-end push-down proof: the four
// selector-level parameters reach the upstream queries through the HTTP
// handler, each series receiving exactly the dimensions its labels support,
// and the service-graph family receiving none.
func TestGraph_SelectorQueriesCaptured(t *testing.T) {
	q, captured := recordingQuerierWith(t, map[string]model.Vector{
		"kube_pod_info": {&model.Sample{Metric: model.Metric{
			"cluster": "cluster-alpha", "namespace": "shop", "pod": "checkout", "uid": "alpha-1",
		}, Value: 1}},
		// A claim that matches a FlexVol, so the scoped QoS leg is issued and
		// its query string can be asserted below.
		"kube_persistentvolumeclaim_info": {&model.Sample{Metric: model.Metric{
			"cluster": "cluster-alpha", "namespace": "shop",
			"persistentvolumeclaim": "netapp-data", "volumename": "pvc-9f3a",
		}, Value: 1}},
		"volume_labels": {&model.Sample{Metric: model.Metric{
			"volume": "trident_pvc_9f3a", "cluster": "ontap-prod",
			"node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm-prod",
		}, Value: 1}},
	})
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=1746442800&end=1746446400" +
		"&cluster=cluster-alpha&namespace=shop&az=zone-a&env=prod")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	seen := captured()
	assert.Equal(t,
		`last_over_time(kube_pod_info{az="zone-a",env="prod",cluster="cluster-alpha",namespace="shop"}[1h])`,
		seen["kube_pod_info"])
	assert.Equal(t,
		`last_over_time(kube_node_status_addresses{type=~"ExternalIP|InternalIP",az="zone-a",env="prod",cluster="cluster-alpha"}[1h])`,
		seen["kube_node_status_addresses"], "the fixed selector stays ahead of the request matchers")
	assert.Equal(t,
		`last_over_time(volume_labels[1h])`,
		seen["volume_labels"], "Harvest takes no request matcher — az only routes it, env is inert")
	assert.Equal(t,
		`last_over_time(qos_read_ops{lun="",volume="trident_pvc_9f3a"}[1h])`,
		seen["qos_read_ops"],
		"the QoS scope is derived from the matched FlexVol names, never from the request: still no az / env / cluster / namespace matcher")
	assert.Equal(t,
		`last_over_time(kubelet_volume_stats_used_bytes{az="zone-a",env="prod",cluster="cluster-alpha",namespace="shop"}[1h])`,
		seen["kubelet_volume_stats_used_bytes"])
	assert.Equal(t,
		`rate(traces_service_graph_request_total{client!~"user|unknown",server!~"user"}[1h])`,
		seen["traces_service_graph_request_total"], "service-graph queries are never narrowed")
	assert.NotContains(t, seen, "up", "no retention probe on a filtered build")
}

// The unfiltered request must reproduce the pre-change query strings verbatim
// — the same property TestRender_EmptySelectorMatchesBaseline pins at the
// renderer, asserted here through the whole handler.
func TestGraph_UnfilteredQueriesCarryNoRequestMatchers(t *testing.T) {
	q, captured := recordingQuerier(t)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=1746442800&end=1746446400")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	seen := captured()
	assert.Equal(t, `last_over_time(kube_pod_info[1h])`, seen["kube_pod_info"])
	assert.Equal(t, `last_over_time(volume_labels[1h])`, seen["volume_labels"])
	assert.Contains(t, seen, "up", "the unfiltered empty build still classifies retention")
}

// A deployment that rebinds the zone label changes the MATCHER, never the
// request parameter name.
func TestGraph_ConfiguredLabelKeyUsedInMatcher(t *testing.T) {
	q, captured := recordingQuerier(t)
	s := newServerWithMocks(t, q, func(cfg *config.Config) {
		cfg.AZLabel = "topology_zone"
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=1746442800&end=1746446400&az=zone-a")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, `last_over_time(kube_pod_info{topology_zone="zone-a"}[1h])`, captured()["kube_pod_info"])
}

// A filtered request matching nothing is an empty 200, not outside_retention.
func TestGraph_FilteredEmptyResultReturns200(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=1746442800&end=1746446400&env=staging")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Clusters []string `json:"clusters"`
		Elements struct {
			Nodes []json.RawMessage `json:"nodes"`
			Edges []json.RawMessage `json:"edges"`
		} `json:"elements"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Empty(t, body.Clusters)
	assert.Empty(t, body.Elements.Nodes)
	assert.Empty(t, body.Elements.Edges)
}

func TestGraph_InvalidSelectorValuesRejected(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	for _, qs := range []string{
		"&env=prod%0A",
		"&prune=maybe",
	} {
		resp, err := http.Get(srv.URL + "/v1/graph?start=1746442800&end=1746446400" + qs)
		require.NoError(t, err)
		func() {
			defer resp.Body.Close()
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, "query %q", qs)
			var body errReason
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
			assert.Equal(t, "invalid_scope", body.Error.Reason)
		}()
	}
}

// The withdrawn parameters are ignored, not rejected.
func TestGraph_WithdrawnParametersReturn200(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, happyFixtures()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=1746442800&end=1746446400&name=web-1&root=test/uid-web-1&depth=9")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
