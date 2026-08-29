package build

import (
	"context"
	"errors"
	"sync"
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

// netappFixture names the three hops of the storage join so a test states only
// the vectors it cares about. Positional arguments would make a 14-vector
// resolver unreadable and silently mis-aligned on the next family added.
type netappFixture struct {
	claims []pvcVolume
	// hop A — topology.
	vol model.Vector
	// hop B — QoS workload I/O.
	readOps, writeOps, readLat, writeLat, readData, writeData model.Vector
	// hop C — QoS fixed-policy ceilings.
	maxIOPS, maxMBps model.Vector
	// aggregate / controller gauges.
	aggrStatus, aggrUsed, aggrTotal, nodeStatus model.Vector
}

func (f netappFixture) run() netappResult {
	return resolveNetAppStorage(f.claims, f.vol,
		f.readOps, f.writeOps, f.readLat, f.writeLat, f.readData, f.writeData,
		f.maxIOPS, f.maxMBps,
		f.aggrStatus, f.aggrUsed, f.aggrTotal, f.nodeStatus)
}

func claim1() []pvcVolume { return []pvcVolume{{id: "c/db/data", volumeName: "pvc-x"}} }

// volLabelSample is a hop-A volume_labels series. Its value is deliberately a
// constant: the resolver must never read it.
func volLabelSample(vn, oc, node, aggr, svm string) model.Sample {
	return model.Sample{
		Metric: model.Metric{
			"volume_name": model.LabelValue(vn),
			"cluster":     model.LabelValue(oc),
			"node":        model.LabelValue(node),
			"aggr":        model.LabelValue(aggr),
			"svm":         model.LabelValue(svm),
		},
		Value: 1,
	}
}

// qosSample is a hop-B QoS workload series. It carries no aggr/node dimension —
// that is exactly why hop A exists. LUN-level workloads never reach the
// resolver: they are excluded at the query layer by the `lun=""` matcher,
// pinned by promql.TestRender_QoSVolumeGranularity.
func qosSample(vn, oc, svm, policy string, val float64) model.Sample {
	return model.Sample{
		Metric: model.Metric{
			"volume_name":  model.LabelValue(vn),
			"cluster":      model.LabelValue(oc),
			"svm":          model.LabelValue(svm),
			"policy_group": model.LabelValue(policy),
		},
		Value: model.SampleValue(val),
	}
}

// policySample is a hop-C qos_policy_fixed_max_throughput_* series.
func policySample(oc, svm, policy string, val float64) model.Sample {
	return model.Sample{
		Metric: model.Metric{
			"cluster": model.LabelValue(oc),
			"svm":     model.LabelValue(svm),
			"name":    model.LabelValue(policy),
		},
		Value: model.SampleValue(val),
	}
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
	res := netappFixture{
		claims:   []pvcVolume{{id: "c/db/data", volumeName: "pvc-9f3a"}},
		vol:      sampleVec(volLabelSample("pvc-9f3a", "ontap-prod", "ontap-prod-01", "aggr1", "svm-prod")),
		readOps:  sampleVec(qosSample("pvc-9f3a", "ontap-prod", "svm-prod", "", 150)),
		writeOps: sampleVec(qosSample("pvc-9f3a", "ontap-prod", "svm-prod", "", 40)),
	}.run()

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
	recs := captureDebugRecords(t, func() {
		res := netappFixture{
			claims: []pvcVolume{{id: "c/db/data", volumeName: "pvc-nope"}},
			vol:    sampleVec(volLabelSample("pvc-other", "oc", "n1", "aggr1", "svm")),
		}.run()
		assert.Empty(t, res.edges)
		assert.Empty(t, res.svmByPVC)
	})
	assert.True(t, hasMsg(recs, "netapp_volume_join_miss"))
}

func TestResolveNetAppStorage_FlexGroupEmptyAggr(t *testing.T) {
	recs := captureDebugRecords(t, func() {
		res := netappFixture{
			claims: []pvcVolume{{id: "c/db/data", volumeName: "pvc-fg"}},
			vol:    sampleVec(volLabelSample("pvc-fg", "oc", "n1", "", "svm-fg")),
		}.run()
		assert.Empty(t, res.edges, "empty aggr emits no edge")
		assert.Equal(t, "svm-fg", res.svmByPVC["c/db/data"], "svm still resolves")
	})
	assert.True(t, hasMsg(recs, "netapp_volume_join_miss"), "empty-aggr counts as a miss")
}

func TestResolveNetAppStorage_HarvestAbsentSilent(t *testing.T) {
	recs := captureDebugRecords(t, func() {
		res := netappFixture{claims: claim1()}.run()
		assert.Empty(t, res.edges)
	})
	assert.False(t, hasMsg(recs, "netapp_volume_join_miss"))
	assert.False(t, hasMsg(recs, "netapp_qos_join_miss"))
}

func TestResolveNetAppStorage_FullCoverageSilent(t *testing.T) {
	recs := captureDebugRecords(t, func() {
		netappFixture{
			claims:  claim1(),
			vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
			readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "", 1)),
		}.run()
	})
	assert.False(t, hasMsg(recs, "netapp_volume_join_miss"))
	assert.False(t, hasMsg(recs, "netapp_qos_join_miss"))
}

