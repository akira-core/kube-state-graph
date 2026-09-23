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
	names, err := readScopedApplications(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(), opts, sel, roots, v, &mu)
	return names, v, err
}

func TestPodScopeUnderApp(t *testing.T) {
	bind := func(cluster, ns, pod, claim string) *model.Sample {
		return &model.Sample{Metric: model.Metric{
			"cluster": model.LabelValue(cluster), "namespace": model.LabelValue(ns),
			"pod": model.LabelValue(pod), "persistentvolumeclaim": model.LabelValue(claim),
		}, Value: 1}
	}
	ann := func(cluster, ns, claim, app string) *model.Sample {
		return &model.Sample{Metric: model.Metric{
			"cluster": model.LabelValue(cluster), "namespace": model.LabelValue(ns),
			"persistentvolumeclaim": model.LabelValue(claim),
			trackingLabel:           model.LabelValue(app + ":apps/PersistentVolumeClaim:" + ns + "/" + claim),
		}, Value: 1}
	}
	bindings := model.Vector{
		bind("c1", "shop", "orders-0", "orders-data"),
		bind("c1", "shop", "catalog-0", "catalog-data"),
		bind("c1", "shop", "ledger-0", "ledger-data"),
		bind("c1", "shop", "report-0", "shared-data"),
		bind("c1", "shop", "orders-0", "shared-data"),
		bind("c1", "platform", "other-0", "orders-data"),
		bind("c1", "shop", "root-mate", "root-data"),
		bind("c1", "shop", "web-0", "root-data"),
	}
	annotations := model.Vector{ann("c1", "shop", "ledger-data", "billing")}

	// Unrelated catalog-0 is excluded; orders-0's co-mounter report-0 is kept;
	// the own-annotated ledger claim pulls ledger-0; platform's same-named
	// claim does not.
	got := podScopeUnderApp(bindings, annotations, []string{"orders-0"}, nil, []string{"checkout"})
	assert.Equal(t, []string{"orders-0", "report-0"}, got, "catalog-0 is unrelated; ledger-data is annotated billing, not checkout")

	// A pod= root pulls its co-mounters the same way.
	rooted := podScopeUnderApp(bindings, nil, nil, []string{"web-0"}, []string{"checkout"})
	assert.Equal(t, []string{"root-mate", "web-0"}, rooted)

	// Own-annotated claim with no recovered pod.
	own := podScopeUnderApp(bindings, annotations, nil, nil, []string{"billing"})
	assert.Equal(t, []string{"ledger-0"}, own)

	// Input order does not change the scope.
	reversed := model.Vector{bindings[7], bindings[1], bindings[5], bindings[0], bindings[3], bindings[2], bindings[4], bindings[6]}
	assert.Equal(t, got, podScopeUnderApp(reversed, annotations, []string{"orders-0"}, nil, []string{"checkout"}))
	assert.Empty(t, podScopeUnderApp(nil, nil, nil, nil, []string{"checkout"}))
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

	scope := graph.StorageScope{Roots: graph.StorageRoots{Applications: map[string]struct{}{"reports": {}}}}
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

func TestReadScopedApplications_UnboundedRootSetReadsUnrestricted(t *testing.T) {
	const keep = "keep"
	apps := make([]string, 0, maxApplicationRootChunks+2)
	apps = append(apps, keep)
	for i := range maxApplicationRootChunks + 1 {
		apps = append(apps, "app-"+strings.Repeat("n", 8)+string(rune('a'+i%26))+string(rune('0'+i%10)))
	}
	// Distinct, stable names: the chunker counts values, not their spelling.
	apps = apps[:1]
	for i := range maxApplicationRootChunks + 1 {
		apps = append(apps, "app"+strings.Repeat("x", 3)+itoa(i))
	}
	fixtures := map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", trackingLabel, keep+":apps/Deployment:shop/web"),
			planKSM("namespace", "shop", "deployment", "other", trackingLabel, "other:apps/Deployment:shop/other"),
		},
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-1", "owner_kind", "Deployment", "owner_name", "web"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "shop", "pod", "web-1-pod", "owner_kind", "ReplicaSet", "owner_name", "web-1", "owner_is_controller", "true"),
		},
	}
	wide, _, err := recoverNames(t, promqlfake.New(fixtures), Options{}, promql.Selector{}, apps)
	require.NoError(t, err)

	narrowQ := promqlfake.New(fixtures)
	var narrow []string
	recs := captureDebugRecords(t, func() {
		var recErr error
		narrow, _, recErr = recoverNames(t, narrowQ, Options{QoSScopeBatchBytes: 1}, promql.Selector{}, apps)
		require.NoError(t, recErr)
	})
	assert.Equal(t, wide, narrow)
	assert.Equal(t, []string{"web-1-pod"}, narrow)

	for _, q := range []promql.Query{
		promql.QDeploymentAnnotations, promql.QStatefulSetAnnotations, promql.QDaemonSetAnnotations,
		promql.QCronJobAnnotations, promql.QReplicaSetAnnotations, promql.QJobAnnotations,
	} {
		got := narrowQ.QueriesFor(q)
		require.Len(t, got, 1, "unbounded stage 1 issues the family once")
		assert.NotContains(t, got[0], "(?:", "the fallback is the unrestricted rendering")
		assert.Contains(t, got[0], trackingLabel+`!=""`)
	}
	logged := false
	for _, rec := range recs {
		if rec["msg"] == "application_root_restriction_unbounded" {
			logged = true
		}
	}
	assert.True(t, logged, "the build logs that the roots did not yield a bounded restriction")
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
	var recovered []string
	errCh := make(chan error, 1)
	go func() {
		v := &topologyVectors{PVC: model.Vector{podBinding("orders-0")}}
		var mu sync.Mutex
		errCh <- readScopedPods(context.Background(), f, time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{},
			nil, []string{"checkout"}, v, &mu, bindingsDone, appDone, pvcDone, &recovered)
	}()

	assert.Never(t, func() bool { return len(f.QueriesFor(promql.QPodInfo)) > 0 }, 80*time.Millisecond, 10*time.Millisecond,
		"the pod read waits for the recovery")
	recovered = []string{"orders-0"}
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
	v := &topologyVectors{PVC: model.Vector{
		planKSM("namespace", "shop", "pod", "orders-0", "persistentvolumeclaim", "orders-data"),
		planKSM("namespace", "shop", "pod", "catalog-0", "persistentvolumeclaim", "catalog-data"),
		planKSM("namespace", "shop", "pod", "ledger-0", "persistentvolumeclaim", "ledger-data"),
	}, PVCAnnotations: model.Vector{
		planKSM("namespace", "shop", "persistentvolumeclaim", "ledger-data", trackingLabel, "billing:apps/PersistentVolumeClaim:shop/ledger-data"),
	}}
	done := make(chan struct{})
	close(done)
	recovered := []string{"orders-0"}
	var mu sync.Mutex
	err := readScopedPods(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(), Options{}, storageSel,
		nil, []string{"checkout"}, v, &mu, done, done, done, &recovered)
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"orders-0"}}, f.ScopeValues(promql.QPodInfo, "pod"))
	assert.Equal(t, [][]string{{"orders-0"}}, f.ScopeValues(promql.QPodOwner, "pod"))
}
