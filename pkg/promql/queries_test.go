package promql

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRender_PodInfoNoClusterFilter(t *testing.T) {
	got := Render(QPodInfo, time.Minute)
	assert.Contains(t, got, "kube_pod_info")
	assert.Contains(t, got, "[1m]")
	assert.NotContains(t, got, "cluster=~", "PromQL must not push cluster filtering")
}

func TestRender_ServiceGraphTotal(t *testing.T) {
	got := Render(QServiceGraphTotal, time.Minute)
	assert.Contains(t, got, "traces_service_graph_request_total")
	assert.NotContains(t, got, "client_cluster")
	assert.NotContains(t, got, "server_cluster")
}

// TestRender_ServiceGraphExcludesSentinelPeers pins design.md D30: the
// service-graph selector drops the servicegraph connector's virtual peers
// (uninstrumented caller "user", unresolved peer "unknown") at the query
// layer via anchored negative matchers on the client and server labels. The
// match is exact (RE2 is fully anchored) and case-sensitive, so a connection
// string such as "http://user/..." is NOT excluded.
func TestRender_ServiceGraphExcludesSentinelPeers(t *testing.T) {
	got := Render(QServiceGraphTotal, time.Minute)
	assert.Equal(t, `rate(traces_service_graph_request_total{client!~"user|unknown",server!~"user|unknown"}[1m])`, got)
	assert.Contains(t, got, `client!~"user|unknown"`)
	assert.Contains(t, got, `server!~"user|unknown"`)

	// The Query constant itself MUST stay the bare metric name so the
	// `query` / `query_name` self-metric + span dimensions stay stable across
	// deployments (design.md D25 / D26); only the rendered PromQL gains the
	// matchers.
	assert.Equal(t, "traces_service_graph_request_total", string(QServiceGraphTotal))
}

func TestRender_NodeAddressesIncludesExternalIPSelector(t *testing.T) {
	got := Render(QNodeAddresses, time.Minute)
	assert.Contains(t, got, `type=~"ExternalIP|InternalIP"`)
}

// TestRenderer_PrefixApplied covers the additive metric-name prefix knob
// (design.md D26) across every kube-state-metrics-shaped query plus the
// cluster-discovery query that wraps kube_node_info.
func TestRenderer_PrefixApplied(t *testing.T) {
	cases := []struct {
		name   string
		q      Query
		window time.Duration
		want   string
	}{
		{"pod-info", QPodInfo, time.Minute, "last_over_time(o11y_kube_pod_info[1m])"},
		{"node-info", QNodeInfo, time.Minute, "last_over_time(o11y_kube_node_info[1m])"},
		{"node-addresses", QNodeAddresses, time.Minute, `last_over_time(o11y_kube_node_status_addresses{type=~"ExternalIP|InternalIP"}[1m])`},
		{"pvc-bindings", QPVCBindings, time.Minute, "last_over_time(o11y_kube_pod_spec_volumes_persistentvolumeclaims_info[1m])"},
		{"node-labels", QNodeLabels, time.Minute, "last_over_time(o11y_kube_node_labels[1m])"},
		{"service-info", QServiceInfo, time.Minute, "last_over_time(o11y_kube_service_info[1m])"},
		{"endpointslice-endpoints", QEndpointSliceEndpoints, time.Minute, "last_over_time(o11y_kube_endpointslice_endpoints[1m])"},
		{"endpointslice-labels", QEndpointSliceLabels, time.Minute, "last_over_time(o11y_kube_endpointslice_labels[1m])"},
		{"pod-owner", QPodOwner, time.Minute, "last_over_time(o11y_kube_pod_owner[1m])"},
		{"replicaset-owner", QReplicaSetOwner, time.Minute, "last_over_time(o11y_kube_replicaset_owner[1m])"},
		{"pvc-info", QPVCInfo, time.Minute, "last_over_time(o11y_kube_persistentvolumeclaim_info[1m])"},
		{"storageclass-info", QStorageClassInfo, time.Minute, "last_over_time(o11y_kube_storageclass_info[1m])"},
		{"pod-container-info", QPodContainerInfo, time.Minute, "tlast_over_time(o11y_kube_pod_container_info[1m])"},
		{"node-status-condition", QNodeStatusCondition, time.Minute, `last_over_time(o11y_kube_node_status_condition{condition="Ready"}[1m])`},
		{"service-annotations", QServiceAnnotations, time.Minute, "last_over_time(o11y_kube_service_annotations[1m])"},
		{"pvc-annotations", QPVCAnnotations, time.Minute, "last_over_time(o11y_kube_persistentvolumeclaim_annotations[1m])"},
		{"tridentvolume-info", QTridentVolumeInfo, time.Minute, "last_over_time(o11y_kube_tridentvolume_info[1m])"},
		{"tridentbackend-info", QTridentBackendInfo, time.Minute, "last_over_time(o11y_kube_tridentbackend_info[1m])"},
		{"cluster-discovery", QClusterDiscovery, time.Hour, "group by (cluster) (last_over_time(o11y_kube_node_info[1h]))"},
	}
	r := Renderer{Prefix: "o11y_"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, r.Render(tc.q, tc.window))
		})
	}
}

