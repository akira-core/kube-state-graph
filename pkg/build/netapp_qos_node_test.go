package build

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The ceiling is resolved ONCE per claim and surfaces on the PVC node whether
// or not the claim's edge carries a measurement (add-pvc-qos-ceiling-attribute
// D1). These tests pin the resolver's per-claim result, `qosByPVC`; the edge's
// own presence rule is pinned by the ceiling tests in netapp_test.go.

// Spec: "FlexGroup claim resolves its ceiling on the node" — SVM resolved, no
// aggregate: no edge, but the ceiling still resolves, keyed on the SVM pick's
// own ONTAP cluster.
func TestResolveNetAppStorage_FlexGroupClaimResolvesCeiling(t *testing.T) {
	res := netappFixture{
		claims:  []pvcVolume{{id: "c/db/data", volumeName: "pvc-fg"}},
		vol:     sampleVec(volLabelSample("pvc-fg", "ontap-prod", "n1", "", "svm_big")),
		readOps: sampleVec(qosSample("pvc-fg", "ontap-prod", "svm_big", "gold-tier", 10)),
		maxIOPS: sampleVec(policySample("ontap-prod", "svm_big", "gold-tier", 8000)),
	}.run()

	assert.Empty(t, res.edges, "no aggregate ⇒ no pvc-to-netapp-aggr edge")
	got := res.qosByPVC["c/db/data"]
	require.NotNil(t, got, "FlexGroup claim still resolves its ceiling")
	assert.Equal(t, "gold-tier", got.PolicyGroup)
	require.NotNil(t, got.MaxIOPS)
	assert.InDelta(t, 8000.0, *got.MaxIOPS, 1e-12)
	assert.Nil(t, got.MaxBytesPerSec, "no mbps series ⇒ that field stays absent")
}

// A FlexGroup claim has no edge for hop B to measure, so it never counts
// toward netapp_qos_join_miss — resolving its ceiling must not change that.
func TestResolveNetAppStorage_FlexGroupCeilingLeavesQoSMissCountAlone(t *testing.T) {
	recs := captureDebugRecords(t, func() {
		netappFixture{
			claims:  []pvcVolume{{id: "c/db/data", volumeName: "pvc-fg"}},
			vol:     sampleVec(volLabelSample("pvc-fg", "oc", "n1", "", "svm")),
			readOps: sampleVec(qosSample("pvc-fg", "oc", "svm", "gold", 10)),
			maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
		}.run()
	})
	assert.False(t, hasMsg(recs, "netapp_qos_join_miss"), "FlexGroup claim is never a QoS miss")
	assert.True(t, hasMsg(recs, "netapp_volume_join_miss"), "the topology miss is unchanged")
}

// Spec: "Ceiling on the node when the edge carries no measurement" — the only
// in-scope workload is a LUN-level row, which names the policy but is never
// summed, so the edge has no metrics while the node carries the ceiling.
func TestResolveNetAppStorage_LUNOnlyClaimCarriesNodeCeilingNotEdgeCeiling(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(lunQosSample("pvc-x", "oc", "svm", "gold-tier", "lun0", 90)),
		maxIOPS: sampleVec(policySample("oc", "svm", "gold-tier", 5000)),
		maxMBps: sampleVec(policySample("oc", "svm", "gold-tier", 250)),
	}.run()

	require.Len(t, res.edges, 1)
	assert.Nil(t, res.edges[0].IO, "no volume-level row ⇒ the edge carries no metrics, ceiling included")

	got := res.qosByPVC["c/db/data"]
	require.NotNil(t, got)
	assert.Equal(t, "gold-tier", got.PolicyGroup)
	require.NotNil(t, got.MaxIOPS)
	assert.InDelta(t, 5000.0, *got.MaxIOPS, 1e-12)
	require.NotNil(t, got.MaxBytesPerSec)
	assert.InDelta(t, 250.0*1048576, *got.MaxBytesPerSec, 1e-12)
}

// Spec: "Node and edge agree" — one resolution feeds both, so a measured claim's
// node ceiling equals its edge ceiling. The pointers are distinct cells.
func TestResolveNetAppStorage_NodeAndEdgeCeilingAgree(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "ontap-prod", "n1", "a1", "svm-prod")),
		readOps: sampleVec(qosSample("pvc-x", "ontap-prod", "svm-prod", "gold-tier", 1200)),
		maxIOPS: sampleVec(policySample("ontap-prod", "svm-prod", "gold-tier", 5000)),
		maxMBps: sampleVec(policySample("ontap-prod", "svm-prod", "gold-tier", 250)),
	}.run()

	io := res.edges[0].IO
	require.NotNil(t, io)
	got := res.qosByPVC["c/db/data"]
	require.NotNil(t, got)
	require.NotNil(t, io.MaxIOPS)
	require.NotNil(t, got.MaxIOPS)
	assert.InDelta(t, *io.MaxIOPS, *got.MaxIOPS, 1e-12)
	require.NotNil(t, io.MaxBytesPerSec)
	require.NotNil(t, got.MaxBytesPerSec)
	assert.InDelta(t, *io.MaxBytesPerSec, *got.MaxBytesPerSec, 1e-12)
	assert.NotSame(t, io.MaxIOPS, got.MaxIOPS, "edge and node never share a float cell")
	assert.NotSame(t, io.MaxBytesPerSec, got.MaxBytesPerSec)
}

