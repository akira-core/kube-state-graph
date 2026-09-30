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
		if kind == StorageRootPod || kind == StorageRootPVC {
			continue // ref-valued kinds: covered by their own tests
		}
		s, err := NewStorageScope(nil, nil, kind, []string{"n"})
		require.NoError(t, err)
		assert.Equal(t, kind, s.Roots.Kind)
		assert.Equal(t, []string{"n"}, s.Roots.Names)
		assert.True(t, s.Roots.Any())
	}

	_, err := NewStorageScope(nil, nil, StorageRootKind("nope"), []string{"n"})
	require.Error(t, err)
}

func TestPodRef_String(t *testing.T) {
	assert.Equal(t, "shop/orders-0", PodRef{Namespace: "shop", Name: "orders-0"}.String())
}

// A pvc root is a (namespace, claim) ref like a pod root, and lives in Claims
// — never in Names or Pods. Refs are sorted and de-duplicated, empty values
// dropped, so a request repeating or re-ordering its roots yields one scope.
func TestNewStorageScope_ClaimRoots(t *testing.T) {
	s, err := NewStorageScope(nil, nil, StorageRootPVC,
		[]string{"shop/orders-data", "shop/cache", "", "platform/queue", "shop/cache"})
	require.NoError(t, err)
	assert.Equal(t, StorageRootPVC, s.Roots.Kind)
	assert.Equal(t, []ClaimRef{
		{Namespace: "platform", Name: "queue"},
		{Namespace: "shop", Name: "cache"},
		{Namespace: "shop", Name: "orders-data"},
	}, s.Roots.Claims)
	assert.Empty(t, s.Roots.Names)
	assert.Empty(t, s.Roots.Pods)
	assert.True(t, s.Roots.Any())

	reordered, err := NewStorageScope(nil, nil, StorageRootPVC,
		[]string{"platform/queue", "shop/orders-data", "shop/cache"})
	require.NoError(t, err)
	assert.Equal(t, s, reordered)
}

func TestClaimRef_String(t *testing.T) {
	assert.Equal(t, "shop/orders-data", ClaimRef{Namespace: "shop", Name: "orders-data"}.String())
}

// A pv root is a bare PersistentVolume name, so it is stored in Names like every
// other bare-name kind.
func TestNewStorageScope_VolumeRoots(t *testing.T) {
	s, err := NewStorageScope(nil, nil, StorageRootPV, []string{"pvc-b", "", "pvc-a", "pvc-b"})
	require.NoError(t, err)
	assert.Equal(t, StorageRootPV, s.Roots.Kind)
	assert.Equal(t, []string{"pvc-a", "pvc-b"}, s.Roots.Names)
	assert.Empty(t, s.Roots.Claims)
	assert.True(t, s.Roots.Any())
}

// A pvc value shares the pod rule verbatim: exactly one "/" between two
// non-empty segments.
func TestNewStorageScope_MalformedClaimRoots(t *testing.T) {
	for _, bad := range []string{"orders-data", "shop/orders/data", "/x", "x/"} {
		t.Run(bad, func(t *testing.T) {
			_, err := NewStorageScope(nil, nil, StorageRootPVC, []string{bad})
			require.Error(t, err)
			assert.Contains(t, err.Error(), bad)
			assert.Contains(t, err.Error(), "<namespace>/<claim-name>")
		})
	}
}

// The pod rule and its message are unchanged by sharing the parser.
func TestNewStorageScope_MalformedPodRootMessageUnchanged(t *testing.T) {
	_, err := NewStorageScope(nil, nil, StorageRootPod, []string{"orders-0"})
	require.EqualError(t, err, `invalid pod root "orders-0": expected <namespace>/<pod-name>`)
}

func TestNewStorageScope_BareClaimAndVolumeValuesAreNoOps(t *testing.T) {
	for _, kind := range []StorageRootKind{StorageRootPVC, StorageRootPV} {
		s, err := NewStorageScope(nil, nil, kind, []string{""})
		require.NoError(t, err)
		assert.False(t, s.Roots.Any(), "%s: bare values leave no root requested", kind)
		assert.Empty(t, s.Roots.Kind)
		assert.Nil(t, s.Roots.Claims)
		assert.Nil(t, s.Roots.Names)
	}
}