func TestResolveNetAppStorage_DuplicateAggrPick(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol: sampleVec(
			volLabelSample("pvc-x", "oc", "n1", "aggr-b", "svm-b"),
			volLabelSample("pvc-x", "oc", "n1", "aggr-a", "svm-a"),
		),
	}.run()
	require.Len(t, res.edges, 1)
	assert.Equal(t, graph.NetAppAggrID("oc", "aggr-a"), res.edges[0].Target)
	assert.Equal(t, "svm-a", res.svmByPVC["c/db/data"])
}

func TestResolveNetAppStorage_TakeoverPicksLexicalOwner(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol: sampleVec(
			volLabelSample("pvc-x", "oc", "node-b", "aggr1", "svm"),
			volLabelSample("pvc-x", "oc", "node-a", "aggr1", "svm"),
		),
	}.run()
	require.Len(t, res.aggrs, 1)
	assert.Equal(t, "node-a", res.aggrs[0].Labels()["node"])
	assert.Equal(t, graph.NetAppAggrID("oc", "aggr1"), res.aggrs[0].ID(), "id excludes owner")
}

func TestResolveNetAppStorage_HealthMapping(t *testing.T) {
	res := netappFixture{
		claims:     claim1(),
		vol:        sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		aggrStatus: sampleVec(aggrSample("oc", "n1", "a1", 1)),
		nodeStatus: sampleVec(model.Sample{
			Metric: model.Metric{"cluster": "oc", "node": "n1"},
			Value:  0,
		}),
	}.run()
	require.Len(t, res.aggrs, 1)
	assert.Equal(t, graph.HealthOnline, res.aggrs[0].Health())
	require.Len(t, res.nodes, 1)
	assert.Equal(t, graph.HealthDegraded, res.nodes[0].Health())
}

func TestResolveNetAppStorage_AbsentHealthNotDegraded(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol:    sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
	}.run()
	assert.Empty(t, res.aggrs[0].Health())
	assert.Empty(t, res.nodes[0].Health())
}

func TestResolveNetAppStorage_IOSumAscending(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol:    sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(
			qosSample("pvc-x", "oc", "svm", "", 3),
			qosSample("pvc-x", "oc", "svm", "", 2),
		),
	}.run()
	require.NotNil(t, res.edges[0].IO)
	assert.InDelta(t, 5.0, *res.edges[0].IO.ReadOps, 1e-12)
}

func TestResolveNetAppStorage_UnreferencedAggrNotMaterialised(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol: sampleVec(
			volLabelSample("pvc-x", "oc", "n1", "a1", "svm"),
			volLabelSample("other", "oc", "n1", "idle", "svm"),
		),
	}.run()
	require.Len(t, res.aggrs, 1)
	assert.Equal(t, "a1", res.aggrs[0].Name())
}

