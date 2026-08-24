# Upstream metrics used to build the graph

This is the operator catalog of every PromQL series `kube-state-graph` reads
from centralised VictoriaMetrics when it builds a `/v1/graph` response.

The names are the `Query` constants in `pkg/promql/queries.go`. There is **no
configurable metric-name prefix**: every series is queried at its **bare**
name. A scrape path that publishes `kube_*` (or Harvest) under an organisational
prefix silently yields an empty graph — see [`BREAKING.md`](BREAKING.md).

Install-side companions:

- kube-state-metrics collectors, RBAC, Helm values, allowlists →
  [`kube-state-metrics-preconditions.md`](kube-state-metrics-preconditions.md)
- NetApp Harvest relabel, hops, templates →
  [`netapp-harvest-preconditions.md`](netapp-harvest-preconditions.md)

## Inventory (41 series)

| Family | Count | Producer |
|---|---|---|
| kube-state-metrics | 22 | KSM |
| NetApp Harvest | 13 | Harvest |
| kubelet | 2 | kubelet `/metrics` |
| traces service-graph | 3 | Tempo / Alloy `servicegraph` connector (or compatible) |
| probe | 1 | Prometheus `up` |

`up` is a diagnostic, not graph data. The other 40 series are the graph inputs.

## Fan-out per `/v1/graph` request

```
GET /v1/graph?start=&end=&…
        │
        ├─ ReadTopology — 37 queries in parallel
        │     20 kube-state-metrics   abort the build on query error
        │      2 accumulating-cardinality annotation families
        │         (kube_replicaset_annotations, kube_job_annotations)
        │         log-and-continue (empty vector)
        │     13 Harvest + 2 kubelet  log-and-continue (empty vector)
        │
        ├─ up{}  — only when the request is unfiltered AND pods+nodes are empty
        │           (classifies outside_retention vs a genuine empty graph)
        │
        └─ ReadServiceGraph — 3 queries in parallel
              skipped entirely when the request is filtered AND the selector
              loaded neither pods nor services (those three series are never
              narrowed by ?cluster= / ?namespace= / ?az= / ?env=, so a
              mistyped filter would otherwise scan the whole estate)
```

v1 has no result cache: this fan-out runs on every request.

### Wrappers

| Family | PromQL wrapper |
|---|---|
| kube-state-metrics, Harvest, kubelet | `last_over_time(<metric>[<window>])` evaluated at `end` |
| `kube_pod_container_info` only | `tlast_over_time(...)` so each per-image series carries its last-sample timestamp |
| `traces_service_graph_*` | `rate(<metric>[<window>])` evaluated at `end` |
| `up` | bare `up` (no window) |

## Query error vs empty vector

The README **"Required?"** column answers "does an **empty vector** drop this
feature?". Query **errors** (timeout, 5xx, PromQL parse) are a separate axis:

| Legs | Query error | Empty vector |
|---|---|---|
| 20 kube-state-metrics topology queries | **Fails the build** (HTTP 5xx / mapped `build.Reason`) | Feature omitted (no pods, no IPs, no `://` services, …) |
| 2 accumulating-cardinality annotation families (`kube_replicaset_annotations`, `kube_job_annotations`) | Log-and-continue; empty vector — **except** a failure caused by the CALLER's own context (build timeout / client disconnect), which still fails the request (`optionalQueryFatal`). Cardinality grows with history (`revisionHistoryLimit` / Job history limits), not live object count. The degrade is silent in the response — alert on the self-metric `kube_state_graph_upstream_query_failures_total{query="kube_replicaset_annotations"}` / `{query="kube_job_annotations"}`, which `pkg/promql.Client` increments for every failed query regardless of which fetch helper called it, or on the `optional topology query failed` Warn | No `data.application` for bare-ReplicaSet / Job-owned pods — which also **reshapes the Cytoscape hierarchy** (those pods reparent from `…/application/<app>/controller/…` to `…/controller/…`, an `application` group node with no other member disappears, and a PVC that inherited its Application from such a pod re-inherits from a different mounter). A degraded `kube_job_annotations` additionally **suppresses the Job → CronJob hop** for that build (`topologyVectors.JobAnnotationsDegraded`): the hop is gated on "this Job carries no annotation of its own", which an unread family cannot establish, so following it would attribute a directly-managed Job's pod to its CronJob's Application — a wrong value, not a missing one. A genuinely annotation-less Job under an annotated CronJob therefore also loses its Application while the leg is degraded. Every degrade in this table is subtractive |
| `traces_service_graph_request_total` | **Fails the build** | No call edges; topology still returned |
| 13 Harvest + 2 kubelet | Log-and-continue; empty vector — **except** a failure caused by the CALLER's own context (build timeout / client disconnect), which still fails the request (`optionalQueryFatal`) | No NetApp chain / no PVC `usage` |
| `traces_service_graph_request_failed_total` | Log-and-continue | Measured edges omit `error_rate` (never reports `0`) |
| `traces_service_graph_request_server_seconds_bucket` | Log-and-continue | Measured edges omit `p90_server_ms` |
| `up` | Warn; skip `outside_retention` classification | n/a |

