package build

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// A claim's volume_labels candidates prefer series whose zone agrees with the
// claim's, and fall back to every matched series when none agree. An unknown
// zone on either side agrees (the alert-overlay rule). Series outside the
// chosen set still vote on the aggregate owner and still fill the inventory.

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

// Spec: "A claim with no agreeing series joins among every matched series".
func TestResolveNetAppStorage_NoAgreeingSeriesFallsBackToEveryMatch(t *testing.T) {
	res := netappFixture{
		claims: zonedClaim("zone-a", "prod"),
		vol: sampleVec(
			harvestInZone(volLabelSample("pvc-x", "ontap-b", "node-b", "aggr-q", "svm-b"), "zone-b", "prod"),
			harvestInZone(volLabelSample("pvc-x", "ontap-0", "node-0", "aggr-a", "svm-0"), "zone-c", "prod"),
		),
	}.run()
	require.Len(t, res.edges, 1)
	assert.Equal(t, graph.NetAppAggrID("ontap-0", "aggr-a"), res.edges[0].Target,
		"neither series agrees, so the lexically-smallest pair across every match wins")
	assert.Equal(t, SVMRef{ONTAPCluster: "ontap-0", SVM: "svm-0"}, res.svmByPVC[zonedClaimID])
}

// The fallback pick is a pure function of the matched set.
func TestResolveNetAppStorage_FallbackIsOrderFree(t *testing.T) {
	a := harvestInZone(volLabelSample("pvc-x", "ontap-b", "node-b", "aggr-q", "svm-b"), "zone-b", "prod")
	b := harvestInZone(volLabelSample("pvc-x", "ontap-0", "node-0", "aggr-a", "svm-0"), "zone-c", "prod")
	one := netappFixture{claims: zonedClaim("zone-a", "prod"), vol: sampleVec(a, b)}.run()
	two := netappFixture{claims: zonedClaim("zone-a", "prod"), vol: sampleVec(b, a)}.run()
	require.Len(t, one.edges, 1)
	require.Len(t, two.edges, 1)
	assert.Equal(t, one.edges[0].Target, two.edges[0].Target)
	assert.Equal(t, one.edges[0].ID, two.edges[0].ID)
	assert.Equal(t, one.svmByPVC, two.svmByPVC)
}

func zonedQoSClaim() netappFixture {
	return netappFixture{
		claims: zonedClaim("zone-a", "prod"),
		vol:    sampleVec(harvestInZone(volLabelSample("pvc-x", "ontap-a", "node-a", "aggr-z", "svm-a"), "zone-a", "prod")),
	}
}

func assertReadOps(t *testing.T, res netappResult, want float64) {
	t.Helper()
	require.Len(t, res.edges, 1)
	require.NotNil(t, res.edges[0].IO)
	require.NotNil(t, res.edges[0].IO.ReadOps)
	assert.InDelta(t, want, *res.edges[0].IO.ReadOps, 1e-12)
}

// Spec: "A workload labelled under two zones is counted once".
func TestResolveNetAppStorage_QoSWorkloadCountedOnce(t *testing.T) {
	fx := zonedQoSClaim()
	fx.readOps = sampleVec(
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-a", "prod"),
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-b", "prod"),
	)
	assertReadOps(t, fx.run(), 100)
}

// Spec: "No row of the workload carries the claim's zone". The policy group
// comes from the kept row alone: "alpha" sorts before "silver", so a mixed pick
// would name the other zone's policy.
func TestResolveNetAppStorage_QoSFallsBackToSmallestPair(t *testing.T) {
	fx := zonedQoSClaim()
	fx.readOps = sampleVec(
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "alpha", 80), "zone-c", "prod"),
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "silver", 100), "zone-b", "prod"),
	)
	fx.maxIOPS = sampleVec(
		policySample("ontap-a", "svm-a", "alpha", 800),
		policySample("ontap-a", "svm-a", "silver", 1000),
	)
	res := fx.run()
	assertReadOps(t, res, 100)
	require.NotNil(t, res.qosByPVC[zonedClaimID])
	assert.Equal(t, "silver", res.qosByPVC[zonedClaimID].PolicyGroup)
	require.NotNil(t, res.edges[0].IO.MaxIOPS)
	assert.InDelta(t, 1000.0, *res.edges[0].IO.MaxIOPS, 1e-12)
}

// Spec: "A row without a zone is preferred to another zone's". A row carrying
// no pair agrees with the claim (as in hop A), a row of another pair does not;
// the claim's own zone still beats both.
func TestResolveNetAppStorage_QoSUnlabelledRowBeatsAnotherZone(t *testing.T) {
	foreign := zonedQoSClaim()
	foreign.readOps = sampleVec(
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-b", "prod"),
		qosSample("pvc-x", "ontap-a", "svm-a", "gold", 60),
	)
	assertReadOps(t, foreign.run(), 60)

	own := zonedQoSClaim()
	own.readOps = sampleVec(
		qosSample("pvc-x", "ontap-a", "svm-a", "gold", 60),
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-a", "prod"),
	)
	assertReadOps(t, own.run(), 100)
}

