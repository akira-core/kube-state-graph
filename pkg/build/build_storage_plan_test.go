package build

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// planKSM is one kube-state-metrics / kubelet / ALERTS series of planEstate:
// cluster c1 in zone-a / prod, plus the given label pairs.
func planKSM(pairs ...string) *model.Sample {
	m := model.Metric{"cluster": "c1", "az": "zone-a", "env": "prod"}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[model.LabelName(pairs[i])] = model.LabelValue(pairs[i+1])
	}
	return &model.Sample{Metric: m, Value: 1}
}

// planHarvest is one Harvest series: no Kubernetes cluster, no az / env.
func planHarvest(pairs ...string) *model.Sample {
	m := model.Metric{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[model.LabelName(pairs[i])] = model.LabelValue(pairs[i+1])
	}
	return &model.Sample{Metric: m, Value: 1}
}

func withValue(s *model.Sample, v float64) *model.Sample {
	s.Value = model.SampleValue(v)
	return s
}

// planEstate carries every family either read plan issues, including the
// shapes the storage plan must not be fooled by: a pod root that mounts no
// claim (shop/web-0), a claimless pod sharing a mounting pod's name in another
// namespace (platform/orders-0), a pod that mounts nothing (shop/api-0), a
// claim no FlexVol backs (platform/cache-data), a Service with an endpoint
// slice, and container info on every pod.
func planEstate() map[promql.Query]model.Vector {
	type pod struct{ ns, name, uid, node, ownerKind, owner string }
	pods := []pod{
		{"shop", "orders-0", "uid-o0", "worker-1", "StatefulSet", "orders"},
		{"shop", "orders-1", "uid-o1", "worker-2", "StatefulSet", "orders"},
		{"shop", "web-0", "uid-w0", "worker-1", "ReplicaSet", "web-7d9"},
		{"shop", "api-0", "uid-a0", "worker-2", "ReplicaSet", "api-5f6"},
		{"platform", "orders-0", "uid-p0", "worker-2", "ReplicaSet", "orders-6c4"},
		{"platform", "redis-0", "uid-r0", "worker-1", "StatefulSet", "redis"},
		{"platform", "cache-0", "uid-c0", "worker-2", "StatefulSet", "cache"},
	}
	podInfo := make(model.Vector, 0, len(pods))
	podOwner := make(model.Vector, 0, len(pods))
	containers := make(model.Vector, 0, len(pods))
	for _, p := range pods {
		podInfo = append(podInfo, planKSM("namespace", p.ns, "pod", p.name, "uid", p.uid, "node", p.node))
		podOwner = append(podOwner, planKSM("namespace", p.ns, "pod", p.name,
			"owner_kind", p.ownerKind, "owner_name", p.owner, "owner_is_controller", "true"))
		containers = append(containers, withValue(planKSM("namespace", p.ns, "pod", p.name, "uid", p.uid,
			"container", "app", "image", "reg/"+p.name+":1"), 100))
	}
	const tracking = "annotation_argocd_argoproj_io_tracking_id"
	return map[promql.Query]model.Vector{
		promql.QPodInfo:          podInfo,
		promql.QPodOwner:         podOwner,
		promql.QPodContainerInfo: containers,
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-7d9", "owner_kind", "Deployment", "owner_name", "web"),
			planKSM("namespace", "shop", "replicaset", "api-5f6", "owner_kind", "Deployment", "owner_name", "api"),
			planKSM("namespace", "platform", "replicaset", "orders-6c4", "owner_kind", "Deployment", "owner_name", "orders"),
		},
		promql.QStatefulSetAnnotations: {
			planKSM("namespace", "shop", "statefulset", "orders", tracking, "shop-orders:apps/StatefulSet:shop/orders"),
			planKSM("namespace", "platform", "statefulset", "redis", tracking, "platform-redis:apps/StatefulSet:platform/redis"),
		},
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", tracking, "storefront:apps/Deployment:shop/web"),
		},
		promql.QNodeInfo: {planKSM("node", "worker-1"), planKSM("node", "worker-2")},
		promql.QNodeAddresses: {
			planKSM("node", "worker-1", "type", "InternalIP", "address", "192.168.0.1"),
			planKSM("node", "worker-2", "type", "InternalIP", "address", "192.168.0.2"),
		},
		promql.QNodeStatusCondition: {
			planKSM("node", "worker-1", "condition", "Ready", "status", "true"),
			planKSM("node", "worker-2", "condition", "Ready", "status", "true"),
		},
		promql.QPVCBindings: {
			planKSM("namespace", "shop", "pod", "orders-0", "persistentvolumeclaim", "orders-data", "volume", "data"),
			planKSM("namespace", "shop", "pod", "orders-1", "persistentvolumeclaim", "orders-data", "volume", "data"),
			planKSM("namespace", "platform", "pod", "redis-0", "persistentvolumeclaim", "redis-data", "volume", "data"),
			planKSM("namespace", "platform", "pod", "cache-0", "persistentvolumeclaim", "cache-data", "volume", "data"),
		},
		promql.QPVCInfo: {
			planKSM("namespace", "shop", "persistentvolumeclaim", "orders-data", "volumename", "pvc-orders", "storageclass", "netapp-nas"),
			planKSM("namespace", "platform", "persistentvolumeclaim", "redis-data", "volumename", "pvc-redis", "storageclass", "netapp-nas"),
			planKSM("namespace", "platform", "persistentvolumeclaim", "cache-data", "volumename", "pvc-cache", "storageclass", "standard"),
		},
		promql.QServiceInfo: {planKSM("namespace", "shop", "service", "orders", "cluster_ip", "10.96.0.10")},
		promql.QEndpointSliceEndpoints: {planKSM("namespace", "shop", "endpointslice", "orders-x1",
			"address", "10.1.0.1", "targetref_kind", "Pod", "targetref_name", "orders-0", "targetref_namespace", "shop")},
		promql.QEndpointSliceLabels: {planKSM("namespace", "shop", "endpointslice", "orders-x1",
			"label_kubernetes_io_service_name", "orders")},
		promql.QServiceAnnotations: {planKSM("namespace", "shop", "service", "orders", tracking, "shop-orders:/Service:shop/orders")},
		promql.QKubeletVolumeUsedBytes: {
			withValue(planKSM("namespace", "shop", "persistentvolumeclaim", "orders-data"), 4096),
		},
		promql.QKubeletVolumeCapacityBytes: {
			withValue(planKSM("namespace", "shop", "persistentvolumeclaim", "orders-data"), 8192),
		},
		promql.QVolumeLabels: {
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "aggr", "aggr1", "svm", "svm_shop", "volume", "trident_pvc_orders"),
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-02", "aggr", "aggr2", "svm", "svm_platform", "volume", "trident_pvc_redis"),
		},
		promql.QQoSReadOps: {
			withValue(planHarvest("cluster", "ontap-prod", "svm", "svm_shop", "volume", "trident_pvc_orders"), 300),
		},
		promql.QAggrStatus: {
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "aggr", "aggr1"),
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-02", "aggr", "aggr2"),
		},
		promql.QNetAppNodeStatus: {
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01"),
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-02"),
		},
		promql.QAlerts: {
			planKSM("alertname", "PodWarning", "alertstate", "firing", "severity", "warning",
				"namespace", "shop", "pod", "orders-0"),
			planHarvest("alertname", "AggrFilling", "alertstate", "firing", "severity", "warning",
				"cluster", "ontap-prod", "aggr", "aggr1", "az", "zone-a", "env", "prod"),
		},
	}
}

