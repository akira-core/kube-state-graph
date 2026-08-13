package graph

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var uuidV5Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestNewEdge_StableAcrossRebuilds(t *testing.T) {
	a := NewEdge(EdgeTypePodCallsPod, "cluster-alpha/abc", "cluster-beta/def", nil)
	b := NewEdge(EdgeTypePodCallsPod, "cluster-alpha/abc", "cluster-beta/def", nil)
	assert.Equal(t, a.ID, b.ID, "expected stable ID across rebuilds")
}

func TestNewEdge_UUIDv5Format(t *testing.T) {
	for _, e := range []*Edge{
		NewEdge(EdgeTypePodCallsPod, "cluster-alpha/abc", "cluster-beta/def", nil),
		NewEdge(EdgeTypeServiceSelectsPod, "cluster-alpha/ns/svc", "cluster-alpha/abc", nil),
		NewEdge(EdgeTypePodMountsPVC, "cluster-alpha/abc", "cluster-alpha/ns/claim", nil),
	} {
		assert.Regexp(t, uuidV5Re, e.ID)
	}
}

func TestNewEdge_DistinctTuplesProduceDistinctIDs(t *testing.T) {
	base := NewEdge(EdgeTypePodCallsPod, "src", "tgt", nil)
	others := []*Edge{
		NewEdge(EdgeTypeServiceSelectsPod, "src", "tgt", nil),
		NewEdge(EdgeTypePodCallsPod, "src2", "tgt", nil),
		NewEdge(EdgeTypePodCallsPod, "src", "tgt2", nil),
	}
	seen := map[string]bool{base.ID: true}
	for _, o := range others {
		assert.False(t, seen[o.ID], "expected distinct ID, got collision %s", o.ID)
		seen[o.ID] = true
	}
}

func TestNewEdge_LabelsDefaultEmpty(t *testing.T) {
	e := NewEdge(EdgeTypePodCallsPod, "a", "b", nil)
	assert.NotNil(t, e.Labels, "expected non-nil labels even when nil supplied")
	assert.Empty(t, e.Labels)
}

// TestNewEdge_MetricsNil pins that NewEdge alone yields Metrics == nil —
// RED is attached only via WithMetrics after the edge is constructed.
func TestNewEdge_MetricsNil(t *testing.T) {
	e := NewEdge(EdgeTypePodCallsPod, "a", "b", nil)
	assert.Nil(t, e.Metrics)
}

// TestWithMetrics_ImmutableCopy asserts WithMetrics leaves the original
// untouched, produces an identical ID, and only sets Metrics on the copy.
func TestWithMetrics_ImmutableCopy(t *testing.T) {
	orig := NewEdge(EdgeTypePodCallsPod, "cluster-a/uid-1", "cluster-a/uid-2",
		map[string]string{"cluster": "cluster-a"})
	origID := orig.ID
	origType := orig.Type
	origSrc := orig.Source
	origTgt := orig.Target
	origLabels := map[string]string{}
	for k, v := range orig.Labels {
		origLabels[k] = v
	}

	errRate := 0.1
	p90 := 12.5
	with := orig.WithMetrics(EdgeMetrics{
		Rate:        3.5,
		ErrorRate:   &errRate,
		P90ServerMs: &p90,
	})

	// Original untouched.
	assert.Nil(t, orig.Metrics)
	assert.Equal(t, origID, orig.ID)
	assert.Equal(t, origType, orig.Type)
	assert.Equal(t, origSrc, orig.Source)
	assert.Equal(t, origTgt, orig.Target)
	assert.Equal(t, origLabels, orig.Labels)

	// Copy carries metrics with identical identity.
	assert.Equal(t, origID, with.ID)
	assert.Equal(t, origType, with.Type)
	assert.Equal(t, origSrc, with.Source)
	assert.Equal(t, origTgt, with.Target)
	assert.Equal(t, origLabels, with.Labels)
	require.NotNil(t, with.Metrics)
	assert.InDelta(t, 3.5, with.Metrics.Rate, 1e-12)
	assert.InDelta(t, 0.1, *with.Metrics.ErrorRate, 1e-12)
	assert.InDelta(t, 12.5, *with.Metrics.P90ServerMs, 1e-12)

	// Distinct pointers.
	assert.NotSame(t, orig, with)
}

// TestEdgeTypes_TopologyRelationshipEntries — the two new topology edge types
// are registered (so /v1/edge-types advertises them and ?edge_type= accepts
// them) with the expected directed/intra-cluster source/target contract.
func TestEdgeTypes_TopologyRelationshipEntries(t *testing.T) {
	assert.True(t, ValidEdgeType(EdgeTypePodToNode))
	assert.True(t, ValidEdgeType(EdgeTypePVCToStorageClass))

	byType := map[EdgeType]EdgeTypeDefinition{}
	for _, d := range EdgeTypes {
		byType[d.Type] = d
	}

	p2n := byType[EdgeTypePodToNode]
	assert.True(t, p2n.Directed)
	assert.False(t, p2n.MayCrossCluster)
	assert.Equal(t, []NodeType{NodeTypePod}, p2n.SourceType)
	assert.Equal(t, []NodeType{NodeTypeK8sNode}, p2n.TargetType)

	p2s := byType[EdgeTypePVCToStorageClass]
	assert.True(t, p2s.Directed)
	assert.False(t, p2s.MayCrossCluster)
	assert.Equal(t, []NodeType{NodeTypePVC}, p2s.SourceType)
	assert.Equal(t, []NodeType{NodeTypeStorageClass}, p2s.TargetType)
}