// TestRenderer_PrefixNotAppliedToServiceGraphOrUp asserts the negative
// scope rule: the configurable prefix targets only kube-state-metrics-shaped
// series. Service-graph metrics (Alloy/Tempo family) and the Prometheus-native
// `up{}` readiness probe MUST be unaffected.
func TestRenderer_PrefixNotAppliedToServiceGraphOrUp(t *testing.T) {
	r := Renderer{Prefix: "o11y_"}
	sg := r.Render(QServiceGraphTotal, time.Minute)
	assert.Equal(t, `rate(traces_service_graph_request_total{client!~"user|unknown",server!~"user|unknown"}[1m])`, sg)
	assert.NotContains(t, sg, "o11y_", "prefix must NOT apply to service-graph metric")

	up := r.Render(QUpProbe, 0)
	assert.Equal(t, "up", up)
	assert.NotContains(t, up, "o11y_", "prefix must NOT apply to up{} probe")
}

// TestRender_ZeroPrefixIdenticalToBareNames pins the back-compat contract:
// the package-level Render is a zero-prefix Renderer; a deployment that
// leaves MetricPrefix empty issues bit-identical PromQL to the pre-D26
// behaviour.
func TestRender_ZeroPrefixIdenticalToBareNames(t *testing.T) {
	r := Renderer{}
	assert.Equal(t, Render(QPodInfo, time.Minute), r.Render(QPodInfo, time.Minute))
	assert.Equal(t, Render(QNodeInfo, time.Minute), r.Render(QNodeInfo, time.Minute))
	assert.Equal(t, Render(QClusterDiscovery, time.Hour), r.Render(QClusterDiscovery, time.Hour))
	assert.Contains(t, r.Render(QPodInfo, time.Minute), "kube_pod_info")
}

// TestRender_PVCInfoPrefixAware pins the new kube_persistentvolumeclaim_info
// query: bare by default (stable self-metric dimension), prefix-aware via
// Renderer like every other KSM-shaped series.
func TestRender_PVCInfoPrefixAware(t *testing.T) {
	assert.Equal(t, "last_over_time(kube_persistentvolumeclaim_info[1m])", Render(QPVCInfo, time.Minute))
	assert.Equal(t, "kube_persistentvolumeclaim_info", string(QPVCInfo),
		"Query constant stays the bare metric name for stable query/query_name dimensions")
	assert.Equal(t, "last_over_time(o11y_kube_persistentvolumeclaim_info[1m])",
		Renderer{Prefix: "o11y_"}.Render(QPVCInfo, time.Minute))
}

// TestRender_StorageClassInfoPrefixAware pins the new kube_storageclass_info
// query: bare by default (stable self-metric dimension), prefix-aware via
// Renderer like every other KSM-shaped series.
func TestRender_StorageClassInfoPrefixAware(t *testing.T) {
	assert.Equal(t, "last_over_time(kube_storageclass_info[1m])", Render(QStorageClassInfo, time.Minute))
	assert.Equal(t, "kube_storageclass_info", string(QStorageClassInfo),
		"Query constant stays the bare metric name for stable query/query_name dimensions")
	assert.Equal(t, "last_over_time(o11y_kube_storageclass_info[1m])",
		Renderer{Prefix: "o11y_"}.Render(QStorageClassInfo, time.Minute))
}

// TestRender_PodContainerInfoPrefixAware pins the new kube_pod_container_info
// query (per-container name/image). It uses tlast_over_time (NOT last_over_time)
// so each image-variant series carries its last-sample timestamp as the value,
// letting the resolver pick the latest image per container. The Query constant
// stays the bare metric name (stable self-metric dimension); prefix-aware via
// Renderer like every other KSM-shaped series.
func TestRender_PodContainerInfoPrefixAware(t *testing.T) {
	assert.Equal(t, "tlast_over_time(kube_pod_container_info[1m])", Render(QPodContainerInfo, time.Minute))
	assert.Equal(t, "kube_pod_container_info", string(QPodContainerInfo),
		"Query constant stays the bare metric name for stable query/query_name dimensions")
	assert.Equal(t, "tlast_over_time(o11y_kube_pod_container_info[1m])",
		Renderer{Prefix: "o11y_"}.Render(QPodContainerInfo, time.Minute))
}

