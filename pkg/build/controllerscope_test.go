package build

import (
	"errors"
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

func TestControllerTargetsAreControllerScopedQueries(t *testing.T) {
	got := make([]promql.Query, 0, len(promql.ControllerScopedQueries))
	for _, tg := range controllerTargets(&topologyVectors{}) {
		got = append(got, tg.query)
	}
	assert.Equal(t, promql.ControllerScopedQueries, got)
}

func ownerRow(pod, kind, name string) *model.Sample {
	return planKSM("namespace", "shop", "pod", pod, "owner_kind", kind, "owner_name", name, "owner_is_controller", "true")
}

// controllerScope mirrors resolvePodOwners' own discard: only controller rows
// with a non-empty kind and name contribute, and the result is deterministic
// regardless of input order.
func TestControllerScope(t *testing.T) {
	vec := model.Vector{
		ownerRow("a", "StatefulSet", "orders"),
		ownerRow("b", "ReplicaSet", "web-7d9"),
		ownerRow("c", "ReplicaSet", "web-7d9"), // duplicate name
		ownerRow("d", "Job", "batch-1"),
		planKSM("namespace", "shop", "pod", "e", "owner_kind", "StatefulSet", "owner_name", "orders", "owner_is_controller", "false"), // not a controller
		planKSM("namespace", "shop", "pod", "f", "owner_kind", "", "owner_name", "x", "owner_is_controller", "true"),                  // empty kind
	}
	got := controllerScope(vec)
	assert.Equal(t, map[string][]string{
		"StatefulSet": {"orders"},
		"ReplicaSet":  {"web-7d9"},
		"Job":         {"batch-1"},
	}, got)
	assert.Empty(t, controllerScope(nil))
}

// deploymentScope unions direct Deployment owners with every Deployment name a
// loaded kube_replicaset_owner series resolves a ReplicaSet up to — the same
// owner_kind=="Deployment" filter resolvePodOwners applies.
func TestDeploymentScope(t *testing.T) {
	rsOwner := model.Vector{
		planKSM("namespace", "shop", "replicaset", "web-7d9", "owner_kind", "Deployment", "owner_name", "web"),
		planKSM("namespace", "shop", "replicaset", "bare-x1", "owner_kind", "", "owner_name", ""), // bare: no Deployment owner
	}
	assert.Equal(t, []string{"api", "web"}, deploymentScope([]string{"api"}, rsOwner))
	assert.Equal(t, []string{"web"}, deploymentScope(nil, rsOwner))
	assert.Empty(t, deploymentScope(nil, nil))
}

// cronJobScope unions direct CronJob owners with every owner_name a loaded
// kube_job_owner series carries — kube_job_owner's own fixed selector already
// restricts every row it can return to a CronJob controller.
func TestCronJobScope(t *testing.T) {
	jobOwner := model.Vector{
		planKSM("namespace", "shop", "job_name", "nightly-28901", "owner_kind", "CronJob", "owner_name", "nightly", "owner_is_controller", "true"),
	}
	assert.Equal(t, []string{"nightly", "weekly"}, cronJobScope([]string{"weekly"}, jobOwner))
	assert.Equal(t, []string{"nightly"}, cronJobScope(nil, jobOwner))
	assert.Empty(t, cronJobScope(nil, nil))
}

func indexOfIssuedQuery(issued []promqlfake.Issued, name promql.Query) int {
	for i, is := range issued {
		if is.Name == string(name) {
			return i
		}
	}
	return -1
}

// Spec: "Controller read is restricted to the loaded pods' owners".
func TestReadScopedControllers_RestrictedToLoadedOwners(t *testing.T) {
	bind := func(pod string) *model.Sample {
		return planKSM("namespace", "shop", "pod", pod, "persistentvolumeclaim", "data-"+pod)
	}
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {bind("sts-0"), bind("web-0")},
		promql.QPodOwner:    {ownerRow("sts-0", "StatefulSet", "orders"), ownerRow("web-0", "ReplicaSet", "web-7d9")},
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-7d9", "owner_kind", "Deployment", "owner_name", "web"),
		},
	})
	_, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, graph.StorageRoots{})
	require.NoError(t, err)

	const tracking = `annotation_argocd_argoproj_io_tracking_id!=""`
	assert.Equal(t,
		[]string{`last_over_time(kube_statefulset_annotations{` + tracking + `,az="zone-a",env="prod",statefulset="orders"}[1m])`},
		f.QueriesFor(promql.QStatefulSetAnnotations))
	assert.Equal(t,
		[]string{`last_over_time(kube_replicaset_owner{az="zone-a",env="prod",replicaset="web-7d9"}[1m])`},
		f.QueriesFor(promql.QReplicaSetOwner))
	assert.Equal(t,
		[]string{`last_over_time(kube_replicaset_annotations{` + tracking + `,az="zone-a",env="prod",replicaset="web-7d9"}[1m])`},
		f.QueriesFor(promql.QReplicaSetAnnotations))
	assert.Equal(t,
		[]string{`last_over_time(kube_deployment_annotations{` + tracking + `,az="zone-a",env="prod",deployment="web"}[1m])`},
		f.QueriesFor(promql.QDeploymentAnnotations))

	for _, q := range []promql.Query{promql.QJobOwner, promql.QJobAnnotations, promql.QDaemonSetAnnotations, promql.QCronJobAnnotations} {
		assert.Empty(t, f.QueriesFor(q), "%s", q)
	}

	issued := f.Issued()
	rsAt := indexOfIssuedQuery(issued, promql.QReplicaSetOwner)
	depAt := indexOfIssuedQuery(issued, promql.QDeploymentAnnotations)
	require.GreaterOrEqual(t, rsAt, 0)
	assert.Greater(t, depAt, rsAt, "kube_deployment_annotations waits for kube_replicaset_owner (stage B waits for stage A)")
}

