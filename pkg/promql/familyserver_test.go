package promql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Serves is the unrestricted "any backend serves this family" test. A table
// that omits appmeta answers false; one that declares it answers true. A
// requested zone no backend declares is still served — Instant returns the
// empty vector for that miss — so the bound querier's zones do not change
// the answer.
func TestRouter_ServesAppMeta(t *testing.T) {
	var none *Router
	assert.False(t, none.Serves(FamilyAppMeta))

	with, err := NewTable([]Backend{be("all", "http://vm:8428", allFamilies())})
	require.NoError(t, err)
	served := routerWithFakes(t, with, map[string]*fakeBackend{"all": {}}, nil)
	assert.True(t, served.Serves(FamilyAppMeta))
	assertServes(t, served.QuerierFor(Selector{}), FamilyAppMeta, true)

	without, err := NewTable([]Backend{
		be("k8s", "http://vm:8428", []Family{FamilyKSM, FamilyKubelet, FamilyServiceGraph, FamilyProbe, FamilyAlerts}),
		be("netapp", "http://vm-netapp:8428", []Family{FamilyHarvest}),
	})
	require.NoError(t, err)
	unserved := routerWithFakes(t, without, map[string]*fakeBackend{
		"k8s":    {},
		"netapp": {},
	}, nil)
	assert.False(t, unserved.Serves(FamilyAppMeta))
	assert.True(t, unserved.Serves(FamilyKSM))
	assertServes(t, unserved.QuerierFor(Selector{AZ: []string{"zone-a"}}), FamilyAppMeta, false)

	zoned, err := NewTable([]Backend{
		be("k8s", "http://vm:8428", []Family{FamilyKSM, FamilyKubelet, FamilyHarvest, FamilyServiceGraph, FamilyProbe, FamilyAlerts}),
		be("meta-a", "http://meta:8428", []Family{FamilyAppMeta}, "zone-a"),
	})
	require.NoError(t, err)
	zoneMiss := routerWithFakes(t, zoned, map[string]*fakeBackend{
		"k8s":    {},
		"meta-a": {},
	}, nil)
	assert.True(t, zoneMiss.Serves(FamilyAppMeta), "a zone miss is not an unserved family")
	assertServes(t, zoneMiss.QuerierFor(Selector{AZ: []string{"zone-b"}}), FamilyAppMeta, true)
}

func assertServes(t *testing.T, q Querier, fam Family, want bool) {
	t.Helper()
	fs, ok := q.(FamilyServer)
	require.True(t, ok, "the querier a Router binds must satisfy FamilyServer")
	assert.Equal(t, want, fs.Serves(fam))
}
