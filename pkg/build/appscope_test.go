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

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

const trackingLabel = "annotation_argocd_argoproj_io_tracking_id"

func recoverNames(t *testing.T, f promql.Querier, opts Options, sel promql.Selector, roots []string) ([]string, *topologyVectors, error) {
	t.Helper()
	v := &topologyVectors{}
	var mu sync.Mutex
	keys, err := readScopedApplications(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(), opts, sel, roots, v, &mu)
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, k.pod)
	}
	return names, v, err
}

// Spec: "Only the recovered pods' bindings are read" and "A claim annotated
// with the Application is tracked without its pods".
func TestApplicationSeed_TracksRecoveredPodsAndAnnotatedClaims(t *testing.T) {
	const tracking = trackingLabel
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", tracking, "checkout:apps/Deployment:shop/web"),
		},
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-1", "owner_kind", "Deployment", "owner_name", "web"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "shop", "pod", "orders-0", "owner_kind", "ReplicaSet", "owner_name", "web-1", "owner_is_controller", "true"),
		},
		promql.QPVCAnnotations: {
			planKSM("namespace", "shop", "persistentvolumeclaim", "ledger-data", tracking, "billing:apps/PersistentVolumeClaim:shop/ledger-data"),
		},
		promql.QPVCBindings: {
			planKSM("namespace", "shop", "pod", "orders-0", "persistentvolumeclaim", "orders-data"),
			planKSM("namespace", "shop", "pod", "report-0", "persistentvolumeclaim", "orders-data"),
			planKSM("namespace", "shop", "pod", "catalog-0", "persistentvolumeclaim", "catalog-data"),
			planKSM("namespace", "shop", "pod", "ledger-0", "persistentvolumeclaim", "ledger-data"),
		},
	})
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootApplication, []string{"checkout"})
	require.NoError(t, err)
	_, err = New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)

	var byPod string
	for _, query := range f.QueriesFor(promql.QPVCBindings) {
		if strings.Contains(query, `pod=`) {
			byPod = query
		}
		assert.NotContains(t, query, "catalog-0")
		assert.NotContains(t, query, "ledger-0")
		assert.NotContains(t, query, "catalog-data")
		assert.NotContains(t, query, "ledger-data")
	}
	assert.Contains(t, byPod, `pod="orders-0"`)
	assert.Contains(t, strings.Join(f.QueriesFor(promql.QPodInfo), "\n"), "orders-0")
	assert.Contains(t, strings.Join(f.QueriesFor(promql.QPodInfo), "\n"), "report-0", "the other mounter of the recovered pod's claim")
	assert.NotContains(t, strings.Join(f.QueriesFor(promql.QPodInfo), "\n"), "catalog-0")
}

// Spec: "A claim annotated with the Application is tracked without its pods".
func TestApplicationSeed_AnnotatedClaimLoadsItsMounter(t *testing.T) {
	const tracking = trackingLabel
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCAnnotations: {
			planKSM("namespace", "shop", "persistentvolumeclaim", "ledger-data", tracking, "billing:apps/PersistentVolumeClaim:shop/ledger-data"),
		},
		promql.QPVCBindings: {
			planKSM("namespace", "shop", "pod", "ledger-0", "persistentvolumeclaim", "ledger-data"),
		},
		promql.QPodInfo: {
			planKSM("namespace", "shop", "pod", "ledger-0", "uid", "uid-l", "node", "worker-1"),
		},
	})
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootApplication, []string{"billing"})
	require.NoError(t, err)
	_, err = New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)

	anns := f.QueriesFor(promql.QPVCAnnotations)
	require.Len(t, anns, 2, "tracking-id recovery, then the claim-name read")
	assert.Contains(t, anns[0], `annotation_argocd_argoproj_io_tracking_id=~"(?:billing)(?::.*)?"`)
	assert.Contains(t, anns[1], `namespace="shop",persistentvolumeclaim="ledger-data"`)
	for _, query := range f.QueriesFor(promql.QPodOwner) {
		assert.NotContains(t, query, `owner_kind=`, "no controller carries billing, so stage 3 does not run")
	}
	joined := strings.Join(f.QueriesFor(promql.QPVCBindings), "\n")
	assert.Contains(t, joined, `namespace="shop",persistentvolumeclaim="ledger-data"`)
	assert.Contains(t, strings.Join(f.QueriesFor(promql.QPodInfo), "\n"), "ledger-0")
}

