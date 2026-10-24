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

// Spec: "A rescheduled pod still follows its newest incarnation". shop/db-0 ran
// on worker-1 early in the window and was recreated on worker-2 before its end.
// The node root reads bindings and owners on the seed's pairs, beside incarnation
// completion, so it reads db-0's too; they are dropped once the newest
// incarnation is known, its claim is never tracked, and the body is the one an
// unrestricted read produces.
func TestBuildK8s_ARescheduledPodFollowsItsNewestIncarnation(t *testing.T) {
	const tracking = "annotation_argocd_argoproj_io_tracking_id"
	at := func(s *model.Sample, ts model.Time) *model.Sample {
		s.Timestamp = ts
		return s
	}
	fx := planEstate()
	fx[promql.QPodInfo] = append(slices.Clone(fx[promql.QPodInfo]),
		at(planKSM("namespace", "shop", "pod", "db-0", "uid", "uid-db-1", "node", "worker-1"), 1000),
		at(planKSM("namespace", "shop", "pod", "db-0", "uid", "uid-db-2", "node", "worker-2"), 2000))
	fx[promql.QPodOwner] = append(slices.Clone(fx[promql.QPodOwner]),
		planKSM("namespace", "shop", "pod", "db-0", "owner_kind", "StatefulSet", "owner_name", "db-sts", "owner_is_controller", "true"))
	fx[promql.QStatefulSetAnnotations] = append(slices.Clone(fx[promql.QStatefulSetAnnotations]),
		planKSM("namespace", "shop", "statefulset", "db-sts", tracking, "dbapp:apps/StatefulSet:shop/db-sts"))
	fx[promql.QPVCBindings] = append(slices.Clone(fx[promql.QPVCBindings]),
		planKSM("namespace", "shop", "pod", "db-0", "persistentvolumeclaim", "db-data", "volume", "data"))
	fx[promql.QPVCInfo] = append(slices.Clone(fx[promql.QPVCInfo]),
		planKSM("namespace", "shop", "persistentvolumeclaim", "db-data", "volumename", "pvc-db", "storageclass", "netapp-nas"))

	scope := vlrScope(t, graph.StorageRootNode, []string{"worker-1"})
	g := requireK8sOutputPreserved(t, fx, Options{}, vlrSel, scope)
	assert.Nil(t, g.NodesByID[graph.PodID(k8sCluster, "uid-db-1")], "the old incarnation is not drawn")
	assert.Nil(t, g.NodesByID[graph.PodID(k8sCluster, "uid-db-2")], "the new one runs off the root")
	assert.Nil(t, g.NodesByID[graph.PVCID(k8sCluster, "shop", "db-data")], "its claim is not tracked")

	_, q := buildK8sOver(t, fx, Options{}, scope.Roots)
	assert.Contains(t, flatten(q.ScopeValues(promql.QPVCBindings, "pod")), "db-0",
		"the bindings are read on the seed's pairs, before the newest incarnation is known")
	assert.NotContains(t, flatten(q.ScopeValues(promql.QPVCBindings, "persistentvolumeclaim")), "db-data",
		"but the claim is not tracked, so no mounter completion names it")
	assert.NotContains(t, flatten(q.ScopeValues(promql.QStatefulSetAnnotations, "statefulset")), "db-sts",
		"and its controller is never read")
}

// Spec: "A cross pair is discarded", on the shapes of planEstate(): platform/orders-0
// shares its name with shop/orders-0, which runs on the root node, and runs on
// worker-2. The grouped incarnation read matches it, and nothing is read for it.
func TestBuildK8s_ACrossPairRunsOffTheRootNode(t *testing.T) {
	scope := vlrScope(t, graph.StorageRootNode, []string{"worker-1"})
	g := requireK8sOutputPreserved(t, planEstate(), Options{}, vlrSel, scope)
	assert.Nil(t, g.NodesByID[graph.PodID(k8sCluster, "uid-p0")], "platform/orders-0 runs on worker-2")
	assert.NotNil(t, g.NodesByID[graph.PodID(k8sCluster, "uid-o0")], "shop/orders-0 runs on the root")

	_, q := buildK8sOver(t, planEstate(), Options{}, scope.Roots)
	require.Contains(t, flatten(q.ScopeValues(promql.QPodInfo, "namespace")), "platform",
		"the grouped read does name the platform namespace, so it can match the cross pair")
	assert.NotContains(t, flatten(q.ScopeValues(promql.QReplicaSetOwner, "replicaset")), "orders-6c4",
		"the cross pair's controller is never read")
}
