package build

import (
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// A family's tally is the total number of series every read of the build
// matched, so the seed reads that land nowhere but a local — the node seed's
// pod-by-node and incarnation reads, and every seed's bindings-by-pod read —
// count beside the reads that fill the family's slot.

// tallyEstate: web-0 and api-0 run on worker-1, db-0 on worker-2; web-0 and
// db-0 share claim web-data, api-0 mounts nothing.
func tallyEstate() map[promql.Query]model.Vector {
	return map[promql.Query]model.Vector{
		promql.QPodInfo: {
			planKSM("namespace", "shop", "pod", "web-0", "uid", "u-web", "node", "worker-1"),
			planKSM("namespace", "shop", "pod", "api-0", "uid", "u-api", "node", "worker-1"),
			planKSM("namespace", "shop", "pod", "db-0", "uid", "u-db", "node", "worker-2"),
		},
		promql.QPVCBindings: {
			planKSM("namespace", "shop", "pod", "web-0", "persistentvolumeclaim", "web-data"),
			planKSM("namespace", "shop", "pod", "db-0", "persistentvolumeclaim", "web-data"),
		},
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", trackingLabel, "checkout:apps/Deployment:shop/web"),
		},
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-1", "owner_kind", "Deployment", "owner_name", "web"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "shop", "pod", "web-0", "owner_kind", "ReplicaSet", "owner_name", "web-1", "owner_is_controller", "true"),
		},
	}
}

func tallyFor(t *testing.T, kind graph.StorageRootKind, values ...string) map[string]int {
	t.Helper()
	scope, err := graph.NewStorageScope(nil, nil, kind, values)
	require.NoError(t, err)
	tp, err := readTopology(t.Context(), promqlfake.New(tallyEstate()), time.Minute, time.Unix(1, 0).UTC(),
		Options{}, storageSel, storagePlan(scope.Roots))
	require.NoError(t, err)
	return tp.RawSeriesCount
}

func TestTallySeries_CountsNodeSeedReads(t *testing.T) {
	raw := tallyFor(t, graph.StorageRootNode, "worker-1")
	// kube_pod_info: by node (web-0, api-0) + incarnation (web-0, api-0) +
	// pod wave (web-0, db-0, the mounters of web-data).
	assert.Equal(t, 6, raw[string(promql.QPodInfo)])
	// bindings: by pod (web-0; api-0 mounts nothing) + by claim (web-0, db-0).
	assert.Equal(t, 3, raw[string(promql.QPVCBindings)])
}

func TestTallySeries_CountsPodSeedBindings(t *testing.T) {
	raw := tallyFor(t, graph.StorageRootPod, "shop/web-0")
	// bindings: the root's own (web-0) + by claim (web-0, db-0).
	assert.Equal(t, 3, raw[string(promql.QPVCBindings)])
}

func TestTallySeries_CountsApplicationSeedBindings(t *testing.T) {
	raw := tallyFor(t, graph.StorageRootApplication, "checkout")
	// bindings: the recovered pod's (web-0) + by claim (web-0, db-0).
	assert.Equal(t, 3, raw[string(promql.QPVCBindings)])
}
