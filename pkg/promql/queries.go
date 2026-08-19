package promql

import (
	"fmt"
	"time"
)

// Query identifies one of the PromQL templates the build pipeline issues.
// The name is also used as the `query` label on self-metrics.
//
// The constant values are the bare upstream metric names. The Query string
// itself is never rewritten so that self-metric and span dimensions stay
// stable. There is no configurable metric-name prefix — every series is
// queried at its bare name.
type Query string

const (
	QPodInfo           Query = "kube_pod_info"
	QNodeInfo          Query = "kube_node_info"
	QNodeAddresses     Query = "kube_node_status_addresses"
	QPVCBindings       Query = "kube_pod_spec_volumes_persistentvolumeclaims_info"
	QNodeLabels        Query = "kube_node_labels"
	QServiceGraphTotal Query = "traces_service_graph_request_total"
	// QServiceGraphFailedTotal is the Errors counter of the service-graph RED
	// triple. OPTIONAL — a missing metric, empty result, or query error degrades
	// to "no error_rate on the edge" and never fails the build. Alloy/Tempo
	// exporter family, same as QServiceGraphTotal. See design D3 / D6 of
	// add-service-graph-red-metrics.
	QServiceGraphFailedTotal Query = "traces_service_graph_request_failed_total"
	// QServiceGraphServerSecondsBucket is the Duration classic histogram of the
	// service-graph RED triple (server-observed). OPTIONAL — absence degrades to
	// "no p90_server_ms". The rendered PromQL is RAW: no upstream aggregation, so
	// each bucket series keeps the full dimension set of its request-total
	// series plus `le` and joins by exact identity, exactly like the failure
	// counter (design D4 / D5).
	QServiceGraphServerSecondsBucket Query = "traces_service_graph_request_server_seconds_bucket"
	QClusterDiscovery                Query = "cluster_discovery"
	QUpProbe                         Query = "up"

	// Service / endpointslice topology (D29 connection-string resolution).
	QServiceInfo            Query = "kube_service_info"
	QEndpointSliceEndpoints Query = "kube_endpointslice_endpoints"
	QEndpointSliceLabels    Query = "kube_endpointslice_labels"

	// Pod controller-owner resolution (D34). kube_pod_owner gives a pod's
	// owner refs; kube_replicaset_owner resolves a ReplicaSet owner up to
	// its owning Deployment (the ReplicaSet is skipped). The
	// owner_kind/owner_name/owner_is_controller labels are KSM defaults
	// (no --metric-labels-allowlist required). NOTE: the optional
	// `argocd_tracking_id` label the application resolver reads off kube_pod_owner
	// (resolvePodApplications) is NOT a KSM default — it is operator-provided
	// (e.g. via --metric-labels-allowlist or a relabel); absence degrades
	// gracefully to no `application` attribute. See design.md D-A4.
	QPodOwner        Query = "kube_pod_owner"
	QReplicaSetOwner Query = "kube_replicaset_owner"

	// PVC StorageClass name + bound PV name. kube_persistentvolumeclaim_info
	// carries the `storageclass` and `volumename` labels that the pod→PVC
	// binding metric (QPVCBindings) lacks; joined on
	// (cluster, namespace, persistentvolumeclaim) to enrich existing PVC
	// nodes (never to materialise new ones). OPTIONAL — a KSM default, no
	// --metric-labels-allowlist required. The StorageClass name is the PVC's
	// own typed attribute (never a node); `volumename` roots the Harvest
	// volume join (see QVolumeReadOps).
	QPVCInfo Query = "kube_persistentvolumeclaim_info"

	// Pod container list resolution. kube_pod_container_info emits one series
	// per container carrying the `container` (name) and `image` labels; joined
	// on (cluster, namespace, pod) to enrich existing pod nodes with their
	// typed `containers` attribute (never new nodes). OPTIONAL — a KSM
	// default, no --metric-labels-allowlist required.
	QPodContainerInfo Query = "kube_pod_container_info"

	// K8s node Ready-status resolution. kube_node_status_condition emits one
	// series per (condition, status) with value 1 for the active combination;
	// the topology reader reads the active condition="Ready" row's `status`
	// label (true/false/unknown, matched case-insensitively — a raw-enum
	// exporter emits True/False/Unknown) to enrich the node's typed
	// `ready_status` attribute (never a label, never a new node).
	// The condition="Ready" selector is a fixed, request-invariant metric-
	// selection contract (same class as the QNodeAddresses type selector and the
	// D30 sentinel selector), NOT a caller filter. OPTIONAL — a KSM default,
	// absence degrades gracefully to no `ready_status`.
	QNodeStatusCondition Query = "kube_node_status_condition"

	// Service / PVC ArgoCD Application resolution.
	// kube_service_annotations / kube_persistentvolumeclaim_annotations
	// carry the `annotation_argocd_argoproj_io_tracking_id` label — KSM's sanitised
	// form of the argocd.argoproj.io/tracking-id annotation — whose value uses the
	// same <app>:<group>/<kind>:<ns>/<name> grammar as the pod's argocd_tracking_id,
	// so the Application is the segment before the first ":". Joined on
	// (cluster, namespace, service) / (cluster, namespace, persistentvolumeclaim) to
	// enrich existing service / PVC nodes (never new nodes). OPTIONAL — the
	// annotation label requires the operator's --metric-annotations-allowlist;
	// absence degrades gracefully to no `application` attribute.
	QServiceAnnotations Query = "kube_service_annotations"
	QPVCAnnotations     Query = "kube_persistentvolumeclaim_annotations"

	// NetApp Harvest volume I/O (replace-storageclass-with-netapp-nodes).
	// Harvest has already resolved ONTAP base counters — ops are per-second,
	// latency is an average in microseconds, data is bytes per second — so
	// these are read verbatim via last_over_time, NEVER wrapped in rate().
	// OPTIONAL: a query error or empty vector degrades to no I/O field /
	// no join, never a build failure. The `volume_name` label is a
	// deployment relabel (not stock Harvest) mapping each FlexVol to the
	// Kubernetes PV it backs.
	QVolumeReadOps      Query = "volume_read_ops"
	QVolumeWriteOps     Query = "volume_write_ops"
	QVolumeReadLatency  Query = "volume_read_latency"
	QVolumeWriteLatency Query = "volume_write_latency"
	QVolumeReadData     Query = "volume_read_data"
	QVolumeWriteData    Query = "volume_write_data"

	// NetApp Harvest aggregate + controller gauges. Same last_over_time
	// verbatim read; OPTIONAL; log-and-continue on query error.
	QAggrStatus       Query = "aggr_new_status"
	QAggrSpaceUsed    Query = "aggr_space_used"
	QAggrSpaceTotal   Query = "aggr_space_total"
	QNetAppNodeStatus Query = "node_new_status"

	// Kubelet PVC usage (bytes). OPTIONAL; per-field independent; joined on
	// (cluster, namespace, persistentvolumeclaim). Introduces kubelet as a
	// fourth upstream family.
	QKubeletVolumeUsedBytes     Query = "kubelet_volume_stats_used_bytes"
	QKubeletVolumeCapacityBytes Query = "kubelet_volume_stats_capacity_bytes"
)