func TestTallySeries_AddsExtraSeries(t *testing.T) {
	v := &topologyVectors{
		DeploymentAnnotations: model.Vector{&model.Sample{}, &model.Sample{}, &model.Sample{}},
		ScopeIssued:           map[promql.Query]bool{promql.QDeploymentAnnotations: true},
		ExtraSeriesCount:      map[promql.Query]int{promql.QDeploymentAnnotations: 2},
	}
	assert.Equal(t, 5, tallySeries(nil, topologyPlan{}, v)["kube_deployment_annotations"])

	extraOnly := &topologyVectors{ExtraSeriesCount: map[promql.Query]int{promql.QDeploymentAnnotations: 2}}
	got := tallySeries(nil, topologyPlan{}, extraOnly)
	assert.Equal(t, 2, got["kube_deployment_annotations"])

	assert.NotContains(t, tallySeries(nil, topologyPlan{}, &topologyVectors{}), "kube_deployment_annotations")
}

// The application-metadata seed lands in a local vector and is recorded as
// extra. The leg's own slot is the namespace read. The tally is their sum.
func TestTallySeries_AppMetadataSeedAddsToTheNamespaceRead(t *testing.T) {
	src := defaultAppMetadata()
	v := &topologyVectors{
		AppMetadata:      model.Vector{&model.Sample{}, &model.Sample{}},
		ScopeIssued:      map[promql.Query]bool{promql.QAppMetadata: true},
		ExtraSeriesCount: map[promql.Query]int{promql.QAppMetadata: 3},
	}
	plan := storagePlan(graph.StorageRoots{Kind: graph.StorageRootApplication, Names: []string{"checkout"}})
	got := tallySeries(topologyLegs(v, &src), plan, v)
	assert.Equal(t, 5, got[string(promql.QAppMetadata)])
}

func TestReadScopedApplications_DeploymentManagedClaimlessPod(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", trackingLabel, "checkout:apps/Deployment:shop/web"),
		},
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-7d9f", "owner_kind", "Deployment", "owner_name", "web"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "shop", "pod", "web-7d9f-abc", "owner_kind", "ReplicaSet", "owner_name", "web-7d9f", "owner_is_controller", "true"),
		},
	})
	names, _, err := recoverNames(t, f, Options{}, storageSel, []string{"checkout"})
	require.NoError(t, err)
	assert.Equal(t, []string{"web-7d9f-abc"}, names)

	for _, q := range []promql.Query{
		promql.QDeploymentAnnotations, promql.QStatefulSetAnnotations, promql.QDaemonSetAnnotations,
		promql.QCronJobAnnotations, promql.QReplicaSetAnnotations, promql.QJobAnnotations,
	} {
		assert.Equal(t, [][]string{{"checkout"}}, f.ScopeValues(q, promql.TrackingIDLabel), string(q))
		got := f.QueriesFor(q)
		require.Len(t, got, 1, string(q))
		assert.Contains(t, got[0], trackingLabel+`!=""`)
		assert.Contains(t, got[0], `az="zone-a"`)
		assert.Contains(t, got[0], `env="prod"`)
	}
	assert.Equal(t, [][]string{{"Deployment"}}, f.ScopeValues(promql.QReplicaSetOwner, "owner_kind"))
	assert.Equal(t, [][]string{{"web"}}, f.ScopeValues(promql.QReplicaSetOwner, "owner_name"))
	assert.Contains(t, f.ScopeValues(promql.QPodOwner, "owner_kind"), []string{"ReplicaSet"})
	assert.Contains(t, f.ScopeValues(promql.QPodOwner, "owner_kind"), []string{"Deployment"}, "a pod directly owned by the Deployment is a candidate too")
	assert.Contains(t, f.ScopeValues(promql.QPodOwner, "owner_name"), []string{"web-7d9f"})
	for _, vals := range f.ScopeValues(promql.QPodOwner, "owner_is_controller") {
		assert.Equal(t, []string{"true"}, vals)
	}
	assert.Empty(t, f.QueriesFor(promql.QJobOwner))
}

