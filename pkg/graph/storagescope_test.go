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
		[]string{"ontap-prod", "ontap-prod"},
		[]string{"n1", "", "n1"},
		[]string{"aggr1", "aggr2", "aggr1"},
		[]string{"svm_shop"},
		[]string{"shop/orders-0", "shop/orders-0", ""},
	)
	require.NoError(t, err)

	assert.Equal(t, map[string]struct{}{"c1": {}}, a.Clusters)
	assert.Equal(t, map[string]struct{}{"shop": {}}, a.Namespaces)
	assert.Equal(t, map[string]struct{}{"ontap-prod": {}}, a.Roots.ONTAPClusters)
	assert.Equal(t, map[string]struct{}{"n1": {}}, a.Roots.Nodes)
	assert.Len(t, a.Roots.Aggrs, 2)
	assert.Equal(t, map[string]struct{}{"svm_shop": {}}, a.Roots.SVMs)
	assert.Equal(t, map[PodRef]struct{}{{Namespace: "shop", Name: "orders-0"}: {}}, a.Roots.Pods)

	// The same values in a different order build the identical scope.
	b, err := NewStorageScope(
		[]string{"c1"}, []string{"shop"}, []string{"ontap-prod"}, []string{"n1"},
		[]string{"aggr2", "aggr1"}, []string{"svm_shop"}, []string{"shop/orders-0"},
	)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

// A bare `?aggr=` (or any other bare root) is a no-op, not a root that matches
// nothing — the same convention every other set follows.
func TestNewStorageScope_BareValuesAreNoOps(t *testing.T) {
	s, err := NewStorageScope(nil, nil, []string{""}, []string{""}, []string{""}, []string{""}, []string{""})
	require.NoError(t, err)
	assert.False(t, s.Roots.Any(), "bare values leave no root requested")
	assert.Nil(t, s.Roots.Pods)
}

// A NON-empty malformed pod root is an error rather than a silent drop:
// degrading a typo to "no workload root" would widen the answer from one pod's
// storage chain to the whole estate.
func TestNewStorageScope_RejectsMalformedPodRoot(t *testing.T) {
	for _, bad := range []string{
		"orders-0",        // no separator
		"/orders-0",       // empty namespace
		"shop/",           // empty name
		"shop/sub/orders", // two separators
		"/",               // both empty
	} {
		_, err := NewStorageScope(nil, nil, nil, nil, nil, nil, []string{bad})
		require.Errorf(t, err, "%q must be rejected", bad)
		assert.Contains(t, err.Error(), bad, "the error must name the offending value")
	}
}

// Requested* is a property of the REQUEST, not of what resolved. That is what
// lets ProjectStorage return an empty body for `?aggr=typo` instead of falling
// back to the whole estate: the side was asked for, so it must be hit.
func TestStorageRoots_RequestedPerSide(t *testing.T) {
	tests := []struct {
		name              string
		roots             StorageRoots
		storage, workload bool
	}{
		{"no roots", StorageRoots{}, false, false},
		{
			"ontap cluster is storage only",
			StorageRoots{ONTAPClusters: map[string]struct{}{"oc": {}}},
			true, false,
		},
		{
			"aggr is storage only",
			StorageRoots{Aggrs: map[string]struct{}{"aggr1": {}}},
			true, false,
		},
		{
			"svm is storage only",
			StorageRoots{SVMs: map[string]struct{}{"svm1": {}}},
			true, false,
		},
		{
			"pod is workload only",
			StorageRoots{Pods: map[PodRef]struct{}{{Namespace: "shop", Name: "o-0"}: {}}},
			false, true,
		},
		{
			// The load-bearing case: `node=` is matched against BOTH the ONTAP
			// controller name and the Kubernetes node name, because an operator
			// searching a node by name does not know which kind it is. It
			// therefore requests BOTH sides.
			"node requests both sides",
			StorageRoots{Nodes: map[string]struct{}{"n1": {}}},
			true, true,
		},
		{
			"mixed sides",
			StorageRoots{
				Aggrs: map[string]struct{}{"aggr1": {}},
				Pods:  map[PodRef]struct{}{{Namespace: "shop", Name: "o-0"}: {}},
			},
			true, true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.storage, tc.roots.RequestedStorage())
			assert.Equal(t, tc.workload, tc.roots.RequestedWorkload())
			assert.Equal(t, tc.storage || tc.workload, tc.roots.Any())
		})
	}
}

func TestPodRef_String(t *testing.T) {
	assert.Equal(t, "shop/orders-0", PodRef{Namespace: "shop", Name: "orders-0"}.String())
}

// Root iteration must be order-free, so the pod refs come back sorted by
// (namespace, name) rather than in map order.
func TestSortedPodRefs(t *testing.T) {
	got := SortedPodRefs(map[PodRef]struct{}{
		{Namespace: "shop", Name: "orders-0"}:  {},
		{Namespace: "platform", Name: "db-0"}:  {},
		{Namespace: "shop", Name: "catalog-0"}: {},
	})
	assert.Equal(t, []PodRef{
		{Namespace: "platform", Name: "db-0"},
		{Namespace: "shop", Name: "catalog-0"},
		{Namespace: "shop", Name: "orders-0"},
	}, got)
	assert.Empty(t, SortedPodRefs(nil))
}
