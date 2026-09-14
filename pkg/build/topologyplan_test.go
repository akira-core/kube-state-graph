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

	scope, err := graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"b/y", "a/x", "c/x"})
	require.NoError(t, err)
	p := storagePlan(scope.Roots)
	assert.True(t, p.scopePods)
	assert.Equal(t, []string{"x", "x", "y"}, p.podRoots, "root names are sorted: map order must not reach the scope")
	issued := 0
	for _, l := range legs {
		if p.issuesFirstWave(l.query) {
			issued++
		}
	}
	assert.Equal(t, 30, issued, "37 less the five skipped families less the two pod families moved to a wave")
	for _, q := range promql.PodScopedQueries {
		assert.False(t, p.issuesFirstWave(q), "%s is read by reference, in a second wave", q)
	}
}
