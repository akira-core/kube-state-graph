package promql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allFamilies is the full family set, so a fixture that only cares about one
// rule still satisfies the "every family must be served" check.
func allFamilies() []Family { return append([]Family(nil), Families...) }

func be(name, rawURL string, families []Family, zones ...string) Backend {
	return NewBackend(name, rawURL, families, zones, "", "")
}

func TestNewTable_ValidTable(t *testing.T) {
	tbl, err := NewTable([]Backend{
		be("netapp-a", "http://vm-netapp:8428", []Family{FamilyHarvest}, "zone-a"),
		be("zone-a", "http://vm-a:8428", []Family{FamilyKSM, FamilyKubelet, FamilyServiceGraph, FamilyProbe}, "zone-a"),
	})
	require.NoError(t, err)
	require.Equal(t, 2, tbl.Len())

	// Backends are held in ascending name order — the merge order.
	names := []string{tbl.Backends()[0].Name(), tbl.Backends()[1].Name()}
	assert.Equal(t, []string{"netapp-a", "zone-a"}, names)
}

func TestNewTable_RejectsEmptyTable(t *testing.T) {
	_, err := NewTable(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no backends declared")
}

func TestNewTable_RejectsEmptyName(t *testing.T) {
	_, err := NewTable([]Backend{be("  ", "http://vm:8428", allFamilies())})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty name")
}

func TestNewTable_RejectsDuplicateName(t *testing.T) {
	_, err := NewTable([]Backend{
		be("zone-a", "http://vm-1:8428", allFamilies()),
		be("zone-a", "http://vm-2:8428", allFamilies()),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `backend "zone-a"`)
	assert.Contains(t, err.Error(), "duplicate backend name")
}

func TestNewTable_RejectsBadURL(t *testing.T) {
	cases := map[string]struct{ url, want string }{
		"empty":       {"", "empty url"},
		"no scheme":   {"vm.example:8428", "is not http or https"},
		"wrong sche.": {"ftp://vm.example:8428", `scheme "ftp"`},
		"no host":     {"http://", "has no host"},
		"unparseable": {"http://[::1", "not parseable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewTable([]Backend{be("zone-a", tc.url, allFamilies())})
			require.Error(t, err)
			assert.Contains(t, err.Error(), `backend "zone-a"`)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestNewTable_RejectsEmptyFamilies(t *testing.T) {
	_, err := NewTable([]Backend{be("zone-a", "http://vm:8428", nil)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declares no families")
}

func TestNewTable_RejectsUnknownFamily(t *testing.T) {
	_, err := NewTable([]Backend{be("zone-a", "http://vm:8428", []Family{"netapp"})})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown family "netapp"`)
	assert.Contains(t, err.Error(), "harvest", "the error must list the known families")
}

func TestNewTable_RejectsDuplicateFamily(t *testing.T) {
	_, err := NewTable([]Backend{be("zone-a", "http://vm:8428", []Family{FamilyKSM, FamilyKSM})})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `family "ksm" declared twice`)
}

func TestNewTable_RejectsEmptyZoneValue(t *testing.T) {
	_, err := NewTable([]Backend{be("zone-a", "http://vm:8428", allFamilies(), "zone-a", "")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty zone value")
}

// A family nobody serves is fatal: its queries would have nowhere to go, and
// the empty vector that produced would be indistinguishable from an estate
// that genuinely holds nothing.
func TestNewTable_RejectsUnservedFamily(t *testing.T) {
	_, err := NewTable([]Backend{
		be("k8s", "http://vm:8428", []Family{FamilyKSM, FamilyKubelet, FamilyServiceGraph, FamilyProbe}),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `family "harvest" is served by no backend`)
}

// Declaration order must not be observable: two tables differing only in the
// order of backends, families, or zones are indistinguishable — the same
// property Selector.render depends on for query determinism.
func TestNewTable_NormalisesDeclarationOrder(t *testing.T) {
	a, err := NewTable([]Backend{
		be("b", "http://vm-b:8428", []Family{FamilyHarvest, FamilyKSM}, "zone-b", "zone-a"),
		be("a", "http://vm-a:8428", []Family{FamilyKubelet, FamilyServiceGraph, FamilyProbe}),
	})
	require.NoError(t, err)
	b, err := NewTable([]Backend{
		be("a", "http://vm-a:8428", []Family{FamilyProbe, FamilyServiceGraph, FamilyKubelet}),
		be("b", "http://vm-b:8428", []Family{FamilyKSM, FamilyHarvest}, "zone-a", "zone-b", "zone-a"),
	})
	require.NoError(t, err)
	assert.Equal(t, a.String(), b.String())
}

// --- Select --------------------------------------------------------------

// ksmTable is the two-zone fixture every selection case below reads from:
// one backend per zone serving the zone-routable families, one NetApp backend
// per zone, and a catch-all serving the dimensionless families.
func ksmTable(t *testing.T) *Table {
	t.Helper()
	tbl, err := NewTable([]Backend{
		be("k-a", "http://vm-a:8428", []Family{FamilyKSM, FamilyKubelet}, "zone-a"),
		be("k-b", "http://vm-b:8428", []Family{FamilyKSM, FamilyKubelet}, "zone-b"),
		be("n-a", "http://netapp-a:8428", []Family{FamilyHarvest}, "zone-a"),
		be("n-b", "http://netapp-b:8428", []Family{FamilyHarvest}, "zone-b"),
		be("sg", "http://vm-sg:8428", []Family{FamilyServiceGraph, FamilyProbe}, "zone-a"),
	})
	require.NoError(t, err)
	return tbl
}

func names(bs []Backend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name()
	}
	return out
}

func TestSelect_ZoneSelectsSingleBackend(t *testing.T) {
	assert.Equal(t, []string{"k-a"}, names(ksmTable(t).Select(FamilyKSM, []string{"zone-a"})))
}

func TestSelect_AbsentZoneFansOut(t *testing.T) {
	assert.Equal(t, []string{"k-a", "k-b"}, names(ksmTable(t).Select(FamilyKSM, nil)))
}

// A bare `?az=` is a no-op at the matcher layer, so it must be a no-op at the
// routing layer too — otherwise an empty value would narrow the fan-out.
func TestSelect_EmptyZoneValueFansOut(t *testing.T) {
	assert.Equal(t, []string{"k-a", "k-b"}, names(ksmTable(t).Select(FamilyKSM, []string{""})))
}

func TestSelect_MultiZoneSelectsCoveringSubset(t *testing.T) {
	tbl, err := NewTable([]Backend{
		be("k-a", "http://vm-a:8428", allFamilies(), "zone-a"),
		be("k-b", "http://vm-b:8428", []Family{FamilyKSM}, "zone-b"),
		be("k-c", "http://vm-c:8428", []Family{FamilyKSM}, "zone-c"),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"k-a", "k-c"}, names(tbl.Select(FamilyKSM, []string{"zone-a", "zone-c"})))
}

func TestSelect_CatchAllAlwaysSelected(t *testing.T) {
	tbl, err := NewTable([]Backend{
		be("all", "http://vm-all:8428", allFamilies()),
		be("k-a", "http://vm-a:8428", []Family{FamilyKSM}, "zone-a"),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"all", "k-a"}, names(tbl.Select(FamilyKSM, []string{"zone-a"})))
	assert.Equal(t, []string{"all"}, names(tbl.Select(FamilyKSM, []string{"zone-z"})))
}

// Service-graph and probe accept no request dimension, so zones must be
// ignored for them entirely: narrowing them would drop edges the loaded
// topology still needs (design D4).
func TestSelect_DimensionlessFamiliesIgnoreZones(t *testing.T) {
	tbl, err := NewTable([]Backend{
		be("sg-a", "http://vm-a:8428", []Family{FamilyServiceGraph, FamilyProbe, FamilyKSM, FamilyKubelet, FamilyHarvest}, "zone-a"),
		be("sg-b", "http://vm-b:8428", []Family{FamilyServiceGraph, FamilyProbe}, "zone-b"),
	})
	require.NoError(t, err)
	for _, f := range []Family{FamilyServiceGraph, FamilyProbe} {
		assert.Equal(t, []string{"sg-a", "sg-b"}, names(tbl.Select(f, []string{"zone-a"})),
			"%s must ignore zones", f)
		assert.Equal(t, []string{"sg-a", "sg-b"}, names(tbl.Select(f, nil)),
			"%s must fan out unfiltered too", f)
	}
	// The zone-routable family on the same backends still narrows.
	assert.Equal(t, []string{"sg-a"}, names(tbl.Select(FamilyKSM, []string{"zone-a"})))
}

func TestSelect_HarvestRoutedByZone(t *testing.T) {
	assert.Equal(t, []string{"n-b"}, names(ksmTable(t).Select(FamilyHarvest, []string{"zone-b"})))
	assert.Equal(t, []string{"n-a", "n-b"}, names(ksmTable(t).Select(FamilyHarvest, nil)))
}

func TestSelect_UnmatchedZoneSelectsNothing(t *testing.T) {
	assert.Empty(t, ksmTable(t).Select(FamilyKSM, []string{"zone-z"}))
}

func TestSelect_NilTableSelectsNothing(t *testing.T) {
	var tbl *Table
	assert.Empty(t, tbl.Select(FamilyKSM, nil))
	assert.Equal(t, 0, tbl.Len())
	assert.Empty(t, tbl.Backends())
}

// --- Credential containment ---------------------------------------------

// A routing table is logged on every accepted reload, so its rendered form
// must never carry a credential value.
func TestTable_StringNeverCarriesCredentials(t *testing.T) {
	const pw = "s3cret-do-not-leak"
	const user = "ksg-upstream-user"
	tbl, err := NewTable([]Backend{
		NewBackend("zone-a", "http://vm-a:8428", allFamilies(), []string{"zone-a"}, user, pw),
	})
	require.NoError(t, err)

	for _, rendered := range []string{tbl.String(), tbl.Backends()[0].String()} {
		assert.NotContains(t, rendered, pw)
		assert.NotContains(t, rendered, user)
		assert.Contains(t, rendered, "auth=true", "the rendered form reports only WHETHER auth is configured")
		assert.Contains(t, rendered, "zone-a")
	}
}

func TestBackend_CredentialsRoundTrip(t *testing.T) {
	b := NewBackend("zone-a", "http://vm-a:8428", allFamilies(), nil, "u", "p")
	u, p := b.Credentials()
	assert.Equal(t, "u", u)
	assert.Equal(t, "p", p)
	assert.Contains(t, b.String(), "auth=true")

	none := NewBackend("zone-b", "http://vm-b:8428", allFamilies(), nil, "", "")
	assert.Contains(t, none.String(), "auth=false")
	assert.NotContains(t, none.String(), "auth=true")
}

// Accessors must hand back copies: a holder mutating the returned slice must
// not be able to reach into a live table.
func TestBackend_AccessorsReturnCopies(t *testing.T) {
	tbl, err := NewTable([]Backend{be("zone-a", "http://vm-a:8428", allFamilies(), "zone-a")})
	require.NoError(t, err)
	b := tbl.Backends()[0]

	fams := b.Families()
	fams[0] = "clobbered"
	zones := b.Zones()
	zones[0] = "clobbered"

	assert.NotContains(t, tbl.Backends()[0].Families(), Family("clobbered"))
	assert.NotContains(t, tbl.Backends()[0].Zones(), "clobbered")
}
