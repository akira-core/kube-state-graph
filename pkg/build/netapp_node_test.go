package build

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// nodeLabelSample is a Harvest node_labels info series. Its value is
// deliberately a constant: the resolver must never read it, exactly as for
// volume_labels.
func nodeLabelSample(oc, node string, kv map[string]string) model.Sample {
	m := model.Metric{
		"cluster": model.LabelValue(oc),
		"node":    model.LabelValue(node),
	}
	for k, v := range kv {
		m[model.LabelName(k)] = model.LabelValue(v)
	}
	return model.Sample{Metric: m, Value: 1}
}

// perfSample is one system_node counter series.
func perfSample(oc, node string, val float64) model.Sample {
	return model.Sample{
		Metric: model.Metric{
			"cluster": model.LabelValue(oc),
			"node":    model.LabelValue(node),
		},
		Value: model.SampleValue(val),
	}
}

// joinedVol is the hop-A fixture every case here shares: one claim landing on
// controller n1 of ONTAP cluster oc, which is what materialises the
// netapp-node the attributes are stamped on.
func joinedVol() model.Vector {
	return sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm0"))
}

// --- hardware -------------------------------------------------------------

// Every field is read verbatim from its like-named label and an EMPTY label is
// omitted rather than serialised as an empty string — the spec's "Hardware
// attribute resolved" scenario, whose `location=""` is the load-bearing half.
func TestResolveNetAppStorage_HardwareResolved(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol:    joinedVol(),
		nodeLabels: sampleVec(nodeLabelSample("oc", "n1", map[string]string{
			"model":    "AFF-A400",
			"serial":   "721234000123",
			"version":  "9.14.1",
			"vendor":   "NetApp",
			"location": "",
		})),
	}.run()

	require.Len(t, res.nodes, 1)
	hw := res.nodes[0].Hardware()
	require.NotNil(t, hw)
	assert.Equal(t, graph.Hardware{
		Model:   "AFF-A400",
		Serial:  "721234000123",
		Version: "9.14.1",
		Vendor:  "NetApp",
	}, *hw, "an empty location label resolves no field")

	assert.Equal(t, map[string]string{"ontap_cluster": "oc"}, res.nodes[0].Labels(),
		"hardware is a typed attribute and must never leak into labels")
}

// A controller no node_labels series matched carries no hardware object at all
// — the attribute is omitted, not emitted with absent keys.
func TestResolveNetAppStorage_HardwareAbsent(t *testing.T) {
	res := netappFixture{claims: claim1(), vol: joinedVol()}.run()
	require.Len(t, res.nodes, 1)
	assert.Nil(t, res.nodes[0].Hardware())

	// A series matching a DIFFERENT controller must not bleed across.
	other := netappFixture{
		claims:     claim1(),
		vol:        joinedVol(),
		nodeLabels: sampleVec(nodeLabelSample("oc", "n2", map[string]string{"model": "FAS2720"})),
	}.run()
	require.Len(t, other.nodes, 1)
	assert.Nil(t, other.nodes[0].Hardware(), "node_labels is joined on (cluster, node)")
}

// A series carrying only SOME fields must not blank out a sibling that carried
// the rest, and the per-field pick is the lexically-smallest non-empty value so
// a duplicate series (a poller restart inside the window) is order-free.
func TestResolveNetAppStorage_HardwareDuplicateSeriesIsOrderFree(t *testing.T) {
	a := nodeLabelSample("oc", "n1", map[string]string{"model": "AFF-A400", "version": "9.14.1"})
	b := nodeLabelSample("oc", "n1", map[string]string{"model": "AFF-A220", "vendor": "NetApp"})

	fwd := netappFixture{claims: claim1(), vol: joinedVol(), nodeLabels: sampleVec(a, b)}.run()
	rev := netappFixture{claims: claim1(), vol: joinedVol(), nodeLabels: sampleVec(b, a)}.run()

	require.Len(t, fwd.nodes, 1)
	require.Len(t, rev.nodes, 1)
	assert.Equal(t, fwd.nodes[0].Hardware(), rev.nodes[0].Hardware(),
		"upstream vector order must not reach the output")
	assert.Equal(t, graph.Hardware{
		Model:   "AFF-A220", // lexically smallest of the two
		Version: "9.14.1",   // only the first series carried it
		Vendor:  "NetApp",   // only the second series carried it
	}, *fwd.nodes[0].Hardware())
}

