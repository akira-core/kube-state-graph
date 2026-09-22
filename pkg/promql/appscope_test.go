package promql

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrackingIDWrapperCost(t *testing.T) {
	t.Parallel()
	assert.Equal(t, len("(?:")+len(")(?::.*)?"), TrackingIDWrapperCost)
}

func TestRenderTrackingIDScoped(t *testing.T) {
	t.Parallel()
	one, ok := RenderTrackingIDScoped(QDeploymentAnnotations, time.Minute, LabelKeys{}, Selector{}, []string{"checkout"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(kube_deployment_annotations{annotation_argocd_argoproj_io_tracking_id!="",annotation_argocd_argoproj_io_tracking_id=~"(?:checkout)(?::.*)?"}[1m])`,
		one)

	two, ok := RenderTrackingIDScoped(QStatefulSetAnnotations, time.Minute, LabelKeys{}, Selector{
		AZ: []string{"zone-a"}, Env: []string{"prod"}, Cluster: []string{"c1"}, Namespace: []string{"shop"},
	}, []string{"b", "a"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(kube_statefulset_annotations{annotation_argocd_argoproj_io_tracking_id!="",az="zone-a",env="prod",cluster="c1",namespace="shop",annotation_argocd_argoproj_io_tracking_id=~"(?:a|b)(?::.*)?"}[1m])`,
		two, "the fixed !=\"\" matcher stays ahead of the request matchers")

	meta, ok := RenderTrackingIDScoped(QCronJobAnnotations, time.Minute, LabelKeys{}, Selector{}, []string{"a.b\"c"})
	require.True(t, ok)
	assert.Contains(t, meta, `annotation_argocd_argoproj_io_tracking_id=~"(?:a\\.b\"c)(?::.*)?"`)

	_, ok = RenderTrackingIDScoped(QPodInfo, time.Minute, LabelKeys{}, Selector{}, []string{"checkout"})
	assert.False(t, ok, "only the six annotation families accept a tracking-id scope")
	_, ok = RenderTrackingIDScoped(QDeploymentAnnotations, time.Minute, LabelKeys{}, Selector{}, nil)
	assert.False(t, ok)
	_, ok = RenderTrackingIDScoped(QDeploymentAnnotations, time.Minute, LabelKeys{}, Selector{}, []string{"", ""})
	assert.False(t, ok)
}

func TestRenderTrackingIDScoped_IsRenderPlusOneMatcher(t *testing.T) {
	t.Parallel()
	sel := Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Namespace: []string{"shop"}}
	families := []Query{
		QDeploymentAnnotations, QStatefulSetAnnotations, QDaemonSetAnnotations,
		QReplicaSetAnnotations, QJobAnnotations, QCronJobAnnotations,
	}
	for _, q := range families {
		for _, s := range []Selector{{}, sel} {
			t.Run(string(q), func(t *testing.T) {
				got, ok := RenderTrackingIDScoped(q, time.Minute, LabelKeys{}, s, []string{"b", "a"})
				require.True(t, ok)
				base := Render(q, time.Minute, LabelKeys{}, s)
				extra := TrackingIDLabel + `=~"(?:a|b)(?::.*)?"`
				want := strings.TrimSuffix(base, "}[1m])") + `,` + extra + `}[1m])`
				if !strings.Contains(base, "{") {
					want = strings.TrimSuffix(base, "[1m])") + `{` + extra + `}[1m])`
				}
				assert.Equal(t, want, got)
			})
		}
	}
}

