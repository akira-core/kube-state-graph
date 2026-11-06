package build

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// deltaVectors names the six delta vectors a test sets, the way netappFixture
// names the measurement ones.
type deltaVectors struct {
	readOps, writeOps, readLat, writeLat, readData, writeData model.Vector
}

func (d deltaVectors) apply(v topologyVectors) topologyVectors {
	v.QoSReadOpsDelta, v.QoSWriteOpsDelta = d.readOps, d.writeOps
	v.QoSReadLatencyDelta, v.QoSWriteLatencyDelta = d.readLat, d.writeLat
	v.QoSReadDataDelta, v.QoSWriteDataDelta = d.readData, d.writeData
	return v
}

func (f netappFixture) runDelta(d deltaVectors) netappResult {
	return resolveNetAppStorage(f.claims, d.apply(f.vectors()), promql.LabelKeys{})
}

func requireIO(t *testing.T, res netappResult) *graph.IOMetrics {
	t.Helper()
	require.Len(t, res.edges, 1)
	require.NotNil(t, res.edges[0].IO)
	return res.edges[0].IO
}

func assertPtr(t *testing.T, want float64, got *float64, msg string) {
	t.Helper()
	require.NotNil(t, got, msg)
	assert.InDelta(t, want, *got, 1e-12, msg)
}

// otherVolume rewrites a QoS or volume_labels sample onto a second FlexVol of
// the same claim.
func otherVolume(s model.Sample, vol string) model.Sample {
	s.Metric = s.Metric.Clone()
	s.Metric["volume"] = model.LabelValue(vol)
	return s
}

func deltaFixture() netappFixture {
	return netappFixture{
		claims: claim1(),
		vol:    sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(
			qosSample("pvc-x", "oc", "svm", "gold", 100),
		),
		maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
	}
}

// Spec: "A claim sums the workloads that have a delta" and "The measurement's
// reader rules apply".
func TestIODelta_ClaimDeltaIsTheSumOfItsKeptWorkloads(t *testing.T) {
	fx := deltaFixture()
	fx.vol = sampleVec(
		volLabelSample("pvc-x", "oc", "n1", "a1", "svm"),
		otherVolume(volLabelSample("pvc-x", "oc", "n1", "a1", "svm"), "other_pvc_x"),
	)
	fx.readOps = sampleVec(
		qosSample("pvc-x", "oc", "svm", "gold", 300),
		otherVolume(qosSample("pvc-x", "oc", "svm", "gold", 200), "other_pvc_x"),
		// The LUN workload is measured too, so its delta row passes the
		// label-change gate and it is the LUN discard that drops it.
		lunQosSample("pvc-x", "oc", "svm", "gold", "lun0", 9),
	)
	fx.writeOps = sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 40))
	fx.readLat = sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 400))
	fx.writeLat = sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 500))
	fx.readData = sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 4_000_000))
	fx.writeData = sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 7))

	io := requireIO(t, fx.runDelta(deltaVectors{
		readOps: sampleVec(
			qosSample("pvc-x", "oc", "svm", "gold", 50),
			otherVolume(qosSample("pvc-x", "oc", "svm", "gold", -20), "other_pvc_x"),
			// A LUN-level delta row is discarded like the LUN measurement row.
			lunQosSample("pvc-x", "oc", "svm", "gold", "lun0", 30),
		),
		writeOps:  sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 0)),
		readLat:   sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 250)),
		writeLat:  sampleVec(qosSample("pvc-x", "oc", "svm", "gold", -5.5)),
		readData:  sampleVec(qosSample("pvc-x", "oc", "svm", "gold", -3_000_000)),
		writeData: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 1)),
	}))

	assertPtr(t, 500, io.ReadOps, "read_ops still sums both workloads' measurements")
	assertPtr(t, 30, io.ReadOpsDelta, "50 + -20; the LUN row's 30 is discarded")
	assertPtr(t, 0, io.WriteOpsDelta, "an unchanged rate is a zero delta, not absent")
	assertPtr(t, 250, io.ReadLatencyUsDelta, "")
	assertPtr(t, -5.5, io.WriteLatencyUsDelta, "")
	assertPtr(t, -3_000_000, io.ReadBytesPerSecDelta, "")
	assertPtr(t, 1, io.WriteBytesPerSecDelta, "")
}