An **unfiltered** build with zero parsed pods **and** zero parsed nodes, plus a
healthy `up{}`, is classified `outside_retention` (HTTP 400). A **filtered**
build never probes `up{}`: zero rows means "nothing in scope" and returns HTTP
200 with empty `elements`.

## Which request filter reaches which series

Selector-level parameters (`cluster`, `namespace`, `az`, `env`) are rendered
into the upstream queries. Which dimension is allowed on which series is
hardcoded (`pkg/promql/queryDims`):

| Series | `az` | `env` | `cluster` | `namespace` |
|---|---|---|---|---|
| namespaced KSM (pod / owner / claim / Service / EndpointSlice / ReplicaSet — every `kube_*` series except the `kube_node_*` family) + kubelet volume stats | yes | yes | yes | yes |
| `kube_node_*` | yes | yes | yes | no (nodes have no namespace; they follow **by reference** from scheduled pods) |
| NetApp Harvest | yes | yes | **no** (Harvest `cluster` is the **ONTAP** cluster name) | no |
| `traces_service_graph_*`, `up` | no | no | no | no |

`az` / `env` match `--az-label` / `--env-label` (defaults `az` / `env`). Every
topology family that a live dimension actually reaches must carry those labels;
a family that does not matches nothing, and the default connectivity prune can
then empty the graph. A `selector_family_empty` warning fires when KSM matched
but a kubelet / Harvest family that the selector *can* narrow returned nothing.

The `cluster` value `unknown` is rendered `cluster=~"unknown|"` (literal plus
the empty alternative) because an absent `cluster` label and a literal
`unknown` land in the same bucket.

## Fixed selectors (request-invariant)

These matchers are metric-selection contracts, identical for every request.
They are **not** caller filters and are always rendered **before** any
`?az=` / `?env=` / `?cluster=` / `?namespace=` matchers.

| Series | Fixed selector | Why |
|---|---|---|
| `kube_node_status_addresses` | `type=~"ExternalIP\|InternalIP"` | ExternalIP wins; InternalIP is the fallback when the node has no ExternalIP |
| `kube_node_status_condition` | `condition="Ready"` | Only the Ready condition is surfaced as `data.ready_status` |
| six `qos_{read,write}_{ops,latency,data}` | `lun=""` | ONTAP also collects a per-LUN workload that carries the FlexVol's `volume_name`; without this matcher LUN traffic is double-counted. An empty-string matcher also matches series that omit `lun` |
| `traces_service_graph_request_total` | `client!~"user\|unknown",server!~"user"` | Drops the connector's virtual peers (`client="user"` / `"unknown"`, `server="user"`). Exact, case-sensitive. `server="unknown"` **is** admitted so the unknown-server peer-address ladder can run |
| two RED series | same sentinel **plus** `edge_relation!="link"` | Span-link series still produce an edge from `_total`, but they do not contribute rate / error / latency. An absent `edge_relation` label is retained (`!=` treats missing as `""`) |
| `kube_job_owner` | `owner_kind="CronJob",owner_is_controller="true"` | The reader (`resolveJobCronJobOwners`) keeps exactly those rows — Jobs owned by a CronJob controller. Every other owner kind, a non-controller row, or a missing `owner_is_controller` is discarded before it is keyed or counted |
| six controller-annotation families (`kube_{deployment,statefulset,daemonset,replicaset,job,cronjob}_annotations`) | `annotation_argocd_argoproj_io_tracking_id!=""` | The reader (`resolveApplications`) skips an empty or absent tracking-id before `keyOf`. PromQL treats a missing label as `""`, so an **allowlisted** family returns only its annotated objects instead of one series per workload object. It saves nothing on the **un-allowlisted** case — KSM short-circuits an un-allowlisted resource to an empty family (see [`kube-state-metrics-preconditions.md`](kube-state-metrics-preconditions.md)) |

