package build

import (
	"context"
	"errors"
	"slices"
	"strings"
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

var storageSel = promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}

// delayedQuerier holds back every query whose rendered text contains delayIf,
// forcing out-of-order completion without depending on the scheduler.
type delayedQuerier struct {
	promql.Querier
	delayIf string
	delay   time.Duration
}

func (d delayedQuerier) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	if strings.Contains(query, d.delayIf) {
		time.Sleep(d.delay)
	}
	return d.Querier.Instant(ctx, name, query, ts)
}

// scopedPodVectors drives readScopedPods directly with the binding family
// already landed, so a test can assert on the merged vectors themselves.
func scopedPodVectors(t *testing.T, q promql.Querier, opts Options, roots []graph.PodRef, bindings model.Vector) (*topologyVectors, error) {
	t.Helper()
	v := &topologyVectors{PVC: bindings}
	var mu sync.Mutex
	done := make(chan struct{})
	close(done)
	var recovered []podSeriesKey
	err := readScopedPods(t.Context(), q, time.Minute, time.Unix(1, 0).UTC(), opts, promql.Selector{}, roots, nil, v, &mu, done, done, done, &recovered)
	return v, err
}

func podBinding(pod string) *model.Sample {
	return &model.Sample{Metric: model.Metric{
		"cluster": "c", "namespace": "shop", "pod": model.LabelValue(pod), "persistentvolumeclaim": "data-" + model.LabelValue(pod),
	}, Value: 1}
}

// The two lists must agree: podTargets decides which vectors the wave fills,
// promql.PodScopedQueries decides which families RenderScoped accepts and which
// the plan withholds from the first wave.
func TestPodTargetsArePodScopedQueries(t *testing.T) {
	got := make([]promql.Query, 0, len(promql.PodScopedQueries))
	for _, tg := range podTargets(&topologyVectors{}) {
		got = append(got, tg.query)
	}
	assert.Equal(t, promql.PodScopedQueries, got)
}

// The scope mirrors the binding reader's discard: a series naming no claim
// binds nothing, so its pod does not enter the scope. Each pod keeps the
// namespace its binding row carries.
func TestBindingPodRefs(t *testing.T) {
	bindings := model.Vector{
		{Metric: model.Metric{"namespace": "shop", "pod": "b", "persistentvolumeclaim": "c1"}},
		{Metric: model.Metric{"namespace": "platform", "pod": "a", "claim_name": "c2"}},
		{Metric: model.Metric{"namespace": "shop", "pod": "a", "persistentvolumeclaim": "c3"}},
		{Metric: model.Metric{"namespace": "shop", "pod": "unbound", "volume": "data"}},
		{Metric: model.Metric{"namespace": "shop", "persistentvolumeclaim": "c4"}},
	}
	assert.Equal(t, []graph.PodRef{
		{Namespace: "shop", Name: "b"},
		{Namespace: "platform", Name: "a"},
		{Namespace: "shop", Name: "a"},
	}, bindingPodRefs(bindings))
	assert.Empty(t, bindingPodRefs(nil))
}

// Refs group by namespace, sorted and de-duplicated, independent of input
// order; a nameless ref is dropped.
func TestGroupPodRefs(t *testing.T) {
	in := []graph.PodRef{
		{Namespace: "shop", Name: "web-0"},
		{Namespace: "platform", Name: "orders-0"},
		{Namespace: "shop", Name: "orders-0"},
		{Namespace: "shop", Name: "web-0"},
		{Namespace: "shop", Name: ""},
	}
	snapshot := slices.Clone(in)
	namespaces, byNS := groupPodRefs(in)
	assert.Equal(t, []string{"platform", "shop"}, namespaces)
	assert.Equal(t, map[string][]string{"platform": {"orders-0"}, "shop": {"orders-0", "web-0"}}, byNS)
	assert.Equal(t, snapshot, in, "the input is not mutated")

	empty, _ := groupPodRefs([]graph.PodRef{{Namespace: "shop"}})
	assert.Empty(t, empty)
}

// A pod root's pod read is the root plus the other mounters of its claims.
// catalog-0 mounts a different claim, so the seed never tracks it.
func TestReadScopedPods_RestrictedToMountingPodsAndRoots(t *testing.T) {
	bind := func(pod, claim string) *model.Sample {
		return planKSM("namespace", "shop", "pod", pod, "persistentvolumeclaim", claim)
	}
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {
			bind("web-0", "data-web"),
			bind("share-0", "data-web"),
			bind("catalog-0", "data-catalog"),
		},
	})
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootPod, []string{"shop/web-0"})
	require.NoError(t, err)

	_, err = New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, scope.Roots)
	require.NoError(t, err)

	for _, q := range promql.PodScopedQueries {
		assert.Equal(t, []string{
			`last_over_time(` + string(q) + `{az="zone-a",env="prod",namespace="shop",pod=~"share-0|web-0"}[1m])`,
		}, f.QueriesFor(q), "request matchers first, then the (namespace, pod) scope")
	}

	// The wave waits for the binding family its scope is computed from.
	issued := f.Issued()
	bindingAt := slices.IndexFunc(issued, func(is promqlfake.Issued) bool { return is.Name == string(promql.QPVCBindings) })
	podAt := slices.IndexFunc(issued, func(is promqlfake.Issued) bool { return is.Name == string(promql.QPodInfo) })
	require.GreaterOrEqual(t, bindingAt, 0)
	assert.Greater(t, podAt, bindingAt, "the scoped pod read waits for kube_pod_spec_volumes_persistentvolumeclaims_info")
}