// Spec: "Job-owned mounting pod resolves its CronJob Application through two
// stages".
func TestReadScopedControllers_JobResolvesThroughCronJobInTwoStages(t *testing.T) {
	const tracking = "annotation_argocd_argoproj_io_tracking_id"
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {planKSM("namespace", "shop", "pod", "job-0", "persistentvolumeclaim", "data-job-0")},
		promql.QPodInfo:     {planKSM("namespace", "shop", "pod", "job-0", "uid", "uid-job0")},
		promql.QPodOwner:    {ownerRow("job-0", "Job", "nightly-28901")},
		promql.QJobOwner: {
			planKSM("namespace", "shop", "job_name", "nightly-28901", "owner_kind", "CronJob", "owner_name", "nightly", "owner_is_controller", "true"),
		},
		promql.QCronJobAnnotations: {
			planKSM("namespace", "shop", "cronjob", "nightly", tracking, "reports:batch/CronJob:shop/nightly"),
		},
	})
	g, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, graph.StorageRoots{})
	require.NoError(t, err)

	assert.Equal(t,
		[]string{`last_over_time(kube_job_owner{owner_kind="CronJob",owner_is_controller="true",az="zone-a",env="prod",job_name="nightly-28901"}[1m])`},
		f.QueriesFor(promql.QJobOwner))
	assert.Equal(t,
		[]string{`last_over_time(kube_job_annotations{` + tracking + `!="",az="zone-a",env="prod",job_name="nightly-28901"}[1m])`},
		f.QueriesFor(promql.QJobAnnotations))
	assert.Equal(t,
		[]string{`last_over_time(kube_cronjob_annotations{` + tracking + `!="",az="zone-a",env="prod",cronjob="nightly"}[1m])`},
		f.QueriesFor(promql.QCronJobAnnotations))

	issued := f.Issued()
	jobAt := indexOfIssuedQuery(issued, promql.QJobOwner)
	cronAt := indexOfIssuedQuery(issued, promql.QCronJobAnnotations)
	require.GreaterOrEqual(t, jobAt, 0)
	assert.Greater(t, cronAt, jobAt, "kube_cronjob_annotations waits for kube_job_owner")

	pod, ok := g.NodesByID["zone-a-prod-c1/uid-job0"].(*graph.PodNode)
	require.True(t, ok)
	assert.Equal(t, "reports", pod.Application(), "resolved through the CronJob, since the Job itself carries no annotation")
	assert.Equal(t, &graph.Owner{Kind: "Job", Name: "nightly-28901"}, pod.Owner(), "the hop never alters data.owner")
}

// Spec: "A kind no loaded pod is owned by issues no query".
func TestReadScopedControllers_KindWithNoOwnersIssuesNothing(t *testing.T) {
	bind := func(pod string) *model.Sample {
		return planKSM("namespace", "shop", "pod", pod, "persistentvolumeclaim", "data-"+pod)
	}
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {bind("a"), bind("b")},
		promql.QPodOwner:    {ownerRow("a", "StatefulSet", "orders"), ownerRow("b", "StatefulSet", "orders")},
	})
	tp, err := readTopology(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(), Options{}, storageSel, storagePlan(graph.StorageRoots{}))
	require.NoError(t, err)

	for _, q := range []promql.Query{
		promql.QReplicaSetOwner, promql.QReplicaSetAnnotations, promql.QJobOwner, promql.QJobAnnotations,
		promql.QDaemonSetAnnotations, promql.QDeploymentAnnotations, promql.QCronJobAnnotations,
	} {
		assert.Empty(t, f.QueriesFor(q), "%s", q)
		assert.NotContains(t, tp.RawSeriesCount, string(q), "%s", q)
	}
	assert.NotEmpty(t, f.QueriesFor(promql.QStatefulSetAnnotations))
}