func TestReadScopedApplications_CronJobManagedPod(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QCronJobAnnotations: {
			planKSM("namespace", "batch", "cronjob", "nightly", trackingLabel, "reports:batch/CronJob:batch/nightly"),
		},
		promql.QJobOwner: {
			planKSM("namespace", "batch", "job_name", "nightly-28901", "owner_kind", "CronJob", "owner_name", "nightly", "owner_is_controller", "true"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "batch", "pod", "nightly-28901-x", "owner_kind", "Job", "owner_name", "nightly-28901", "owner_is_controller", "true"),
		},
		promql.QPodInfo: {
			planKSM("namespace", "batch", "pod", "nightly-28901-x", "uid", "uid-nightly", "node", "worker-1"),
		},
	})
	names, _, err := recoverNames(t, f, Options{}, promql.Selector{}, []string{"reports"})
	require.NoError(t, err)
	assert.Equal(t, []string{"nightly-28901-x"}, names)

	jobQ := f.QueriesFor(promql.QJobOwner)
	require.NotEmpty(t, jobQ)
	assert.Contains(t, jobQ[0], `owner_kind="CronJob"`)
	assert.Contains(t, jobQ[0], `owner_is_controller="true"`)
	assert.Contains(t, jobQ[0], `owner_name="nightly"`)
	assert.Contains(t, f.ScopeValues(promql.QPodOwner, "owner_kind"), []string{"Job"})
	assert.Contains(t, f.ScopeValues(promql.QPodOwner, "owner_kind"), []string{"CronJob"}, "a pod directly owned by the CronJob is a candidate too")
	assert.Contains(t, f.ScopeValues(promql.QPodOwner, "owner_name"), []string{"nightly-28901"})

	scope := graph.StorageScope{Roots: graph.StorageRoots{Kind: graph.StorageRootApplication, Names: []string{"reports"}}}
	g, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), promql.Selector{}, scope.Roots)
	require.NoError(t, err)
	body := cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	var pod *cytoscape.NodeData
	for i := range body.Elements.Nodes {
		if body.Elements.Nodes[i].Data.ID == "unknown/uid-nightly" || strings.HasSuffix(body.Elements.Nodes[i].Data.ID, "/uid-nightly") {
			pod = &body.Elements.Nodes[i].Data
		}
	}
	require.NotNil(t, pod, "the claimless CronJob pod is drawn")
	assert.Equal(t, "reports", pod.Application)
	require.NotNil(t, pod.Owner)
	assert.Equal(t, "Job", pod.Owner.Kind)
	assert.Equal(t, "nightly-28901", pod.Owner.Name)
	for _, e := range body.Elements.Edges {
		assert.NotEqual(t, pod.ID, e.Data.Source)
		assert.NotEqual(t, pod.ID, e.Data.Target)
	}
}

func TestReadScopedApplications_NoControllerMatches(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", trackingLabel, "other:apps/Deployment:shop/web"),
		},
	})
	names, _, err := recoverNames(t, f, Options{}, promql.Selector{}, []string{"typo"})
	require.NoError(t, err)
	assert.Empty(t, names)
	for _, q := range []promql.Query{
		promql.QDeploymentAnnotations, promql.QStatefulSetAnnotations, promql.QDaemonSetAnnotations,
		promql.QCronJobAnnotations, promql.QReplicaSetAnnotations, promql.QJobAnnotations,
	} {
		assert.Len(t, f.QueriesFor(q), 1, string(q))
	}
	assert.Empty(t, f.QueriesFor(promql.QReplicaSetOwner))
	assert.Empty(t, f.QueriesFor(promql.QJobOwner))
	assert.Empty(t, f.QueriesFor(promql.QPodOwner))
	assert.Empty(t, f.QueriesFor(promql.QPVCBindings), "nothing recovered and no annotated claim: no binding query")
	require.Len(t, f.QueriesFor(promql.QPVCAnnotations), 1)
	assert.Contains(t, f.QueriesFor(promql.QPVCAnnotations)[0], trackingLabel+`=~`)
}

