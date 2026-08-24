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
	QUpProbe                         Query = "up"

	// Service / endpointslice topology (D29 connection-string resolution).
	QServiceInfo            Query = "kube_service_info"
	QEndpointSliceEndpoints Query = "kube_endpointslice_endpoints"
	QEndpointSliceLabels    Query = "kube_endpointslice_labels"

	// Pod controller-owner resolution (D34). kube_pod_owner gives a pod's
	// owner refs; kube_replicaset_owner resolves a ReplicaSet owner up to
	// its owning Deployment (the ReplicaSet is skipped). The
	// owner_kind/owner_name/owner_is_controller labels are KSM defaults
	// (no --metric-labels-allowlist required). kube_pod_owner carries NO
	// application information: ArgoCD stamps its tracking-id on the managed
	// controller, never on the pods a controller spawns, so the pod's
	// Application is resolved from the controller-annotation families below.
	QPodOwner        Query = "kube_pod_owner"
	QReplicaSetOwner Query = "kube_replicaset_owner"

	// Job → CronJob resolution, for ArgoCD Application resolution ONLY.
	// The Kubernetes CronJob controller copies only spec.jobTemplate.metadata
	// annotations onto the Jobs it creates — never the CronJob object's own
	// annotations — so ArgoCD's tracking-id never reaches a Job and a
	// CronJob-managed pod can only resolve its Application one level up.
	// Keyed (cluster, namespace, job_name); the identity label is `job_name`,
	// NOT `job` (kube-state-metrics avoids Prometheus' reserved target label).
	// This leg NEVER alters the pod `owner` attribute — resolvePodOwners does
	// not read it. OPTIONAL — a KSM default, absence degrades gracefully.
	QJobOwner Query = "kube_job_owner"

	// PVC StorageClass name + bound PV name. kube_persistentvolumeclaim_info
	// carries the `storageclass` and `volumename` labels that the pod→PVC
	// binding metric (QPVCBindings) lacks; joined on
	// (cluster, namespace, persistentvolumeclaim) to enrich existing PVC
	// nodes (never to materialise new ones). OPTIONAL — a KSM default, no
	// --metric-labels-allowlist required. The StorageClass name is the PVC's
	// own typed attribute (never a node); `volumename` roots the Harvest
	// volume join (see QVolumeLabels).
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
	// <app>:<group>/<kind>:<ns>/<name> grammar, so the Application is the segment
	// before the first ":". Joined on
	// (cluster, namespace, service) / (cluster, namespace, persistentvolumeclaim) to
	// enrich existing service / PVC nodes (never new nodes). OPTIONAL — the
	// annotation label requires the operator's --metric-annotations-allowlist;
	// absence degrades gracefully to no `application` attribute.
	QServiceAnnotations Query = "kube_service_annotations"
	QPVCAnnotations     Query = "kube_persistentvolumeclaim_annotations"

	// Pod ArgoCD Application resolution — the controller-annotation families.
	// ArgoCD stamps `argocd.argoproj.io/tracking-id` on the workload objects it
	// applies, never on the pods a controller spawns, so a pod's Application is
	// read from its CONTROLLER's annotation series. Each family carries the same
	// sanitised `annotation_argocd_argoproj_io_tracking_id` label and the same
	// <app>:<group>/<kind>:<ns>/<name> grammar as the service / PVC families
	// above, and each is keyed by its own resource-identity label:
	//
	//	Deployment   kube_deployment_annotations   `deployment`
	//	StatefulSet  kube_statefulset_annotations  `statefulset`
	//	DaemonSet    kube_daemonset_annotations    `daemonset`
	//	ReplicaSet   kube_replicaset_annotations   `replicaset`
	//	Job          kube_job_annotations          `job_name`   (NOT `job`)
	//	CronJob      kube_cronjob_annotations      `cronjob`
	//
	// The Job family's identity label is `job_name` — kube-state-metrics avoids
	// Prometheus' reserved `job` target label. Joined on
	// (cluster, namespace, kind, name) against the pod's already-resolved
	// controller owner, so the Deployment case needs no extra owner hop (the
	// D34 ReplicaSet skip has already collapsed it). ALL are OPTIONAL — each
	// annotation label requires the operator's
	// --metric-annotations-allowlist=<plural-resource>=[argocd.argoproj.io/tracking-id],
	// and because that flag is per-resource the degradation is per-family: an
	// operator may enable `deployments` alone. Absence degrades gracefully to no
	// `application` attribute.
	QDeploymentAnnotations  Query = "kube_deployment_annotations"
	QStatefulSetAnnotations Query = "kube_statefulset_annotations"
	QDaemonSetAnnotations   Query = "kube_daemonset_annotations"
	QReplicaSetAnnotations  Query = "kube_replicaset_annotations"
	QJobAnnotations         Query = "kube_job_annotations"
	QCronJobAnnotations     Query = "kube_cronjob_annotations"

	// NetApp Harvest volume label series — the SOLE source of the storage
	// topology (hop A of design.md D3): the pvc-to-netapp-aggr edge, the
	// netapp-aggr / netapp-node entities and the PVC `svm` label all derive
	// from this one series and nothing else. It is an info series: its sample
	// value is discarded, only its label set is consumed. The `volume_name`
	// label is a deployment relabel (not stock Harvest) mapping each FlexVol
	// to the Kubernetes PV it backs. OPTIONAL: a query error or empty vector
	// degrades to no storage topology, never a build failure.
	QVolumeLabels Query = "volume_labels"

	// NetApp Harvest QoS workload I/O (hop B of design.md D3). Harvest has
	// already resolved ONTAP base counters — ops are per-second, latency is an
	// average in microseconds, data is bytes per second — so these are read
	// verbatim via last_over_time, NEVER wrapped in rate(). Read at volume
	// granularity only (see qosVolumeGranularitySelector). OPTIONAL and
	// independent of the topology source: a miss leaves the claim's edge in
	// place carrying no metrics at all.
	QQoSReadOps      Query = "qos_read_ops"
	QQoSWriteOps     Query = "qos_write_ops"
	QQoSReadLatency  Query = "qos_read_latency"
	QQoSWriteLatency Query = "qos_write_latency"
	QQoSReadData     Query = "qos_read_data"
	QQoSWriteData    Query = "qos_write_data"

	// NetApp Harvest QoS fixed-policy ceilings (hop C of design.md D3),
	// joined on the (ontap_cluster, svm, policy_group) triple recovered from
	// the matched QoS workload series. Rendered bare — a policy object has no
	// LUN dimension. OPTIONAL: absence means "no declared ceiling", which is
	// never rendered as a number.
	QQoSPolicyFixedMaxIOPS Query = "qos_policy_fixed_max_throughput_iops"
	QQoSPolicyFixedMaxMBps Query = "qos_policy_fixed_max_throughput_mbps"

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

