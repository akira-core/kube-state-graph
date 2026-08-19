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

// TestRender_ServiceGraphExcludesSentinelPeers pins design.md D30 plus its
// resolve-unknown-server-peer-labels D1 narrowing: the service-graph selector
// drops the servicegraph connector's virtual "user" peer on both sides, but
// only drops the virtual "unknown" peer on the client side — a literal
// server="unknown" now reaches Go so the peer-label enrichment branch can run.
// The match is exact (RE2 is fully anchored) and case-sensitive, so a
// connection string such as "http://user/..." is NOT excluded.
func TestRender_ServiceGraphExcludesSentinelPeers(t *testing.T) {
	got := Render(QServiceGraphTotal, time.Minute)
	assert.Equal(t, `rate(traces_service_graph_request_total{client!~"user|unknown",server!~"user"}[1m])`, got)
	assert.Contains(t, got, `client!~"user|unknown"`)
	assert.Contains(t, got, `server!~"user"`)
	assert.NotContains(t, got, `server!~"user|unknown"`)

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

// TestRender_BareKSMNames pins every surviving kube-state-metrics-shaped
// query (and cluster-discovery) at its bare series name — there is no
// configurable metric-name prefix.
func TestRender_BareKSMNames(t *testing.T) {
	cases := []struct {
		name   string
		q      Query
		window time.Duration
		want   string
	}{
		{"pod-info", QPodInfo, time.Minute, "last_over_time(kube_pod_info[1m])"},
		{"node-info", QNodeInfo, time.Minute, "last_over_time(kube_node_info[1m])"},
		{"node-addresses", QNodeAddresses, time.Minute, `last_over_time(kube_node_status_addresses{type=~"ExternalIP|InternalIP"}[1m])`},
		{"pvc-bindings", QPVCBindings, time.Minute, "last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info[1m])"},
		{"node-labels", QNodeLabels, time.Minute, "last_over_time(kube_node_labels[1m])"},
		{"service-info", QServiceInfo, time.Minute, "last_over_time(kube_service_info[1m])"},
		{"endpointslice-endpoints", QEndpointSliceEndpoints, time.Minute, "last_over_time(kube_endpointslice_endpoints[1m])"},
		{"endpointslice-labels", QEndpointSliceLabels, time.Minute, "last_over_time(kube_endpointslice_labels[1m])"},
		{"pod-owner", QPodOwner, time.Minute, "last_over_time(kube_pod_owner[1m])"},
		{"replicaset-owner", QReplicaSetOwner, time.Minute, "last_over_time(kube_replicaset_owner[1m])"},
		{"pvc-info", QPVCInfo, time.Minute, "last_over_time(kube_persistentvolumeclaim_info[1m])"},
		{"pod-container-info", QPodContainerInfo, time.Minute, "tlast_over_time(kube_pod_container_info[1m])"},
		{"node-status-condition", QNodeStatusCondition, time.Minute, `last_over_time(kube_node_status_condition{condition="Ready"}[1m])`},
		{"service-annotations", QServiceAnnotations, time.Minute, "last_over_time(kube_service_annotations[1m])"},
		{"pvc-annotations", QPVCAnnotations, time.Minute, "last_over_time(kube_persistentvolumeclaim_annotations[1m])"},
		{"cluster-discovery", QClusterDiscovery, time.Hour, "group by (cluster) (last_over_time(kube_node_info[1h]))"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Render(tc.q, tc.window))
		})
	}
}

// TestRender_ServiceGraphFailedTotal pins the OPTIONAL Errors counter render:
// rate(...) at raw label granularity, sentinel + span-link selectors, bare
// metric name (no prefix), stable Query constant.
func TestRender_ServiceGraphFailedTotal(t *testing.T) {
	want := `rate(traces_service_graph_request_failed_total{client!~"user|unknown",server!~"user",edge_relation!="link"}[1m])`
	assert.Equal(t, want, Render(QServiceGraphFailedTotal, time.Minute))
	assert.Equal(t, "traces_service_graph_request_failed_total", string(QServiceGraphFailedTotal))
}