func TestResolveNetAppStorage_PerFamilyPresenceAbsence(t *testing.T) {
	res := netappFixture{
		claims:   claim1(),
		vol:      sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps:  sampleVec(qosSample("pvc-x", "oc", "svm", "", 150)),
		readData: sampleVec(qosSample("pvc-x", "oc", "svm", "", 5242880)),
	}.run()
	io := res.edges[0].IO
	require.NotNil(t, io)
	assert.InDelta(t, 150.0, *io.ReadOps, 1e-12)
	assert.InDelta(t, 5242880.0, *io.ReadBytesPerSec, 1e-12)
	assert.Nil(t, io.WriteOps)
	assert.Nil(t, io.ReadLatencyUs)
	assert.Nil(t, io.WriteLatencyUs)
	assert.Nil(t, io.WriteBytesPerSec)
}

// A hop-B miss must cost measurements ONLY. The claim keeps its edge, its
// aggregate, its controller and its svm — the property the whole hop split
// exists for (design.md D3).
func TestResolveNetAppStorage_TopologyWithoutQoS(t *testing.T) {
	recs := captureDebugRecords(t, func() {
		res := netappFixture{
			claims:  claim1(),
			vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm-prod")),
			readOps: sampleVec(qosSample("pvc-other", "oc", "svm-prod", "", 99)),
		}.run()
		require.Len(t, res.edges, 1)
		assert.Nil(t, res.edges[0].IO, "no QoS match ⇒ no metrics object")
		require.Len(t, res.aggrs, 1)
		require.Len(t, res.nodes, 1)
		assert.Equal(t, "svm-prod", res.svmByPVC["c/db/data"])
	})
	assert.True(t, hasMsg(recs, "netapp_qos_join_miss"))
	assert.False(t, hasMsg(recs, "netapp_volume_join_miss"))
}

// The two signals gate on their OWN family: a deployment running Harvest's
// volume template without the QoS template must not be warned about I/O.
func TestResolveNetAppStorage_CoverageSignalsGateIndependently(t *testing.T) {
	recs := captureDebugRecords(t, func() {
		res := netappFixture{
			claims: claim1(),
			vol:    sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		}.run()
		require.Len(t, res.edges, 1)
		assert.Nil(t, res.edges[0].IO)
	})
	assert.False(t, hasMsg(recs, "netapp_qos_join_miss"), "no QoS vector read ⇒ no I/O warning")
	assert.False(t, hasMsg(recs, "netapp_volume_join_miss"))
}

func TestResolveNetAppStorage_CeilingResolvedAndConverted(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "ontap-prod", "n1", "a1", "svm-prod")),
		readOps: sampleVec(qosSample("pvc-x", "ontap-prod", "svm-prod", "gold-tier", 1200)),
		maxIOPS: sampleVec(policySample("ontap-prod", "svm-prod", "gold-tier", 5000)),
		maxMBps: sampleVec(policySample("ontap-prod", "svm-prod", "gold-tier", 250)),
	}.run()
	io := res.edges[0].IO
	require.NotNil(t, io)
	require.NotNil(t, io.MaxIOPS)
	assert.InDelta(t, 5000.0, *io.MaxIOPS, 1e-12)
	require.NotNil(t, io.MaxBytesPerSec)
	assert.InDelta(t, 250.0*1048576, *io.MaxBytesPerSec, 1e-12, "mbps is scaled to bytes/s")
}

func TestResolveNetAppStorage_CeilingPerField(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 10)),
		maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
	}.run()
	io := res.edges[0].IO
	require.NotNil(t, io.MaxIOPS)
	assert.Nil(t, io.MaxBytesPerSec)
}

func TestResolveNetAppStorage_NoPolicyGroupNoCeiling(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "", 10)),
		maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
	}.run()
	io := res.edges[0].IO
	require.NotNil(t, io)
	assert.InDelta(t, 10.0, *io.ReadOps, 1e-12)
	assert.Nil(t, io.MaxIOPS, "a volume in no policy group has no declared ceiling")
	assert.Nil(t, io.MaxBytesPerSec)
}

// The ceiling rides on the workload series' policy_group, so an edge with no
// measurement can never acquire one. Structural, not defensive.
func TestResolveNetAppStorage_NoCeilingWithoutMeasurement(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
		maxMBps: sampleVec(policySample("oc", "svm", "gold", 250)),
	}.run()
	require.Len(t, res.edges, 1)
	assert.Nil(t, res.edges[0].IO, "no workload ⇒ no metrics at all, ceiling included")
}

