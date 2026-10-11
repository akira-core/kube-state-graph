package build

import (
	"encoding/json"
	"maps"
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

// Spec: "A multi-zone body is the union of its zones" (accept-multi-zone-storage-graph
// D5). One mocked estate holds two zones that both run a cluster named c1, both
// serve an aggregate `aggr1` and a FlexVol `trident_pvc_orders`, and zone-b
// recreated its `shop/db-0` on another node. The same request is built for
// zone-a, for zone-b and for both, and the third body must be the union of the
// first two — every element, edge weights included.

var harvestFamilies = []promql.Query{
	promql.QVolumeLabels, promql.QQoSReadOps, promql.QQoSWriteOps, promql.QQoSReadLatency,
	promql.QQoSWriteLatency, promql.QQoSReadData, promql.QQoSWriteData,
	promql.QQoSPolicyFixedMaxIOPS, promql.QQoSPolicyFixedMaxMBps,
	promql.QAggrStatus, promql.QAggrSpaceUsed, promql.QAggrSpaceTotal,
	promql.QNetAppNodeStatus, promql.QNetAppNodeLabels, promql.QNetAppNodeCPUBusy,
	promql.QNetAppNodeTotalOps, promql.QNetAppNodeTotalLatency, promql.QNetAppNodeTotalData,
}

// restampHarvest sets the az label of every Harvest series of fx.
func restampHarvest(fx map[promql.Query]model.Vector, az string) {
	for _, q := range harvestFamilies {
		for _, s := range fx[q] {
			s.Metric["az"] = model.LabelValue(az)
		}
	}
}

// appendFixtures concatenates, per family, the series of every fixture.
func appendFixtures(fixtures ...map[promql.Query]model.Vector) map[promql.Query]model.Vector {
	out := map[promql.Query]model.Vector{}
	for _, fx := range fixtures {
		for q, vec := range fx {
			out[q] = append(out[q], vec...)
		}
	}
	return out
}

// twoZoneEstate is the mocked estate.
//
//	zone-a  filer ontap-a: aggr1 / svm_shop   claims orders-data (orders-0), db-data (db-0)
//	zone-b  filer ontap-b: aggr1 / svm_shop   claims orders-data (orders-0), db-data (db-0)
//
// Both zones name their claims' PersistentVolumes identically, so both filers
// hold FlexVols `trident_pvc_orders` / `trident_pvc_db`: each claim must join
// its OWN zone's volume. In zone-b, shop/db-0 first ran on worker-1 and was
// recreated on worker-2; in zone-a it stays on worker-1.
func twoZoneEstate() map[promql.Query]model.Vector {
	claims := func(az string) []hubClaim {
		return []hubClaim{
			{ns: "shop", claim: "orders-data", pv: "pvc-orders", pods: []string{"orders-0"}, az: az},
			{ns: "shop", claim: "db-data", pv: "pvc-db", pods: []string{"db-0"}, az: az},
		}
	}
	zoneA := hubEstate([]vlrVol{
		{"trident_pvc_orders", "ontap-a", "ontap-a-01", "aggr1", "svm_shop", 100},
		{"trident_pvc_db", "ontap-a", "ontap-a-01", "aggr1", "svm_shop", 40},
	}, claims("zone-a"))
	restampHarvest(zoneA, "zone-a")

	zoneB := hubEstate([]vlrVol{
		{"trident_pvc_orders", "ontap-b", "ontap-b-01", "aggr1", "svm_shop", 900},
		{"trident_pvc_db", "ontap-b", "ontap-b-01", "aggr1", "svm_shop", 70},
	}, claims("zone-b"))
	restampHarvest(zoneB, "zone-b")

	// zone-b's db-0 was recreated on worker-2: a newer incarnation, and the node.
	newest := planKSM("az", "zone-b", "namespace", "shop", "pod", "db-0", "uid", "uid-zone-b-shop-db-0-new", "node", "worker-2")
	newest.Timestamp = 5_000
	zoneB[promql.QPodInfo] = append(zoneB[promql.QPodInfo], newest)
	zoneB[promql.QNodeInfo] = append(zoneB[promql.QNodeInfo], planKSM("az", "zone-b", "node", "worker-2"))

	return appendFixtures(zoneA, zoneB)
}

// elementsByID keys every element of a body by its id, as the JSON it
// serialises to, so a comparison covers every attribute — flow weights included.
func elementsByID(t *testing.T, body cytoscape.Body) (nodes, edges map[string]string) {
	t.Helper()
	nodes, edges = map[string]string{}, map[string]string{}
	for _, n := range body.Elements.Nodes {
		raw, err := json.Marshal(n)
		require.NoError(t, err)
		nodes[n.Data.ID] = string(raw)
	}
	for _, e := range body.Elements.Edges {
		raw, err := json.Marshal(e)
		require.NoError(t, err)
		edges[e.Data.ID] = string(raw)
	}
	return nodes, edges
}

func zoneSel(az ...string) promql.Selector {
	return promql.Selector{AZ: az, Env: []string{"prod"}}
}

func buildBody(t *testing.T, fx map[promql.Query]model.Vector, sel promql.Selector, scope graph.StorageScope) cytoscape.Body {
	t.Helper()
	q := promqlfake.New(fx)
	g, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, sel, scope.Roots)
	require.NoError(t, err)
	return cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
}

