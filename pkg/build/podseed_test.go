package build

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// Spec: "A pod root reads its bindings by reference".
func TestPodSeed_ReadsBindingsByReference(t *testing.T) {
	bind := func(ns, pod, claim string) *model.Sample {
		return planKSM("namespace", ns, "pod", pod, "persistentvolumeclaim", claim)
	}
	fx := map[promql.Query]model.Vector{
		promql.QPVCBindings: {
			bind("shop", "orders-0", "orders-data"),
			bind("platform", "redis-0", "redis-data"),
			bind("shop", "redis-0", "foreign-data"),
			bind("platform", "orders-0", "other-data"),
			bind("shop", "catalog-0", "catalog-data"),
		},
		promql.QPodInfo: {
			planKSM("namespace", "shop", "pod", "orders-0", "uid", "uid-o", "node", "worker-1"),
			planKSM("namespace", "platform", "pod", "redis-0", "uid", "uid-r", "node", "worker-2"),
			planKSM("namespace", "shop", "pod", "redis-0", "uid", "uid-sr", "node", "worker-1"),
			planKSM("namespace", "platform", "pod", "orders-0", "uid", "uid-po", "node", "worker-2"),
			planKSM("namespace", "shop", "pod", "catalog-0", "uid", "uid-c", "node", "worker-3"),
		},
	}
	q := promqlfake.New(fx)
	_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootPod, []string{"shop/orders-0", "platform/redis-0"}).Roots)
	require.NoError(t, err)

	bindings := q.QueriesFor(promql.QPVCBindings)
	require.NotEmpty(t, bindings)
	assert.Contains(t, bindings,
		`last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info{az="zone-a",env="prod",namespace=~"platform|shop",pod=~"orders-0|redis-0"}[1m])`)
	for _, query := range bindings {
		assert.NotContains(t, query, "catalog-0", "a pod the roots do not name is not fetched")
		assert.NotContains(t, query, "foreign-data")
		assert.NotContains(t, query, "other-data")
		assert.NotContains(t, query, "catalog-data")
		restricted := strings.Contains(query, `pod=`) || strings.Contains(query, `persistentvolumeclaim=`)
		assert.True(t, restricted, "every binding query is restricted: %s", query)
	}

	// Mounter completion and the claim families read the tracked claims by
	// (namespace, claim), one query per namespace, so a same-named claim in
	// another namespace is never read.
	assert.Contains(t, bindings,
		`last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info{az="zone-a",env="prod",namespace="shop",persistentvolumeclaim="orders-data"}[1m])`)
	assert.Contains(t, bindings,
		`last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info{az="zone-a",env="prod",namespace="platform",persistentvolumeclaim="redis-data"}[1m])`)
	for _, fam := range promql.ClaimScopedQueries {
		for _, query := range q.QueriesFor(fam) {
			if strings.Contains(query, `persistentvolumeclaim=`) {
				assert.Contains(t, query, `,namespace="`, "%s is read per namespace: %s", fam, query)
			}
		}
	}

	// The pod wave reads the roots by (namespace, pod), one query per namespace,
	// so neither cross pair (shop/redis-0, platform/orders-0) is fetched.
	assert.ElementsMatch(t, []string{
		`last_over_time(kube_pod_info{az="zone-a",env="prod",namespace="platform",pod="redis-0"}[1m])`,
		`last_over_time(kube_pod_info{az="zone-a",env="prod",namespace="shop",pod="orders-0"}[1m])`,
	}, q.QueriesFor(promql.QPodInfo))
}

// Spec: "Claimless pod root is still drawn", kept identical to an unrestricted
// read of the same estate.
func TestPodSeed_ClaimlessRootMatchesUnrestricted(t *testing.T) {
	body := assertStorageParity(t, parityFlowless(), parityRoot{kind: graph.StorageRootPod, values: []string{"shop/web-0"}})
	assert.True(t, parityHasID(body, "zone-a-prod-c1/uid-w0"))
}

func TestPodSeed_OverCapRejected(t *testing.T) {
	refs := make([]string, maxRootedVolumeLabelChunks+1)
	for i := range refs {
		refs[i] = "shop/p" + strings.Repeat("x", 8) + itoa(i)
	}
	q := promqlfake.New(nil)
	_, err := New(q, Options{QoSScopeBatchBytes: 1}, nil, nil).BuildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootPod, refs).Roots)
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Empty(t, q.Issued())
}

