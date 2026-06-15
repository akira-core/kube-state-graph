package promql

import (
	"fmt"
	"time"
)

// Query identifies one of the PromQL templates the build pipeline issues.
// The name is also used as the `query` label on self-metrics.
//
// The constant values are the bare upstream metric names (kube-state-metrics
// naming). At render time a configurable prefix may be prepended via
// Renderer; the Query string itself is not rewritten so that self-metric and
// span dimensions stay stable across deployments that differ only by prefix.
type Query string

const (
	QPodInfo           Query = "kube_pod_info"
	QNodeInfo          Query = "kube_node_info"
	QNodeAddresses     Query = "kube_node_status_addresses"
	QPVCBindings       Query = "kube_pod_spec_volumes_persistentvolumeclaims_info"
	QNodeLabels        Query = "kube_node_labels"
	QServiceGraphTotal Query = "traces_service_graph_request_total"
	QClusterDiscovery  Query = "cluster_discovery"
	QUpProbe           Query = "up"

	// Service / endpointslice topology (D29 connection-string resolution).
	// KSM-shaped, so prefix-aware via Renderer.
	QServiceInfo            Query = "kube_service_info"
	QEndpointSliceEndpoints Query = "kube_endpointslice_endpoints"
	QEndpointSliceLabels    Query = "kube_endpointslice_labels"

	// Pod controller-owner resolution (D34). KSM-shaped, so prefix-aware via
	// Renderer. kube_pod_owner gives a pod's owner refs; kube_replicaset_owner
	// resolves a ReplicaSet owner up to its owning Deployment (the ReplicaSet is
	// skipped). The owner_kind/owner_name/owner_is_controller labels are KSM
	// defaults (no --metric-labels-allowlist required). NOTE: the optional
	// `argocd_tracking_id` label the application resolver reads off kube_pod_owner
	// (resolvePodApplications) is NOT a KSM default — it is operator-provided
	// (e.g. via --metric-labels-allowlist or a relabel); absence degrades
	// gracefully to no `application` attribute. See design.md D-A4.
	QPodOwner        Query = "kube_pod_owner"
	QReplicaSetOwner Query = "kube_replicaset_owner"

	// PVC StorageClass resolution. KSM-shaped, so prefix-aware via Renderer.
	// kube_persistentvolumeclaim_info carries the `storageclass` label that the
	// pod→PVC binding metric (QPVCBindings) lacks; it is joined on
	// (cluster, namespace, persistentvolumeclaim) to enrich existing PVC nodes
	// (never to materialise new ones). OPTIONAL — a KSM default, no
	// --metric-labels-allowlist required.
	QPVCInfo Query = "kube_persistentvolumeclaim_info"

	// Pod container list resolution. KSM-shaped, so prefix-aware via Renderer.
	// kube_pod_container_info emits one series per container carrying the
	// `container` (name) and `image` labels; joined on (cluster, namespace, pod)
	// to enrich existing pod nodes with their typed `containers` attribute
	// (never new nodes). OPTIONAL — a KSM default, no --metric-labels-allowlist
	// required.
	QPodContainerInfo Query = "kube_pod_container_info"

	// K8s node Ready-status resolution. KSM-shaped, so prefix-aware via Renderer.
	// kube_node_status_condition emits one series per (condition, status) with
	// value 1 for the active combination; the topology reader reads the active
	// condition="Ready" row's `status` label (true/false/unknown) to enrich the
	// node's typed `ready_status` attribute (never a label, never a new node).
	// The condition="Ready" selector is a fixed, request-invariant metric-
	// selection contract (same class as the QNodeAddresses type selector and the
	// D30 sentinel selector), NOT a caller filter. OPTIONAL — a KSM default,
	// absence degrades gracefully to no `ready_status`.
	QNodeStatusCondition Query = "kube_node_status_condition"
)

// ClusterDiscoveryLookback is the fixed lookback used by /v1/clusters
// discovery. Sized to absorb transient KSM scrape gaps; not configurable.
const ClusterDiscoveryLookback = time.Hour

// serviceGraphSentinelSelector excludes the servicegraph connector's virtual
// peers from the service-graph series at the query layer (design.md D30): an
// uninstrumented caller surfaces as client="user", an unresolved peer as
// "unknown". These carry no pod UID and resolve to no actionable node.
//
// PromQL / MetricsQL `!~` is a fully-anchored RE2 match, so this drops a series
// only when the WHOLE client/server value is exactly "user" or "unknown"
// (case-sensitive). A connection-string label like "http://user/..." is NOT
// excluded (it is not equal to "user"), so D29 resolution is unaffected. The
// two matchers are ANDed in the selector, so a series is dropped when EITHER
// endpoint is a sentinel. The set is fixed — no operator knob (consistent with
// D29's removal of KSG_OTHERS_NAME_PATTERN).
//
// Dropping the WHOLE series is safe (no empty-UID gating needed): by the
// connector's contract a sentinel client/server value never co-occurs with a
// populated *_k8s_pod_uid — the virtual node exists precisely because there was
// no instrumented (hence no pod-identified) peer — so no real pod-resolved edge
// is ever discarded by this matcher.
//
// When the deferred numeric service-graph metrics
// (traces_service_graph_request_failed_total,
// traces_service_graph_request_server_seconds_bucket) are queried in a future
// revision, they MUST reuse this fragment so the edge set stays consistent
// across metric families.
const serviceGraphSentinelSelector = `client!~"user|unknown",server!~"user|unknown"`

