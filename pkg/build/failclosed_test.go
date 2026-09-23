package build

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// The storage plan fails closed on every first-wave leg but ALERTS; the full
// plan keeps each family's own class (fail-storage-graph-on-any-leg-error).
func TestTopologyPlan_FailsClosed(t *testing.T) {
	storage := storagePlan(graph.StorageRoots{})
	var v topologyVectors
	for _, l := range topologyLegs(&v) {
		assert.False(t, fullPlan.failsClosed(l.query), "fullPlan keeps %s's own class", l.query)
		assert.Equal(t, l.query != promql.QAlerts, storage.failsClosed(l.query), "storagePlan / %s", l.query)
	}
}

// storageFailFixture is a minimal estate with one claim mounted by one pod,
// so every first-wave leg of the storage plan is issued.
func storageFailFixture() map[promql.Query]model.Vector {
	return map[promql.Query]model.Vector{
		promql.QPVCBindings: {planKSM("namespace", "shop", "pod", "orders-0", "persistentvolumeclaim", "orders-data")},
		promql.QPVCInfo:     {planKSM("namespace", "shop", "persistentvolumeclaim", "orders-data", "volumename", "pvc-orders")},
		promql.QPodInfo:     {planKSM("namespace", "shop", "pod", "orders-0", "uid", "uid-o0", "node", "n1")},
	}
}

// Spec: "A volume-label failure fails the storage request", "A kubelet failure
// fails the storage request" and the first-wave half of "Storage build fails
// closed on upstream query errors" — every family /v1/graph degrades.
func TestBuildStorage_FirstWaveOptionalLegFailsClosed(t *testing.T) {
	for _, q := range []promql.Query{
		promql.QVolumeLabels,
		promql.QAggrStatus, promql.QAggrSpaceUsed, promql.QAggrSpaceTotal,
		promql.QNetAppNodeStatus, promql.QNetAppNodeLabels,
		promql.QNetAppNodeCPUBusy, promql.QNetAppNodeTotalOps, promql.QNetAppNodeTotalLatency, promql.QNetAppNodeTotalData,
		promql.QQoSPolicyFixedMaxIOPS, promql.QQoSPolicyFixedMaxMBps,
		promql.QKubeletVolumeUsedBytes, promql.QKubeletVolumeCapacityBytes,
	} {
		t.Run(string(q), func(t *testing.T) {
			f := promqlfake.New(storageFailFixture())
			f.Fail = func(name, _ string) error {
				if name == string(q) {
					return errors.New(`Post "http://vm-internal:8428/api/v1/query": 503`)
				}
				return nil
			}
			_, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, graph.StorageRoots{})
			require.Error(t, err)
			be, ok := errors.AsType[*Error](err)
			require.True(t, ok)
			assert.Equal(t, ReasonUpstream, be.Reason)
			assert.Equal(t, string(q), be.Query)
		})
	}
}

// Spec: "An ALERTS failure does not fail the storage request".
func TestBuildStorage_AlertsFailureDegrades(t *testing.T) {
	f := promqlfake.New(storageFailFixture())
	f.Fail = func(name, _ string) error {
		if name == string(promql.QAlerts) {
			return errors.New("alert store down")
		}
		return nil
	}
	g, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, graph.StorageRoots{})
	require.NoError(t, err)
	for _, n := range g.NodesByID {
		assert.Empty(t, n.Alerts(), n.ID())
	}
}

// Spec: "An empty family is not a failure" — absence is not an error.
func TestBuildStorage_EmptyFamiliesDoNotFail(t *testing.T) {
	f := promqlfake.New(storageFailFixture()) // every Harvest, kubelet and annotation family empty
	_, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, graph.StorageRoots{})
	require.NoError(t, err)
}

// Spec: "The graph endpoint keeps degrading".
func TestBuild_GraphKeepsDegradingOnVolumeLabels(t *testing.T) {
	f := promqlfake.New(storageFailFixture())
	f.Fail = func(name, _ string) error {
		if name == string(promql.QVolumeLabels) {
			return errors.New("upstream 5xx")
		}
		return nil
	}
	_, err := New(f, Options{}, nil, nil).Build(t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel)
	require.NoError(t, err)
}
