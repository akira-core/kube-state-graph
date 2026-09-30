package build

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// accept-multi-zone-storage-graph: a claim's volume_labels candidates are
// restricted to series whose zone agrees with the claim's. An unknown zone on
// either side never excludes (the alert-overlay rule); the excluded series still
// votes on the aggregate owner and still fills the inventory.

// harvestInZone stamps a series with the configured az / env labels.
func harvestInZone(s model.Sample, az, env string) model.Sample {
	s.Metric["az"], s.Metric["env"] = model.LabelValue(az), model.LabelValue(env)
	return s
}

func zonedClaim(az, env string) []pvcVolume {
	return []pvcVolume{{id: "zone-a-prod-c1/db/data", volumeName: "pvc-x", zone: zone{az: az, env: env}}}
}

const zonedClaimID = "zone-a-prod-c1/db/data"

// Spec: "Another zone's colliding FlexVol is never a candidate". (ontap-0,
// aggr-a) sorts before (ontap-a, aggr-z) but lives in zone-b, so the zone-a
// claim's edge, svm, measurement and ceiling all come from ontap-a.
func TestResolveNetAppStorage_CrossZoneCollisionPicksTheOwnZone(t *testing.T) {
	res := netappFixture{
		claims: zonedClaim("zone-a", "prod"),
		vol: sampleVec(
			harvestInZone(volLabelSample("pvc-x", "ontap-0", "node-0", "aggr-a", "svm-0"), "zone-b", "prod"),
			harvestInZone(volLabelSample("pvc-x", "ontap-a", "node-a", "aggr-z", "svm-a"), "zone-a", "prod"),
		),
		readOps: sampleVec(
			harvestInZone(qosSample("pvc-x", "ontap-0", "svm-0", "gold-b", 900), "zone-b", "prod"),
			harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold-a", 100), "zone-a", "prod"),
		),
		maxIOPS: sampleVec(
			harvestInZone(policySample("ontap-0", "svm-0", "gold-b", 9000), "zone-b", "prod"),
			harvestInZone(policySample("ontap-a", "svm-a", "gold-a", 1000), "zone-a", "prod"),
		),
	}.run()

	require.Len(t, res.edges, 1)
	assert.Equal(t, graph.NetAppAggrID("ontap-a", "aggr-z"), res.edges[0].Target, "the own-zone aggregate, although the other sorts first")
	assert.Equal(t, SVMRef{ONTAPCluster: "ontap-a", SVM: "svm-a"}, res.svmByPVC[zonedClaimID])
	require.NotNil(t, res.edges[0].IO)
	assert.InDelta(t, 100.0, *res.edges[0].IO.ReadOps, 1e-12, "the measurement is the own zone's, never summed with the other's")
	require.NotNil(t, res.edges[0].IO.MaxIOPS)
	assert.InDelta(t, 1000.0, *res.edges[0].IO.MaxIOPS, 1e-12, "the ceiling is the own zone's policy")

	// The excluded series still fills the inventory.
	aggrs := make([]string, 0, len(res.inventory.Aggrs))
	for _, a := range res.inventory.Aggrs {
		aggrs = append(aggrs, a.ID())
	}
	assert.ElementsMatch(t, []string{graph.NetAppAggrID("ontap-0", "aggr-a"), graph.NetAppAggrID("ontap-a", "aggr-z")}, aggrs)
}

