package promql

import (
	"testing"
	"time"

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

// Spec: "Dedicated store" / "Application metadata separable from
// kube-state-metrics". A zone-scoped appmeta backend receives the
// application-metadata query and nothing else, and no other backend receives
// it: not the kube-state-metrics store, and not an appmeta store of another
// zone.
func TestFanout_AppMetaReachesOnlyItsOwnBackend(t *testing.T) {
	tbl, err := NewTable([]Backend{
		be("k8s", "http://vm:8428", []Family{FamilyKSM, FamilyKubelet, FamilyHarvest, FamilyServiceGraph, FamilyProbe}),
		be("meta-a", "http://meta-a:8428", []Family{FamilyAppMeta}, "zone-a"),
		be("meta-b", "http://meta-b:8428", []Family{FamilyAppMeta}, "zone-b"),
	})
	require.NoError(t, err)
	k8s, metaA, metaB := &fakeBackend{}, &fakeBackend{}, &fakeBackend{}
	r := routerWithFakes(t, tbl, map[string]*fakeBackend{"k8s": k8s, "meta-a": metaA, "meta-b": metaB}, nil)

	q := r.QuerierFor(Selector{AZ: []string{"zone-a"}})
	_, err = q.Instant(t.Context(), string(QAppMetadata), "meta-q", time.Unix(0, 0))
	require.NoError(t, err)
	_, err = q.Instant(t.Context(), string(QPodInfo), "ksm-q", time.Unix(0, 0))
	require.NoError(t, err)

	_, aQueries := metaA.seen()
	_, bQueries := metaB.seen()
	_, ksmQueries := k8s.seen()
	assert.Equal(t, []string{"meta-q"}, aQueries)
	assert.Empty(t, bQueries)
	assert.Equal(t, []string{"ksm-q"}, ksmQueries)
}
