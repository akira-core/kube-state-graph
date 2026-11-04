package build

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// readQoSRollup runs the scoped QoS read under rollup against fx. PVCInfo and
// VolumeLabels are already loaded, the way the second wave sees them; the
// fake answers the six workload queries from fx and records the rendered
// strings.
func readQoSRollup(t *testing.T, fx map[promql.Query]model.Vector, roll promql.Rollup) (*promqlfake.Querier, topologyVectors) {
	t.Helper()
	q := promqlfake.New(fx)
	v := topologyVectors{
		PVCInfo:      fx[promql.QPVCInfo],
		VolumeLabels: fx[promql.QVolumeLabels],
	}
	ctx := t.Context()
	done := make(chan struct{})
	close(done)
	var mu sync.Mutex
	require.NoError(t, readScopedQoS(ctx, ctx, q, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{}, &v, &mu, false, roll, done, done))
	return q, v
}

func rollupFuncName(roll promql.Rollup) string {
	switch roll {
	case promql.RollupMax:
		return "max_over_time"
	case promql.RollupAvg:
		return "avg_over_time"
	default:
		return "last_over_time"
	}
}

func assertQoSFunction(t *testing.T, q *promqlfake.Querier, roll promql.Rollup) {
	t.Helper()
	prefix := rollupFuncName(roll) + "("
	for _, family := range promql.QoSWorkloadQueries {
		got := q.QueriesFor(family)
		require.NotEmpty(t, got, family)
		for _, rendered := range got {
			assert.True(t, strings.HasPrefix(rendered, prefix), "%s: %s", family, rendered)
		}
	}
}

func ioFieldSet(io *graph.IOMetrics) []string {
	if io == nil {
		return nil
	}
	var out []string
	if io.ReadOps != nil {
		out = append(out, "read_ops")
	}
	if io.WriteOps != nil {
		out = append(out, "write_ops")
	}
	if io.ReadLatencyUs != nil {
		out = append(out, "read_latency_us")
	}
	if io.WriteLatencyUs != nil {
		out = append(out, "write_latency_us")
	}
	if io.ReadBytesPerSec != nil {
		out = append(out, "read_bytes_per_sec")
	}
	if io.WriteBytesPerSec != nil {
		out = append(out, "write_bytes_per_sec")
	}
	if io.MaxIOPS != nil {
		out = append(out, "max_iops")
	}
	if io.MaxBytesPerSec != nil {
		out = append(out, "max_bytes_per_sec")
	}
	return out
}

// twoWorkloadsAndALUN is one claim whose matched FlexVols are two volume-level
// workloads (300 and 200) plus a LUN row (90) the reader must discard.
func twoWorkloadsAndALUN() (netappFixture, map[promql.Query]model.Vector) {
	other := qosSample("pvc-a", "ontap-prod", "svm0", "", 200)
	other.Metric["volume"] = "other_pvc_a"
	otherVol := volLabelSample("pvc-a", "ontap-prod", "ontap-prod-01", "aggr1", "svm0")
	otherVol.Metric["volume"] = "other_pvc_a"
	fx := netappFixture{
		claims: []pvcVolume{{id: "c/db/data", volumeName: "pvc-a"}},
		vol: sampleVec(
			volLabelSample("pvc-a", "ontap-prod", "ontap-prod-01", "aggr1", "svm0"),
			otherVol,
		),
		readOps: sampleVec(
			qosSample("pvc-a", "ontap-prod", "svm0", "gold", 300),
			other,
			lunQosSample("pvc-a", "ontap-prod", "svm0", "gold", "lun0", 90),
		),
		maxIOPS: sampleVec(policySample("ontap-prod", "svm0", "gold", 5000)),
	}
	vec := map[promql.Query]model.Vector{
		promql.QPVCInfo:               {claimSample("db", "data", "pvc-a")},
		promql.QVolumeLabels:          fx.vol,
		promql.QQoSReadOps:            fx.readOps,
		promql.QQoSPolicyFixedMaxIOPS: fx.maxIOPS,
	}
	return fx, vec
}

func TestIORollup_SumsRolledUpWorkloads(t *testing.T) {
	direct, vec := twoWorkloadsAndALUN()
	assertReadOps(t, direct.run(), 500)

	var fields []string
	for _, roll := range []promql.Rollup{"", promql.RollupLast, promql.RollupMax, promql.RollupAvg} {
		q, v := readQoSRollup(t, vec, roll)
		assertQoSFunction(t, q, roll)
		v.QoSPolicyMaxIOPS = vec[promql.QQoSPolicyFixedMaxIOPS]
		res := resolveNetAppStorage(direct.claims, v, promql.LabelKeys{})
		assertReadOps(t, res, 500)
		got := ioFieldSet(res.edges[0].IO)
		if fields == nil {
			fields = got
			assert.Equal(t, []string{"read_ops", "max_iops"}, got)
			continue
		}
		assert.Equal(t, fields, got, "rollup %q", roll)
	}
}

func TestIORollup_ZoneChoiceAndLUNDiscardHold(t *testing.T) {
	// One zone per workload: the claim's own zone (100) beats the other zone's
	// copy of the same workload (900). Reuses the zoned QoS fixture.
	zoned := zonedQoSClaim()
	zoned.readOps = sampleVec(
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-a", "prod"),
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 900), "zone-b", "prod"),
	)
	assertReadOps(t, zoned.run(), 100)

	// LUN discard: the volume row (150) is the measurement; the LUN row is not.
	lun := netappFixture{
		claims:  claim1(),
		vol:     sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm")),
		readOps: sampleVec(qosSample("pvc-x", "oc", "svm", "", 150), lunQosSample("pvc-x", "oc", "svm", "gold", "lun0", 90)),
	}
	assertReadOps(t, lun.run(), 150)

	cases := []netappFixture{zoned, lun}
	for _, fx := range cases {
		vec := map[promql.Query]model.Vector{
			promql.QPVCInfo:      {claimSample("db", "data", fx.claims[0].volumeName)},
			promql.QVolumeLabels: fx.vol,
			promql.QQoSReadOps:   fx.readOps,
		}
		var fields []string
		for _, roll := range []promql.Rollup{promql.RollupMax, promql.RollupAvg, promql.RollupLast} {
			q, v := readQoSRollup(t, vec, roll)
			assertQoSFunction(t, q, roll)
			res := resolveNetAppStorage(fx.claims, v, promql.LabelKeys{})
			assertReadOps(t, res, *fx.run().edges[0].IO.ReadOps)
			got := ioFieldSet(res.edges[0].IO)
			if fields == nil {
				fields = got
				continue
			}
			assert.Equal(t, fields, got, "rollup %q", roll)
		}
	}
}
