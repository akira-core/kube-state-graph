package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ioRollupFixtures() map[string]model.Vector {
	return map[string]model.Vector{
		"kube_persistentvolumeclaim_info": {&model.Sample{Metric: model.Metric{
			"cluster": "c1", "namespace": "shop", "az": "zone-a", "env": "prod",
			"persistentvolumeclaim": "netapp-data", "volumename": "pvc-9f3a",
		}, Value: 1}},
		"volume_labels": {&model.Sample{Metric: model.Metric{
			"volume": "trident_pvc_9f3a", "cluster": "ontap-prod",
			"node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm-prod",
			"az": "zone-a", "env": "prod",
		}, Value: 1}},
	}
}

func TestStorageGraph_IORollupMaxChangesOnlyQoS(t *testing.T) {
	q, captured := recordingQuerierWith(t, ioRollupFixtures())
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{"aggr": {"aggr1"}, "io_rollup": {"max"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.NotContains(t, string(raw), "io_rollup")

	seen := captured()
	qos := []string{
		"qos_read_ops", "qos_write_ops", "qos_read_latency",
		"qos_write_latency", "qos_read_data", "qos_write_data",
	}
	for _, name := range qos {
		assert.Truef(t, strings.HasPrefix(seen[name], "max_over_time("), "%s: %s", name, seen[name])
	}
	for name, query := range seen {
		if strings.HasPrefix(query, "max_over_time(") || strings.HasPrefix(query, "avg_over_time(") {
			assert.Contains(t, qos, name, query)
		}
	}
	assert.True(t, strings.HasPrefix(seen["volume_labels"], "last_over_time("), seen["volume_labels"])
}

func TestStorageGraph_InvalidIORollup(t *testing.T) {
	q, captured := recordingQuerierWith(t, ioRollupFixtures())
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(storageGraphURL(srv.URL, url.Values{"aggr": {"aggr1"}, "io_rollup": {"p99"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	errField, _ := body["error"].(map[string]any)
	assert.Equal(t, "invalid_scope", errField["reason"])
	assert.Empty(t, captured())
}

func TestGraph_IORollupIsIgnored(t *testing.T) {
	q, captured := recordingQuerierWith(t, ioRollupFixtures())
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=1746442800&end=1746446400&az=zone-a&env=prod&io_rollup=max")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, strings.HasPrefix(captured()["qos_read_ops"], "last_over_time("), captured()["qos_read_ops"])
}

func TestK8sGraph_IORollupIsIgnored(t *testing.T) {
	q, captured := recordingQuerierWith(t, nil)
	s := newServerWithMocks(t, q, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(k8sGraphURL(srv.URL, url.Values{"node": {"worker-1"}, "io_rollup": {"p99"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	seen := captured()
	assert.NotContains(t, seen, "volume_labels")
	assert.NotContains(t, seen, "qos_read_ops")
	for name := range seen {
		assert.NotContains(t, name, "qos_")
	}
}