// ClusterDiscoveryLookback is the fixed lookback used by /v1/clusters
// discovery. Sized to absorb transient KSM scrape gaps; not configurable.
const ClusterDiscoveryLookback = time.Hour

// serviceGraphSentinelSelector excludes the servicegraph connector's virtual
// peers from the service-graph series at the query layer (design.md D30): an
// uninstrumented caller surfaces as client="user", an unresolved peer as
// "unknown", neither carrying a pod UID. `!~` is a fully-anchored RE2 match, so
// a series is dropped only when the WHOLE client/server value equals a sentinel
// (case-sensitive) — a "http://user/..." connection string is unaffected, so
// D29 resolution is untouched. The set is fixed, no operator knob (as with
// D29's removal of KSG_OTHERS_NAME_PATTERN).
//
// The two matchers are independent (resolve-unknown-server-peer-labels D1). The
// client matcher excludes both sentinels: by the connector's contract a
// sentinel client never co-occurs with a populated pod UID, so dropping the
// whole series upstream discards no real pod-resolved edge, and an unresolved
// caller carries no identity worth recovering anyway. The server matcher is
// narrowed to `server!~"user"` so a literal server="unknown" reaches Go, where
// the "Unknown-server peer-label enrichment" branch resolves it from the
// client-recorded peer-address labels (client_net_peer_name /
// client_server_address), or drops it. This is not a general relaxation: every
// server="unknown" outside that narrow trigger is still dropped in Go with the
// identical no-node/no-edge outcome as the old exclusion — see
// resolveUnknownServerPeer in pkg/build/servicegraph.go.
//
// The numeric service-graph metrics (traces_service_graph_request_failed_total,
// traces_service_graph_request_server_seconds_bucket) reuse this fragment so the
// three series always describe one edge population (add-service-graph-red-metrics D6).
const serviceGraphSentinelSelector = `client!~"user|unknown",server!~"user"`