func TestResolveNetAppStorage_PolicyTriplePickIsLexical(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol:    sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(
			qosSample("pvc-x", "oc", "svm", "silver", 10),
			qosSample("pvc-x", "oc", "svm", "gold", 10),
		),
		maxIOPS: sampleVec(
			policySample("oc", "svm", "gold", 5000),
			policySample("oc", "svm", "silver", 1000),
		),
	}.run()
	require.NotNil(t, res.edges[0].IO.MaxIOPS)
	assert.InDelta(t, 5000.0, *res.edges[0].IO.MaxIOPS, 1e-12, "lexically-smallest policy wins")
}

// Harvest names the fixed policy's identity label differently across templates.
// The join key's SHAPE is the contract, not the label's spelling.
func TestResolveNetAppStorage_PolicyLabelSpellingFallback(t *testing.T) {
	pg := model.Sample{
		Metric: model.Metric{"cluster": "oc", "svm": "svm", "policy_group": "gold"},
		Value:  5000,
	}
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 10)),
		maxIOPS: sampleVec(pg),
	}.run()
	require.NotNil(t, res.edges[0].IO.MaxIOPS)
	assert.InDelta(t, 5000.0, *res.edges[0].IO.MaxIOPS, 1e-12)
}

func TestResolveNetAppStorage_CeilingSmallestOnDuplicate(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 10)),
		maxIOPS: sampleVec(
			policySample("oc", "svm", "gold", 5000),
			policySample("oc", "svm", "gold", 4000),
		),
	}.run()
	assert.InDelta(t, 4000.0, *res.edges[0].IO.MaxIOPS, 1e-12)
}

// The PVC svm label has ONE source: hop A. A QoS workload on another SVM is a
// different volume, so it neither overrides the label nor contributes I/O.
func TestResolveNetAppStorage_SVMComesFromTopologyOnly(t *testing.T) {
	recs := captureDebugRecords(t, func() {
		res := netappFixture{
			claims:  claim1(),
			vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm-a")),
			readOps: sampleVec(qosSample("pvc-x", "oc", "svm-b", "", 99)),
		}.run()
		assert.Equal(t, "svm-a", res.svmByPVC["c/db/data"])
		assert.Nil(t, res.edges[0].IO, "another SVM's workload is not this volume")
	})
	assert.True(t, hasMsg(recs, "netapp_qos_join_miss"))
}

// A volume_name colliding across two ONTAP clusters sharing one
// VictoriaMetrics must not have the other filer's throughput summed onto this
// edge.
func TestResolveNetAppStorage_QoSScopedToPickedCluster(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol: sampleVec(
			volLabelSample("pvc-x", "oc-a", "n1", "aggr1", "svm"),
			volLabelSample("pvc-x", "oc-b", "n9", "aggr9", "svm"),
		),
		readOps: sampleVec(
			qosSample("pvc-x", "oc-a", "svm", "", 10),
			qosSample("pvc-x", "oc-b", "svm", "", 90),
		),
	}.run()
	require.Len(t, res.edges, 1)
	assert.Equal(t, graph.NetAppAggrID("oc-a", "aggr1"), res.edges[0].Target)
	assert.InDelta(t, 10.0, *res.edges[0].IO.ReadOps, 1e-12,
		"oc-b's workload must not be summed onto the oc-a edge")
}

// volume_labels rows without a `node` label leave the aggregate owner-less: the
// key is ABSENT (never empty-string, mirroring the PVC volumename/svm rule) and
// no controller node is materialised.
func TestResolveNetAppStorage_OwnerlessAggrOmitsNodeLabel(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol:    sampleVec(volLabelSample("pvc-x", "oc", "", "aggr1", "svm")),
	}.run()
	require.Len(t, res.aggrs, 1)
	_, hasNode := res.aggrs[0].Labels()["node"]
	assert.False(t, hasNode, "node key must be absent, not empty-string")
	assert.Empty(t, res.nodes, "no controller node without a resolved owner")
}