## kube-state-metrics (22)

20 abort-on-error; `kube_replicaset_annotations` and `kube_job_annotations`
log-and-continue (accumulating cardinality —
harden-controller-annotation-legs D3). Empty vectors
degrade as in the last column.

| Metric | Graph role | Labels read | Empty vector |
|---|---|---|---|
| `kube_pod_info` | `type="pod"` nodes; `pod-to-node` via `node`; `data.ipaddress` from `pod_ip` (`host_ip` is not exported) | `cluster`, `namespace`, `pod`, `uid`, `node`, `pod_ip` | No pods |
| `kube_node_info` | `type="node"` nodes | `cluster`, `node` | No K8s nodes |
| `kube_node_status_addresses{type=~"ExternalIP\|InternalIP"}` | Node `data.ipaddress` — ExternalIP preferred, InternalIP fallback; other address types ignored; duplicate `(cluster, node)` within a type keeps the lexically-smallest address | `cluster`, `node`, `type`, `address` | Node has no `ipaddress` |
| `kube_node_labels` | Node `labels` (KSM `label_*` keys) | `cluster`, `node`, `label_*` | Node `labels` empty of those keys |
| `kube_node_status_condition{condition="Ready"}` | Node `data.ready_status` ∈ {`Ready`, `NotReady`, `Unknown`} from the **active** (`value=1`) Ready row; `status` matched case-insensitively (`true`/`True`). Omitted when no Ready data — **not** the same as `"Unknown"` (kubelet lost contact) | `cluster`, `node`, `condition`, `status` | Attribute omitted |
| `kube_pod_spec_volumes_persistentvolumeclaims_info` | PVC nodes + `pod-mounts-pvc` | `cluster`, `namespace`, `pod`, `persistentvolumeclaim`, `volume` | No PVCs / mount edges |
| `kube_persistentvolumeclaim_info` | PVC `data.storageclass` (policy name, never a node) + `labels.volumename` (bound PV name; Harvest join key) | `cluster`, `namespace`, `persistentvolumeclaim`, `storageclass`, `volumename` | No StorageClass / no Harvest join |
| `kube_service_info` | Service **index** for `://` resolution (D29). A `type="service"` node is materialised only when a connection-string / route-engine / peer-address path needs it — this series is not emitted as nodes on its own. `cluster_ip` → `data.ipaddress`; headless `"None"` omits it | `cluster`, `namespace`, `service`, `cluster_ip` | `://` endpoints fall back to `external` |
| `kube_endpointslice_endpoints` | `service-selects-pod` fan-out | `cluster`, `namespace`, `endpointslice`, `targetref_kind`, `targetref_namespace`, `targetref_name` | No backing-pod edges |
| `kube_endpointslice_labels` | EndpointSlice → Service name. **Requires** `--metric-labels-allowlist=endpointslices=[kubernetes.io/service-name]` (not a KSM default) | `cluster`, `namespace`, `endpointslice`, `label_kubernetes_io_service_name` | Same as above |
| `kube_pod_owner` | Pod `data.owner` = `{kind, name}` (ReplicaSet skipped to its Deployment). The resolved owner is also the **join key** for the pod's `data.application` — the tracking-id lives on the controller, never on the pod | `cluster`, `namespace`, `pod`, `owner_kind`, `owner_name`, `owner_is_controller` | No `owner`, and no pod Application (it is keyed on the controller) |
| `kube_replicaset_owner` | ReplicaSet → Deployment | `cluster`, `namespace`, `replicaset`, `owner_kind`, `owner_name` | ReplicaSet kept as owner |
| `kube_job_owner{owner_kind="CronJob",owner_is_controller="true"}` | Job → CronJob, **for pod `data.application` only** — the CronJob controller copies only `spec.jobTemplate.metadata` annotations onto the Jobs it creates, so ArgoCD's tracking-id never reaches a Job. Never alters `data.owner` | `cluster`, `namespace`, `job_name`, `owner_kind`, `owner_name`, `owner_is_controller` | CronJob-managed pods carry no Application |
| `kube_deployment_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Pod `data.application` for Deployment-owned pods (segment before the first `:` of the tracking-id), joined on `(cluster, namespace, kind, name)` against the pod's resolved controller. **Requires** `--metric-annotations-allowlist=deployments=[argocd.argoproj.io/tracking-id]` | `cluster`, `namespace`, `deployment`, `annotation_argocd_argoproj_io_tracking_id` | No Application for pods of that kind |
| `kube_statefulset_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Same, for StatefulSet-owned pods. **Requires** `--metric-annotations-allowlist=statefulsets=[…]` | `cluster`, `namespace`, `statefulset`, `annotation_argocd_argoproj_io_tracking_id` | Same |
| `kube_daemonset_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Same, for DaemonSet-owned pods. **Requires** `--metric-annotations-allowlist=daemonsets=[…]` | `cluster`, `namespace`, `daemonset`, `annotation_argocd_argoproj_io_tracking_id` | Same |
| `kube_replicaset_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Same, for pods whose owner stayed a bare ReplicaSet. **Requires** `--metric-annotations-allowlist=replicasets=[…]`. Query error **degrades** (log-and-continue) — cardinality accumulates with `revisionHistoryLimit` | `cluster`, `namespace`, `replicaset`, `annotation_argocd_argoproj_io_tracking_id` | Same |
| `kube_job_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Same, for Job-owned pods. Identity label is **`job_name`**, not `job`. **Requires** `--metric-annotations-allowlist=jobs=[…]`. Query error **degrades** (log-and-continue) — cardinality accumulates with Job history limits | `cluster`, `namespace`, `job_name`, `annotation_argocd_argoproj_io_tracking_id` | Same |
| `kube_cronjob_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Same, for pods reached through `kube_job_owner`. **Requires** `--metric-annotations-allowlist=cronjobs=[…]` | `cluster`, `namespace`, `cronjob`, `annotation_argocd_argoproj_io_tracking_id` | Same |
| `kube_pod_container_info` | Pod `data.containers` = `[{name, image}]`, ordered by `(name, image)`; latest-seen image wins on a mid-window change | `cluster`, `namespace`, `pod`, `container`, `image` | Attribute omitted |
| `kube_service_annotations` | Service `data.application`. **Requires** `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id]` | `cluster`, `namespace`, `service`, `annotation_argocd_argoproj_io_tracking_id` | Attribute omitted |
| `kube_persistentvolumeclaim_annotations` | PVC's **own** `data.application` (same parse). **Requires** `--metric-annotations-allowlist=persistentvolumeclaims=[argocd.argoproj.io/tracking-id]`. An app-less PVC additionally **inherits** the lexically-smallest Application among pods that mount it | `cluster`, `namespace`, `persistentvolumeclaim`, `annotation_argocd_argoproj_io_tracking_id` | Own annotation omitted; inheritance may still fill it |

