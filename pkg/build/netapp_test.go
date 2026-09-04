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
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// netappFixture names the hops of the storage join so a test states only the
// vectors it cares about. The resolver reads nineteen same-typed vectors;
// naming them here (and passing a topologyVectors rather than a positional
// list) is what keeps a mis-aligned argument impossible.
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
	// controller hardware identity (an info series) and the four system_node
	// performance counters — data.hardware and data.perf, never data.health.
	nodeLabels                                 model.Vector
	cpuBusy, totalOps, totalLatency, totalData model.Vector
	// rw overrides the derivation; nil exercises the shipped defaults
	// (`-` → `_`, suffix match), which is what almost every case wants.
	rw *VolumeKeyRewriter
}

func (f netappFixture) rewriter() *VolumeKeyRewriter {
	if f.rw != nil {
		return f.rw
	}
	return defaultVolumeKeyRewriter()
}

func (f netappFixture) vectors() topologyVectors {
	return topologyVectors{
		VolumeKey:              f.rewriter(),
		VolumeLabels:           f.vol,
		QoSReadOps:             f.readOps,
		QoSWriteOps:            f.writeOps,
		QoSReadLatency:         f.readLat,
		QoSWriteLatency:        f.writeLat,
		QoSReadData:            f.readData,
		QoSWriteData:           f.writeData,
		QoSPolicyMaxIOPS:       f.maxIOPS,
		QoSPolicyMaxMBps:       f.maxMBps,
		AggrStatus:             f.aggrStatus,
		AggrSpaceUsed:          f.aggrUsed,
		AggrSpaceTotal:         f.aggrTotal,
		NetAppNodeStatus:       f.nodeStatus,
		NetAppNodeLabels:       f.nodeLabels,
		NetAppNodeCPUBusy:      f.cpuBusy,
		NetAppNodeTotalOps:     f.totalOps,
		NetAppNodeTotalLatency: f.totalLatency,
		NetAppNodeTotalData:    f.totalData,
	}
}

func (f netappFixture) run() netappResult {
	return resolveNetAppStorage(f.claims, f.vectors())
}

func claim1() []pvcVolume { return []pvcVolume{{id: "c/db/data", volumeName: "pvc-x"}} }

// tridentVol renders the ONTAP FlexVol name a CSI provisioner derives from a
// PersistentVolume name: the prefix it is configured with, plus the PV name with
// every `-` replaced by `_` (ONTAP volume names admit no dashes). Fixtures state
// PV names and the series carry stock Harvest `volume` values, so every case
// below exercises the shipped derivation end to end rather than a pre-joined
// key.
func tridentVol(pvName string) string {
	return "trident_" + strings.ReplaceAll(pvName, "-", "_")
}

// volLabelSample is a hop-A volume_labels series. Its value is deliberately a
// constant: the resolver must never read it.
func volLabelSample(pvName, oc, node, aggr, svm string) model.Sample {
	return model.Sample{
		Metric: model.Metric{
			"volume":  model.LabelValue(tridentVol(pvName)),
			"cluster": model.LabelValue(oc),
			"node":    model.LabelValue(node),
			"aggr":    model.LabelValue(aggr),
			"svm":     model.LabelValue(svm),
		},
		Value: 1,
	}
}

// qosSample is a hop-B QoS workload series at VOLUME granularity (no `lun`
// label). It carries no aggr/node dimension — that is exactly why hop A exists.
// LUN-level workloads DO reach the resolver since D11 (see lunQosSample); it is
// sumQoSIO that discards them, so their policy group stays readable.
func qosSample(pvName, oc, svm, policy string, val float64) model.Sample {
	return model.Sample{
		Metric: model.Metric{
			"volume":       model.LabelValue(tridentVol(pvName)),
			"cluster":      model.LabelValue(oc),
			"svm":          model.LabelValue(svm),
			"policy_group": model.LabelValue(policy),
		},
		Value: model.SampleValue(val),
	}
}

