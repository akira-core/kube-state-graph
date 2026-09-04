package build

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// storageFixtures is one joined-and-mounted claim end to end: pod shop/orders-0
// on worker-1 mounting shop/orders-data, whose FlexVol lives on
// (ontap-prod, ontap-prod-01, aggr1, svm_shop).
func storageFixtures() map[promql.Query]model.Vector {
	return map[promql.Query]model.Vector{
		promql.QPodInfo: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c1", "namespace": "shop", "pod": "orders-0",
			"uid": "uid-1", "node": "worker-1",
		}}),
		promql.QNodeInfo: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c1", "node": "worker-1",
		}}),
		promql.QPVCBindings: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c1", "namespace": "shop", "pod": "orders-0",
			"claim_name": "orders-data", "volume": "data",
		}}),
		promql.QPVCInfo: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c1", "namespace": "shop", "persistentvolumeclaim": "orders-data",
			"volumename": "pvc-9f3a", "storageclass": "trident",
		}}),
		promql.QVolumeLabels: sampleVec(model.Sample{Metric: model.Metric{
			"volume": "trident_pvc_9f3a", "cluster": "ontap-prod",
			"node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm_shop",
		}}),
	}
}

func newStorageBuilder(t *testing.T, q promql.Querier) *Builder {
	t.Helper()
	return New(q, Options{}, nil, nil)
}

// The storage build reuses ReadTopology unchanged and skips the service-graph
// read entirely: those three legs are the most expensive of the fan-out and the
// storage body uses none of them. The up{} probe is skipped too — the endpoint
// is always a filtered build.
func TestBuildStorage_IssuesNoServiceGraphOrProbeQuery(t *testing.T) {
	q, seen := newRecordingQuerier(t, storageFixtures())
	sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}

	_, err := newStorageBuilder(t, q).BuildStorage(
		context.Background(), time.Minute, time.Unix(1, 0).UTC(), sel)
	require.NoError(t, err)

	issued := map[string]bool{}
	for _, name := range seen() {
		issued[name] = true
	}
	for _, forbidden := range []promql.Query{
		promql.QServiceGraphTotal,
		promql.QServiceGraphFailedTotal,
		promql.QServiceGraphServerSecondsBucket,
		promql.QUpProbe,
	} {
		assert.Falsef(t, issued[string(forbidden)], "%s must not be issued by BuildStorage", forbidden)
	}
	// The topology legs it DOES share with Build.
	assert.True(t, issued[string(promql.QPodInfo)])
	assert.True(t, issued[string(promql.QVolumeLabels)])
	assert.True(t, issued[string(promql.QAlerts)], "the alert overlay reaches both endpoints")
}

// The body holds only storage-flow edges and only the six node kinds the
// storage chain names — no service, no external.
func TestBuildStorage_EmitsOnlyStorageFlow(t *testing.T) {
	q, _ := newRecordingQuerier(t, storageFixtures())

	g, err := newStorageBuilder(t, q).BuildStorage(
		context.Background(), time.Minute, time.Unix(1, 0).UTC(),
		promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}})
	require.NoError(t, err)

	require.NotEmpty(t, g.Edges)
	for _, e := range g.Edges {
		assert.Equal(t, graph.EdgeTypeStorageFlow, e.Type)
		assert.NotEmpty(t, e.Labels["tier"])
	}

	kinds := map[graph.NodeType]int{}
	for _, n := range g.NodesByID {
		kinds[n.Type()]++
	}
	assert.NotContains(t, kinds, graph.NodeTypeService)
	assert.NotContains(t, kinds, graph.NodeTypeExternal)
	assert.Equal(t, 1, kinds[graph.NodeTypeNetAppSVM], "the SVM tier is materialised")
	assert.Equal(t, 1, kinds[graph.NodeTypeNetAppAggr])
	assert.Equal(t, 1, kinds[graph.NodeTypeNetAppNode])
}

// The whole chain is present and oriented storage → workload.
func TestBuildStorage_DrawsTheWholeChain(t *testing.T) {
	q, _ := newRecordingQuerier(t, storageFixtures())

	g, err := newStorageBuilder(t, q).BuildStorage(
		context.Background(), time.Minute, time.Unix(1, 0).UTC(),
		promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}})
	require.NoError(t, err)

	tiers := map[string]int{}
	for _, e := range g.Edges {
		tiers[e.Labels["tier"]]++
	}
	assert.Equal(t, map[string]int{
		graph.StorageTierNodeAggr: 1,
		graph.StorageTierAggrSVM:  1,
		graph.StorageTierSVMPVC:   1,
		graph.StorageTierPVCPod:   1,
		graph.StorageTierPodNode:  1,
	}, tiers)
}

// The identity table reaches the storage graph too, so the projection-level
// cluster filter can recover each identity's raw component exactly as it does
// for /v1/graph.
func TestBuildStorage_CarriesClusterIdentities(t *testing.T) {
	fixtures := storageFixtures()
	for _, q := range []promql.Query{promql.QPodInfo, promql.QNodeInfo, promql.QPVCBindings, promql.QPVCInfo} {
		for _, s := range fixtures[q] {
			s.Metric["az"] = "zone-a"
			s.Metric["env"] = "prod"
		}
	}
	q, _ := newRecordingQuerier(t, fixtures)

	g, err := newStorageBuilder(t, q).BuildStorage(
		context.Background(), time.Minute, time.Unix(1, 0).UTC(),
		promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}})
	require.NoError(t, err)

	require.NotNil(t, g.ClusterIdentities)
	assert.Contains(t, g.ClusterIdentities, "zone-a-prod-c1")
	assert.Equal(t, "c1", g.ClusterRawName("zone-a-prod-c1"))
}

// An estate the selector matched nothing in is an empty 200's worth of graph,
// never an outside-retention error: BuildStorage issues no up{} probe, so the
// classification cannot fire.
func TestBuildStorage_EmptyEstateIsNotOutsideRetention(t *testing.T) {
	q, seen := newRecordingQuerier(t, nil)

	g, err := newStorageBuilder(t, q).BuildStorage(
		context.Background(), time.Minute, time.Unix(1, 0).UTC(),
		promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}})
	require.NoError(t, err, "an empty filtered estate is an empty graph, not an error")
	assert.Empty(t, g.NodesByID)
	assert.Empty(t, g.Edges)

	for _, name := range seen() {
		assert.NotEqual(t, string(promql.QUpProbe), name)
	}
}

// A flowless aggregate is materialised — that is what lets ProjectStorage draw
// a root nothing flows through — even though no claim reaches it.
func TestBuildStorage_MaterialisesFlowlessInventory(t *testing.T) {
	fixtures := storageFixtures()
	fixtures[promql.QAggrStatus] = sampleVec(model.Sample{Metric: model.Metric{
		"cluster": "ontap-prod", "node": "ontap-prod-02", "aggr": "aggr9",
	}, Value: 1})
	q, _ := newRecordingQuerier(t, fixtures)

	g, err := newStorageBuilder(t, q).BuildStorage(
		context.Background(), time.Minute, time.Unix(1, 0).UTC(),
		promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}})
	require.NoError(t, err)

	assert.Contains(t, g.NodesByID, graph.NetAppAggrID("ontap-prod", "aggr9"),
		"an aggregate no claim joined is still materialised, for the root rule")
	for _, e := range g.Edges {
		assert.NotEqual(t, graph.NetAppAggrID("ontap-prod", "aggr9"), e.Source)
		assert.NotEqual(t, graph.NetAppAggrID("ontap-prod", "aggr9"), e.Target)
	}
}