func planBodyJSON(t *testing.T, body cytoscape.Body) string {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return string(raw)
}

// stripContainers removes the ONE attribute the storage plan is allowed to
// change: data.containers, which the storage body no longer carries.
func stripContainers(body *cytoscape.Body) (stripped int) {
	for i := range body.Elements.Nodes {
		if len(body.Elements.Nodes[i].Data.Containers) > 0 {
			stripped++
		}
		body.Elements.Nodes[i].Data.Containers = nil
	}
	return stripped
}

// TestBuildStorage_PlanIsOutputPreserving pins the storage read plan's whole
// claim: skipping the five families the body cannot carry and reading pods by
// reference changes what is fetched, never what is drawn — except that no pod
// carries data.containers. Each root shape is built once through the full
// /v1/graph read and once through the storage plan, over a fake that applies
// every rendered matcher the way the upstream would.
func TestBuildStorage_PlanIsOutputPreserving(t *testing.T) {
	cases := map[string]struct {
		namespaces []string
		scope      func() (graph.StorageScope, error)
	}{
		"no root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, nil)
		}},
		"claimless pod root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/web-0"})
		}},
		"mounting pod root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/orders-0"})
		}},
		"pod roots in two namespaces": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/orders-0", "platform/redis-0"})
		}},
		"aggregate root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, []string{"aggr1"}, nil, nil)
		}},
		"roots on both sides": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, []string{"aggr1"}, nil, []string{"shop/orders-0"})
		}},
		"kubernetes node root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, []string{"worker-2"}, nil, nil, nil)
		}},
		"namespace filter": {namespaces: []string{"shop"}, scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, []string{"shop"}, nil, nil, nil, nil, nil)
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			scope, err := tc.scope()
			require.NoError(t, err)
			sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Namespace: tc.namespaces}
			end := time.Unix(1, 0).UTC()

			fullQ := promqlfake.New(planEstate())
			full, err := New(fullQ, Options{}, nil, nil).buildStorage(t.Context(), time.Minute, end, sel, fullPlan)
			require.NoError(t, err)
			scopedQ := promqlfake.New(planEstate())
			scoped, err := New(scopedQ, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, end, sel, scope.Roots)
			require.NoError(t, err)

			fullBody := cytoscape.Serialise(full, graph.ProjectStorage(full, scope))
			scopedBody := cytoscape.Serialise(scoped, graph.ProjectStorage(scoped, scope))
			require.NotEmpty(t, scopedBody.Elements.Nodes, "a vacuous body would prove nothing")

			// Positive control: the full read DOES carry containers, so the strip
			// below is load-bearing and equality after it proves containers are
			// the only difference.
			if stripContainers(&fullBody) == 0 {
				assert.NotEqual(t, "no root", name, "the full read must resolve containers for drawn pods")
			}
			assert.JSONEq(t, planBodyJSON(t, fullBody), planBodyJSON(t, scopedBody))

			// And the storage read really is narrower.
			assert.Empty(t, scopedQ.QueriesFor(promql.QPodContainerInfo))
			assert.Empty(t, scopedQ.QueriesFor(promql.QServiceInfo))
			assert.NotEmpty(t, fullQ.QueriesFor(promql.QPodContainerInfo))
		})
	}
}