The eleven KSM collectors that produce these 22 series, the ClusterRole, and
Helm values are in
[`kube-state-metrics-preconditions.md`](kube-state-metrics-preconditions.md).

## NetApp Harvest (13)

All 13 are OPTIONAL (log-and-continue). Harvest `cluster` is the ONTAP cluster
and is **never** used as a Kubernetes `?cluster=` matcher.

The storage join is three independently-degrading hops. Hops A and B are keyed
by PVC `volumename` (bound PV name) = Harvest `volume_name`; hop C rides on a
matched hop-B workload series and joins on the `(ontap_cluster, svm,
policy_group)` triple recovered from it — which is why a ceiling can never
appear without a measurement. `volume_name` is
**not** a stock Harvest label — the deployment must relabel it onto both the
volume-object series **and** the QoS workload series. See
[`netapp-harvest-preconditions.md`](netapp-harvest-preconditions.md).

| Metric | Hop | Graph role | Empty / miss |
|---|---|---|---|
| `volume_labels` | A — topology | Sole source of the storage *shape*: `pvc-to-netapp-aggr`, `netapp-aggr` / `netapp-node`, PVC `labels.svm`. Info series: sample **value discarded**, labels only (`cluster`, `node`, `aggr`, `svm`, `volume_name`) | No NetApp nodes, edges, or `svm` |
| `qos_read_ops` | B — I/O | `data.metrics.read_ops` (ops/s, verbatim — never `rate()`) | Edge kept, no I/O fields |
| `qos_write_ops` | B | `write_ops` | same |
| `qos_read_latency` | B | `read_latency_us` (average µs, verbatim) | same |
| `qos_write_latency` | B | `write_latency_us` | same |
| `qos_read_data` | B | `read_bytes_per_sec` (bytes/s, verbatim) | same |
| `qos_write_data` | B | `write_bytes_per_sec` | same |
| `qos_policy_fixed_max_throughput_iops` | C — ceiling | `max_iops`, joined on `(ontap_cluster, svm, policy_group)` recovered from hop B. Identity label `name`, `policy_group` fallback | No ceiling (never `0`). A ceiling cannot appear without a measurement |
| `qos_policy_fixed_max_throughput_mbps` | C | `max_bytes_per_sec` = mbps × 1048576 (the one converted value, so it shares the unit of `read_bytes_per_sec`) | same |
| `aggr_new_status` | — | Aggregate `data.health` (`online` if sample is `1`, else `degraded`; omitted if no series) | Attribute omitted |
| `aggr_space_used` | — | Aggregate `data.usage.used_bytes` | `usage` incomplete / omitted |
| `aggr_space_total` | — | Aggregate `data.usage.capacity_bytes` | same |
| `node_new_status` | — | Controller `data.health` (same mapping as aggregate) | Attribute omitted |