// Spec: "Empty scope issues no pod query".
func TestReadScopedPods_EmptyScopeIssuesNothing(t *testing.T) {
	fixtures := map[promql.Query]model.Vector{
		promql.QAggrStatus: {planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "aggr", "aggr1")},
	}
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootAggr, []string{"aggr1"})
	require.NoError(t, err)

	f := promqlfake.New(fixtures)
	tp, err := readTopology(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(), Options{}, storageSel, storagePlan(scope.Roots))
	require.NoError(t, err)
	for _, q := range promql.PodScopedQueries {
		assert.Empty(t, f.QueriesFor(q), "%s must not be issued at all", q)
		assert.NotContains(t, tp.RawSeriesCount, string(q), "an unread family has no tally entry")
	}

	g, err := New(promqlfake.New(fixtures), Options{}, nil, nil).
		BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, scope.Roots)
	require.NoError(t, err)
	body := cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	var real []string
	for _, n := range body.Elements.Nodes {
		switch n.Data.Type {
		case string(graph.NodeTypeNetAppAggr), string(graph.NodeTypeNetAppNode):
			real = append(real, n.Data.ID)
		case "storage-cluster":
		default:
			t.Errorf("unexpected node %s (%s)", n.Data.ID, n.Data.Type)
		}
	}
	assert.ElementsMatch(t, []string{"netapp/ontap-prod/aggr/aggr1", "netapp/ontap-prod/ontap-prod-01"}, real)
}

// Spec: "Claimless pod root is still drawn".
func TestReadScopedPods_ClaimlessRootIsLoaded(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPodInfo:  {planKSM("namespace", "shop", "pod", "web-0", "uid", "uid-w0", "node", "worker-1")},
		promql.QNodeInfo: {planKSM("node", "worker-1")},
	})
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootPod, []string{"shop/web-0"})
	require.NoError(t, err)

	g, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, scope.Roots)
	require.NoError(t, err)

	assert.Equal(t, []string{`last_over_time(kube_pod_info{az="zone-a",env="prod",namespace="shop",pod="web-0"}[1m])`},
		f.QueriesFor(promql.QPodInfo), "no binding at all: the root alone makes the scope")
	body := cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	drawn := false
	for _, n := range body.Elements.Nodes {
		drawn = drawn || n.Data.ID == "zone-a-prod-c1/uid-w0"
	}
	assert.True(t, drawn, "the root is drawn with no flow through it")
	assert.Empty(t, body.Elements.Edges)
}

// Chunks merge by index, never completion order, and every chunk is issued
// under the bare family name — so a chunked scope reaches self-metrics as one
// observation per chunk under one label value per family.
func TestReadScopedPods_ChunksMergeInChunkOrder(t *testing.T) {
	pod := func(name string) *model.Sample {
		return &model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "shop", "pod": model.LabelValue(name), "uid": "uid-" + model.LabelValue(name)}, Value: 1}
	}
	f := promqlfake.New(map[promql.Query]model.Vector{promql.QPodInfo: {pod("a"), pod("b"), pod("c")}})
	// Make the FIRST chunk answer last.
	slow := delayedQuerier{Querier: f, delayIf: `pod="a"`, delay: 40 * time.Millisecond}

	// A budget below one name's rendered length forces one chunk per name.
	v, err := scopedPodVectors(t, slow, Options{QoSScopeBatchBytes: 1}, nil,
		model.Vector{podBinding("c"), podBinding("a"), podBinding("b")})
	require.NoError(t, err)

	order := make([]string, 0, len(v.Pod))
	for _, s := range v.Pod {
		order = append(order, string(s.Metric["pod"]))
	}
	assert.Equal(t, []string{"a", "b", "c"}, order, "the merged vector follows chunk order, not completion order")
	assert.True(t, v.ScopeIssued[promql.QPodInfo])
	assert.True(t, v.ScopeIssued[promql.QPodOwner])
	assert.Len(t, f.QueriesFor(promql.QPodInfo), 3, "one query per chunk")
	assert.Len(t, f.QueriesFor(promql.QPodOwner), 3, "one query per chunk")
	for _, is := range f.Issued() {
		assert.Contains(t, []string{string(promql.QPodInfo), string(promql.QPodOwner)}, is.Name,
			"every chunk is issued under its bare family name")
	}
}

// Spec: "A pod chunk failure fails the build". Unlike a QoS chunk, a pod chunk
// fails closed: a missing chunk would be a smaller, plausible, wrong body.
func TestReadScopedPods_ChunkFailureFailsBuild(t *testing.T) {
	failB := func(_, query string) error {
		if strings.Contains(query, `pod="b"`) {
			return errors.New("upstream 5xx")
		}
		return nil
	}

	f := promqlfake.New(nil)
	f.Fail = failB
	_, err := scopedPodVectors(t, f, Options{QoSScopeBatchBytes: 1}, nil,
		model.Vector{podBinding("a"), podBinding("b"), podBinding("c")})
	require.Error(t, err, "the second of three chunks failed")

	through := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {podBinding("a"), podBinding("b"), podBinding("c")},
	})
	through.Fail = failB
	_, err = readTopology(t.Context(), through, time.Minute, time.Unix(1, 0).UTC(),
		Options{QoSScopeBatchBytes: 1}, promql.Selector{}, storagePlan(graph.StorageRoots{}))
	require.Error(t, err, "the failure reaches the build exactly as an unscoped kube_pod_info error does")
	assert.Contains(t, err.Error(), "upstream 5xx")
}
