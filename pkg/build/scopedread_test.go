package build

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func TestIssueScoped_EmptyScopeIssuesNothing(t *testing.T) {
	f := promqlfake.New(nil)
	v := &topologyVectors{}
	var mu sync.Mutex
	err := issueScopedFamilies(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{}, v, &mu, []scopedFamily{
			{query: promql.QStatefulSetAnnotations, dst: &v.StatefulSetAnnotations, scope: nil},
		})
	require.NoError(t, err)
	assert.Empty(t, f.Issued())
	assert.False(t, v.ScopeIssued[promql.QStatefulSetAnnotations])
}

func TestIssueScoped_RequiredChunkFails(t *testing.T) {
	f := promqlfake.New(nil)
	f.Fail = func(name, _ string) error {
		if name == string(promql.QReplicaSetOwner) {
			return errors.New("upstream 5xx")
		}
		return nil
	}
	v := &topologyVectors{}
	var mu sync.Mutex
	err := issueScopedFamilies(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{}, v, &mu, []scopedFamily{
			{query: promql.QReplicaSetOwner, dst: &v.ReplicaSetOwner, scope: []string{"a"}},
		})
	require.Error(t, err, "a chunk failure fails the whole read")
}

// Every family fails closed, the two that degrade on /v1/graph included: a
// by-reference wave only runs on /v1/storage-graph
// (fail-storage-graph-on-any-leg-error). One failed chunk of three fails the
// read, and the error names the family and nothing from the upstream text.
func TestIssueScoped_AnyChunkFailureFailsAndNamesFamily(t *testing.T) {
	const tracking = "annotation_argocd_argoproj_io_tracking_id"
	rsRow := func(rs string) *model.Sample {
		return &model.Sample{Metric: model.Metric{"replicaset": model.LabelValue(rs), tracking: "x:y/z:ns/n"}, Value: 1}
	}
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QReplicaSetAnnotations: {rsRow("a"), rsRow("b"), rsRow("c")},
	})
	f.Fail = func(name, query string) error {
		if name == string(promql.QReplicaSetAnnotations) && strings.Contains(query, `replicaset="b"`) {
			return errors.New(`Post "http://vm-internal:8428/api/v1/query": upstream 5xx`)
		}
		return nil
	}
	v := &topologyVectors{}
	var mu sync.Mutex
	// A budget below one name's rendered length forces one chunk per name.
	err := issueScopedFamilies(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
		Options{QoSScopeBatchBytes: 1}, promql.Selector{}, v, &mu, []scopedFamily{
			{query: promql.QReplicaSetAnnotations, dst: &v.ReplicaSetAnnotations, scope: []string{"a", "b", "c"}},
		})
	require.Error(t, err)
	qe, ok := errors.AsType[*QueryError](err)
	require.True(t, ok, "the chunk error is named with its family")
	assert.Equal(t, string(promql.QReplicaSetAnnotations), qe.Query)
	assert.False(t, v.JobAnnotationsDegraded, "no wave here ever marks a family degraded")
}

// Two families with independent scopes and independent chunk counts issue and
// merge without interfering with each other.
func TestIssueScoped_HeterogeneousScopesPerFamily(t *testing.T) {
	const tracking = "annotation_argocd_argoproj_io_tracking_id"
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QStatefulSetAnnotations: {{Metric: model.Metric{"statefulset": "orders", tracking: "x:y/z:ns/n"}, Value: 1}},
		promql.QDaemonSetAnnotations:   {{Metric: model.Metric{"daemonset": "logger", tracking: "x:y/z:ns/n"}, Value: 1}},
	})
	v := &topologyVectors{}
	var mu sync.Mutex
	err := issueScopedFamilies(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{}, v, &mu, []scopedFamily{
			{query: promql.QStatefulSetAnnotations, dst: &v.StatefulSetAnnotations, scope: []string{"orders"}},
			{query: promql.QDaemonSetAnnotations, dst: &v.DaemonSetAnnotations, scope: nil},
		})
	require.NoError(t, err)
	assert.Len(t, v.StatefulSetAnnotations, 1)
	assert.True(t, v.ScopeIssued[promql.QStatefulSetAnnotations])
	assert.Empty(t, f.QueriesFor(promql.QDaemonSetAnnotations), "an empty-scope family in the same call issues nothing")
	assert.False(t, v.ScopeIssued[promql.QDaemonSetAnnotations])
}
