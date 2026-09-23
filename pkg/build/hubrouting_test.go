package build

import (
	"slices"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// The volume hub's routing and request matchers
// (read-storage-roots-through-volume-hub D7 / D8), over a *promql.Router whose
// every backend is a matcher-applying promqlfake: a hub build reads exactly the
// stores and series a non-hub build of the same request reads — the request's
// az selects every backend, and az / env stay matchers on every Kubernetes and
// ALERTS query.

// hubZonesFixture is two Kubernetes zones behind two ksm stores and one filer
// behind zone-a's harvest store. zone-b's harvest store holds a different
// filer, so a harvest query that reached it would load rows the request never
// asked for. Both zones run a cluster named c1, with a claim on ontap-prod
// each; zone-b's must stay out of a zone-a request, since reaching it would
// mean querying zone-b's store.
type hubZonesFixture struct {
	k8sA, k8sB, netappA, netappB *promqlfake.Querier
	router                       *promql.Router
}

func newHubZonesFixture(t *testing.T) hubZonesFixture {
	t.Helper()
	k8sA, k8sB, netappA, netappB := hubZonesStores()
	return newHubZonesFixtureFrom(t, k8sA, k8sB, netappA, netappB)
}

// hubZonesStores is the per-backend content of hubZonesFixture, returned
// separately so a case can add series to one store before the fakes are built.
func hubZonesStores() (k8sA, k8sB, netappA, netappB map[promql.Query]model.Vector) {
	ontapProd := []vlrVol{
		{"trident_pvc_aaaa", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
		{"trident_pvc_bbbb", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 200},
	}
	return hubEstate(nil, []hubClaim{
			{ns: "shop", claim: "orders-data", pv: "pvc-aaaa", pods: []string{"orders-0"}},
		}), hubEstate(nil, []hubClaim{
			{ns: "shop", claim: "orders-data", pv: "pvc-bbbb", pods: []string{"orders-0"}, az: "zone-b"},
		}), vlrHarvest(ontapProd), vlrHarvest([]vlrVol{
			{"trident_pvc_cccc", "ontap-lab", "ontap-lab-01", "aggr1", "svm_lab", 5},
		})
}

func newHubZonesFixtureFrom(t *testing.T, k8sA, k8sB, netappA, netappB map[promql.Query]model.Vector) hubZonesFixture {
	t.Helper()
	f := hubZonesFixture{
		k8sA:    promqlfake.New(k8sA),
		k8sB:    promqlfake.New(k8sB),
		netappA: promqlfake.New(netappA),
		netappB: promqlfake.New(netappB),
	}
	k8s := []promql.Family{promql.FamilyKSM, promql.FamilyKubelet, promql.FamilyServiceGraph, promql.FamilyProbe, promql.FamilyAlerts}
	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("k8s-a", "http://vm-a:8428", k8s, []string{"zone-a"}, "", ""),
		promql.NewBackend("k8s-b", "http://vm-b:8428", k8s, []string{"zone-b"}, "", ""),
		promql.NewBackend("netapp-a", "http://netapp-a:8428", []promql.Family{promql.FamilyHarvest}, []string{"zone-a"}, "", ""),
		promql.NewBackend("netapp-b", "http://netapp-b:8428", []promql.Family{promql.FamilyHarvest}, []string{"zone-b"}, "", ""),
	})
	require.NoError(t, err)
	byName := map[string]promql.Querier{"k8s-a": f.k8sA, "k8s-b": f.k8sB, "netapp-a": f.netappA, "netapp-b": f.netappB}
	f.router, err = promql.NewRouter(tbl, nil, func(b promql.Backend) (promql.Querier, error) {
		return byName[b.Name()], nil
	})
	require.NoError(t, err)
	return f
}

// assertOnlyZoneA fails when any query reached a zone-b store, or when a
// Kubernetes or ALERTS query of zone-a's store lacks the request's az / env.
func assertOnlyZoneA(t *testing.T, f hubZonesFixture) {
	t.Helper()
	assert.Empty(t, f.k8sB.Issued(), "no query reaches zone-b's ksm store")
	assert.Empty(t, f.netappB.Issued(), "no query reaches zone-b's harvest store")
	for _, is := range f.k8sA.Issued() {
		assert.Contains(t, is.Query, `az="zone-a",env="prod"`, is.Name)
	}
}