// lunQosSample is a LUN-level hop-B workload series. ONTAP collects one per
// LUN as well as per volume and it carries its FlexVol's `volume`, so it must
// never be summed onto the claim — but on a SAN backend it is the ONLY series
// naming the real QoS policy group.
func lunQosSample(pvName, oc, svm, policy, lun string, val float64) model.Sample {
	sm := qosSample(pvName, oc, svm, policy, val)
	sm.Metric["lun"] = model.LabelValue(lun)
	return sm
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

// Spec: "Volume in no policy group carries no ceiling" — an incomplete key is
// IGNORED, never widened. The SVM holds a gold policy, but this volume is in
// none, so borrowing gold's figure would name a limit it does not have.
func TestResolveNetAppStorage_EmptyPolicyGroupIgnoresSVMCeiling(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "", 10)),
		maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
	}.run()
	io := res.edges[0].IO
	require.NotNil(t, io)
	assert.InDelta(t, 10.0, *io.ReadOps, 1e-12)
	assert.Nil(t, io.MaxIOPS, "no policy group ⇒ no ceiling, not the SVM's")
	assert.Nil(t, io.MaxBytesPerSec)
}

// Spec: "Volume in no policy group carries no ceiling" — empty policy_group
// on the workload AND no fixed-policy series for the pair.
func TestResolveNetAppStorage_NoPolicySeriesNoCeiling(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "", 10)),
	}.run()
	io := res.edges[0].IO
	require.NotNil(t, io)
	assert.InDelta(t, 10.0, *io.ReadOps, 1e-12)
	assert.Nil(t, io.MaxIOPS)
	assert.Nil(t, io.MaxBytesPerSec)
}

// The ceiling is attached only inside the matched-workload branch, so an
// edge with no measurement can never acquire one. Structural, not defensive.
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

// Spec: "Another policy group in the same SVM is never borrowed". A gold
// volume in an SVM that also holds bronze must report gold's own figures —
// not bronze's, and not a per-figure minimum across the two, either of which
// would read as a healthy volume many times over its limit.
func TestResolveNetAppStorage_CeilingIsTheVolumesOwnPolicy(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 1200)),
		maxIOPS: sampleVec(
			policySample("oc", "svm", "gold", 5000),
			policySample("oc", "svm", "bronze", 100),
		),
		maxMBps: sampleVec(
			policySample("oc", "svm", "gold", 250),
			policySample("oc", "svm", "bronze", 400),
		),
	}.run()
	io := res.edges[0].IO
	require.NotNil(t, io)
	require.NotNil(t, io.MaxIOPS)
	assert.InDelta(t, 5000.0, *io.MaxIOPS, 1e-12, "gold's ceiling, not bronze's")
	require.NotNil(t, io.MaxBytesPerSec)
	assert.InDelta(t, 250.0*1048576, *io.MaxBytesPerSec, 1e-12,
		"each figure comes from the same policy — never a cross-policy minimum")
}

// A volume observed under two policy groups inside the window resolves
// deterministically to the lexically-smallest, independent of vector order.
func TestResolveNetAppStorage_CeilingPolicyPickDeterministic(t *testing.T) {
	for _, order := range [][]model.Sample{
		{qosSample("pvc-x", "oc", "svm", "gold", 10), qosSample("pvc-x", "oc", "svm", "bronze", 5)},
		{qosSample("pvc-x", "oc", "svm", "bronze", 5), qosSample("pvc-x", "oc", "svm", "gold", 10)},
	} {
		res := netappFixture{
			claims:  claim1(),
			vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
			readOps: sampleVec(order...),
			maxIOPS: sampleVec(
				policySample("oc", "svm", "gold", 5000),
				policySample("oc", "svm", "bronze", 100),
			),
		}.run()
		require.NotNil(t, res.edges[0].IO.MaxIOPS)
		assert.InDelta(t, 100.0, *res.edges[0].IO.MaxIOPS, 1e-12,
			"lexically-smallest policy group wins regardless of series order")
	}
}