// queryDims is the hardcoded "which request dimension reaches which series"
// contract. It is a TABLE, not per-case logic, so the contract is greppable in
// one place and a new Query constant cannot silently default into accepting
// (or refusing) a caller filter: TestQueryDims_EveryQueryListed parses this
// file's Query constants and fails on a missing entry.
//
// The three groupings and their reasons:
//
//   - dimsNamespaced — pod-, claim-, Service- and EndpointSlice-scoped
//     kube-state-metrics series plus the kubelet volume-stats family. These
//     carry every label, so every dimension applies.
//   - dimsClusterScoped — the kube_node_* family: cluster-keyed, no namespace.
//     A namespace filter reaches K8s nodes by REFERENCE (a node is emitted when
//     an in-scope pod is scheduled on it), never by matcher.
//   - dimsHarvest — every NetApp Harvest series. Their `cluster` label is the
//     ONTAP cluster name, NOT a Kubernetes cluster, so pushing a Kubernetes
//     cluster value into it would match nothing; they carry no namespace
//     either. Narrowed by reference through the loaded claims' volumename join.
//   - dimsNone — the three traces_service_graph_* series (read in full for
//     every request: their `cluster` label is the unreliable trace-source
//     cluster and their namespace labels describe only the caller's own view,
//     so narrowing here would drop edges the loaded topology still needs) and
//     the up{} probe (it measures the store, not the data).
var queryDims = map[Query]dims{
	// kube-state-metrics — namespaced.
	QPodInfo:                dimsNamespaced,
	QPVCBindings:            dimsNamespaced,
	QServiceInfo:            dimsNamespaced,
	QEndpointSliceEndpoints: dimsNamespaced,
	QEndpointSliceLabels:    dimsNamespaced,
	QPodOwner:               dimsNamespaced,
	QReplicaSetOwner:        dimsNamespaced,
	QJobOwner:               dimsNamespaced,
	QPVCInfo:                dimsNamespaced,
	QPodContainerInfo:       dimsNamespaced,
	QServiceAnnotations:     dimsNamespaced,
	QPVCAnnotations:         dimsNamespaced,

	// kube-state-metrics — the controller-annotation families feeding the pod
	// ArgoCD Application. Namespaced like every other workload family, which is
	// not merely permissible but REQUIRED for correctness under a filter: a
	// pod's controller always lives in the pod's own (cluster, namespace), so
	// narrowing both sides by the same matcher keeps every join intact.
	QDeploymentAnnotations:  dimsNamespaced,
	QStatefulSetAnnotations: dimsNamespaced,
	QDaemonSetAnnotations:   dimsNamespaced,
	QReplicaSetAnnotations:  dimsNamespaced,
	QJobAnnotations:         dimsNamespaced,
	QCronJobAnnotations:     dimsNamespaced,

	// kubelet — namespaced.
	QKubeletVolumeUsedBytes:     dimsNamespaced,
	QKubeletVolumeCapacityBytes: dimsNamespaced,

	// kube-state-metrics — cluster-scoped (node objects have no namespace).
	QNodeInfo:            dimsClusterScoped,
	QNodeAddresses:       dimsClusterScoped,
	QNodeLabels:          dimsClusterScoped,
	QNodeStatusCondition: dimsClusterScoped,

	// NetApp Harvest — zone/environment only.
	QVolumeLabels:          dimsHarvest,
	QQoSReadOps:            dimsHarvest,
	QQoSWriteOps:           dimsHarvest,
	QQoSReadLatency:        dimsHarvest,
	QQoSWriteLatency:       dimsHarvest,
	QQoSReadData:           dimsHarvest,
	QQoSWriteData:          dimsHarvest,
	QQoSPolicyFixedMaxIOPS: dimsHarvest,
	QQoSPolicyFixedMaxMBps: dimsHarvest,
	QAggrStatus:            dimsHarvest,
	QAggrSpaceUsed:         dimsHarvest,
	QAggrSpaceTotal:        dimsHarvest,
	QNetAppNodeStatus:      dimsHarvest,

	// Read unfiltered for every request.
	QServiceGraphTotal:               dimsNone,
	QServiceGraphFailedTotal:         dimsNone,
	QServiceGraphServerSecondsBucket: dimsNone,
	QUpProbe:                         dimsNone,
}