// Spec: "A required controller chunk failure fails the build".
func TestReadScopedControllers_RequiredChunkFailsBuild(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {planKSM("namespace", "shop", "pod", "a", "persistentvolumeclaim", "data-a")},
		promql.QPodOwner:    {ownerRow("a", "DaemonSet", "logger")},
	})
	f.Fail = func(name, _ string) error {
		if name == string(promql.QDaemonSetAnnotations) {
			return errors.New("upstream 5xx")
		}
		return nil
	}
	_, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, graph.StorageRoots{})
	require.Error(t, err, "kube_daemonset_annotations is required, exactly as its unscoped read is")
}

// Spec: "A degrading controller chunk degrades and suppresses the hop".
func TestReadScopedControllers_JobAnnotationsChunkDegradesAndSuppressesHop(t *testing.T) {
	const tracking = "annotation_argocd_argoproj_io_tracking_id"
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {planKSM("namespace", "shop", "pod", "job-0", "persistentvolumeclaim", "data-job-0")},
		promql.QPodInfo:     {planKSM("namespace", "shop", "pod", "job-0", "uid", "uid-job0")},
		promql.QPodOwner:    {ownerRow("job-0", "Job", "nightly-28901")},
		promql.QJobOwner: {
			planKSM("namespace", "shop", "job_name", "nightly-28901", "owner_kind", "CronJob", "owner_name", "nightly", "owner_is_controller", "true"),
		},
		promql.QCronJobAnnotations: {
			planKSM("namespace", "shop", "cronjob", "nightly", tracking, "reports:batch/CronJob:shop/nightly"),
		},
	})
	f.Fail = func(name, _ string) error {
		if name == string(promql.QJobAnnotations) {
			return errors.New("upstream 5xx")
		}
		return nil
	}
	g, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, graph.StorageRoots{})
	require.NoError(t, err, "an optional-tracking degrade must not fail the build")

	pod, ok := g.NodesByID["zone-a-prod-c1/uid-job0"].(*graph.PodNode)
	require.True(t, ok)
	assert.Empty(t, pod.Application(), "the hop is suppressed for a build where kube_job_annotations degraded")
	assert.Equal(t, &graph.Owner{Kind: "Job", Name: "nightly-28901"}, pod.Owner())
}

// Spec: "Accumulated Job history does not reach the storage build".
func TestReadScopedControllers_AccumulatedJobHistoryNotRead(t *testing.T) {
	bind := func(pod string) *model.Sample {
		return planKSM("namespace", "shop", "pod", pod, "persistentvolumeclaim", "data-"+pod)
	}
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {bind("backup-0"), bind("backup2-0")},
		promql.QPodOwner: {
			ownerRow("backup-0", "Job", "backup-28901"),
			ownerRow("backup2-0", "Job", "backup-28902"),
		},
	})
	f.Fail = func(name, query string) error {
		if name == string(promql.QJobOwner) && !strings.Contains(query, "job_name") {
			return errors.New("would have scanned the whole day's index")
		}
		return nil
	}
	_, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, graph.StorageRoots{})
	require.NoError(t, err)
	got := f.QueriesFor(promql.QJobOwner)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], `job_name=~"backup-28901|backup-28902"`)
}

// Spec: "Controller scope cross-namespace collision is harmless".
func TestReadScopedControllers_CrossNamespaceCollisionIsHarmless(t *testing.T) {
	const tracking = "annotation_argocd_argoproj_io_tracking_id"
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {planKSM("namespace", "shop", "pod", "db-0", "persistentvolumeclaim", "data-db-0")},
		promql.QPodInfo:     {planKSM("namespace", "shop", "pod", "db-0", "uid", "uid-db0")},
		promql.QPodOwner:    {ownerRow("db-0", "StatefulSet", "db")},
		promql.QStatefulSetAnnotations: {
			planKSM("namespace", "shop", "statefulset", "db", tracking, "shop-db:apps/StatefulSet:shop/db"),
			planKSM("namespace", "platform", "statefulset", "db", tracking, "platform-db:apps/StatefulSet:platform/db"),
		},
	})
	g, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, graph.StorageRoots{})
	require.NoError(t, err)

	pod, ok := g.NodesByID["zone-a-prod-c1/uid-db0"].(*graph.PodNode)
	require.True(t, ok)
	assert.Equal(t, "shop-db", pod.Application(), "the platform/db series is admitted but resolves nobody's Application")
}
