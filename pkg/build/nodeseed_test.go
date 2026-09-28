package build

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func TestNodeSeed_ReadsPodsByNode(t *testing.T) {
	pod := func(name, node, uid string) *model.Sample {
		return planKSM("namespace", "shop", "pod", name, "uid", uid, "node", node)
	}
	bind := func(name, claim string) *model.Sample {
		return planKSM("namespace", "shop", "pod", name, "persistentvolumeclaim", claim)
	}
	fx := map[promql.Query]model.Vector{
		promql.QPodInfo: {
			pod("orders-0", "worker-1", "uid-o"),
			pod("web-0", "worker-1", "uid-w"),
			pod("other-0", "worker-2", "uid-x"),
		},
		promql.QPVCBindings: {
			bind("orders-0", "orders-data"),
			bind("web-0", "web-data"),
			bind("other-0", "other-data"),
		},
	}
	q := promqlfake.New(fx)
	_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootNode, []string{"worker-1"}).Roots)
	require.NoError(t, err)

	info := q.QueriesFor(promql.QPodInfo)
	require.GreaterOrEqual(t, len(info), 2)
	assert.Contains(t, info[0], `node="worker-1"`, "the first pod read is the node seed")
	assert.NotContains(t, info[0], `pod=`)
	assert.Contains(t, info[1], `pod=~"orders-0|web-0"`, "incarnation completion drops the node matcher")
	assert.NotContains(t, info[1], `node=`)

	var bindings []string
	for _, query := range q.QueriesFor(promql.QPVCBindings) {
		if strings.Contains(query, `pod=~"`) || strings.Contains(query, `pod="`) {
			bindings = append(bindings, query)
		}
	}
	require.NotEmpty(t, bindings)
	assert.Contains(t, bindings[0], "orders-0")
	assert.Contains(t, bindings[0], "web-0")
	assert.NotContains(t, bindings[0], "other-0", "a pod on another node is not fetched")
}

func TestNodeSeed_RescheduledPodFollowsItsNewestIncarnation(t *testing.T) {
	old := planKSM("namespace", "shop", "pod", "db-0", "uid", "uid-old", "node", "worker-1")
	newest := planKSM("namespace", "shop", "pod", "db-0", "uid", "uid-new", "node", "worker-2")
	newest.Timestamp = 5_000
	stay := planKSM("namespace", "shop", "pod", "stay-0", "uid", "uid-stay", "node", "worker-1")
	fx := planEstate()
	fx[promql.QPodInfo] = append(fx[promql.QPodInfo], old, newest, stay)
	fx[promql.QPVCBindings] = append(fx[promql.QPVCBindings],
		planKSM("namespace", "shop", "pod", "db-0", "persistentvolumeclaim", "db-data"),
		planKSM("namespace", "shop", "pod", "stay-0", "persistentvolumeclaim", "stay-data"))
	fx[promql.QPVCInfo] = append(fx[promql.QPVCInfo],
		planKSM("namespace", "shop", "persistentvolumeclaim", "db-data", "volumename", "pvc-db"),
		planKSM("namespace", "shop", "persistentvolumeclaim", "stay-data", "volumename", "pvc-stay"))

	body := assertStorageParity(t, fx, parityRoot{kind: graph.StorageRootNode, values: []string{"worker-1"}})
	assert.False(t, parityHasID(body, "zone-a-prod-c1/uid-old"))
	assert.False(t, parityHasID(body, "zone-a-prod-c1/uid-new"), "db-0's newest incarnation is on worker-2")

	q := promqlfake.New(fx)
	_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootNode, []string{"worker-1"}).Roots)
	require.NoError(t, err)
	var sawStay bool
	for _, query := range q.QueriesFor(promql.QPVCBindings) {
		assert.NotRegexp(t, `(^|[|"])db-0([|"]|$)`, query, "a pod that left the root contributes no binding")
		sawStay = sawStay || strings.Contains(query, "stay-0")
	}
	assert.True(t, sawStay, "a pod that stayed on the root is fetched")
}

func TestNodeSeed_OverCapRejected(t *testing.T) {
	names := make([]string, maxRootedVolumeLabelChunks+1)
	for i := range names {
		names[i] = "n" + strings.Repeat("x", 8) + itoa(i)
	}
	q := promqlfake.New(nil)
	_, err := New(q, Options{QoSScopeBatchBytes: 1}, nil, nil).BuildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootNode, names).Roots)
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Empty(t, q.Issued())
}

// A pod's first kube_pod_info series can carry no node (scraped before
// scheduling). parseTopology merges a UID's labels, so the node another series
// of the same UID names must decide the seed, whatever the vector order.
func TestPodsNewestOn_EmptyNodeSeriesOfSameUIDDoesNotHideTheNode(t *testing.T) {
	at := model.Time(1000)
	unscheduled := &model.Sample{Timestamp: at, Metric: model.Metric{
		"cluster": "c1", "namespace": "shop", "pod": "orders-0", "uid": "u1",
	}}
	scheduled := &model.Sample{Timestamp: at, Metric: model.Metric{
		"cluster": "c1", "namespace": "shop", "pod": "orders-0", "uid": "u1", "node": "worker-1",
	}}
	want := map[podSeriesKey]struct{}{{cluster: "c1", namespace: "shop", pod: "orders-0"}: {}}
	for _, rows := range []model.Vector{{unscheduled, scheduled}, {scheduled, unscheduled}} {
		assert.Equal(t, want, podsNewestOn(rows, []string{"worker-1"}))
	}
}