// Spec: "No controller or claim carries the Application", with the metric
// configured: the seed is still issued in the stage-1 wave, and with no row
// naming the Application nothing downstream is read.
func TestReadScopedApplications_NoControllerMatchesStillSeeds(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", trackingLabel, "other:apps/Deployment:shop/web"),
		},
	})
	opts := appMetadataOpts()
	names, _, err := recoverNames(t, f, opts, promql.Selector{}, []string{"typo"})
	require.NoError(t, err)
	assert.Empty(t, names)
	seed := f.QueriesFor(promql.QAppMetadata)
	require.Len(t, seed, 1)
	assert.Contains(t, seed[0], opts.AppMetadata.App+`="typo"`)
	assert.Empty(t, f.QueriesFor(promql.QReplicaSetOwner))
	assert.Empty(t, f.QueriesFor(promql.QJobOwner))
	assert.Empty(t, f.QueriesFor(promql.QPodOwner))
	assert.Empty(t, f.QueriesFor(promql.QPVCBindings))
	assert.Empty(t, f.QueriesFor(promql.QPodInfo))
}

func TestReadScopedApplications_NamespaceFilterNarrows(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", trackingLabel, "checkout:apps/Deployment:shop/web"),
			planKSM("namespace", "platform", "deployment", "web", trackingLabel, "checkout:apps/Deployment:platform/web"),
		},
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-shop", "owner_kind", "Deployment", "owner_name", "web"),
			planKSM("namespace", "platform", "replicaset", "web-plat", "owner_kind", "Deployment", "owner_name", "web"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "shop", "pod", "shop-pod", "owner_kind", "ReplicaSet", "owner_name", "web-shop", "owner_is_controller", "true"),
			planKSM("namespace", "platform", "pod", "plat-pod", "owner_kind", "ReplicaSet", "owner_name", "web-plat", "owner_is_controller", "true"),
		},
	})
	sel := promql.Selector{Namespace: []string{"shop"}}
	names, _, err := recoverNames(t, f, Options{}, sel, []string{"checkout"})
	require.NoError(t, err)
	assert.Equal(t, []string{"shop-pod"}, names)
	for _, is := range f.Issued() {
		assert.Contains(t, is.Query, `namespace="shop"`, is.Name)
	}
}

// Spec: "A stage-1 annotation chunk failure fails the build" — the recovery
// runs only on /v1/storage-graph, which fails closed on the two families
// /v1/graph degrades.
func TestReadScopedApplications_DegradingFamilyFailsClosed(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", trackingLabel, "checkout:apps/Deployment:shop/web"),
		},
		promql.QJobAnnotations: {
			planKSM("namespace", "shop", "job_name", "batch-1", trackingLabel, "checkout:batch/Job:shop/batch-1"),
		},
	})
	f.Fail = func(name, _ string) error {
		if name == string(promql.QJobAnnotations) {
			return errors.New("upstream 5xx")
		}
		return nil
	}
	_, _, err := recoverNames(t, f, Options{}, promql.Selector{}, []string{"checkout"})
	require.Error(t, err)
	qe, ok := errors.AsType[*QueryError](err)
	require.True(t, ok)
	assert.Equal(t, string(promql.QJobAnnotations), qe.Query)
}

