# cluster-topology-source Delta: add-netapp-trident-pvc-labels

## ADDED Requirements

### Requirement: PVC PersistentVolume name and NetApp SVM labels

The topology reader SHALL resolve each PVC's bound **PersistentVolume name** and, when the NetApp Trident custom-resource metrics are present, the **ONTAP SVM** serving it, and surface both as additive entries in the PVC entity's `labels` map (strict `map[string]string`):

- `volumename` — the bound PV name, read from the `volumename` label of `kube_persistentvolumeclaim_info`, joined on `(cluster, namespace, persistentvolumeclaim)` to the PVC entity (the same join as PVC StorageClass resolution; the two label reads are per-field independent — a series may carry `volumename` without `storageclass` and vice versa). The key SHALL be set only when the resolved value is non-empty.
- `svm` — the NetApp SVM, resolved by chaining two joins rooted at the resolved PV name, both within the same cluster:
  1. `kube_tridentvolume_info`: the series whose `name` label equals the PV name yields its `backendUUID` label.
  2. `kube_tridentbackend_info`: the series whose `backendUUID` label equals that value yields its `svm` label.
  The key SHALL be set only when every link resolves to a non-empty value. By construction `svm` SHALL never be present without `volumename`.

The `volumename` key is DISTINCT from the existing `volume` key (the pod-spec volume name from `kube_pod_spec_volumes_persistentvolumeclaims_info`); both MAY coexist on one PVC entity and neither replaces the other.

