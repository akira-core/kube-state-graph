package promql

import (
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seriesRecorder implements Metrics AND the optional SeriesMetrics upgrade.
type seriesRecorder struct {
	observed map[string][]int
	failures int
}

func (*seriesRecorder) ObserveQueryDuration(string, float64) {}
func (r *seriesRecorder) IncQueryFailure(string)             { r.failures++ }
func (r *seriesRecorder) ObserveQuerySeries(name string, n int) {
	r.observed[name] = append(r.observed[name], n)
}

// durationOnly implements Metrics without the upgrade — an embedder's recorder
// written before SeriesMetrics existed.
type durationOnly struct{}

func (durationOnly) ObserveQueryDuration(string, float64) {}
func (durationOnly) IncQueryFailure(string)               {}

func TestInstant_ObservesResultSeries(t *testing.T) {
	rec := &seriesRecorder{observed: map[string][]int{}}
	vec := model.Vector{
		&model.Sample{Metric: model.Metric{"pod": "a"}, Value: 1},
		&model.Sample{Metric: model.Metric{"pod": "b"}, Value: 1},
		&model.Sample{Metric: model.Metric{"pod": "c"}, Value: 1},
	}
	c := &Client{api: fakeAPI{val: vec}, metrics: rec}

	_, err := c.Instant(t.Context(), string(QPodInfo), "kube_pod_info", time.Unix(1000, 0))
	require.NoError(t, err)
	_, err = c.Instant(t.Context(), string(QPodInfo), "kube_pod_info", time.Unix(1000, 0))
	require.NoError(t, err)
	assert.Equal(t, map[string][]int{string(QPodInfo): {3, 3}}, rec.observed,
		"one observation per successful query, under the query name")

	failing := &Client{api: fakeAPI{err: assert.AnError}, metrics: rec}
	_, err = failing.Instant(t.Context(), string(QPodContainerInfo), "kube_pod_container_info", time.Unix(1000, 0))
	require.Error(t, err)
	assert.NotContains(t, rec.observed, string(QPodContainerInfo), "a failed query contributes no observation")
	assert.Equal(t, 1, rec.failures, "the failure is still counted by the required interface")
}

func TestInstant_SeriesMetricsIsOptional(t *testing.T) {
	for name, m := range map[string]Metrics{"without the upgrade": durationOnly{}, "nil": nil} {
		t.Run(name, func(t *testing.T) {
			c := &Client{api: fakeAPI{val: model.Vector{}}, metrics: m}
			_, err := c.Instant(t.Context(), string(QNodeInfo), "kube_node_info", time.Unix(1000, 0))
			require.NoError(t, err)
		})
	}
}
