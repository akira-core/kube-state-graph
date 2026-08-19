package build

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// resolveNetApp is the four-family wrapper used by the pre-throughput tests.
func resolveNetApp(
	claims []pvcVolume,
	readOps, writeOps, readLat, writeLat, aggrStatus, aggrUsed, aggrTotal, nodeStatus model.Vector,
) netappResult {
	return resolveNetAppStorage(claims, readOps, writeOps, readLat, writeLat, nil, nil, aggrStatus, aggrUsed, aggrTotal, nodeStatus)
}

func volSample(vn, oc, node, aggr, svm string, val float64) model.Sample {
	m := model.Metric{
		"volume_name": model.LabelValue(vn),
		"cluster":     model.LabelValue(oc),
		"node":        model.LabelValue(node),
		"aggr":        model.LabelValue(aggr),
		"svm":         model.LabelValue(svm),
	}
	return model.Sample{Metric: m, Value: model.SampleValue(val)}
}

func aggrSample(oc, node, aggr string, val float64) model.Sample {
	return model.Sample{
		Metric: model.Metric{
			"cluster": model.LabelValue(oc),
			"node":    model.LabelValue(node),
			"aggr":    model.LabelValue(aggr),
		},
		Value: model.SampleValue(val),
	}
}

func TestResolveNetAppStorage_JoinHit(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-9f3a"}}
	res := resolveNetApp(claims,
		sampleVec(volSample("pvc-9f3a", "ontap-prod", "ontap-prod-01", "aggr1", "svm-prod", 150)),
		sampleVec(volSample("pvc-9f3a", "ontap-prod", "ontap-prod-01", "aggr1", "svm-prod", 40)),
		nil, nil, nil, nil, nil, nil)
	assert.Equal(t, "svm-prod", res.svmByPVC["c/db/data"])
	require.Len(t, res.aggrs, 1)
	assert.Equal(t, graph.NetAppAggrID("ontap-prod", "aggr1"), res.aggrs[0].ID())
	assert.Equal(t, "ontap-prod-01", res.aggrs[0].Labels()["node"])
	require.Len(t, res.nodes, 1)
	assert.Equal(t, graph.NetAppNodeID("ontap-prod", "ontap-prod-01"), res.nodes[0].ID())
	require.Len(t, res.edges, 1)
	assert.Equal(t, graph.EdgeTypePVCToNetAppAggr, res.edges[0].Type)
	require.NotNil(t, res.edges[0].IO)
	assert.InDelta(t, 150.0, *res.edges[0].IO.ReadOps, 1e-12)
	assert.InDelta(t, 40.0, *res.edges[0].IO.WriteOps, 1e-12)
}

func TestResolveNetAppStorage_JoinMiss(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-nope"}}
	recs := captureDebugRecords(t, func() {
		res := resolveNetApp(claims,
			sampleVec(volSample("pvc-other", "ontap-prod", "n1", "aggr1", "svm", 1)),
			nil, nil, nil, nil, nil, nil, nil)
		assert.Empty(t, res.edges)
		assert.Empty(t, res.svmByPVC)
	})
	assert.True(t, hasMsg(recs, "netapp_volume_join_miss"))
}

func TestResolveNetAppStorage_FlexGroupEmptyAggr(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-fg"}}
	recs := captureDebugRecords(t, func() {
		res := resolveNetApp(claims,
			sampleVec(volSample("pvc-fg", "ontap-prod", "n1", "", "svm-fg", 1)),
			nil, nil, nil, nil, nil, nil, nil)
		assert.Empty(t, res.edges, "empty aggr emits no edge")
		assert.Equal(t, "svm-fg", res.svmByPVC["c/db/data"], "svm still resolves")
	})
	assert.True(t, hasMsg(recs, "netapp_volume_join_miss"), "empty-aggr counts as a miss")
}

func TestResolveNetAppStorage_HarvestAbsentSilent(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-9f3a"}}
	recs := captureDebugRecords(t, func() {
		res := resolveNetApp(claims, nil, nil, nil, nil, nil, nil, nil, nil)
		assert.Empty(t, res.edges)
	})
	assert.False(t, hasMsg(recs, "netapp_volume_join_miss"))
}

func TestResolveNetAppStorage_FullCoverageSilent(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-9f3a"}}
	recs := captureDebugRecords(t, func() {
		resolveNetApp(claims,
			sampleVec(volSample("pvc-9f3a", "oc", "n1", "a1", "svm", 1)),
			nil, nil, nil, nil, nil, nil, nil)
	})
	assert.False(t, hasMsg(recs, "netapp_volume_join_miss"))
}

