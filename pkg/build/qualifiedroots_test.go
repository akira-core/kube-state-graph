package build

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// accept-multi-zone-storage-graph: `aggr=` / `svm=` accept `<ontap_cluster>/<name>`
// beside the bare form. A qualified value is read one query per ONTAP cluster
// (cluster equality + name alternation), so an aggregate or SVM of that name on
// another filer is never read for the root.

func qualifiedRoots(t *testing.T, kind graph.StorageRootKind, values ...string) graph.StorageRoots {
	t.Helper()
	return vlrRoots(t, kind, values)
}

// qualifiedEstate: aggr1 / svm0 exist on two filers, each with a mounted claim,
// plus an aggregate holding no claim on each filer.
func qualifiedEstate() map[promql.Query]model.Vector {
	fx := hubEstate([]vlrVol{
		{"trident_pvc_orders", "ontap-prod", "ontap-prod-01", "aggr1", "svm0", 300},
		{"trident_pvc_ledger", "ontap-lab", "ontap-lab-01", "aggr1", "svm0", 200},
		{"trident_pvc_other", "ontap-prod", "ontap-prod-02", "aggr2", "svm_x", 50},
	}, []hubClaim{
		{ns: "shop", claim: "orders-data", pv: "pvc-orders", pods: []string{"orders-0"}},
		{ns: "shop", claim: "ledger-data", pv: "pvc-ledger", pods: []string{"ledger-0"}},
		{ns: "shop", claim: "other-data", pv: "pvc-other", pods: []string{"other-0"}},
	})
	// Aggregates carrying no volume: present in the gauge families only.
	gauges := vlrHarvest(nil,
		[3]string{"ontap-prod", "ontap-prod-03", "aggr9"},
		[3]string{"ontap-lab", "ontap-lab-03", "aggr9"})
	for _, q := range []promql.Query{promql.QAggrStatus, promql.QNetAppNodeStatus} {
		fx[q] = append(fx[q], gauges[q]...)
	}
	return fx
}

func TestStoragePlan_CarriesQualifiedRoots(t *testing.T) {
	plan := storagePlan(qualifiedRoots(t, graph.StorageRootAggr,
		"ontap-prod/aggr2", "aggr9", "ontap-prod/aggr1", "ontap-lab/aggr1", ""))
	assert.Equal(t, []string{"aggr9"}, plan.volumeAggrs)
	assert.Equal(t, map[string][]string{
		"ontap-lab":  {"aggr1"},
		"ontap-prod": {"aggr1", "aggr2"},
	}, plan.volumeAggrPairs, "keyed by ONTAP cluster, each name set sorted")
	assert.Empty(t, plan.volumeSVMPairs)
	assert.True(t, plan.harvestSeed())
	assert.True(t, plan.flowlessAggrGauges())
	assert.True(t, plan.flowlessControllers())

	svm := storagePlan(qualifiedRoots(t, graph.StorageRootSVM, "ontap-prod/svm0"))
	assert.Empty(t, svm.volumeSVMs)
	assert.Equal(t, map[string][]string{"ontap-prod": {"svm0"}}, svm.volumeSVMPairs)
	assert.True(t, svm.harvestSeed(), "a qualified value alone seeds volume_labels")
	assert.Empty(t, svm.volumeAggrPairs)

	// An embedder fills StorageRoots itself; a half-empty ref names nothing.
	raw := storagePlan(graph.StorageRoots{Kind: graph.StorageRootAggr, Qualified: []graph.ONTAPRef{
		{ONTAPCluster: "", Name: "aggr1"}, {ONTAPCluster: "ontap-prod", Name: ""},
	}})
	assert.Empty(t, raw.volumeAggrPairs)
	assert.False(t, raw.harvestSeed())

	// Other kinds never read Qualified.
	other := storagePlan(graph.StorageRoots{Kind: graph.StorageRootONTAPCluster, Names: []string{"c"},
		Qualified: []graph.ONTAPRef{{ONTAPCluster: "x", Name: "y"}}})
	assert.Empty(t, other.volumeAggrPairs)
	assert.Empty(t, other.volumeSVMPairs)
}

// Spec: "A qualified aggregate root reads one filer's aggregate".
func TestRootedVolumeLabels_QualifiedAggregateSeedQueries(t *testing.T) {
	roots := qualifiedRoots(t, graph.StorageRootAggr,
		"ontap-prod/aggr1", "ontap-prod/aggr2", "ontap-lab/aggr1", "aggr9")
	plan, err := storagePlan(roots).prepareHarvestSeed(time.Minute, Options{}.qosScopeBatchBytes(), promql.LabelKeys{}.OrDefault(), vlrSel)
	require.NoError(t, err)

	got := make([]string, 0, len(plan.phaseOne))
	for _, r := range plan.phaseOne {
		got = append(got, r.rendered)
	}
	assert.Equal(t, []string{
		`last_over_time(volume_labels{az="zone-a",env="prod",aggr="aggr9"}[1m])`,
		`last_over_time(volume_labels{az="zone-a",env="prod",cluster="ontap-lab",aggr="aggr1"}[1m])`,
		`last_over_time(volume_labels{az="zone-a",env="prod",cluster="ontap-prod",aggr=~"aggr1|aggr2"}[1m])`,
	}, got, "bare group first, then one query per ONTAP cluster in sorted order")
}

