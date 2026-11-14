package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
)

// Spec: "Non-finite upstream values are dropped". A NaN / +Inf Harvest gauge of
// a rooted aggregate used to reach json.Marshal and turn a valid build into a
// 500 encode error.
func TestStorageGraph_NonFiniteUpstreamValueIsDropped(t *testing.T) {
	aggr := model.Metric{"cluster": "ontap-a", "aggr": "aggr1", "node": "ontap-a-01"}
	fixtures := fixtureSet{
		"aggr_space_used":  model.Vector{&model.Sample{Metric: aggr, Value: model.SampleValue(math.NaN())}},
		"aggr_space_total": model.Vector{&model.Sample{Metric: aggr, Value: model.SampleValue(math.Inf(1))}},
		"aggr_new_status":  model.Vector{&model.Sample{Metric: aggr, Value: 1}},
		"node_cpu_busy":    model.Vector{&model.Sample{Metric: model.Metric{"cluster": "ontap-a", "node": "ontap-a-01"}, Value: model.SampleValue(math.NaN())}},
		"node_new_status":  model.Vector{&model.Sample{Metric: model.Metric{"cluster": "ontap-a", "node": "ontap-a-01"}, Value: 1}},
	}
	srv := httptest.NewServer(newServerWithMocks(t, newMockQuerier(t, fixtures), nil).Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/storage-graph?start=1746442800&end=1746446400&az=zone-a&env=prod&aggr=aggr1")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "a non-finite sample is not an encode error")

	var body cytoscape.Body
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	var sawAggr bool
	for _, n := range body.Elements.Nodes {
		if n.Data.Type != "netapp-aggr" {
			continue
		}
		sawAggr = true
		assert.Nil(t, n.Data.Usage, "both usage figures were non-finite, so no usage is drawn")
	}
	assert.True(t, sawAggr, "the aggregate itself is still drawn")
}
