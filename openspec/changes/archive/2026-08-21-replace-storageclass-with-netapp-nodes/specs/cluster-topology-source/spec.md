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
- `kube_persistentvolumeclaim_info{cluster, namespace, persistentvolumeclaim, storageclass, volumename, ...}` (OPTIONAL — feeds the PVC `storageclass` attribute and — via the `volumename` label — the PVC `volumename` label that roots the NetApp Harvest join defined by the `netapp-storage-graph` capability)
- `kube_pod_container_info{cluster, namespace, pod, uid, container, image, ...}` (OPTIONAL — feeds the per-pod container list attribute; one series per container)
- `kube_node_status_condition{cluster, node, condition="Ready", status, ...}` (OPTIONAL — feeds the K8s node `ready_status` attribute; the `condition="Ready"` selector is a fixed, request-invariant metric-selection contract, and the `status` label carries `true`/`false`/`unknown` **matched case-insensitively** — stock kube-state-metrics lowercases the value, but an exporter re-publishing the raw Kubernetes `v1.ConditionStatus` enum verbatim emits `True`/`False`/`Unknown` — with the active row's sample value being `1`)
- `kube_persistentvolumeclaim_annotations{cluster, namespace, persistentvolumeclaim, annotation_argocd_argoproj_io_tracking_id, ...}` (OPTIONAL — feeds the PVC ArgoCD Application attribute; the `annotation_argocd_argoproj_io_tracking_id` label is kube-state-metrics' sanitised form of the `argocd.argoproj.io/tracking-id` annotation and requires the operator's `--metric-annotations-allowlist=persistentvolumeclaims=[argocd.argoproj.io/tracking-id]`)
- `kube_service_annotations{cluster, namespace, service, annotation_argocd_argoproj_io_tracking_id, ...}` (OPTIONAL — feeds the service ArgoCD Application attribute; the `annotation_argocd_argoproj_io_tracking_id` label requires the operator's `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id]`)

Every series above SHALL be queried at its bare (unprefixed) name — there is no configurable metric-name prefix.

The three service/endpointslice families are OPTIONAL: when absent (kube-state-metrics not exporting services or endpointslices), the reader SHALL still build a valid topology, the service/endpoint indexes are simply empty, and connection-string resolution in the pod-service-graph reader degrades gracefully — `"://"` service endpoints that cannot be resolved against an empty index become `external/<label>` nodes.

`kube_persistentvolumeclaim_info` is likewise OPTIONAL: when absent — or when no series matches a given PVC — the reader SHALL still build a valid topology, the affected PVC entities carry no `storageclass` attribute, no `volumename` label (and hence no `svm` label and no `pvc-to-netapp-aggr` edge — see the `netapp-storage-graph` capability), and the Cytoscape serialiser nests those PVCs under their namespace group (`cluster > namespace > pvc`) like any other PVC.

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
- **THEN** the reader produces a valid topology in which every PVC entity has no `storageclass` attribute, carries no `volumename` or `svm` label, emits no `pvc-to-netapp-aggr` edge, the build does not fail, and the serialiser nests every PVC under its namespace group

#### Scenario: StorageClass info metric absent

- **WHEN** the upstream contains `kube_storageclass_info` series for the window
- **THEN** the reader never queries the metric (it is removed from the topology fan-out), no `storageclass` entity is materialised, and PVC entities still resolve their `storageclass` attribute from `kube_persistentvolumeclaim_info`

#### Scenario: Trident custom-resource metrics absent

- **WHEN** the upstream contains (or lacks) `kube_tridentvolume_info` / `kube_tridentbackend_info` series for the window
- **THEN** the reader never queries either metric (the Trident chain is removed from the topology fan-out) and the PVC `svm` label resolves solely via the `netapp-storage-graph` capability's `volume_labels` join

#### Scenario: Container info metric absent

- **WHEN** the upstream contains `kube_pod_info` but no `kube_pod_container_info` series for the window
- **THEN** the reader produces a valid topology in which every pod entity carries no `containers` attribute, and the build does not fail

#### Scenario: Node status-condition metric absent

- **WHEN** the upstream contains `kube_node_info` but no `kube_node_status_condition` series for the window
- **THEN** the reader produces a valid topology in which every K8s node entity carries no `ready_status` attribute, and the build does not fail

#### Scenario: PVC/service annotation metrics absent

- **WHEN** the upstream contains `kube_persistentvolumeclaim_info` and `kube_service_info` but no `kube_persistentvolumeclaim_annotations` or `kube_service_annotations` series for the window
- **THEN** the reader produces a valid topology in which every PVC and service entity carries no `application` attribute and nests under its namespace group, and the build does not fail

### Requirement: PVC StorageClass resolution

The topology reader SHALL resolve each PVC's StorageClass **name** from `kube_persistentvolumeclaim_info`, joining on `(cluster, namespace, persistentvolumeclaim)` to the PVC entity (which derives from `kube_pod_spec_volumes_persistentvolumeclaims_info`, where the claim name comes from the `claim_name` label). The resolved name SHALL be surfaced as the PVC's own typed `storageclass` attribute (serialised `data.storageclass`, `omitempty` — graph-api "PVC `storageclass` and `usage` attributes"). It SHALL NOT be added to the PVC `labels` map, SHALL NOT drive any edge, and SHALL NOT materialise any node — the StorageClass entity and the `pvc-to-storageclass` edge are removed. The name is retained because it is the operator's discriminator for the `netapp-storage-graph` join-coverage signal: it distinguishes "this claim was never meant to have a NetApp backend" from "this claim should have joined and did not".

`kube_persistentvolumeclaim_info` is OPTIONAL: when the series is absent, or when no series matches a given `(cluster, namespace, claim)`, that PVC's StorageClass name SHALL be empty (the attribute absent), and the build SHALL NOT fail. When the upstream reports more than one StorageClass value for a single `(cluster, namespace, claim)` the reader SHALL pick deterministically (the lexically smallest StorageClass name) so the emitted attribute is byte-stable across rebuilds.

#### Scenario: StorageClass resolved for a PVC drives an edge

- **WHEN** the upstream provides `kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="cluster-alpha", namespace="db", claim_name="data-mongo-0"}` and `kube_persistentvolumeclaim_info{cluster="cluster-alpha", namespace="db", persistentvolumeclaim="data-mongo-0", storageclass="netapp-nas"}`
- **THEN** the `cluster-alpha/db/data-mongo-0` PVC entity carries `data.storageclass="netapp-nas"`, no `storageclass` key appears in its `labels`, and no edge or node is emitted for the StorageClass (the former drives-an-edge behaviour is removed)

#### Scenario: PVC with no matching StorageClass series

- **WHEN** a PVC derived from `kube_pod_spec_volumes_persistentvolumeclaims_info` has no matching `kube_persistentvolumeclaim_info{persistentvolumeclaim=...}` series for its `(cluster, namespace, claim)`
- **THEN** that PVC entity carries no `storageclass` attribute and the build does not fail

#### Scenario: Deterministic pick on duplicate StorageClass series

- **WHEN** the upstream reports two `kube_persistentvolumeclaim_info` series for the same `(cluster, namespace, claim)` with `storageclass="gp3"` and `storageclass="gp2"`
- **THEN** the reader resolves the PVC's `data.storageclass` to `gp2` (the lexically smallest) deterministically across rebuilds

### Requirement: Topology relationship edges

The topology reader SHALL emit one directed topology relationship edge, in addition to `pod-mounts-pvc`, using deterministic UUIDv5 edge IDs (canonical input `<type>|<source>|<target>`) and de-duplicating by `(type, source, target)` so the emitted set is byte-stable across rebuilds:

- **`pod-to-node`** — for every pod whose `labels.node` (the cluster-scoped node ID) is non-empty (i.e. the pod is scheduled), one edge from the pod node ID to that node ID. The edge SHALL carry no `labels`. It is always intra-cluster (the node is in the pod's own cluster); `may_cross_cluster` is `false`.

The former `pvc-to-storageclass` edge is removed. The `pvc-to-netapp-aggr` edge is NOT a topology relationship edge — it is derived from the Harvest volume join and defined by the `netapp-storage-graph` capability. An unscheduled pod (no `node` label) emits no `pod-to-node` edge.

#### Scenario: Scheduled pod emits a pod-to-node edge

- **WHEN** the reader emits a pod entity `cluster-alpha/abc` with `labels.node="cluster-alpha/worker-0"`
- **THEN** the graph contains a directed `pod-to-node` edge from `cluster-alpha/abc` to `cluster-alpha/worker-0` with empty `labels`

#### Scenario: Unscheduled pod emits no pod-to-node edge

- **WHEN** a pod entity has no `node` label (unscheduled)
- **THEN** the graph contains no `pod-to-node` edge originating from that pod

#### Scenario: PVC with a StorageClass emits a pvc-to-storageclass edge

- **WHEN** the reader resolves PVC `cluster-alpha/db/data-mongo-0` to StorageClass name `gp3`
- **THEN** the graph contains no `pvc-to-storageclass` edge and no `storageclass` node — the former edge behaviour this scenario named is removed; the name surfaces only as the PVC's `data.storageclass` attribute

#### Scenario: Relationship edge IDs are stable across rebuilds

- **WHEN** the same `pod-to-node` relationship is produced by two consecutive builds for the same window
- **THEN** the edge `id` (UUIDv5 over `<type>|<source>|<target>`) is byte-identical between the two builds

### Requirement: PVC PersistentVolume name and NetApp SVM labels

The topology reader SHALL resolve each PVC's bound **PersistentVolume name** and surface it, together with the **ONTAP SVM** serving it when the NetApp Harvest join resolves, as additive entries in the PVC entity's `labels` map (strict `map[string]string`):

- `volumename` — the bound PV name, read from the `volumename` label of `kube_persistentvolumeclaim_info`, joined on `(cluster, namespace, persistentvolumeclaim)` to the PVC entity (the same join as PVC StorageClass resolution; the two label reads are per-field independent — a series may carry `volumename` without `storageclass` and vice versa). The key SHALL be set only when the resolved value is non-empty.
- `svm` — the NetApp SVM, resolved by the `netapp-storage-graph` capability's Harvest join rooted at the resolved PV name (the Trident custom-resource chain is removed; the `svm` label rides on the same `volume_labels` series that provides the serving aggregate and controller, and never on the QoS families that carry the edge's I/O). The key SHALL be set only when the join resolves a non-empty `svm` value. By construction `svm` SHALL never be present without `volumename`.

The `volumename` key is DISTINCT from the existing `volume` key (the pod-spec volume name from `kube_pod_spec_volumes_persistentvolumeclaims_info`); both MAY coexist on one PVC entity and neither replaces the other.

Every link degrades gracefully: when `kube_persistentvolumeclaim_info` is absent, a join finds no match, or a required label is empty, the affected key(s) are simply omitted — the reader SHALL still build a valid topology, SHALL NOT fail the build, and SHALL NOT emit an empty-string label value. The join ENRICHES PVC entities that exist via the pod→PVC binding metric; it SHALL NOT materialise a PVC on its own.

Resolution SHALL be deterministic: on duplicate series the reader SHALL pick the lexically-smallest non-empty value per stage (`volumename` per `(cluster, namespace, claim)`; `svm` per join key, per the `netapp-storage-graph` capability), so the emitted labels are a pure function of the upstream data, independent of vector order. Labels are baked at build time before any projection.

#### Scenario: Full chain resolves volumename and svm

- **WHEN** the upstream provides a PVC entity `cluster-alpha/db/data-mongo-0` with `kube_persistentvolumeclaim_info{cluster="cluster-alpha", namespace="db", persistentvolumeclaim="data-mongo-0", volumename="pvc-9f3a"}` and a Harvest `volume_labels` series with `volume_name="pvc-9f3a", svm="svm-prod"`
- **THEN** the emitted PVC entity's `labels` contains `volumename="pvc-9f3a"` and `svm="svm-prod"`

#### Scenario: PV without a TridentVolume row yields volumename only

- **WHEN** a PVC resolves `volumename="pvc-9f3a"` but no Harvest `volume_labels` series carries `volume_name="pvc-9f3a"` (the former Trident chain no longer exists; the Harvest join is the only `svm` source)
- **THEN** the emitted PVC entity's `labels` contains `volumename="pvc-9f3a"` and no `svm` key, and the build does not fail

#### Scenario: TridentVolume without a matching backend yields no svm

- **WHEN** the Harvest `volume_labels` series matched by a PVC's PV name carries an empty `svm` label (the former TridentVolume→backend hop no longer exists)
- **THEN** the emitted PVC entity carries `volumename` but no `svm` key — never an empty-string value — and the build does not fail

#### Scenario: Trident metrics absent entirely

- **WHEN** the upstream contains `kube_persistentvolumeclaim_info` (with `volumename` labels) and no Harvest `volume_labels` series for the window (the Trident custom-resource metrics are no longer read at all)
- **THEN** the reader produces a valid topology in which PVC entities carry `volumename` but no `svm` key, and the build does not fail

#### Scenario: PVC info without volumename yields neither label

- **WHEN** a PVC's `kube_persistentvolumeclaim_info` series carries no (or an empty) `volumename` label, or no info series matches the PVC at all
- **THEN** the emitted PVC entity carries neither a `volumename` nor an `svm` key — no empty-string value is emitted — and the build does not fail

#### Scenario: volumename is independent of storageclass on the same series

- **WHEN** a `kube_persistentvolumeclaim_info` series carries `volumename="pvc-9f3a"` but an empty `storageclass` label
- **THEN** the emitted PVC entity carries `labels.volumename="pvc-9f3a"` while no `storageclass` attribute is emitted for it (and vice versa: a series with `storageclass` but no `volumename` drives the attribute without the label)

#### Scenario: volume and volumename coexist

- **WHEN** a PVC entity derives `volume="data"` from the pod→PVC binding metric and resolves `volumename="pvc-9f3a"` from `kube_persistentvolumeclaim_info`
- **THEN** the emitted PVC entity's `labels` contains both `volume="data"` (the pod-spec volume name) and `volumename="pvc-9f3a"` (the bound PV name) as distinct keys

#### Scenario: Deterministic pick on duplicate series at every stage

- **WHEN** the upstream reports two `kube_persistentvolumeclaim_info` series for `(cluster-alpha, db, data-mongo-0)` with `volumename="pvc-b"` and `volumename="pvc-a"`, and two Harvest `volume_labels` series for `volume_name="pvc-a"` with `svm="svm-b"` and `svm="svm-a"`
- **THEN** the reader resolves `volumename="pvc-a"` and `svm="svm-a"` (the lexically-smallest non-empty value at each stage) deterministically across rebuilds, independent of upstream vector order

## ADDED Requirements

### Requirement: PVC usage from kubelet volume stats

The topology reader SHALL resolve each PVC's storage usage from the kubelet volume-stats series `kubelet_volume_stats_used_bytes{cluster, namespace, persistentvolumeclaim, ...}` and `kubelet_volume_stats_capacity_bytes{cluster, namespace, persistentvolumeclaim, ...}`, joined on `(cluster, namespace, persistentvolumeclaim)` to the PVC entity. This introduces **kubelet** as a further upstream metric family alongside kube-state-metrics and NetApp Harvest (whose volume-label, QoS workload, QoS fixed-policy, aggregate, and node objects are read by the `netapp-storage-graph` capability); the label contract above is fixed and case-sensitive. A series missing the `cluster` label SHALL be bucketed under `cluster="unknown"` (the same rule as every other topology series).

Both series are OPTIONAL and per-field independent: `used_bytes` resolves from the first, `capacity_bytes` from the second; the PVC's `usage` attribute (graph-api "PVC `storageclass` and `usage` attributes") SHALL be present iff at least one field resolved, with an unresolved field omitted from the object. When a metric is absent, or no series matches a given PVC, the affected field(s) are simply omitted — the reader SHALL still build a valid topology and SHALL NOT fail the build. On duplicate series for one `(cluster, namespace, claim)` the reader SHALL pick deterministically (the smallest numeric value) so the emitted attribute is byte-stable across rebuilds. The values SHALL never appear inside `labels`.

#### Scenario: Both usage fields resolved

- **WHEN** the upstream provides `kubelet_volume_stats_used_bytes{cluster="cluster-alpha", namespace="db", persistentvolumeclaim="data-mongo-0"} = 5368709120` and `kubelet_volume_stats_capacity_bytes{...} = 10737418240` for a PVC entity
- **THEN** the `cluster-alpha/db/data-mongo-0` PVC entity carries `usage = {used_bytes: 5368709120, capacity_bytes: 10737418240}` and its `labels` gains no new key

#### Scenario: Capacity only

- **WHEN** a PVC matches a `kubelet_volume_stats_capacity_bytes` series but no `kubelet_volume_stats_used_bytes` series
- **THEN** the PVC's `usage` object contains `capacity_bytes` and no `used_bytes` key, and the build does not fail

#### Scenario: Kubelet metrics absent entirely

- **WHEN** the upstream contains no `kubelet_volume_stats_*` series for the window
- **THEN** the reader produces a valid topology in which no PVC entity carries a `usage` attribute, and the build does not fail

#### Scenario: Deterministic pick on duplicate usage series

- **WHEN** two `kubelet_volume_stats_used_bytes` series for one `(cluster, namespace, claim)` report `100` and `90`
- **THEN** the PVC resolves `used_bytes: 90` (the smallest value) deterministically across rebuilds

## REMOVED Requirements

### Requirement: Configurable upstream metric-name prefix

**Reason**: The `KSG_METRIC_PREFIX` / `--metric-prefix` knob and the `Renderer.Prefix` plumbing behind it (including the public `kubegraph.Options.MetricPrefix` field) are removed. All kube-state-metrics-shaped series are queried at their bare names.

**Migration**: A deployment whose KSM series ARE published under an organisational prefix will silently return an empty graph after this change — the removal requires a release note, not just a changelog line. Such deployments must re-publish the series at their bare `kube_*` names (drop the prefixing relabel/fork) before upgrading. Embedders (`graph-api-gateway`) must drop `Options.MetricPrefix`; the two repositories must land in a coordinated order.

### Requirement: StorageClass entity from kube_storageclass_info

**Reason**: The `storageclass` node type is removed — the storage half of the graph re-anchors on the physical ONTAP controller (`netapp-node`, see the `netapp-storage-graph` capability) instead of the Kubernetes provisioning policy. The `kube_storageclass_info` query and the `data.provisioner` / `data.parameters` attributes it fed are dropped.

**Migration**: The claim's StorageClass **name** survives as the PVC's own `data.storageclass` attribute ("PVC StorageClass resolution"). Provisioner and backing-storage parameters are no longer served; the physical backend surfaces via the `pvc-to-netapp-aggr` edge and the `netapp-aggr` / `netapp-node` payloads instead.
