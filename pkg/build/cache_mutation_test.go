package build

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// snapshottingQuerier records, for every sample it hands out, the sample's
// label fingerprint and value AT HAND-OUT time. The query-result cache shares
// these very *model.Sample pointers with every later reader, so any reader
// writing through a sample would show up as a drifted snapshot.
type snapshottingQuerier struct {
	inner promql.Querier
	mu    sync.Mutex
	seen  map[*model.Sample]sampleSnap
}

type sampleSnap struct {
	fp    model.Fingerprint
	value model.SampleValue
}

func (s *snapshottingQuerier) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	v, err := s.inner.Instant(ctx, name, query, ts)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, smp := range v {
		s.seen[smp] = sampleSnap{fp: smp.Metric.Fingerprint(), value: smp.Value}
	}
	return v, err
}

func (s *snapshottingQuerier) assertUnchanged(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.seen, "a vacuous run would prove nothing")
	for smp, snap := range s.seen {
		assert.Equal(t, snap.fp, smp.Metric.Fingerprint(), "a reader mutated a shared sample's labels")
		// Bit-exact: any write through the sample is a mutation, however small.
		assert.Equal(t, math.Float64bits(float64(snap.value)), math.Float64bits(float64(smp.Value)),
			"a reader mutated a shared sample's value")
	}
}

func singleRouterOver(t *testing.T, q promql.Querier, opts ...promql.RouterOption) *promql.Router {
	t.Helper()
	tbl, err := promql.SingleBackendTable("http://vm:8428", "", "")
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, nil, func(promql.Backend) (promql.Querier, error) { return q, nil }, opts...)
	require.NoError(t, err)
	return r
}

// Cached samples are shared read-only by every build that hits them. Running
// every build shape twice through a caching Router must leave each shared
// sample untouched, and produce bodies byte-identical to an uncached run.
func TestCachedSamplesNotMutated(t *testing.T) {
	end := time.Unix(1, 0).UTC()
	sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}
	aggrScope, err := graph.NewStorageScope(nil, nil, nil, nil, []string{"aggr1"}, nil, nil, nil)
	require.NoError(t, err)
	podScope, err := graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/orders-0"}, nil)
	require.NoError(t, err)
	appScope, err := graph.NewStorageScope(nil, nil, nil, nil, nil, nil, nil, []string{"beta"})
	require.NoError(t, err)

	run := func(q promql.Querier) []string {
		b := New(q, Options{}, nil, nil)
		bodies := make([]string, 0, 6)
		g, err := b.Build(t.Context(), time.Minute, end, promql.Selector{})
		require.NoError(t, err)
		bodies = append(bodies, planBodyJSON(t, cytoscape.Serialise(g, graph.Project(g, graph.Scope{}))))
		g, err = b.Build(t.Context(), time.Minute, end, sel)
		require.NoError(t, err)
		bodies = append(bodies, planBodyJSON(t, cytoscape.Serialise(g, graph.Project(g, graph.Scope{Inventory: true}))))
		for _, sc := range []graph.StorageScope{{}, aggrScope, podScope, appScope} {
			g, err := b.BuildStorage(t.Context(), time.Minute, end, sel, sc.Roots)
			require.NoError(t, err)
			bodies = append(bodies, planBodyJSON(t, cytoscape.Serialise(g, graph.ProjectStorage(g, sc))))
		}
		return bodies
	}

	uncached := run(singleRouterOver(t, promqlfake.New(planEstate()), promql.WithQueryCache(0, 0)))

	snap := &snapshottingQuerier{inner: promqlfake.New(planEstate()), seen: map[*model.Sample]sampleSnap{}}
	cached := singleRouterOver(t, snap)
	first := run(cached)
	second := run(cached) // every query of this pass is a cache hit
	snap.assertUnchanged(t)

	assert.Equal(t, uncached, first)
	assert.Equal(t, uncached, second)
}