func TestResolvePVCUsage_PerFieldAndSmallest(t *testing.T) {
	used := sampleVec(
		model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data"}, Value: 100},
		model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data"}, Value: 90},
	)
	capacity := sampleVec(
		model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data"}, Value: 200},
	)
	out := resolvePVCUsage(used, capacity, newClusterResolver(promql.LabelKeys{}))
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
		VolumeLabels: sampleVec(volLabelSample("pvc-9f3a", "oc", "n1", "a1", "svm-prod")),
		QoSReadOps:   sampleVec(qosSample("pvc-9f3a", "oc", "svm-prod", "gold", 10)),
		QoSPolicyMaxIOPS: sampleVec(
			policySample("oc", "svm-prod", "gold", 5000),
		),
		KubeletVolumeUsed: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data",
		}, Value: 50}),
	}, promql.LabelKeys{})
	require.Len(t, tp.PVCs, 1)
	assert.Equal(t, "pvc-9f3a", tp.PVCs[0].Labels()["volumename"])
	assert.Equal(t, "svm-prod", tp.PVCs[0].Labels()["svm"])
	assert.Equal(t, "netapp-nas", tp.PVCs[0].StorageClass())
	require.NotNil(t, tp.PVCs[0].Usage())
	assert.InDelta(t, 50.0, *tp.PVCs[0].Usage().UsedBytes, 1e-12)
	require.Len(t, tp.NetAppAggrs, 1)
	require.Len(t, tp.StorageEdges, 1)
	require.NotNil(t, tp.StorageEdges[0].IO)
	assert.InDelta(t, 5000.0, *tp.StorageEdges[0].IO.MaxIOPS, 1e-12)
}

func TestReadTopology_HarvestLegFailureDoesNotFailBuild(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, string(promql.QVolumeLabels), mock.Anything, mock.Anything).
		Return(nil, errors.New("no such metric")).
		Maybe()
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Vector{}, nil).
		Maybe()

	tp, err := ReadTopology(context.Background(), q, time.Minute, time.Unix(1, 0).UTC(), promql.LabelKeys{}, promql.Selector{})
	require.NoError(t, err, "a failing Harvest leg must not fail the build")
	assert.Empty(t, tp.NetAppAggrs)
}

func TestReadTopology_QoSLegFailureDoesNotFailBuild(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, string(promql.QQoSReadOps), mock.Anything, mock.Anything).
		Return(nil, errors.New("no such metric")).
		Maybe()
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Vector{}, nil).
		Maybe()

	_, err := ReadTopology(context.Background(), q, time.Minute, time.Unix(1, 0).UTC(), promql.LabelKeys{}, promql.Selector{})
	require.NoError(t, err, "a failing QoS leg must not fail the build")
}

// The fan-out is 37 legs (design.md D1: 18 KSM − 3 removed + 15 storage, plus
// the 7 controller-annotation / kube_job_owner legs that resolve a pod's ArgoCD
// Application from its controller).
// Pinning the count catches a leg silently dropped or double-registered.
func TestReadTopology_FanOutLegCount(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}

	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(_ context.Context, name string, _ string, _ time.Time) {
			mu.Lock()
			seen[name]++
			mu.Unlock()
		}).
		Return(model.Vector{}, nil).
		Maybe()

	_, err := ReadTopology(context.Background(), q, time.Minute, time.Unix(1, 0).UTC(), promql.LabelKeys{}, promql.Selector{})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, seen, 37, "one query per leg, no duplicates")
	total := 0
	for _, n := range seen {
		total += n
	}
	assert.Equal(t, 37, total, "each leg issued exactly once")
}

// legFixture is one topology leg's canned answer.
type legFixture struct {
	vec model.Vector
	err error
}