// The ceiling key takes its svm from hop A, so a workload series carrying a
// policy_group but NO svm label of its own still reaches hop C — it measures
// the edge under qosInScope and contributes its policy.
func TestResolveNetAppStorage_CeilingFromSVMLessWorkload(t *testing.T) {
	noSVM := model.Sample{
		Metric: model.Metric{
			"volume":       model.LabelValue(tridentVol("pvc-x")),
			"cluster":      "oc",
			"policy_group": "gold",
		},
		Value: 10,
	}
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm-prod")),
		readOps: sampleVec(noSVM),
		maxIOPS: sampleVec(policySample("oc", "svm-prod", "gold", 5000)),
	}.run()
	io := res.edges[0].IO
	require.NotNil(t, io)
	assert.InDelta(t, 10.0, *io.ReadOps, 1e-12)
	require.NotNil(t, io.MaxIOPS, "hop A supplies the svm the workload lacks")
	assert.InDelta(t, 5000.0, *io.MaxIOPS, 1e-12)
}

// Spec: "Claim without an SVM carries no ceiling". Hop A's empty svm is the
// gate — hop B carrying a policy_group, and hop C holding series for that
// cluster, must not leak a ceiling onto an SVM-less claim.
func TestResolveNetAppStorage_EmptySVMNoCeiling(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "")),
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm-prod", "gold", 10)),
		maxIOPS: sampleVec(policySample("oc", "svm-prod", "gold", 5000)),
		maxMBps: sampleVec(policySample("oc", "svm-prod", "gold", 250)),
	}.run()
	require.Len(t, res.edges, 1)
	io := res.edges[0].IO
	require.NotNil(t, io)
	assert.InDelta(t, 10.0, *io.ReadOps, 1e-12)
	assert.Nil(t, io.MaxIOPS)
	assert.Nil(t, io.MaxBytesPerSec)
}

// Harvest spells the policy's identity label differently across templates, so
// the reader takes `name` with a `policy_group` fallback. A series carrying
// neither cannot be keyed and is dropped.
func TestResolveNetAppStorage_CeilingPolicyIdentitySpellings(t *testing.T) {
	policyNamed := func(label, policy string) model.Sample {
		return model.Sample{
			Metric: model.Metric{
				"cluster":              "oc",
				"svm":                  "svm",
				model.LabelName(label): model.LabelValue(policy),
			},
			Value: 5000,
		}
	}
	fixture := func(ceiling model.Sample) *graph.IOMetrics {
		return netappFixture{
			claims:  claim1(),
			vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
			readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 10)),
			maxIOPS: sampleVec(ceiling),
		}.run().edges[0].IO
	}
	for _, label := range []string{"name", "policy_group"} {
		io := fixture(policyNamed(label, "gold"))
		require.NotNil(t, io.MaxIOPS, "identity label %q must resolve", label)
		assert.InDelta(t, 5000.0, *io.MaxIOPS, 1e-12)
	}
	bare := model.Sample{Metric: model.Metric{"cluster": "oc", "svm": "svm"}, Value: 5000}
	assert.Nil(t, fixture(bare).MaxIOPS, "a policy with no identity label cannot be keyed")
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

// A FlexVol name colliding across two ONTAP clusters sharing one
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