// Renderer renders Query templates to PromQL strings, optionally prepending
// Prefix to every kube-state-metrics-shaped metric name. The prefix is
// additive — it is not applied to the service-graph metric
// (`traces_service_graph_request_total`, produced by a different exporter
// family) or to the Prometheus-native readiness probe (`up`).
//
// Zero value (`Renderer{}`) preserves stock kube-state-metrics behaviour.
type Renderer struct {
	Prefix string
}

// Render returns the PromQL string for the named query, parameterised by
// `window` (the bucketed end-start) and prefixed per r.Prefix where
// applicable.
func (r Renderer) Render(q Query, window time.Duration) string {
	w := FormatDuration(window)

	switch q {
	case QPodInfo:
		return fmt.Sprintf(`last_over_time(%skube_pod_info[%s])`, r.Prefix, w)
	case QNodeInfo:
		return fmt.Sprintf(`last_over_time(%skube_node_info[%s])`, r.Prefix, w)
	case QNodeAddresses:
		// ExternalIP preferred, InternalIP fallback; anchored alternation
		// selects exactly the two types — the topology reader applies the
		// preference at parse time.
		return fmt.Sprintf(`last_over_time(%skube_node_status_addresses{type=~"ExternalIP|InternalIP"}[%s])`, r.Prefix, w)
	case QPVCBindings:
		return fmt.Sprintf(`last_over_time(%skube_pod_spec_volumes_persistentvolumeclaims_info[%s])`, r.Prefix, w)
	case QNodeLabels:
		return fmt.Sprintf(`last_over_time(%skube_node_labels[%s])`, r.Prefix, w)
	case QServiceInfo:
		return fmt.Sprintf(`last_over_time(%skube_service_info[%s])`, r.Prefix, w)
	case QEndpointSliceEndpoints:
		return fmt.Sprintf(`last_over_time(%skube_endpointslice_endpoints[%s])`, r.Prefix, w)
	case QEndpointSliceLabels:
		return fmt.Sprintf(`last_over_time(%skube_endpointslice_labels[%s])`, r.Prefix, w)
	case QPodOwner:
		return fmt.Sprintf(`last_over_time(%skube_pod_owner[%s])`, r.Prefix, w)
	case QReplicaSetOwner:
		return fmt.Sprintf(`last_over_time(%skube_replicaset_owner[%s])`, r.Prefix, w)
	case QPVCInfo:
		return fmt.Sprintf(`last_over_time(%skube_persistentvolumeclaim_info[%s])`, r.Prefix, w)
	case QPodContainerInfo:
		// tlast_over_time (MetricsQL) — value is each series' last-sample timestamp
		// (unix seconds). A container that changed image in the window has one
		// series per image (image is a label); the resolver picks the image with
		// the greatest last-sample timestamp (the current one). last_over_time
		// would stamp every series at the eval instant, flattening recency.
		return fmt.Sprintf(`tlast_over_time(%skube_pod_container_info[%s])`, r.Prefix, w)
	case QNodeStatusCondition:
		// condition="Ready" is a fixed, request-invariant metric-selection
		// contract (anchored equality), not a caller filter — the reader reads
		// the active row's `status` label at parse time. The four other node
		// conditions (MemoryPressure/DiskPressure/PIDPressure/NetworkUnavailable)
		// are never surfaced, so they are excluded here.
		return fmt.Sprintf(`last_over_time(%skube_node_status_condition{condition="Ready"}[%s])`, r.Prefix, w)
	case QServiceGraphTotal:
		// Service-graph metrics come from Alloy/Tempo, not kube-state-metrics;
		// the configurable prefix deliberately does NOT apply here. The metric
		// carries a single `cluster` label representing the trace source
		// (client-side) cluster; server-side cluster is recovered at build time
		// via the topology pod-UID index, not via PromQL.
		//
		// The fixed sentinel matcher (D30) drops the connector's virtual
		// `user` / `unknown` peers upstream. It is a metric-selection contract,
		// identical for every request — NOT a caller-supplied filter — so it
		// does not violate the "no filters pushed to PromQL" rule (D2 / D7).
		return fmt.Sprintf(`rate(traces_service_graph_request_total{%s}[%s])`, serviceGraphSentinelSelector, w)
	case QClusterDiscovery:
		return fmt.Sprintf(`group by (cluster) (last_over_time(%skube_node_info[%s]))`, r.Prefix, w)
	case QUpProbe:
		// Prometheus-native; the configurable prefix does not apply.
		return `up`
	}
	return ""
}

// Render returns the PromQL string for the named query using a zero-prefix
// Renderer. Retained for tests and existing callers that do not need a
// configurable prefix; production code paths go through a Renderer held by
// build.Builder / api.Server.
func Render(q Query, window time.Duration) string {
	return Renderer{}.Render(q, window)
}