func TestReadScopedApplications_RequiredStageFails(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", trackingLabel, "checkout:apps/Deployment:shop/web"),
		},
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-7d9f", "owner_kind", "Deployment", "owner_name", "web"),
		},
	})
	f.Fail = func(name, _ string) error {
		if name == string(promql.QPodOwner) {
			return errors.New("upstream 5xx")
		}
		return nil
	}
	_, _, err := recoverNames(t, f, Options{}, promql.Selector{}, []string{"checkout"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstream 5xx")
}

func TestReadScopedApplications_StagesAreOrdered(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", trackingLabel, "checkout:apps/Deployment:shop/web"),
		},
		promql.QCronJobAnnotations: {
			planKSM("namespace", "shop", "cronjob", "nightly", trackingLabel, "checkout:batch/CronJob:shop/nightly"),
		},
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-7d9f", "owner_kind", "Deployment", "owner_name", "web"),
		},
		promql.QJobOwner: {
			planKSM("namespace", "shop", "job_name", "nightly-1", "owner_kind", "CronJob", "owner_name", "nightly", "owner_is_controller", "true"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "shop", "pod", "web-pod", "owner_kind", "ReplicaSet", "owner_name", "web-7d9f", "owner_is_controller", "true"),
			planKSM("namespace", "shop", "pod", "job-pod", "owner_kind", "Job", "owner_name", "nightly-1", "owner_is_controller", "true"),
		},
	})
	_, _, err := recoverNames(t, f, Options{}, promql.Selector{}, []string{"checkout"})
	require.NoError(t, err)

	stage1 := map[string]bool{
		string(promql.QDeploymentAnnotations): true, string(promql.QStatefulSetAnnotations): true,
		string(promql.QDaemonSetAnnotations): true, string(promql.QCronJobAnnotations): true,
		string(promql.QReplicaSetAnnotations): true, string(promql.QJobAnnotations): true,
	}
	last1, first2, last2, first3 := -1, -1, -1, -1
	for i, is := range f.Issued() {
		switch {
		case stage1[is.Name]:
			last1 = i
		case is.Name == string(promql.QReplicaSetOwner) || is.Name == string(promql.QJobOwner):
			if first2 < 0 {
				first2 = i
			}
			last2 = i
		case is.Name == string(promql.QPodOwner):
			if first3 < 0 {
				first3 = i
			}
		}
	}
	require.GreaterOrEqual(t, last1, 0)
	require.GreaterOrEqual(t, first2, 0)
	require.GreaterOrEqual(t, first3, 0)
	assert.Greater(t, first2, last1, "stage 2 waits for stage 1")
	assert.Greater(t, first3, last2, "stage 3 waits for stage 2")
}

func TestReadScopedApplications_ValueOrderIsIrrelevant(t *testing.T) {
	render := func(roots []string) []string {
		f := promqlfake.New(nil)
		_, _, err := recoverNames(t, f, Options{}, promql.Selector{}, roots)
		require.NoError(t, err)
		issued := f.Issued()
		out := make([]string, 0, len(issued))
		for _, is := range issued {
			out = append(out, is.Name+" "+is.Query)
		}
		return out
	}
	assert.ElementsMatch(t, render([]string{"b", "a"}), render([]string{"a", "b", "a"}))
}

func TestReadScopedApplications_OverCapRejected(t *testing.T) {
	apps := make([]string, maxApplicationRootChunks+1)
	for i := range apps {
		apps[i] = "app" + strings.Repeat("x", 3) + itoa(i)
	}
	q := promqlfake.New(nil)
	_, _, err := recoverNames(t, q, Options{QoSScopeBatchBytes: 1}, promql.Selector{}, apps)
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Empty(t, q.Issued(), "the cap is rejected before any query")

	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootApplication, apps)
	require.NoError(t, err)
	buildQ := promqlfake.New(nil)
	_, err = New(buildQ, Options{QoSScopeBatchBytes: 1}, nil, nil).BuildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Empty(t, buildQ.Issued())
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestReadScopedPods_WaitsOnRecoveryAndPVCAnnotations(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPodInfo: {planKSM("namespace", "shop", "pod", "orders-0", "uid", "uid-o")},
	})
	bindingsDone := make(chan struct{})
	close(bindingsDone)
	appDone := make(chan struct{})
	pvcDone := make(chan struct{})
	var recovered []podSeriesKey
	errCh := make(chan error, 1)
	go func() {
		v := &topologyVectors{PVC: model.Vector{podBinding("orders-0")}}
		var mu sync.Mutex
		errCh <- readScopedPods(context.Background(), f, time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{},
			nil, []string{"checkout"}, v, &mu, bindingsDone, appDone, pvcDone, &recovered)
	}()

	assert.Never(t, func() bool { return len(f.QueriesFor(promql.QPodInfo)) > 0 }, 80*time.Millisecond, 10*time.Millisecond,
		"the pod read waits for the recovery")
	recovered = []podSeriesKey{{cluster: "c1", namespace: "shop", pod: "orders-0"}}
	close(appDone)
	assert.Never(t, func() bool { return len(f.QueriesFor(promql.QPodInfo)) > 0 }, 80*time.Millisecond, 10*time.Millisecond,
		"the pod read also waits for pvc annotations")
	close(pvcDone)
	require.NoError(t, <-errCh)
	assert.NotEmpty(t, f.QueriesFor(promql.QPodInfo))
}

