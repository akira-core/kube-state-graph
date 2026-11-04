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

// substituteQoSFunc rewrites only the six QoS workload queries' range
// function. Every other issued line stays byte-identical, which is how a
// max or avg build is compared with the last build.
func substituteQoSFunc(lines []string, fn string) []string {
	qos := map[string]bool{}
	for _, q := range promql.QoSWorkloadQueries {
		qos[string(q)] = true
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		name, query, ok := strings.Cut(line, "|")
		if ok && qos[name] {
			_, rest, found := strings.Cut(query, "(")
			if found {
				query = fn + "(" + rest
			}
		}
		out[i] = name + "|" + query
	}
	return out
}

func storagePodRoot(t *testing.T) graph.StorageRoots {
	t.Helper()
	return vlrScope(t, graph.StorageRootPod, []string{"shop/orders-0"}).Roots
}

func issueStorage(t *testing.T, opts StorageOptions) []string {
	t.Helper()
	q := promqlfake.New(planEstate())
	_, err := New(q, Options{}, nil, nil).BuildStorageWith(
		t.Context(), time.Minute, vlrEnd, vlrSel, storagePodRoot(t), opts)
	require.NoError(t, err)
	lines := allFanOut(q)
	for _, family := range promql.QoSWorkloadQueries {
		require.NotEmpty(t, q.QueriesFor(family), family)
	}
	return lines
}

func TestBuildStorageWith_IORollupChangesOnlyQoSFunctions(t *testing.T) {
	last := issueStorage(t, StorageOptions{})
	assert.Equal(t, last, issueStorage(t, StorageOptions{IORollup: promql.RollupLast}),
		"an explicit last rollup issues the zero-value queries")

	q := promqlfake.New(planEstate())
	_, err := New(q, Options{}, nil, nil).BuildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel, storagePodRoot(t))
	require.NoError(t, err)
	assert.Equal(t, last, allFanOut(q), "BuildStorage is the zero StorageOptions build")

	assert.Equal(t, substituteQoSFunc(last, "max_over_time"),
		issueStorage(t, StorageOptions{IORollup: promql.RollupMax}))
	assert.Equal(t, substituteQoSFunc(last, "avg_over_time"),
		issueStorage(t, StorageOptions{IORollup: promql.RollupAvg}))
}

func TestBuild_QoSStaysLast(t *testing.T) {
	fx := map[promql.Query]model.Vector{
		promql.QPodInfo: {&model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "pod": "mongo", "uid": "u1", "node": "w",
		}, Value: 1}},
		promql.QPVCInfo: {&model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data",
			"volumename": "pvc-9f3a",
		}, Value: 1}},
		promql.QVolumeLabels: {&model.Sample{Metric: model.Metric{
			"volume": "trident_pvc_9f3a", "cluster": "ontap-prod",
			"node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm0",
		}, Value: 1}},
	}
	q := promqlfake.New(fx)
	_, err := New(q, Options{}, nil, nil).Build(t.Context(), time.Minute, vlrEnd, promql.Selector{})
	require.NoError(t, err)
	for _, family := range promql.QoSWorkloadQueries {
		got := q.QueriesFor(family)
		require.NotEmpty(t, got, family)
		for _, rendered := range got {
			assert.True(t, strings.HasPrefix(rendered, "last_over_time("), rendered)
		}
	}
}

func TestBuildStorageWith_InvalidRollupIssuesNothing(t *testing.T) {
	q := promqlfake.New(planEstate())
	g, err := New(q, Options{}, nil, nil).BuildStorageWith(
		t.Context(), time.Minute, vlrEnd, vlrSel, storagePodRoot(t),
		StorageOptions{IORollup: promql.Rollup("p99")})
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Nil(t, g)
	assert.Empty(t, q.Issued(), "an invalid rollup is rejected before any query")
}