func TestResolveNetAppStorage_DuplicateAggrPick(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-x"}}
	res := resolveNetApp(claims,
		sampleVec(
			volSample("pvc-x", "oc", "n1", "aggr-b", "svm-b", 1),
			volSample("pvc-x", "oc", "n1", "aggr-a", "svm-a", 1),
		),
		nil, nil, nil, nil, nil, nil, nil)
	require.Len(t, res.edges, 1)
	assert.Equal(t, graph.NetAppAggrID("oc", "aggr-a"), res.edges[0].Target)
	assert.Equal(t, "svm-a", res.svmByPVC["c/db/data"])
}

func TestResolveNetAppStorage_TakeoverPicksLexicalOwner(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-x"}}
	res := resolveNetApp(claims,
		sampleVec(
			volSample("pvc-x", "oc", "node-b", "aggr1", "svm", 1),
			volSample("pvc-x", "oc", "node-a", "aggr1", "svm", 1),
		),
		nil, nil, nil, nil, nil, nil, nil)
	require.Len(t, res.aggrs, 1)
	assert.Equal(t, "node-a", res.aggrs[0].Labels()["node"])
	assert.Equal(t, graph.NetAppAggrID("oc", "aggr1"), res.aggrs[0].ID(), "id excludes owner")
}

func TestResolveNetAppStorage_HealthMapping(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-x"}}
	res := resolveNetApp(claims,
		sampleVec(volSample("pvc-x", "oc", "n1", "a1", "svm", 1)),
		nil, nil, nil,
		sampleVec(aggrSample("oc", "n1", "a1", 1)),
		nil, nil,
		sampleVec(model.Sample{
			Metric: model.Metric{"cluster": "oc", "node": "n1"},
			Value:  0,
		}),
	)
	require.Len(t, res.aggrs, 1)
	assert.Equal(t, graph.HealthOnline, res.aggrs[0].Health())
	require.Len(t, res.nodes, 1)
	assert.Equal(t, graph.HealthDegraded, res.nodes[0].Health())
}

func TestResolveNetAppStorage_AbsentHealthNotDegraded(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-x"}}
	res := resolveNetApp(claims,
		sampleVec(volSample("pvc-x", "oc", "n1", "a1", "svm", 1)),
		nil, nil, nil, nil, nil, nil, nil)
	assert.Empty(t, res.aggrs[0].Health())
	assert.Empty(t, res.nodes[0].Health())
}

func TestResolveNetAppStorage_IOSumAscending(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-x"}}
	res := resolveNetApp(claims,
		sampleVec(
			volSample("pvc-x", "oc", "n1", "a1", "svm", 3),
			volSample("pvc-x", "oc", "n1", "a1", "svm", 2),
		),
		nil, nil, nil, nil, nil, nil, nil)
	require.NotNil(t, res.edges[0].IO)
	assert.InDelta(t, 5.0, *res.edges[0].IO.ReadOps, 1e-12)
}

func TestResolveNetAppStorage_UnreferencedAggrNotMaterialised(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-x"}}
	res := resolveNetApp(claims,
		sampleVec(
			volSample("pvc-x", "oc", "n1", "a1", "svm", 1),
			volSample("other", "oc", "n1", "idle", "svm", 1),
		),
		nil, nil, nil, nil, nil, nil, nil)
	require.Len(t, res.aggrs, 1)
	assert.Equal(t, "a1", res.aggrs[0].Name())
}

func TestResolvePVCUsage_PerFieldAndSmallest(t *testing.T) {
	used := sampleVec(
		model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data"}, Value: 100},
		model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data"}, Value: 90},
	)
	cap := sampleVec(
		model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data"}, Value: 200},
	)
	out := resolvePVCUsage(used, cap, missingClusterCounts{})
	u := out[pvcKey{"c", "db", "data"}]
	require.NotNil(t, u)
	assert.InDelta(t, 90.0, *u.UsedBytes, 1e-12)
	assert.InDelta(t, 200.0, *u.CapacityBytes, 1e-12)
}