func TestReadScopedPods_UnrelatedBindingPodsNotRead(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPodInfo: {
			planKSM("namespace", "shop", "pod", "orders-0", "uid", "uid-o"),
			planKSM("namespace", "shop", "pod", "catalog-0", "uid", "uid-c"),
			planKSM("namespace", "shop", "pod", "ledger-0", "uid", "uid-l"),
		},
	})
	// The seed has already kept only the recovered pod's bindings. readScopedPods
	// loads every pod those bindings name, plus the recovered names.
	v := &topologyVectors{PVC: model.Vector{
		planKSM("namespace", "shop", "pod", "orders-0", "persistentvolumeclaim", "orders-data"),
	}}
	done := make(chan struct{})
	close(done)
	recovered := []podSeriesKey{{cluster: "c1", namespace: "shop", pod: "orders-0"}}
	var mu sync.Mutex
	err := readScopedPods(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(), Options{}, storageSel,
		nil, []string{"checkout"}, v, &mu, done, done, done, &recovered)
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"orders-0"}}, f.ScopeValues(promql.QPodInfo, "pod"))
	assert.Equal(t, [][]string{{"orders-0"}}, f.ScopeValues(promql.QPodOwner, "pod"))
}

// canaryAppEstate is a Rollout pod the controller-annotation chain cannot
// name: no tracking-id carries canary-app. The metric row is the only seed.
func canaryAppEstate() map[promql.Query]model.Vector {
	fx := map[promql.Query]model.Vector{
		promql.QAppMetadata: {
			appMetaRow("shop", "canary-0", "canary-app", "team-c", "carol"),
		},
		promql.QPodInfo: {
			planKSM("namespace", "shop", "pod", "canary-0", "uid", "uid-canary", "node", "worker-1"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "shop", "pod", "canary-0", "owner_kind", "Rollout", "owner_name", "canary", "owner_is_controller", "true"),
		},
		promql.QPVCBindings: {
			planKSM("namespace", "shop", "pod", "canary-0", "persistentvolumeclaim", "canary-data"),
		},
		promql.QPVCInfo: {
			claimInfoRow("shop", "canary-data", "pvc-canary"),
		},
		promql.QNodeInfo: {planKSM("node", "worker-1")},
	}
	for q, rows := range vlrHarvest([]vlrVol{
		{"pvc_canary", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 10},
	}) {
		fx[q] = rows
	}
	return fx
}

func appMetadataOpts() Options {
	src := defaultAppMetadata()
	return Options{AppMetadata: &src}
}

// Spec: "A pod named only by the metric is seeded and drawn".
func TestApplicationSeed_MetricOnlyRolloutPod(t *testing.T) {
	opts := appMetadataOpts()
	names, _, err := recoverNames(t, promqlfake.New(canaryAppEstate()), opts, vlrSel, []string{"canary-app"})
	require.NoError(t, err)
	assert.Equal(t, []string{"canary-0"}, names)

	scope := vlrScope(t, graph.StorageRootApplication, []string{"canary-app"})
	q := promqlfake.New(canaryAppEstate())
	g, err := New(q, opts, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"canary-app"}}, q.ScopeValues(promql.QAppMetadata, "argocd_app"),
		"the seed restricts the configured app label to the root")

	const cluster = "zone-a-prod-c1"
	body := cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	var pod *cytoscape.NodeData
	for i := range body.Elements.Nodes {
		if body.Elements.Nodes[i].Data.ID == cluster+"/uid-canary" {
			pod = &body.Elements.Nodes[i].Data
		}
	}
	require.NotNil(t, pod, "the metric-only pod is drawn")
	assert.Equal(t, "canary-app", pod.Application)
	require.NotNil(t, pod.Owner)
	assert.Equal(t, "Rollout", pod.Owner.Kind)
	assert.Equal(t, "canary", pod.Owner.Name)

	pvcID := cluster + "/shop/canary-data"
	tiers := map[string]bool{}
	var sawPVCPod bool
	for _, e := range body.Elements.Edges {
		tier := e.Data.Labels["tier"]
		tiers[tier] = true
		if tier == graph.StorageTierPVCPod && e.Data.Source == pvcID && e.Data.Target == pod.ID {
			sawPVCPod = true
		}
	}
	assert.True(t, sawPVCPod, "the claim is tracked onto the pod")
	assert.True(t, tiers[graph.StorageTierNodeAggr], "the claim's storage path is drawn")
	assert.True(t, tiers[graph.StorageTierSVMPVC])
}

