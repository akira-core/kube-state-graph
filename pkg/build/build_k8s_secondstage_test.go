package build

import (
	"slices"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// roundsOf returns the indices of the rounds that carried a query of family q.
func roundsOf(b lockstepBuild, q promql.Query) []int {
	var out []int
	for i, round := range b.Rounds {
		if slices.Contains(round, string(q)) {
			out = append(out, i)
		}
	}
	return out
}

func appendRows(fx map[promql.Query]model.Vector, q promql.Query, rows ...*model.Sample) {
	fx[q] = append(slices.Clone(fx[q]), rows...)
}

// deploymentEstate: shop/orders-abc on worker-1, owned by ReplicaSet
// orders-7c9f8d, which Deployment orders owns and which carries the Application.
func deploymentEstate() map[promql.Query]model.Vector {
	fx := miniEstate(miniPod{ns: "shop", name: "orders-abc", node: "worker-1", owner: "orders-7c9f8d", ownerKind: "ReplicaSet"})
	appendRows(fx, promql.QReplicaSetOwner, planKSM("namespace", "shop", "replicaset", "orders-7c9f8d",
		"owner_kind", "Deployment", "owner_name", "orders"))
	appendRows(fx, promql.QDeploymentAnnotations, planKSM("namespace", "shop", "deployment", "orders",
		trackingIDLabel, "storefront:apps/Deployment:shop/orders"))
	return fx
}

// cronJobEstate: shop/run-x on worker-1, owned by the named Job, which CronJob
// nightly controls and which carries no tracking-id of its own.
func cronJobEstate(job string) map[promql.Query]model.Vector {
	fx := miniEstate(miniPod{ns: "shop", name: "run-x", node: "worker-1", owner: job, ownerKind: "Job"})
	appendRows(fx, promql.QJobOwner, planKSM("namespace", "shop", "job_name", job,
		"owner_kind", "CronJob", "owner_name", "nightly", "owner_is_controller", "true"))
	appendRows(fx, promql.QCronJobAnnotations, planKSM("namespace", "shop", "cronjob", "nightly",
		trackingIDLabel, "reports:batch/CronJob:shop/nightly"))
	return fx
}

func podOf(t *testing.T, g *graph.Graph, name string) graph.GraphNode {
	t.Helper()
	n := g.NodesByID[graph.PodID(k8sCluster, "uid-shop-"+name)]
	require.NotNil(t, n, name)
	return n
}

// Spec: "A Deployment is read with its ReplicaSet". kube_replicaset_owner and
// kube_deployment_annotations are issued in the same round, and nothing reads a
// Deployment afterwards: the guess was right.
func TestBuildK8s_ADeploymentIsReadWithItsReplicaSet(t *testing.T) {
	fx := deploymentEstate()
	for kind, roots := range map[string]graph.StorageRoots{
		"node root": k8sRootsOf(t, graph.StorageRootNode, "worker-1"),
		"pod root":  k8sRootsOf(t, graph.StorageRootPod, "shop/orders-abc"),
	} {
		t.Run(kind, func(t *testing.T) {
			_, q := buildK8sOver(t, fx, Options{}, roots)
			assert.Equal(t, [][]string{{"orders-7c9f8d"}}, q.ScopeValues(promql.QReplicaSetOwner, "replicaset"))
			assert.Equal(t, [][]string{{"orders"}}, q.ScopeValues(promql.QDeploymentAnnotations, "deployment"),
				"one Deployment query, and no further one")

			b := runLockstep(t, fx, Options{}, roots)
			assert.Equal(t, roundsOf(b, promql.QReplicaSetOwner), roundsOf(b, promql.QDeploymentAnnotations),
				"the same round: %v", b.Rounds)
			assert.Equal(t, "storefront", podOf(t, b.Graph, "orders-abc").Application())
			assert.Equal(t, &graph.Owner{Kind: "Deployment", Name: "orders"}, podOf(t, b.Graph, "orders-abc").Owner())
		})
	}
	requireK8sOutputPreserved(t, fx, Options{}, vlrSel, vlrScope(t, graph.StorageRootNode, []string{"worker-1"}))
	requireK8sOutputPreserved(t, fx, Options{}, vlrSel, vlrScope(t, graph.StorageRootPod, []string{"shop/orders-abc"}))
}

// Spec: "A CronJob is read with its Job".
func TestBuildK8s_ACronJobIsReadWithItsJob(t *testing.T) {
	fx := cronJobEstate("nightly-29318400")
	for kind, roots := range map[string]graph.StorageRoots{
		"node root": k8sRootsOf(t, graph.StorageRootNode, "worker-1"),
		"pod root":  k8sRootsOf(t, graph.StorageRootPod, "shop/run-x"),
	} {
		t.Run(kind, func(t *testing.T) {
			_, q := buildK8sOver(t, fx, Options{}, roots)
			assert.Equal(t, [][]string{{"nightly-29318400"}}, q.ScopeValues(promql.QJobOwner, "job_name"))
			assert.Equal(t, [][]string{{"nightly"}}, q.ScopeValues(promql.QCronJobAnnotations, "cronjob"),
				"one CronJob query, and no further one")

			b := runLockstep(t, fx, Options{}, roots)
			assert.Equal(t, roundsOf(b, promql.QJobOwner), roundsOf(b, promql.QCronJobAnnotations),
				"the same round: %v", b.Rounds)
			assert.Equal(t, "reports", podOf(t, b.Graph, "run-x").Application())
		})
	}
	requireK8sOutputPreserved(t, fx, Options{}, vlrSel, vlrScope(t, graph.StorageRootNode, []string{"worker-1"}))
	requireK8sOutputPreserved(t, fx, Options{}, vlrSel, vlrScope(t, graph.StorageRootPod, []string{"shop/run-x"}))
}

// Spec: "A name outside the convention is read afterwards". Job manual-run was
// made by hand from CronJob nightly, so the early read guesses `manual`; once
// kube_job_owner returns, nightly is read in the round after, and the pod still
// resolves nightly's Application.
func TestBuildK8s_ANameOutsideTheConventionIsReadAfterwards(t *testing.T) {
	fx := cronJobEstate("manual-run")
	for kind, roots := range map[string]graph.StorageRoots{
		"node root": k8sRootsOf(t, graph.StorageRootNode, "worker-1"),
		"pod root":  k8sRootsOf(t, graph.StorageRootPod, "shop/run-x"),
	} {
		t.Run(kind, func(t *testing.T) {
			_, q := buildK8sOver(t, fx, Options{}, roots)
			assert.Equal(t, [][]string{{"manual"}, {"nightly"}}, sortedScopes(q.ScopeValues(promql.QCronJobAnnotations, "cronjob")),
				"the guess, then the name kube_job_owner revealed, each once")

			b := runLockstep(t, fx, Options{}, roots)
			jobOwner := roundsOf(b, promql.QJobOwner)
			cronJobs := roundsOf(b, promql.QCronJobAnnotations)
			require.Len(t, jobOwner, 1)
			require.Len(t, cronJobs, 2)
			assert.Equal(t, jobOwner[0], cronJobs[0], "the guess is read with kube_job_owner")
			assert.Equal(t, jobOwner[0]+1, cronJobs[1], "the real name the round after: %v", b.Rounds)
			assert.Equal(t, "reports", podOf(t, b.Graph, "run-x").Application(), "nightly's Application")
		})
	}
	requireK8sOutputPreserved(t, fx, Options{}, vlrSel, vlrScope(t, graph.StorageRootNode, []string{"worker-1"}))
	requireK8sOutputPreserved(t, fx, Options{}, vlrSel, vlrScope(t, graph.StorageRootPod, []string{"shop/run-x"}))
}

// Spec: "A wrong early name leaves the body unchanged". ReplicaSet api-5d4f has
// no Deployment owner, yet a Deployment api carrying a tracking-id exists in its
// namespace: the guess returns api's annotation row, which no loaded pod's owner
// chain consults, so the pod stays owned by the bare ReplicaSet and resolves no
// Application.
func TestBuildK8s_AWrongEarlyNameLeavesTheBodyUnchanged(t *testing.T) {
	fx := miniEstate(miniPod{ns: "shop", name: "api-pod", node: "worker-1", owner: "api-5d4f", ownerKind: "ReplicaSet"})
	appendRows(fx, promql.QDeploymentAnnotations, planKSM("namespace", "shop", "deployment", "api",
		trackingIDLabel, "ledger:apps/Deployment:shop/api"))

	for kind, kindRoots := range map[graph.StorageRootKind]string{
		graph.StorageRootNode: "worker-1",
		graph.StorageRootPod:  "shop/api-pod",
	} {
		scope := vlrScope(t, kind, []string{kindRoots})
		g := requireK8sOutputPreserved(t, fx, Options{}, vlrSel, scope)
		pod := podOf(t, g, "api-pod")
		assert.Equal(t, &graph.Owner{Kind: "ReplicaSet", Name: "api-5d4f"}, pod.Owner())
		assert.Empty(t, pod.Application(), "the Deployment's Application does not leak onto a bare ReplicaSet")

		_, q := buildK8sOver(t, fx, Options{}, scope.Roots)
		assert.Equal(t, [][]string{{"api"}}, q.ScopeValues(promql.QDeploymentAnnotations, "deployment"),
			"the guess was issued, and nothing followed it")
	}
}

// A ReplicaSet or Job name with no `-` implies no owner, and the second stage
// still finds the real one.
func TestBuildK8s_ANameWithNoSegmentImpliesNothing(t *testing.T) {
	fx := miniEstate(miniPod{ns: "shop", name: "solo-pod", node: "worker-1", owner: "solo", ownerKind: "ReplicaSet"})
	appendRows(fx, promql.QReplicaSetOwner, planKSM("namespace", "shop", "replicaset", "solo",
		"owner_kind", "Deployment", "owner_name", "real-deployment"))
	appendRows(fx, promql.QDeploymentAnnotations, planKSM("namespace", "shop", "deployment", "real-deployment",
		trackingIDLabel, "found:apps/Deployment:shop/real-deployment"))

	scope := vlrScope(t, graph.StorageRootNode, []string{"worker-1"})
	g := requireK8sOutputPreserved(t, fx, Options{}, vlrSel, scope)
	assert.Equal(t, "found", podOf(t, g, "solo-pod").Application())

	_, q := buildK8sOver(t, fx, Options{}, scope.Roots)
	assert.Equal(t, [][]string{{"real-deployment"}}, q.ScopeValues(promql.QDeploymentAnnotations, "deployment"),
		"no early read at all: there is nothing to guess from")
}

// docs/upstream-metrics.md: a miss of the early controller read costs one round
// — except for the root pods' owners when a co-mounter is read anyway, where the
// fallback rides the co-mounter's pod round.
func TestBuildK8s_AnEarlyMissCostsAtMostOneRound(t *testing.T) {
	handMadeJob := func(fx map[promql.Query]model.Vector) {
		appendRows(fx, promql.QJobOwner, planKSM("namespace", "shop", "job_name", "manual-run",
			"owner_kind", "CronJob", "owner_name", "nightly", "owner_is_controller", "true"))
		appendRows(fx, promql.QCronJobAnnotations, planKSM("namespace", "shop", "cronjob", "nightly",
			trackingIDLabel, "reports:batch/CronJob:shop/nightly"))
	}
	node := k8sRootsOf(t, graph.StorageRootNode, "worker-1")

	t.Run("node root, no co-mounter", func(t *testing.T) {
		b := runLockstep(t, cronJobEstate("manual-run"), Options{}, node)
		assert.Len(t, b.Rounds, 4, "rounds: %v", b.Rounds)
	})
	t.Run("pod root, no co-mounter", func(t *testing.T) {
		b := runLockstep(t, cronJobEstate("manual-run"), Options{}, k8sRootsOf(t, graph.StorageRootPod, "shop/run-x"))
		assert.Len(t, b.Rounds, 3, "rounds: %v", b.Rounds)
	})
	t.Run("a co-mounter, the root pod's owner misses", func(t *testing.T) {
		fx := miniEstate(
			miniPod{ns: "shop", name: "run-x", node: "worker-1", owner: "manual-run", ownerKind: "Job", claim: "shared"},
			miniPod{ns: "shop", name: "report-0", node: "worker-2", owner: "report-sts", claim: "shared"},
		)
		handMadeJob(fx)
		b := runLockstep(t, fx, Options{}, node)
		assert.Len(t, b.Rounds, 5, "the fallback rides r4: %v", b.Rounds)
		assert.Equal(t, "reports", podOf(t, b.Graph, "run-x").Application())
	})
	t.Run("a co-mounter, its own owner misses", func(t *testing.T) {
		fx := miniEstate(
			miniPod{ns: "shop", name: "orders-0", node: "worker-1", owner: "orders-sts", claim: "shared"},
			miniPod{ns: "shop", name: "run-x", node: "worker-2", owner: "manual-run", ownerKind: "Job", claim: "shared"},
		)
		handMadeJob(fx)
		b := runLockstep(t, fx, Options{}, node)
		assert.Len(t, b.Rounds, 6, "r4 pods, r5 controllers, r6 the second stage: %v", b.Rounds)
	})
}
