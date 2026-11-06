package promql

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two rollups of one window render two different query strings, and the
// query-result cache keys on the rendered string, so a request for one rollup
// is never served the figures cached for another. The repeat of each rollup is
// a cache hit, which is what proves the cache was on while the two stayed apart.
func TestGuard_RollupsOfOneWindowAreCachedApart(t *testing.T) {
	t.Parallel()

	render := func(r Rollup) string {
		q, ok := RenderQoSVolumeScopedRollup(QQoSReadOps, 5*time.Minute, LabelKeys{}, Selector{}, []string{"v_a"}, r)
		require.True(t, ok)
		return q
	}
	var calls atomic.Int64
	backend := querierFunc(func(_ context.Context, _, query string, _ time.Time) (model.Vector, error) {
		calls.Add(1)
		v := model.SampleValue(200) // last: the sample at the window end
		if strings.HasPrefix(query, "max_over_time(") {
			v = 400 // max: the series' own peak
		}
		return model.Vector{&model.Sample{Metric: model.Metric{"volume": "v_a"}, Value: v}}, nil
	})
	q := Guard(backend)

	for range 2 {
		for _, tc := range []struct {
			rollup Rollup
			want   float64
		}{
			{rollup: RollupMax, want: 400},
			{rollup: RollupLast, want: 200},
		} {
			got, err := q.Instant(t.Context(), string(QQoSReadOps), render(tc.rollup), ts0)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.InDelta(t, tc.want, float64(got[0].Value), 1e-9, "rollup %q", tc.rollup)
		}
	}
	assert.EqualValues(t, 2, calls.Load(), "one upstream call per rollup; each repeat is a cache hit")
}

// A delta query and the last measurement query of the same scope and instant
// render two different strings, so they are two cache entries; each repeat is
// a hit.
func TestGuard_DeltaAndMeasurementAreCachedApart(t *testing.T) {
	t.Parallel()

	volumes := []string{"v_a"}
	last, ok := RenderQoSVolumeScopedRollup(QQoSReadOps, 5*time.Minute, LabelKeys{}, Selector{}, volumes, RollupLast)
	require.True(t, ok)
	delta, ok := RenderQoSVolumeScopedDelta(QQoSReadOps, 5*time.Minute, LabelKeys{}, Selector{}, volumes)
	require.True(t, ok)
	require.NotEqual(t, last, delta)

	var calls atomic.Int64
	backend := querierFunc(func(_ context.Context, _, query string, _ time.Time) (model.Vector, error) {
		calls.Add(1)
		v := model.SampleValue(100) // the sample at the window end
		if strings.Contains(query, " offset ") {
			v = 50 // end minus start
		}
		return model.Vector{&model.Sample{Metric: model.Metric{"volume": "v_a"}, Value: v}}, nil
	})
	q := Guard(backend)

	for range 2 {
		for _, tc := range []struct {
			query string
			want  float64
		}{
			{query: delta, want: 50},
			{query: last, want: 100},
		} {
			got, err := q.Instant(t.Context(), string(QQoSReadOps), tc.query, ts0)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.InDelta(t, tc.want, float64(got[0].Value), 1e-9, "query %q", tc.query)
		}
	}
	assert.EqualValues(t, 2, calls.Load(), "one upstream call per query shape; each repeat is a cache hit")
}
