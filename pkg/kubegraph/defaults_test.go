package kubegraph_test

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// countingQuerier counts upstream calls and records every evaluation instant.
// The outside-retention up{} probe is counted separately: it evaluates at the
// wall clock, not the request's end, and always bypasses the cache.
type countingQuerier struct {
	mu     sync.Mutex
	calls  int
	probes int
	ts     map[time.Time]bool
}

func (c *countingQuerier) Instant(_ context.Context, name, _ string, ts time.Time) (model.Vector, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if name == string(promql.QUpProbe) {
		c.probes++
		return model.Vector{}, nil
	}
	c.calls++
	if c.ts == nil {
		c.ts = map[time.Time]bool{}
	}
	c.ts[ts.UTC()] = true
	return model.Vector{}, nil
}

func (c *countingQuerier) snapshot() (int, map[time.Time]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.ts
}

var unalignedWindow = url.Values{
	"start": {"2026-05-02T12:04:17Z"},
	"end":   {"2026-05-02T12:19:47Z"},
}

func TestEngine_EndAlignDefaultsOn(t *testing.T) {
	cases := []struct {
		name  string
		align time.Duration
		want  string
	}{
		{"zero uses the default grid", 0, "2026-05-02T12:19:30Z"},
		{"negative disables alignment", -1, "2026-05-02T12:19:47Z"},
		{"positive sets the grid", time.Minute, "2026-05-02T12:19:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &countingQuerier{}
			eng := kubegraph.New(q, kubegraph.Options{APITimeout: time.Second, EndAlign: tc.align})
			_, err := eng.BuildFromValues(t.Context(), unalignedWindow)
			require.NoError(t, err)
			_, err = eng.BuildStorageFromValues(t.Context(), url.Values{
				"start": unalignedWindow["start"], "end": unalignedWindow["end"],
				"az": {"zone-a"}, "env": {"prod"},
			})
			require.NoError(t, err)

			want, _ := time.Parse(time.RFC3339, tc.want)
			_, seen := q.snapshot()
			assert.Equal(t, map[time.Time]bool{want: true}, seen,
				"every upstream query of both endpoints evaluates at the aligned end")
		})
	}
}

// A plain Querier handed to New is guarded by default: a second identical
// build is answered from the cache.
func TestEngine_PlainQuerierCachedByDefault(t *testing.T) {
	q := &countingQuerier{}
	eng := kubegraph.New(q, kubegraph.Options{APITimeout: time.Second})
	_, err := eng.BuildFromValues(t.Context(), unalignedWindow)
	require.NoError(t, err)
	first, _ := q.snapshot()
	require.Positive(t, first)

	_, err = eng.BuildFromValues(t.Context(), unalignedWindow)
	require.NoError(t, err)
	second, _ := q.snapshot()
	assert.Equal(t, first, second, "identical build served from the query cache")
}

func TestEngine_PlainQuerierCacheOptOut(t *testing.T) {
	q := &countingQuerier{}
	eng := kubegraph.New(q, kubegraph.Options{APITimeout: time.Second, QueryCacheMaxSeries: -1})
	_, err := eng.BuildFromValues(t.Context(), unalignedWindow)
	require.NoError(t, err)
	first, _ := q.snapshot()
	_, err = eng.BuildFromValues(t.Context(), unalignedWindow)
	require.NoError(t, err)
	second, _ := q.snapshot()
	assert.Equal(t, 2*first, second)
}

// Probe always reaches upstream, never the cache.
func TestEngine_ProbeNeverCached(t *testing.T) {
	q := &countingQuerier{}
	eng := kubegraph.New(q, kubegraph.Options{})
	require.NoError(t, eng.Probe(t.Context()))
	require.NoError(t, eng.Probe(t.Context()))
	q.mu.Lock()
	defer q.mu.Unlock()
	assert.Equal(t, 2, q.probes)
}

// A Router passed to New is used as-is, so its zone routing still reaches the
// builder, and it carries its own default guard.
func TestEngine_RouterKeepsRoutingAndDefaults(t *testing.T) {
	zoneA, zoneB := &countingQuerier{}, &countingQuerier{}
	fams := []promql.Family{promql.FamilyKSM, promql.FamilyKubelet, promql.FamilyServiceGraph, promql.FamilyProbe, promql.FamilyHarvest}
	table, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", fams, []string{"zone-a"}, "", ""),
		promql.NewBackend("zone-b", "http://vm-b:8428", fams, []string{"zone-b"}, "", ""),
	})
	require.NoError(t, err)
	byName := map[string]promql.Querier{"zone-a": zoneA, "zone-b": zoneB}
	router, err := promql.NewRouter(table, nil, func(b promql.Backend) (promql.Querier, error) {
		return byName[b.Name()], nil
	})
	require.NoError(t, err)

	eng := kubegraph.New(router, kubegraph.Options{APITimeout: time.Second})
	vals := url.Values{"start": unalignedWindow["start"], "end": unalignedWindow["end"], "az": {"zone-a"}}
	_, err = eng.BuildFromValues(t.Context(), vals)
	require.NoError(t, err)
	a1, _ := zoneA.snapshot()
	b1, _ := zoneB.snapshot()
	assert.Positive(t, a1)
	assert.Less(t, b1, a1, "zone-scoped families never reached zone-b")

	_, err = eng.BuildFromValues(t.Context(), vals)
	require.NoError(t, err)
	a2, _ := zoneA.snapshot()
	assert.Equal(t, a1, a2, "the router's default cache answered the repeat")
}