// A cross-filer token collision must not let the SVM pick and the aggregate
// pick land on different filers: the resolved svm is paired with the picked
// aggregate's ONTAP cluster by both qosInScope and the hop-C ceiling key, so an
// unscoped lexically-smallest pick would take oc-b's "alpha" against oc-a's
// aggregate — dropping every in-scope workload and attaching oc-a's unrelated
// "alpha" tenant ceiling.
func TestResolveNetAppStorage_SVMScopedToPickedCluster(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol: sampleVec(
			volLabelSample("pvc-x", "oc-a", "n1", "aggr1", "zulu"),
			volLabelSample("pvc-x", "oc-b", "n9", "aggr9", "alpha"),
		),
		readOps: sampleVec(qosSample("pvc-x", "oc-a", "zulu", "gold", 10)),
		maxIOPS: sampleVec(
			policySample("oc-a", "zulu", "gold", 5000),
			policySample("oc-a", "alpha", "gold", 100),
		),
	}.run()
	assert.Equal(t, "zulu", res.svmByPVC["c/db/data"],
		"svm must come from the filer the aggregate pick landed on")
	require.Len(t, res.edges, 1)
	assert.Equal(t, graph.NetAppAggrID("oc-a", "aggr1"), res.edges[0].Target)
	require.NotNil(t, res.edges[0].IO, "the oc-a workload must stay in scope")
	assert.InDelta(t, 10.0, *res.edges[0].IO.ReadOps, 1e-12)
	require.NotNil(t, res.edges[0].IO.MaxIOPS)
	assert.InDelta(t, 5000.0, *res.edges[0].IO.MaxIOPS, 1e-12,
		"the ceiling must key on (oc-a, zulu, gold), not on the other filer's svm")
}

// The ontap-san shape, one LUN per FlexVol: the QoS policy is attached to the
// LUN, so the FlexVol's own workload falls into ONTAP's built-in
// "User-Best_effort" class, which declares no ceiling and has no fixed-policy
// series. The ceiling must come from the LUN row's policy — while the LUN
// row's I/O must still be excluded from the sum.
func TestResolveNetAppStorage_SANPolicyOnLUN(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol:    sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(
			qosSample("pvc-x", "oc", "svm", "User-Best_effort", 150),
			lunQosSample("pvc-x", "oc", "svm", "gold-tier", "/vol/trident_pvc_x/lun0", 90),
		),
		maxIOPS: sampleVec(policySample("oc", "svm", "gold-tier", 5000)),
		maxMBps: sampleVec(policySample("oc", "svm", "gold-tier", 250)),
	}.run()
	require.Len(t, res.edges, 1)
	io := res.edges[0].IO
	require.NotNil(t, io)
	require.NotNil(t, io.ReadOps)
	assert.InDelta(t, 150.0, *io.ReadOps, 1e-12,
		"the LUN workload must not be summed on top of the volume workload")
	require.NotNil(t, io.MaxIOPS, "the ceiling must come from the LUN row's policy")
	assert.InDelta(t, 5000.0, *io.MaxIOPS, 1e-12)
	require.NotNil(t, io.MaxBytesPerSec)
	assert.InDelta(t, 250.0*1048576, *io.MaxBytesPerSec, 1e-12)
}

// The preference is data-driven, not a hardcoded list of ONTAP built-in class
// names: "User-Best_effort" sorts BEFORE "gold-tier", so a plain lexical pick
// would take it. The policy that the fixed-policy index actually holds wins.
func TestResolveNetAppStorage_PolicyPickPrefersAResolvingOne(t *testing.T) {
	assert.Less(t, "User-Best_effort", "gold-tier",
		"the built-in class must sort first, or this test proves nothing")
	res := netappFixture{
		claims: claim1(),
		vol:    sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(
			lunQosSample("pvc-x", "oc", "svm", "gold-tier", "/vol/trident_pvc_x/lun0", 90),
			qosSample("pvc-x", "oc", "svm", "User-Best_effort", 150),
		),
		maxIOPS: sampleVec(policySample("oc", "svm", "gold-tier", 5000)),
	}.run()
	require.NotNil(t, res.edges[0].IO.MaxIOPS)
	assert.InDelta(t, 5000.0, *res.edges[0].IO.MaxIOPS, 1e-12)
}

