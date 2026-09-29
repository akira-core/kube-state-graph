package build

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// accept-multi-zone-storage-graph: a /v1/storage-graph request may select
// several zones and environments. The selector reaches upstream exactly as it
// does on /v1/graph — one anchored alternation on every family that carries the
// dimension, and every zone-routed family dispatched to the backends serving
// ANY of the selected zones.

// multiZoneFixture is hubZonesFixture whose zone-b Harvest store also holds the
// FlexVol of zone-b's claim, stamped with zone-b: a claim joins only FlexVols of
// its own zone, so without it the zone-b claim would (correctly) draw no path.
func multiZoneFixture(t *testing.T) hubZonesFixture {
	t.Helper()
	k8sA, k8sB, netappA, netappB := hubZonesStores()
	netappB[promql.QVolumeLabels] = append(netappB[promql.QVolumeLabels],
		planHarvest("az", "zone-b", "cluster", "ontap-lab", "node", "ontap-lab-01", "aggr", "aggr1", "svm", "svm_lab", "volume", "trident_pvc_bbbb"))
	return newHubZonesFixtureFrom(t, k8sA, k8sB, netappA, netappB)
}

// stores is every backend of the fixture, keyed by its table name.
func (f hubZonesFixture) stores() map[string]*promqlfake.Querier {
	return map[string]*promqlfake.Querier{
		"k8s-a": f.k8sA, "k8s-b": f.k8sB, "netapp-a": f.netappA, "netapp-b": f.netappB,
	}
}

// Spec: "Several zones are a Cartesian product". Both zones' stores of every
// routed family are asked, and every query of every one of them carries the
// whole zone set as one alternation.
func TestBuildStorage_MultiZoneSelectorReachesEveryFamilyAndBackend(t *testing.T) {
	f := multiZoneFixture(t)
	scope := vlrScope(t, graph.StorageRootONTAPCluster, []string{"ontap-lab", "ontap-prod"})
	sel := promql.Selector{AZ: []string{"zone-b", "zone-a"}, Env: []string{"prod"}}
	g, err := New(f.router, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, sel, scope.Roots)
	require.NoError(t, err)

	const zones = `az=~"zone-a|zone-b",env="prod"`
	for name, store := range f.stores() {
		issued := store.Issued()
		require.NotEmpty(t, issued, "%s is asked: the request selects its zone", name)
		for _, is := range issued {
			assert.Contains(t, is.Query, zones, "%s / %s carries the zone set", name, is.Name)
		}
	}

	ids := vlrIDs(cytoscape.Serialise(g, graph.ProjectStorage(g, scope)))
	assert.True(t, ids["zone-a-prod-c1/shop/orders-data"], "zone-a's claim is drawn")
	assert.True(t, ids["zone-b-prod-c1/shop/orders-data"], "zone-b's claim is drawn")
	assert.True(t, ids["netapp/ontap-prod/aggr/aggr1"], "zone-a's filer")
	assert.True(t, ids["netapp/ontap-lab/aggr/aggr1"], "zone-b's filer")
}

// The rendered queries are a pure function of the SORTED zone set, so the order
// a client repeats `az` in changes nothing that reaches upstream.
func TestBuildStorage_MultiZoneQueriesIgnoreValueOrder(t *testing.T) {
	scope := vlrScope(t, graph.StorageRootONTAPCluster, []string{"ontap-lab", "ontap-prod"})
	issue := func(az []string) []string {
		f := newHubZonesFixture(t)
		sel := promql.Selector{AZ: az, Env: []string{"prod"}}
		_, err := New(f.router, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, sel, scope.Roots)
		require.NoError(t, err)
		var out []string
		for name, store := range f.stores() {
			for _, is := range store.Issued() {
				out = append(out, name+" "+is.Name+" "+is.Query)
			}
		}
		return sortedCopy(out)
	}
	assert.Equal(t, issue([]string{"zone-a", "zone-b"}), issue([]string{"zone-b", "zone-a"}))
}
