package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every root set drops empty values and de-duplicates, so a request repeating
// or re-ordering its roots produces an identical scope — the precondition for
// the storage-graph body being byte-identical across such requests.
func TestNewStorageScope_DeduplicatesAndDropsEmpties(t *testing.T) {
	a, err := NewStorageScope(
		[]string{"c1", "c1", ""},
		[]string{"shop", ""},
		StorageRootAggr,
		[]string{"aggr1", "aggr2", "aggr1", ""},
	)
	require.NoError(t, err)
	assert.Equal(t, StorageRootAggr, a.Roots.Kind)
	assert.Equal(t, map[string]struct{}{"c1": {}}, a.Clusters)
	assert.Equal(t, map[string]struct{}{"shop": {}}, a.Namespaces)
	assert.Equal(t, []string{"aggr1", "aggr2"}, a.Roots.Names)
	assert.Empty(t, a.Roots.Pods)

	b, err := NewStorageScope(
		[]string{"c1"}, []string{"shop"}, StorageRootAggr, []string{"aggr2", "aggr1"},
	)
	require.NoError(t, err)
	assert.Equal(t, a, b)

	pods, err := NewStorageScope(nil, nil, StorageRootPod, []string{"shop/orders-0", "shop/orders-0", "", "platform/db-0"})
	require.NoError(t, err)
	assert.Equal(t, StorageRootPod, pods.Roots.Kind)
	assert.Equal(t, []PodRef{
		{Namespace: "platform", Name: "db-0"},
		{Namespace: "shop", Name: "orders-0"},
	}, pods.Roots.Pods)

	apps, err := NewStorageScope(nil, nil, StorageRootApplication, []string{"b", "a", "", "a"})
	require.NoError(t, err)
	assert.Equal(t, StorageRootApplication, apps.Roots.Kind)
	assert.Equal(t, []string{"a", "b"}, apps.Roots.Names)
}

// A bare root value is a no-op, not a root that matches nothing.
func TestNewStorageScope_BareValuesAreNoOps(t *testing.T) {
	s, err := NewStorageScope(nil, nil, StorageRootAggr, []string{""})
	require.NoError(t, err)
	assert.False(t, s.Roots.Any(), "bare values leave no root requested")
	assert.Empty(t, s.Roots.Kind)
	assert.Nil(t, s.Roots.Names)
	assert.Nil(t, s.Roots.Pods)

	bareApp, err := NewStorageScope(nil, nil, StorageRootApplication, []string{""})
	require.NoError(t, err)
	assert.False(t, bareApp.Roots.Any())
	assert.Empty(t, bareApp.Roots.Kind)
}

// A NON-empty malformed pod root is an error rather than a silent drop.
func TestNewStorageScope_RejectsMalformedPodRoot(t *testing.T) {
	for _, bad := range []string{
		"orders-0",        // no separator
		"/orders-0",       // empty namespace
		"shop/",           // empty name
		"shop/sub/orders", // two separators
		"/",               // both empty
	} {
		_, err := NewStorageScope(nil, nil, StorageRootPod, []string{bad})
		require.Errorf(t, err, "%q must be rejected", bad)
		assert.Contains(t, err.Error(), bad, "the error must name the offending value")
	}
}

func TestNewStorageScope_Kind(t *testing.T) {
	for _, kind := range StorageRootKinds {
		if kind == StorageRootPod {
			continue
		}
		s, err := NewStorageScope(nil, nil, kind, []string{"n"})
		require.NoError(t, err)
		assert.Equal(t, kind, s.Roots.Kind)
		assert.Equal(t, []string{"n"}, s.Roots.Names)
		assert.True(t, s.Roots.Any())
		assert.True(t, s.Roots.HasName("n"))
	}

	_, err := NewStorageScope(nil, nil, StorageRootKind("nope"), []string{"n"})
	require.Error(t, err)
}

func TestPodRef_String(t *testing.T) {
	assert.Equal(t, "shop/orders-0", PodRef{Namespace: "shop", Name: "orders-0"}.String())
}