// A LUN-only volume has no volume-level row to measure: the edge is still
// drawn (hop A resolved it) but carries no metrics at all, so no ceiling can
// ride along either — the attachment invariant holds under the widened read.
func TestResolveNetAppStorage_LUNOnlyWorkloadMeasuresNothing(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(lunQosSample("pvc-x", "oc", "svm", "gold-tier", "/vol/trident_pvc_x/lun0", 90)),
		maxIOPS: sampleVec(policySample("oc", "svm", "gold-tier", 5000)),
	}.run()
	require.Len(t, res.edges, 1)
	assert.Nil(t, res.edges[0].IO,
		"a LUN row alone is not a volume measurement, and a ceiling never rides alone")
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

	tp, err := ReadTopology(context.Background(), q, time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{})
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

	_, err := ReadTopology(context.Background(), q, time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{})
	require.NoError(t, err, "a failing QoS leg must not fail the build")
}

// The fan-out is 43 legs: the 37 that preceded the storage-flow work, plus the
// five Harvest controller legs (node_labels and the four system_node counters)
// resolving the netapp-node data.hardware / data.perf attributes, plus the one
// ALERTS leg of the alert overlay.
// Pinning the count catches a leg silently dropped or double-registered.
func TestReadTopology_FanOutLegCount(t *testing.T) {
	// The six QoS workload legs are no longer unconditional: they are issued
	// only for FlexVol names a loaded claim already matched, so the fan-out has
	// two shapes and both are pinned here.
	fanOut := func(t *testing.T, fixtures map[promql.Query]model.Vector) map[string]int {
		t.Helper()
		var mu sync.Mutex
		seen := map[string]int{}

		q := promqlmocks.NewMockQuerier(t)
		q.EXPECT().
			Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, name string, _ string, _ time.Time) (model.Vector, error) {
				mu.Lock()
				seen[name]++
				mu.Unlock()
				return fixtures[promql.Query(name)], nil
			}).
			Maybe()

		_, err := ReadTopology(context.Background(), q, time.Minute, time.Unix(1, 0).UTC(),
			Options{}, promql.Selector{})
		require.NoError(t, err)

		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, n := range seen {
			out[k] = n
		}
		return out
	}

	assertOncePerLeg := func(t *testing.T, seen map[string]int, want int, msg string) {
		t.Helper()
		assert.Len(t, seen, want, msg)
		total := 0
		for _, n := range seen {
			total += n
		}
		assert.Equal(t, want, total, "each leg issued exactly once")
	}

	t.Run("no matched volume issues no QoS workload query", func(t *testing.T) {
		seen := fanOut(t, nil)
		assertOncePerLeg(t, seen, 37, "37 legs, the six QoS workload families withheld")
		for _, q := range promql.QoSWorkloadQueries {
			assert.Zero(t, seen[string(q)], string(q))
		}
	})

	t.Run("a matched volume adds the six QoS workload legs", func(t *testing.T) {
		seen := fanOut(t, map[promql.Query]model.Vector{
			promql.QPVCInfo: {&model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data",
				"volumename": "pvc-9f3a",
			}, Value: 1}},
			promql.QVolumeLabels: {&model.Sample{Metric: model.Metric{
				"volume": "trident_pvc_9f3a", "cluster": "ontap-prod",
				"node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm0",
			}, Value: 1}},
		})
		assertOncePerLeg(t, seen, 43, "one query per leg, no duplicates")
		for _, q := range promql.QoSWorkloadQueries {
			assert.Equal(t, 1, seen[string(q)], string(q))
		}
	})
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
	return ReadTopology(ctx, q, time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{})
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

