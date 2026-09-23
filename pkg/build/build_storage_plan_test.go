package build

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
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

// planHarvest is one Harvest series: no Kubernetes cluster, stamped with the
// fixture zone (az / env) every Harvest series must carry
// (read-storage-roots-through-volume-hub D13); a pair overrides it.
func planHarvest(pairs ...string) *model.Sample {
	m := model.Metric{"az": "zone-a", "env": "prod"}
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
//
// It also carries every controller kind the by-reference controller wave must
// resolve identically to an unrestricted read
// (scope-controller-legs-by-reference): a DaemonSet-owned pod (shop/daemon-0),
// a bare ReplicaSet-owned pod with NO kube_replicaset_owner row at all
// (shop/rs-bare-0), a Job-owned pod whose Job carries its own tracking-id
// (shop/job-annotated-0), a Job-owned pod resolved through its CronJob because
// the Job itself carries none (shop/job-cronjob-0, via kube_job_owner to
// CronJob "nightly"), a pod with no controller owner at all
// (shop/ownerless-0), an unscheduled pod with no `node` label
// (shop/unscheduled-0), a third Kubernetes node no pod is scheduled on
// (worker-3, for a `node=` root with no mounting pod), and a StatefulSet named
// "orders" in `platform` — the same name as the `shop` StatefulSet, carrying a
// DIFFERENT tracking-id and owning no pod at all — to prove a same-named
// controller in another namespace is inert noise.
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
		{"shop", "daemon-0", "uid-d0", "worker-1", "DaemonSet", "logger"},
		{"shop", "rs-bare-0", "uid-rb0", "worker-2", "ReplicaSet", "rs-bare-x1"},
		{"shop", "job-annotated-0", "uid-ja0", "worker-1", "Job", "batch-1"},
		{"shop", "job-cronjob-0", "uid-jc0", "worker-2", "Job", "nightly-28901"},
		{"shop", "ownerless-0", "uid-oe0", "worker-1", "", ""},
		{"shop", "unscheduled-0", "uid-us0", "", "StatefulSet", "orders"},
		// Application root `beta`. Claimless pods are invisible to a storage
		// read that does not recover them; mounting pods exercise the narrowed
		// binding scope (own annotation, RWX split, inheritance).
		{"shop", "beta-web-abc", "uid-bw", "worker-1", "ReplicaSet", "beta-web-7d9f"},
		{"shop", "beta-nightly-x", "uid-bn", "worker-2", "Job", "beta-nightly-28901"},
		{"shop", "beta-db-0", "uid-bd", "worker-1", "StatefulSet", "beta-db"},
		{"shop", "two-id-0", "uid-ti", "worker-1", "StatefulSet", "two-id"},
		{"shop", "foreign-0", "uid-fo", "worker-1", "ReplicaSet", "foreign-rs"},
		{"shop", "beta-rwx-0", "uid-br", "worker-1", "StatefulSet", "beta-rwx"},
		{"shop", "alpha-share-0", "uid-as", "worker-2", "StatefulSet", "alpha-share"},
		{"shop", "beta-inh-0", "uid-bi", "worker-1", "StatefulSet", "beta-inh"},
		{"shop", "zeta-0", "uid-ze", "worker-2", "StatefulSet", "zeta"},
	}
	podInfo := make(model.Vector, 0, len(pods))
	podOwner := make(model.Vector, 0, len(pods))
	containers := make(model.Vector, 0, len(pods))
	for _, p := range pods {
		podInfo = append(podInfo, planKSM("namespace", p.ns, "pod", p.name, "uid", p.uid, "node", p.node))
		if p.ownerKind != "" {
			podOwner = append(podOwner, planKSM("namespace", p.ns, "pod", p.name,
				"owner_kind", p.ownerKind, "owner_name", p.owner, "owner_is_controller", "true"))
		}
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
			planKSM("namespace", "shop", "replicaset", "beta-web-7d9f", "owner_kind", "Deployment", "owner_name", "beta-web"),
			planKSM("namespace", "shop", "replicaset", "foreign-rs", "owner_kind", "Deployment", "owner_name", "foreign"),
			// rs-bare-x1 deliberately has NO row here: a bare ReplicaSet with no
			// Deployment owner of its own.
		},
		promql.QStatefulSetAnnotations: {
			planKSM("namespace", "shop", "statefulset", "orders", tracking, "shop-orders:apps/StatefulSet:shop/orders"),
			planKSM("namespace", "platform", "statefulset", "redis", tracking, "platform-redis:apps/StatefulSet:platform/redis"),
			// Noise: same name as the shop StatefulSet, different namespace,
			// different tracking-id, owns no pod at all.
			planKSM("namespace", "platform", "statefulset", "orders", tracking, "cross-ns-orders:apps/StatefulSet:platform/orders"),
			planKSM("namespace", "shop", "statefulset", "beta-db", tracking, "beta:apps/StatefulSet:shop/beta-db"),
			planKSM("namespace", "shop", "statefulset", "two-id", tracking, "alpha:apps/StatefulSet:shop/two-id"),
			planKSM("namespace", "shop", "statefulset", "two-id", tracking, "beta:apps/StatefulSet:shop/two-id"),
			planKSM("namespace", "shop", "statefulset", "beta-rwx", tracking, "beta:apps/StatefulSet:shop/beta-rwx"),
			planKSM("namespace", "shop", "statefulset", "alpha-share", tracking, "alpha:apps/StatefulSet:shop/alpha-share"),
			planKSM("namespace", "shop", "statefulset", "beta-inh", tracking, "beta:apps/StatefulSet:shop/beta-inh"),
			planKSM("namespace", "shop", "statefulset", "zeta", tracking, "zeta:apps/StatefulSet:shop/zeta"),
		},
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", tracking, "storefront:apps/Deployment:shop/web"),
			planKSM("namespace", "shop", "deployment", "beta-web", tracking, "beta:apps/Deployment:shop/beta-web"),
			planKSM("namespace", "shop", "deployment", "foreign", tracking, "ledger:apps/Deployment:shop/foreign"),
		},
		promql.QDaemonSetAnnotations: {
			planKSM("namespace", "shop", "daemonset", "logger", tracking, "observability:apps/DaemonSet:shop/logger"),
		},
		promql.QJobAnnotations: {
			planKSM("namespace", "shop", "job_name", "batch-1", tracking, "batch-app:batch/Job:shop/batch-1"),
		},
		promql.QJobOwner: {
			planKSM("namespace", "shop", "job_name", "nightly-28901",
				"owner_kind", "CronJob", "owner_name", "nightly", "owner_is_controller", "true"),
			planKSM("namespace", "shop", "job_name", "beta-nightly-28901",
				"owner_kind", "CronJob", "owner_name", "beta-nightly", "owner_is_controller", "true"),
		},
		promql.QCronJobAnnotations: {
			planKSM("namespace", "shop", "cronjob", "nightly", tracking, "reports:batch/CronJob:shop/nightly"),
			planKSM("namespace", "shop", "cronjob", "beta-nightly", tracking, "beta:batch/CronJob:shop/beta-nightly"),
		},
		promql.QNodeInfo: {planKSM("node", "worker-1"), planKSM("node", "worker-2"), planKSM("node", "worker-3")},
		promql.QNodeAddresses: {
			planKSM("node", "worker-1", "type", "InternalIP", "address", "192.168.0.1"),
			planKSM("node", "worker-2", "type", "InternalIP", "address", "192.168.0.2"),
			planKSM("node", "worker-3", "type", "InternalIP", "address", "192.168.0.3"),
		},
		promql.QNodeStatusCondition: {
			planKSM("node", "worker-1", "condition", "Ready", "status", "true"),
			planKSM("node", "worker-2", "condition", "Ready", "status", "true"),
			planKSM("node", "worker-3", "condition", "Ready", "status", "true"),
		},
		promql.QPVCBindings: {
			planKSM("namespace", "shop", "pod", "orders-0", "persistentvolumeclaim", "orders-data", "volume", "data"),
			planKSM("namespace", "shop", "pod", "orders-1", "persistentvolumeclaim", "orders-data", "volume", "data"),
			planKSM("namespace", "platform", "pod", "redis-0", "persistentvolumeclaim", "redis-data", "volume", "data"),
			planKSM("namespace", "platform", "pod", "cache-0", "persistentvolumeclaim", "cache-data", "volume", "data"),
			planKSM("namespace", "shop", "pod", "beta-db-0", "persistentvolumeclaim", "beta-db-data", "volume", "data"),
			planKSM("namespace", "shop", "pod", "foreign-0", "persistentvolumeclaim", "foreign-data", "volume", "data"),
			planKSM("namespace", "shop", "pod", "beta-rwx-0", "persistentvolumeclaim", "shared-beta", "volume", "data"),
			planKSM("namespace", "shop", "pod", "alpha-share-0", "persistentvolumeclaim", "shared-beta", "volume", "data"),
			planKSM("namespace", "shop", "pod", "beta-inh-0", "persistentvolumeclaim", "inherit-beta", "volume", "data"),
			planKSM("namespace", "shop", "pod", "zeta-0", "persistentvolumeclaim", "inherit-beta", "volume", "data"),
		},
		promql.QPVCAnnotations: {
			planKSM("namespace", "shop", "persistentvolumeclaim", "foreign-data", tracking, "beta:apps/PersistentVolumeClaim:shop/foreign-data"),
		},
		promql.QPVCInfo: {
			planKSM("namespace", "shop", "persistentvolumeclaim", "orders-data", "volumename", "pvc-orders", "storageclass", "netapp-nas"),
			planKSM("namespace", "platform", "persistentvolumeclaim", "redis-data", "volumename", "pvc-redis", "storageclass", "netapp-nas"),
			planKSM("namespace", "platform", "persistentvolumeclaim", "cache-data", "volumename", "pvc-cache", "storageclass", "standard"),
			planKSM("namespace", "shop", "persistentvolumeclaim", "beta-db-data", "volumename", "pvc-betadb", "storageclass", "netapp-nas"),
			planKSM("namespace", "shop", "persistentvolumeclaim", "foreign-data", "volumename", "pvc-foreign", "storageclass", "netapp-nas"),
			planKSM("namespace", "shop", "persistentvolumeclaim", "shared-beta", "volumename", "pvc-sharedbeta", "storageclass", "netapp-nas"),
			planKSM("namespace", "shop", "persistentvolumeclaim", "inherit-beta", "volumename", "pvc-inheritbeta", "storageclass", "netapp-nas"),
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
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "aggr", "aggr1", "svm", "svm_shop", "volume", "trident_pvc_betadb"),
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "aggr", "aggr1", "svm", "svm_shop", "volume", "trident_pvc_foreign"),
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "aggr", "aggr1", "svm", "svm_shop", "volume", "trident_pvc_sharedbeta"),
			planHarvest("cluster", "ontap-prod", "node", "ontap-prod-02", "aggr", "aggr2", "svm", "svm_shop", "volume", "trident_pvc_inheritbeta"),
		},
		promql.QQoSReadOps: {
			withValue(planHarvest("cluster", "ontap-prod", "svm", "svm_shop", "volume", "trident_pvc_orders"), 300),
			withValue(planHarvest("cluster", "ontap-prod", "svm", "svm_shop", "volume", "trident_pvc_sharedbeta"), 200),
			withValue(planHarvest("cluster", "ontap-prod", "svm", "svm_shop", "volume", "trident_pvc_inheritbeta"), 80),
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
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, nil, nil)
		}},
		"claimless pod root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/web-0"}, nil)
		}},
		"mounting pod root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/orders-0"}, nil)
		}},
		"pod roots in two namespaces": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/orders-0", "platform/redis-0"}, nil)
		}},
		"aggregate root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, []string{"aggr1"}, nil, nil, nil)
		}},
		"roots on both sides": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, []string{"aggr1"}, nil, []string{"shop/orders-0"}, nil)
		}},
		"kubernetes node root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, []string{"worker-2"}, nil, nil, nil, nil)
		}},
		"kubernetes node root with no mounting pod": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, []string{"worker-3"}, nil, nil, nil, nil)
		}},
		"daemonset-owned pod root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/daemon-0"}, nil)
		}},
		"bare replicaset-owned pod root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/rs-bare-0"}, nil)
		}},
		"job with its own annotation, pod root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/job-annotated-0"}, nil)
		}},
		"job resolved through its cronjob, pod root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/job-cronjob-0"}, nil)
		}},
		"ownerless pod root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/ownerless-0"}, nil)
		}},
		"unscheduled pod root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/unscheduled-0"}, nil)
		}},
		"namespace filter": {namespaces: []string{"shop"}, scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, []string{"shop"}, nil, nil, nil, nil, nil, nil)
		}},
		"application root": {scope: func() (graph.StorageScope, error) {
			return graph.NewStorageScope(nil, nil, nil, nil, nil, nil, nil, []string{"beta"})
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

// An over-admitted pod — stage 1 matched a tracking-id the forward resolver
// does not pick — is loaded and then dropped. The body matches an estate
// whose controller carries only the winning tracking-id.
func TestBuildStorage_OverAdmittedPodIsDropped(t *testing.T) {
	scope, err := graph.NewStorageScope(nil, nil, nil, nil, nil, nil, nil, []string{"beta"})
	require.NoError(t, err)
	sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}
	end := time.Unix(1, 0).UTC()

	over := promqlfake.New(planEstate())
	overG, err := New(over, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, end, sel, scope.Roots)
	require.NoError(t, err)

	loaded := false
	for _, chunk := range over.ScopeValues(promql.QPodInfo, "pod") {
		if slices.Contains(chunk, "two-id-0") {
			loaded = true
		}
	}
	assert.True(t, loaded, "kube_pod_info is restricted to the over-admitted pod's name")

	onlyAlpha := maps.Clone(planEstate())
	var kept model.Vector
	for _, s := range onlyAlpha[promql.QStatefulSetAnnotations] {
		if s.Metric["statefulset"] == "two-id" && strings.HasPrefix(string(s.Metric[trackingLabel]), "beta:") {
			continue
		}
		kept = append(kept, s)
	}
	onlyAlpha[promql.QStatefulSetAnnotations] = kept
	alphaG, err := New(promqlfake.New(onlyAlpha), Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, end, sel, scope.Roots)
	require.NoError(t, err)

	overBody := cytoscape.Serialise(overG, graph.ProjectStorage(overG, scope))
	alphaBody := cytoscape.Serialise(alphaG, graph.ProjectStorage(alphaG, scope))
	assert.JSONEq(t, planBodyJSON(t, alphaBody), planBodyJSON(t, overBody))
	for _, n := range overBody.Elements.Nodes {
		assert.NotEqual(t, "zone-a-prod-c1/uid-ti", n.Data.ID)
	}
}

// Stage 1's series and the by-reference read of the same family add under one
// tally key. A family only the recovery issued is present even at zero.
func TestBuildStorage_RecoveryTalliedUnderFamilyName(t *testing.T) {
	const tracking = trackingLabel
	f := promqlfake.New(map[promql.Query]model.Vector{
		promql.QDeploymentAnnotations: {
			planKSM("namespace", "shop", "deployment", "web", tracking, "checkout:b"),
			planKSM("namespace", "shop", "deployment", "web", tracking, "checkout:a"),
			planKSM("namespace", "shop", "deployment", "web", tracking, "aaa:apps/Deployment:shop/web"),
		},
		promql.QReplicaSetOwner: {
			planKSM("namespace", "shop", "replicaset", "web-7d9f", "owner_kind", "Deployment", "owner_name", "web"),
		},
		promql.QPodOwner: {
			planKSM("namespace", "shop", "pod", "web-7d9f-abc", "owner_kind", "ReplicaSet", "owner_name", "web-7d9f", "owner_is_controller", "true"),
		},
		promql.QPodInfo: {
			planKSM("namespace", "shop", "pod", "web-7d9f-abc", "uid", "uid-web", "node", "n1"),
		},
	})
	roots := graph.StorageRoots{Applications: map[string]struct{}{"checkout": {}}}
	tp, err := readTopology(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{}, storagePlan(roots))
	require.NoError(t, err)
	assert.Equal(t, 5, tp.RawSeriesCount["kube_deployment_annotations"], "stage-1 2 + by-reference 3")
	n, ok := tp.RawSeriesCount["kube_daemonset_annotations"]
	assert.True(t, ok, "a family only the recovery issued is present")
	assert.Zero(t, n)
}

// storageUnrestrictedLegs are the eighteen families the storage plan's first
// wave issues under EVERY shape (design.md D6 / D8): the claim families (the
// scope root plus the three that co-move with it), the Harvest inventory, and
// ALERTS. Every other first-wave family the fullPlan issues (kube_pod_info,
// kube_pod_owner, the four kube_node_* families, the eight controller-owner /
// controller-annotation families) moved to a by-reference wave and is present
// here iff this build's pods, nodes or controllers named it.
var storageUnrestrictedLegs = []promql.Query{
	promql.QPVCBindings, promql.QPVCInfo, promql.QVolumeLabels, promql.QPVCAnnotations,
	promql.QQoSPolicyFixedMaxIOPS, promql.QQoSPolicyFixedMaxMBps,
	promql.QAggrStatus, promql.QAggrSpaceUsed, promql.QAggrSpaceTotal,
	promql.QNetAppNodeStatus, promql.QNetAppNodeLabels, promql.QNetAppNodeCPUBusy,
	promql.QNetAppNodeTotalOps, promql.QNetAppNodeTotalLatency, promql.QNetAppNodeTotalData,
	promql.QAlerts, promql.QKubeletVolumeUsedBytes, promql.QKubeletVolumeCapacityBytes,
}

// The storage plan's fan-out is 18 first-wave legs plus a per-build addition
// for each by-reference family a loaded pod, node or owner actually named
// (design.md D6). Every case pins the EXACT set of families issued, not just
// a count, so a family added to the wrong wave — or omitted from one it
// belongs in — fails here even when the total happens to match.
func TestBuildStorage_FanOutLegCount(t *testing.T) {
	require.Len(t, storageUnrestrictedLegs, 18, "design D6: the fixed first wave")

	binding := func(ns, pod string) model.Vector {
		return sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": model.LabelValue(ns), "pod": model.LabelValue(pod), "persistentvolumeclaim": "data-" + model.LabelValue(pod),
		}})
	}
	podInfo := func(ns, pod, node string) model.Vector {
		return sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": model.LabelValue(ns), "pod": model.LabelValue(pod), "uid": "u-" + model.LabelValue(pod), "node": model.LabelValue(node),
		}})
	}
	owner := func(ns, pod, kind, name string) model.Vector {
		return sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": model.LabelValue(ns), "pod": model.LabelValue(pod),
			"owner_kind": model.LabelValue(kind), "owner_name": model.LabelValue(name), "owner_is_controller": "true",
		}})
	}
	merge := func(vs ...model.Vector) model.Vector {
		var out model.Vector
		for _, v := range vs {
			out = append(out, v...)
		}
		return out
	}
	claimInfo := sampleVec(model.Sample{Metric: model.Metric{
		"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data-db-0", "volumename": "pvc-9f3a",
	}})
	volume := sampleVec(model.Sample{Metric: model.Metric{
		"volume": "trident_pvc_9f3a", "cluster": "ontap-prod", "node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm0",
	}})

	cases := []struct {
		name        string
		fixtures    map[promql.Query]model.Vector
		roots       graph.StorageRoots
		byReference []promql.Query // by-reference families this build must issue
		matchVolume bool           // also load a claim that matches a FlexVol: +6 QoS legs
		want        int
	}{
		{"empty scope, no roots", nil, graph.StorageRoots{}, nil, false, 18},
		{"node root alone, no mounting pod", nil,
			graph.StorageRoots{Nodes: map[string]struct{}{"n9": {}}}, promql.NodeScopedQueries, false, 22},
		{"one statefulset-owned scheduled pod", map[promql.Query]model.Vector{
			promql.QPVCBindings: binding("db", "db-0"),
			promql.QPodInfo:     podInfo("db", "db-0", "n1"),
			promql.QPodOwner:    owner("db", "db-0", "StatefulSet", "orders"),
		}, graph.StorageRoots{},
			slices.Concat(promql.PodScopedQueries, promql.NodeScopedQueries, []promql.Query{promql.QStatefulSetAnnotations}),
			false, 25},
		{"one deployment-owned pod via replicaset", map[promql.Query]model.Vector{
			promql.QPVCBindings: binding("db", "db-0"),
			promql.QPodInfo:     podInfo("db", "db-0", "n1"),
			promql.QPodOwner:    owner("db", "db-0", "ReplicaSet", "web-7d9"),
			promql.QReplicaSetOwner: sampleVec(model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "replicaset": "web-7d9", "owner_kind": "Deployment", "owner_name": "web",
			}}),
		}, graph.StorageRoots{},
			slices.Concat(promql.PodScopedQueries, promql.NodeScopedQueries,
				[]promql.Query{promql.QReplicaSetOwner, promql.QReplicaSetAnnotations, promql.QDeploymentAnnotations}),
			false, 27},
		{"one cronjob-owned pod via job", map[promql.Query]model.Vector{
			promql.QPVCBindings: binding("db", "db-0"),
			promql.QPodInfo:     podInfo("db", "db-0", "n1"),
			promql.QPodOwner:    owner("db", "db-0", "Job", "nightly-28901"),
			promql.QJobOwner: sampleVec(model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "job_name": "nightly-28901",
				"owner_kind": "CronJob", "owner_name": "nightly", "owner_is_controller": "true",
			}}),
		}, graph.StorageRoots{},
			slices.Concat(promql.PodScopedQueries, promql.NodeScopedQueries,
				[]promql.Query{promql.QJobOwner, promql.QJobAnnotations, promql.QCronJobAnnotations}),
			false, 27},
		{"every controller kind present", map[promql.Query]model.Vector{
			promql.QPVCBindings: merge(binding("db", "sts-0"), binding("db", "ds-0"), binding("db", "dep-0"),
				binding("db", "rs-0"), binding("db", "job-0"), binding("db", "cj-0")),
			promql.QPodInfo: merge(podInfo("db", "sts-0", "n1"), podInfo("db", "ds-0", "n1"), podInfo("db", "dep-0", "n1"),
				podInfo("db", "rs-0", "n1"), podInfo("db", "job-0", "n1"), podInfo("db", "cj-0", "n1")),
			promql.QPodOwner: merge(
				owner("db", "sts-0", "StatefulSet", "orders"),
				owner("db", "ds-0", "DaemonSet", "logger"),
				owner("db", "dep-0", "ReplicaSet", "web-7d9"),
				owner("db", "rs-0", "ReplicaSet", "rs-bare"),
				owner("db", "job-0", "Job", "batch-1"),
				owner("db", "cj-0", "Job", "nightly-28901"),
			),
			promql.QReplicaSetOwner: sampleVec(model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "replicaset": "web-7d9", "owner_kind": "Deployment", "owner_name": "web",
			}}),
			promql.QJobOwner: sampleVec(model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "job_name": "nightly-28901",
				"owner_kind": "CronJob", "owner_name": "nightly", "owner_is_controller": "true",
			}}),
		}, graph.StorageRoots{},
			slices.Concat(promql.PodScopedQueries, promql.NodeScopedQueries, promql.ControllerScopedQueries),
			false, 32},
		{"every controller kind present, and a matched volume", nil /* filled below */, graph.StorageRoots{},
			slices.Concat(promql.PodScopedQueries, promql.NodeScopedQueries, promql.ControllerScopedQueries),
			true, 38},
	}
	// The last case reuses the richest fixture with the volume join added.
	cases[len(cases)-1].fixtures = merge2(cases[5].fixtures, map[promql.Query]model.Vector{
		promql.QPVCInfo: claimInfo, promql.QVolumeLabels: volume,
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := promqlfake.New(tc.fixtures)
			tp, err := readTopology(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
				Options{}, promql.Selector{}, storagePlan(tc.roots))
			require.NoError(t, err)

			seen := map[string]int{}
			for _, is := range f.Issued() {
				seen[is.Name]++
			}
			want := make(map[string]bool, tc.want)
			for _, q := range storageUnrestrictedLegs {
				want[string(q)] = true
			}
			for _, q := range tc.byReference {
				want[string(q)] = true
			}
			if tc.matchVolume {
				for _, q := range promql.QoSWorkloadQueries {
					want[string(q)] = true
				}
			}
			assert.Len(t, want, tc.want, "expected-set size must match the design D6 pin")
			assert.Len(t, seen, tc.want, "distinct families issued")
			for name := range want {
				assert.Equal(t, 1, seen[name], "%s issued exactly once", name)
				assert.Contains(t, tp.RawSeriesCount, name, "%s tallied", name)
			}
			for name := range seen {
				assert.True(t, want[name], "unexpected family issued: %s", name)
			}

			for q := range storageSkippedLegs {
				assert.Zero(t, seen[string(q)], "%s is never read by the storage build", q)
				assert.NotContains(t, tp.RawSeriesCount, string(q), "a skipped family is absent, never 0")
			}
			for _, q := range promql.ReferenceScopedQueries {
				if want[string(q)] {
					continue
				}
				assert.NotContains(t, tp.RawSeriesCount, string(q), "%s: a family this case did not name is absent, never 0", q)
			}
		})
	}
}

