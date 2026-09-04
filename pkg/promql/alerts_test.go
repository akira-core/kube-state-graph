package promql

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/internal/testlog"
)

// --- QAlerts rendering (design D7) ---------------------------------------

// The load-bearing part of the ALERTS contract is the dimension it does NOT
// carry. An alert expression does not reliably preserve the `cluster` label —
// an aggregation drops it, or the rule is written over a series that never had
// it — so a ?cluster= request must never narrow the alert read: it would
// silently delete alerts the estate genuinely has. az / env / namespace DO
// reach it, and the fixed alertstate="firing" selector is always rendered
// first so the request matchers compose with it rather than replace it.
func TestRender_AlertsCarriesEveryDimensionButCluster(t *testing.T) {
	sel := Selector{
		AZ:        []string{"zone-a"},
		Env:       []string{"prod"},
		Cluster:   []string{"c1"},
		Namespace: []string{"shop"},
	}
	got := Render(QAlerts, time.Minute, LabelKeys{}, sel)

	assert.Equal(t,
		`last_over_time(ALERTS{alertstate="firing",az="zone-a",env="prod",namespace="shop"}[1m])`,
		got)
	assert.NotContains(t, got, "cluster",
		"a ?cluster= value must never narrow the alert read")
}

func TestRender_AlertsHonoursConfiguredLabelKeys(t *testing.T) {
	sel := Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}
	got := Render(QAlerts, time.Minute, LabelKeys{AZ: "zone", Env: "stage"}, sel)
	assert.Equal(t,
		`last_over_time(ALERTS{alertstate="firing",zone="zone-a",stage="prod"}[1m])`,
		got)
}

// An unfiltered build reads every alert in the estate.
func TestRender_AlertsUnfiltered(t *testing.T) {
	assert.Equal(t,
		`last_over_time(ALERTS{alertstate="firing"}[5m])`,
		Render(QAlerts, 5*time.Minute, LabelKeys{}, Selector{}))
}

// --- FamilyAlerts routing --------------------------------------------------

// AcceptsAZ is DERIVED from queryDims, so the zone that routes the alerts
// backend and the zone rendered into its matcher are one fact read from one
// table. QAlerts carries dimAZ, so the family is zone-routable.
func TestFamilyAlerts_AcceptsAZ(t *testing.T) {
	assert.True(t, FamilyAlerts.AcceptsAZ(),
		"ALERTS carries dimAZ, so a ?az= request must select the zone's alerts backend")

	fam, ok := FamilyOf(QAlerts)
	require.True(t, ok)
	assert.Equal(t, FamilyAlerts, fam)
}

// alerts is the ONE family a valid table may leave unserved. Every other
// family stays mandatory.
func TestFamilyOptional(t *testing.T) {
	assert.True(t, FamilyAlerts.Optional())
	for _, f := range RequiredFamilies() {
		assert.False(t, f.Optional(), "family %q must stay required", f)
	}
	assert.Len(t, Families, 6)
	assert.Len(t, RequiredFamilies(), 5)

	_, ok := ParseFamily("alerts")
	assert.True(t, ok, "the routing file must accept the name")
}

// --- table validation ------------------------------------------------------

// Requiring `alerts` would have invalidated every backends file written before
// the overlay existed, so an unserved alerts family loads.
func TestNewTable_AcceptsUnservedOptionalFamily(t *testing.T) {
	tbl, err := NewTable([]Backend{
		be("k8s", "http://vm:8428", []Family{FamilyKSM, FamilyKubelet, FamilyServiceGraph, FamilyProbe}),
		be("netapp", "http://vm-netapp:8428", []Family{FamilyHarvest}),
	})
	require.NoError(t, err)
	assert.Equal(t, []Family{FamilyAlerts}, tbl.Unserved())
}

func TestTable_UnservedIsEmptyWhenAlertsServed(t *testing.T) {
	tbl, err := NewTable([]Backend{be("all", "http://vm:8428", allFamilies())})
	require.NoError(t, err)
	assert.Empty(t, tbl.Unserved())
}

