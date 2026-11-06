package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

type issuedQuery struct{ name, query string }

// ioDeltaQuerier answers by family name, the measurement fixture to a plain
// query and the delta fixture to an end-minus-start one, and records every
// query it saw.
func ioDeltaQuerier(t *testing.T, measurements, deltas map[string]model.Vector) (*promqlmocks.MockQuerier, func() []issuedQuery) {
	t.Helper()
	var mu sync.Mutex
	var seen []issuedQuery
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name, query string, _ time.Time) (model.Vector, error) {
			mu.Lock()
			seen = append(seen, issuedQuery{name, query})
			mu.Unlock()
			src := measurements
			if isDeltaQuery(query) {
				src = deltas
			}
			if out := src[name]; out != nil {
				return out, nil
			}
			return model.Vector{}, nil
		}).Maybe()
	return q, func() []issuedQuery {
		mu.Lock()
		defer mu.Unlock()
		return append([]issuedQuery(nil), seen...)
	}
}

func qosRow(v float64) model.Vector {
	return model.Vector{&model.Sample{Metric: model.Metric{
		"volume": "trident_pvc_9f3a", "cluster": "ontap-prod", "svm": "svm-prod",
		"az": "zone-a", "env": "prod",
	}, Value: model.SampleValue(v)}}
}

// ioDeltaEstate is one claim, bound to one pod, on one aggregate, with a
// measurement and a start value for ops, write data and read latency.
func ioDeltaEstate() (measurements, deltas map[string]model.Vector) {
	measurements = map[string]model.Vector{
		"kube_persistentvolumeclaim_info": {&model.Sample{Metric: model.Metric{
			"cluster": "c1", "namespace": "shop", "az": "zone-a", "env": "prod",
			"persistentvolumeclaim": "netapp-data", "volumename": "pvc-9f3a",
		}, Value: 1}},
		"volume_labels": {&model.Sample{Metric: model.Metric{
			"volume": "trident_pvc_9f3a", "cluster": "ontap-prod",
			"node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm-prod",
			"az": "zone-a", "env": "prod",
		}, Value: 1}},
		"kube_pod_persistentvolumeclaim": nil,
		"qos_read_ops":                   qosRow(100),
		"qos_write_data":                 qosRow(1_000_000),
		"qos_read_latency":               qosRow(650),
	}
	deltas = map[string]model.Vector{
		"qos_read_ops":     qosRow(50),
		"qos_write_data":   qosRow(-3_000_000),
		"qos_read_latency": qosRow(250),
	}
	return measurements, deltas
}

func getStorage(t *testing.T, srv *httptest.Server, extra url.Values) (int, []byte) {
	t.Helper()
	base := url.Values{"pvc": {"shop/netapp-data"}}
	for k, v := range extra {
		base[k] = v
	}
	resp, err := http.Get(storageGraphURL(srv.URL, base))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

func metricsOf(t *testing.T, raw []byte, tier string) map[string]float64 {
	t.Helper()
	var body cytoscape.Body
	require.NoError(t, json.Unmarshal(raw, &body))
	for _, e := range body.Elements.Edges {
		if e.Data.Labels["tier"] != tier || e.Data.Metrics == nil {
			continue
		}
		b, err := json.Marshal(e.Data.Metrics)
		require.NoError(t, err)
		var m map[string]float64
		require.NoError(t, json.Unmarshal(b, &m))
		return m
	}
	return nil
}

func deltaQueriesOf(seen []issuedQuery) []issuedQuery {
	var out []issuedQuery
	for _, s := range seen {
		if isDeltaQuery(s.query) {
			out = append(out, s)
		}
	}
	return out
}

func TestStorageGraph_IODeltaCarriesChangeFields(t *testing.T) {
	m, d := ioDeltaEstate()
	q, issued := ioDeltaQuerier(t, m, d)
	srv := httptest.NewServer(newServerWithMocks(t, q, nil).Handler())
	t.Cleanup(srv.Close)

	status, raw := getStorage(t, srv, url.Values{"io_delta": {"true"}})
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.NotContains(t, string(raw), `"io_delta"`, "the switch is not echoed")

	deltas := deltaQueriesOf(issued())
	names := make([]string, 0, len(deltas))
	for _, s := range deltas {
		names = append(names, s.name)
	}
	assert.ElementsMatch(t, []string{
		"qos_read_ops", "qos_write_ops", "qos_read_latency",
		"qos_write_latency", "qos_read_data", "qos_write_data",
	}, names, "one delta query per QoS family, and nothing else carries the offset shape")

	claim := metricsOf(t, raw, "svm-pvc")
	require.NotNil(t, claim, "the claim-level edge carries metrics")
	assert.InDelta(t, 100, claim["read_ops"], 1e-9)
	assert.InDelta(t, 50, claim["read_ops_delta"], 1e-9)
	assert.InDelta(t, 650, claim["read_latency_us"], 1e-9)
	assert.InDelta(t, 250, claim["read_latency_us_delta"], 1e-9)
	assert.NotContains(t, claim, "write_ops_delta", "no start value: absent, never 0")
	assert.NotContains(t, claim, "write_ops")

	nodeAggr := metricsOf(t, raw, "node-aggr")
	require.NotNil(t, nodeAggr, "the unmounted root claim's sink chain carries the measurement up to node-aggr")
	assert.InDelta(t, 50, nodeAggr["read_ops_delta"], 1e-9)
	assert.NotContains(t, nodeAggr, "read_latency_us_delta", "latency change is claim-level only")
}

func TestStorageGraph_IODeltaFalseIsTheDefaultBody(t *testing.T) {
	m, d := ioDeltaEstate()
	serve := func(extra url.Values) (int, []byte, []issuedQuery) {
		q, issued := ioDeltaQuerier(t, m, d)
		srv := httptest.NewServer(newServerWithMocks(t, q, nil).Handler())
		t.Cleanup(srv.Close)
		status, raw := getStorage(t, srv, extra)
		return status, raw, issued()
	}
	statusDefault, defaultBody, defaultQueries := serve(nil)
	require.Equal(t, http.StatusOK, statusDefault)
	for _, extra := range []url.Values{{"io_delta": {"false"}}, {"io_delta": {""}}} {
		status, raw, queries := serve(extra)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, string(defaultBody), string(raw))
		assert.Empty(t, deltaQueriesOf(queries))
		assert.Len(t, queries, len(defaultQueries))
	}
	assert.NotContains(t, string(defaultBody), "_delta")
}