// merge2 returns a new map combining a and b, with b's entries winning on a
// key collision. Used only to derive the richest fan-out case's
// matched-volume variant from its base fixture without mutating it.
func merge2(a, b map[promql.Query]model.Vector) map[promql.Query]model.Vector {
	out := make(map[promql.Query]model.Vector, len(a)+len(b))
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

// Application rows of the fan-out pin (design D11). Rows without application=
// stay in TestBuildStorage_FanOutLegCount. A recovered family is read again
// by the forward wave, so the pin is a per-family query count.
func TestBuildStorage_FanOutLegCount_Application(t *testing.T) {
	const tracking = trackingLabel
	ann := []promql.Query{
		promql.QDeploymentAnnotations, promql.QStatefulSetAnnotations, promql.QDaemonSetAnnotations,
		promql.QCronJobAnnotations, promql.QReplicaSetAnnotations, promql.QJobAnnotations,
	}
	base := func(qs ...promql.Query) map[string]int {
		m := map[string]int{}
		for _, q := range storageUnrestrictedLegs {
			m[string(q)] = 1
		}
		for _, q := range qs {
			m[string(q)]++
		}
		return m
	}
	app := func(name string) graph.StorageRoots {
		return graph.StorageRoots{Applications: map[string]struct{}{name: {}}}
	}
	track := func(label, name, id string) model.Vector {
		return sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", model.LabelName(label): model.LabelValue(name),
			tracking: model.LabelValue("checkout:apps/" + id + ":db/" + name),
		}})
	}

	deployPod := base(
		promql.QPodInfo,
		promql.QPodOwner, promql.QPodOwner, promql.QPodOwner,
		promql.QNodeInfo, promql.QNodeAddresses, promql.QNodeLabels, promql.QNodeStatusCondition,
		promql.QReplicaSetOwner, promql.QReplicaSetOwner,
		promql.QReplicaSetAnnotations, promql.QReplicaSetAnnotations,
		promql.QDeploymentAnnotations, promql.QDeploymentAnnotations,
		promql.QStatefulSetAnnotations, promql.QDaemonSetAnnotations, promql.QJobAnnotations, promql.QCronJobAnnotations,
	)
	cronPod := base(
		promql.QPodInfo,
		promql.QPodOwner, promql.QPodOwner, promql.QPodOwner,
		promql.QNodeInfo, promql.QNodeAddresses, promql.QNodeLabels, promql.QNodeStatusCondition,
		promql.QJobOwner, promql.QJobOwner,
		promql.QJobAnnotations, promql.QJobAnnotations,
		promql.QCronJobAnnotations, promql.QCronJobAnnotations,
		promql.QDeploymentAnnotations, promql.QStatefulSetAnnotations, promql.QDaemonSetAnnotations, promql.QReplicaSetAnnotations,
	)

	everyForward := slices.Concat(storageUnrestrictedLegs, promql.PodScopedQueries, promql.NodeScopedQueries, promql.ControllerScopedQueries, promql.QoSWorkloadQueries)
	every := map[string]int{}
	for _, q := range everyForward {
		every[string(q)]++
	}
	for _, q := range ann {
		every[string(q)]++
	}
	every[string(promql.QReplicaSetOwner)]++
	every[string(promql.QJobOwner)]++
	every[string(promql.QPodOwner)] += 6

	// The fourth row is appended after its fixture is built.
	cases := []struct { //nolint:prealloc
		name     string
		fixtures map[promql.Query]model.Vector
		roots    graph.StorageRoots
		want     map[string]int
		podKinds int // stage-3 kube_pod_owner queries, which carry owner_kind
	}{
		{
			name: "application with nothing matching issues no pod query",
			fixtures: map[promql.Query]model.Vector{
				promql.QPVCBindings: sampleVec(model.Sample{Metric: model.Metric{
					"cluster": "c", "namespace": "db", "pod": "catalog-0", "persistentvolumeclaim": "catalog-data",
				}}),
			},
			roots: app("checkout"),
			want:  base(ann...),
		},
		{
			name: "one deployment-managed claimless pod",
			fixtures: map[promql.Query]model.Vector{
				promql.QDeploymentAnnotations: track("deployment", "web", "Deployment"),
				promql.QReplicaSetOwner: sampleVec(model.Sample{Metric: model.Metric{
					"cluster": "c", "namespace": "db", "replicaset": "web-7d9f", "owner_kind": "Deployment", "owner_name": "web",
				}}),
				promql.QPodOwner: sampleVec(model.Sample{Metric: model.Metric{
					"cluster": "c", "namespace": "db", "pod": "web-7d9f-abc",
					"owner_kind": "ReplicaSet", "owner_name": "web-7d9f", "owner_is_controller": "true",
				}}),
				promql.QPodInfo: sampleVec(model.Sample{Metric: model.Metric{
					"cluster": "c", "namespace": "db", "pod": "web-7d9f-abc", "uid": "u-web", "node": "n1",
				}}),
			},
			roots:    app("checkout"),
			want:     deployPod,
			podKinds: 2,
		},
		{
			name: "one cronjob-managed claimless pod",
			fixtures: map[promql.Query]model.Vector{
				promql.QCronJobAnnotations: track("cronjob", "nightly", "CronJob"),
				promql.QJobOwner: sampleVec(model.Sample{Metric: model.Metric{
					"cluster": "c", "namespace": "db", "job_name": "nightly-28901",
					"owner_kind": "CronJob", "owner_name": "nightly", "owner_is_controller": "true",
				}}),
				promql.QPodOwner: sampleVec(model.Sample{Metric: model.Metric{
					"cluster": "c", "namespace": "db", "pod": "nightly-28901-x",
					"owner_kind": "Job", "owner_name": "nightly-28901", "owner_is_controller": "true",
				}}),
				promql.QPodInfo: sampleVec(model.Sample{Metric: model.Metric{
					"cluster": "c", "namespace": "db", "pod": "nightly-28901-x", "uid": "u-nightly", "node": "n1",
				}}),
			},
			roots:    app("checkout"),
			want:     cronPod,
			podKinds: 2,
		},
	}

	// Every controller kind, plus a matched volume: the 38-leg forward maximum
	// plus stage 1 (6), stage 2 (2) and stage 3 (6, including the direct
	// Deployment and CronJob arms).
	everyFix := map[promql.Query]model.Vector{
		promql.QPVCBindings: sampleVec(
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "sts-0", "persistentvolumeclaim": "data-sts-0"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "ds-0", "persistentvolumeclaim": "data-ds-0"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "dep-0", "persistentvolumeclaim": "data-dep-0"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "rs-0", "persistentvolumeclaim": "data-rs-0"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "job-0", "persistentvolumeclaim": "data-job-0"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "cj-0", "persistentvolumeclaim": "data-cj-0"}},
		),
		promql.QPodInfo: sampleVec(
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "sts-0", "uid": "u-sts", "node": "n1"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "ds-0", "uid": "u-ds", "node": "n1"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "dep-0", "uid": "u-dep", "node": "n1"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "rs-0", "uid": "u-rs", "node": "n1"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "job-0", "uid": "u-job", "node": "n1"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "cj-0", "uid": "u-cj", "node": "n1"}},
		),
		promql.QPodOwner: sampleVec(
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "sts-0", "owner_kind": "StatefulSet", "owner_name": "orders", "owner_is_controller": "true"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "ds-0", "owner_kind": "DaemonSet", "owner_name": "logger", "owner_is_controller": "true"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "dep-0", "owner_kind": "ReplicaSet", "owner_name": "web-7d9", "owner_is_controller": "true"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "rs-0", "owner_kind": "ReplicaSet", "owner_name": "rs-bare", "owner_is_controller": "true"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "job-0", "owner_kind": "Job", "owner_name": "batch-1", "owner_is_controller": "true"}},
			model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "cj-0", "owner_kind": "Job", "owner_name": "nightly-28901", "owner_is_controller": "true"}},
		),
		promql.QReplicaSetOwner: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "replicaset": "web-7d9", "owner_kind": "Deployment", "owner_name": "web",
		}}),
		promql.QJobOwner: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "job_name": "nightly-28901",
			"owner_kind": "CronJob", "owner_name": "nightly", "owner_is_controller": "true",
		}}),
		promql.QDeploymentAnnotations:  track("deployment", "web", "Deployment"),
		promql.QStatefulSetAnnotations: track("statefulset", "orders", "StatefulSet"),
		promql.QDaemonSetAnnotations:   track("daemonset", "logger", "DaemonSet"),
		promql.QReplicaSetAnnotations:  track("replicaset", "rs-bare", "ReplicaSet"),
		promql.QJobAnnotations:         track("job_name", "batch-1", "Job"),
		promql.QCronJobAnnotations:     track("cronjob", "nightly", "CronJob"),
		promql.QPVCInfo: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data-db-0", "volumename": "pvc-9f3a",
		}}),
		promql.QVolumeLabels: sampleVec(model.Sample{Metric: model.Metric{
			"volume": "trident_pvc_9f3a", "cluster": "ontap-prod", "node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm0",
		}}),
	}
	cases = append(cases, struct {
		name     string
		fixtures map[promql.Query]model.Vector
		roots    graph.StorageRoots
		want     map[string]int
		podKinds int
	}{name: "every controller kind present, and a matched volume", fixtures: everyFix, roots: app("checkout"), want: every, podKinds: 6})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := promqlfake.New(tc.fixtures)
			_, err := readTopology(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{}, storagePlan(tc.roots))
			require.NoError(t, err)
			seen := map[string]int{}
			for _, is := range f.Issued() {
				seen[is.Name]++
			}
			assert.Equal(t, tc.want, seen)
			var kinds []string
			for _, q := range f.QueriesFor(promql.QPodOwner) {
				if strings.Contains(q, `owner_kind="`) {
					kinds = append(kinds, q)
				}
			}
			assert.Len(t, kinds, tc.podKinds)
			if tc.podKinds == 6 {
				joined := strings.Join(kinds, "\n")
				assert.Contains(t, joined, `owner_kind="Deployment"`, "dropping the direct Deployment arm fails this row")
				assert.Contains(t, joined, `owner_kind="CronJob"`, "dropping the direct CronJob arm fails this row")
			}
		})
	}
}

