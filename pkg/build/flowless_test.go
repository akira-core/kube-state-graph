package build

import (
	"slices"
	"strings"
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

// Spec: "Aggregate with no claims still shows" and "A rooted aggregate with no
// claim is still drawn".
func TestFlowless_AggregateWithNoClaimStillDrawn(t *testing.T) {
	fx := map[promql.Query]model.Vector{
		promql.QAggrStatus: {
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-02", "aggr", "aggr9"),
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "aggr", "aggr1"),
		},
		promql.QAggrSpaceUsed: {
			withValue(planHarvest("cluster", "ontap-prod", "node", "ontap-prod-02", "aggr", "aggr9"), 10),
		},
		promql.QAggrSpaceTotal: {
			withValue(planHarvest("cluster", "ontap-prod", "node", "ontap-prod-02", "aggr", "aggr9"), 100),
		},
		promql.QNetAppNodeStatus: {
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-02"),
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01"),
		},
	}
	q := promqlfake.New(fx)
	scope := vlrScope(t, graph.StorageRootAggr, []string{"aggr9"})
	g, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)

	for _, fam := range promql.AggrPairQueries {
		got := q.QueriesFor(fam)
		require.NotEmpty(t, got, fam)
		assert.Contains(t, got[0], `aggr="aggr9"`, fam)
		assert.NotContains(t, got[0], "aggr1", fam)
	}
	for _, fam := range promql.NetAppNodePairQueries {
		got := q.QueriesFor(fam)
		require.NotEmpty(t, got, fam)
		assert.Contains(t, got[0], `node="ontap-prod-02"`, fam)
		assert.NotContains(t, got[0], "ontap-prod-01", fam)
	}

	body := cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	assert.True(t, parityHasID(body, "netapp/ontap-prod/aggr/aggr9"))
	assert.True(t, parityHasID(body, "netapp/ontap-prod/ontap-prod-02"), "the owning controller is the compound parent")
	assert.False(t, parityHasID(body, "netapp/ontap-prod/aggr/aggr1"))
	assert.Empty(t, body.Elements.Edges)
	aggr := g.NodesByID[graph.NetAppAggrID("ontap-prod", "aggr9")]
	require.NotNil(t, aggr)
	assert.Equal(t, graph.HealthOnline, aggr.Health())
}

// Spec: "Controller with no claims still shows".
func TestFlowless_ControllerWithNoClaimsStillShows(t *testing.T) {
	fx := map[promql.Query]model.Vector{
		promql.QNetAppNodeStatus: {
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-03"),
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01"),
		},
		promql.QNetAppNodeLabels: {
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-03", "model", "FAS8300"),
		},
	}
	q := promqlfake.New(fx)
	scope := vlrScope(t, graph.StorageRootONTAPNode, []string{"ontap-prod-03"})
	g, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)

	for _, fam := range promql.NetAppNodePairQueries {
		got := q.QueriesFor(fam)
		require.Len(t, got, 1, fam)
		assert.Contains(t, got[0], `node="ontap-prod-03"`, fam)
		assert.NotContains(t, got[0], "ontap-prod-01", fam)
	}
	body := cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	assert.True(t, parityHasID(body, "netapp/ontap-prod/ontap-prod-03"))
	assert.False(t, parityHasID(body, "netapp/ontap-prod/ontap-prod-01"))
	assert.Empty(t, body.Elements.Edges)
	node := g.NodesByID[graph.NetAppNodeID("ontap-prod", "ontap-prod-03")]
	require.NotNil(t, node)
	assert.Equal(t, graph.HealthOnline, node.Health())
}

func TestFlowless_ONTAPClusterReadsTheFiler(t *testing.T) {
	fx := map[promql.Query]model.Vector{
		promql.QAggrStatus: {
			planHarvest("cluster", "ontap-empty", "node", "ontap-empty-01", "aggr", "aggr-empty"),
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "aggr", "aggr1"),
		},
		promql.QNetAppNodeStatus: {
			planHarvest("cluster", "ontap-empty", "node", "ontap-empty-01"),
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01"),
		},
	}
	q := promqlfake.New(fx)
	scope := vlrScope(t, graph.StorageRootONTAPCluster, []string{"ontap-empty"})
	g, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)

	for _, fam := range slicesConcatGauges() {
		got := q.QueriesFor(fam)
		require.NotEmpty(t, got, fam)
		assert.Contains(t, got[0], `cluster="ontap-empty"`, fam)
		assert.NotContains(t, got[0], "ontap-prod", fam)
	}
	body := cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	assert.True(t, parityHasID(body, "netapp/ontap-empty/aggr/aggr-empty"))
	assert.True(t, parityHasID(body, "netapp/ontap-empty/ontap-empty-01"))
	assert.False(t, parityHasID(body, "netapp/ontap-prod/aggr/aggr1"))
}

func TestFlowless_OverCapRejected(t *testing.T) {
	names := make([]string, maxRootedVolumeLabelChunks+1)
	for i := range names {
		names[i] = "aggr" + strings.Repeat("x", 8) + itoa(i)
	}
	q := promqlfake.New(nil)
	_, err := New(q, Options{QoSScopeBatchBytes: 1}, nil, nil).BuildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootAggr, names).Roots)
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Empty(t, q.Issued())
}

func slicesConcatGauges() []promql.Query {
	return slices.Concat(promql.AggrPairQueries, promql.NetAppNodePairQueries)
}
