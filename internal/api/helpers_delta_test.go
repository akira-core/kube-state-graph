package api

import (
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// A `last_over_time(qos_read_ops` needle and a delta needle for the same family
// no longer collide: each answers only its own query shape.
func TestFixtureSet_DeltaNeedleSplitsTheShapes(t *testing.T) {
	t.Parallel()

	measurement := model.Vector{&model.Sample{Metric: model.Metric{"volume": "v_a"}, Value: 100}}
	delta := model.Vector{&model.Sample{Metric: model.Metric{"volume": "v_a"}, Value: 50}}
	q := newMockQuerier(t, fixtureSet{
		"last_over_time(qos_read_ops": measurement,
		deltaNeedle("qos_read_ops"):   delta,
	})

	lastQ, ok := promql.RenderQoSVolumeScopedRollup(promql.QQoSReadOps, 5*time.Minute, promql.LabelKeys{}, promql.Selector{}, []string{"v_a"}, promql.RollupLast)
	require.True(t, ok)
	deltaQ, ok := promql.RenderQoSVolumeScopedDelta(promql.QQoSReadOps, 5*time.Minute, promql.LabelKeys{}, promql.Selector{}, []string{"v_a"})
	require.True(t, ok)

	got, err := q.Instant(t.Context(), "qos_read_ops", lastQ, time.Unix(1, 0))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.InDelta(t, 100, float64(got[0].Value), 1e-9, "the measurement needle answers the measurement query")

	got, err = q.Instant(t.Context(), "qos_read_ops", deltaQ, time.Unix(1, 0))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.InDelta(t, 50, float64(got[0].Value), 1e-9, "the delta needle answers the delta query")
}

func TestFixtureNeedleMatches(t *testing.T) {
	t.Parallel()

	const lastQ = `last_over_time(qos_read_ops{volume="v"}[5m])`
	const deltaQ = lastQ + ` - last_over_time(qos_read_ops{volume="v"}[5m] offset 5m)`

	assert.True(t, fixtureNeedleMatches("qos_read_ops", lastQ))
	assert.False(t, fixtureNeedleMatches("qos_read_ops", deltaQ), "an unmarked needle never matches a delta query")
	assert.True(t, fixtureNeedleMatches(deltaNeedle("qos_read_ops"), deltaQ))
	assert.False(t, fixtureNeedleMatches(deltaNeedle("qos_read_ops"), lastQ), "a delta needle never matches a measurement query")
	assert.False(t, fixtureNeedleMatches(deltaNeedle("qos_write_ops"), deltaQ), "the body still has to be a substring")
}