// hubFirstWave are the thirteen families a hub-mode storage build's first wave
// issues (read-storage-roots-through-volume-hub D9): storageUnrestrictedLegs
// minus the five claim-keyed families, which leave for the hub's by-reference
// reads. volume_labels here is phase 1.
var hubFirstWave = slices.DeleteFunc(slices.Clone(storageUnrestrictedLegs), func(q promql.Query) bool {
	return slices.Contains(promql.ClaimScopedQueries, q)
})

// The hub-mode fan-out pin: a per-family QUERY count, since volume_labels is
// read up to three times (phase 1, owner completion, phase 2). The counts are
// the ones docs/upstream-metrics.md documents.
func TestBuildStorage_FanOutLegCount_Hub(t *testing.T) {
	require.Len(t, hubFirstWave, 13, "design D9: the hub's first wave")

	vol := func(name, svm string) *model.Sample {
		return &model.Sample{Metric: model.Metric{
			"volume": model.LabelValue(name), "cluster": "ontap-prod", "node": "ontap-prod-01", "aggr": "aggr1", "svm": model.LabelValue(svm),
		}, Value: 1}
	}
	ksm := func(pairs ...string) *model.Sample {
		m := model.Metric{"cluster": "c", "namespace": "db"}
		for i := 0; i+1 < len(pairs); i += 2 {
			m[model.LabelName(pairs[i])] = model.LabelValue(pairs[i+1])
		}
		return &model.Sample{Metric: m, Value: 1}
	}
	path := map[promql.Query]model.Vector{
		promql.QVolumeLabels: {vol("trident_pvc_9f3a", "svm0")},
		promql.QPVCInfo:      {ksm("persistentvolumeclaim", "data-db-0", "volumename", "pvc-9f3a")},
		promql.QPVCBindings:  {ksm("pod", "db-0", "persistentvolumeclaim", "data-db-0")},
		promql.QPodInfo:      {ksm("pod", "db-0", "uid", "u-db-0", "node", "n1")},
		promql.QPodOwner:     {ksm("pod", "db-0", "owner_kind", "StatefulSet", "owner_name", "db", "owner_is_controller", "true")},
	}
	count := func(qs ...promql.Query) map[string]int {
		m := map[string]int{}
		for _, q := range hubFirstWave {
			m[string(q)] = 1
		}
		for _, q := range qs {
			m[string(q)]++
		}
		return m
	}
	fullPath := slices.Concat(
		promql.ClaimScopedQueries, promql.PodScopedQueries, promql.NodeScopedQueries,
		[]promql.Query{promql.QStatefulSetAnnotations}, promql.QoSWorkloadQueries,
	)

	cases := []struct {
		name     string
		fixtures map[promql.Query]model.Vector
		roots    graph.StorageRoots
		want     map[string]int
	}{
		{"aggregate root, no candidate", map[promql.Query]model.Vector{
			promql.QVolumeLabels: {vol("vol0", "svm0")},
		}, graph.StorageRoots{Aggrs: map[string]struct{}{"aggr1": {}}}, count()},
		{"aggregate root, a candidate naming no claim", map[promql.Query]model.Vector{
			promql.QVolumeLabels: {vol("trident_pvc_gone", "svm0")},
		}, graph.StorageRoots{Aggrs: map[string]struct{}{"aggr1": {}}}, count(promql.QPVCInfo)},
		{"aggregate root, one statefulset-owned pod on a matched volume", path,
			graph.StorageRoots{Aggrs: map[string]struct{}{"aggr1": {}}},
			count(append(fullPath, promql.QVolumeLabels)...)}, // + phase 2
		{"svm root, the same path", path,
			graph.StorageRoots{SVMs: map[string]struct{}{"svm0": {}}},
			count(append(fullPath, promql.QVolumeLabels, promql.QVolumeLabels)...)}, // + completion + phase 2
		{"aggregate and svm roots on the same aggregate: nothing to complete", path,
			graph.StorageRoots{Aggrs: map[string]struct{}{"aggr1": {}}, SVMs: map[string]struct{}{"svm0": {}}},
			count(append(fullPath, promql.QVolumeLabels, promql.QVolumeLabels)...)}, // + SVM group + phase 2
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := promqlfake.New(tc.fixtures)
			tp, err := readTopology(t.Context(), f, time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{}, storagePlan(tc.roots))
			require.NoError(t, err)
			seen := map[string]int{}
			for _, is := range f.Issued() {
				seen[is.Name]++
			}
			assert.Equal(t, tc.want, seen)
			for name := range tc.want {
				assert.Contains(t, tp.RawSeriesCount, name, "%s tallied", name)
			}
			for _, q := range promql.ClaimScopedQueries {
				if _, issued := tc.want[string(q)]; !issued {
					assert.NotContains(t, tp.RawSeriesCount, string(q), "%s: not issued, so absent, never 0", q)
				}
			}
		})
	}
}