// qosVolumeGranularitySelector keeps the Harvest QoS workload reads at volume
// granularity. A PromQL empty-string matcher also matches series carrying no
// such label at all, so the contract stays correct against a Harvest template
// that omits `lun` entirely.
const qosVolumeGranularitySelector = `lun=""`

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
// `window` (the bucketed end-start) and by the request-scoped `sel` selector.
// Every series is queried at its bare name — there is no configurable
// metric-name prefix.
//
// Two selector layers meet here and MUST NOT be conflated. A query's FIXED
// selector (`type=~"ExternalIP|InternalIP"`, `condition="Ready"`, `lun=""`,
// the service-graph sentinel and link matchers) is a request-invariant
// metric-selection contract, identical for every request. `sel` carries the
// caller's `az` / `env` / `cluster` / `namespace` values and reaches only the
// dimensions queryDims grants this query. The fixed part is always rendered
// FIRST, so composing the two never reorders an existing matcher.
//
// A zero Selector renders every query exactly as it was rendered before
// request-scoped selectors existed (pinned by
// TestRender_EmptySelectorMatchesBaseline).
func Render(q Query, window time.Duration, keys LabelKeys, sel Selector) string {
	w := FormatDuration(window)
	req := sel.render(queryDims[q], keys)

	// braces joins a query's fixed selector with the request matchers, omitting
	// the braces entirely when neither is present.
	braces := func(fixed string) string {
		switch {
		case fixed == "" && req == "":
			return ""
		case req == "":
			return "{" + fixed + "}"
		case fixed == "":
			return "{" + req + "}"
		default:
			return "{" + fixed + "," + req + "}"
		}
	}

	switch q {
	case QPodInfo:
		return fmt.Sprintf(`last_over_time(kube_pod_info%s[%s])`, braces(""), w)
	case QNodeInfo:
		return fmt.Sprintf(`last_over_time(kube_node_info%s[%s])`, braces(""), w)
	case QNodeAddresses:
		// ExternalIP preferred, InternalIP fallback; anchored alternation
		// selects exactly the two types — the topology reader applies the
		// preference at parse time.
		return fmt.Sprintf(`last_over_time(kube_node_status_addresses%s[%s])`, braces(`type=~"ExternalIP|InternalIP"`), w)
	case QPVCBindings:
		return fmt.Sprintf(`last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info%s[%s])`, braces(""), w)
	case QNodeLabels:
		return fmt.Sprintf(`last_over_time(kube_node_labels%s[%s])`, braces(""), w)
	case QServiceInfo:
		return fmt.Sprintf(`last_over_time(kube_service_info%s[%s])`, braces(""), w)
	case QEndpointSliceEndpoints:
		return fmt.Sprintf(`last_over_time(kube_endpointslice_endpoints%s[%s])`, braces(""), w)
	case QEndpointSliceLabels:
		return fmt.Sprintf(`last_over_time(kube_endpointslice_labels%s[%s])`, braces(""), w)
	case QPodOwner:
		return fmt.Sprintf(`last_over_time(kube_pod_owner%s[%s])`, braces(""), w)
	case QReplicaSetOwner:
		return fmt.Sprintf(`last_over_time(kube_replicaset_owner%s[%s])`, braces(""), w)
	case QJobOwner:
		return fmt.Sprintf(`last_over_time(kube_job_owner%s[%s])`, braces(""), w)
	case QPVCInfo:
		return fmt.Sprintf(`last_over_time(kube_persistentvolumeclaim_info%s[%s])`, braces(""), w)
	case QPodContainerInfo:
		// tlast_over_time (MetricsQL) — value is each series' last-sample timestamp
		// (unix seconds). A container that changed image in the window has one
		// series per image (image is a label); the resolver picks the image with
		// the greatest last-sample timestamp (the current one). last_over_time
		// would stamp every series at the eval instant, flattening recency.
		return fmt.Sprintf(`tlast_over_time(kube_pod_container_info%s[%s])`, braces(""), w)
	case QNodeStatusCondition:
		// condition="Ready" is a fixed, request-invariant metric-selection
		// contract (anchored equality), not a caller filter — the reader reads
		// the active row's `status` label at parse time. The four other node
		// conditions (MemoryPressure/DiskPressure/PIDPressure/NetworkUnavailable)
		// are never surfaced, so they are excluded here.
		return fmt.Sprintf(`last_over_time(kube_node_status_condition%s[%s])`, braces(`condition="Ready"`), w)
	case QServiceAnnotations:
		return fmt.Sprintf(`last_over_time(kube_service_annotations%s[%s])`, braces(""), w)
	case QPVCAnnotations:
		return fmt.Sprintf(`last_over_time(kube_persistentvolumeclaim_annotations%s[%s])`, braces(""), w)
	case QDeploymentAnnotations:
		return fmt.Sprintf(`last_over_time(kube_deployment_annotations%s[%s])`, braces(""), w)
	case QStatefulSetAnnotations:
		return fmt.Sprintf(`last_over_time(kube_statefulset_annotations%s[%s])`, braces(""), w)
	case QDaemonSetAnnotations:
		return fmt.Sprintf(`last_over_time(kube_daemonset_annotations%s[%s])`, braces(""), w)
	case QReplicaSetAnnotations:
		return fmt.Sprintf(`last_over_time(kube_replicaset_annotations%s[%s])`, braces(""), w)
	case QJobAnnotations:
		return fmt.Sprintf(`last_over_time(kube_job_annotations%s[%s])`, braces(""), w)
	case QCronJobAnnotations:
		return fmt.Sprintf(`last_over_time(kube_cronjob_annotations%s[%s])`, braces(""), w)
	case QVolumeLabels:
		return fmt.Sprintf(`last_over_time(volume_labels%s[%s])`, braces(""), w)
	case QQoSReadOps, QQoSWriteOps, QQoSReadLatency, QQoSWriteLatency, QQoSReadData, QQoSWriteData:
		// Volume-granularity restriction (design.md D2): ONTAP collects a
		// workload per LUN as well as per volume, and a LUN workload carries
		// the volume_name of its containing FlexVol once the deployment
		// relabel rule has run — an unrestricted read would sum LUN traffic on
		// top of volume traffic for the same claim. This is a fixed,
		// request-invariant metric-selection contract (same class as the D30
		// sentinel matcher and condition="Ready"), NOT a caller filter.
		return fmt.Sprintf(`last_over_time(%s%s[%s])`, q, braces(qosVolumeGranularitySelector), w)
	case QQoSPolicyFixedMaxIOPS:
		return fmt.Sprintf(`last_over_time(qos_policy_fixed_max_throughput_iops%s[%s])`, braces(""), w)
	case QQoSPolicyFixedMaxMBps:
		return fmt.Sprintf(`last_over_time(qos_policy_fixed_max_throughput_mbps%s[%s])`, braces(""), w)
	case QAggrStatus:
		return fmt.Sprintf(`last_over_time(aggr_new_status%s[%s])`, braces(""), w)
	case QAggrSpaceUsed:
		return fmt.Sprintf(`last_over_time(aggr_space_used%s[%s])`, braces(""), w)
	case QAggrSpaceTotal:
		return fmt.Sprintf(`last_over_time(aggr_space_total%s[%s])`, braces(""), w)
	case QNetAppNodeStatus:
		return fmt.Sprintf(`last_over_time(node_new_status%s[%s])`, braces(""), w)
	case QKubeletVolumeUsedBytes:
		return fmt.Sprintf(`last_over_time(kubelet_volume_stats_used_bytes%s[%s])`, braces(""), w)
	case QKubeletVolumeCapacityBytes:
		return fmt.Sprintf(`last_over_time(kubelet_volume_stats_capacity_bytes%s[%s])`, braces(""), w)
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
		return fmt.Sprintf(`rate(traces_service_graph_request_total%s[%s])`, braces(serviceGraphSentinelSelector), w)
	case QServiceGraphFailedTotal:
		// OPTIONAL Errors counter. Raw label granularity so failures join the
		// total series by exact identity (design D4).
		return fmt.Sprintf(`rate(traces_service_graph_request_failed_total%s[%s])`,
			braces(serviceGraphSentinelSelector+","+serviceGraphLinkExclusionSelector), w)
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
			`rate(traces_service_graph_request_server_seconds_bucket%s[%s])`,
			braces(serviceGraphSentinelSelector+","+serviceGraphLinkExclusionSelector), w)
	case QUpProbe:
		return `up`
	}
	return ""
}