// TestRender_ServiceGraphServerSecondsBucket pins the OPTIONAL Duration
// histogram render: RAW rate(...) with NO upstream aggregation (design D4), so
// each bucket series keeps its full dimension set plus `le` and joins by exact
// identity. Sentinel + span-link selectors, bare metric name (no prefix).
func TestRender_ServiceGraphServerSecondsBucket(t *testing.T) {
	want := `rate(traces_service_graph_request_server_seconds_bucket{client!~"user|unknown",server!~"user",edge_relation!="link"}[1m])`
	got := Render(QServiceGraphServerSecondsBucket, time.Minute)
	assert.Equal(t, want, got)
	assert.NotContains(t, got, "sum by",
		"the duration histogram must NOT be aggregated upstream — a group-by silently merges unrelated edges")
	assert.Equal(t, "traces_service_graph_request_server_seconds_bucket", string(QServiceGraphServerSecondsBucket))
}

// TestRender_ServiceGraphTotalKeepsSpanLinkSeries pins the deliberate asymmetry
// of design D6: the span-link exclusion belongs to the two OPTIONAL RED
// selectors only. A link edge must still be EMITTED — it is a real dependency —
// so filtering it out of the request-total query would delete the edge itself,
// not just its numbers.
func TestRender_ServiceGraphTotalKeepsSpanLinkSeries(t *testing.T) {
	total := Render(QServiceGraphTotal, time.Minute)
	assert.Equal(t, `rate(traces_service_graph_request_total{client!~"user|unknown",server!~"user"}[1m])`, total)
	assert.NotContains(t, total, "edge_relation",
		"the request-total selector must not exclude span-link series")

	for _, q := range []Query{QServiceGraphFailedTotal, QServiceGraphServerSecondsBucket} {
		assert.Contains(t, Render(q, time.Minute), `edge_relation!="link"`,
			"RED selector %s must exclude span-link series", q)
		assert.NotContains(t, Render(q, time.Minute), "client_k8s_pod_uid",
			"RED selector %s must not filter on pod UIDs — peer-resolved edges are eligible", q)
	}
}

// TestRender_QueryConstantsStayBare pins that every Query constant equals
// the bare series name so query=/query_name= self-metric dimensions stay
// stable.
func TestRender_QueryConstantsStayBare(t *testing.T) {
	assert.Equal(t, "kube_persistentvolumeclaim_info", string(QPVCInfo))
	assert.Equal(t, "kube_pod_container_info", string(QPodContainerInfo))
	assert.Equal(t, "kube_node_status_condition", string(QNodeStatusCondition))
	assert.Equal(t, "kube_service_annotations", string(QServiceAnnotations))
	assert.Equal(t, "kube_persistentvolumeclaim_annotations", string(QPVCAnnotations))
}

// TestRender_HarvestAndKubeletLastOverTime pins the ten new legs: each
// renders as last_over_time(<series>[w]) with no rate() and no sum by.
func TestRender_HarvestAndKubeletLastOverTime(t *testing.T) {
	cases := []struct {
		q    Query
		want string
	}{
		{QVolumeReadOps, "last_over_time(volume_read_ops[1m])"},
		{QVolumeWriteOps, "last_over_time(volume_write_ops[1m])"},
		{QVolumeReadLatency, "last_over_time(volume_read_latency[1m])"},
		{QVolumeWriteLatency, "last_over_time(volume_write_latency[1m])"},
		{QAggrStatus, "last_over_time(aggr_new_status[1m])"},
		{QAggrSpaceUsed, "last_over_time(aggr_space_used[1m])"},
		{QAggrSpaceTotal, "last_over_time(aggr_space_total[1m])"},
		{QNetAppNodeStatus, "last_over_time(node_new_status[1m])"},
		{QKubeletVolumeUsedBytes, "last_over_time(kubelet_volume_stats_used_bytes[1m])"},
		{QKubeletVolumeCapacityBytes, "last_over_time(kubelet_volume_stats_capacity_bytes[1m])"},
	}
	for _, tc := range cases {
		t.Run(string(tc.q), func(t *testing.T) {
			got := Render(tc.q, time.Minute)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "rate(")
			assert.NotContains(t, got, "sum by")
			assert.Contains(t, got, string(tc.q))
		})
	}
}

// TestRender_NodeStatusConditionSelector pins the fixed condition="Ready"
// metric-selection contract (not a caller filter).
func TestRender_NodeStatusConditionSelector(t *testing.T) {
	got := Render(QNodeStatusCondition, time.Minute)
	assert.Equal(t, `last_over_time(kube_node_status_condition{condition="Ready"}[1m])`, got)
	assert.Contains(t, got, `condition="Ready"`)
	assert.NotContains(t, got, "cluster=~", "PromQL must not push cluster filtering")
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