func TestParseTopology_NetAppJoinAndUsage(t *testing.T) {
	tp := parseTopology(topologyVectors{
		PVC: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "pod": "mongo", "claim_name": "data", "volume": "data",
		}}),
		Pod: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "pod": "mongo", "uid": "u1", "node": "w0",
		}}),
		PVCInfo: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data",
			"storageclass": "netapp-nas", "volumename": "pvc-9f3a",
		}}),
		VolumeReadOps: sampleVec(volSample("pvc-9f3a", "oc", "n1", "a1", "svm-prod", 10)),
		KubeletVolumeUsed: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data",
		}, Value: 50}),
	})
	require.Len(t, tp.PVCs, 1)
	assert.Equal(t, "pvc-9f3a", tp.PVCs[0].Labels()["volumename"])
	assert.Equal(t, "svm-prod", tp.PVCs[0].Labels()["svm"])
	assert.Equal(t, "netapp-nas", tp.PVCs[0].StorageClass())
	require.NotNil(t, tp.PVCs[0].Usage())
	assert.InDelta(t, 50.0, *tp.PVCs[0].Usage().UsedBytes, 1e-12)
	require.Len(t, tp.NetAppAggrs, 1)
	require.Len(t, tp.StorageEdges, 1)
}

func TestResolveNetAppStorage_DataFamilyPresenceAbsence(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-x"}}
	res := resolveNetAppStorage(claims,
		sampleVec(volSample("pvc-x", "oc", "n1", "a1", "svm", 150)),
		nil, nil, nil,
		sampleVec(volSample("pvc-x", "oc", "n1", "a1", "svm", 5242880)),
		nil, nil, nil, nil, nil)
	require.NotNil(t, res.edges[0].IO)
	assert.InDelta(t, 150.0, *res.edges[0].IO.ReadOps, 1e-12)
	assert.Nil(t, res.edges[0].IO.WriteOps)
	assert.Nil(t, res.edges[0].IO.ReadLatencyUs)
	assert.Nil(t, res.edges[0].IO.WriteLatencyUs)
	assert.InDelta(t, 5242880.0, *res.edges[0].IO.ReadBytesPerSec, 1e-12)
	assert.Nil(t, res.edges[0].IO.WriteBytesPerSec)
}

func TestResolveNetAppStorage_DataSumAscending(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-x"}}
	res := resolveNetAppStorage(claims,
		nil, nil, nil, nil,
		sampleVec(
			volSample("pvc-x", "oc", "n1", "a1", "svm", 3),
			volSample("pvc-x", "oc", "n1", "a1", "svm", 2),
		),
		nil, nil, nil, nil, nil)
	require.NotNil(t, res.edges[0].IO)
	assert.InDelta(t, 5.0, *res.edges[0].IO.ReadBytesPerSec, 1e-12)
	assert.Nil(t, res.edges[0].IO.ReadOps)
}

func TestResolveNetAppStorage_DataFamiliesOnly(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-x"}}
	res := resolveNetAppStorage(claims,
		nil, nil, nil, nil,
		sampleVec(volSample("pvc-x", "oc", "n1", "a1", "svm-data", 5242880)),
		sampleVec(volSample("pvc-x", "oc", "n1", "a1", "svm-data", 1048576)),
		nil, nil, nil, nil)
	assert.Equal(t, "svm-data", res.svmByPVC["c/db/data"])
	require.Len(t, res.aggrs, 1)
	assert.Equal(t, "n1", res.aggrs[0].Labels()["node"])
	require.Len(t, res.edges, 1)
	require.NotNil(t, res.edges[0].IO)
	assert.Nil(t, res.edges[0].IO.ReadOps)
	assert.InDelta(t, 5242880.0, *res.edges[0].IO.ReadBytesPerSec, 1e-12)
	assert.InDelta(t, 1048576.0, *res.edges[0].IO.WriteBytesPerSec, 1e-12)
}

func TestResolveNetAppStorage_DataFamilyCountsAsHarvestPresent(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-nope"}}
	recs := captureDebugRecords(t, func() {
		res := resolveNetAppStorage(claims,
			nil, nil, nil, nil,
			sampleVec(volSample("pvc-other", "oc", "n1", "a1", "svm", 1)),
			nil, nil, nil, nil, nil)
		assert.Empty(t, res.edges)
	})
	assert.True(t, hasMsg(recs, "netapp_volume_join_miss"))
}

func TestReadTopology_HarvestLegFailureDoesNotFailBuild(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, string(promql.QVolumeReadOps), mock.Anything, mock.Anything).
		Return(nil, errors.New("no such metric")).
		Maybe()
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Vector{}, nil).
		Maybe()

	tp, err := ReadTopology(context.Background(), q, time.Minute, time.Unix(1, 0).UTC())
	require.NoError(t, err, "a failing Harvest leg must not fail the build")
	assert.Empty(t, tp.NetAppAggrs)
}
