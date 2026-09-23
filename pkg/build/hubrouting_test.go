package build

import (
	"context"
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
// every backend is a matcher-applying promqlfake.

// hubZonesFixture is two Kubernetes zones behind two ksm stores and one filer
// behind zone-a's harvest store. zone-b's harvest store holds a different
// filer, so a harvest query that reached it would load rows the request never
// asked for. Both zones run a cluster named c1, with a claim on ontap-prod
// each; zone-b's is the one the pre-change read cannot see.
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

// zoneOnlySource is a QuerierSource WITHOUT the family-zone upgrade: an
// embedder's own source, which a hub-mode build must still work through.
type zoneOnlySource struct{ r *promql.Router }

func (s zoneOnlySource) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	return s.r.Instant(ctx, name, query, ts)
}

func (s zoneOnlySource) QuerierFor(sel promql.Selector) promql.Querier { return s.r.QuerierFor(sel) }

// Spec: "A storage-exclusive root reads claims in every zone", and
// "Hub mode routes Harvest by zone and the Kubernetes families everywhere".
func TestBuildStorage_HubReadsClaimsInEveryZone(t *testing.T) {
	f := newHubZonesFixture(t)
	scope := vlrScope(t, []string{"ontap-prod"}, nil, nil, nil, nil)
	g, err := New(f.router, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)

	assert.Empty(t, f.netappB.Issued(), "harvest is still routed by az: zone-b's filer store is never asked")
	assert.NotEmpty(t, f.netappA.QueriesFor(promql.QVolumeLabels))
	for _, fam := range []promql.Query{promql.QPVCInfo, promql.QPVCBindings, promql.QKubeletVolumeUsedBytes, promql.QAlerts, promql.QPodInfo} {
		a, b := f.k8sA.QueriesFor(fam), f.k8sB.QueriesFor(fam)
		require.NotEmpty(t, a, "%s reaches zone-a's store", fam)
		assert.Equal(t, a, b, "%s: both ksm stores receive byte-identical queries", fam)
		for _, qy := range a {
			assert.NotContains(t, qy, `az=`, "%s carries no az matcher", fam)
			assert.NotContains(t, qy, `env=`, "%s carries no env matcher", fam)
		}
	}

	body := cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	ids := vlrIDs(body)
	assert.True(t, ids["zone-b-prod-c1/uid-zone-b-shop-orders-0"], "the zone-b claim's pod is drawn")
	assert.True(t, ids["zone-b-prod-c1/shop/orders-data"])
	assert.True(t, ids["zone-a-prod-c1/uid--shop-orders-0"], "and zone-a's, on the same filer")
	// Task 6.3: a raw cluster name reused across zones stays two identities.
	assert.Equal(t, []string{"zone-a-prod-c1", "zone-b-prod-c1"}, body.Clusters)
	require.NotEmpty(t, vlrPaths(body))
}

// withZone returns a copy of a store whose every series also carries the
// given az / env pair — a Harvest store stamped the way this estate's
// collectors stamp every family.
func withZone(store map[promql.Query]model.Vector, az, env string) map[promql.Query]model.Vector {
	out := make(map[promql.Query]model.Vector, len(store))
	for q, vec := range store {
		cp := make(model.Vector, 0, len(vec))
		for _, s := range vec {
			m := s.Metric.Clone()
			m["az"], m["env"] = model.LabelValue(az), model.LabelValue(env)
			cp = append(cp, &model.Sample{Metric: m, Value: s.Value, Timestamp: s.Timestamp})
		}
		out[q] = cp
	}
	return out
}

// Task 11.4 / design D11: the relaxed hub read reaches zone-b's alert store,
// which holds an alert naming ontap-prod / aggr1 for zone-b's own
// (identically named) filer. Only the zone-a alert may reach the rooted
// zone-a aggregate, so its status folds from the warning alone.
func TestBuildStorage_HubAlertsAgreeOnZone(t *testing.T) {
	k8sA, k8sB, netappA, netappB := hubZonesStores()
	alert := func(name, severity, az string) *model.Sample {
		return planHarvest("alertname", name, "alertstate", graph.AlertStateFiring, "severity", severity,
			"az", az, "env", "prod", "cluster", "ontap-prod", "aggr", "aggr1")
	}
	k8sA[promql.QAlerts] = model.Vector{alert("AggrFillingA", "warning", "zone-a")}
	k8sB[promql.QAlerts] = model.Vector{alert("AggrFillingB", "critical", "zone-b")}
	f := newHubZonesFixtureFrom(t, k8sA, k8sB, withZone(netappA, "zone-a", "prod"), netappB)

	scope := vlrScope(t, []string{"ontap-prod"}, nil, nil, nil, nil)
	g, err := New(f.router, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)
	require.NotEmpty(t, f.k8sB.QueriesFor(promql.QAlerts), "the relaxed read does reach zone-b's alerts")

	aggr, ok := g.NodesByID[graph.NetAppAggrID("ontap-prod", "aggr1")]
	require.True(t, ok)
	assert.Equal(t,
		[]graph.Alert{{Name: "AggrFillingA", State: graph.AlertStateFiring, Severity: "warning"}},
		aggr.Alerts(), "the zone-b alert names another zone's filer and stays off")
	assert.Equal(t, graph.StatusWarning, aggr.Status())
}