func TestRootedVolumeLabels_QualifiedSVMSeedQueries(t *testing.T) {
	roots := qualifiedRoots(t, graph.StorageRootSVM, "ontap-prod/svm0", "ontap-lab/svm_lab", "svm_shop")
	plan, err := storagePlan(roots).prepareHarvestSeed(time.Minute, Options{}.qosScopeBatchBytes(), promql.LabelKeys{}.OrDefault(), vlrSel)
	require.NoError(t, err)

	got := make([]string, 0, len(plan.phaseOne))
	for _, r := range plan.phaseOne {
		assert.Equal(t, groupSVM, r.group)
		got = append(got, r.rendered)
	}
	assert.Equal(t, []string{
		`last_over_time(volume_labels{az="zone-a",env="prod",svm="svm_shop"}[1m])`,
		`last_over_time(volume_labels{az="zone-a",env="prod",cluster="ontap-lab",svm="svm_lab"}[1m])`,
		`last_over_time(volume_labels{az="zone-a",env="prod",cluster="ontap-prod",svm="svm0"}[1m])`,
	}, got)
}

// The chunk cap counts every group, so a qualified set spanning more ONTAP
// clusters than the cap allows is rejected before any query.
func TestRootedVolumeLabels_QualifiedSetPastTheCapIsRejected(t *testing.T) {
	var refs []graph.ONTAPRef
	for i := 0; i <= maxRootedVolumeLabelChunks; i++ {
		refs = append(refs, graph.ONTAPRef{ONTAPCluster: fmt.Sprintf("ontap-%03d", i), Name: "aggr1"})
	}
	q := promqlfake.New(qualifiedEstate())
	_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		graph.StorageRoots{Kind: graph.StorageRootAggr, Qualified: refs})
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Empty(t, q.Issued(), "no upstream query is issued, and no unrestricted read replaces it")

	// One group fewer fits.
	ok := promqlfake.New(qualifiedEstate())
	_, err = New(ok, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		graph.StorageRoots{Kind: graph.StorageRootAggr, Qualified: refs[:maxRootedVolumeLabelChunks]})
	require.NoError(t, err)
}

// Bare and qualified groups share the one cap.
func TestRootedVolumeLabels_CapCountsBareAndQualifiedGroups(t *testing.T) {
	refs := make([]graph.ONTAPRef, 0, maxRootedVolumeLabelChunks)
	for i := range maxRootedVolumeLabelChunks {
		refs = append(refs, graph.ONTAPRef{ONTAPCluster: fmt.Sprintf("ontap-%03d", i), Name: "aggr1"})
	}
	q := promqlfake.New(qualifiedEstate())
	_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		graph.StorageRoots{Kind: graph.StorageRootAggr, Names: []string{"bare"}, Qualified: refs})
	require.Equal(t, ReasonInvalidScope, AsReason(err), "one bare group plus a full set of qualified groups is past the cap")
	assert.Empty(t, q.Issued())
}

// Spec (design D4): owner completion treats a qualified (oc, aggr) as read
// whole, like a bare aggregate — an SVM-group row of it needs no second read.
func TestOwnerCompletionTargets_QualifiedAggregateIsReadWhole(t *testing.T) {
	row := func(oc, aggr string) *model.Sample {
		return &model.Sample{Metric: model.Metric{"cluster": model.LabelValue(oc), "aggr": model.LabelValue(aggr)}}
	}
	rows := model.Vector{row("ontap-prod", "aggr1"), row("ontap-lab", "aggr1"), row("ontap-prod", "aggr7")}

	plan := topologyPlan{volumeAggrPairs: map[string][]string{"ontap-prod": {"aggr1"}}}
	assert.Equal(t, map[string][]string{
		"ontap-lab":  {"aggr1"},
		"ontap-prod": {"aggr7"},
	}, ownerCompletionTargets(rows, plan), "only the qualified pair is covered; the same name on another filer is not")

	bare := topologyPlan{volumeAggrs: []string{"aggr1"}}
	assert.Equal(t, map[string][]string{"ontap-prod": {"aggr7"}}, ownerCompletionTargets(rows, bare))
}

func TestUncoveredAggrPairs_QualifiedAggregateIsCovered(t *testing.T) {
	row := func(oc, aggr string) *model.Sample {
		return &model.Sample{Metric: model.Metric{"cluster": model.LabelValue(oc), "aggr": model.LabelValue(aggr)}}
	}
	rows := model.Vector{row("ontap-prod", "aggr1"), row("ontap-lab", "aggr1"), row("ontap-prod", "aggr7")}
	plan := topologyPlan{volumeAggrPairs: map[string][]string{"ontap-prod": {"aggr1"}}}
	assert.Equal(t, map[string][]string{
		"ontap-lab":  {"aggr1"},
		"ontap-prod": {"aggr7"},
	}, uncoveredAggrPairs(rows, plan), "the flowless read already fetched ontap-prod/aggr1's gauges")
}

// Spec (4.3): a qualified aggregate's flowless gauges are read by the (ONTAP
// cluster, aggregate) pair alone, and no other filer's same-named aggregate is
// materialised.
func TestFlowless_QualifiedAggregateReadsThatFilersGaugesOnly(t *testing.T) {
	q := promqlfake.New(qualifiedEstate())
	g, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		qualifiedRoots(t, graph.StorageRootAggr, "ontap-prod/aggr9"))
	require.NoError(t, err)

	gauge := q.QueriesFor(promql.QAggrStatus)
	require.Len(t, gauge, 1)
	assert.Equal(t, `last_over_time(aggr_new_status{az="zone-a",env="prod",cluster="ontap-prod",aggr="aggr9"}[1m])`, gauge[0])
	for _, fam := range promql.AggrPairQueries {
		for _, query := range q.QueriesFor(fam) {
			assert.NotContains(t, query, "ontap-lab", "%s never reads the other filer", fam)
		}
	}

	assert.Contains(t, g.NodesByID, graph.NetAppAggrID("ontap-prod", "aggr9"), "the qualified aggregate is materialised")
	assert.NotContains(t, g.NodesByID, graph.NetAppAggrID("ontap-lab", "aggr9"), "the same-named aggregate of another filer is never fetched")
}