// A claim can match more than one FlexVol name — two filers each carrying a
// volume whose name ends with the same derived token. The aggregate pick stays
// the lexically-smallest (ontap_cluster, aggr) pair and the result is
// independent of upstream vector order.
func TestResolveNetAppStorage_TwoVolumesMatchOneClaim(t *testing.T) {
	a := model.Sample{Metric: model.Metric{
		"volume": "trident_pvc_x", "cluster": "ontap-b", "node": "n-b", "aggr": "aggr-b", "svm": "svm0",
	}, Value: 1}
	b := model.Sample{Metric: model.Metric{
		"volume": "other_pvc_x", "cluster": "ontap-a", "node": "n-a", "aggr": "aggr-a", "svm": "svm0",
	}, Value: 1}

	fwd := netappFixture{claims: claim1(), vol: model.Vector{&a, &b}}.run()
	rev := netappFixture{claims: claim1(), vol: model.Vector{&b, &a}}.run()

	require.Len(t, fwd.edges, 1)
	assert.Equal(t, graph.NetAppAggrID("ontap-a", "aggr-a"), fwd.edges[0].Target,
		"the lexically-smallest (ontap_cluster, aggr) pair wins across matched volumes")
	assert.Equal(t, fwd.edges[0].Target, rev.edges[0].Target, "independent of vector order")
	assert.Equal(t, fwd.edges[0].ID, rev.edges[0].ID)
}

// QoS candidates are gathered across every FlexVol name a claim matched, in
// sorted name order, so the summed I/O is a pure function of the matched set.
func TestResolveNetAppStorage_QoSSummedAcrossMatchedVolumes(t *testing.T) {
	vol := func(v, oc, aggr string) *model.Sample {
		return &model.Sample{Metric: model.Metric{
			"volume": model.LabelValue(v), "cluster": model.LabelValue(oc),
			"node": "n", "aggr": model.LabelValue(aggr), "svm": "svm0",
		}, Value: 1}
	}
	qos := func(v, oc string, val float64) *model.Sample {
		return &model.Sample{Metric: model.Metric{
			"volume": model.LabelValue(v), "cluster": model.LabelValue(oc), "svm": "svm0",
		}, Value: model.SampleValue(val)}
	}

	f := netappFixture{
		claims:  claim1(),
		vol:     model.Vector{vol("trident_pvc_x", "ontap-a", "aggr1"), vol("other_pvc_x", "ontap-a", "aggr1")},
		readOps: model.Vector{qos("trident_pvc_x", "ontap-a", 10), qos("other_pvc_x", "ontap-a", 5)},
	}
	got := f.run()

	require.Len(t, got.edges, 1)
	require.NotNil(t, got.edges[0].IO)
	require.NotNil(t, got.edges[0].IO.ReadOps)
	assert.InDelta(t, 15.0, *got.edges[0].IO.ReadOps, 1e-12,
		"both matched volumes' workloads measure this claim")
}

// A derivation that does not fit the estate's FlexVol naming is REPORTED, not
// silent: every claim misses and the aggregated warning carries the full count.
// This is the operator's signal to tune the rewrite rules or the match mode.
func TestResolveNetAppStorage_DerivationMisfitIsCounted(t *testing.T) {
	claims := []pvcVolume{
		{id: "c/db/a", volumeName: "pvc-a"},
		{id: "c/db/b", volumeName: "pvc-b"},
		{id: "c/db/c", volumeName: "pvc-c"},
	}
	// A filer whose volume names embed no PV name at all.
	vol := model.Vector{
		&model.Sample{Metric: model.Metric{
			"volume": "vol0", "cluster": "ontap-prod", "node": "n", "aggr": "aggr1",
		}, Value: 1},
		&model.Sample{Metric: model.Metric{
			"volume": "root_vol", "cluster": "ontap-prod", "node": "n", "aggr": "aggr1",
		}, Value: 1},
	}

	var got netappResult
	recs := captureDebugRecords(t, func() {
		got = netappFixture{claims: claims, vol: vol}.run()
	})

	assert.Empty(t, got.edges, "nothing joined")
	assert.Empty(t, got.aggrs)
	require.True(t, hasMsg(recs, "netapp_volume_join_miss"))
	var count float64
	for _, r := range recs {
		if r["msg"] == "netapp_volume_join_miss" {
			count, _ = r["count"].(float64)
		}
	}
	assert.InDelta(t, float64(len(claims)), count, 1e-12,
		"every claim is counted, so the misfit is visible rather than silent")
	assert.False(t, hasMsg(recs, "netapp_qos_join_miss"),
		"no edge was drawn, so there is nothing for the I/O signal to report")
}