// Task 8.2: the pre-change read — the same request with the storage roots
// taken out of the READ plan, so it reads the whole filer and every claim
// family under az / env through QuerierFor — never sees zone-b's claim.
func TestBuildStorage_PreChangeReadMissesTheOtherZone(t *testing.T) {
	f := newHubZonesFixture(t)
	scope := vlrScope(t, []string{"ontap-prod"}, nil, nil, nil, nil)
	plan := storagePlan(scope.Roots)
	plan.volumeClusters = nil
	g, err := New(f.router, Options{}, nil, nil).buildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, plan)
	require.NoError(t, err)

	assert.Empty(t, f.k8sB.Issued(), "az routes every ksm query to zone-a's store")
	ids := vlrIDs(cytoscape.Serialise(g, graph.ProjectStorage(g, scope)))
	assert.False(t, ids["zone-b-prod-c1/shop/orders-data"], "the pre-change read draws no path through zone-b")
	assert.True(t, ids["zone-a-prod-c1/shop/orders-data"])
}

// Task 6.2: the three querier sources a hub-mode build binds through.
func TestBuildStorage_HubQuerierSources(t *testing.T) {
	scope := vlrScope(t, []string{"ontap-prod"}, nil, nil, nil, nil)

	t.Run("a Router: harvest by zone, the rest everywhere", func(t *testing.T) {
		f := newHubZonesFixture(t)
		b := New(f.router, Options{}, nil, nil)
		_, ok := b.hubQuerierFor(vlrSel).(interface {
			QuerierFor(promql.Selector) promql.Querier
		})
		assert.False(t, ok, "the bound querier is a dispatcher, not a source")
		_, err := b.BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
		require.NoError(t, err)
		assert.NotEmpty(t, f.k8sB.QueriesFor(promql.QPVCInfo))
		assert.Empty(t, f.netappB.Issued())
	})

	t.Run("a QuerierSource without the upgrade: zone routing everywhere, matchers still relaxed", func(t *testing.T) {
		f := newHubZonesFixture(t)
		g, err := New(zoneOnlySource{f.router}, Options{}, nil, nil).
			BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
		require.NoError(t, err)
		assert.Empty(t, f.k8sB.Issued(), "without the upgrade az routes the ksm family too")
		for _, qy := range f.k8sA.QueriesFor(promql.QPVCInfo) {
			assert.NotContains(t, qy, `az=`)
		}
		ids := vlrIDs(cytoscape.Serialise(g, graph.ProjectStorage(g, scope)))
		assert.True(t, ids["zone-a-prod-c1/shop/orders-data"], "the body is correct for the zone it reached")
		assert.False(t, ids["zone-b-prod-c1/shop/orders-data"], "cross-zone discovery is what the upgrade buys")
	})

	t.Run("a plain Querier is used as is", func(t *testing.T) {
		q := promqlfake.New(nil)
		b := New(q, Options{}, nil, nil)
		assert.Same(t, promql.Querier(q), b.hubQuerierFor(vlrSel))
	})
}

// Task 6.1 — spec: "A hub-mode storage build renders no zone or environment
// matcher". Every kube-state-metrics, kubelet and ALERTS query of a hub build
// keeps namespace (or-absent on ALERTS) and drops az / env; outside hub mode
// the same families carry all three.
func TestBuildStorage_HubRelaxesTheRequestMatchers(t *testing.T) {
	sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Namespace: []string{"shop"}}
	issued := func(t *testing.T, roots graph.StorageRoots) *promqlfake.Querier {
		t.Helper()
		q := promqlfake.New(planEstate())
		_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, sel, roots)
		require.NoError(t, err)
		return q
	}
	kubernetes := func(name string) bool {
		fam, ok := promql.FamilyOf(promql.Query(name))
		require.True(t, ok, name)
		return fam == promql.FamilyKSM || fam == promql.FamilyKubelet || fam == promql.FamilyAlerts
	}

	t.Run("hub mode", func(t *testing.T) {
		q := issued(t, vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil).Roots)
		var seen []string
		for _, is := range q.Issued() {
			if !kubernetes(is.Name) {
				continue
			}
			seen = append(seen, is.Name)
			assert.NotContains(t, is.Query, `az="zone-a"`, is.Name)
			assert.NotContains(t, is.Query, `env="prod"`, is.Name)
			if is.Name == string(promql.QAlerts) {
				assert.Contains(t, is.Query, `namespace=~"shop|"`)
				continue
			}
			if !slices.Contains(promql.NodeScopedQueries, promql.Query(is.Name)) {
				assert.Contains(t, is.Query, `namespace="shop"`, is.Name)
			}
		}
		for _, want := range []promql.Query{promql.QPVCInfo, promql.QPVCBindings, promql.QKubeletVolumeUsedBytes,
			promql.QAlerts, promql.QPodInfo, promql.QNodeInfo, promql.QStatefulSetAnnotations} {
			assert.Contains(t, seen, string(want), "the assertion must cover %s", want)
		}
	})

	t.Run("outside hub mode", func(t *testing.T) {
		q := issued(t, vlrScope(t, nil, nil, nil, nil, []string{"shop/orders-0"}).Roots)
		for _, is := range q.Issued() {
			if !kubernetes(is.Name) {
				continue
			}
			assert.Contains(t, is.Query, `az="zone-a",env="prod"`, is.Name)
		}
	})
}