// legQuerier answers each named leg with its fixture and every OTHER leg with an
// empty vector.
//
// Two things are load-bearing. (1) The per-leg expectations are registered
// BEFORE the catch-all: testify returns the first registered expectation whose
// arguments match, so a catch-all registered first would shadow every specific
// one. (2) They are deliberately NOT .Maybe() — the catch-all answers
// successfully, so a query name that stops matching (a renamed Query constant, a
// reordered Instant signature) would otherwise let the whole test pass
// vacuously. Requiring the call makes that a loud failure instead.
func legQuerier(t *testing.T, legs map[promql.Query]legFixture) *promqlmocks.MockQuerier {
	t.Helper()
	q := promqlmocks.NewMockQuerier(t)
	for name, f := range legs { // keys are distinct legs, so order is irrelevant
		q.EXPECT().
			Instant(mock.Anything, string(name), mock.Anything, mock.Anything).
			Return(f.vec, f.err)
	}
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Vector{}, nil).
		Maybe()
	return q
}

// failingLegs builds the map form for the common "these legs all fail the same
// way" case.
func failingLegs(vec model.Vector, err error, names ...promql.Query) map[promql.Query]legFixture {
	out := make(map[promql.Query]legFixture, len(names))
	for _, name := range names {
		out[name] = legFixture{vec, err}
	}
	return out
}

// readTopologyDefaults runs ReadTopology with the zero LabelKeys / Selector —
// the shape every leg-failure test wants.
func readTopologyDefaults(ctx context.Context, q promql.Querier) (Topology, error) {
	return ReadTopology(ctx, q, time.Minute, time.Unix(1, 0).UTC(), promql.LabelKeys{}, promql.Selector{})
}

func TestReadTopology_AccumulatingAnnotationLegDegrades(t *testing.T) {
	// Samples returned ALONGSIDE the error: fetchOptional must discard them.
	discarded := sampleVec(ctrlAnn("replicaset", "shop", "rs-1", "app:apps/ReplicaSet:shop/rs-1"))
	legs := failingLegs(discarded, errors.New("search.maxUniqueTimeseries exceeded"),
		promql.QReplicaSetAnnotations, promql.QJobAnnotations)

	// A sibling family that SUCCEEDS, and a pod that depends on it. Without them
	// the whole fixture is empty, so a degrade that clobbered more than its own
	// destination (a shared dst, a parseTopology that bails on one nil vector)
	// would be invisible.
	survivor := sampleVec(
		ctrlAnn("deployment", "shop", "web", "storefront:apps/Deployment:shop/web"),
		ctrlAnn("deployment", "shop", "api", "storefront:apps/Deployment:shop/api"),
	)
	legs[promql.QDeploymentAnnotations] = legFixture{survivor, nil}
	legs[promql.QPodInfo] = legFixture{sampleVec(appPod("shop", "web-1", "uid-web-1")), nil}
	legs[promql.QPodOwner] = legFixture{sampleVec(appOwner("shop", "web-1", "Deployment", "web")), nil}
	q := legQuerier(t, legs)

	tp, err := readTopologyDefaults(context.Background(), q)
	require.NoError(t, err, "an accumulating-cardinality annotation leg must not fail the build")
	for _, name := range []promql.Query{promql.QReplicaSetAnnotations, promql.QJobAnnotations} {
		require.Contains(t, tp.RawSeriesCount, string(name),
			"%s must still be counted after degrading (an absent key is not the same as 0)", name)
		assert.Zero(t, tp.RawSeriesCount[string(name)],
			"%s: a query error must yield an empty vector, discarding any samples returned with the error", name)
	}
	assert.Equal(t, len(survivor), tp.RawSeriesCount[string(promql.QDeploymentAnnotations)],
		"a degrading leg must not disturb a sibling family that succeeded")
	require.Len(t, tp.Pods, 1, "the surviving pod family must still parse")
	assert.Equal(t, "storefront", tp.Pods[0].Application(),
		"a pod owned by the surviving family must still resolve its Application end to end")
}