// Spec: "A zone choice never drops another workload". The zone is chosen per
// workload — FlexVol, LUN, family — never once for the whole claim.
func TestResolveNetAppStorage_QoSEachWorkloadKeepsItsOwnZone(t *testing.T) {
	t.Run("a second FlexVol without a zone is still summed", func(t *testing.T) {
		other := func(s model.Sample) model.Sample {
			s.Metric["volume"] = "other_pvc_x"
			return s
		}
		fx := zonedQoSClaim()
		fx.vol = sampleVec(
			harvestInZone(volLabelSample("pvc-x", "ontap-a", "node-a", "aggr-z", "svm-a"), "zone-a", "prod"),
			other(volLabelSample("pvc-x", "ontap-a", "node-a", "aggr-z", "svm-a")),
		)
		fx.readOps = sampleVec(
			harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-a", "prod"),
			other(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 5)),
		)
		assertReadOps(t, fx.run(), 105)
	})
	t.Run("an own-zone LUN row never hides another zone's volume row", func(t *testing.T) {
		fx := zonedQoSClaim()
		fx.readOps = sampleVec(
			harvestInZone(lunQosSample("pvc-x", "ontap-a", "svm-a", "gold", "/vol/x/lun0", 90), "zone-a", "prod"),
			harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "User-Best_effort", 150), "zone-b", "prod"),
		)
		fx.maxIOPS = sampleVec(policySample("ontap-a", "svm-a", "gold", 1000))
		res := fx.run()
		assertReadOps(t, res, 150)
		require.NotNil(t, res.qosByPVC[zonedClaimID])
		assert.Equal(t, "gold", res.qosByPVC[zonedClaimID].PolicyGroup, "the LUN row still names the policy")
	})
	t.Run("a family the own zone lacks comes from the next zone", func(t *testing.T) {
		fx := zonedQoSClaim()
		fx.readOps = sampleVec(
			harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-a", "prod"),
			harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 100), "zone-b", "prod"),
		)
		fx.writeOps = sampleVec(harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "gold", 40), "zone-b", "prod"))
		res := fx.run()
		assertReadOps(t, res, 100)
		require.NotNil(t, res.edges[0].IO.WriteOps)
		assert.InDelta(t, 40.0, *res.edges[0].IO.WriteOps, 1e-12)
	})
}

// Spec: "Estate without Harvest zone labels is unchanged".
func TestResolveNetAppStorage_QoSWithoutZoneLabelsSumsEveryInScopeRow(t *testing.T) {
	fx := netappFixture{
		claims: claim1(),
		vol:    sampleVec(volLabelSample("pvc-x", "ontap-a", "node-a", "aggr1", "svm-a")),
		readOps: sampleVec(
			qosSample("pvc-x", "ontap-a", "svm-a", "", 10),
			qosSample("pvc-x", "ontap-a", "svm-a", "", 20),
		),
	}
	assertReadOps(t, fx.run(), 30)
}

// A row of the claim's zone that exists only on another filer is out of scope,
// so it cannot win and blank the measurement of the filer the edge points at.
func TestResolveNetAppStorage_QoSZoneOnAnotherFilerNeverWins(t *testing.T) {
	fx := zonedQoSClaim()
	fx.readOps = sampleVec(
		harvestInZone(qosSample("pvc-x", "ontap-b", "svm-b", "foreign", 900), "zone-a", "prod"),
		harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "silver", 100), "zone-b", "prod"),
	)
	fx.maxIOPS = sampleVec(
		policySample("ontap-a", "svm-a", "silver", 1000),
		policySample("ontap-b", "svm-b", "foreign", 9000),
	)
	res := fx.run()
	assertReadOps(t, res, 100)
	require.NotNil(t, res.qosByPVC[zonedClaimID])
	assert.Equal(t, "silver", res.qosByPVC[zonedClaimID].PolicyGroup)
}

// The kept rows, their sum and their policy are identical under a shuffled
// vector.
func TestResolveNetAppStorage_QoSZoneChoiceIsOrderFree(t *testing.T) {
	own := harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "silver", 100), "zone-b", "prod")
	other := harvestInZone(qosSample("pvc-x", "ontap-a", "svm-a", "alpha", 80), "zone-c", "prod")
	policies := sampleVec(
		policySample("ontap-a", "svm-a", "alpha", 800),
		policySample("ontap-a", "svm-a", "silver", 1000),
	)
	run := func(ops ...model.Sample) netappResult {
		fx := zonedQoSClaim()
		fx.readOps = sampleVec(ops...)
		fx.maxIOPS = policies
		return fx.run()
	}
	one, two := run(own, other), run(other, own)
	assertReadOps(t, one, 100)
	assertReadOps(t, two, 100)
	assert.Equal(t, one.qosByPVC[zonedClaimID].PolicyGroup, two.qosByPVC[zonedClaimID].PolicyGroup)
	assert.InDelta(t, *one.edges[0].IO.ReadOps, *two.edges[0].IO.ReadOps, 1e-12)
	assert.InDelta(t, *one.edges[0].IO.MaxIOPS, *two.edges[0].IO.MaxIOPS, 1e-12)
}
