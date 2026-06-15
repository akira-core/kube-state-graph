# cluster-topology-source — delta for add-k8s-node-status

## MODIFIED Requirements

### Requirement: Topology series consumed

The topology reader SHALL consume at minimum the following `kube-state-metrics` series, each carrying a `cluster` external label:

- `kube_pod_info{cluster, namespace, pod, uid, node, pod_ip, host_ip, ...}` (`pod_ip` and `host_ip` are surfaced when present)
- `kube_node_info{cluster, node, ...}`
- `kube_node_status_addresses{cluster, node, type="ExternalIP", address, ...}`
- `kube_pod_spec_volumes_persistentvolumeclaims_info{cluster, namespace, pod, volume, claim_name, ...}`
- `kube_node_labels{cluster, node, label_*, ...}`
- `kube_service_info{cluster, namespace, service, cluster_ip, ...}` (OPTIONAL — feeds the service/endpoint indexes)
- `kube_endpointslice_endpoints{cluster, namespace, endpointslice, address, targetref_kind, targetref_name, targetref_namespace, ...}` (OPTIONAL — feeds the service/endpoint indexes)
- `kube_endpointslice_labels{cluster, namespace, endpointslice, label_kubernetes_io_service_name, ...}` (OPTIONAL — joins each slice back to its owning service)
- `kube_pod_owner{cluster, namespace, pod, owner_kind, owner_name, owner_is_controller, argocd_tracking_id, ...}` (OPTIONAL — feeds the pod controller-owner labels and, via the `argocd_tracking_id` label, the pod ArgoCD Application attribute)
- `kube_replicaset_owner{cluster, namespace, replicaset, owner_kind, owner_name, ...}` (OPTIONAL — resolves a ReplicaSet pod owner up to its owning Deployment)
- `kube_persistentvolumeclaim_info{cluster, namespace, persistentvolumeclaim, storageclass, ...}` (OPTIONAL — feeds PVC StorageClass resolution and the StorageClass compound grouping)
- `kube_pod_container_info{cluster, namespace, pod, uid, container, image, ...}` (OPTIONAL — feeds the per-pod container list attribute; one series per container)
- `kube_node_status_condition{cluster, node, condition="Ready", status, ...}` (OPTIONAL — feeds the K8s node `ready_status` attribute; the `condition="Ready"` selector is a fixed, request-invariant metric-selection contract, and the `status` label carries `true`/`false`/`unknown`, with the active row's sample value being `1`)

The three service/endpointslice families are OPTIONAL: when absent (kube-state-metrics not exporting services or endpointslices), the reader SHALL still build a valid topology, the service/endpoint indexes are simply empty, and connection-string resolution in the pod-service-graph reader degrades gracefully — `"://"` service endpoints that cannot be resolved against an empty index become `external/<label>` nodes.

`kube_persistentvolumeclaim_info` is likewise OPTIONAL: when absent — or when no series matches a given PVC — the reader SHALL still build a valid topology, the affected PVC entities carry no resolved StorageClass, and the Cytoscape serialiser nests those PVCs directly under their cluster group (`cluster > pvc`) instead of a StorageClass group.

`kube_pod_container_info` is likewise OPTIONAL: when absent — or when no series matches a given pod — the reader SHALL still build a valid topology, the affected pod entities carry no `containers` attribute, and the build does not fail. The `argocd_tracking_id` label on `kube_pod_owner` is likewise OPTIONAL: when absent, the affected pod entities carry no `application` attribute and the build does not fail.

`kube_node_status_condition` is likewise OPTIONAL: when absent — or when no `condition="Ready"` series matches a given node — the reader SHALL still build a valid topology, the affected K8s node entities carry no `ready_status` attribute, and the build does not fail.

#### Scenario: All families queried

- **WHEN** a graph build runs against an upstream containing all families above
- **THEN** the reader emits exactly one PromQL query per family for the build, each evaluated at the bucketed `end` over the bucketed window

#### Scenario: Missing optional family

- **WHEN** the upstream contains `kube_pod_info` and `kube_node_info` but no `kube_node_labels` series for the window
- **THEN** the reader produces a valid topology with empty `labels` maps on node entities and does not fail the build

#### Scenario: Service and endpointslice metrics absent

- **WHEN** the upstream contains `kube_pod_info` and `kube_node_info` but no `kube_service_info`, `kube_endpointslice_endpoints`, or `kube_endpointslice_labels` series for the window
- **THEN** the reader produces a valid topology with empty service/endpoint indexes, the build does not fail, and any `"://"` connection-string endpoint that would otherwise resolve to an in-cluster service falls back to an `external/<label>` node with empty `labels`

#### Scenario: PVC info metric absent

- **WHEN** the upstream contains `kube_pod_spec_volumes_persistentvolumeclaims_info` but no `kube_persistentvolumeclaim_info` series for the window
- **THEN** the reader produces a valid topology in which every PVC entity has an empty StorageClass, the build does not fail, and the serialiser nests every PVC directly under its cluster group

#### Scenario: Container info metric absent

- **WHEN** the upstream contains `kube_pod_info` but no `kube_pod_container_info` series for the window
- **THEN** the reader produces a valid topology in which every pod entity carries no `containers` attribute, and the build does not fail

#### Scenario: Node status-condition metric absent

- **WHEN** the upstream contains `kube_node_info` but no `kube_node_status_condition` series for the window
- **THEN** the reader produces a valid topology in which every K8s node entity carries no `ready_status` attribute, and the build does not fail

### Requirement: Configurable upstream metric-name prefix

The topology reader SHALL prepend a single configurable prefix to every `kube_*` series name it queries, so deployments using a fork of kube-state-metrics or a custom exporter that re-publishes the same series under an organisational prefix (e.g. `o11y_kube_pod_info`) can be supported without forking the API server. The prefix SHALL be sourced from the `KSG_METRIC_PREFIX` environment variable or the `--metric-prefix` flag (flag wins over env when both are set). The default value SHALL be the empty string, preserving stock kube-state-metrics behaviour. The prefix SHALL be additive — appended verbatim before the existing series name; the existing `kube_*` suffix and the upstream label-name contract (`cluster`, `namespace`, `pod`, `uid`, `node`, `persistentvolumeclaim`, `label_*`, etc.) are unchanged. The prefix SHALL be validated against the Prometheus metric-name charset `^[a-zA-Z_:][a-zA-Z0-9_:]*$` when non-empty; an invalid value SHALL fail server startup. The trailing underscore (if any) is the operator's responsibility — the server does not inject one.

The same prefix SHALL apply to every kube-state-metrics-shaped series the reader consumes: `kube_pod_info`, `kube_node_info`, `kube_node_status_addresses`, `kube_pod_spec_volumes_persistentvolumeclaims_info`, `kube_node_labels`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`, `kube_pod_owner`, `kube_replicaset_owner`, `kube_persistentvolumeclaim_info`, `kube_pod_container_info`, `kube_node_status_condition`, and the `kube_node_info`-backed cluster discovery query. The upstream label-name contract those series carry is unchanged (`cluster`, `namespace`, `pod`, `uid`, `node`, `persistentvolumeclaim`, `storageclass`, `container`, `image`, `argocd_tracking_id`, `condition`, `status`, `label_*`, `service`, `cluster_ip`, `endpointslice`, `address`, `hostname`, `targetref_kind`, `targetref_name`, `targetref_namespace`, `label_kubernetes_io_service_name`, etc.). The prefix SHALL NOT be applied to `traces_service_graph_request_total` (which is produced by a different exporter family) nor to the Prometheus-native `up{}` readiness probe.

#### Scenario: Default empty prefix preserves stock series names

- **WHEN** the server starts without `KSG_METRIC_PREFIX` or `--metric-prefix`
- **THEN** every topology query string contains the bare `kube_*` series name (e.g. `last_over_time(kube_pod_info[<window>])`) and no prefix is added

#### Scenario: Custom prefix from environment

- **WHEN** the server starts with `KSG_METRIC_PREFIX=o11y_`
- **THEN** the issued topology PromQL contains `last_over_time(o11y_kube_pod_info[<window>])`, `last_over_time(o11y_kube_node_info[<window>])`, `last_over_time(o11y_kube_node_status_addresses{type="ExternalIP"}[<window>])`, `last_over_time(o11y_kube_pod_spec_volumes_persistentvolumeclaims_info[<window>])`, `last_over_time(o11y_kube_node_labels[<window>])`, `last_over_time(o11y_kube_service_info[<window>])`, `last_over_time(o11y_kube_endpointslice_endpoints[<window>])`, `last_over_time(o11y_kube_endpointslice_labels[<window>])`, `last_over_time(o11y_kube_persistentvolumeclaim_info[<window>])`, `tlast_over_time(o11y_kube_pod_container_info[<window>])` (the container query uses `tlast_over_time` so each image-variant series' value is its last-sample timestamp — see the "Pod container list attribute" requirement and design.md D-A4), and `last_over_time(o11y_kube_node_status_condition{condition="Ready"}[<window>])`, AND the cluster-discovery query becomes `group by (cluster) (last_over_time(o11y_kube_node_info[<lookback>]))`

#### Scenario: Prefix does not affect service-graph or probe queries

- **WHEN** the server starts with `KSG_METRIC_PREFIX=o11y_`
- **THEN** the service-graph reader still queries `rate(traces_service_graph_request_total[<window>])` (no prefix) and the `/readyz` probe still issues `up` (no prefix)

#### Scenario: Flag overrides environment variable

- **WHEN** the server starts with `KSG_METRIC_PREFIX=acme_` in the environment and `--metric-prefix=beta_` on the command line
- **THEN** the resulting topology queries reference `beta_kube_pod_info` and not `acme_kube_pod_info`

#### Scenario: Invalid prefix charset rejected at startup

- **WHEN** the server starts with `KSG_METRIC_PREFIX="o11y-bad!"`
- **THEN** `config.Validate` returns an error containing `metric-prefix` and the process exits non-zero before binding the listener

## ADDED Requirements

### Requirement: K8s node Ready-status attribute

The topology reader SHALL resolve each K8s node's **Ready status** from `kube_node_status_condition{condition="Ready"}`, queried as `last_over_time(kube_node_status_condition{condition="Ready"}[w])`, and surface it on the K8s node entity as a typed, nullable `ready_status` attribute (a string), serialised as `data.ready_status` (`omitempty`) and **never inside `labels`**. The `condition="Ready"` selector is a fixed, request-invariant metric-selection contract (the same precedent as the node-address `type` selector) — it is applied at the query layer for every build and is NOT a caller filter, preserving the "no caller filters pushed to PromQL" contract.

For each `(cluster, node)`, the reader SHALL read the `status` label of the **active** `condition="Ready"` series — the series whose sample value is `1` — and map it: `status="true"` → `"Ready"`, `status="false"` → `"NotReady"`, `status="unknown"` → `"Unknown"`. When more than one `condition="Ready"` series is active for the same `(cluster, node)` (a defensive case that does not occur in correct kube-state-metrics output, where exactly one Ready status is active at a time), the reader SHALL pick the lexically-smallest `status` label so the emitted value is deterministic and order-free.

**Absence is distinct from `"Unknown"`.** The reader SHALL emit a nil `ready_status` — so `data.ready_status` is omitted entirely — when the metric is absent, when a node has no `condition="Ready"` series, or when no Ready series is active. It SHALL NOT emit an empty string, and SHALL NOT substitute `"Unknown"` for missing data. The literal `"Unknown"` value SHALL be reserved for the genuine Kubernetes state in which the Ready condition's `status` label is `unknown` (the node's kubelet has stopped reporting). `kube_node_status_condition` is OPTIONAL: when absent the reader SHALL build a valid topology with no `ready_status` on any node and SHALL NOT fail the build. This requirement introduces NO new node or edge type — the Ready status is a typed attribute on the existing `type="node"` node (the same precedent as the `ipaddress` attribute), keeping `labels` a strict `map[string]string` of typological metadata.

#### Scenario: Ready node

- **WHEN** `kube_node_status_condition{cluster="cluster-alpha", node="worker-0", condition="Ready", status="true"}` has sample value `1` (and the `status="false"`/`status="unknown"` rows have value `0`)
- **THEN** the emitted K8s node entity has `ready_status="Ready"` and no status key in `labels`

#### Scenario: NotReady node

- **WHEN** the active `condition="Ready"` series for `(cluster="cluster-alpha", node="worker-0")` carries `status="false"` (value `1`)
- **THEN** the emitted K8s node entity has `ready_status="NotReady"`

#### Scenario: Unknown node (kubelet lost contact)

- **WHEN** the active `condition="Ready"` series for `(cluster="cluster-alpha", node="worker-0")` carries `status="unknown"` (value `1`)
- **THEN** the emitted K8s node entity has `ready_status="Unknown"`, which is distinct from a node that carries no `ready_status` at all

#### Scenario: Node with no Ready condition omits ready_status

- **WHEN** a node has no `condition="Ready"` series, or no Ready series is active for the window
- **THEN** the emitted K8s node entity has a nil `ready_status` (`data.ready_status` omitted entirely) and carries no status key in `labels`

#### Scenario: Status-condition value is order-free

- **WHEN** the three `condition="Ready"` status rows for a node arrive in any upstream order
- **THEN** the emitted `ready_status` is decided by the active (value `1`) row and is byte-identical regardless of upstream order
