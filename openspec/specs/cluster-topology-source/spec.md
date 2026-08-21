# cluster-topology-source Specification

## Purpose
TBD - created by archiving change add-k8s-pod-graph-api. Update Purpose after archive.
## Requirements

### Requirement: Centralised VictoriaMetrics as the only topology source

The topology reader SHALL fetch all pod, node, and PVC topology by issuing PromQL queries against a single configurable Prometheus-compatible endpoint (`--prom-url`), pointing at centralised VictoriaMetrics. The reader SHALL NOT call the Kubernetes API server, SHALL NOT scrape `kube-state-metrics` directly, and SHALL NOT use Kubernetes informers.

#### Scenario: Single configured upstream

- **WHEN** the server starts with `--prom-url=http://vm.example:8428`
- **THEN** every topology query is sent to `http://vm.example:8428` and no other HTTP destinations

#### Scenario: No Kubernetes API access

- **WHEN** the server runs in any environment
- **THEN** the binary makes no requests to any `/api/*` Kubernetes API path and requires no Kubernetes ServiceAccount or kubeconfig

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

### Requirement: Service and endpoint indexes

When the optional `kube_service_info`, `kube_endpointslice_endpoints`, and `kube_endpointslice_labels` families are present, the topology reader SHALL build two lookup INDEXES that the pod-service-graph reader consults to resolve `"://"` connection-string endpoints. The reader SHALL build INDEXES ONLY — it SHALL NOT emit `service` nodes or `service-selects-pod` edges into the graph wholesale. Those are materialised ON DEMAND by the pod-service-graph reader, for referenced services only, to avoid graph bloat.

The two indexes are:

- **ServicesByNameNS**: keyed by `(cluster, namespace, service)`, mapping to the service facts from `kube_service_info` — including `cluster_ip` (used to set the service node's `ipaddress`, omitted when `cluster_ip="None"` for headless services).
- **EndpointsByService**: keyed by `(cluster, namespace, service)`, mapping to the list of backing pods (the source of the Service → backing-pod fan-out). Each slice is joined back to its owning service via the `label_kubernetes_io_service_name` label on `kube_endpointslice_labels`, joined to `kube_endpointslice_endpoints` by `(cluster, namespace, endpointslice)`. Each endpoint is then resolved to a topology pod by joining `(namespace, targetref_name)` against `kube_pod_info` (matching the pod by name within the namespace to recover its UID). The per-endpoint `hostname` label is NOT consumed — there is no per-pod headless resolution.

#### Scenario: Service index resolves backing pods

- **WHEN** the upstream provides `kube_service_info{cluster="cluster-alpha", namespace="db", service="mongo", cluster_ip="10.96.0.5"}`, a `kube_endpointslice_labels{cluster="cluster-alpha", namespace="db", endpointslice="mongo-abc", label_kubernetes_io_service_name="mongo"}` series, and `kube_endpointslice_endpoints{cluster="cluster-alpha", namespace="db", endpointslice="mongo-abc", targetref_kind="Pod", targetref_name="mongo-0", targetref_namespace="db"}` whose `(namespace, targetref_name)` matches a `kube_pod_info` pod
- **THEN** `ServicesByNameNS[(cluster-alpha, db, mongo)]` carries `cluster_ip="10.96.0.5"` and `EndpointsByService[(cluster-alpha, db, mongo)]` lists the resolved backing pod, while no `service` node or `service-selects-pod` edge is emitted into the graph by the topology reader

### Requirement: Time-window evaluation

Each topology query SHALL be evaluated at the caller-supplied `end` timestamp using `last_over_time(<series>[<window>]) @ <end>` so the result reflects the most recent value of each series within the requested window. The reader SHALL NOT fall back to instant evaluation at `now`.

#### Scenario: last_over_time used for kube_pod_info

- **WHEN** the reader runs a query for `kube_pod_info`
- **THEN** the issued PromQL string contains `last_over_time(kube_pod_info[<window>]) @ <end>` where `<window>` equals `end - start` and `<end>` equals the caller-supplied `end`

#### Scenario: Windowed result mid-restart

- **WHEN** a pod was running at `start` and replaced before `end`
- **THEN** the reader emits both pod-info entries for the window (the prior and the current UID); see "Pod restart handling" requirement

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

### Requirement: Canonical entity fields

Every emitted topology entity SHALL carry the canonical fields consumed by the graph API: `id`, `name`, `type`, `labels`, and `ipaddress` (for pods and K8s nodes). The reader SHALL set these as follows:

- For pods: `name` = the `pod` label of `kube_pod_info`; `type` = `"pod"`; `labels` includes `cluster`, `namespace`, `node` (cluster-scoped node ID), and any K8s pod labels available from `kube_pod_labels` for that pod (added under their original keys). `ipaddress` = `[pod_ip]` from `kube_pod_info.pod_ip` when surfaced; otherwise empty / omitted. The `host_ip` series label is intentionally not surfaced on the pod entity — the node's IP is exposed only via the K8s node entity. When kube-state-metrics emits multiple `kube_pod_info` series for the same pod-UID with evolving label sets (e.g. earlier scrapes that lack `node` or `pod_ip`), the reader SHALL merge labels across same-UID samples and pick the newest non-empty `pod_ip` so the emitted entity reflects the most informative observation. When `kube_pod_owner` is available, the pod entity additionally carries a typed nullable `owner` attribute (`{kind, name}`, serialised as `data.owner`, NOT a label) for the pod's controller owner (with the ReplicaSet skipped to the owning Deployment) — see the "Pod controller-owner attribute with ReplicaSet skip" requirement; `owner` is omitted entirely when the pod has no controller owner.
- For K8s nodes: `name` = the `node` label of `kube_node_info`; `type` = `"node"`; `labels` includes `cluster` and any node labels from `kube_node_labels` for that node (the `label_*=` series translates to entries under their original key with the `label_` prefix removed). `ipaddress` = `[external_ip]` from `kube_node_status_addresses{type="ExternalIP"}` when surfaced, falling back to `[internal_ip]` from `kube_node_status_addresses{type="InternalIP"}` when the node has no ExternalIP row; omitted only when neither address type is surfaced. An ExternalIP row SHALL always win over an InternalIP row regardless of upstream vector order. Within each address type, duplicate `(cluster, node)` samples SHALL resolve to the lexically-smallest address, so the emitted IP is a pure function of the data (determinism). Address types other than `ExternalIP` / `InternalIP` SHALL be ignored. IPs SHALL NOT be carried inside `labels`.
- For PVCs: `name` = the `claim_name` label of `kube_pod_spec_volumes_persistentvolumeclaims_info`; `type` = `"pvc"`; `labels` includes `cluster`, `namespace`, and `volume`. `ipaddress` is not emitted.

#### Scenario: Pod entity canonical fields

- **WHEN** `kube_pod_info{cluster="cluster-alpha", namespace="shop", pod="checkout-1", uid="abc", node="worker-0"}` is present
- **THEN** the emitted pod entity has `id="cluster-alpha/abc"`, `name="checkout-1"`, `type="pod"`, `labels.cluster="cluster-alpha"`, `labels.namespace="shop"`, and `labels.node="cluster-alpha/worker-0"`

#### Scenario: Pod IP surfaced on the ipaddress attribute

- **WHEN** `kube_pod_info{cluster="cluster-alpha", namespace="shop", pod="checkout-1", uid="abc", node="worker-0", pod_ip="10.244.0.42", host_ip="10.0.0.7"}` is present
- **THEN** the emitted pod entity has `ipaddress=["10.244.0.42"]`; neither `labels.pod_ip` nor `labels.host_ip` is present, and `host_ip` is dropped because the node's IP lives on the K8s node entity

#### Scenario: Pod ipaddress merged across same-UID samples

- **WHEN** kube-state-metrics emits two `kube_pod_info` series with the same `uid` — one without `pod_ip`/`node` (early scrape during scheduling) and a later one with both populated
- **THEN** the emitted pod entity carries the populated `node` label and `ipaddress=[<pod_ip>]` regardless of the order returned by the upstream

#### Scenario: K8s node ExternalIP surfaced on the ipaddress attribute

- **WHEN** `kube_node_status_addresses{cluster="cluster-alpha", node="worker-0", type="ExternalIP", address="203.0.113.10"}` is present
- **THEN** the emitted K8s node entity has `ipaddress=["203.0.113.10"]` and `labels.external_ip` is not present

#### Scenario: K8s node falls back to InternalIP when no ExternalIP exists

- **WHEN** the only `kube_node_status_addresses` rows for `(cluster="cluster-alpha", node="worker-0")` carry `type="InternalIP"` (e.g. `address="10.0.0.7"`)
- **THEN** the emitted K8s node entity has `ipaddress=["10.0.0.7"]` and neither `labels.internal_ip` nor `labels.external_ip` is present

#### Scenario: ExternalIP wins over InternalIP regardless of vector order

- **WHEN** `(cluster="cluster-alpha", node="worker-0")` has both `kube_node_status_addresses{type="InternalIP", address="10.0.0.7"}` and `kube_node_status_addresses{type="ExternalIP", address="203.0.113.10"}` rows, in any upstream order
- **THEN** the emitted K8s node entity has `ipaddress=["203.0.113.10"]`

#### Scenario: K8s node with no address rows omits ipaddress

- **WHEN** `(cluster="cluster-alpha", node="worker-0")` has no `kube_node_status_addresses` row of type `ExternalIP` or `InternalIP`
- **THEN** the emitted K8s node entity carries no `ipaddress`

#### Scenario: K8s node labels flattened

- **WHEN** the upstream provides `kube_node_labels{cluster="cluster-alpha", node="worker-0", label_topology_kubernetes_io_zone="us-east-1a", label_kubernetes_io_arch="amd64"}`
- **THEN** the emitted node entity's `labels` map contains `topology.kubernetes.io/zone="us-east-1a"` and `kubernetes.io/arch="amd64"` under their original keys

### Requirement: Pod controller-owner attribute with ReplicaSet skip

The topology reader SHALL resolve each pod's **controller owner** from `kube_pod_owner` and surface it on the pod entity as a typed, nullable `owner` attribute (`{kind, name}`), serialised as `data.owner` (`omitempty`) and **never inside `labels`**. The reader SHALL select the owner whose `owner_is_controller="true"`; when multiple controller owners are reported for a single `(cluster, namespace, pod)` the reader SHALL pick deterministically (lexical order of `(kind, name)`) so the emitted entity is stable across rebuilds.

When the selected controller owner has `kind="ReplicaSet"`, the reader SHALL transparently **skip the ReplicaSet** and resolve one level up via `kube_replicaset_owner` keyed by `(cluster, namespace, replicaset=owner_name)`:

- If a `kube_replicaset_owner` series with `owner_kind="Deployment"` exists for that ReplicaSet, the emitted `owner` is `{kind:"Deployment", name:<deployment>}`.
- If no `kube_replicaset_owner` series exists for that ReplicaSet (a bare ReplicaSet with no owning Deployment), the emitted `owner` SHALL remain `{kind:"ReplicaSet", name:<replicaset>}`.

Owners of any other kind (`DaemonSet`, `StatefulSet`, `Job`, `Node` for static pods reported as a controller, etc.) SHALL be surfaced verbatim with no further resolution. When a pod has no controller owner at all (`kube_pod_owner` absent for the pod, or no series with `owner_is_controller="true"`), the reader SHALL emit a nil `owner` so `data.owner` is omitted entirely — it SHALL NOT emit an empty object, empty-string fields, or any owner key in `labels`. `kube_pod_owner` and `kube_replicaset_owner` are OPTIONAL: when absent the reader SHALL build a valid topology with no `owner` on any pod and SHALL NOT fail the build. This requirement introduces NO new node or edge type — the owner is a typed attribute on the existing `type="pod"` node (the same precedent as the `ipaddress` attribute), keeping `labels` a strict `map[string]string` of typological metadata.

#### Scenario: Pod owned by a Deployment via ReplicaSet

- **WHEN** `kube_pod_owner{cluster="cluster-alpha", namespace="shop", pod="checkout-1", owner_kind="ReplicaSet", owner_name="checkout-7f9c", owner_is_controller="true"}` and `kube_replicaset_owner{cluster="cluster-alpha", namespace="shop", replicaset="checkout-7f9c", owner_kind="Deployment", owner_name="checkout"}` are present
- **THEN** the emitted pod entity has `owner={kind:"Deployment", name:"checkout"}` (the intermediate ReplicaSet does not appear), and no `owner_kind` / `owner_name` key in `labels`

#### Scenario: Bare ReplicaSet with no owning Deployment

- **WHEN** `kube_pod_owner{..., pod="adhoc-1", owner_kind="ReplicaSet", owner_name="adhoc-rs", owner_is_controller="true"}` is present but no `kube_replicaset_owner` series exists for `adhoc-rs`
- **THEN** the emitted pod entity has `owner={kind:"ReplicaSet", name:"adhoc-rs"}`

#### Scenario: Pod owned directly by a non-ReplicaSet controller

- **WHEN** `kube_pod_owner{..., pod="logs-x9", owner_kind="DaemonSet", owner_name="fluentd", owner_is_controller="true"}` is present
- **THEN** the emitted pod entity has `owner={kind:"DaemonSet", name:"fluentd"}` with no `kube_replicaset_owner` lookup

#### Scenario: Pod with no controller owner

- **WHEN** no `kube_pod_owner` series with `owner_is_controller="true"` exists for a pod (e.g. a static or bare pod)
- **THEN** the emitted pod entity has a nil `owner` (`data.owner` omitted entirely) and carries no owner key in `labels`

#### Scenario: Owner metrics absent entirely

- **WHEN** the upstream contains `kube_pod_info` but no `kube_pod_owner` or `kube_replicaset_owner` series for the window
- **THEN** the reader produces a valid topology with no `owner` on any pod and does not fail the build

### Requirement: Pod restart handling within window

When `last_over_time(kube_pod_info[...])` returns multiple `uid` values for the same `(cluster, namespace, pod)` tuple within the requested window (i.e. the pod was deleted and recreated mid-window), the reader SHALL retain ONLY the entity with the latest evaluation timestamp as the canonical pod and SHALL discard prior UIDs. There is no reliable identity link between the deleted pod and its replacement once kubelet stops reporting the deleted UID, so the API does not attempt to reconstruct one.

#### Scenario: Pod replaced mid-window collapses to latest UID

- **WHEN** the window includes a pod restart producing two distinct UIDs for the same `(cluster, namespace, pod)` tuple
- **THEN** the resulting topology contains exactly one pod entity, identified by the newest UID; the prior UID does not appear as a node and no synthetic edge is emitted

### Requirement: Cluster discovery query

The topology reader SHALL provide a discovery query, used by the cluster discovery endpoint, that returns the set of distinct `cluster` label values observed in `kube_node_info` over a configurable lookback (default 1 hour) via PromQL `group by (cluster) (last_over_time(kube_node_info[<lookback>]))`.

#### Scenario: Two clusters discovered

- **WHEN** centralised VictoriaMetrics holds `kube_node_info` series for `cluster=cluster-alpha` and `cluster=cluster-beta` within the discovery lookback
- **THEN** the discovery query returns exactly the set `{ "cluster-alpha", "cluster-beta" }`

### Requirement: Series missing the cluster label

A topology series that is missing the `cluster` label SHALL be bucketed under `cluster="unknown"`. The reader SHALL surface the count of such series via the `kube_state_graph_clusters_observed` gauge (the value `unknown` will appear in the gauge's label set when present).

#### Scenario: Legacy series without cluster label

- **WHEN** a `kube_pod_info` series has no `cluster` label
- **THEN** the resulting pod entity has `cluster: "unknown"` and contributes to the `unknown` value in the observed-clusters set

### Requirement: Per-call upstream timeout

Each topology query SHALL be issued with a per-call context timeout (default 10 seconds, configurable). On timeout or non-2xx response, the reader SHALL increment `kube_state_graph_upstream_query_failures_total{query=<name>}` and propagate the error so the build aborts.

#### Scenario: Single query times out

- **WHEN** centralised VictoriaMetrics fails to respond to the `kube_node_labels` query within the per-call timeout
- **THEN** the failure counter for `query="kube_node_labels"` increments by 1 and the build returns an error

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

### Requirement: Optional basic-auth credentials for the upstream endpoint

The server SHALL support optional HTTP Basic Auth credentials for the single upstream Prometheus-compatible endpoint, sourced **exclusively** from the environment variables `KSG_PROM_USERNAME` and `KSG_PROM_PASSWORD`. No CLI flag SHALL exist for either value — credential-carrying flags leak through process listings and container specs; this is a deliberate exception to the env+flag dual-track configuration convention.

When both variables are set (non-empty), every outbound HTTP request to the upstream — topology queries, the service-graph query, the cluster-discovery query, and the `/readyz` `up` probe — SHALL carry an `Authorization: Basic` header for those credentials. When both are unset, requests SHALL carry no `Authorization` header and behaviour is unchanged from an unauthenticated deployment.

Setting exactly one of the two variables (non-empty) SHALL fail server startup with a validation error that names both environment variables but does NOT echo either value.

The credential values SHALL NOT appear in any log line, trace span attribute, metric label, error message, or HTTP response body. Rotation requires a process restart — there is no hot reload for upstream credentials.

#### Scenario: Credentials applied to all upstream queries

- **WHEN** the server starts with `KSG_PROM_USERNAME=ksg` and `KSG_PROM_PASSWORD=s3cret` and serves a `/v1/graph` request
- **THEN** every upstream HTTP request issued for the build (topology fan-out, service-graph, and any cluster-discovery or readiness query) carries `Authorization: Basic` for `ksg:s3cret`

#### Scenario: No credentials configured

- **WHEN** the server starts with neither `KSG_PROM_USERNAME` nor `KSG_PROM_PASSWORD` set
- **THEN** upstream requests carry no `Authorization` header and startup validation passes

#### Scenario: Half-configured credentials rejected at startup

- **WHEN** the server starts with `KSG_PROM_USERNAME=ksg` and no `KSG_PROM_PASSWORD` (or vice versa)
- **THEN** `config.Validate` returns an error naming `KSG_PROM_USERNAME` and `KSG_PROM_PASSWORD`, the error does not contain the configured value, and the process exits non-zero before binding the listener

#### Scenario: No CLI flag exists for credentials

- **WHEN** the server is started with `--prom-username=x` or `--prom-password=x`
- **THEN** flag parsing fails with an unknown-flag error, because credentials are env-only

#### Scenario: Credentials never logged

- **WHEN** the server runs with credentials configured at any log level, including `debug`, and upstream queries succeed or fail
- **THEN** no emitted log line, span attribute, or error string contains the configured username or password

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

### Requirement: K8s node Ready-status attribute

The topology reader SHALL resolve each K8s node's **Ready status** from `kube_node_status_condition{condition="Ready"}`, queried as `last_over_time(kube_node_status_condition{condition="Ready"}[w])`, and surface it on the K8s node entity as a typed, nullable `ready_status` attribute (a string), serialised as `data.ready_status` (`omitempty`) and **never inside `labels`**. The `condition="Ready"` selector is a fixed, request-invariant metric-selection contract (the same precedent as the node-address `type` selector) — it is applied at the query layer for every build and is NOT a caller filter, preserving the "no caller filters pushed to PromQL" contract.

For each `(cluster, node)`, the reader SHALL read the `status` label of the **active** `condition="Ready"` series — the series whose sample value is `1` — and map it **case-insensitively**: `true` → `"Ready"`, `false` → `"NotReady"`, `unknown` → `"Unknown"`. The `status`-label casing is NOT pinned by the KSM-shaped contract: stock kube-state-metrics lowercases the value (`addConditionMetrics` → `strings.ToLower`), but an exporter that re-publishes the raw Kubernetes `v1.ConditionStatus` enum verbatim emits `True`/`False`/`Unknown`; the reader SHALL canonicalise casing (to lowercase) at the read site so both forms resolve and the guard, tie-break, and mapping all operate on one casing. When more than one `condition="Ready"` series is active for the same `(cluster, node)` (a defensive case that does not occur in correct kube-state-metrics output, where exactly one Ready status is active at a time), the reader SHALL pick the lexically-smallest (canonicalised) `status` label so the emitted value is deterministic and order-free.

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

#### Scenario: Raw-enum (capitalised) status casing resolves

- **WHEN** the active `condition="Ready"` series for `(cluster="cluster-alpha", node="worker-0")` carries `status="True"` (value `1`) — the raw Kubernetes `v1.ConditionStatus` casing an exporter may emit instead of the lowercase form stock kube-state-metrics produces
- **THEN** the emitted K8s node entity has `ready_status="Ready"` (the reader matches the `status` label case-insensitively), and likewise `status="False"` → `"NotReady"` and `status="Unknown"` → `"Unknown"`

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

### Requirement: Service and PVC ArgoCD Application resolution

The topology reader SHALL resolve an ArgoCD Application name for service and PVC entities from the `annotation_argocd_argoproj_io_tracking_id` label, read from `kube_service_annotations` (joined on `(cluster, namespace, service)` to the service entity) and `kube_persistentvolumeclaim_annotations` (joined on `(cluster, namespace, persistentvolumeclaim)` to the PVC entity, where the PVC entity derives its claim name from the `claim_name` label of `kube_pod_spec_volumes_persistentvolumeclaims_info`). A series missing the `cluster` label SHALL be bucketed under `cluster="unknown"` (the same rule as every other topology series).

The Application name SHALL be derived **identically to the pod ArgoCD Application** (graph-api "Pod `application` and `containers` attributes"): it is the segment of the tracking-id value **before the first `:`** (ArgoCD `<app>:<group>/<kind>:<ns>/<name>` form); a value with no `:` is taken verbatim; a value whose leading segment is empty resolves to **no** Application (the entity is absent from the application index, never present-but-empty).

`kube_service_annotations` and `kube_persistentvolumeclaim_annotations` are OPTIONAL: when absent, when no series matches a given entity, or when the matched series has an empty `annotation_argocd_argoproj_io_tracking_id` label, that entity SHALL carry no Application name and the build SHALL NOT fail. When the upstream reports more than one non-empty tracking-id for a single entity, the reader SHALL pick deterministically (the lexically smallest raw tracking-id value, mirroring the pod resolver) so the resolved Application is byte-stable across rebuilds.

The resolved Application name SHALL be surfaced on the service / PVC node's typed `application` attribute (graph-api "Pod `application` and `containers` attributes") and SHALL drive the node's `application` compound-group nesting (graph-api "Cytoscape compound node grouping"). It SHALL NOT be added to the entity's `labels` map.

For **PVC** entities specifically, when this annotation path resolves no Application (the annotation series is absent, unmatched, empty, or its leading segment is empty), a fallback MAY still resolve one by inheritance from a mounting pod — see "PVC ArgoCD Application inheritance from mounting pod". This fallback never applies to service entities.

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

#### Scenario: Service/PVC without a tracking-id annotation and no inheritance

- **WHEN** a service has no matching annotation series (or an empty `annotation_argocd_argoproj_io_tracking_id` label), or a PVC has neither a matching/non-empty annotation series **nor** a mounting pod with a resolved Application
- **THEN** that entity carries no Application name, nests under its namespace group, and the build does not fail

### Requirement: PVC ArgoCD Application inheritance from mounting pod

When a PVC entity has **no** ArgoCD Application resolved from its own annotation (the "Service and PVC ArgoCD Application resolution" path produced none — the `kube_persistentvolumeclaim_annotations` series is absent, unmatched, has an empty `annotation_argocd_argoproj_io_tracking_id` label, or that label's leading segment is empty), the topology reader SHALL attempt to **inherit** an Application from the pods that mount the PVC. For every `pod-mounts-pvc` edge incident to the PVC (source = a pod entity, target = the PVC entity), the mounting pod's own resolved Application (the pod ArgoCD Application from the `argocd_tracking_id` label on `kube_pod_owner`) is a candidate. The PVC SHALL inherit the **lexically-smallest non-empty** candidate Application; a mounting pod with no resolved Application contributes no candidate.

Inheritance is a strictly-ordered **fallback**: a PVC's own annotation-resolved Application ALWAYS wins and SHALL NEVER be overridden by inheritance. Inheritance fires **only** for a PVC that would otherwise carry no Application.

The inherited Application SHALL be surfaced and SHALL drive grouping **identically** to an annotation-resolved one — it is baked onto the PVC node's typed `application` attribute (graph-api "Pod `application` and `containers` attributes") and drives the PVC's `application` compound-group nesting (graph-api "Cytoscape compound node grouping"), so the wire output is **indistinguishable** from a natively-resolved PVC Application. The inherited value SHALL NOT be added to the PVC's `labels` map.

Because `pod-mounts-pvc` is always intra-cluster and a pod mounts a PVC in its own namespace, inheritance NEVER crosses cluster or namespace — the inherited Application's compound group (`<cluster>/namespace/<ns>/application/<app>`) nests under the PVC's own namespace.

Inheritance SHALL be resolved at build time over the fully-assembled graph (all pod entities and all `pod-mounts-pvc` edges present), **before** any projection/filter is applied, so a `?cluster=` / `?namespace=` / `?name=` filter that would drop the mounting pod from a given response SHALL NOT change the PVC's resolved Application (the value is computed once over the full graph, never recomputed per request — consistent with "build once, project many"). The result SHALL depend only on the **set** of mounting pods and their resolved Applications (selected by the lexically-smallest rule), independent of edge iteration order, so it is byte-stable across rebuilds. A PVC with no incident `pod-mounts-pvc` edge, or whose every mounting pod has no resolved Application, SHALL carry no Application and SHALL nest under its namespace group; the build SHALL NOT fail.

#### Scenario: PVC inherits the mounting pod's Application

- **WHEN** a PVC `cluster-alpha/shop/checkout-data` has no `kube_persistentvolumeclaim_annotations` tracking-id annotation and a pod `cluster-alpha/<uid>` resolving Application `checkout` mounts it via a `pod-mounts-pvc` edge
- **THEN** the `cluster-alpha/shop/checkout-data` PVC entity resolves Application `checkout`, surfaced on its `application` attribute (no `application` / `argocd_tracking_id` key in `labels`), and nesting it under the `cluster-alpha/namespace/shop/application/checkout` compound group

#### Scenario: Own annotation wins over inheritance

- **WHEN** a PVC resolves Application `mongo` from its own `annotation_argocd_argoproj_io_tracking_id` AND is mounted by a pod resolving Application `checkout`
- **THEN** the PVC keeps Application `mongo` (its own annotation) and never inherits `checkout`

#### Scenario: Deterministic lexically-smallest pick across multiple mounting pods

- **WHEN** a PVC with no own annotation is mounted by two pods resolving Applications `b-app` and `a-app`
- **THEN** the PVC inherits `a-app` (the lexically smallest non-empty candidate) deterministically across rebuilds, independent of edge iteration order

#### Scenario: Mounting pod with no Application yields no inheritance

- **WHEN** a PVC with no own annotation is mounted only by pods that have no resolved Application
- **THEN** the PVC carries no Application, nests under its namespace group, and the build does not fail

#### Scenario: Inheritance is resolved before projection

- **WHEN** a PVC inherits Application `checkout` from its mounting pod at build time
- **THEN** the PVC node's `application` attribute is `checkout` in the response under any filter projection, because inheritance is resolved over the full graph before projection and is not recomputed per request

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
