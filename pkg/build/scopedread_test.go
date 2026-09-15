package build

import (
	"context"
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
	err := issueScopedFamilies(t.Context(), t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{}, v, &mu, []scopedFamily{
			{query: promql.QStatefulSetAnnotations, dst: &v.StatefulSetAnnotations, scope: nil, mode: legRequired},
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
	err := issueScopedFamilies(t.Context(), t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{}, v, &mu, []scopedFamily{
			{query: promql.QReplicaSetOwner, dst: &v.ReplicaSetOwner, scope: []string{"a"}, mode: legRequired},
		})
	require.Error(t, err, "a legRequired chunk failure fails the whole read")
}

// A failed OPTIONAL chunk degrades on its own: the other chunks of the same
// family still land, and the family is still counted as issued.
func TestIssueScoped_OptionalChunkDegradesAlone(t *testing.T) {
	const tracking = "annotation_argocd_argoproj_io_tracking_id"
	rsRow := func(rs string) *model.Sample {
		return &model.Sample{Metric: model.Metric{"replicaset": model.LabelValue(rs), tracking: "x:y/z:ns/n"}, Value: 1}
	}
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QReplicaSetAnnotations: {rsRow("a"), rsRow("b"), rsRow("c")},
	})
	f.Fail = func(name, query string) error {
		if name == string(promql.QReplicaSetAnnotations) && strings.Contains(query, `replicaset="b"`) {
			return errors.New("upstream 5xx")
		}
		return nil
	}
	v := &topologyVectors{}
	var mu sync.Mutex
	// A budget below one name's rendered length forces one chunk per name.
	err := issueScopedFamilies(t.Context(), t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
		Options{QoSScopeBatchBytes: 1}, promql.Selector{}, v, &mu, []scopedFamily{
			{query: promql.QReplicaSetAnnotations, dst: &v.ReplicaSetAnnotations, scope: []string{"a", "b", "c"}, mode: legOptional},
		})
	require.NoError(t, err)
	assert.Len(t, v.ReplicaSetAnnotations, 2, "the failed chunk's series are missing, the other two survive")
	assert.True(t, v.ScopeIssued[promql.QReplicaSetAnnotations], "the family is still counted as issued overall")
}

// legOptionalTracking sets the shared flag iff at least one chunk degraded —
// the kube_job_annotations contract that suppresses the Job -> CronJob hop.
func TestIssueScoped_OptionalTrackingSetsFlag(t *testing.T) {
	f := promqlfake.New(nil)
	f.Fail = func(name, _ string) error {
		if name == string(promql.QJobAnnotations) {
			return errors.New("upstream 5xx")
		}
		return nil
	}
	v := &topologyVectors{}
	var mu sync.Mutex
	err := issueScopedFamilies(t.Context(), t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{}, v, &mu, []scopedFamily{
			{
				query: promql.QJobAnnotations, dst: &v.JobAnnotations, scope: []string{"a"},
				mode: legOptionalTracking, degraded: &v.JobAnnotationsDegraded,
			},
		})
	require.NoError(t, err)
	assert.True(t, v.JobAnnotationsDegraded)
	assert.True(t, v.ScopeIssued[promql.QJobAnnotations])
}

// optionalQueryFatal fails an OPTIONAL leg whenever the CALLER's context is
// already done — a build timeout or a disconnected client — even though the
// error class would otherwise degrade.
func TestIssueScoped_CallerCancellationFailsOptional(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	f := promqlfake.New(nil)
	f.Fail = func(_, _ string) error {
		cancel() // the caller went away mid-query
		return errors.New("upstream 5xx")
	}
	v := &topologyVectors{}
	var mu sync.Mutex
	err := issueScopedFamilies(ctx, ctx, f, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{}, v, &mu, []scopedFamily{
			{query: promql.QReplicaSetAnnotations, dst: &v.ReplicaSetAnnotations, scope: []string{"a"}, mode: legOptional},
		})
	require.Error(t, err, "caller cancellation must still fail an optional leg")
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
	err := issueScopedFamilies(t.Context(), t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{}, v, &mu, []scopedFamily{
			{query: promql.QStatefulSetAnnotations, dst: &v.StatefulSetAnnotations, scope: []string{"orders"}, mode: legRequired},
			{query: promql.QDaemonSetAnnotations, dst: &v.DaemonSetAnnotations, scope: nil, mode: legRequired},
		})
	require.NoError(t, err)
	assert.Len(t, v.StatefulSetAnnotations, 1)
	assert.True(t, v.ScopeIssued[promql.QStatefulSetAnnotations])
	assert.Empty(t, f.QueriesFor(promql.QDaemonSetAnnotations), "an empty-scope family in the same call issues nothing")
	assert.False(t, v.ScopeIssued[promql.QDaemonSetAnnotations])
}