`kube_tridentvolume_info` and `kube_tridentbackend_info` are NOT stock kube-state-metrics defaults — they come from a kube-state-metrics custom-resource-state configuration over the Trident `tridentvolumes` / `tridentbackends` CRDs (or a compatible exporter). The fixed label contract any exporter MUST honour, case-sensitive and verbatim: `kube_tridentvolume_info` carries `name` (the TridentVolume CR name, which equals the PV name under Trident's naming) and `backendUUID`; `kube_tridentbackend_info` carries `backendUUID` and `svm`. A series missing the `cluster` label SHALL be bucketed under `cluster="unknown"` (the same rule as every other topology series).

Both Trident metrics are OPTIONAL, and every link of the chain degrades gracefully: when a metric is absent, a join finds no match, or a required label is empty, the affected key(s) are simply omitted — the reader SHALL still build a valid topology, SHALL NOT fail the build, and SHALL NOT emit an empty-string label value. The chain ENRICHES PVC entities that exist via the pod→PVC binding metric; it SHALL NOT materialise a PVC on its own.

Resolution SHALL be deterministic: on duplicate series the reader SHALL pick the lexically-smallest non-empty value at each stage (`volumename` per `(cluster, namespace, claim)`, `backendUUID` per `(cluster, name)`, `svm` per `(cluster, backendUUID)`), so the emitted labels are a pure function of the upstream data, independent of vector order. Labels are baked at build time before any projection; no new node or edge type is introduced.

#### Scenario: Full chain resolves volumename and svm

- **WHEN** the upstream provides a PVC entity `cluster-alpha/db/data-mongo-0` with `kube_persistentvolumeclaim_info{cluster="cluster-alpha", namespace="db", persistentvolumeclaim="data-mongo-0", volumename="pvc-9f3a"}`, `kube_tridentvolume_info{cluster="cluster-alpha", name="pvc-9f3a", backendUUID="be-1234"}`, and `kube_tridentbackend_info{cluster="cluster-alpha", backendUUID="be-1234", svm="svm-prod"}`
- **THEN** the emitted PVC entity's `labels` contains `volumename="pvc-9f3a"` and `svm="svm-prod"`

#### Scenario: PV without a TridentVolume row yields volumename only

- **WHEN** a PVC resolves `volumename="pvc-9f3a"` but no `kube_tridentvolume_info` series with `name="pvc-9f3a"` exists in its cluster
- **THEN** the emitted PVC entity's `labels` contains `volumename="pvc-9f3a"` and no `svm` key, and the build does not fail

#### Scenario: TridentVolume without a matching backend yields no svm

- **WHEN** a PVC's chain reaches `backendUUID="be-1234"` but no `kube_tridentbackend_info` series with `backendUUID="be-1234"` exists in its cluster (or the matching series has an empty `svm` label)
- **THEN** the emitted PVC entity carries `volumename` but no `svm` key, and the build does not fail

#### Scenario: PVC info without volumename yields neither label

- **WHEN** a PVC's `kube_persistentvolumeclaim_info` series carries no (or an empty) `volumename` label, or no info series matches the PVC at all
- **THEN** the emitted PVC entity carries neither a `volumename` nor an `svm` key — no empty-string value is emitted — and the build does not fail

#### Scenario: Trident metrics absent entirely

- **WHEN** the upstream contains `kube_persistentvolumeclaim_info` (with `volumename` labels) but no `kube_tridentvolume_info` or `kube_tridentbackend_info` series for the window
- **THEN** the reader produces a valid topology in which PVC entities carry `volumename` but no `svm` key, and the build does not fail

#### Scenario: volumename is independent of storageclass on the same series

- **WHEN** a `kube_persistentvolumeclaim_info` series carries `volumename="pvc-9f3a"` but an empty `storageclass` label
- **THEN** the emitted PVC entity carries `labels.volumename="pvc-9f3a"` while no `pvc-to-storageclass` edge is emitted for it (and vice versa: a series with `storageclass` but no `volumename` drives the edge without the label)

#### Scenario: volume and volumename coexist

- **WHEN** a PVC entity derives `volume="data"` from the pod→PVC binding metric and resolves `volumename="pvc-9f3a"` from `kube_persistentvolumeclaim_info`
- **THEN** the emitted PVC entity's `labels` contains both `volume="data"` (the pod-spec volume name) and `volumename="pvc-9f3a"` (the bound PV name) as distinct keys

#### Scenario: Deterministic pick on duplicate series at every stage

- **WHEN** the upstream reports two `kube_tridentvolume_info` series for `(cluster-alpha, name="pvc-9f3a")` with `backendUUID="be-b"` and `backendUUID="be-a"`, and two `kube_tridentbackend_info` series for `(cluster-alpha, backendUUID="be-a")` with `svm="svm-b"` and `svm="svm-a"`
- **THEN** the chain resolves via `be-a` to `svm="svm-a"` (the lexically-smallest non-empty value at each stage) deterministically across rebuilds, independent of upstream vector order

## MODIFIED Requirements

### Requirement: Topology series consumed

The topology reader SHALL consume at minimum the following `kube-state-metrics` series, each carrying a `cluster` external label:

- `kube_pod_info{cluster, namespace, pod, uid, node, pod_ip, host_ip, ...}` (`pod_ip` and `host_ip` are surfaced when present)
- `kube_node_info{cluster, node, ...}`
- `kube_node_status_addresses{cluster, node, type=~"ExternalIP|InternalIP", address, ...}` (the anchored alternation selects exactly the two address types; ExternalIP is preferred and InternalIP is the fallback for the node `ipaddress` attribute)
- `kube_pod_spec_volumes_persistentvolumeclaims_info{cluster, namespace, pod, volume, claim_name, ...}`
- `kube_node_labels{cluster, node, label_*, ...}`
- `kube_service_info{cluster, namespace, service, cluster_ip, ...}` (OPTIONAL — feeds the service/endpoint indexes)
- `kube_endpointslice_endpoints{cluster, namespace, endpointslice, address, targetref_kind, targetref_name, targetref_namespace, ...}` (OPTIONAL — feeds the service/endpoint indexes)
- `kube_endpointslice_labels{cluster, namespace, endpointslice, label_kubernetes_io_service_name, ...}` (OPTIONAL — joins each slice back to its owning service)
- `kube_pod_owner{cluster, namespace, pod, owner_kind, owner_name, owner_is_controller, argocd_tracking_id, ...}` (OPTIONAL — feeds the pod controller-owner labels and, via the `argocd_tracking_id` label, the pod ArgoCD Application attribute)
- `kube_replicaset_owner{cluster, namespace, replicaset, owner_kind, owner_name, ...}` (OPTIONAL — resolves a ReplicaSet pod owner up to its owning Deployment)
- `kube_persistentvolumeclaim_info{cluster, namespace, persistentvolumeclaim, storageclass, volumename, ...}` (OPTIONAL — feeds PVC StorageClass resolution and the `pvc-to-storageclass` edge, and — via the `volumename` label — the PVC `volumename` label that roots the NetApp Trident SVM join)
- `kube_storageclass_info{cluster, storageclass, provisioner, storagePools, pool, fsType, fsName, ClusterID, selector, ...}` (OPTIONAL — feeds the real `type="storageclass"` node, its `provisioner` attribute, and its `parameters` object of NetApp/Ceph backing-storage values; the parameter labels are the operator's `--metric-labels-allowlist` responsibility)
- `kube_pod_container_info{cluster, namespace, pod, uid, container, image, ...}` (OPTIONAL — feeds the per-pod container list attribute; one series per container)
- `kube_node_status_condition{cluster, node, condition="Ready", status, ...}` (OPTIONAL — feeds the K8s node `ready_status` attribute; the `condition="Ready"` selector is a fixed, request-invariant metric-selection contract, and the `status` label carries `true`/`false`/`unknown` **matched case-insensitively** — stock kube-state-metrics lowercases the value, but an exporter re-publishing the raw Kubernetes `v1.ConditionStatus` enum verbatim emits `True`/`False`/`Unknown` — with the active row's sample value being `1`)
- `kube_persistentvolumeclaim_annotations{cluster, namespace, persistentvolumeclaim, annotation_argocd_argoproj_io_tracking_id, ...}` (OPTIONAL — feeds the PVC ArgoCD Application attribute; the `annotation_argocd_argoproj_io_tracking_id` label is kube-state-metrics' sanitised form of the `argocd.argoproj.io/tracking-id` annotation and requires the operator's `--metric-annotations-allowlist=persistentvolumeclaims=[argocd.argoproj.io/tracking-id]`)
- `kube_service_annotations{cluster, namespace, service, annotation_argocd_argoproj_io_tracking_id, ...}` (OPTIONAL — feeds the service ArgoCD Application attribute; the `annotation_argocd_argoproj_io_tracking_id` label requires the operator's `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id]`)
- `kube_tridentvolume_info{cluster, name, backendUUID, ...}` (OPTIONAL — NetApp Trident custom-resource series, from a kube-state-metrics custom-resource-state config or compatible exporter, NOT a stock KSM default; `name` is the TridentVolume CR name, equal to the bound PV name; feeds the PVC `svm` label chain)
- `kube_tridentbackend_info{cluster, backendUUID, svm, ...}` (OPTIONAL — NetApp Trident custom-resource series, same provenance; maps a Trident `backendUUID` to its ONTAP `svm`; feeds the PVC `svm` label chain)

The three service/endpointslice families are OPTIONAL: when absent (kube-state-metrics not exporting services or endpointslices), the reader SHALL still build a valid topology, the service/endpoint indexes are simply empty, and connection-string resolution in the pod-service-graph reader degrades gracefully — `"://"` service endpoints that cannot be resolved against an empty index become `external/<label>` nodes.

`kube_persistentvolumeclaim_info` is likewise OPTIONAL: when absent — or when no series matches a given PVC — the reader SHALL still build a valid topology, the affected PVC entities carry no resolved StorageClass, no `pvc-to-storageclass` edge is emitted for them, and the Cytoscape serialiser nests those PVCs under their namespace group (`cluster > namespace > pvc`) like any other PVC. The same absence also yields no `volumename` (and hence no `svm`) label on the affected PVC entities (see "PVC PersistentVolume name and NetApp SVM labels").

`kube_storageclass_info` is likewise OPTIONAL: when absent — or when a PVC's resolved StorageClass name has no matching `kube_storageclass_info` series — the reader SHALL still build a valid topology and SHALL NOT fail the build. A StorageClass node referenced by a PVC but absent from `kube_storageclass_info` SHALL be synthesised **bare** (`labels={cluster}`, no backing-storage attributes) so the `pvc-to-storageclass` edge has a real target (see "StorageClass entity from kube_storageclass_info").

`kube_pod_container_info` is likewise OPTIONAL: when absent — or when no series matches a given pod — the reader SHALL still build a valid topology, the affected pod entities carry no `containers` attribute, and the build does not fail. The `argocd_tracking_id` label on `kube_pod_owner` is likewise OPTIONAL: when absent, the affected pod entities carry no `application` attribute and the build does not fail.

`kube_node_status_condition` is likewise OPTIONAL: when absent — or when no `condition="Ready"` series matches a given node — the reader SHALL still build a valid topology, the affected K8s node entities carry no `ready_status` attribute, and the build does not fail.

`kube_persistentvolumeclaim_annotations` and `kube_service_annotations` are likewise OPTIONAL: when absent — or when no series matches a given `(cluster, namespace, claim)` / `(cluster, namespace, service)`, or its `annotation_argocd_argoproj_io_tracking_id` label is empty — the reader SHALL still build a valid topology, the affected PVC / service entities carry no `application` attribute and nest under their namespace group, and the build does not fail.

`kube_tridentvolume_info` and `kube_tridentbackend_info` are likewise OPTIONAL: when absent — the normal case on clusters without NetApp Trident or without the custom-resource-state config — or when no series matches a given join key, the reader SHALL still build a valid topology, the affected PVC entities carry no `svm` label (their `volumename` label is unaffected), and the build does not fail.

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
- **THEN** the reader produces a valid topology in which every PVC entity has an empty StorageClass and emits no `pvc-to-storageclass` edge, carries no `volumename` or `svm` label, the build does not fail, and the serialiser nests every PVC under its namespace group

#### Scenario: StorageClass info metric absent

- **WHEN** the upstream contains `kube_persistentvolumeclaim_info` (so PVCs resolve a StorageClass name) but no `kube_storageclass_info` series for the window
- **THEN** the reader produces a valid topology in which each referenced StorageClass is materialised as a bare node (`labels={cluster}`, no `provisioner`, no `parameters`), the `pvc-to-storageclass` edges are still emitted, and the build does not fail

#### Scenario: Container info metric absent

- **WHEN** the upstream contains `kube_pod_info` but no `kube_pod_container_info` series for the window
- **THEN** the reader produces a valid topology in which every pod entity carries no `containers` attribute, and the build does not fail

#### Scenario: Node status-condition metric absent

- **WHEN** the upstream contains `kube_node_info` but no `kube_node_status_condition` series for the window
- **THEN** the reader produces a valid topology in which every K8s node entity carries no `ready_status` attribute, and the build does not fail

#### Scenario: PVC/service annotation metrics absent

- **WHEN** the upstream contains `kube_persistentvolumeclaim_info` and `kube_service_info` but no `kube_persistentvolumeclaim_annotations` or `kube_service_annotations` series for the window
- **THEN** the reader produces a valid topology in which every PVC and service entity carries no `application` attribute and nests under its namespace group, and the build does not fail

#### Scenario: Trident custom-resource metrics absent

- **WHEN** the upstream contains `kube_persistentvolumeclaim_info` but no `kube_tridentvolume_info` or `kube_tridentbackend_info` series for the window
- **THEN** the reader produces a valid topology in which PVC entities carry no `svm` label (while `volumename` still resolves from `kube_persistentvolumeclaim_info`), and the build does not fail

### Requirement: Configurable upstream metric-name prefix

The topology reader SHALL prepend a single configurable prefix to every `kube_*` series name it queries, so deployments using a fork of kube-state-metrics or a custom exporter that re-publishes the same series under an organisational prefix (e.g. `o11y_kube_pod_info`) can be supported without forking the API server. The prefix SHALL be sourced from the `KSG_METRIC_PREFIX` environment variable or the `--metric-prefix` flag (flag wins over env when both are set). The default value SHALL be the empty string, preserving stock kube-state-metrics behaviour. The prefix SHALL be additive — appended verbatim before the existing series name; the existing `kube_*` suffix and the upstream label-name contract (`cluster`, `namespace`, `pod`, `uid`, `node`, `persistentvolumeclaim`, `label_*`, etc.) are unchanged. The prefix SHALL be validated against the Prometheus metric-name charset `^[a-zA-Z_:][a-zA-Z0-9_:]*$` when non-empty; an invalid value SHALL fail server startup. The trailing underscore (if any) is the operator's responsibility — the server does not inject one.

The same prefix SHALL apply to every kube-state-metrics-shaped series the reader consumes: `kube_pod_info`, `kube_node_info`, `kube_node_status_addresses`, `kube_pod_spec_volumes_persistentvolumeclaims_info`, `kube_node_labels`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`, `kube_pod_owner`, `kube_replicaset_owner`, `kube_persistentvolumeclaim_info`, `kube_storageclass_info`, `kube_pod_container_info`, `kube_node_status_condition`, `kube_persistentvolumeclaim_annotations`, `kube_service_annotations`, `kube_tridentvolume_info`, `kube_tridentbackend_info`, and the `kube_node_info`-backed cluster discovery query. The upstream label-name contract those series carry is unchanged (`cluster`, `namespace`, `pod`, `uid`, `node`, `persistentvolumeclaim`, `storageclass`, `provisioner`, `storagePools`, `pool`, `fsType`, `fsName`, `ClusterID`, `selector`, `container`, `image`, `argocd_tracking_id`, `annotation_argocd_argoproj_io_tracking_id`, `condition`, `status`, `label_*`, `service`, `cluster_ip`, `endpointslice`, `address`, `hostname`, `targetref_kind`, `targetref_name`, `targetref_namespace`, `label_kubernetes_io_service_name`, `volumename`, `name`, `backendUUID`, `svm`, etc.). The prefix SHALL NOT be applied to `traces_service_graph_request_total` (which is produced by a different exporter family) nor to the Prometheus-native `up{}` readiness probe.

#### Scenario: Default empty prefix preserves stock series names

- **WHEN** the server starts without `KSG_METRIC_PREFIX` or `--metric-prefix`
- **THEN** every topology query string contains the bare `kube_*` series name (e.g. `last_over_time(kube_pod_info[<window>])`) and no prefix is added

#### Scenario: Custom prefix from environment

- **WHEN** the server starts with `KSG_METRIC_PREFIX=o11y_`
- **THEN** the issued topology PromQL contains `last_over_time(o11y_kube_pod_info[<window>])`, `last_over_time(o11y_kube_node_info[<window>])`, `last_over_time(o11y_kube_node_status_addresses{type=~"ExternalIP|InternalIP"}[<window>])`, `last_over_time(o11y_kube_pod_spec_volumes_persistentvolumeclaims_info[<window>])`, `last_over_time(o11y_kube_node_labels[<window>])`, `last_over_time(o11y_kube_service_info[<window>])`, `last_over_time(o11y_kube_endpointslice_endpoints[<window>])`, `last_over_time(o11y_kube_endpointslice_labels[<window>])`, `last_over_time(o11y_kube_persistentvolumeclaim_info[<window>])`, `last_over_time(o11y_kube_storageclass_info[<window>])`, `tlast_over_time(o11y_kube_pod_container_info[<window>])` (the container query uses `tlast_over_time` so each image-variant series' value is its last-sample timestamp — see the "Pod container list attribute" requirement and design.md D-A4), `last_over_time(o11y_kube_node_status_condition{condition="Ready"}[<window>])`, `last_over_time(o11y_kube_persistentvolumeclaim_annotations[<window>])`, `last_over_time(o11y_kube_service_annotations[<window>])`, `last_over_time(o11y_kube_tridentvolume_info[<window>])`, `last_over_time(o11y_kube_tridentbackend_info[<window>])`, AND the cluster-discovery query becomes `group by (cluster) (last_over_time(o11y_kube_node_info[<lookback>]))`

#### Scenario: Prefix does not affect service-graph or probe queries

- **WHEN** the server starts with `KSG_METRIC_PREFIX=o11y_`
- **THEN** the service-graph reader still queries `rate(traces_service_graph_request_total[<window>])` (no prefix) and the `/readyz` probe still issues `up` (no prefix)

#### Scenario: Flag overrides environment variable

- **WHEN** the server starts with `KSG_METRIC_PREFIX=acme_` in the environment and `--metric-prefix=beta_` on the command line
- **THEN** the resulting topology queries reference `beta_kube_pod_info` and not `acme_kube_pod_info`

#### Scenario: Invalid prefix charset rejected at startup

- **WHEN** the server starts with `KSG_METRIC_PREFIX="o11y-bad!"`
- **THEN** `config.Validate` returns an error containing `metric-prefix` and the process exits non-zero before binding the listener