// Spec: "An unknown zone never excludes".
func TestResolveNetAppStorage_UnknownZoneNeverExcludes(t *testing.T) {
	t.Run("series without a zone still competes", func(t *testing.T) {
		res := netappFixture{
			claims: zonedClaim("zone-a", "prod"),
			vol: sampleVec(
				volLabelSample("pvc-x", "ontap-0", "node-0", "aggr-a", "svm-0"), // no az / env
				harvestInZone(volLabelSample("pvc-x", "ontap-a", "node-a", "aggr-z", "svm-a"), "zone-a", "prod"),
			),
		}.run()
		require.Len(t, res.edges, 1)
		assert.Equal(t, graph.NetAppAggrID("ontap-0", "aggr-a"), res.edges[0].Target, "the lexically-smallest pair across both, as before this rule")
	})
	t.Run("a series with only one of the pair is unzoned", func(t *testing.T) {
		half := volLabelSample("pvc-x", "ontap-0", "node-0", "aggr-a", "svm-0")
		half.Metric["az"] = "zone-b"
		res := netappFixture{
			claims: zonedClaim("zone-a", "prod"),
			vol:    sampleVec(half),
		}.run()
		require.Len(t, res.edges, 1, "an incomplete pair is no zone, so it never excludes")
	})
	t.Run("a claim without a zone joins every series", func(t *testing.T) {
		res := netappFixture{
			claims: claim1(),
			vol: sampleVec(
				harvestInZone(volLabelSample("pvc-x", "ontap-0", "node-0", "aggr-a", "svm-0"), "zone-b", "prod"),
				harvestInZone(volLabelSample("pvc-x", "ontap-a", "node-a", "aggr-z", "svm-a"), "zone-a", "prod"),
			),
		}.run()
		require.Len(t, res.edges, 1)
		assert.Equal(t, graph.NetAppAggrID("ontap-0", "aggr-a"), res.edges[0].Target)
	})
}

// A different environment is a different zone even under the same az.
func TestResolveNetAppStorage_EnvironmentIsPartOfTheZone(t *testing.T) {
	res := netappFixture{
		claims: zonedClaim("zone-a", "prod"),
		vol: sampleVec(
			harvestInZone(volLabelSample("pvc-x", "ontap-0", "node-0", "aggr-a", "svm-0"), "zone-a", "dev"),
			harvestInZone(volLabelSample("pvc-x", "ontap-a", "node-a", "aggr-z", "svm-a"), "zone-a", "prod"),
		),
	}.run()
	require.Len(t, res.edges, 1)
	assert.Equal(t, graph.NetAppAggrID("ontap-a", "aggr-z"), res.edges[0].Target)
}

// Spec: "the excluded series still names its aggregate, controller and SVM for
// the owner vote". The vote runs over every series of the aggregate.
func TestResolveNetAppStorage_ExcludedSeriesStillVotesOnTheOwner(t *testing.T) {
	res := netappFixture{
		claims: zonedClaim("zone-a", "prod"),
		vol: sampleVec(
			harvestInZone(volLabelSample("pvc-x", "ontap-a", "node-a", "aggr1", "svm-a"), "zone-b", "prod"),
			harvestInZone(volLabelSample("pvc-x", "ontap-a", "node-z", "aggr1", "svm-a"), "zone-a", "prod"),
		),
	}.run()
	require.Len(t, res.aggrs, 1)
	assert.Equal(t, "node-a", res.aggrs[0].Labels()["node"], "the owner is the lexically-smallest across ALL series, as before")
}

// The join is deterministic under the filter: vector order never matters.
func TestResolveNetAppStorage_ZoneFilterIsOrderFree(t *testing.T) {
	a := harvestInZone(volLabelSample("pvc-x", "ontap-0", "node-0", "aggr-a", "svm-0"), "zone-b", "prod")
	b := harvestInZone(volLabelSample("pvc-x", "ontap-a", "node-a", "aggr-z", "svm-a"), "zone-a", "prod")
	one := netappFixture{claims: zonedClaim("zone-a", "prod"), vol: sampleVec(a, b)}.run()
	two := netappFixture{claims: zonedClaim("zone-a", "prod"), vol: sampleVec(b, a)}.run()
	require.Len(t, one.edges, 1)
	require.Len(t, two.edges, 1)
	assert.Equal(t, one.edges[0].ID, two.edges[0].ID)
	assert.Equal(t, one.svmByPVC, two.svmByPVC)
}
