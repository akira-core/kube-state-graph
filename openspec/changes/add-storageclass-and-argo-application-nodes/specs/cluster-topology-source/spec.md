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
- `kube_persistentvolumeclaim_info{cluster, namespace, persistentvolumeclaim, storageclass, ...}` (OPTIONAL — feeds PVC StorageClass resolution and the `pvc-to-storageclass` edge)
- `kube_storageclass_info{cluster, storageclass, provisioner, storagePools, pool, fsType, fsName, ClusterID, selector, ...}` (OPTIONAL — feeds the real `type="storageclass"` node, its `provisioner` attribute, and its `parameters` object of NetApp/Ceph backing-storage values; the parameter labels are the operator's `--metric-labels-allowlist` responsibility)
- `kube_pod_container_info{cluster, namespace, pod, uid, container, image, ...}` (OPTIONAL — feeds the per-pod container list attribute; one series per container)
- `kube_node_status_condition{cluster, node, condition="Ready", status, ...}` (OPTIONAL — feeds the K8s node `ready_status` attribute; the `condition="Ready"` selector is a fixed, request-invariant metric-selection contract, and the `status` label carries `true`/`false`/`unknown` **matched case-insensitively** — stock kube-state-metrics lowercases the value, but an exporter re-publishing the raw Kubernetes `v1.ConditionStatus` enum verbatim emits `True`/`False`/`Unknown` — with the active row's sample value being `1`)
- `kube_persistentvolumeclaim_annotations{cluster, namespace, persistentvolumeclaim, annotation_argocd_argoproj_io_tracking_id, ...}` (OPTIONAL — feeds the PVC ArgoCD Application attribute; the `annotation_argocd_argoproj_io_tracking_id` label is kube-state-metrics' sanitised form of the `argocd.argoproj.io/tracking-id` annotation and requires the operator's `--metric-annotations-allowlist=persistentvolumeclaims=[argocd.argoproj.io/tracking-id]`)
- `kube_service_annotations{cluster, namespace, service, annotation_argocd_argoproj_io_tracking_id, ...}` (OPTIONAL — feeds the service ArgoCD Application attribute; the `annotation_argocd_argoproj_io_tracking_id` label requires the operator's `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id]`)

The three service/endpointslice families are OPTIONAL: when absent (kube-state-metrics not exporting services or endpointslices), the reader SHALL still build a valid topology, the service/endpoint indexes are simply empty, and connection-string resolution in the pod-service-graph reader degrades gracefully — `"://"` service endpoints that cannot be resolved against an empty index become `external/<label>` nodes.

`kube_persistentvolumeclaim_info` is likewise OPTIONAL: when absent — or when no series matches a given PVC — the reader SHALL still build a valid topology, the affected PVC entities carry no resolved StorageClass, no `pvc-to-storageclass` edge is emitted for them, and the Cytoscape serialiser nests those PVCs under their namespace group (`cluster > namespace > pvc`) like any other PVC.

`kube_storageclass_info` is likewise OPTIONAL: when absent — or when a PVC's resolved StorageClass name has no matching `kube_storageclass_info` series — the reader SHALL still build a valid topology and SHALL NOT fail the build. A StorageClass node referenced by a PVC but absent from `kube_storageclass_info` SHALL be synthesised **bare** (`labels={cluster}`, no backing-storage attributes) so the `pvc-to-storageclass` edge has a real target (see "StorageClass entity from kube_storageclass_info").

`kube_pod_container_info` is likewise OPTIONAL: when absent — or when no series matches a given pod — the reader SHALL still build a valid topology, the affected pod entities carry no `containers` attribute, and the build does not fail. The `argocd_tracking_id` label on `kube_pod_owner` is likewise OPTIONAL: when absent, the affected pod entities carry no `application` attribute and the build does not fail.

`kube_node_status_condition` is likewise OPTIONAL: when absent — or when no `condition="Ready"` series matches a given node — the reader SHALL still build a valid topology, the affected K8s node entities carry no `ready_status` attribute, and the build does not fail.

`kube_persistentvolumeclaim_annotations` and `kube_service_annotations` are likewise OPTIONAL: when absent — or when no series matches a given `(cluster, namespace, claim)` / `(cluster, namespace, service)`, or its `annotation_argocd_argoproj_io_tracking_id` label is empty — the reader SHALL still build a valid topology, the affected PVC / service entities carry no `application` attribute and nest under their namespace group, and the build does not fail.

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
- **THEN** the reader produces a valid topology in which every PVC entity has an empty StorageClass and emits no `pvc-to-storageclass` edge, the build does not fail, and the serialiser nests every PVC under its namespace group

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

### Requirement: Configurable upstream metric-name prefix

The topology reader SHALL prepend a single configurable prefix to every `kube_*` series name it queries, so deployments using a fork of kube-state-metrics or a custom exporter that re-publishes the same series under an organisational prefix (e.g. `o11y_kube_pod_info`) can be supported without forking the API server. The prefix SHALL be sourced from the `KSG_METRIC_PREFIX` environment variable or the `--metric-prefix` flag (flag wins over env when both are set). The default value SHALL be the empty string, preserving stock kube-state-metrics behaviour. The prefix SHALL be additive — appended verbatim before the existing series name; the existing `kube_*` suffix and the upstream label-name contract (`cluster`, `namespace`, `pod`, `uid`, `node`, `persistentvolumeclaim`, `label_*`, etc.) are unchanged. The prefix SHALL be validated against the Prometheus metric-name charset `^[a-zA-Z_:][a-zA-Z0-9_:]*$` when non-empty; an invalid value SHALL fail server startup. The trailing underscore (if any) is the operator's responsibility — the server does not inject one.

The same prefix SHALL apply to every kube-state-metrics-shaped series the reader consumes: `kube_pod_info`, `kube_node_info`, `kube_node_status_addresses`, `kube_pod_spec_volumes_persistentvolumeclaims_info`, `kube_node_labels`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`, `kube_pod_owner`, `kube_replicaset_owner`, `kube_persistentvolumeclaim_info`, `kube_storageclass_info`, `kube_pod_container_info`, `kube_node_status_condition`, `kube_persistentvolumeclaim_annotations`, `kube_service_annotations`, and the `kube_node_info`-backed cluster discovery query. The upstream label-name contract those series carry is unchanged (`cluster`, `namespace`, `pod`, `uid`, `node`, `persistentvolumeclaim`, `storageclass`, `provisioner`, `storagePools`, `pool`, `fsType`, `fsName`, `ClusterID`, `selector`, `container`, `image`, `argocd_tracking_id`, `annotation_argocd_argoproj_io_tracking_id`, `condition`, `status`, `label_*`, `service`, `cluster_ip`, `endpointslice`, `address`, `hostname`, `targetref_kind`, `targetref_name`, `targetref_namespace`, `label_kubernetes_io_service_name`, etc.). The prefix SHALL NOT be applied to `traces_service_graph_request_total` (which is produced by a different exporter family) nor to the Prometheus-native `up{}` readiness probe.

#### Scenario: Default empty prefix preserves stock series names

- **WHEN** the server starts without `KSG_METRIC_PREFIX` or `--metric-prefix`
- **THEN** every topology query string contains the bare `kube_*` series name (e.g. `last_over_time(kube_pod_info[<window>])`) and no prefix is added

#### Scenario: Custom prefix from environment

- **WHEN** the server starts with `KSG_METRIC_PREFIX=o11y_`
- **THEN** the issued topology PromQL contains `last_over_time(o11y_kube_pod_info[<window>])`, `last_over_time(o11y_kube_node_info[<window>])`, `last_over_time(o11y_kube_node_status_addresses{type=~"ExternalIP|InternalIP"}[<window>])`, `last_over_time(o11y_kube_pod_spec_volumes_persistentvolumeclaims_info[<window>])`, `last_over_time(o11y_kube_node_labels[<window>])`, `last_over_time(o11y_kube_service_info[<window>])`, `last_over_time(o11y_kube_endpointslice_endpoints[<window>])`, `last_over_time(o11y_kube_endpointslice_labels[<window>])`, `last_over_time(o11y_kube_persistentvolumeclaim_info[<window>])`, `last_over_time(o11y_kube_storageclass_info[<window>])`, `tlast_over_time(o11y_kube_pod_container_info[<window>])` (the container query uses `tlast_over_time` so each image-variant series' value is its last-sample timestamp — see the "Pod container list attribute" requirement and design.md D-A4), `last_over_time(o11y_kube_node_status_condition{condition="Ready"}[<window>])`, `last_over_time(o11y_kube_persistentvolumeclaim_annotations[<window>])`, `last_over_time(o11y_kube_service_annotations[<window>])`, AND the cluster-discovery query becomes `group by (cluster) (last_over_time(o11y_kube_node_info[<lookback>]))`

#### Scenario: Prefix does not affect service-graph or probe queries

- **WHEN** the server starts with `KSG_METRIC_PREFIX=o11y_`
- **THEN** the service-graph reader still queries `rate(traces_service_graph_request_total[<window>])` (no prefix) and the `/readyz` probe still issues `up` (no prefix)

#### Scenario: Flag overrides environment variable

- **WHEN** the server starts with `KSG_METRIC_PREFIX=acme_` in the environment and `--metric-prefix=beta_` on the command line
- **THEN** the resulting topology queries reference `beta_kube_pod_info` and not `acme_kube_pod_info`

#### Scenario: Invalid prefix charset rejected at startup

- **WHEN** the server starts with `KSG_METRIC_PREFIX="o11y-bad!"`
- **THEN** `config.Validate` returns an error containing `metric-prefix` and the process exits non-zero before binding the listener

### Requirement: Cluster-scoped IDs

The reader SHALL produce topology entities whose stable identifiers are cluster-scoped:

- Pod ID = `<cluster>/<pod-uid>` (composite of `cluster` and `uid` labels).
- K8s node ID = `<cluster>/<node>` (composite of `cluster` and `node` labels).
- PVC ID = `<cluster>/<namespace>/<claim_name>`.
- StorageClass ID = `<cluster>/storageclass/<storageclass>` (composite of `cluster` and the `storageclass` name label; StorageClass is a cluster-scoped Kubernetes object whose name is not globally unique across clusters).

#### Scenario: Two clusters with same node name

- **WHEN** `kube_node_info{cluster="cluster-alpha", node="worker-0"}` and `kube_node_info{cluster="cluster-beta", node="worker-0"}` both exist in the window
- **THEN** the reader emits two distinct K8s node entities with IDs `cluster-alpha/worker-0` and `cluster-beta/worker-0`

#### Scenario: Pod ID derives from uid label

- **WHEN** `kube_pod_info{cluster="cluster-alpha", uid="abc-123", ...}` is present
- **THEN** the reader emits a pod entity with ID `cluster-alpha/abc-123`

#### Scenario: Two clusters with same StorageClass name

- **WHEN** `kube_storageclass_info{cluster="cluster-alpha", storageclass="gp3"}` and `kube_storageclass_info{cluster="cluster-beta", storageclass="gp3"}` both exist in the window
- **THEN** the reader emits two distinct StorageClass entities with IDs `cluster-alpha/storageclass/gp3` and `cluster-beta/storageclass/gp3`

### Requirement: PVC StorageClass resolution

The topology reader SHALL resolve each PVC's StorageClass **name** from `kube_persistentvolumeclaim_info`, joining on `(cluster, namespace, persistentvolumeclaim)` to the PVC entity (which derives from `kube_pod_spec_volumes_persistentvolumeclaims_info`, where the claim name comes from the `claim_name` label). The resolved StorageClass name SHALL drive a directed `pvc-to-storageclass` edge from the PVC node to the StorageClass node `<cluster>/storageclass/<name>` (see "Topology relationship edges"). The name SHALL NOT be added to the PVC `labels` map and SHALL NOT be serialised as a standalone PVC attribute — there SHALL be no `data.storageclass` field on the `type="pvc"` node. The StorageClass surfaces in the wire output as the real `type="storageclass"` node and the `pvc-to-storageclass` edge, NOT as compound nesting of the PVC.

`kube_persistentvolumeclaim_info` is OPTIONAL: when the series is absent, or when no series matches a given `(cluster, namespace, claim)`, that PVC's StorageClass name SHALL be empty, no `pvc-to-storageclass` edge SHALL be emitted for it, and the build SHALL NOT fail. When the upstream reports more than one StorageClass value for a single `(cluster, namespace, claim)` the reader SHALL pick deterministically (the lexically smallest StorageClass name) so the emitted edge target is byte-stable across rebuilds.

#### Scenario: StorageClass resolved for a PVC drives an edge

- **WHEN** the upstream provides `kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="cluster-alpha", namespace="db", claim_name="data-mongo-0"}` and `kube_persistentvolumeclaim_info{cluster="cluster-alpha", namespace="db", persistentvolumeclaim="data-mongo-0", storageclass="gp3"}`
- **THEN** the reader emits a directed `pvc-to-storageclass` edge from `cluster-alpha/db/data-mongo-0` to `cluster-alpha/storageclass/gp3`, no `storageclass` key appears in the PVC's `labels`, and no `data.storageclass` field is emitted on the PVC node

#### Scenario: PVC with no matching StorageClass series

- **WHEN** a PVC derived from `kube_pod_spec_volumes_persistentvolumeclaims_info` has no matching `kube_persistentvolumeclaim_info{persistentvolumeclaim=...}` series for its `(cluster, namespace, claim)`
- **THEN** that PVC entity carries an empty StorageClass name, no `pvc-to-storageclass` edge is emitted for it, and the build does not fail

#### Scenario: Deterministic pick on duplicate StorageClass series

- **WHEN** the upstream reports two `kube_persistentvolumeclaim_info` series for the same `(cluster, namespace, claim)` with `storageclass="gp3"` and `storageclass="gp2"`
- **THEN** the reader resolves the PVC's StorageClass to `gp2` (the lexically smallest) deterministically across rebuilds, and the `pvc-to-storageclass` edge targets `<cluster>/storageclass/gp2`

## ADDED Requirements

### Requirement: StorageClass entity from kube_storageclass_info

The topology reader SHALL materialise each StorageClass observed in `kube_storageclass_info` as a real `type="storageclass"` graph node, keyed by `(cluster, storageclass)` with ID `<cluster>/storageclass/<storageclass>` and `name=<storageclass>`. A series missing the `cluster` label SHALL be bucketed under `cluster="unknown"` (the same rule as every other topology series). The node's `labels` SHALL be a strict `map[string]string` containing exactly `cluster`.

The reader SHALL surface the StorageClass's provisioner and backing-storage parameters as **typed attributes, NOT inside `labels`** (the `owner` / `ipaddress` precedent):

- `provisioner` (serialised `data.provisioner`, `omitempty`) ← the native `provisioner` label of `kube_storageclass_info`.
- `parameters` (serialised `data.parameters`, an object `map[string]string`, `omitempty`) — the NetApp/Ceph backing-storage values, each key resolved as **first non-empty source label wins** and **omitted when its resolved value is empty**:
  - `pool` ← the `storagePools` label, else the `pool` label
  - `fs` ← the `fsType` label, else the `fsName` label
  - `cluster_id` ← the `ClusterID` label
  - `selector` ← the `selector` label

The native kube-state-metrics `reclaim_policy` and `volume_binding_mode` fields are OUT OF SCOPE and SHALL NOT be surfaced. When more than one `kube_storageclass_info` series is observed for a single `(cluster, storageclass)`, the `provisioner` and each `parameters` key SHALL be resolved deterministically (the lexically-smallest non-empty value) so the emitted node is byte-stable across rebuilds. The StorageClass node SHALL NOT carry `ipaddress`, `owner`, `application`, `containers`, or `ready_status`, and SHALL NOT place the provisioner or any parameter inside `labels`.

A StorageClass referenced by a PVC (via "PVC StorageClass resolution") but absent from `kube_storageclass_info` SHALL be materialised as a **bare** node (`labels={cluster}`, no `provisioner`, no `parameters`) so the `pvc-to-storageclass` edge has a real target; when both an attributed and a bare candidate exist for the same `(cluster, storageclass)`, the attributed node SHALL win. `kube_storageclass_info` is OPTIONAL: when absent the reader SHALL build a valid topology (all referenced StorageClass nodes bare) and SHALL NOT fail the build.

#### Scenario: StorageClass node with provisioner and parameters

- **WHEN** `kube_storageclass_info{cluster="cluster-alpha", storageclass="netapp-nas", provisioner="csi.trident.netapp.io", storagePools="aggr1", fsType="nfs", ClusterID="ceph-uuid", selector="region=eu"}` is present
- **THEN** the reader emits a StorageClass entity with `id="cluster-alpha/storageclass/netapp-nas"`, `name="netapp-nas"`, `type="storageclass"`, `labels={cluster:"cluster-alpha"}`, `data.provisioner="csi.trident.netapp.io"`, and `data.parameters={pool:"aggr1", fs:"nfs", cluster_id:"ceph-uuid", selector:"region=eu"}`

#### Scenario: Source-label fallback order

- **WHEN** a series carries `pool="ceph-pool"` (and no `storagePools`) and `fsName="cephfs"` (and no `fsType`)
- **THEN** the StorageClass node's `data.parameters` carries `pool="ceph-pool"` and `fs="cephfs"`

#### Scenario: Unset attributes omitted

- **WHEN** a `kube_storageclass_info` series carries `storageclass="gp3"` and none of `provisioner`/`storagePools`/`pool`/`fsType`/`fsName`/`ClusterID`/`selector`
- **THEN** the StorageClass node's `labels` is `{cluster}`, its `data` has no `provisioner` field, and its `data` has no `parameters` field

#### Scenario: Deterministic attribute pick on duplicate series

- **WHEN** two `kube_storageclass_info` series for `(cluster-alpha, gp3)` carry `pool="b-pool"` and `pool="a-pool"`
- **THEN** the StorageClass node's `data.parameters.pool` is `a-pool` (the lexically smallest) deterministically across rebuilds

#### Scenario: Bare node for a referenced-but-absent StorageClass

- **WHEN** a PVC resolves StorageClass `gp3` but no `kube_storageclass_info` series exists for `(cluster-alpha, gp3)`
- **THEN** the reader materialises `cluster-alpha/storageclass/gp3` as a bare node with `labels={cluster:"cluster-alpha"}`, no `provisioner`/`parameters`, and emits the `pvc-to-storageclass` edge to it

### Requirement: Topology relationship edges

The topology reader SHALL emit two directed topology relationship edges, in addition to `pod-mounts-pvc`, using deterministic UUIDv5 edge IDs (canonical input `<type>|<source>|<target>`) and de-duplicating by `(type, source, target)` so the emitted set is byte-stable across rebuilds:

- **`pod-to-node`** — for every pod whose `labels.node` (the cluster-scoped node ID) is non-empty (i.e. the pod is scheduled), one edge from the pod node ID to that node ID. The edge SHALL carry no `labels`. It is always intra-cluster (the node is in the pod's own cluster); `may_cross_cluster` is `false`.
- **`pvc-to-storageclass`** — for every PVC with a non-empty resolved StorageClass name, one edge from the PVC node ID to the StorageClass node ID `<cluster>/storageclass/<name>` (which is materialised, bare if necessary, per "StorageClass entity from kube_storageclass_info"). The edge SHALL carry no `labels`. It is always intra-cluster; `may_cross_cluster` is `false`.

These two edges replace the previous compound-nesting representation of the pod→node and pvc→storageclass relationships (graph-api "Cytoscape compound node grouping" supersedes D31). An unscheduled pod (no `node` label) emits no `pod-to-node` edge; a PVC with no resolved StorageClass emits no `pvc-to-storageclass` edge.

#### Scenario: Scheduled pod emits a pod-to-node edge

- **WHEN** the reader emits a pod entity `cluster-alpha/abc` with `labels.node="cluster-alpha/worker-0"`
- **THEN** the graph contains a directed `pod-to-node` edge from `cluster-alpha/abc` to `cluster-alpha/worker-0` with empty `labels`

#### Scenario: Unscheduled pod emits no pod-to-node edge

- **WHEN** a pod entity has no `node` label (unscheduled)
- **THEN** the graph contains no `pod-to-node` edge originating from that pod

#### Scenario: PVC with a StorageClass emits a pvc-to-storageclass edge

- **WHEN** the reader resolves PVC `cluster-alpha/db/data-mongo-0` to StorageClass `gp3`
- **THEN** the graph contains a directed `pvc-to-storageclass` edge from `cluster-alpha/db/data-mongo-0` to `cluster-alpha/storageclass/gp3` with empty `labels`

#### Scenario: Relationship edge IDs are stable across rebuilds

- **WHEN** the same `pod-to-node` or `pvc-to-storageclass` relationship is produced by two consecutive builds for the same window
- **THEN** the edge `id` (UUIDv5 over `<type>|<source>|<target>`) is byte-identical between the two builds

### Requirement: Service and PVC ArgoCD Application resolution

The topology reader SHALL resolve an ArgoCD Application name for service and PVC entities from the `annotation_argocd_argoproj_io_tracking_id` label, read from `kube_service_annotations` (joined on `(cluster, namespace, service)` to the service entity) and `kube_persistentvolumeclaim_annotations` (joined on `(cluster, namespace, persistentvolumeclaim)` to the PVC entity, where the PVC entity derives its claim name from the `claim_name` label of `kube_pod_spec_volumes_persistentvolumeclaims_info`). A series missing the `cluster` label SHALL be bucketed under `cluster="unknown"` (the same rule as every other topology series).

The Application name SHALL be derived **identically to the pod ArgoCD Application** (graph-api "Pod, Service, and PVC `application` attribute"): it is the segment of the tracking-id value **before the first `:`** (ArgoCD `<app>:<group>/<kind>:<ns>/<name>` form); a value with no `:` is taken verbatim; a value whose leading segment is empty resolves to **no** Application (the entity is absent from the application index, never present-but-empty).

`kube_service_annotations` and `kube_persistentvolumeclaim_annotations` are OPTIONAL: when absent, when no series matches a given entity, or when the matched series has an empty `annotation_argocd_argoproj_io_tracking_id` label, that entity SHALL carry no Application name and the build SHALL NOT fail. When the upstream reports more than one non-empty tracking-id for a single entity, the reader SHALL pick deterministically (the lexically smallest raw tracking-id value, mirroring the pod resolver) so the resolved Application is byte-stable across rebuilds.

The resolved Application name SHALL be surfaced on the service / PVC node's typed `application` attribute (graph-api "Pod, Service, and PVC `application` attribute") and SHALL drive the node's `application` compound-group nesting (graph-api "Cytoscape compound node grouping"). It SHALL NOT be added to the entity's `labels` map.

#### Scenario: Service Application resolved from tracking-id annotation

- **WHEN** the upstream provides `kube_service_info{cluster="cluster-alpha", namespace="shop", service="checkout"}` and `kube_service_annotations{cluster="cluster-alpha", namespace="shop", service="checkout", annotation_argocd_argoproj_io_tracking_id="checkout:apps/Deployment:shop/checkout"}`
- **THEN** the `cluster-alpha/shop/checkout` service entity resolves Application `checkout`, with no `application` / `argocd_tracking_id` key in its `labels`

#### Scenario: PVC Application resolved from tracking-id annotation

- **WHEN** the upstream provides a PVC entity `cluster-alpha/db/data-mongo-0` and `kube_persistentvolumeclaim_annotations{cluster="cluster-alpha", namespace="db", persistentvolumeclaim="data-mongo-0", annotation_argocd_argoproj_io_tracking_id="mongo:apps/StatefulSet:db/mongo"}`
- **THEN** the `cluster-alpha/db/data-mongo-0` PVC entity resolves Application `mongo`

#### Scenario: Tracking-id with no colon is verbatim

- **WHEN** a service's `annotation_argocd_argoproj_io_tracking_id` is `web` (no `:`)
- **THEN** the service resolves Application `web`

#### Scenario: Empty leading segment yields no Application

- **WHEN** a PVC's `annotation_argocd_argoproj_io_tracking_id` is `:apps/Deployment:ns/x` (empty leading segment)
- **THEN** the PVC carries no Application name and nests under its namespace group

#### Scenario: Deterministic pick on duplicate tracking-id series

- **WHEN** two `kube_service_annotations` series for `(cluster-alpha, shop, checkout)` carry `annotation_argocd_argoproj_io_tracking_id="b-app:..."` and `="a-app:..."`
- **THEN** the service resolves Application `a-app` (from the lexically smallest raw tracking-id) deterministically across rebuilds

#### Scenario: Service/PVC without a tracking-id annotation

- **WHEN** a service or PVC has no matching annotation series, or its `annotation_argocd_argoproj_io_tracking_id` label is empty
- **THEN** that entity carries no Application name, nests under its namespace group, and the build does not fail