// Spec: "A seeded pod whose resolved Application differs is dropped".
func TestApplicationSeed_ResolvedApplicationDiffersIsDropped(t *testing.T) {
	pod := model.Vector{planKSM("namespace", "shop", "pod", "web-0", "uid", "uid-w0", "node", "worker-1")}
	// 200 is later than 100. The lexically smaller app is w, so a latest-wins
	// pick would keep the pod and the two bodies would differ.
	both := model.Vector{
		appMetaSample(map[string]string{
			"az": "zone-a", "env": "prod", "cluster": "c1",
			"namespace": "shop", "pod": "web-0",
			"argocd_app": "x", "owner": "team-a", "user": "alice",
		}, 200),
		appMetaSample(map[string]string{
			"az": "zone-a", "env": "prod", "cluster": "c1",
			"namespace": "shop", "pod": "web-0",
			"argocd_app": "w", "owner": "team-a", "user": "alice",
		}, 100),
	}
	onlyW := model.Vector{both[1]}
	build := func(rows model.Vector) (cytoscape.Body, *promqlfake.Querier) {
		t.Helper()
		scope := vlrScope(t, graph.StorageRootApplication, []string{"x"})
		q := promqlfake.New(map[promql.Query]model.Vector{
			promql.QPodInfo:     pod,
			promql.QAppMetadata: rows,
		})
		g, err := New(q, appMetadataOpts(), nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
		require.NoError(t, err)
		return cytoscape.Serialise(g, graph.ProjectStorage(g, scope)), q
	}
	bodyBoth, qBoth := build(both)
	bodyW, qW := build(onlyW)
	assert.JSONEq(t, planBodyJSON(t, bodyW), planBodyJSON(t, bodyBoth))
	assert.NotContains(t, planBodyJSON(t, bodyBoth), "web-0")
	assert.NotContains(t, planBodyJSON(t, bodyBoth), "uid-w0")
	assert.Contains(t, strings.Join(qBoth.QueriesFor(promql.QPodInfo), "\n"), "web-0", "the seed loads the pod")
	assert.NotContains(t, strings.Join(qW.QueriesFor(promql.QPodInfo), "\n"), "web-0")
}

// Spec: "Unserved metric issues no seed".
func TestApplicationSeed_UnservedIssuesNoSeed(t *testing.T) {
	fx := map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", trackingLabel, "checkout:apps/Deployment:shop/web"),
		},
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-7d9f", "owner_kind", "Deployment", "owner_name", "web"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "shop", "pod", "web-7d9f-abc", "owner_kind", "ReplicaSet", "owner_name", "web-7d9f", "owner_is_controller", "true"),
		},
		promql.QPodInfo: {
			planKSM("namespace", "shop", "pod", "web-7d9f-abc", "uid", "uid-web", "node", "worker-1"),
			planKSM("namespace", "shop", "pod", "canary-0", "uid", "uid-canary", "node", "worker-1"),
		},
		promql.QAppMetadata: {
			appMetaRow("shop", "canary-0", "checkout", "team-c", "carol"),
		},
	}
	scope := vlrScope(t, graph.StorageRootApplication, []string{"checkout"})
	draw := func(q promql.Querier, opts Options) cytoscape.Body {
		t.Helper()
		g, err := New(q, opts, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
		require.NoError(t, err)
		return cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	}
	unsetQ := promqlfake.New(fx)
	unsetBody := draw(unsetQ, Options{})
	inner := promqlfake.New(fx)
	unservedBody := draw(appMetaUnserved{inner}, appMetadataOpts())
	assert.JSONEq(t, planBodyJSON(t, unsetBody), planBodyJSON(t, unservedBody))
	assert.Empty(t, unsetQ.QueriesFor(promql.QAppMetadata))
	assert.Empty(t, inner.QueriesFor(promql.QAppMetadata))
	assert.Contains(t, planBodyJSON(t, unservedBody), "uid-web")
	assert.NotContains(t, planBodyJSON(t, unservedBody), "uid-canary")
}