// TestRender_NodeStatusConditionPrefixAware pins the new
// kube_node_status_condition query: bare by default (stable self-metric
// dimension), prefix-aware via Renderer, and carrying the fixed condition="Ready"
// metric-selection contract (not a caller filter) — the four other node
// conditions are never surfaced.
func TestRender_NodeStatusConditionPrefixAware(t *testing.T) {
	got := Render(QNodeStatusCondition, time.Minute)
	assert.Equal(t, `last_over_time(kube_node_status_condition{condition="Ready"}[1m])`, got)
	assert.Contains(t, got, `condition="Ready"`)
	assert.NotContains(t, got, "cluster=~", "PromQL must not push cluster filtering")
	assert.Equal(t, "kube_node_status_condition", string(QNodeStatusCondition),
		"Query constant stays the bare metric name for stable query/query_name dimensions")
	assert.Equal(t, `last_over_time(o11y_kube_node_status_condition{condition="Ready"}[1m])`,
		Renderer{Prefix: "o11y_"}.Render(QNodeStatusCondition, time.Minute))
}

// TestRender_ServiceAnnotationsPrefixAware pins the new kube_service_annotations
// query (carrying annotation_argocd_argoproj_io_tracking_id for the service
// ArgoCD Application). Bare by default (stable self-metric dimension), prefix-aware
// via Renderer like every other KSM-shaped series.
func TestRender_ServiceAnnotationsPrefixAware(t *testing.T) {
	assert.Equal(t, "last_over_time(kube_service_annotations[1m])", Render(QServiceAnnotations, time.Minute))
	assert.Equal(t, "kube_service_annotations", string(QServiceAnnotations),
		"Query constant stays the bare metric name for stable query/query_name dimensions")
	assert.Equal(t, "last_over_time(o11y_kube_service_annotations[1m])",
		Renderer{Prefix: "o11y_"}.Render(QServiceAnnotations, time.Minute))
}

// TestRender_PVCAnnotationsPrefixAware pins the new
// kube_persistentvolumeclaim_annotations query (carrying
// annotation_argocd_argoproj_io_tracking_id for the PVC ArgoCD Application).
func TestRender_PVCAnnotationsPrefixAware(t *testing.T) {
	assert.Equal(t, "last_over_time(kube_persistentvolumeclaim_annotations[1m])", Render(QPVCAnnotations, time.Minute))
	assert.Equal(t, "kube_persistentvolumeclaim_annotations", string(QPVCAnnotations),
		"Query constant stays the bare metric name for stable query/query_name dimensions")
	assert.Equal(t, "last_over_time(o11y_kube_persistentvolumeclaim_annotations[1m])",
		Renderer{Prefix: "o11y_"}.Render(QPVCAnnotations, time.Minute))
}

// TestRender_TridentPrefixAware pins the two NetApp Trident custom-resource
// queries (kube_tridentvolume_info / kube_tridentbackend_info — the PVC
// volumename→backendUUID→svm label chain). Bare by default (stable self-metric
// dimension), prefix-aware via Renderer like every other KSM-shaped series.
func TestRender_TridentPrefixAware(t *testing.T) {
	assert.Equal(t, "last_over_time(kube_tridentvolume_info[1m])", Render(QTridentVolumeInfo, time.Minute))
	assert.Equal(t, "last_over_time(kube_tridentbackend_info[1m])", Render(QTridentBackendInfo, time.Minute))
	assert.Equal(t, "kube_tridentvolume_info", string(QTridentVolumeInfo),
		"Query constant stays the bare metric name for stable query/query_name dimensions")
	assert.Equal(t, "kube_tridentbackend_info", string(QTridentBackendInfo),
		"Query constant stays the bare metric name for stable query/query_name dimensions")
	assert.Equal(t, "last_over_time(o11y_kube_tridentvolume_info[1m])",
		Renderer{Prefix: "o11y_"}.Render(QTridentVolumeInfo, time.Minute))
	assert.Equal(t, "last_over_time(o11y_kube_tridentbackend_info[1m])",
		Renderer{Prefix: "o11y_"}.Render(QTridentBackendInfo, time.Minute))
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                       "0s",
		2 * time.Hour:           "2h",
		15 * time.Minute:        "15m",
		90 * time.Second:        "90s",
		500 * time.Millisecond:  "1s", // F1: positive sub-second never renders [0s]
		999 * time.Millisecond:  "1s",
		time.Nanosecond:         "1s",
		1500 * time.Millisecond: "1s", // truncates, but floored at 1s
	}
	for in, want := range cases {
		assert.Equal(t, want, FormatDuration(in), "FormatDuration(%s)", in)
	}
}