func TestStorageGraph_IODeltaOnlyAddsFields(t *testing.T) {
	m, d := ioDeltaEstate()
	run := func(extra url.Values) []byte {
		q, _ := ioDeltaQuerier(t, m, d)
		srv := httptest.NewServer(newServerWithMocks(t, q, nil).Handler())
		t.Cleanup(srv.Close)
		status, raw := getStorage(t, srv, extra)
		require.Equal(t, http.StatusOK, status)
		return raw
	}
	plain := run(nil)
	withDelta := run(url.Values{"io_delta": {"true"}})

	var a, b cytoscape.Body
	require.NoError(t, json.Unmarshal(plain, &a))
	require.NoError(t, json.Unmarshal(withDelta, &b))
	require.Len(t, b.Elements.Nodes, len(a.Elements.Nodes))
	require.Len(t, b.Elements.Edges, len(a.Elements.Edges))
	for i := range a.Elements.Nodes {
		assert.Equal(t, a.Elements.Nodes[i], b.Elements.Nodes[i])
	}
	for i := range a.Elements.Edges {
		ea, eb := a.Elements.Edges[i].Data, b.Elements.Edges[i].Data
		assert.Equal(t, ea.ID, eb.ID)
		assert.Equal(t, ea.Labels, eb.Labels)
		var ma, mb map[string]float64
		if ea.Metrics != nil {
			raw, _ := json.Marshal(ea.Metrics)
			require.NoError(t, json.Unmarshal(raw, &ma))
		}
		if eb.Metrics != nil {
			raw, _ := json.Marshal(eb.Metrics)
			require.NoError(t, json.Unmarshal(raw, &mb))
		}
		for k, v := range ma {
			assert.InDelta(t, v, mb[k], 1e-9, "%s keeps its value", k)
		}
		for k := range mb {
			if _, ok := ma[k]; !ok {
				assert.True(t, strings.HasSuffix(k, "_delta"), "only change fields are added, got %s", k)
			}
		}
	}
}

func TestStorageGraph_InvalidIODelta(t *testing.T) {
	for _, extra := range []url.Values{
		{"io_delta": {"yes"}}, {"io_delta": {"1"}}, {"io_delta": {"TRUE"}},
		{"io_delta": {"true", "false"}},
	} {
		m, d := ioDeltaEstate()
		q, issued := ioDeltaQuerier(t, m, d)
		srv := httptest.NewServer(newServerWithMocks(t, q, nil).Handler())
		t.Cleanup(srv.Close)
		status, raw := getStorage(t, srv, extra)
		require.Equal(t, http.StatusBadRequest, status, "%v", extra)

		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		errField, _ := body["error"].(map[string]any)
		assert.Equal(t, "invalid_scope", errField["reason"])
		assert.Contains(t, errField["message"], "io_delta")
		assert.Empty(t, issued(), "no upstream query")
	}
}

func TestStorageGraph_FailedDeltaQueryIs502(t *testing.T) {
	m, d := ioDeltaEstate()
	inner, _ := ioDeltaQuerier(t, m, d)
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(ctx context.Context, name, query string, at time.Time) (model.Vector, error) {
			if name == "qos_read_ops" && isDeltaQuery(query) {
				return nil, assert.AnError
			}
			return inner.Instant(ctx, name, query, at)
		}).Maybe()
	srv := httptest.NewServer(newServerWithMocks(t, q, nil).Handler())
	t.Cleanup(srv.Close)

	status, raw := getStorage(t, srv, url.Values{"io_delta": {"true"}})
	require.Equal(t, http.StatusBadGateway, status, string(raw))
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	errField, _ := body["error"].(map[string]any)
	assert.Equal(t, "upstream", errField["reason"])
	assert.Equal(t, "upstream query failed: qos_read_ops", errField["message"])
}

func TestGraphAndK8sGraph_IgnoreIODelta(t *testing.T) {
	m, d := ioDeltaEstate()
	q, issued := ioDeltaQuerier(t, m, d)
	srv := httptest.NewServer(newServerWithMocks(t, q, nil).Handler())
	t.Cleanup(srv.Close)

	for _, u := range []string{
		srv.URL + "/v1/graph?start=1746442800&end=1746446400&az=zone-a&env=prod&io_delta=true",
		srv.URL + "/v1/graph?start=1746442800&end=1746446400&az=zone-a&env=prod&io_delta=yes",
		k8sGraphURL(srv.URL, url.Values{"node": {"worker-1"}, "io_delta": {"yes"}}),
		k8sGraphURL(srv.URL, url.Values{"node": {"worker-1"}, "io_delta": {"true"}}),
	} {
		resp, err := http.Get(u)
		require.NoError(t, err)
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", u, raw)
		assert.NotContains(t, string(raw), "_delta", u)
	}
	assert.Empty(t, deltaQueriesOf(issued()), "neither endpoint issues a delta query")
}