// Spec: "Stage 1, the claim-annotation read and the application-metadata seed
// are request-derived" — a set the tracking-id cap accepts and the wider
// app-label cap rejects is 400 before any query. An unserved family does not
// count the metric chunks.
func TestApplicationSeed_OversizedMetricRootSetRejected(t *testing.T) {
	const budget = 38
	apps := make([]string, maxApplicationRootChunks+1)
	for i := range apps {
		apps[i] = string(rune('a' + i))
	}
	src := defaultAppMetadata()
	tracking := len(applicationRootChunks(apps, budget))
	metric := len(appMetadataRootChunks(src, promql.LabelKeys{}, vlrSel, apps, budget))
	require.LessOrEqual(t, tracking, maxApplicationRootChunks, "tracking chunks %d", tracking)
	require.Greater(t, metric, maxApplicationRootChunks, "metric chunks %d", metric)

	opts := Options{AppMetadata: &src, QoSScopeBatchBytes: budget}
	scope := vlrScope(t, graph.StorageRootApplication, apps)

	buildQ := promqlfake.New(nil)
	_, err := New(buildQ, opts, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Empty(t, buildQ.Issued())

	direct := promqlfake.New(nil)
	_, err = readTopology(t.Context(), direct, time.Minute, vlrEnd, opts, vlrSel, storagePlan(scope.Roots))
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Empty(t, direct.Issued())

	inner := promqlfake.New(nil)
	_, err = New(appMetaUnserved{inner}, opts, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)
	assert.Empty(t, inner.QueriesFor(promql.QAppMetadata))
	assert.NotEmpty(t, inner.QueriesFor(promql.QDeploymentAnnotations), "annotation recovery still runs")
}

// Spec: "A request with another root kind issues no recovery" — the namespace
// read may still run; the app-label seed must not.
func TestApplicationSeed_OtherRootKindsIssueNoAppRestriction(t *testing.T) {
	opts := appMetadataOpts()
	t.Run("pvc", func(t *testing.T) {
		q := promqlfake.New(appMetaClaimEstate())
		_, err := New(q, opts, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, appMetaClaimScope(t).Roots)
		require.NoError(t, err)
		got := q.QueriesFor(promql.QAppMetadata)
		require.NotEmpty(t, got, "the namespace read still runs")
		for _, query := range got {
			assert.NotContains(t, query, "argocd_app=")
		}
	})
	t.Run("aggr", func(t *testing.T) {
		fx := claimSeedEstate()
		fx[promql.QAppMetadata] = model.Vector{appMetaRow("shop", "orders-0", "shop-v2", "team-a", "alice")}
		q := promqlfake.New(fx)
		_, err := New(q, opts, nil, nil).BuildStorage(
			t.Context(), time.Minute, vlrEnd, vlrSel, vlrScope(t, graph.StorageRootAggr, []string{"aggr1"}).Roots)
		require.NoError(t, err)
		got := q.QueriesFor(promql.QAppMetadata)
		require.NotEmpty(t, got, "the namespace read still runs")
		for _, query := range got {
			assert.NotContains(t, query, "argocd_app=")
		}
	})
}

// Spec: "A chunk error SHALL fail the build" for the application-metadata seed.
func TestApplicationSeed_ChunkErrorFailsClosed(t *testing.T) {
	q := promqlfake.New(canaryAppEstate())
	q.Fail = func(name, _ string) error {
		if name == string(promql.QAppMetadata) {
			return assert.AnError
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := New(q, appMetadataOpts(), nil, nil).BuildStorage(
		ctx, time.Minute, vlrEnd, vlrSel, vlrScope(t, graph.StorageRootApplication, []string{"canary-app"}).Roots)
	require.Error(t, err)
	require.NoError(t, ctx.Err(), "the build returned on the chunk error instead of running into the deadline")
	be, ok := errors.AsType[*Error](err)
	require.True(t, ok)
	assert.Equal(t, ReasonUpstream, be.Reason)
	assert.Equal(t, string(promql.QAppMetadata), be.Query)
}