// --- router behaviour ------------------------------------------------------

// An unserved alerts family answers an empty vector and logs at DEBUG. It must
// NOT take the "no backend serves this query for the requested zones" Warn
// path: being unserved is the documented normal state for this family, and
// warning once per build would train the operator to ignore the level.
func TestFanout_UnservedAlertsFamilyIsQuiet(t *testing.T) {
	buf := testlog.CaptureLevel(t, slog.LevelDebug)

	tbl, err := NewTable([]Backend{
		be("k8s", "http://vm:8428", []Family{FamilyKSM, FamilyKubelet, FamilyServiceGraph, FamilyProbe}),
		be("netapp", "http://vm-netapp:8428", []Family{FamilyHarvest}),
	})
	require.NoError(t, err)
	r := routerWithFakes(t, tbl, map[string]*fakeBackend{"k8s": {}, "netapp": {}}, nil)

	vec, err := r.QuerierFor(Selector{AZ: []string{"zone-a"}}).
		Instant(context.Background(), string(QAlerts), "q", time.Unix(0, 0))
	require.NoError(t, err, "an unserved optional family is never an error")
	assert.Empty(t, vec)

	out := buf.String()
	assert.Contains(t, out, "level=DEBUG")
	assert.Contains(t, out, "optional upstream family is served by no backend")
	assert.NotContains(t, out, "no upstream backend serves this query for the requested zones")
}

// A SERVED alerts family that simply holds no backend for the requested zone
// is a different situation and keeps the Warn: the operator declared an
// alerting store and this request reached none of it.
func TestFanout_ServedAlertsFamilyMissingZoneStillWarns(t *testing.T) {
	buf := testlog.CaptureLevel(t, slog.LevelDebug)

	tbl, err := NewTable([]Backend{
		be("k8s", "http://vm:8428", []Family{FamilyKSM, FamilyKubelet, FamilyServiceGraph, FamilyProbe}),
		be("netapp", "http://vm-netapp:8428", []Family{FamilyHarvest}),
		be("vmalert-a", "http://vmalert-a:8428", []Family{FamilyAlerts}, "zone-a"),
	})
	require.NoError(t, err)
	r := routerWithFakes(t, tbl, map[string]*fakeBackend{
		"k8s": {}, "netapp": {}, "vmalert-a": {},
	}, nil)

	vec, err := r.QuerierFor(Selector{AZ: []string{"zone-b"}}).
		Instant(context.Background(), string(QAlerts), "q", time.Unix(0, 0))
	require.NoError(t, err)
	assert.Empty(t, vec)
	assert.Contains(t, buf.String(), "no upstream backend serves this query for the requested zones")
}

// The alerts family is separable from kube-state-metrics: a dedicated alerts
// backend answers the ALERTS query and nothing else.
func TestFanout_AlertsReachesOnlyItsOwnBackend(t *testing.T) {
	tbl, err := NewTable([]Backend{
		be("k8s", "http://vm:8428", []Family{FamilyKSM, FamilyKubelet, FamilyHarvest, FamilyServiceGraph, FamilyProbe}),
		be("vmalert", "http://vmalert:8428", []Family{FamilyAlerts}),
	})
	require.NoError(t, err)
	k8s, vmalert := &fakeBackend{}, &fakeBackend{}
	r := routerWithFakes(t, tbl, map[string]*fakeBackend{"k8s": k8s, "vmalert": vmalert}, nil)

	q := r.QuerierFor(Selector{})
	_, err = q.Instant(context.Background(), string(QAlerts), "alerts-q", time.Unix(0, 0))
	require.NoError(t, err)
	_, err = q.Instant(context.Background(), string(QPodInfo), "ksm-q", time.Unix(0, 0))
	require.NoError(t, err)

	_, alertQueries := vmalert.seen()
	_, ksmQueries := k8s.seen()
	assert.Equal(t, []string{"alerts-q"}, alertQueries)
	assert.Equal(t, []string{"ksm-q"}, ksmQueries)
}