// One policy-index entry serves every claim in the SVM; handing out its own
// floats would make all their nodes one shared mutable cell.
func TestResolveNetAppStorage_CeilingFloatsNotAliasedAcrossClaims(t *testing.T) {
	res := netappFixture{
		claims: []pvcVolume{
			{id: "c/db/a", volumeName: "pvc-a"},
			{id: "c/db/b", volumeName: "pvc-b"},
		},
		vol: sampleVec(
			volLabelSample("pvc-a", "oc", "n1", "a1", "svm"),
			volLabelSample("pvc-b", "oc", "n1", "a1", "svm"),
		),
		readOps: sampleVec(
			qosSample("pvc-a", "oc", "svm", "gold", 1),
			qosSample("pvc-b", "oc", "svm", "gold", 2),
		),
		maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
	}.run()

	a, b := res.qosByPVC["c/db/a"], res.qosByPVC["c/db/b"]
	require.NotNil(t, a)
	require.NotNil(t, b)
	assert.NotSame(t, a, b)
	assert.NotSame(t, a.MaxIOPS, b.MaxIOPS)
	*a.MaxIOPS = 1
	assert.InDelta(t, 5000.0, *b.MaxIOPS, 1e-12, "mutating one claim's figure must not move another's")
}

// An incomplete or unmatched key resolves nothing — never widened — and a
// policy group with no fixed-policy series is not surfaced as a ceiling.
func TestResolveNetAppStorage_UnresolvedCeilingLeavesNoEntry(t *testing.T) {
	cases := map[string]netappFixture{
		"no workload series at all": {
			claims:  claim1(),
			vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
			maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
		},
		"workload in no policy group": {
			claims:  claim1(),
			vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
			readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "", 10)),
			maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
		},
		"policy group holds no fixed-policy series": {
			claims:  claim1(),
			vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
			readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "User-Best_effort", 10)),
			maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
		},
		"claim resolved no svm": {
			claims:  claim1(),
			vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "")),
			readOps: sampleVec(qosSample("pvc-x", "oc", "", "gold", 10)),
			maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
		},
		"FlexGroup claim with no workload series": {
			claims:  claim1(),
			vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "", "svm")),
			maxIOPS: sampleVec(policySample("oc", "svm", "gold", 5000)),
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			res := f.run()
			assert.NotContains(t, res.qosByPVC, "c/db/data",
				"an unresolved ceiling is absent from the map, never a zero or empty entry")
		})
	}
}

// The FlexGroup key's ONTAP cluster comes from the SVM pick, not the workload
// series: a same-named SVM on a second filer must not lend its ceiling.
func TestResolveNetAppStorage_FlexGroupCeilingKeyedOnSVMPicksFiler(t *testing.T) {
	res := netappFixture{
		claims: []pvcVolume{{id: "c/db/data", volumeName: "pvc-fg"}},
		vol: sampleVec(
			volLabelSample("pvc-fg", "oc-a", "n1", "", "svm"),
			volLabelSample("pvc-fg", "oc-b", "n2", "", "svm"),
		),
		readOps: sampleVec(
			qosSample("pvc-fg", "oc-a", "svm", "gold", 10),
			qosSample("pvc-fg", "oc-b", "svm", "gold", 10),
		),
		maxIOPS: sampleVec(
			policySample("oc-a", "svm", "gold", 111),
			policySample("oc-b", "svm", "gold", 222),
		),
	}.run()

	got := res.qosByPVC["c/db/data"]
	require.NotNil(t, got)
	require.NotNil(t, got.MaxIOPS)
	assert.InDelta(t, 111.0, *got.MaxIOPS, 1e-12,
		"lexically-smallest filer carrying the winning SVM, matching the SVM node the storage graph draws")
}

// D6: the per-claim result is a pure function of the vectors, not of series
// order.
func TestResolveNetAppStorage_CeilingIndependentOfVectorOrder(t *testing.T) {
	vol := []model.Sample{
		volLabelSample("pvc-a", "oc", "n1", "a1", "svm"),
		volLabelSample("pvc-b", "oc", "n1", "", "svm"),
	}
	work := []model.Sample{
		qosSample("pvc-a", "oc", "svm", "gold", 1),
		qosSample("pvc-a", "oc", "svm", "bronze", 2),
		lunQosSample("pvc-b", "oc", "svm", "gold", "lun0", 3),
		qosSample("pvc-b", "oc", "svm", "silver", 4),
	}
	iops := []model.Sample{
		policySample("oc", "svm", "gold", 5000),
		policySample("oc", "svm", "bronze", 100),
		policySample("oc", "svm", "silver", 700),
	}
	mbps := []model.Sample{
		policySample("oc", "svm", "gold", 250),
		policySample("oc", "svm", "bronze", 10),
		policySample("oc", "svm", "silver", 70),
	}
	reversed := func(in []model.Sample) []model.Sample {
		out := make([]model.Sample, len(in))
		for i, s := range in {
			out[len(in)-1-i] = s
		}
		return out
	}
	run := func(vol, work, iops, mbps []model.Sample) netappResult {
		return netappFixture{
			claims: []pvcVolume{
				{id: "c/db/a", volumeName: "pvc-a"},
				{id: "c/db/b", volumeName: "pvc-b"},
			},
			vol:     sampleVec(vol...),
			readOps: sampleVec(work...),
			maxIOPS: sampleVec(iops...),
			maxMBps: sampleVec(mbps...),
		}.run()
	}

	want := run(vol, work, iops, mbps)
	got := run(reversed(vol), reversed(work), reversed(iops), reversed(mbps))
	require.Len(t, want.qosByPVC, 2)
	assert.Equal(t, want.qosByPVC, got.qosByPVC)
}