func TestBuildStorage_MultiZoneBodyIsTheUnionOfItsZones(t *testing.T) {
	roots := map[string]graph.StorageScope{
		"aggr":          vlrScope(t, graph.StorageRootAggr, []string{"aggr1"}),
		"svm":           vlrScope(t, graph.StorageRootSVM, []string{"svm_shop"}),
		"ontap_cluster": vlrScope(t, graph.StorageRootONTAPCluster, []string{"ontap-a", "ontap-b"}),
		"node":          vlrScope(t, graph.StorageRootNode, []string{"worker-1"}),
		"pod":           vlrScope(t, graph.StorageRootPod, []string{"shop/db-0", "shop/orders-0"}),
	}
	for name, scope := range roots {
		t.Run(name, func(t *testing.T) {
			fx := twoZoneEstate()
			a := buildBody(t, fx, zoneSel("zone-a"), scope)
			b := buildBody(t, fx, zoneSel("zone-b"), scope)
			both := buildBody(t, fx, zoneSel("zone-a", "zone-b"), scope)

			aNodes, aEdges := elementsByID(t, a)
			bNodes, bEdges := elementsByID(t, b)
			gotNodes, gotEdges := elementsByID(t, both)
			require.NotEmpty(t, aNodes, "a vacuous zone-a body would prove nothing")
			require.NotEmpty(t, bNodes, "a vacuous zone-b body would prove nothing")

			wantNodes, wantEdges := maps.Clone(aNodes), maps.Clone(aEdges)
			maps.Copy(wantNodes, bNodes)
			maps.Copy(wantEdges, bEdges)
			assert.Equal(t, wantNodes, gotNodes, "node set and every node attribute equal the union")
			assert.Equal(t, wantEdges, gotEdges, "edge set and every edge weight equal the union")

			wantClusters := slices.Compact(slices.Sorted(slices.Values(slices.Concat(a.Clusters, b.Clusters))))
			assert.Equal(t, wantClusters, both.Clusters)
			assert.Len(t, both.Clusters, len(a.Clusters)+len(b.Clusters),
				"the two c1 clusters stay two identities")
		})
	}
}

// The zones the fixture pits against each other are really different: each
// claim lands on its own zone's filer, with its own zone's measurement.
func TestBuildStorage_MultiZoneEstateJoinsEachClaimWithinItsZone(t *testing.T) {
	fx := twoZoneEstate()
	scope := vlrScope(t, graph.StorageRootAggr, []string{"aggr1"})
	body := buildBody(t, fx, zoneSel("zone-a", "zone-b"), scope)
	ids := vlrIDs(body)

	assert.True(t, ids["netapp/ontap-a/aggr/aggr1"])
	assert.True(t, ids["netapp/ontap-b/aggr/aggr1"], "the same aggregate name on the other zone's filer is a distinct node")
	assert.True(t, ids["zone-a-prod-c1/shop/orders-data"])
	assert.True(t, ids["zone-b-prod-c1/shop/orders-data"], "the same cluster and claim name in the other zone is a distinct node")

	reads := map[string]float64{}
	for _, e := range body.Elements.Edges {
		if e.Data.Labels["tier"] == graph.StorageTierSVMPVC && e.Data.Metrics != nil && e.Data.Metrics.ReadOps != nil {
			reads[e.Data.Target] = *e.Data.Metrics.ReadOps
		}
	}
	assert.InDelta(t, 100.0, reads["zone-a-prod-c1/shop/orders-data"], 1e-9, "zone-a's claim carries zone-a's measurement")
	assert.InDelta(t, 900.0, reads["zone-b-prod-c1/shop/orders-data"], 1e-9, "zone-b's carries zone-b's, never a sum")
}

// Spec: "A cluster name reused across selected zones is two clusters".
func TestBuildStorage_MultiZoneNodeRootElectsEachZonesPodIndependently(t *testing.T) {
	fx := twoZoneEstate()
	scope := vlrScope(t, graph.StorageRootNode, []string{"worker-1"})
	ids := vlrIDs(buildBody(t, fx, zoneSel("zone-a", "zone-b"), scope))

	assert.True(t, ids["zone-a-prod-c1/shop/db-data"], "zone-a's db-0 stays on worker-1 and is tracked from it")
	assert.False(t, ids["zone-b-prod-c1/shop/db-data"], "zone-b's db-0 moved to worker-2, so it is not")
	assert.True(t, ids["zone-b-prod-c1/shop/orders-data"], "zone-b's orders-0 is on worker-1")
}