// --- performance ----------------------------------------------------------

// The four counters are read VERBATIM — no rate(), no unit conversion — and
// none of them touches the reported health.
func TestResolveNetAppStorage_PerfResolvedVerbatim(t *testing.T) {
	res := netappFixture{
		claims:       claim1(),
		vol:          joinedVol(),
		nodeStatus:   sampleVec(perfSample("oc", "n1", 1)),
		cpuBusy:      sampleVec(perfSample("oc", "n1", 72.5)),
		totalOps:     sampleVec(perfSample("oc", "n1", 18500)),
		totalLatency: sampleVec(perfSample("oc", "n1", 830)),
		totalData:    sampleVec(perfSample("oc", "n1", 1.2e9)),
	}.run()

	require.Len(t, res.nodes, 1)
	p := res.nodes[0].Perf()
	require.NotNil(t, p)
	require.NotNil(t, p.CPUBusyPct)
	require.NotNil(t, p.TotalOps)
	require.NotNil(t, p.TotalLatencyUs)
	require.NotNil(t, p.TotalBytesPerSec)
	assert.InDelta(t, 72.5, *p.CPUBusyPct, 1e-12)
	assert.InDelta(t, 18500.0, *p.TotalOps, 1e-12)
	assert.InDelta(t, 830.0, *p.TotalLatencyUs, 1e-12)
	assert.InDelta(t, 1.2e9, *p.TotalBytesPerSec, 1e-3)

	assert.Equal(t, graph.HealthOnline, res.nodes[0].Health())
	assert.Equal(t, map[string]string{"ontap_cluster": "oc"}, res.nodes[0].Labels(),
		"perf is a typed attribute and must never leak into labels")
}

// Each counter is independently optional: a leg that matched nothing leaves its
// field nil rather than 0. A controller reporting zero ops and a controller
// whose ops counter was never read are different facts.
func TestResolveNetAppStorage_PartialPerfCounters(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     joinedVol(),
		cpuBusy: sampleVec(perfSample("oc", "n1", 41)),
	}.run()

	require.Len(t, res.nodes, 1)
	p := res.nodes[0].Perf()
	require.NotNil(t, p)
	require.NotNil(t, p.CPUBusyPct)
	assert.InDelta(t, 41.0, *p.CPUBusyPct, 1e-12)
	assert.Nil(t, p.TotalOps)
	assert.Nil(t, p.TotalLatencyUs)
	assert.Nil(t, p.TotalBytesPerSec)
}

// A counter genuinely reporting 0 resolves the field — the absent-vs-zero
// distinction the pointer exists for.
func TestResolveNetAppStorage_ZeroCounterResolves(t *testing.T) {
	res := netappFixture{
		claims:  claim1(),
		vol:     joinedVol(),
		cpuBusy: sampleVec(perfSample("oc", "n1", 0)),
	}.run()

	require.Len(t, res.nodes, 1)
	require.NotNil(t, res.nodes[0].Perf())
	require.NotNil(t, res.nodes[0].Perf().CPUBusyPct)
	assert.Zero(t, *res.nodes[0].Perf().CPUBusyPct)
}

// A controller no counter matched carries no perf object.
func TestResolveNetAppStorage_PerfAbsent(t *testing.T) {
	res := netappFixture{claims: claim1(), vol: joinedVol()}.run()
	require.Len(t, res.nodes, 1)
	assert.Nil(t, res.nodes[0].Perf())
}

// The spec's "High CPU does not degrade health" scenario. Thresholds are
// model-specific and belong in the operator's alert rules; data.health stays
// the ONTAP-reported node_new_status and nothing else.
func TestResolveNetAppStorage_HighCPUDoesNotDegradeHealth(t *testing.T) {
	res := netappFixture{
		claims:     claim1(),
		vol:        joinedVol(),
		nodeStatus: sampleVec(perfSample("oc", "n1", 1)),
		cpuBusy:    sampleVec(perfSample("oc", "n1", 99)),
	}.run()

	require.Len(t, res.nodes, 1)
	assert.Equal(t, graph.HealthOnline, res.nodes[0].Health(),
		"a saturated controller ONTAP reports as healthy stays online")
}