// Spec: "A pod root's namespace side is bounded by the root cap" and "Many
// namespaces under one pod name". One pod name paired with more namespaces than
// the cap's chunk count times 100 fits the name cap in one chunk, but its
// (namespace, name) pairs take more chunks than the cap, so both endpoints
// reject it before any query. At the cap it is still accepted, and what makes
// the cap meaningful is that every query it admits is bounded too: the seed is
// split by pair, so no issued chunk carries more than MaxAlternationValues
// namespaces or a restriction longer than the byte budget. The namespaces are
// 63 bytes, the longest a Kubernetes namespace may be.
func TestPodSeed_NamespaceSideIsBoundedByTheCap(t *testing.T) {
	refsAcross := func(namespaces int) []string {
		refs := make([]string, namespaces)
		for i := range refs {
			ns := "ns-" + itoa(10000+i)
			refs[i] = ns + strings.Repeat("x", 63-len(ns)) + "/web-0"
		}
		return refs
	}
	atCap := maxRootedVolumeLabelChunks * promql.MaxAlternationValues

	q := promqlfake.New(nil)
	_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootPod, refsAcross(atCap)).Roots)
	require.NoError(t, err, "a pair set filling the cap exactly is accepted")
	seed := q.QueriesFor(promql.QPVCBindings)
	assert.Len(t, seed, maxRootedVolumeLabelChunks, "the seed is split by pair, one query per pair chunk")
	assertPodSeedChunksBounded(t, seed)

	q = promqlfake.New(nil)
	_, err = New(q, Options{}, nil, nil).BuildK8s(t.Context(), time.Minute, vlrEnd, vlrSel,
		k8sRootsOf(t, graph.StorageRootPod, refsAcross(atCap)...))
	require.NoError(t, err, "k8s: a pair set filling the cap exactly is accepted")
	for _, fam := range []promql.Query{promql.QPVCBindings, promql.QPodInfo, promql.QPodOwner} {
		issued := q.QueriesFor(fam)
		assert.Len(t, issued, maxRootedVolumeLabelChunks, "k8s %s follows the seed's pair split", fam)
		assertPodSeedChunksBounded(t, issued)
	}

	over := refsAcross(atCap + 1)
	q = promqlfake.New(nil)
	_, err = New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootPod, over).Roots)
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Empty(t, q.Issued(), "storage: rejected before any query")

	q = promqlfake.New(nil)
	_, err = New(q, Options{}, nil, nil).BuildK8s(t.Context(), time.Minute, vlrEnd, vlrSel,
		k8sRootsOf(t, graph.StorageRootPod, over...))
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Empty(t, q.Issued(), "k8s: rejected before any query")
}

// Spec: "A pod root set is bounded by its pair count". A cross product of 40
// namespaces and 40 names packs into ONE pair chunk (each side is far under 100
// values), so the chunk cap alone would admit it at any size. The pair count is
// capped at maxRootedVolumeLabelChunks × MaxAlternationValues, the most values
// any other root kind admits.
func TestPodSeed_PairCountIsCapped(t *testing.T) {
	crossProduct := func(namespaces, names int) []string {
		refs := make([]string, 0, namespaces*names)
		for i := range namespaces {
			for j := range names {
				refs = append(refs, "n"+itoa(i)+"/p"+itoa(j))
			}
		}
		return refs
	}
	atCap := crossProduct(40, 40)
	require.Len(t, atCap, maxPodRootPairs)

	q := promqlfake.New(nil)
	_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootPod, atCap).Roots)
	require.NoError(t, err, "a pair set filling the cap exactly is accepted")
	assert.Len(t, q.QueriesFor(promql.QPVCBindings), 1, "the cross product is one pair chunk")

	over := append(slices.Clone(atCap), "n0/p40")
	for name, build := range map[string]func(*promqlfake.Querier) error{
		"storage": func(q *promqlfake.Querier) error {
			_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
				vlrScope(t, graph.StorageRootPod, over).Roots)
			return err
		},
		"k8s": func(q *promqlfake.Querier) error {
			_, err := New(q, Options{}, nil, nil).BuildK8s(t.Context(), time.Minute, vlrEnd, vlrSel,
				k8sRootsOf(t, graph.StorageRootPod, over...))
			return err
		},
	} {
		q := promqlfake.New(nil)
		require.Equal(t, ReasonInvalidScope, AsReason(build(q)), name)
		assert.Empty(t, q.Issued(), "%s: rejected before any query", name)
	}
}

// assertPodSeedChunksBounded checks every pod-restricted query: neither of its
// two alternations holds more than MaxAlternationValues values, and the
// restriction (both alternations and their wrappers) fits the default byte
// budget.
func assertPodSeedChunksBounded(t *testing.T, queries []string) {
	t.Helper()
	restriction := regexp.MustCompile(`namespace=~?"([^"]*)",pod=~?"([^"]*)"`)
	for _, query := range queries {
		m := restriction.FindStringSubmatch(query)
		require.NotNil(t, m, "a pod-restricted query: %.120s", query)
		assert.LessOrEqual(t, len(strings.Split(m[1], "|")), promql.MaxAlternationValues, "namespace values")
		assert.LessOrEqual(t, len(strings.Split(m[2], "|")), promql.MaxAlternationValues, "pod values")
		assert.LessOrEqual(t, len(m[0]), DefaultQoSScopeBatchBytes, "restriction bytes")
	}
}