// A pod name is unique per namespace only, so the name-scoped pod read admits a
// claimless pod that shares a mounting pod's name. It lies on no drawn path and
// is not a root, so the body does not change.
func TestBuildStorage_CrossNamespaceNameCollisionIsHarmless(t *testing.T) {
	const (
		ident    = "zone-a-prod-c1"
		collider = ident + "/uid-p0" // platform/orders-0, claimless
		mounter  = ident + "/uid-o0" // shop/orders-0, mounts shop/orders-data
		bystand  = ident + "/uid-a0" // shop/api-0, claimless, named by nothing
	)
	sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}
	g, err := New(promqlfake.New(planEstate()), Options{}, nil, nil).
		BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(), sel, graph.StorageRoots{})
	require.NoError(t, err)

	assert.Contains(t, g.NodesByID, collider, "its name matches a mounting pod's, so the scoped read fetches it")
	assert.NotContains(t, g.NodesByID, bystand, "a pod no binding and no root names is never read")

	body := cytoscape.Serialise(g, graph.ProjectStorage(g, graph.StorageScope{}))
	drawn := map[string]bool{}
	for _, n := range body.Elements.Nodes {
		drawn[n.Data.ID] = true
	}
	assert.True(t, drawn[mounter], "the mounting pod is drawn")
	assert.False(t, drawn[collider], "the collider lies on no path and is not a root")
}

// The storage plan's fan-out has four shapes — a pod scope or not, a matched
// FlexVol or not — and every one is pinned, together with the rule that a
// family the read did not issue has no RawSeriesCount entry at all.
func TestBuildStorage_FanOutLegCount(t *testing.T) {
	binding := sampleVec(model.Sample{Metric: model.Metric{
		"cluster": "c", "namespace": "db", "pod": "db-0", "persistentvolumeclaim": "data",
	}})
	claimInfo := sampleVec(model.Sample{Metric: model.Metric{
		"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data", "volumename": "pvc-9f3a",
	}})
	volume := sampleVec(model.Sample{Metric: model.Metric{
		"volume": "trident_pvc_9f3a", "cluster": "ontap-prod", "node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm0",
	}})

	cases := []struct {
		name                 string
		fixtures             map[promql.Query]model.Vector
		want                 int
		podScoped, qosScoped bool
	}{
		{"pod scope, no matched volume", map[promql.Query]model.Vector{promql.QPVCBindings: binding}, 32, true, false},
		{"pod scope and a matched volume", map[promql.Query]model.Vector{
			promql.QPVCBindings: binding, promql.QPVCInfo: claimInfo, promql.QVolumeLabels: volume,
		}, 38, true, true},
		{"empty pod scope, no matched volume", nil, 30, false, false},
		{"empty pod scope and a matched volume", map[promql.Query]model.Vector{
			promql.QPVCInfo: claimInfo, promql.QVolumeLabels: volume,
		}, 36, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := promqlfake.New(tc.fixtures)
			tp, err := readTopology(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
				Options{}, promql.Selector{}, storagePlan(graph.StorageRoots{}))
			require.NoError(t, err)

			seen := map[string]int{}
			for _, is := range f.Issued() {
				seen[is.Name]++
			}
			assert.Len(t, seen, tc.want, "distinct families issued")
			total := 0
			for _, n := range seen {
				total += n
			}
			assert.Equal(t, tc.want, total, "each family issued exactly once")
			assert.Len(t, tp.RawSeriesCount, tc.want, "one tally entry per issued family, none for the rest")

			for q := range storageSkippedLegs {
				assert.Zero(t, seen[string(q)], "%s is never read by the storage build", q)
				assert.NotContains(t, tp.RawSeriesCount, string(q), "a skipped family is absent, never 0")
			}
			for _, q := range promql.PodScopedQueries {
				assert.Equal(t, tc.podScoped, seen[string(q)] == 1, "%s issued iff the pod scope is non-empty", q)
				assert.Equal(t, tc.podScoped, tp.RawSeriesCount[string(q)] >= 0 && hasKey(tp.RawSeriesCount, string(q)), q)
			}
			for _, q := range promql.QoSWorkloadQueries {
				assert.Equal(t, tc.qosScoped, seen[string(q)] == 1, "%s issued iff a claim matched a FlexVol", q)
				assert.Equal(t, tc.qosScoped, hasKey(tp.RawSeriesCount, string(q)), q)
			}
		})
	}
}

func hasKey(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}
