package build

import (
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