// serviceGraphLinkExclusionSelector drops span-link virtual edges from the two
// OPTIONAL RED series (add-service-graph-red-metrics D1b / D6). The
// operator-configured `edge_relation="link"` dimension marks an edge the
// servicegraph connector materialised from a SPAN LINK rather than from a
// paired client/server span: the two spans belong to different trace contexts
// and the interaction physically traverses a queue or a database, so the RED
// series measure something that is not a request-response call. The edge is
// still emitted (the request-total selector deliberately does NOT carry this
// matcher) — only its measurement is suppressed.
//
// Same class as the D30 sentinel and the node condition="Ready" selector: a
// fixed contract that never varies per request, NOT a caller filter, so the
// "no filters pushed to PromQL" rule is preserved. The label name and the value
// are compiled in — no knob.
//
// PromQL `!=` treats an ABSENT label as the empty string, so this retains every
// series that does not carry the dimension at all: inert for a producer that
// never configured it.
//
// Deliberately NOT equivalent to the attachment rule — the rule's remaining
// conditions (both endpoints resolved to a pod or a service; not the ingress
// chain's entry hop) are functions of topology and of the route index and have
// no label-level form. The queried population is a SUPERSET of the attached
// one; what matters is the other direction, which holds: every query-layer
// exclusion here is mirrored in pkg/build, so no eligible edge can have its
// failure/bucket series filtered away upstream and then read as error_rate=0.
const serviceGraphLinkExclusionSelector = `edge_relation!="link"`

