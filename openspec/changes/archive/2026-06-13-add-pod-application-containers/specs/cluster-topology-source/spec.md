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

The three service/endpointslice families are OPTIONAL: when absent (kube-state-metrics not exporting services or endpointslices), the reader SHALL still build a valid topology, the service/endpoint indexes are simply empty, and connection-string resolution in the pod-service-graph reader degrades gracefully — `"://"` service endpoints that cannot be resolved against an empty index become `external/<label>` nodes.

`kube_persistentvolumeclaim_info` is likewise OPTIONAL: when absent — or when no series matches a given PVC — the reader SHALL still build a valid topology, the affected PVC entities carry no resolved StorageClass, and the Cytoscape serialiser nests those PVCs directly under their cluster group (`cluster > pvc`) instead of a StorageClass group.

`kube_pod_container_info` is likewise OPTIONAL: when absent — or when no series matches a given pod — the reader SHALL still build a valid topology, the affected pod entities carry no `containers` attribute, and the build does not fail. The `argocd_tracking_id` label on `kube_pod_owner` is likewise OPTIONAL: when absent, the affected pod entities carry no `application` attribute and the build does not fail.

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

### Requirement: Configurable upstream metric-name prefix

The topology reader SHALL prepend a single configurable prefix to every `kube_*` series name it queries, so deployments using a fork of kube-state-metrics or a custom exporter that re-publishes the same series under an organisational prefix (e.g. `o11y_kube_pod_info`) can be supported without forking the API server. The prefix SHALL be sourced from the `KSG_METRIC_PREFIX` environment variable or the `--metric-prefix` flag (flag wins over env when both are set). The default value SHALL be the empty string, preserving stock kube-state-metrics behaviour. The prefix SHALL be additive — appended verbatim before the existing series name; the existing `kube_*` suffix and the upstream label-name contract (`cluster`, `namespace`, `pod`, `uid`, `node`, `persistentvolumeclaim`, `label_*`, etc.) are unchanged. The prefix SHALL be validated against the Prometheus metric-name charset `^[a-zA-Z_:][a-zA-Z0-9_:]*$` when non-empty; an invalid value SHALL fail server startup. The trailing underscore (if any) is the operator's responsibility — the server does not inject one.