Coverage warnings (each gated on its **own** family having been read):
`netapp_volume_join_miss` (hop A miss or empty `aggr`), `netapp_qos_join_miss`
(edge drawn, no QoS match). No warning for a missing ceiling — a volume in no
policy group is normal.

## kubelet (2)

OPTIONAL (log-and-continue). Namespaced — `?cluster=` / `?namespace=` /
`?az=` / `?env=` all apply.

| Metric | Graph role | Labels read | Empty vector |
|---|---|---|---|
| `kubelet_volume_stats_used_bytes` | PVC `data.usage.used_bytes` | `cluster`, `namespace`, `persistentvolumeclaim` | `usage` incomplete / omitted |
| `kubelet_volume_stats_capacity_bytes` | PVC `data.usage.capacity_bytes` | same | same |

Per-field independent: a used-only or capacity-only result still populates
what it can.

## traces service-graph (3)

Never narrowed by request matchers. Skipped as a group when a **filtered**
build loaded neither pods nor services.

| Metric | Graph role | Empty / error |
|---|---|---|
| `traces_service_graph_request_total` | Trace-derived edges: `pod-calls-pod`, `pod-calls-service`. Also the source of on-demand `service-selects-pod` fan-out (with the EndpointSlice join). Denominator for `data.metrics.rate`. **Does not** exclude `edge_relation="link"` — those series still emit an edge | Query error fails the build. Empty vector ⇒ no call edges |
| `traces_service_graph_request_failed_total` | `data.metrics.error_rate` on measured edges. Joined to `_total` by **exact series identity** (minus `__name__`) | Omits `error_rate` (never `0`) |
| `traces_service_graph_request_server_seconds_bucket` | `data.metrics.p90_server_ms` (server-observed classic histogram). Read **raw** (no upstream `sum by`); joined by identity minus `le` | Omits `p90_server_ms` |

Labels read on the total series (the two RED series must carry the **same**
identity labels or they join nothing — logged as
`failed_total_label_set_mismatch` / `server_seconds_bucket_label_set_mismatch`):

- `cluster` — trace-source / **client-side** cluster. The server-side cluster
  is recovered by looking up `server_k8s_pod_uid` in the topology pod-UID index
- `client`, `server`
- `client_k8s_pod_uid`, `server_k8s_pod_uid`
- `edge_relation` — value `link` marks a span-link logical edge
  (`labels.relation="link"`); those series are excluded from the two RED
  selectors
- unknown-server peer-address ladder (checked in this order; first non-empty
  wins): `client_server_address`, `client_network_peer_address`,
  `client_net_peer_name`
- span-link broker derivation (always on, no knob): the mirrored SERVER-side
  peer labels `server_server_address`, `server_net_peer_name`,
  `server_dns_answers`, `server_server_port` — read on an `edge_relation="link"`
  series to derive the consumer side's broker node and mark the backing hop
  `labels.relation="transport"`. A collector that drops them still emits the
  `link` edge, but the `transport` marker is lost
- route-engine extras when `--route-store-dsn` is set: `client_dns_answers`
  (required to consult the engine), `client_server_port` / `client_net_peer_port`

`client_network_peer_port` is deliberately **not** read: the stable conventions
split the port into its own attribute, and a port takes part in neither peer
identification nor node naming.