// A duplicate counter series resolves to the smallest value, order-free —
// the rule usageByAggr already applies to the aggregate space gauges.
func TestResolveNetAppStorage_PerfDuplicateSeriesIsOrderFree(t *testing.T) {
	a, b := perfSample("oc", "n1", 90), perfSample("oc", "n1", 10)

	fwd := netappFixture{claims: claim1(), vol: joinedVol(), cpuBusy: sampleVec(a, b)}.run()
	rev := netappFixture{claims: claim1(), vol: joinedVol(), cpuBusy: sampleVec(b, a)}.run()

	require.Len(t, fwd.nodes, 1)
	require.Len(t, rev.nodes, 1)
	assert.InDelta(t, 10.0, *fwd.nodes[0].Perf().CPUBusyPct, 1e-12)
	assert.InDelta(t, 10.0, *rev.nodes[0].Perf().CPUBusyPct, 1e-12)
}

// --- inventory ------------------------------------------------------------

// The inventory is JOIN-INDEPENDENT: it names entities no claim reached, which
// is what lets the storage-flow graph draw a flowless root. The join-only
// NetAppAggrs / NetAppNodes lists stay narrow so GET /v1/graph is unchanged.
func TestCollectNetAppInventory_NamesEntitiesNoClaimReached(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol: sampleVec(
			// The joining claim's volume.
			volLabelSample("pvc-x", "oc", "n1", "a1", "svm0"),
			// A volume no claim matches: its aggregate, controller and SVM are
			// inventoried but never materialised by the join.
			model.Sample{Metric: model.Metric{
				"volume": "unrelated_vol", "cluster": "oc",
				"node": "n2", "aggr": "a2", "svm": "svm_other",
			}, Value: 1},
		),
		// An aggregate whose only trace is its status gauge — no volume on it.
		aggrStatus: sampleVec(aggrSample("oc", "n3", "a9", 1)),
		// A controller whose only trace is a counter.
		cpuBusy: sampleVec(perfSample("oc", "n4", 12)),
	}.run()

	inv := res.inventory
	assert.Equal(t, []NetAppInventoryNode{
		{ONTAPCluster: "oc", Node: "n1"},
		{ONTAPCluster: "oc", Node: "n2"},
		{ONTAPCluster: "oc", Node: "n4"},
	}, inv.Nodes, "a controller named by any leg is inventoried; n3 is only an aggr label, not a node one")
	assert.Equal(t, []NetAppInventoryAggr{
		{ONTAPCluster: "oc", Aggr: "a1", Owner: "n1"},
		{ONTAPCluster: "oc", Aggr: "a2", Owner: "n2"},
		{ONTAPCluster: "oc", Aggr: "a9"},
	}, inv.Aggrs, "a9 is named only by the status gauge, so it has no volume-derived owner")
	assert.Equal(t, []NetAppInventorySVM{
		{ONTAPCluster: "oc", SVM: "svm0"},
		{ONTAPCluster: "oc", SVM: "svm_other"},
	}, inv.SVMs)

	// The join-only lists stay narrow — this is what keeps /v1/graph unchanged.
	require.Len(t, res.aggrs, 1)
	assert.Equal(t, graph.NetAppAggrID("oc", "a1"), res.aggrs[0].ID())
	require.Len(t, res.nodes, 1)
	assert.Equal(t, graph.NetAppNodeID("oc", "n1"), res.nodes[0].ID())
}