// Spec: "A claim sums the workloads that have a delta".
func TestIODelta_AWorkloadWithNoDeltaContributesNone(t *testing.T) {
	fx := deltaFixture()
	fx.vol = sampleVec(
		volLabelSample("pvc-x", "oc", "n1", "a1", "svm"),
		otherVolume(volLabelSample("pvc-x", "oc", "n1", "a1", "svm"), "other_pvc_x"),
	)
	fx.readOps = sampleVec(
		qosSample("pvc-x", "oc", "svm", "gold", 300),
		otherVolume(qosSample("pvc-x", "oc", "svm", "gold", 200), "other_pvc_x"),
	)
	fx.writeOps = sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 40))

	io := requireIO(t, fx.runDelta(deltaVectors{
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 10)), // the other workload has no start sample
	}))
	assertPtr(t, 500, io.ReadOps, "the measurement keeps both workloads")
	assertPtr(t, 10, io.ReadOpsDelta, "")
	assertPtr(t, 40, io.WriteOps, "")
	assert.Nil(t, io.WriteOpsDelta, "no delta row at all: the field is absent, never 0")
	assert.Nil(t, io.ReadLatencyUs)
	assert.Nil(t, io.ReadLatencyUsDelta)
}

// Spec: "One zone per QoS workload" and "QoS candidates belong to the claim's
// own volume" apply to the delta rows.
func TestIODelta_ZoneChoiceAndScopeApplyToDeltaRows(t *testing.T) {
	fx := zonedQoSClaim()
	fx.readOps = sampleVec(
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-a", "prod"),
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-b", "prod"),
		// The foreign filer's workload is measured too, so its delta row
		// passes the label-change gate and it is the scope test that drops it.
		harvestInZone(qosSample("pvc-x", "ontap-other", "svm-a", "gold", 6000), "zone-a", "prod"),
	)
	io := requireIO(t, fx.runDelta(deltaVectors{
		readOps: sampleVec(
			// The same workload scraped under two zones counts once, in the claim's.
			harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 50), "zone-a", "prod"),
			harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 900), "zone-b", "prod"),
			// A row on another filer carrying the same FlexVol name is out of scope.
			harvestInZone(qosSample("pvc-x", "ontap-other", "svm-a", "gold", 7000), "zone-a", "prod"),
		),
	}))
	assertPtr(t, 100, io.ReadOps, "")
	assertPtr(t, 50, io.ReadOpsDelta, "one zone per workload; the foreign filer's row never contributes")
}

// A delta never creates a measurement: it attaches only to an existing
// IOMetrics.
func TestIODelta_NeverCreatesAMeasurement(t *testing.T) {
	fx := netappFixture{
		claims: claim1(),
		vol:    sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
	}
	res := fx.runDelta(deltaVectors{
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 50)),
	})
	require.Len(t, res.edges, 1)
	assert.Nil(t, res.edges[0].IO, "delta rows alone leave the edge measurement-less")
}

// Spec: "Deltas never change the policy pick, the ceiling or the coverage
// signal". The ceiling is resolved from measurement rows only.
func TestIODelta_DoesNotChangeCeilingOrThePolicyPick(t *testing.T) {
	// The measurement names policy "gold" (5000 iops). The delta rows add a
	// LUN workload under "bronze", which the fixed-policy index also holds and
	// which sorts first, so it would win the pick if a delta row reached it.
	// The label-change gate already removes it (the measurement has no such
	// series), so a delta index never holds a label set the measurement lacks;
	// what this pins is that the ceiling and the pick read measurement rows
	// only.
	fx := deltaFixture()
	fx.maxIOPS = sampleVec(policySample("oc", "svm", "gold", 5000), policySample("oc", "svm", "bronze", 100))

	without := fx.run()
	with := fx.runDelta(deltaVectors{
		readOps: sampleVec(
			qosSample("pvc-x", "oc", "svm", "gold", 5),
			lunQosSample("pvc-x", "oc", "svm", "bronze", "lun0", 6),
		),
	})
	assert.Equal(t, without.qosByPVC, with.qosByPVC, "node ceiling unchanged")
	require.Len(t, with.edges, 1)
	ioW, ioWithout := with.edges[0].IO, without.edges[0].IO
	require.NotNil(t, ioW)
	assertPtr(t, *ioWithout.ReadOps, ioW.ReadOps, "")
	assertPtr(t, *ioWithout.MaxIOPS, ioW.MaxIOPS, "edge ceiling unchanged")
	assertPtr(t, 5, ioW.ReadOpsDelta, "")
	assert.Equal(t, without.svmByPVC, with.svmByPVC)

	// Every non-delta field of the edge is identical with and without delta rows.
	stripped := *ioW
	stripped.ReadOpsDelta = nil
	assert.Equal(t, *ioWithout, stripped)
}

