package promql

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderHarvestPair(t *testing.T) {
	t.Parallel()

	sel := Selector{
		AZ: []string{"zone-a"}, Env: []string{"prod"},
		Cluster: []string{"c1"}, Namespace: []string{"shop"},
	}
	keys := LabelKeys{AZ: "zone", Env: "tier"}

	aggr, ok := RenderHarvestPair(QAggrStatus, time.Minute, keys, sel, "ontap-prod", []string{"aggr2", "aggr1", "aggr1"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(aggr_new_status{zone="zone-a",tier="prod",cluster="ontap-prod",aggr=~"aggr1|aggr2"}[1m])`,
		aggr, "Kubernetes cluster and namespace never reach a Harvest family")

	node, ok := RenderHarvestPair(QNetAppNodeCPUBusy, time.Minute, LabelKeys{}, sel, "ontap-prod", []string{"na-02", "na-01"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(node_cpu_busy{az="zone-a",env="prod",cluster="ontap-prod",node=~"na-01|na-02"}[1m])`,
		node)

	policy, ok := RenderHarvestPair(QQoSPolicyFixedMaxMBps, time.Minute, LabelKeys{}, Selector{}, "ontap-prod", []string{"svm_a"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(qos_policy_fixed_max_throughput_mbps{cluster="ontap-prod",svm="svm_a"}[1m])`,
		policy)

	emptyCluster, ok := RenderHarvestPair(QAggrSpaceTotal, time.Minute, LabelKeys{}, Selector{}, "", []string{"aggr1"})
	require.True(t, ok)
	assert.Equal(t, `last_over_time(aggr_space_total{cluster="",aggr="aggr1"}[1m])`, emptyCluster,
		"an empty cluster stays an equality so the chunk cannot spill onto a named filer")

	meta, ok := RenderHarvestPair(QAggrSpaceUsed, time.Minute, LabelKeys{}, Selector{}, `on"tap`, []string{"aggr.1", "aggr_2"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(aggr_space_used{cluster="on\"tap",aggr=~"aggr\\.1|aggr_2"}[1m])`,
		meta)

	require.Len(t, HarvestPairQueries, len(AggrPairQueries)+len(NetAppNodePairQueries)+len(PolicyPairQueries))
	assert.Len(t, AggrPairQueries, 3)
	assert.Len(t, NetAppNodePairQueries, 6)
	assert.Len(t, PolicyPairQueries, 2)
	for _, q := range HarvestPairQueries {
		got, ok := RenderHarvestPair(q, time.Minute, LabelKeys{}, Selector{}, "oc", []string{"b", "a"})
		require.True(t, ok, q)
		assert.Contains(t, got, `last_over_time(`+string(q)+`{cluster="oc",`)
		assert.Contains(t, got, `=~"a|b"}[1m])`)
	}

	for _, q := range []Query{QVolumeLabels, QQoSReadOps, QPodInfo, QAlerts, QPVCBindings} {
		_, ok := RenderHarvestPair(q, time.Minute, LabelKeys{}, Selector{}, "oc", []string{"a"})
		assert.False(t, ok, q)
	}
	_, ok = RenderHarvestPair(QAggrStatus, time.Minute, LabelKeys{}, Selector{}, "oc", nil)
	assert.False(t, ok)
	_, ok = RenderHarvestPair(QAggrStatus, time.Minute, LabelKeys{}, Selector{}, "oc", []string{""})
	assert.False(t, ok)
}

func TestRenderHarvestByNameAndCluster(t *testing.T) {
	t.Parallel()

	sel := Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}
	byName, ok := RenderHarvestByName(QAggrStatus, time.Minute, LabelKeys{}, sel, []string{"aggr9", "aggr00"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(aggr_new_status{az="zone-a",env="prod",aggr=~"aggr00|aggr9"}[1m])`,
		byName)

	one, ok := RenderHarvestByName(QNetAppNodeLabels, time.Minute, LabelKeys{}, Selector{}, []string{"ontap-prod-03"})
	require.True(t, ok)
	assert.Equal(t, `last_over_time(node_labels{node="ontap-prod-03"}[1m])`, one)

	byCluster, ok := RenderHarvestByCluster(QNetAppNodeStatus, time.Minute, LabelKeys{}, sel, []string{"ontap-b", "ontap-a"})
	require.True(t, ok)
	assert.Equal(t,
		`last_over_time(node_new_status{az="zone-a",env="prod",cluster=~"ontap-a|ontap-b"}[1m])`,
		byCluster)

	_, ok = RenderHarvestByName(QPodInfo, time.Minute, LabelKeys{}, Selector{}, []string{"aggr1"})
	assert.False(t, ok)
	_, ok = RenderHarvestByCluster(QAggrSpaceUsed, time.Minute, LabelKeys{}, Selector{}, nil)
	assert.False(t, ok)
}

