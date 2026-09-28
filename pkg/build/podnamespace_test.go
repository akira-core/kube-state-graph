package build

import (
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

// A pod is identified by (namespace, pod). Every read a storage build issues
// for known pods is keyed by that pair, one query per namespace, so a
// same-named pod in another namespace is never read — nor its node, nor its
// controllers, nor its claims.

func flatScope(q *promqlfake.Querier, name promql.Query, label string) []string {
	var out []string
	for _, vals := range q.ScopeValues(name, label) {
		out = append(out, vals...)
	}
	return out
}

// The pod wave reads the pods a storage body can draw by (namespace, pod).
func TestPodWave_SameNamedPodInAnotherNamespaceIsNotRead(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCBindings: {
			planKSM("namespace", "shop", "pod", "postgres-0", "persistentvolumeclaim", "data-postgres-0"),
			planKSM("namespace", "platform", "pod", "postgres-0", "persistentvolumeclaim", "data-postgres-0"),
		},
		promql.QPodInfo: {
			planKSM("namespace", "shop", "pod", "postgres-0", "uid", "u-shop", "node", "worker-1"),
			planKSM("namespace", "platform", "pod", "postgres-0", "uid", "u-platform", "node", "worker-9"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "shop", "pod", "postgres-0", "owner_kind", "StatefulSet", "owner_name", "postgres", "owner_is_controller", "true"),
			planKSM("namespace", "platform", "pod", "postgres-0", "owner_kind", "StatefulSet", "owner_name", "pg-other", "owner_is_controller", "true"),
		},
		promql.QNodeInfo: {
			planKSM("node", "worker-1"),
			planKSM("node", "worker-9"),
		},
	})
	_, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootPod, []string{"shop/postgres-0"}).Roots)
	require.NoError(t, err)

	for _, q := range promql.PodScopedQueries {
		issued := f.QueriesFor(q)
		require.NotEmpty(t, issued, q)
		for _, query := range issued {
			assert.Contains(t, query, `namespace="shop",pod="postgres-0"`, "%s is read by (namespace, pod)", q)
		}
	}
	assert.NotContains(t, flatScope(f, promql.QNodeInfo, "node"), "worker-9",
		"the other namespace's pod never reaches the node wave")
	assert.NotContains(t, strings.Join(f.QueriesFor(promql.QStatefulSetAnnotations), "\n"), "pg-other",
		"the other namespace's pod never reaches the controller wave")
}

// Pods from several namespaces are read one query per namespace, in
// namespace order, and the merged vectors hold exactly those pods.
func TestPodWave_OneQueryPerNamespace(t *testing.T) {
	bindings := model.Vector{
		planKSM("namespace", "shop", "pod", "orders-0", "persistentvolumeclaim", "shared"),
		planKSM("namespace", "shop", "pod", "web-0", "persistentvolumeclaim", "shared"),
		planKSM("namespace", "platform", "pod", "orders-0", "persistentvolumeclaim", "shared"),
	}
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPodInfo: {
			planKSM("namespace", "shop", "pod", "orders-0", "uid", "u1", "node", "n1"),
			planKSM("namespace", "shop", "pod", "web-0", "uid", "u2", "node", "n1"),
			planKSM("namespace", "platform", "pod", "orders-0", "uid", "u3", "node", "n2"),
			planKSM("namespace", "platform", "pod", "web-0", "uid", "u4", "node", "n3"),
		},
	})
	v, err := scopedPodVectors(t, f, Options{}, nil, bindings)
	require.NoError(t, err)

	assert.Equal(t, []string{
		`last_over_time(kube_pod_info{namespace="platform",pod="orders-0"}[1m])`,
		`last_over_time(kube_pod_info{namespace="shop",pod=~"orders-0|web-0"}[1m])`,
	}, sortedCopy(f.QueriesFor(promql.QPodInfo)))
	got := make([]string, 0, len(v.Pod))
	for _, s := range v.Pod {
		got = append(got, string(s.Metric["namespace"])+"/"+string(s.Metric["pod"]))
	}
	assert.Equal(t, []string{"platform/orders-0", "shop/orders-0", "shop/web-0"}, got,
		"merged in namespace order; platform/web-0 mounts nothing and is never read")
}

// The node seed's incarnation completion and its claim-binding read are keyed
// by the (namespace, pod) the first node read returned.
func TestNodeSeed_IncarnationAndBindingsAreNamespaced(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPodInfo: {
			planKSM("namespace", "shop", "pod", "web-0", "uid", "u-shop", "node", "worker-1"),
			planKSM("namespace", "platform", "pod", "web-0", "uid", "u-platform", "node", "worker-2"),
		},
		promql.QPVCBindings: {
			planKSM("namespace", "shop", "pod", "web-0", "persistentvolumeclaim", "web-data"),
			planKSM("namespace", "platform", "pod", "web-0", "persistentvolumeclaim", "other-data"),
		},
	})
	_, err := New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootNode, []string{"worker-1"}).Roots)
	require.NoError(t, err)

	info := f.QueriesFor(promql.QPodInfo)
	require.GreaterOrEqual(t, len(info), 2)
	assert.Contains(t, info[0], `node="worker-1"`, "the first pod read is the node seed")
	for _, query := range info[1:] {
		assert.Contains(t, query, `namespace="shop",pod="web-0"`, "incarnation completion and the pod wave are namespaced")
		assert.NotContains(t, query, `node=`)
	}
	for _, query := range f.QueriesFor(promql.QPVCBindings) {
		if strings.Contains(query, `pod=`) {
			assert.Contains(t, query, `namespace="shop",pod="web-0"`, "bindings by pod are namespaced")
		}
	}
	assert.NotContains(t, flatScope(f, promql.QPVCInfo, promql.ClaimLabel), "other-data")
}

// An application root tracks the claims of the recovered (namespace, pod)
// pairs only: a same-named pod in another namespace contributes no claim.
func TestApplicationSeed_SameNamedPodInAnotherNamespaceIsNotTracked(t *testing.T) {
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", trackingLabel, "checkout:apps/Deployment:shop/web"),
		},
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-1", "owner_kind", "Deployment", "owner_name", "web"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "shop", "pod", "web-0", "owner_kind", "ReplicaSet", "owner_name", "web-1", "owner_is_controller", "true"),
		},
		promql.QPVCBindings: {
			planKSM("namespace", "shop", "pod", "web-0", "persistentvolumeclaim", "orders-data"),
			planKSM("namespace", "platform", "pod", "web-0", "persistentvolumeclaim", "platform-data"),
		},
	})
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootApplication, []string{"checkout"})
	require.NoError(t, err)
	_, err = New(f, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)

	var byPod []string
	for _, query := range f.QueriesFor(promql.QPVCBindings) {
		if strings.Contains(query, `pod=`) {
			byPod = append(byPod, query)
		}
	}
	require.NotEmpty(t, byPod)
	for _, query := range byPod {
		assert.Contains(t, query, `namespace="shop",pod="web-0"`)
	}
	claims := flatScope(f, promql.QPVCInfo, promql.ClaimLabel)
	assert.Contains(t, claims, "orders-data")
	assert.NotContains(t, claims, "platform-data", "platform/web-0 is not a recovered pod")
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}