// Spec: "Hub mode reads only the request's zone". The rooted filer serves a
// claim in both zones; a zone-a request draws zone-a's and never asks zone-b.
func TestBuildStorage_HubStaysInTheRequestZone(t *testing.T) {
	f := newHubZonesFixture(t)
	scope := vlrScope(t, []string{"ontap-prod"}, nil, nil, nil, nil)
	g, err := New(f.router, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)

	assertOnlyZoneA(t, f)
	for _, fam := range []promql.Query{promql.QPVCInfo, promql.QPVCBindings, promql.QKubeletVolumeUsedBytes, promql.QAlerts, promql.QPodInfo} {
		assert.NotEmpty(t, f.k8sA.QueriesFor(fam), "%s is read from zone-a's store", fam)
	}
	assert.NotEmpty(t, f.netappA.QueriesFor(promql.QVolumeLabels))

	body := cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	ids := vlrIDs(body)
	assert.True(t, ids["zone-a-prod-c1/shop/orders-data"], "zone-a's claim on the rooted filer is drawn")
	assert.False(t, ids["zone-b-prod-c1/shop/orders-data"], "zone-b's is not: it lives in a store the request does not reach")
	assert.Equal(t, []string{"zone-a-prod-c1"}, body.Clusters)
	require.NotEmpty(t, vlrPaths(body))
}

// The hub changes how claims are found, never which: over two zones its body
// is byte-identical to the pre-change read's — the same request with the
// storage roots taken out of the READ plan.
func TestBuildStorage_HubMatchesThePreChangeReadAcrossZones(t *testing.T) {
	scope := vlrScope(t, []string{"ontap-prod"}, nil, nil, nil, nil)

	hub := newHubZonesFixture(t)
	gHub, err := New(hub.router, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)

	pre := newHubZonesFixture(t)
	plan := storagePlan(scope.Roots)
	plan.volumeClusters = nil
	gPre, err := New(pre.router, Options{}, nil, nil).buildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, plan)
	require.NoError(t, err)

	assertOnlyZoneA(t, hub)
	assertOnlyZoneA(t, pre)
	assert.Equal(t,
		cytoscape.Serialise(gPre, graph.ProjectStorage(gPre, scope)),
		cytoscape.Serialise(gHub, graph.ProjectStorage(gHub, scope)))
}

// ALERTS follows the same rule: zone-b's alert store is never asked, and a
// zone-b alert that sits in zone-a's store is excluded by the az matcher, so
// the rooted aggregate's status folds from the zone-a alert alone.
func TestBuildStorage_HubAlertsStayInTheRequestZone(t *testing.T) {
	k8sA, k8sB, netappA, netappB := hubZonesStores()
	alert := func(name, severity, az string) *model.Sample {
		return planHarvest("alertname", name, "alertstate", graph.AlertStateFiring, "severity", severity,
			"az", az, "env", "prod", "cluster", "ontap-prod", "aggr", "aggr1")
	}
	k8sA[promql.QAlerts] = model.Vector{alert("AggrFillingA", "warning", "zone-a"), alert("Misfiled", "critical", "zone-b")}
	k8sB[promql.QAlerts] = model.Vector{alert("AggrFillingB", "critical", "zone-b")}
	f := newHubZonesFixtureFrom(t, k8sA, k8sB, netappA, netappB)

	scope := vlrScope(t, []string{"ontap-prod"}, nil, nil, nil, nil)
	g, err := New(f.router, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)

	assertOnlyZoneA(t, f)
	aggr, ok := g.NodesByID[graph.NetAppAggrID("ontap-prod", "aggr1")]
	require.True(t, ok)
	assert.Equal(t,
		[]graph.Alert{{Name: "AggrFillingA", State: graph.AlertStateFiring, Severity: "warning"}},
		aggr.Alerts())
	assert.Equal(t, graph.StatusWarning, aggr.Status())
}

// Spec: "A hub-mode storage build keeps the request matchers". Every
// kube-state-metrics, kubelet and ALERTS query carries az / env and namespace
// (or-absent on ALERTS) in hub mode exactly as outside it.
func TestBuildStorage_HubKeepsTheRequestMatchers(t *testing.T) {
	sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Namespace: []string{"shop"}}
	kubernetes := func(name string) bool {
		fam, ok := promql.FamilyOf(promql.Query(name))
		require.True(t, ok, name)
		return fam == promql.FamilyKSM || fam == promql.FamilyKubelet || fam == promql.FamilyAlerts
	}
	for name, roots := range map[string]graph.StorageRoots{
		"hub mode":         vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil).Roots,
		"outside hub mode": vlrScope(t, nil, nil, nil, nil, []string{"shop/orders-0"}).Roots,
	} {
		t.Run(name, func(t *testing.T) {
			q := promqlfake.New(planEstate())
			_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, sel, roots)
			require.NoError(t, err)
			var seen []string
			for _, is := range q.Issued() {
				if !kubernetes(is.Name) {
					continue
				}
				seen = append(seen, is.Name)
				assert.Contains(t, is.Query, `az="zone-a",env="prod"`, is.Name)
				if is.Name == string(promql.QAlerts) {
					assert.Contains(t, is.Query, `namespace=~"shop|"`)
					continue
				}
				if !slices.Contains(promql.NodeScopedQueries, promql.Query(is.Name)) {
					assert.Contains(t, is.Query, `namespace="shop"`, is.Name)
				}
			}
			for _, want := range []promql.Query{promql.QAlerts, promql.QPodInfo, promql.QNodeInfo} {
				assert.Contains(t, seen, string(want), "the assertion must cover %s", want)
			}
		})
	}
}
