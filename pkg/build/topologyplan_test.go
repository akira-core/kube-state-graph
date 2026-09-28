package build

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func TestLargestLeg(t *testing.T) {
	name, n := largestLeg(map[string]int{"kube_pod_owner": 5, "kube_pod_info": 5, "ALERTS": 1})
	assert.Equal(t, "kube_pod_info", name, "a tie breaks on the lexically-smallest name")
	assert.Equal(t, 5, n)

	name, n = largestLeg(map[string]int{"kube_node_info": 0})
	assert.Equal(t, "kube_node_info", name)
	assert.Zero(t, n)

	name, n = largestLeg(nil)
	assert.Empty(t, name)
	assert.Zero(t, n)
}

func TestTopologyPlans(t *testing.T) {
	legs := topologyLegs(&topologyVectors{})
	require.Len(t, legs, 37, "the first wave of the /v1/graph read")
	for _, l := range legs {
		assert.True(t, fullPlan.issuesFirstWave(l.query), "the full plan issues %s", l.query)
	}

	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootPod, []string{"b/y", "a/x", "c/x"})
	require.NoError(t, err)
	p := storagePlan(scope.Roots)
	assert.True(t, p.byReference)
	nodeScope, err := graph.NewStorageScope(nil, nil, graph.StorageRootNode, []string{"n1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"n1"}, storagePlan(nodeScope.Roots).nodeRoots)
	issued := 0
	for _, l := range legs {
		if p.issuesFirstWave(l.query) {
			issued++
		}
	}
	assert.Equal(t, 1, issued, "a storage plan's first wave is ALERTS alone")
	assert.True(t, p.issuesFirstWave(promql.QAlerts))
	beside := 0
	for _, l := range legs {
		if p.issuesBesideSeed(l.query) {
			beside++
		}
	}
	assert.Equal(t, 0, beside,
		"a pod root reads nothing across the zone; ALERTS is the first wave and every other family hangs off the seed")
	for _, q := range promql.ReferenceScopedQueries {
		assert.False(t, p.issuesFirstWave(q), "%s is read by reference, in a second wave", q)
		assert.False(t, p.issuesBesideSeed(q), "%s is not zone-wide inventory", q)
	}
	assert.Equal(t, graph.StorageRootPod, p.kind)
	assert.Equal(t, []graph.PodRef{{Namespace: "a", Name: "x"}, {Namespace: "b", Name: "y"}, {Namespace: "c", Name: "x"}}, p.pods)
}