The same prefix SHALL apply to every kube-state-metrics-shaped series the reader consumes: `kube_pod_info`, `kube_node_info`, `kube_node_status_addresses`, `kube_pod_spec_volumes_persistentvolumeclaims_info`, `kube_node_labels`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`, `kube_pod_owner`, `kube_replicaset_owner`, `kube_persistentvolumeclaim_info`, `kube_pod_container_info`, and the `kube_node_info`-backed cluster discovery query. The upstream label-name contract those series carry is unchanged (`cluster`, `namespace`, `pod`, `uid`, `node`, `persistentvolumeclaim`, `storageclass`, `container`, `image`, `argocd_tracking_id`, `label_*`, `service`, `cluster_ip`, `endpointslice`, `address`, `hostname`, `targetref_kind`, `targetref_name`, `targetref_namespace`, `label_kubernetes_io_service_name`, etc.). The prefix SHALL NOT be applied to `traces_service_graph_request_total` (which is produced by a different exporter family) nor to the Prometheus-native `up{}` readiness probe.

#### Scenario: Default empty prefix preserves stock series names

- **WHEN** the server starts without `KSG_METRIC_PREFIX` or `--metric-prefix`
- **THEN** every topology query string contains the bare `kube_*` series name (e.g. `last_over_time(kube_pod_info[<window>])`) and no prefix is added

#### Scenario: Custom prefix from environment

- **WHEN** the server starts with `KSG_METRIC_PREFIX=o11y_`
- **THEN** the issued topology PromQL contains `last_over_time(o11y_kube_pod_info[<window>])`, `last_over_time(o11y_kube_node_info[<window>])`, `last_over_time(o11y_kube_node_status_addresses{type="ExternalIP"}[<window>])`, `last_over_time(o11y_kube_pod_spec_volumes_persistentvolumeclaims_info[<window>])`, `last_over_time(o11y_kube_node_labels[<window>])`, `last_over_time(o11y_kube_service_info[<window>])`, `last_over_time(o11y_kube_endpointslice_endpoints[<window>])`, `last_over_time(o11y_kube_endpointslice_labels[<window>])`, `last_over_time(o11y_kube_persistentvolumeclaim_info[<window>])`, and `tlast_over_time(o11y_kube_pod_container_info[<window>])` (the container query uses `tlast_over_time` so each image-variant series' value is its last-sample timestamp — see the "Pod container list attribute" requirement and design.md D-A4), AND the cluster-discovery query becomes `group by (cluster) (last_over_time(o11y_kube_node_info[<lookback>]))`

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

### Requirement: Pod container list attribute

The topology reader SHALL resolve each pod's **container list** from `kube_pod_container_info`, queried as `tlast_over_time(kube_pod_container_info[w])` so each series' value is its last-sample timestamp, and surface it on the pod entity as a typed, nullable `containers` attribute — an ordered list of `{name, image}` objects — serialised as `data.containers` (`omitempty`) and **never inside `labels`**. For each series matching a pod by `(cluster, namespace, pod)`, the reader SHALL emit one list element with `name` taken from the `container` label and `image` taken from the `image` label.

The list SHALL be ordered deterministically by `(name, image)` so the emitted entity is byte-identical across rebuilds. The reader SHALL skip any series whose `image` label is empty (it carries no information and must not mask a populated sibling). When a single container reports more than one non-empty `image` in the window — a mid-window image change, where each image is a DISTINCT series — the reader SHALL pick the image with the **greatest last-sample timestamp** (the current image), breaking exact-timestamp ties by the lexically-smallest `image` (determinism). `kube_pod_container_info` is OPTIONAL: when absent, or when no series matches a given pod, the reader SHALL emit a nil `containers` so `data.containers` is omitted entirely — it SHALL NOT emit an empty array or any container key in `labels`. This requirement introduces NO new node or edge type — the container list is a typed attribute on the existing `type="pod"` node (the same precedent as the `ipaddress` and `owner` attributes), keeping `labels` a strict `map[string]string` of typological metadata.

Note (design.md D-A4): the latest-image pick is reliable only for query windows near the real wall clock (the dominant case). For windows far in the past VictoriaMetrics returns only one image-variant series per container regardless of rollup, so the reader surfaces whatever single variant VM returns — never worse than a fixed deterministic pick.

#### Scenario: Pod with multiple containers

- **WHEN** `kube_pod_container_info{cluster="cluster-alpha", namespace="shop", pod="checkout-1", container="app", image="reg/app:1.2"}` and `kube_pod_container_info{cluster="cluster-alpha", namespace="shop", pod="checkout-1", container="sidecar", image="reg/proxy:0.9"}` are present
- **THEN** the emitted pod entity has `containers=[{name:"app", image:"reg/app:1.2"}, {name:"sidecar", image:"reg/proxy:0.9"}]` (ordered by `(name, image)`) and no container key in `labels`

#### Scenario: Container list ordering is deterministic

- **WHEN** the container series for a pod arrive in any upstream order
- **THEN** the emitted `containers` list is ordered by `(name, image)` and is byte-identical to the list produced by the same series in any other order

#### Scenario: Container changed image in the window — latest wins

- **WHEN** a single container has two `kube_pod_container_info` series for the same `(cluster, namespace, pod, container)` with different `image` values, the older `reg/app:1.0` last seen earlier and the newer `reg/app:2.0` last seen later (its `tlast_over_time` value is greater)
- **THEN** the emitted container carries `reg/app:2.0` (the image seen latest), regardless of upstream order and even though it is lexically larger; on an exact last-seen tie the lexically-smallest image wins deterministically

#### Scenario: Empty image is skipped

- **WHEN** a container has both an empty-`image` series and a populated one (e.g. `image=""` and `image="reg/app:1.4"`), and another container has only an empty-`image` series
- **THEN** the first container carries `image="reg/app:1.4"` (the empty image does not win the slot) and the empty-only container is omitted from `containers` entirely

#### Scenario: Pod with no container info

- **WHEN** no `kube_pod_container_info` series matches a given pod (e.g. a synthesised service-graph pod, or the metric absent for that pod)
- **THEN** the emitted pod entity has a nil `containers` (`data.containers` omitted entirely) and carries no container key in `labels`

#### Scenario: Container metric absent entirely

- **WHEN** the upstream contains `kube_pod_info` but no `kube_pod_container_info` series for the window
- **THEN** the reader produces a valid topology with no `containers` on any pod and does not fail the build

### Requirement: Pod ArgoCD Application attribute

The topology reader SHALL resolve each pod's **ArgoCD Application** from the `argocd_tracking_id` label carried on its `kube_pod_owner` series and surface it on the pod entity as a typed, nullable `application` attribute (a string), serialised as `data.application` (`omitempty`) and **never inside `labels`**. The reader SHALL read the `argocd_tracking_id` label value independently of which `kube_pod_owner` row wins the controller-owner pick (the Application is a pod-level fact that must survive even when no row is a controller).

The Application name SHALL be the substring of the `argocd_tracking_id` value **before the first `:`** (ArgoCD annotation-based tracking-id form `<app>:<group>/<kind>:<namespace>/<name>`); when the value contains no `:`, the **entire value** SHALL be surfaced verbatim. When more than one distinct non-empty `argocd_tracking_id` value is observed across a pod's `kube_pod_owner` rows, the reader SHALL pick the **lexically-smallest non-empty** value so the emitted entity is deterministic and order-free. When the label is absent or empty for a pod, the reader SHALL emit a nil `application` so `data.application` is omitted entirely — it SHALL NOT emit an empty string or any application key in `labels`. The `argocd_tracking_id` label is OPTIONAL: when no `kube_pod_owner` series carries it, the reader SHALL build a valid topology with no `application` on any pod and SHALL NOT fail the build. This requirement introduces NO new node or edge type — the Application is a typed attribute on the existing `type="pod"` node (the same precedent as the `owner` attribute), keeping `labels` a strict `map[string]string` of typological metadata.

#### Scenario: Pod with a full ArgoCD tracking-id

- **WHEN** `kube_pod_owner{cluster="cluster-alpha", namespace="shop", pod="checkout-1", owner_kind="ReplicaSet", owner_name="checkout-7f9c", owner_is_controller="true", argocd_tracking_id="checkout:apps/Deployment:shop/checkout"}` is present
- **THEN** the emitted pod entity has `application="checkout"` (the segment before the first `:`) and no `argocd_tracking_id` key in `labels`

#### Scenario: Pod with a bare Application name (no colon)

- **WHEN** a pod's `kube_pod_owner` series carries `argocd_tracking_id="checkout"` (no `:`)
- **THEN** the emitted pod entity has `application="checkout"` (the verbatim value)

#### Scenario: Pod with no ArgoCD label

- **WHEN** no `kube_pod_owner` series for a pod carries a non-empty `argocd_tracking_id` label
- **THEN** the emitted pod entity has a nil `application` (`data.application` omitted entirely) and carries no application key in `labels`

#### Scenario: ArgoCD label absent entirely

- **WHEN** the upstream contains `kube_pod_owner` series but none carry an `argocd_tracking_id` label for the window
- **THEN** the reader produces a valid topology with no `application` on any pod and does not fail the build