func TestRenderOwnerScoped(t *testing.T) {
	t.Parallel()
	sel := Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Cluster: []string{"c1"}, Namespace: []string{"shop"}}

	rs, ok := RenderOwnerScoped(QReplicaSetOwner, time.Minute, LabelKeys{}, Selector{}, "Deployment", []string{"web"})
	require.True(t, ok)
	assert.Equal(t, `last_over_time(kube_replicaset_owner{owner_kind="Deployment",owner_name="web"}[1m])`, rs)

	rsSel, ok := RenderOwnerScoped(QReplicaSetOwner, time.Minute, LabelKeys{}, sel, "Deployment", []string{"b", "a"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(kube_replicaset_owner{az="zone-a",env="prod",cluster="c1",namespace="shop",owner_kind="Deployment",owner_name=~"a|b"}[1m])`,
		rsSel)

	pod, ok := RenderOwnerScoped(QPodOwner, time.Minute, LabelKeys{}, Selector{}, "ReplicaSet", []string{"web-7d9f"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(kube_pod_owner{owner_is_controller="true",owner_kind="ReplicaSet",owner_name="web-7d9f"}[1m])`,
		pod)
	podSel, ok := RenderOwnerScoped(QPodOwner, time.Minute, LabelKeys{}, sel, "Job", []string{"nightly-28901"})
	require.True(t, ok)
	assert.Contains(t, podSel, `az="zone-a",env="prod",cluster="c1",namespace="shop",owner_is_controller="true",owner_kind="Job",owner_name="nightly-28901"`)

	job, ok := RenderOwnerScoped(QJobOwner, time.Minute, LabelKeys{}, Selector{}, "CronJob", []string{"nightly"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(kube_job_owner{owner_kind="CronJob",owner_is_controller="true",owner_name="nightly"}[1m])`,
		job, "the fixed selector is preserved and no second kind matcher is added")
	jobSel, ok := RenderOwnerScoped(QJobOwner, time.Minute, LabelKeys{}, sel, "CronJob", []string{"b", "a"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(kube_job_owner{owner_kind="CronJob",owner_is_controller="true",az="zone-a",env="prod",cluster="c1",namespace="shop",owner_name=~"a|b"}[1m])`,
		jobSel)

	_, ok = RenderOwnerScoped(QJobOwner, time.Minute, LabelKeys{}, Selector{}, "Deployment", []string{"web"})
	assert.False(t, ok, "kube_job_owner is CronJob-only")
	_, ok = RenderOwnerScoped(QPodInfo, time.Minute, LabelKeys{}, Selector{}, "Pod", []string{"a"})
	assert.False(t, ok)
	_, ok = RenderOwnerScoped(QPodOwner, time.Minute, LabelKeys{}, Selector{}, "", []string{"a"})
	assert.False(t, ok)
	_, ok = RenderOwnerScoped(QPodOwner, time.Minute, LabelKeys{}, Selector{}, "ReplicaSet", nil)
	assert.False(t, ok)
}

func TestRenderOwnerScoped_IsRenderPlusNMatchers(t *testing.T) {
	t.Parallel()
	sel := Selector{AZ: []string{"zone-a"}, Namespace: []string{"shop"}}
	cases := []struct {
		q     Query
		kind  string
		added int
	}{
		{QReplicaSetOwner, "Deployment", 2},
		{QPodOwner, "ReplicaSet", 3},
		{QJobOwner, "CronJob", 1},
	}
	for _, tc := range cases {
		for _, s := range []Selector{{}, sel} {
			t.Run(string(tc.q), func(t *testing.T) {
				got, ok := RenderOwnerScoped(tc.q, time.Minute, LabelKeys{}, s, tc.kind, []string{"b", "a"})
				require.True(t, ok)
				base := Render(tc.q, time.Minute, LabelKeys{}, s)
				assert.Equal(t, tc.added, extraMatchers(t, base, got))
			})
		}
	}
}

// extraMatchers reports how many matchers scoped adds past base. Both strings
// are last_over_time renderings of the same query over a one-minute window.
func extraMatchers(t *testing.T, base, scoped string) int {
	t.Helper()
	baseN := matcherCount(base)
	scopedN := matcherCount(scoped)
	require.GreaterOrEqual(t, scopedN, baseN)
	require.True(t, strings.HasPrefix(scoped, strings.TrimSuffix(strings.TrimSuffix(base, "}[1m])"), "[1m])")) ||
		strings.Contains(scoped, strings.TrimPrefix(strings.TrimSuffix(base, "[1m])"), "last_over_time(")))
	return scopedN - baseN
}

func matcherCount(rendered string) int {
	open := strings.IndexByte(rendered, '{')
	if open < 0 {
		return 0
	}
	close := strings.IndexByte(rendered[open:], '}')
	if close < 0 {
		return 0
	}
	body := rendered[open+1 : open+close]
	if body == "" {
		return 0
	}
	// Matchers rendered by this package never contain a raw comma; a comma
	// separates matchers. Quoted values here are label names and regexes
	// without commas (QuoteMeta does not introduce one).
	return strings.Count(body, ",") + 1
}
