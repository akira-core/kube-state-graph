package promql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueryFamily_EveryQueryListed is the completeness guard for the
// series × upstream-family contract, the routing counterpart of
// TestQueryDims_EveryQueryListed: every Query constant must have an explicit
// queryFamily entry, and queryFamily must not name a constant that no longer
// exists. A query with no family would have no backend to be dispatched to.
func TestQueryFamily_EveryQueryListed(t *testing.T) {
	consts := queryConstants(t)
	for name, q := range consts {
		if _, ok := queryFamily[q]; !ok {
			t.Errorf("Query constant %s (%q) has no queryFamily entry — add one", name, q)
		}
	}
	values := map[Query]struct{}{}
	for _, q := range consts {
		values[q] = struct{}{}
	}
	for q := range queryFamily {
		if _, ok := values[q]; !ok {
			t.Errorf("queryFamily names %q, which is not a Query constant", q)
		}
	}
}

// TestQueryFamily_EveryFamilyIsDeclared pins that no entry names a family
// outside Families — the set a routing table validates and must cover.
func TestQueryFamily_EveryFamilyIsDeclared(t *testing.T) {
	declared := map[Family]struct{}{}
	for _, f := range Families {
		declared[f] = struct{}{}
	}
	used := map[Family]struct{}{}
	for q, f := range queryFamily {
		if _, ok := declared[f]; !ok {
			t.Errorf("query %q names family %q, which is not in Families", q, f)
		}
		used[f] = struct{}{}
	}
	for _, f := range Families {
		assert.Contains(t, used, f, "family %q is declared but no query belongs to it", f)
	}
}

// TestQueryFamily_Membership pins the deliberate splits. The Harvest and
// kubelet groups are the ones the routing capability exists to separate, so
// they are enumerated rather than counted.
func TestQueryFamily_Membership(t *testing.T) {
	harvest := []Query{
		QVolumeLabels,
		QQoSReadOps, QQoSWriteOps,
		QQoSReadLatency, QQoSWriteLatency,
		QQoSReadData, QQoSWriteData,
		QQoSPolicyFixedMaxIOPS, QQoSPolicyFixedMaxMBps,
		QAggrStatus, QAggrSpaceUsed, QAggrSpaceTotal,
		QNetAppNodeStatus,
	}
	for _, q := range harvest {
		f, ok := FamilyOf(q)
		require.True(t, ok, "%s has no family", q)
		assert.Equal(t, FamilyHarvest, f, "%s must be a Harvest query", q)
	}
	assert.Len(t, harvest, 13, "the Harvest family is the 13 legs the storage chain reads")

	for _, q := range []Query{QKubeletVolumeUsedBytes, QKubeletVolumeCapacityBytes} {
		f, _ := FamilyOf(q)
		assert.Equal(t, FamilyKubelet, f, "%s must be a kubelet query", q)
	}
	for _, q := range []Query{QServiceGraphTotal, QServiceGraphFailedTotal, QServiceGraphServerSecondsBucket} {
		f, _ := FamilyOf(q)
		assert.Equal(t, FamilyServiceGraph, f, "%s must be a service-graph query", q)
	}
	f, _ := FamilyOf(QUpProbe)
	assert.Equal(t, FamilyProbe, f)
	f, _ = FamilyOf(QPodInfo)
	assert.Equal(t, FamilyKSM, f)
}

// TestFamilyOf_UnknownQuery pins that an unrecognised query reports not-found
// rather than defaulting into a family — the dispatch layer must be able to
// tell the difference.
func TestFamilyOf_UnknownQuery(t *testing.T) {
	f, ok := FamilyOf(Query("not_a_real_metric"))
	assert.False(t, ok)
	assert.Equal(t, Family(""), f)
}

// TestParseFamily pins the configuration-boundary parser: every declared name
// round-trips and an unknown name is rejected, never silently accepted.
func TestParseFamily(t *testing.T) {
	for _, want := range Families {
		got, ok := ParseFamily(string(want))
		assert.True(t, ok, "%q must parse", want)
		assert.Equal(t, want, got)
	}
	for _, bad := range []string{"", "KSM", "netapp", "kube-state-metrics", " ksm"} {
		_, ok := ParseFamily(bad)
		assert.False(t, ok, "%q must not parse", bad)
	}
}

// TestFamilyAcceptsAZ pins the zone-routable set. The service-graph and probe
// families accept no request dimension, so backend selection must never narrow
// them by zone — narrowing them would drop edges the loaded topology needs
// (design D4). Harvest is zone-routable through the routing-only dimAZRoute
// bit even though it renders no az matcher.
func TestFamilyAcceptsAZ(t *testing.T) {
	assert.True(t, FamilyKSM.AcceptsAZ())
	assert.True(t, FamilyKubelet.AcceptsAZ())
	assert.True(t, FamilyHarvest.AcceptsAZ())
	assert.False(t, FamilyServiceGraph.AcceptsAZ())
	assert.False(t, FamilyProbe.AcceptsAZ())
	assert.False(t, Family("not-a-family").AcceptsAZ(), "an unknown family is never zone-routable")
}

// TestFamilyAcceptsAZ_HomogeneousWithinFamily is the guard the derivation
// depends on: every query in a family must agree about zone-routability
// (dimAZ or dimAZRoute).
// A family whose queries disagreed would resolve to the conservative false,
// silently widening its fan-out — so the disagreement is caught here instead.
func TestFamilyAcceptsAZ_HomogeneousWithinFamily(t *testing.T) {
	seen := map[Family]bool{}
	first := map[Family]Query{}
	for q, f := range queryFamily {
		az := queryDims[q]&(dimAZ|dimAZRoute) != 0
		if prev, ok := seen[f]; ok {
			assert.Equal(t, prev, az,
				"family %q is inhomogeneous: %s and %s disagree about the az dimension",
				f, first[f], q)
			continue
		}
		seen[f] = az
		first[f] = q
	}
	for _, f := range Families {
		assert.Equal(t, seen[f], f.AcceptsAZ(), "AcceptsAZ must mirror queryDims for %q", f)
	}
}
