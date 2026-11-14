package build

import (
	"math"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

var nonFinite = []float64{math.NaN(), math.Inf(1), math.Inf(-1)}

// Spec: "A non-finite sample does not shadow a finite one". Each reader's
// smallest-value pick used to keep a NaN read first (100 < NaN is false), so the
// non-finite sample is placed BEFORE the finite one.
func TestNonFinite_UsageByAggrSkipsTheSample(t *testing.T) {
	m := model.Metric{"cluster": "oc", "aggr": "aggr1"}
	for _, bad := range nonFinite {
		used := sampleVec(model.Sample{Metric: m, Value: model.SampleValue(bad)}, model.Sample{Metric: m, Value: 100})
		total := sampleVec(model.Sample{Metric: m, Value: model.SampleValue(bad)})
		u := usageByAggr(used, total)[aggrKey{"oc", "aggr1"}]
		require.NotNil(t, u, "%v", bad)
		require.NotNil(t, u.UsedBytes)
		assert.InDelta(t, 100.0, *u.UsedBytes, 1e-12, "%v", bad)
		assert.Nil(t, u.CapacityBytes, "%v: a capacity with only a non-finite sample is absent", bad)
	}
}

func TestNonFinite_ResolvePVCUsageSkipsTheSample(t *testing.T) {
	m := model.Metric{"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data"}
	for _, bad := range nonFinite {
		used := sampleVec(model.Sample{Metric: m, Value: model.SampleValue(bad)})
		capacity := sampleVec(model.Sample{Metric: m, Value: model.SampleValue(bad)}, model.Sample{Metric: m, Value: 200})
		u := resolvePVCUsage(used, capacity, newClusterResolver(promql.LabelKeys{}))[pvcKey{"c", "db", "data"}]
		require.NotNil(t, u, "%v", bad)
		assert.Nil(t, u.UsedBytes, "%v: no used_bytes from a non-finite sample alone", bad)
		require.NotNil(t, u.CapacityBytes)
		assert.InDelta(t, 200.0, *u.CapacityBytes, 1e-12, "%v", bad)
	}
}

func TestNonFinite_PerfByNodeSkipsTheSample(t *testing.T) {
	m := model.Metric{"cluster": "oc", "node": "n1"}
	for _, bad := range nonFinite {
		perf := perfByNode(topologyVectors{
			NetAppNodeCPUBusy:  sampleVec(model.Sample{Metric: m, Value: model.SampleValue(bad)}, model.Sample{Metric: m, Value: 34}),
			NetAppNodeTotalOps: sampleVec(model.Sample{Metric: m, Value: model.SampleValue(bad)}),
		})[nodeKey{"oc", "n1"}]
		require.NotNil(t, perf, "%v", bad)
		require.NotNil(t, perf.CPUBusyPct)
		assert.InDelta(t, 34.0, *perf.CPUBusyPct, 1e-12, "%v", bad)
		assert.Nil(t, perf.TotalOps, "%v", bad)
	}
	assert.Nil(t, perfByNode(topologyVectors{
		NetAppNodeCPUBusy: sampleVec(model.Sample{Metric: m, Value: model.SampleValue(math.NaN())}),
	}), "a controller with only non-finite counters carries no perf")
}

func TestNonFinite_PolicyCeilingsSkipTheSample(t *testing.T) {
	m := model.Metric{"cluster": "oc", "svm": "svm0", "name": "gold"}
	for _, bad := range nonFinite {
		idx := indexPolicyCeilings(
			sampleVec(model.Sample{Metric: m, Value: model.SampleValue(bad)}, model.Sample{Metric: m, Value: 500}),
			sampleVec(model.Sample{Metric: m, Value: model.SampleValue(bad)}),
		)
		c := idx[policyKey{"oc", "svm0", "gold"}]
		require.NotNil(t, c, "%v", bad)
		require.NotNil(t, c.MaxIOPS)
		assert.InDelta(t, 500.0, *c.MaxIOPS, 1e-12, "%v", bad)
		assert.Nil(t, c.MaxBytesPerSec, "%v", bad)
	}
}

func TestNonFinite_QoSIndexSkipsTheSample(t *testing.T) {
	m := model.Metric{promql.HarvestVolumeLabel: "trident_pvc_x", "cluster": "oc", "svm": "svm0"}
	for _, bad := range nonFinite {
		dst := map[string][]qosCandidate{}
		indexQoSFamily(dst, sampleVec(model.Sample{Metric: m, Value: model.SampleValue(bad)}, model.Sample{Metric: m, Value: 7}),
			ioReadOps, promql.LabelKeys{}, false)
		require.Len(t, dst["trident_pvc_x"], 1, "%v: only the finite row is indexed", bad)
		assert.InDelta(t, 7.0, dst["trident_pvc_x"][0].value, 1e-12)
	}
}