// withLabel returns s as another series: the same workload under one more
// label, the way a second poller instance would scrape it.
func withLabel(s model.Sample, name, value string) model.Sample {
	s.Metric = s.Metric.Clone()
	s.Metric[model.LabelName(name)] = model.LabelValue(value)
	return s
}

// Spec: "A series whose labels changed has no delta". The policy group moved
// from silver to gold inside the window. The upstream still returns the OLD
// series' change (it has samples on both sides of start), and nothing for the
// new one (no start sample); the reader discards the stale row.
func TestIODelta_ALabelChangeInsideTheWindowLeavesNoDelta(t *testing.T) {
	fx := deltaFixture()
	fx.readOps = sampleVec(
		qosSample("pvc-x", "oc", "svm", "silver", 80), // its last sample before the move
		qosSample("pvc-x", "oc", "svm", "gold", 100),
	)
	io := requireIO(t, fx.runDelta(deltaVectors{
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "silver", 30)),
	}))
	require.NotNil(t, io.ReadOps, "the measurement is unaffected")
	assert.Nil(t, io.ReadOpsDelta, "the stale change of the old series is discarded")
}

// The gate is per workload (volume, lun and family included), never per
// claim: the changed workload drops out and the rest keep their deltas.
func TestIODelta_TheLabelChangeGateIsPerWorkload(t *testing.T) {
	fx := deltaFixture()
	fx.vol = sampleVec(
		volLabelSample("pvc-x", "oc", "n1", "a1", "svm"),
		otherVolume(volLabelSample("pvc-x", "oc", "n1", "a1", "svm"), "other_pvc_x"),
	)
	fx.readOps = sampleVec(
		qosSample("pvc-x", "oc", "svm", "silver", 80),
		qosSample("pvc-x", "oc", "svm", "gold", 100),
		otherVolume(qosSample("pvc-x", "oc", "svm", "gold", 200), "other_pvc_x"),
	)
	fx.writeOps = sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 40))

	io := requireIO(t, fx.runDelta(deltaVectors{
		readOps: sampleVec(
			qosSample("pvc-x", "oc", "svm", "silver", 30),
			otherVolume(qosSample("pvc-x", "oc", "svm", "gold", -20), "other_pvc_x"),
		),
		writeOps: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 4)),
	}))
	assertPtr(t, -20, io.ReadOpsDelta, "the claim's other FlexVol keeps its delta")
	assertPtr(t, 4, io.WriteOpsDelta, "another family of the changed volume is its own workload")
}

// A LUN workload that appears inside the window is a group of its own: the
// volume-level delta of the same FlexVol is kept.
func TestIODelta_ANewLUNWorkloadLeavesTheVolumeDelta(t *testing.T) {
	fx := deltaFixture()
	fx.readOps = sampleVec(
		qosSample("pvc-x", "oc", "svm", "gold", 100),
		lunQosSample("pvc-x", "oc", "svm", "gold", "lun0", 60), // no start value
	)
	io := requireIO(t, fx.runDelta(deltaVectors{
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 5)),
	}))
	assertPtr(t, 5, io.ReadOpsDelta, "")
}

// withoutLabel returns s as another series lacking one label.
func withoutLabel(s model.Sample, name string) model.Sample {
	s.Metric = s.Metric.Clone()
	delete(s.Metric, model.LabelName(name))
	return s
}

// The svm is part of the zone choice's workload key but not of the gate's
// group, so a series whose svm label appears inside the window (a template
// change) or whose svm is renamed is caught like any other relabel.
func TestIODelta_AnSVMRelabelInsideTheWindowLeavesNoDelta(t *testing.T) {
	t.Run("svm label appears", func(t *testing.T) {
		fx := deltaFixture()
		fx.readOps = sampleVec(
			withoutLabel(qosSample("pvc-x", "oc", "svm", "gold", 80), "svm"),
			qosSample("pvc-x", "oc", "svm", "gold", 100),
		)
		io := requireIO(t, fx.runDelta(deltaVectors{
			readOps: sampleVec(withoutLabel(qosSample("pvc-x", "oc", "svm", "gold", 30), "svm")),
		}))
		require.NotNil(t, io.ReadOps)
		assert.Nil(t, io.ReadOpsDelta, "the svm-less series is in scope for every svm, and its change is stale")
	})
	t.Run("svm renamed", func(t *testing.T) {
		// Both names reach volume_labels over the window; the pick lands on
		// "svm", the old name, whose series stopped inside the window.
		fx := deltaFixture()
		fx.vol = sampleVec(
			volLabelSample("pvc-x", "oc", "n1", "a1", "svm"),
			volLabelSample("pvc-x", "oc", "n1", "a1", "svm_new"),
		)
		fx.readOps = sampleVec(
			qosSample("pvc-x", "oc", "svm", "gold", 80),
			qosSample("pvc-x", "oc", "svm_new", "gold", 100),
		)
		io := requireIO(t, fx.runDelta(deltaVectors{
			readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 30)),
		}))
		require.NotNil(t, io.ReadOps)
		assert.Nil(t, io.ReadOpsDelta)
	})
}