`add_metric_suffixes` must stay on so the histogram is named
`..._server_seconds_bucket` (not `..._server_bucket`).

`data.metrics` (RED) is attached only when the edge is trace-derived, both
resolved endpoints are `type="pod"` or `type="service"`, the hop is not the
ingress-chain **entry** hop, and at least one contributing series is not
`edge_relation="link"`. No RED on: any `external` endpoint; synthesised edges
(`service-selects-pod`, ingress gateway-pod → backend, topology edges); an
all-link edge.

## Probe (1)

| Metric | When issued | Role |
|---|---|---|
| `up` | `/readyz`; and during an **unfiltered** build whose topology parsed to zero pods and zero nodes | Distinguishes `outside_retention` from "upstream healthy, window empty". Never materialises nodes or edges |

## Not PromQL — also not graph-input metrics

These are sometimes confused with the catalog above:

| Input | Role |
|---|---|
| ClickHouse Istio route store (`--route-store-dsn` / `KSG_ROUTE_STORE_DSN`) | **Opt-in.** Resolves a global FQDN peer to a Kubernetes Service. Off by default; setting the DSN also **requires** `--router-check-bin` (the native Envoy `router_check_tool`) or the server refuses to start. A miss degrades to the existing `external` node and **never** fails a build. Not a VictoriaMetrics series |
| `kube_state_graph_*` | The API's **own** Prometheus self-metrics (`/metrics`). Not read as graph input |
| `kube_storageclass_info`, `kube_tridentvolume_info`, `kube_tridentbackend_info` | **No longer queried.** The StorageClass node type is gone; Trident CRS metrics are unused. See [`BREAKING.md`](BREAKING.md) |

## Edge type → source metric

| Edge type | Source |
|---|---|
| `pod-to-node` | `kube_pod_info` (`node` label) |
| `pod-mounts-pvc` | `kube_pod_spec_volumes_persistentvolumeclaims_info` |
| `pvc-to-netapp-aggr` | Harvest `volume_labels` joined on `volume_name` = PVC `volumename` |
| `pod-calls-pod` | `traces_service_graph_request_total` |
| `pod-calls-service` | `traces_service_graph_request_total` (target resolved to a service node: `://` connection string, unknown-server peer, or route engine) |
| `service-selects-pod` | same total series + `kube_service_info` / `kube_endpointslice_*` (on-demand fan-out; not a raw series of its own) |

## Verifying the store

Against centralised VictoriaMetrics:

```promql
# Every name the builder can query (41, `up` included). The alternation is
# explicit on purpose: the useful answer is which name is MISSING from the
# output, and a `kube_.+` / `qos_.+` wildcard buries that under the hundreds
# of other series kube-state-metrics and Harvest publish.
count by (__name__) ({__name__=~"aggr_new_status|aggr_space_total|aggr_space_used|kube_cronjob_annotations|kube_daemonset_annotations|kube_deployment_annotations|kube_endpointslice_endpoints|kube_endpointslice_labels|kube_job_annotations|kube_job_owner|kube_node_info|kube_node_labels|kube_node_status_addresses|kube_node_status_condition|kube_persistentvolumeclaim_annotations|kube_persistentvolumeclaim_info|kube_pod_container_info|kube_pod_info|kube_pod_owner|kube_pod_spec_volumes_persistentvolumeclaims_info|kube_replicaset_annotations|kube_replicaset_owner|kube_service_annotations|kube_service_info|kube_statefulset_annotations|kubelet_volume_stats_capacity_bytes|kubelet_volume_stats_used_bytes|node_new_status|qos_policy_fixed_max_throughput_iops|qos_policy_fixed_max_throughput_mbps|qos_read_data|qos_read_latency|qos_read_ops|qos_write_data|qos_write_latency|qos_write_ops|traces_service_graph_request_failed_total|traces_service_graph_request_server_seconds_bucket|traces_service_graph_request_total|up|volume_labels"})

# The one REQUIRED non-default KSM label.
count(kube_endpointslice_labels{label_kubernetes_io_service_name!=""})

# Harvest join key present on both hops.
count(volume_labels{volume_name!=""})
count(qos_read_ops{volume_name!="",lun=""})
```

The code-side pins: `TestQueryDims_EveryQueryListed` fails if a `Query`
constant is missing from the dimension table; `TestReadTopology_FanOutLegCount`
fails if topology issues anything other than exactly 37 queries.
