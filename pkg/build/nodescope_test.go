package build

import (
	"errors"
	"strings"
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

func TestNodeTargetsAreNodeScopedQueries(t *testing.T) {
	got := make([]promql.Query, 0, len(promql.NodeScopedQueries))
	for _, tg := range nodeTargets(&topologyVectors{}) {
		got = append(got, tg.query)
	}
	assert.Equal(t, promql.NodeScopedQueries, got)
}

// nodeScope mirrors an unscheduled pod's own discard: a pod with no `node`
// label contributes nothing, and a root is drawable even when no pod names it.
func TestNodeScope(t *testing.T) {
	pods := model.Vector{
		{Metric: model.Metric{"node": "n2"}},
		{Metric: model.Metric{"node": "n1"}},
		{Metric: model.Metric{"node": "n1"}}, // duplicate node, two pods scheduled
		{Metric: model.Metric{}},             // unscheduled: no node label at all
	}
	assert.Equal(t, []string{"n1", "n2", "n3"}, nodeScope(pods, []string{"n3", "", "n1"}))
	assert.Empty(t, nodeScope(nil, nil))
}

// Spec: "Node read is restricted to the pods' nodes and node roots".
func TestReadScopedNodes_RestrictedToPodNodesAndRoots(t *testing.T) {
	bind := func(pod string) *model.Sample {
		return planKSM("namespace", "shop", "pod", pod, "persistentvolumeclaim", "data-"+pod)
	}
	podInfo := func(pod, node string) *model.Sample {
		return planKSM("namespace", "shop", "pod", pod, "uid", "uid-"+pod, "node", node)
	}
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {bind("orders-0"), bind("orders-1")},
		promql.QPodInfo:     {podInfo("orders-0", "n1"), podInfo("orders-1", "n2")},
	})
	scope, err := graph.NewStorageScope(nil, nil, nil, []string{"n9"}, nil, nil, nil)
	require.NoError(t, err)

	_, err = New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, scope.Roots)
	require.NoError(t, err)

	for _, q := range promql.NodeScopedQueries {
		got := f.QueriesFor(q)
		require.Len(t, got, 1, "%s", q)
		assert.Contains(t, got[0], `node=~"n1|n2|n9"`, "%s: restricted to exactly the pods' nodes plus the root", q)
	}
}

// Spec: "Kubernetes node root with no mounting pod is still drawn".
func TestReadScopedNodes_RootWithoutPodsIsLoaded(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QNodeInfo: {planKSM("node", "n9")},
	})
	scope, err := graph.NewStorageScope(nil, nil, nil, []string{"n9"}, nil, nil, nil)
	require.NoError(t, err)

	g, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, scope.Roots)
	require.NoError(t, err)

	for _, q := range promql.NodeScopedQueries {
		got := f.QueriesFor(q)
		require.Len(t, got, 1, "%s", q)
		assert.Contains(t, got[0], `node="n9"`, "no binding, no pod: the root alone makes the scope")
	}
	assert.Empty(t, f.QueriesFor(promql.QPodInfo), "no pod query at all: no binding and no pod root")

	body := cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	drawn := false
	for _, n := range body.Elements.Nodes {
		drawn = drawn || n.Data.ID == "zone-a-prod-c1/n9"
	}
	assert.True(t, drawn, "the node root is drawn with no flow through it")
	assert.Empty(t, body.Elements.Edges)
}

func TestReadScopedNodes_EmptyScopeIssuesNothing(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QAggrStatus: {planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "aggr", "aggr1")},
	})
	scope, err := graph.NewStorageScope(nil, nil, nil, nil, []string{"aggr1"}, nil, nil)
	require.NoError(t, err)

	tp, err := readTopology(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(), Options{}, storageSel, storagePlan(scope.Roots))
	require.NoError(t, err)
	for _, q := range promql.NodeScopedQueries {
		assert.Empty(t, f.QueriesFor(q), "%s must not be issued at all", q)
		assert.NotContains(t, tp.RawSeriesCount, string(q), "an unread family has no tally entry")
	}
}

func TestReadScopedNodes_ChunkFailureFailsBuild(t *testing.T) {
	bind := func(pod string) *model.Sample {
		return planKSM("namespace", "shop", "pod", pod, "persistentvolumeclaim", "data-"+pod)
	}
	podInfo := func(pod, node string) *model.Sample {
		return planKSM("namespace", "shop", "pod", pod, "uid", "uid-"+pod, "node", node)
	}
	failN2 := func(_, query string) error {
		if strings.Contains(query, `node="n2"`) {
			return errors.New("upstream 5xx")
		}
		return nil
	}
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {bind("a"), bind("b"), bind("c")},
		promql.QPodInfo:     {podInfo("a", "n1"), podInfo("b", "n2"), podInfo("c", "n3")},
	})
	f.Fail = failN2

	scope, err := graph.NewStorageScope(nil, nil, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	_, err = New(f, Options{QoSScopeBatchBytes: 1}, nil, nil).
		BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, scope.Roots)
	require.Error(t, err, "a node chunk failure fails the build exactly as an unscoped kube_node_info failure does")
}