func TestChunkHarvestPairs(t *testing.T) {
	t.Parallel()

	// Remainder 5 splits one-byte names into [a b c][d]: each extra name costs
	// its byte plus the '|' separator, so a|b|c is 5 and d does not fit.
	const nameBudget = 5
	names := []string{"d", "b", "a", "c", "a"}

	t.Run("charges the cluster equality", func(t *testing.T) {
		t.Parallel()
		budget := OwnerCompletionClusterCost("oc") + nameBudget
		got := ChunkHarvestPairs(QAggrStatus, LabelKeys{}, Selector{}, map[string][]string{"oc": names}, budget)
		assert.Equal(t, []HarvestNameChunk{
			{Cluster: "oc", Names: []string{"a", "b", "c"}},
			{Cluster: "oc", Names: []string{"d"}},
		}, got)
	})

	t.Run("charges the request matchers", func(t *testing.T) {
		t.Parallel()
		sel := Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}
		budget := RequestMatcherCost(QAggrStatus, LabelKeys{}, sel) + OwnerCompletionClusterCost("oc") + nameBudget
		got := ChunkHarvestPairs(QAggrStatus, LabelKeys{}, sel, map[string][]string{"oc": names}, budget)
		assert.Equal(t, []HarvestNameChunk{
			{Cluster: "oc", Names: []string{"a", "b", "c"}},
			{Cluster: "oc", Names: []string{"d"}},
		}, got, "forgetting the request matchers would leave room for every name in one chunk")
	})

	t.Run("charges the escaped cluster equality", func(t *testing.T) {
		t.Parallel()
		// aa|bb fits a remainder of 5 and splits at 4. The quoted cluster
		// renders one byte longer than an equal-length plain name, so the
		// same budget splits it.
		budget := OwnerCompletionClusterCost("octap") + 5
		one := ChunkHarvestPairs(QAggrStatus, LabelKeys{}, Selector{}, map[string][]string{
			"octap": {"aa", "bb"},
		}, budget)
		require.Equal(t, []HarvestNameChunk{{Cluster: "octap", Names: []string{"aa", "bb"}}}, one)

		split := ChunkHarvestPairs(QAggrStatus, LabelKeys{}, Selector{}, map[string][]string{
			`oc"ap`: {"bb", "aa"},
		}, budget)
		require.Equal(t, []HarvestNameChunk{
			{Cluster: `oc"ap`, Names: []string{"aa"}},
			{Cluster: `oc"ap`, Names: []string{"bb"}},
		}, split)
		rendered, ok := RenderHarvestPair(QAggrSpaceUsed, time.Minute, LabelKeys{}, Selector{}, split[0].Cluster, split[0].Names)
		require.True(t, ok)
		assert.Equal(t, `last_over_time(aggr_space_used{cluster="oc\"ap",aggr="aa"}[1m])`, rendered)
	})

	t.Run("one filer per chunk", func(t *testing.T) {
		t.Parallel()
		got := ChunkHarvestPairs(QNetAppNodeStatus, LabelKeys{}, Selector{}, map[string][]string{
			"z-filer": {"n1"},
			"a-filer": {"n2", "n1"},
			"":        {"n0"},
		}, 10_000)
		require.Equal(t, []HarvestNameChunk{
			{Cluster: "", Names: []string{"n0"}},
			{Cluster: "a-filer", Names: []string{"n1", "n2"}},
			{Cluster: "z-filer", Names: []string{"n1"}},
		}, got)
		for _, chunk := range got {
			rendered, ok := RenderHarvestPair(QNetAppNodeLabels, time.Minute, LabelKeys{}, Selector{}, chunk.Cluster, chunk.Names)
			require.True(t, ok)
			assert.Contains(t, rendered, `cluster="`+chunk.Cluster+`"`)
			for _, other := range got {
				if other.Cluster == chunk.Cluster || other.Cluster == "" {
					continue
				}
				assert.NotContains(t, rendered, other.Cluster)
			}
		}
	})

	t.Run("an over-budget name is issued alone", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("v", 40)
		budget := OwnerCompletionClusterCost("oc") + 8
		got := ChunkHarvestPairs(QAggrSpaceUsed, LabelKeys{}, Selector{}, map[string][]string{
			"oc": {"a", long},
		}, budget)
		require.Equal(t, []HarvestNameChunk{
			{Cluster: "oc", Names: []string{"a"}},
			{Cluster: "oc", Names: []string{long}},
		}, got)
		rendered, ok := RenderHarvestPair(QAggrSpaceUsed, time.Minute, LabelKeys{}, Selector{}, "oc", got[1].Names)
		require.True(t, ok)
		assert.Contains(t, rendered, `aggr="`+long+`"`)
	})

	t.Run("no budget keeps each filer whole", func(t *testing.T) {
		t.Parallel()
		got := ChunkHarvestPairs(QQoSPolicyFixedMaxIOPS, LabelKeys{}, Selector{}, map[string][]string{
			"b": {strings.Repeat("s", 80), "a"},
			"a": {"svm"},
		}, 0)
		assert.Equal(t, []HarvestNameChunk{
			{Cluster: "a", Names: []string{"svm"}},
			{Cluster: "b", Names: []string{"a", strings.Repeat("s", 80)}},
		}, got)
	})

	t.Run("empty and non-pair inputs render nothing", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, ChunkHarvestPairs(QAggrStatus, LabelKeys{}, Selector{}, nil, 100))
		assert.Nil(t, ChunkHarvestPairs(QAggrStatus, LabelKeys{}, Selector{}, map[string][]string{"oc": {"", ""}}, 100))
		assert.Nil(t, ChunkHarvestPairs(QPodInfo, LabelKeys{}, Selector{}, map[string][]string{"oc": {"a"}}, 100))
		assert.Nil(t, ChunkHarvestPairs(QVolumeLabels, LabelKeys{}, Selector{}, map[string][]string{"oc": {"a"}}, 100))
		assert.Nil(t, ChunkHarvestPairs(QQoSReadOps, LabelKeys{}, Selector{}, map[string][]string{"oc": {"a"}}, 100))
	})
}