// Ignoring the svm in the gate's group never drops a steady delta: one FlexVol
// name in two SVMs of one filer, both scraped throughout, keeps both deltas,
// and the claim reads its own SVM's.
func TestIODelta_OneFlexVolNameInTwoSVMsKeepsItsDelta(t *testing.T) {
	fx := deltaFixture()
	fx.readOps = sampleVec(
		qosSample("pvc-x", "oc", "svm", "gold", 100),
		qosSample("pvc-x", "oc", "svm_other", "gold", 900),
	)
	io := requireIO(t, fx.runDelta(deltaVectors{
		readOps: sampleVec(
			qosSample("pvc-x", "oc", "svm", "gold", 5),
			qosSample("pvc-x", "oc", "svm_other", "gold", 70),
		),
	}))
	assertPtr(t, 100, io.ReadOps, "")
	assertPtr(t, 5, io.ReadOpsDelta, "the other SVM's row is out of scope, not a relabel")
}

// Two series of one workload that both have a start value (two poller
// instances scraping it throughout) are an unchanged set: both deltas count,
// as both measurements do.
func TestIODelta_AnUnchangedSeriesSetKeepsItsDelta(t *testing.T) {
	fx := deltaFixture()
	fx.readOps = sampleVec(
		withLabel(qosSample("pvc-x", "oc", "svm", "gold", 60), "instance", "poller-1"),
		withLabel(qosSample("pvc-x", "oc", "svm", "gold", 40), "instance", "poller-2"),
	)
	io := requireIO(t, fx.runDelta(deltaVectors{
		readOps: sampleVec(
			withLabel(qosSample("pvc-x", "oc", "svm", "gold", 10), "instance", "poller-1"),
			withLabel(qosSample("pvc-x", "oc", "svm", "gold", 5), "instance", "poller-2"),
		),
	}))
	assertPtr(t, 15, io.ReadOpsDelta, "")
}

// A series of the workload that appears inside the window without a start
// value is indistinguishable from a relabel: a poller of another zone that
// starts scraping the workload (or a poller re-zoned) drops the workload's
// delta, even though the zone-a copy alone has one. The gate compares the
// series across zones, so a relabel of the zone labels themselves is caught.
func TestIODelta_ASeriesNewInsideTheWindowLeavesNoDelta(t *testing.T) {
	fx := zonedQoSClaim()
	fx.readOps = sampleVec(
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-a", "prod"),
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-b", "prod"),
	)
	io := requireIO(t, fx.runDelta(deltaVectors{
		readOps: sampleVec(harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 50), "zone-a", "prod")),
	}))
	assertPtr(t, 100, io.ReadOps, "one zone per workload for the measurement")
	assert.Nil(t, io.ReadOpsDelta, "the zone-b series has no start value")
}

// A delta row naming a series the measurement read did not return (two reads
// of a moving upstream) is not trusted either.
func TestIODelta_ADeltaSeriesTheMeasurementLacksLeavesNoDelta(t *testing.T) {
	fx := deltaFixture()
	io := requireIO(t, fx.runDelta(deltaVectors{
		readOps: sampleVec(
			qosSample("pvc-x", "oc", "svm", "gold", 5),
			qosSample("pvc-x", "oc", "svm", "silver", 7),
		),
	}))
	assertPtr(t, 100, io.ReadOps, "")
	assert.Nil(t, io.ReadOpsDelta)
}

// The metric name never takes part in a series identity: a store whose range
// function keeps __name__ on the measurement and drops it from the arithmetic
// result still pairs the two. The shared vector is never mutated.
func TestIODelta_TheGateIgnoresTheMetricName(t *testing.T) {
	fx := deltaFixture()
	named := qosSample("pvc-x", "oc", "svm", "gold", 100)
	named.Metric[model.MetricNameLabel] = "qos_read_ops"
	fx.readOps = sampleVec(named)

	io := requireIO(t, fx.runDelta(deltaVectors{
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "gold", 5)),
	}))
	assertPtr(t, 5, io.ReadOpsDelta, "")
	assert.Equal(t, model.LabelValue("qos_read_ops"), fx.readOps[0].Metric[model.MetricNameLabel],
		"the cached measurement row keeps its name")
}