// Parameter order is part of the contract: the mixed-kind message names
// parameters in this order.
func TestStorageRootKinds_Order(t *testing.T) {
	assert.Equal(t, []StorageRootKind{
		StorageRootONTAPCluster, StorageRootONTAPNode, StorageRootAggr, StorageRootSVM,
		StorageRootNode, StorageRootPod, StorageRootPVC, StorageRootPV, StorageRootApplication,
	}, StorageRootKinds)
}

// accept-multi-zone-storage-graph: `aggr=` and `svm=` also accept
// `<ontap_cluster>/<name>`, naming exactly one component. Bare values keep their
// meaning (that name on every filer) and the two forms may be mixed.
func TestNewStorageScope_QualifiedAggrAndSVM(t *testing.T) {
	for _, kind := range []StorageRootKind{StorageRootAggr, StorageRootSVM} {
		t.Run(string(kind), func(t *testing.T) {
			s, err := NewStorageScope(nil, nil, kind, []string{
				"ontap-prod/x2", "bare", "ontap-lab/x1", "ontap-prod/x2", "ontap-prod/x1", "",
			})
			require.NoError(t, err)
			assert.Equal(t, kind, s.Roots.Kind)
			assert.Equal(t, []string{"bare"}, s.Roots.Names)
			assert.Equal(t, []ONTAPRef{
				{ONTAPCluster: "ontap-lab", Name: "x1"},
				{ONTAPCluster: "ontap-prod", Name: "x1"},
				{ONTAPCluster: "ontap-prod", Name: "x2"},
			}, s.Roots.Qualified, "sorted by (ONTAP cluster, name) and de-duplicated")
			assert.True(t, s.Roots.Any())

			reordered, err := NewStorageScope(nil, nil, kind, []string{
				"ontap-prod/x1", "ontap-lab/x1", "bare", "ontap-prod/x2",
			})
			require.NoError(t, err)
			assert.Equal(t, s, reordered, "value order never changes the scope")
		})
	}
}

func TestNewStorageScope_QualifiedOnlyIsARoot(t *testing.T) {
	s, err := NewStorageScope(nil, nil, StorageRootAggr, []string{"ontap-prod/aggr9"})
	require.NoError(t, err)
	assert.True(t, s.Roots.Any(), "a qualified value alone is a root")
	assert.Empty(t, s.Roots.Names)
	assert.Equal(t, []ONTAPRef{{ONTAPCluster: "ontap-prod", Name: "aggr9"}}, s.Roots.Qualified)
}

// A qualified value whose name also appears bare adds nothing: the bare form
// already roots that name on every filer.
func TestNewStorageScope_BareSubsumesQualified(t *testing.T) {
	s, err := NewStorageScope(nil, nil, StorageRootSVM, []string{"ontap-prod/svm0", "svm0", "ontap-lab/svm_shop"})
	require.NoError(t, err)
	assert.Equal(t, []string{"svm0"}, s.Roots.Names)
	assert.Equal(t, []ONTAPRef{{ONTAPCluster: "ontap-lab", Name: "svm_shop"}}, s.Roots.Qualified)
}

func TestNewStorageScope_RejectsMalformedQualifiedRoot(t *testing.T) {
	for _, kind := range []StorageRootKind{StorageRootAggr, StorageRootSVM} {
		for _, bad := range []string{
			"ontap-prod/", // empty name
			"/aggr1",      // empty ONTAP cluster
			"a/b/c",       // two separators
			"/",           // both empty
		} {
			_, err := NewStorageScope(nil, nil, kind, []string{"ok", bad})
			require.Error(t, err, "%s %q", kind, bad)
			assert.Contains(t, err.Error(), bad)
			assert.Contains(t, err.Error(), string(kind))
		}
	}
}

// Only aggr and svm split on "/": every other kind's value is a bare name.
func TestNewStorageScope_OtherKindsDoNotSplit(t *testing.T) {
	for _, kind := range []StorageRootKind{StorageRootONTAPCluster, StorageRootONTAPNode, StorageRootNode, StorageRootApplication} {
		s, err := NewStorageScope(nil, nil, kind, []string{"a/b"})
		require.NoError(t, err, kind)
		assert.Equal(t, []string{"a/b"}, s.Roots.Names, kind)
		assert.Empty(t, s.Roots.Qualified, kind)
	}
}
