package build

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// newRecordingEmptyQuerier returns a MockQuerier that answers every query with
// an empty vector and records the query NAMES it was asked for, so a test can
// assert which legs of the fan-out were issued.
func newRecordingEmptyQuerier(t *testing.T) (*promqlmocks.MockQuerier, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var names []string
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name, _ string, _ time.Time) (model.Vector, error) {
			mu.Lock()
			names = append(names, name)
			mu.Unlock()
			return model.Vector{}, nil
		}).
		Maybe()
	return q, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(names))
		copy(out, names)
		return out
	}
}

func hasServiceGraphLeg(names []string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, "traces_service_graph_") {
			return true
		}
	}
	return false
}

// TestBuild_FilteredEmptyTopology_SkipsServiceGraphRead: when a selector
// narrowed the topology to nothing, no service-graph series can survive
// admission (design D6 needs one resolved endpoint in loaded topology, and a
// service node can only come from ServicesByNameNS). The three
// traces_service_graph_* queries are the one leg no selector narrows, so
// issuing them would scan the whole estate to produce an empty response on
// every such request.
func TestBuild_FilteredEmptyTopology_SkipsServiceGraphRead(t *testing.T) {
	q, recorded := newRecordingEmptyQuerier(t)

	sel := promql.Selector{Namespace: []string{"does-not-exist"}}
	g, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd, sel)

	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Empty(t, g.NodesByID)
	assert.Empty(t, g.Edges)

	names := recorded()
	assert.False(t, hasServiceGraphLeg(names), "service-graph legs must be skipped, got %v", names)
	assert.NotContains(t, names, string(promql.QUpProbe), "a filtered build issues no up probe")
	assert.Contains(t, names, string(promql.QPodInfo), "the topology fan-out still runs")
}

// TestBuild_UnfilteredEmptyTopology_StillReadsServiceGraph: the skip is gated
// on `filtered`. An unfiltered empty topology is the outside-retention path,
// whose upstream traffic must stay byte-for-byte what it was.
func TestBuild_UnfilteredEmptyTopology_StillReadsServiceGraph(t *testing.T) {
	q, recorded := newRecordingEmptyQuerier(t)

	_, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd, promql.Selector{})

	require.NoError(t, err)
	names := recorded()
	assert.True(t, hasServiceGraphLeg(names), "unfiltered build still reads the service graph, got %v", names)
	assert.Contains(t, names, string(promql.QUpProbe))
}

// TestBuild_FilteredWithLoadedTopology_ReadsServiceGraph: the skip must not
// fire whenever the selector DID load workload — that is the ordinary
// filtered build.
func TestBuild_FilteredWithLoadedTopology_ReadsServiceGraph(t *testing.T) {
	var mu sync.Mutex
	var names []string
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name, _ string, _ time.Time) (model.Vector, error) {
			mu.Lock()
			names = append(names, name)
			mu.Unlock()
			if name == string(promql.QPodInfo) {
				return model.Vector{{Metric: model.Metric{
					"cluster": "alpha", "namespace": "shop", "pod": "checkout", "uid": "alpha-1",
				}, Value: 1}}, nil
			}
			return model.Vector{}, nil
		}).
		Maybe()

	sel := promql.Selector{Namespace: []string{"shop"}}
	g, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd, sel)

	require.NoError(t, err)
	require.NotNil(t, g)

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, hasServiceGraphLeg(names), "a filtered build with loaded pods still reads the service graph")
}
