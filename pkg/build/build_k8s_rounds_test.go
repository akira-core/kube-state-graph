package build

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// Spec: "Reads follow their real dependencies". The number of sequential
// upstream rounds a Kubernetes-only build takes is the length of its critical
// path, measured by the lockstep querier rather than inferred from the code.
// Both variants matter: a StatefulSet owner has no second controller stage, so
// it isolates the rounds the pod, claim and node reads cost; a Deployment owner
// adds the ReplicaSet -> Deployment hop, which is what the early second stage
// reads ahead.
var lockstepOwners = map[string]lockstepOwner{
	"statefulset-owned": ownedByStatefulSet,
	"deployment-owned":  ownedByDeployment,
}

// Scenario "Node root with no co-mounter completes in three rounds".
func TestBuildK8s_NodeRootCompletesInThreeRounds(t *testing.T) {
	for name, owner := range lockstepOwners {
		t.Run(name, func(t *testing.T) {
			b := runLockstep(t, lockstepEstate(owner), Options{}, k8sRootsOf(t, graph.StorageRootNode, "worker-1"))
			require.NotNil(t, b.Graph.NodesByID[graph.PodID(k8sCluster, "uid-a0")], "the root node's pod is drawn")
			assert.Len(t, b.Rounds, 3, "rounds: %v", b.Rounds)
		})
	}
}

// Scenario "Pod root with no co-mounter completes in two rounds".
func TestBuildK8s_PodRootCompletesInTwoRounds(t *testing.T) {
	for name, owner := range lockstepOwners {
		t.Run(name, func(t *testing.T) {
			b := runLockstep(t, lockstepEstate(owner), Options{}, k8sRootsOf(t, graph.StorageRootPod, "shop/app-0"))
			require.NotNil(t, b.Graph.NodesByID[graph.PodID(k8sCluster, "uid-a0")], "the root pod is drawn")
			assert.Len(t, b.Rounds, 2, "rounds: %v", b.Rounds)
		})
	}
}

// docs/upstream-metrics.md and .claude/rules/storage-graph.md list what each
// round carries. This pins that list on the smallest estate, so the documented
// order cannot drift from the order the build issues its reads in.
func TestBuildK8s_RoundsCarryTheDocumentedReads(t *testing.T) {
	const (
		podInfo  = string(promql.QPodInfo)
		podOwner = string(promql.QPodOwner)
		bindings = string(promql.QPVCBindings)
		alerts   = string(promql.QAlerts)
	)
	claimFamilies := []string{
		string(promql.QPVCBindings), // mounter completion
		string(promql.QPVCInfo), string(promql.QPVCAnnotations),
		string(promql.QKubeletVolumeUsedBytes), string(promql.QKubeletVolumeCapacityBytes),
	}
	nodeFamilies := []string{
		string(promql.QNodeInfo), string(promql.QNodeAddresses),
		string(promql.QNodeLabels), string(promql.QNodeStatusCondition),
	}
	// What r3 of a node root, and r2 of a pod root, carries: everything that
	// depends on the root pods alone.
	fanOut := slices.Concat(claimFamilies, nodeFamilies, []string{string(promql.QStatefulSetAnnotations)})
	slices.Sort(fanOut)

	t.Run("node root", func(t *testing.T) {
		b := runLockstep(t, lockstepEstate(ownedByStatefulSet), Options{}, k8sRootsOf(t, graph.StorageRootNode, "worker-1"))
		require.Len(t, b.Rounds, 3)
		assert.Equal(t, []string{alerts, podInfo}, b.Rounds[0], "r1: the seed beside ALERTS")
		assert.Equal(t, []string{podInfo, podOwner, bindings}, b.Rounds[1],
			"r2: incarnation completion, kube_pod_owner and the claim bindings, on the seed's pairs")
		assert.Equal(t, fanOut, b.Rounds[2], "r3: claims, nodes and controllers together")
	})

	t.Run("pod root mounting nothing", func(t *testing.T) {
		fx := lockstepEstate(ownedByStatefulSet)
		for _, q := range promql.ClaimScopedQueries {
			delete(fx, q)
		}
		b := runLockstep(t, fx, Options{}, k8sRootsOf(t, graph.StorageRootPod, "shop/app-0"))
		require.Len(t, b.Rounds, 2, "rounds: %v", b.Rounds)
		assert.Equal(t, []string{alerts, podInfo, podOwner, bindings}, b.Rounds[0],
			"r1: the seed still reads the bindings, and finds none")
		nodesAndControllers := slices.Concat(nodeFamilies, []string{string(promql.QStatefulSetAnnotations)})
		slices.Sort(nodesAndControllers)
		assert.Equal(t, nodesAndControllers, b.Rounds[1], "r2: no claim read at all, only nodes and controllers")
	})

	t.Run("pod root", func(t *testing.T) {
		b := runLockstep(t, lockstepEstate(ownedByStatefulSet), Options{}, k8sRootsOf(t, graph.StorageRootPod, "shop/app-0"))
		require.Len(t, b.Rounds, 2)
		assert.Equal(t, []string{alerts, podInfo, podOwner, bindings}, b.Rounds[0],
			"r1: the claim-binding seed with the roots' kube_pod_info and kube_pod_owner")
		assert.Equal(t, fanOut, b.Rounds[1], "r2: as r3 of a node root")
	})
}