// The inventory is a pure function of the vectors: shuffling every input leaves
// it byte-identical, since each set is accumulated insert-only and then sorted.
func TestCollectNetAppInventory_OrderFree(t *testing.T) {
	v1 := volLabelSample("pvc-x", "oc-b", "n2", "a2", "svm_b")
	v2 := volLabelSample("pvc-y", "oc-a", "n1", "a1", "svm_a")
	s1, s2 := aggrSample("oc-b", "n2", "a7", 1), aggrSample("oc-a", "n1", "a8", 1)
	c1, c2 := perfSample("oc-b", "n9", 1), perfSample("oc-a", "n8", 1)

	fwd := netappFixture{
		claims:     []pvcVolume{{id: "c/db/x", volumeName: "pvc-x"}, {id: "c/db/y", volumeName: "pvc-y"}},
		vol:        sampleVec(v1, v2),
		aggrStatus: sampleVec(s1, s2),
		totalOps:   sampleVec(c1, c2),
	}.run()
	rev := netappFixture{
		claims:     []pvcVolume{{id: "c/db/y", volumeName: "pvc-y"}, {id: "c/db/x", volumeName: "pvc-x"}},
		vol:        sampleVec(v2, v1),
		aggrStatus: sampleVec(s2, s1),
		totalOps:   sampleVec(c2, c1),
	}.run()

	assert.Equal(t, fwd.inventory, rev.inventory)
	// And it is genuinely sorted by (ontap cluster, name), not merely stable.
	assert.Equal(t, []NetAppInventorySVM{
		{ONTAPCluster: "oc-a", SVM: "svm_a"},
		{ONTAPCluster: "oc-b", SVM: "svm_b"},
	}, fwd.inventory.SVMs)
}

// A non-NetApp deployment reads no Harvest series at all, so the inventory is
// empty rather than a set of zero-valued entries.
func TestCollectNetAppInventory_EmptyWithoutHarvest(t *testing.T) {
	res := netappFixture{claims: claim1()}.run()
	assert.True(t, res.inventory.Empty())
	assert.Empty(t, res.inventory.Nodes)
	assert.Empty(t, res.inventory.Aggrs)
	assert.Empty(t, res.inventory.SVMs)
}

// A series missing either half of the (cluster, node) / (cluster, aggr) key is
// skipped, never inventoried under an empty name.
func TestCollectNetAppInventory_SkipsIncompleteKeys(t *testing.T) {
	res := netappFixture{
		claims: claim1(),
		vol:    joinedVol(),
		cpuBusy: sampleVec(
			model.Sample{Metric: model.Metric{"cluster": "oc"}, Value: 1},
			model.Sample{Metric: model.Metric{"node": "orphan"}, Value: 1},
		),
		aggrStatus: sampleVec(model.Sample{Metric: model.Metric{"cluster": "oc"}, Value: 1}),
	}.run()

	assert.Equal(t, []NetAppInventoryNode{{ONTAPCluster: "oc", Node: "n1"}}, res.inventory.Nodes)
	assert.Equal(t, []NetAppInventoryAggr{{ONTAPCluster: "oc", Aggr: "a1", Owner: "n1"}}, res.inventory.Aggrs)
}

// --- Topology plumbing ----------------------------------------------------

// Populating the inventory must not disturb what assemble / GET /v1/graph
// sees: the join-only node lists, the storage edges and the PVC svm label are
// all unchanged, and SVMByPVC agrees with the label it mirrors.
func TestParseTopology_InventoryDoesNotDisturbTheJoin(t *testing.T) {
	v := topologyVectors{
		// A PVC node is materialised from the BINDING series; PVCInfo only
		// enriches it with the storageclass and the bound PV name. Without a
		// mounting pod there is no claim for the storage join to reach.
		PVC: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "pod": "mongo",
			"claim_name": "data", "volume": "data",
		}}),
		Pod: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "pod": "mongo", "uid": "u1", "node": "w0",
		}}),
		PVCInfo: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data",
			"volumename": "pvc-9f3a", "storageclass": "trident",
		}, Value: 1}),
		VolumeLabels: sampleVec(
			model.Sample{Metric: model.Metric{
				"volume": "trident_pvc_9f3a", "cluster": "oc",
				"node": "n1", "aggr": "a1", "svm": "svm_prod",
			}, Value: 1},
			// A flowless entity: inventoried, never joined.
			model.Sample{Metric: model.Metric{
				"volume": "unrelated", "cluster": "oc",
				"node": "n2", "aggr": "a2", "svm": "svm_other",
			}, Value: 1},
		),
		NetAppNodeLabels:  sampleVec(nodeLabelSample("oc", "n1", map[string]string{"model": "AFF-A400"})),
		NetAppNodeCPUBusy: sampleVec(perfSample("oc", "n1", 55)),
	}
	tp := parseTopology(v, promql.LabelKeys{})

	// Join-only output is unchanged by the inventory.
	require.Len(t, tp.NetAppAggrs, 1)
	assert.Equal(t, graph.NetAppAggrID("oc", "a1"), tp.NetAppAggrs[0].ID())
	require.Len(t, tp.NetAppNodes, 1)
	assert.Equal(t, graph.NetAppNodeID("oc", "n1"), tp.NetAppNodes[0].ID())
	require.Len(t, tp.StorageEdges, 1)

	// The attributes reached the materialised controller.
	require.NotNil(t, tp.NetAppNodes[0].Hardware())
	assert.Equal(t, "AFF-A400", tp.NetAppNodes[0].Hardware().Model)
	require.NotNil(t, tp.NetAppNodes[0].Perf())
	assert.InDelta(t, 55.0, *tp.NetAppNodes[0].Perf().CPUBusyPct, 1e-12)

	// The inventory is wider than the join.
	assert.Len(t, tp.NetAppInventory.Aggrs, 2)
	assert.Len(t, tp.NetAppInventory.SVMs, 2)

	// SVMByPVC mirrors the label rather than replacing it.
	require.Len(t, tp.PVCs, 1)
	assert.Equal(t, "svm_prod", tp.PVCs[0].Labels()["svm"])
	assert.Equal(t, map[string]string{tp.PVCs[0].ID(): "svm_prod"}, tp.SVMByPVC)
}