// TestReadTopology_DegradedJobAnnotationsSuppressCronJobHop pins the one
// degrade that could substitute a WRONG value rather than omit one. The
// Job -> CronJob hop's precondition is "the Job carries no annotation of its
// own", which is only knowable when kube_job_annotations was actually READ:
// after a degrade a Job that DOES carry its own tracking-id is indistinguishable
// from one that does not, so following the hop would silently re-attribute the
// pod to the CronJob's Application. Every other optional leg is subtractive;
// this one must be too.
//
// The subtests are a matched pair: the SAME fixture with the leg alive must
// still take the hop, so a broken fixture cannot make the suppression look
// correct.
func TestReadTopology_DegradedJobAnnotationsSuppressCronJobHop(t *testing.T) {
	fixture := func(jobAnn legFixture) map[promql.Query]legFixture {
		return map[promql.Query]legFixture{
			promql.QJobAnnotations: jobAnn,
			promql.QPodInfo:        {sampleVec(appPod("shop", "migrate-1-xyz", "uid-1")), nil},
			promql.QPodOwner:       {sampleVec(appOwner("shop", "migrate-1-xyz", "Job", "migrate-1")), nil},
			promql.QJobOwner: {sampleVec(model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "shop", "job_name": "migrate-1",
				"owner_kind": "CronJob", "owner_name": "nightly", "owner_is_controller": "true",
			}}), nil},
			promql.QCronJobAnnotations: {
				sampleVec(ctrlAnn("cronjob", "shop", "nightly", "reports:batch/CronJob:shop/nightly")), nil},
		}
	}

	t.Run("degraded suppresses the hop", func(t *testing.T) {
		q := legQuerier(t, fixture(legFixture{nil, errors.New("search.maxUniqueTimeseries exceeded")}))

		tp, err := readTopologyDefaults(context.Background(), q)
		require.NoError(t, err, "the leg must still degrade rather than fail the build")
		require.Len(t, tp.Pods, 1)
		assert.Empty(t, tp.Pods[0].Application(),
			"a degraded kube_job_annotations must omit the Application, never attribute the pod to the CronJob")
		require.NotNil(t, tp.Pods[0].Owner())
		assert.Equal(t, graph.Owner{Kind: "Job", Name: "migrate-1"}, *tp.Pods[0].Owner(),
			"suppressing the hop must not disturb the owner attribute")
	})

	t.Run("alive leg still takes the hop", func(t *testing.T) {
		// An empty vector is the genuine "this Job carries no annotation" answer.
		q := legQuerier(t, fixture(legFixture{model.Vector{}, nil}))

		tp, err := readTopologyDefaults(context.Background(), q)
		require.NoError(t, err)
		require.Len(t, tp.Pods, 1)
		assert.Equal(t, "reports", tp.Pods[0].Application(),
			"a read-but-empty family is a real answer: the hop still resolves the CronJob's Application")
	})
}

// TestReadTopology_RequiredAnnotationLegFailsBuild covers every leg the
// abort-on-error half of the split names — the four controller-annotation
// families that stayed on `fetch` plus kube_job_owner. Only the two
// accumulating-cardinality families are allowed to degrade, so flipping any of
// these five to fetchOptional must fail here (TestReadTopology_FanOutLegCount
// pins the total, not which leg is on which side).
func TestReadTopology_RequiredAnnotationLegFailsBuild(t *testing.T) {
	for _, name := range []promql.Query{
		promql.QDeploymentAnnotations,
		promql.QStatefulSetAnnotations,
		promql.QDaemonSetAnnotations,
		promql.QCronJobAnnotations,
		promql.QJobOwner,
	} {
		t.Run(string(name), func(t *testing.T) {
			q := legQuerier(t, failingLegs(nil, errors.New("upstream 5xx"), name))

			_, err := readTopologyDefaults(context.Background(), q)
			require.Error(t, err, "%s is abort-on-error and must fail the build", name)
		})
	}
}

func TestReadTopology_DegradingLegHonoursCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	q := legQuerier(t, failingLegs(nil, errors.New("context canceled"), promql.QJobAnnotations))

	_, err := readTopologyDefaults(ctx, q)
	require.Error(t, err, "caller cancellation must fail a degrading family rather than swallow it")

	// Positive control: the SAME leg and the SAME error under a live caller ctx
	// must degrade. Without it this test passes byte-identically whether the leg
	// is on fetch or fetchOptional, so it would not actually pin
	// optionalQueryFatal's callerCtx branch — only the pair does.
	live := legQuerier(t, failingLegs(nil, errors.New("upstream 5xx"), promql.QJobAnnotations))
	_, err = readTopologyDefaults(context.Background(), live)
	require.NoError(t, err, "the same leg and error must degrade when the caller is still alive")
}