// Render returns the PromQL string for the named query, parameterised by
// `window` (the bucketed end-start). Every series is queried at its bare
// name — there is no configurable metric-name prefix.
func Render(q Query, window time.Duration) string {
	w := FormatDuration(window)

	switch q {
	case QPodInfo:
		return fmt.Sprintf(`last_over_time(kube_pod_info[%s])`, w)
	case QNodeInfo:
		return fmt.Sprintf(`last_over_time(kube_node_info[%s])`, w)
	case QNodeAddresses:
		// ExternalIP preferred, InternalIP fallback; anchored alternation
		// selects exactly the two types — the topology reader applies the
		// preference at parse time.
		return fmt.Sprintf(`last_over_time(kube_node_status_addresses{type=~"ExternalIP|InternalIP"}[%s])`, w)
	case QPVCBindings:
		return fmt.Sprintf(`last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info[%s])`, w)
	case QNodeLabels:
		return fmt.Sprintf(`last_over_time(kube_node_labels[%s])`, w)
	case QServiceInfo:
		return fmt.Sprintf(`last_over_time(kube_service_info[%s])`, w)
	case QEndpointSliceEndpoints:
		return fmt.Sprintf(`last_over_time(kube_endpointslice_endpoints[%s])`, w)
	case QEndpointSliceLabels:
		return fmt.Sprintf(`last_over_time(kube_endpointslice_labels[%s])`, w)
	case QPodOwner:
		return fmt.Sprintf(`last_over_time(kube_pod_owner[%s])`, w)
	case QReplicaSetOwner:
		return fmt.Sprintf(`last_over_time(kube_replicaset_owner[%s])`, w)
	case QPVCInfo:
		return fmt.Sprintf(`last_over_time(kube_persistentvolumeclaim_info[%s])`, w)
	case QPodContainerInfo:
		// tlast_over_time (MetricsQL) — value is each series' last-sample timestamp
		// (unix seconds). A container that changed image in the window has one
		// series per image (image is a label); the resolver picks the image with
		// the greatest last-sample timestamp (the current one). last_over_time
		// would stamp every series at the eval instant, flattening recency.
		return fmt.Sprintf(`tlast_over_time(kube_pod_container_info[%s])`, w)
	case QNodeStatusCondition:
		// condition="Ready" is a fixed, request-invariant metric-selection
		// contract (anchored equality), not a caller filter — the reader reads
		// the active row's `status` label at parse time. The four other node
		// conditions (MemoryPressure/DiskPressure/PIDPressure/NetworkUnavailable)
		// are never surfaced, so they are excluded here.
		return fmt.Sprintf(`last_over_time(kube_node_status_condition{condition="Ready"}[%s])`, w)
	case QServiceAnnotations:
		return fmt.Sprintf(`last_over_time(kube_service_annotations[%s])`, w)
	case QPVCAnnotations:
		return fmt.Sprintf(`last_over_time(kube_persistentvolumeclaim_annotations[%s])`, w)
	case QVolumeReadOps:
		return fmt.Sprintf(`last_over_time(volume_read_ops[%s])`, w)
	case QVolumeWriteOps:
		return fmt.Sprintf(`last_over_time(volume_write_ops[%s])`, w)
	case QVolumeReadLatency:
		return fmt.Sprintf(`last_over_time(volume_read_latency[%s])`, w)
	case QVolumeWriteLatency:
		return fmt.Sprintf(`last_over_time(volume_write_latency[%s])`, w)
	case QVolumeReadData:
		return fmt.Sprintf(`last_over_time(volume_read_data[%s])`, w)
	case QVolumeWriteData:
		return fmt.Sprintf(`last_over_time(volume_write_data[%s])`, w)
	case QAggrStatus:
		return fmt.Sprintf(`last_over_time(aggr_new_status[%s])`, w)
	case QAggrSpaceUsed:
		return fmt.Sprintf(`last_over_time(aggr_space_used[%s])`, w)
	case QAggrSpaceTotal:
		return fmt.Sprintf(`last_over_time(aggr_space_total[%s])`, w)
	case QNetAppNodeStatus:
		return fmt.Sprintf(`last_over_time(node_new_status[%s])`, w)
	case QKubeletVolumeUsedBytes:
		return fmt.Sprintf(`last_over_time(kubelet_volume_stats_used_bytes[%s])`, w)
	case QKubeletVolumeCapacityBytes:
		return fmt.Sprintf(`last_over_time(kubelet_volume_stats_capacity_bytes[%s])`, w)
	case QServiceGraphTotal:
		// Service-graph metrics come from Alloy/Tempo, not kube-state-metrics.
		// The metric carries a single `cluster` label representing the trace
		// source (client-side) cluster; server-side cluster is recovered at
		// build time via the topology pod-UID index, not via PromQL.
		//
		// The fixed sentinel matcher (D30) drops the connector's virtual
		// `user` / `unknown` peers upstream. It is a metric-selection contract,
		// identical for every request — NOT a caller-supplied filter — so it
		// does not violate the "no filters pushed to PromQL" rule (D2 / D7).
		return fmt.Sprintf(`rate(traces_service_graph_request_total{%s}[%s])`, serviceGraphSentinelSelector, w)
	case QServiceGraphFailedTotal:
		// OPTIONAL Errors counter. Raw label granularity so failures join the
		// total series by exact identity (design D4).
		return fmt.Sprintf(`rate(traces_service_graph_request_failed_total{%s,%s}[%s])`,
			serviceGraphSentinelSelector, serviceGraphLinkExclusionSelector, w)
	case QServiceGraphServerSecondsBucket:
		// OPTIONAL Duration classic histogram.
		//
		// Read RAW — deliberately no upstream `sum by` (design D4). Once an
		// endpoint may be resolved from a peer address or a connection string,
		// no low-cardinality label subset identifies an edge: grouping by the
		// pod-pair triple merges every peer-resolved destination of one client
		// pod into a single bucket set, and recovering the distinction means
		// tracking the resolver's full peer-label set in the group-by forever.
		// Series identity has no such coupling. The cost is bucket-count
		// multiplied cardinality on the wire; the metric is OPTIONAL, so a store
		// that refuses the query degrades exactly as an absent one.
		return fmt.Sprintf(
			`rate(traces_service_graph_request_server_seconds_bucket{%s,%s}[%s])`,
			serviceGraphSentinelSelector, serviceGraphLinkExclusionSelector, w)
	case QClusterDiscovery:
		return fmt.Sprintf(`group by (cluster) (last_over_time(kube_node_info[%s]))`, w)
	case QUpProbe:
		return `up`
	}
	return ""
}