// Each of the five new legs is OPTIONAL: a query error degrades log-and-continue
// and costs at most one attribute, never the build and never the storage
// topology.
func TestReadTopology_ControllerLegFailureDoesNotFailBuild(t *testing.T) {
	for _, name := range []promql.Query{
		promql.QNetAppNodeLabels,
		promql.QNetAppNodeCPUBusy,
		promql.QNetAppNodeTotalOps,
		promql.QNetAppNodeTotalLatency,
		promql.QNetAppNodeTotalData,
	} {
		t.Run(string(name), func(t *testing.T) {
			legs := failingLegs(nil, errors.New("no such metric"), name)
			// A claim that joins, so the failure is proven not to cost the
			// storage topology rather than merely not to fail an empty build.
			// The binding + pod are what materialise the PVC node the join
			// runs over; PVCInfo alone only carries the bound PV name.
			legs[promql.QPVCBindings] = legFixture{sampleVec(model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "pod": "mongo",
				"claim_name": "data", "volume": "data",
			}}), nil}
			legs[promql.QPodInfo] = legFixture{sampleVec(model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "pod": "mongo", "uid": "u1", "node": "w0",
			}}), nil}
			legs[promql.QPVCInfo] = legFixture{sampleVec(model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data",
				"volumename": "pvc-9f3a",
			}, Value: 1}), nil}
			legs[promql.QVolumeLabels] = legFixture{sampleVec(model.Sample{Metric: model.Metric{
				"volume": "trident_pvc_9f3a", "cluster": "oc",
				"node": "n1", "aggr": "a1", "svm": "svm0",
			}, Value: 1}), nil}

			tp, err := readTopologyDefaults(context.Background(), legQuerier(t, legs))
			require.NoError(t, err, "%s is optional and must not fail the build", name)
			require.Len(t, tp.NetAppNodes, 1, "%s failing must not cost the storage topology", name)
			require.Contains(t, tp.RawSeriesCount, string(name),
				"%s must still be counted after degrading", name)
			assert.Zero(t, tp.RawSeriesCount[string(name)])
		})
	}
}

// The five legs carry NO request matcher — Harvest is zone-ROUTED, and its
// `cluster` label is the ONTAP cluster, not a Kubernetes one.
func TestReadTopology_ControllerLegsCarryNoRequestMatcher(t *testing.T) {
	sel := promql.Selector{
		AZ: []string{"zone-a"}, Env: []string{"prod"},
		Cluster: []string{"c1"}, Namespace: []string{"shop"},
	}
	for _, name := range []promql.Query{
		promql.QNetAppNodeLabels,
		promql.QNetAppNodeCPUBusy,
		promql.QNetAppNodeTotalOps,
		promql.QNetAppNodeTotalLatency,
		promql.QNetAppNodeTotalData,
	} {
		got := promql.Render(name, time.Minute, promql.LabelKeys{}, sel)
		assert.Equalf(t, "last_over_time("+string(name)+"[1m])", got,
			"%s must render exactly as an unfiltered build renders it", name)
	}
}
