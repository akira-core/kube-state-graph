## MODIFIED Requirements

### Requirement: Topology series consumed

The topology reader SHALL consume at minimum the following `kube-state-metrics` series, each carrying a `cluster` external label. The bracketed tag after each series names the request-scoped matchers it receives (see "Request-scoped upstream selectors"): **[AECN]** = `az`, `env`, `cluster`, `namespace`; **[AEC]** = `az`, `env`, `cluster`.

- `kube_pod_info{cluster, namespace, pod, uid, node, pod_ip, host_ip, ...}` **[AECN]** (`pod_ip` and `host_ip` are surfaced when present)
- `kube_node_info{cluster, node, ...}` **[AEC]**
- `kube_node_status_addresses{cluster, node, type=~"ExternalIP|InternalIP", address, ...}` **[AEC]** (the anchored alternation selects exactly the two address types; ExternalIP is preferred and InternalIP is the fallback for the node `ipaddress` attribute)
- `kube_pod_spec_volumes_persistentvolumeclaims_info{cluster, namespace, pod, volume, claim_name, ...}` **[AECN]**
- `kube_node_labels{cluster, node, label_*, ...}` **[AEC]**
- `kube_service_info{cluster, namespace, service, cluster_ip, ...}` **[AECN]** (OPTIONAL — feeds the service/endpoint indexes)
- `kube_endpointslice_endpoints{cluster, namespace, endpointslice, address, targetref_kind, targetref_name, targetref_namespace, ...}` **[AECN]** (OPTIONAL — feeds the service/endpoint indexes)
- `kube_endpointslice_labels{cluster, namespace, endpointslice, label_kubernetes_io_service_name, ...}` **[AECN]** (OPTIONAL — joins each slice back to its owning service)
- `kube_pod_owner{cluster, namespace, pod, owner_kind, owner_name, owner_is_controller, ...}` **[AECN]** (OPTIONAL — feeds the pod controller-owner attribute and, through the resolved owner, the pod ArgoCD Application attribute)
- `kube_replicaset_owner{cluster, namespace, replicaset, owner_kind, owner_name, ...}` **[AECN]** (OPTIONAL — resolves a ReplicaSet pod owner up to its owning Deployment)
- `kube_job_owner{cluster, namespace, job_name, owner_kind, owner_name, owner_is_controller, ...}` **[AECN]** (OPTIONAL — resolves a Job up to its owning CronJob **for ArgoCD Application resolution only**; it SHALL NOT alter the pod `owner` attribute)
- `kube_persistentvolumeclaim_info{cluster, namespace, persistentvolumeclaim, storageclass, volumename, ...}` **[AECN]** (OPTIONAL — feeds the PVC `storageclass` attribute and — via the `volumename` label — the PVC `volumename` label that roots the NetApp Harvest join defined by the `netapp-storage-graph` capability)
- `kube_pod_container_info{cluster, namespace, pod, uid, container, image, ...}` **[AECN]** (OPTIONAL — feeds the per-pod container list attribute; one series per container)
- `kube_node_status_condition{cluster, node, condition="Ready", status, ...}` **[AEC]** (OPTIONAL — feeds the K8s node `ready_status` attribute; the `condition="Ready"` selector is a fixed, request-invariant metric-selection contract, and the `status` label carries `true`/`false`/`unknown` **matched case-insensitively** — stock kube-state-metrics lowercases the value, but an exporter re-publishing the raw Kubernetes `v1.ConditionStatus` enum verbatim emits `True`/`False`/`Unknown` — with the active row's sample value being `1`)
- `kube_persistentvolumeclaim_annotations{cluster, namespace, persistentvolumeclaim, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]** (OPTIONAL — feeds the PVC ArgoCD Application attribute; the `annotation_argocd_argoproj_io_tracking_id` label is kube-state-metrics' sanitised form of the `argocd.argoproj.io/tracking-id` annotation and requires the operator's `--metric-annotations-allowlist=persistentvolumeclaims=[argocd.argoproj.io/tracking-id]`)
- `kube_service_annotations{cluster, namespace, service, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]** (OPTIONAL — feeds the service ArgoCD Application attribute; the `annotation_argocd_argoproj_io_tracking_id` label requires the operator's `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id]`)

The following six **controller-annotation** families feed the pod ArgoCD Application attribute (see "Pod ArgoCD Application attribute"). Each is OPTIONAL and **[AECN]**, each carries the `annotation_argocd_argoproj_io_tracking_id` label under the operator's `--metric-annotations-allowlist=<plural-resource>=[argocd.argoproj.io/tracking-id]`, and each is keyed by its own resource-identity label — note that the Job family's identity label is `job_name`, not `job`:

- `kube_deployment_annotations{cluster, namespace, deployment, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]**
- `kube_statefulset_annotations{cluster, namespace, statefulset, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]**
- `kube_daemonset_annotations{cluster, namespace, daemonset, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]**
- `kube_replicaset_annotations{cluster, namespace, replicaset, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]**
- `kube_job_annotations{cluster, namespace, job_name, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]**
- `kube_cronjob_annotations{cluster, namespace, cronjob, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]**

The kubelet volume-stats series of the "PVC usage from kubelet volume stats" requirement are **[AECN]**. The NetApp Harvest series of the `netapp-storage-graph` capability receive `az` and `env` only.

Every series above SHALL be queried at its bare (unprefixed) name — there is no configurable metric-name prefix. A request with no selector-level filter SHALL issue each query exactly as listed, with no request-scoped matcher.

The three service/endpointslice families are OPTIONAL: when absent (kube-state-metrics not exporting services or endpointslices), the reader SHALL still build a valid topology, the service/endpoint indexes are simply empty, and connection-string resolution in the pod-service-graph reader degrades gracefully — `"://"` service endpoints that cannot be resolved against an empty index become `external/<label>` nodes. Under a selector-level filter the indexes hold only the in-scope services and the in-scope backing pods.

`kube_persistentvolumeclaim_info` is likewise OPTIONAL: when absent — or when no series matches a given PVC — the reader SHALL still build a valid topology, the affected PVC entities carry no `storageclass` attribute, no `volumename` label (and hence no `svm` label and no `pvc-to-netapp-aggr` edge — see the `netapp-storage-graph` capability), and the Cytoscape serialiser nests those PVCs under their namespace group (`cluster > namespace > pvc`) like any other PVC.

`kube_pod_container_info` is likewise OPTIONAL: when absent — or when no series matches a given pod — the reader SHALL still build a valid topology, the affected pod entities carry no `containers` attribute, and the build does not fail.

The six controller-annotation families and `kube_job_owner` are likewise OPTIONAL: when absent — or when no series matches a given pod's resolved controller owner, or the matched series' `annotation_argocd_argoproj_io_tracking_id` label is empty — the reader SHALL still build a valid topology, the affected pod entities carry no `application` attribute and nest directly under their `controller` group (or their namespace group when they also have no controller owner), and the build does not fail. The reader SHALL NOT read any `argocd_tracking_id` label from `kube_pod_owner`; that label is no longer part of the series contract.

`kube_node_status_condition` is likewise OPTIONAL: when absent — or when no `condition="Ready"` series matches a given node — the reader SHALL still build a valid topology, the affected K8s node entities carry no `ready_status` attribute, and the build does not fail.

`kube_persistentvolumeclaim_annotations` and `kube_service_annotations` are likewise OPTIONAL: when absent — or when no series matches a given `(cluster, namespace, claim)` / `(cluster, namespace, service)`, or its `annotation_argocd_argoproj_io_tracking_id` label is empty — the reader SHALL still build a valid topology, the affected PVC / service entities carry no `application` attribute and nest under their namespace group, and the build does not fail.

#### Scenario: All families queried

- **WHEN** a graph build runs against an upstream containing all families above
- **THEN** the reader emits exactly one PromQL query per family for the build, each evaluated at the caller-supplied `end` over `end - start`

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

#### Scenario: Controller annotation metrics absent

- **WHEN** the upstream contains `kube_pod_owner` and `kube_replicaset_owner` but none of the six controller-annotation families and no `kube_job_owner` series for the window
- **THEN** the reader produces a valid topology in which every pod entity keeps its `owner` attribute and carries no `application` attribute, no PVC inherits an Application from a mounting pod, and the build does not fail

#### Scenario: Pod-level tracking-id label is not read

- **WHEN** the upstream contains `kube_pod_owner` series carrying a non-empty `argocd_tracking_id` label
- **THEN** the reader ignores that label entirely and resolves each pod's Application solely from its controller's `annotation_argocd_argoproj_io_tracking_id` — a pod whose controller carries no such annotation has no `application` attribute even though the pod-level label is populated

#### Scenario: Namespace-scoped series narrowed at the source

- **WHEN** a build runs with the namespace filter `shop` against an upstream holding pods in `shop` and `payments`
- **THEN** every **[AECN]** series is issued with `namespace="shop"`, every **[AEC]** series is issued without a namespace matcher, the topology holds only `shop` pods, claims, services, and endpoints, and every node of the loaded clusters

### Requirement: Pod ArgoCD Application attribute

The topology reader SHALL resolve each pod's **ArgoCD Application** from the `annotation_argocd_argoproj_io_tracking_id` label carried on the annotation series of the pod's **controller**, and surface it on the pod entity as a typed, nullable `application` attribute (a string), serialised as `data.application` (`omitempty`) and **never inside `labels`**. ArgoCD stamps `argocd.argoproj.io/tracking-id` on the workload objects it applies, never on the pods a controller spawns, so the controller is the only place the value exists; the reader SHALL NOT read any `argocd_tracking_id` label from `kube_pod_owner`.

**Controller key.** The lookup key SHALL be the pod's own resolved controller owner from "Pod controller-owner attribute with ReplicaSet skip" — `(cluster, namespace, owner_kind, owner_name)` — so a pod owned through a ReplicaSet is looked up against its already-resolved **Deployment**, and no additional owner hop is needed for that case. A pod with no controller owner SHALL resolve no Application.

**Controller-annotation index.** The reader SHALL build one index keyed `(cluster, namespace, kind, name)` from the six controller-annotation families, each contributing its own owner kind and identity label:

| Owner kind | Series | Identity label |
|---|---|---|
| `Deployment` | `kube_deployment_annotations` | `deployment` |
| `StatefulSet` | `kube_statefulset_annotations` | `statefulset` |
| `DaemonSet` | `kube_daemonset_annotations` | `daemonset` |
| `ReplicaSet` | `kube_replicaset_annotations` | `replicaset` |
| `Job` | `kube_job_annotations` | `job_name` |
| `CronJob` | `kube_cronjob_annotations` | `cronjob` |

A series whose identity label is empty SHALL be skipped. A series missing the `cluster` label SHALL be bucketed under `cluster="unknown"` (the same rule as every other topology series).

**Job → CronJob hop.** When the resolved owner kind is `Job` and the index holds no entry for that Job, the reader SHALL resolve the Job's owning CronJob via `kube_job_owner` keyed `(cluster, namespace, job_name=<owner_name>)` selecting the series with `owner_is_controller="true"` and `owner_kind="CronJob"`, and SHALL then look the index up again at `(cluster, namespace, "CronJob", <owner_name of the job_owner series>)`. This hop exists because the Kubernetes CronJob controller propagates only `spec.jobTemplate.metadata` annotations onto the Jobs it creates — never the CronJob object's own annotations — so ArgoCD's tracking-id never reaches the Job. A directly ArgoCD-managed Job is resolved by its own annotation before the hop is attempted. The hop is **resolution-only**: it SHALL NOT change the pod's `owner` attribute, which stays `{kind:"Job", name:<job>}`, nor its `controller` compound group. On a defensive collision (more than one controller CronJob owner for a Job) the lexically-smallest `owner_name` SHALL win.

**Application name.** The Application name SHALL be the substring of the tracking-id value **before the first `:`** (ArgoCD annotation-based tracking-id form `<app>:<group>/<kind>:<namespace>/<name>`); when the value contains no `:`, the **entire value** SHALL be surfaced verbatim; when the leading segment is empty (e.g. `:apps/Deployment:ns/x`) the controller SHALL resolve to **no** Application rather than to an empty string. This is byte-for-byte the parse the service and PVC resolvers use, so a workload, its Service and its PVC derive their Application from one grammar.

**Determinism.** When more than one distinct non-empty tracking-id is observed for a single `(cluster, namespace, kind, name)`, the reader SHALL pick the **lexically-smallest** raw value whose derived Application is non-empty, so the emitted entity is stable and order-free across rebuilds (D6), mirroring the service and PVC resolvers.

**Unsupported owner kinds.** A pod whose resolved controller owner is of a kind with no kube-state-metrics annotation family — `ReplicationController` (no `kube_replicationcontroller_annotations` exists), `Node` (static / mirror pods), and any third-party CRD controller such as argo-rollouts `Rollout` or OpenKruise `CloneSet` — SHALL resolve no Application. The reader SHALL NOT fail the build, SHALL keep the pod's `owner` attribute, and SHALL NOT emit an empty string.

**Optionality.** The six controller-annotation families and `kube_job_owner` are OPTIONAL: when the annotation allowlist is not configured (their common state under stock kube-state-metrics) the vectors are empty, no pod resolves an Application, and the build SHALL NOT fail. When the label is absent, empty, or unmatched for a pod, the reader SHALL emit a nil `application` so `data.application` is omitted entirely — it SHALL NOT emit an empty string or any application key in `labels`.

This requirement introduces NO new node or edge type — the Application is a typed attribute on the existing `type="pod"` node (the same precedent as the `owner` attribute), keeping `labels` a strict `map[string]string` of typological metadata.

#### Scenario: Pod with a full ArgoCD tracking-id

- **WHEN** `kube_pod_owner{cluster="cluster-alpha", namespace="shop", pod="checkout-1", owner_kind="ReplicaSet", owner_name="checkout-7f9c", owner_is_controller="true"}`, `kube_replicaset_owner{cluster="cluster-alpha", namespace="shop", replicaset="checkout-7f9c", owner_kind="Deployment", owner_name="checkout"}` and `kube_deployment_annotations{cluster="cluster-alpha", namespace="shop", deployment="checkout", annotation_argocd_argoproj_io_tracking_id="storefront:apps/Deployment:shop/checkout"}` are present
- **THEN** the emitted pod entity has `application="storefront"` (the segment before the first `:`), `owner={kind:"Deployment", name:"checkout"}`, and no tracking-id key in `labels`

#### Scenario: Pod owned directly by a StatefulSet

- **WHEN** a pod's resolved owner is `{kind:"StatefulSet", name:"mongo"}` and `kube_statefulset_annotations{cluster="cluster-alpha", namespace="db", statefulset="mongo", annotation_argocd_argoproj_io_tracking_id="mongo:apps/StatefulSet:db/mongo"}` is present
- **THEN** the emitted pod entity has `application="mongo"`

#### Scenario: Pod owned directly by a DaemonSet

- **WHEN** a pod's resolved owner is `{kind:"DaemonSet", name:"fluentd"}` and `kube_daemonset_annotations{cluster="cluster-alpha", namespace="platform", daemonset="fluentd", annotation_argocd_argoproj_io_tracking_id="logging:apps/DaemonSet:platform/fluentd"}` is present
- **THEN** the emitted pod entity has `application="logging"`

#### Scenario: Pod owned by a bare ReplicaSet

- **WHEN** a pod's resolved owner is `{kind:"ReplicaSet", name:"adhoc-rs"}` (no owning Deployment) and `kube_replicaset_annotations{cluster="cluster-alpha", namespace="shop", replicaset="adhoc-rs", annotation_argocd_argoproj_io_tracking_id="adhoc:apps/ReplicaSet:shop/adhoc-rs"}` is present
- **THEN** the emitted pod entity has `application="adhoc"`

#### Scenario: Pod owned by a CronJob-created Job

- **WHEN** a pod's resolved owner is `{kind:"Job", name:"nightly-28901"}`, no `kube_job_annotations` series matches that Job, `kube_job_owner{cluster="cluster-alpha", namespace="batch", job_name="nightly-28901", owner_kind="CronJob", owner_name="nightly", owner_is_controller="true"}` is present, and `kube_cronjob_annotations{cluster="cluster-alpha", namespace="batch", cronjob="nightly", annotation_argocd_argoproj_io_tracking_id="reports:batch/CronJob:batch/nightly"}` is present
- **THEN** the emitted pod entity has `application="reports"` and its `owner` attribute remains `{kind:"Job", name:"nightly-28901"}` — the hop does not rewrite the owner

#### Scenario: Directly managed Job resolves before the hop

- **WHEN** a pod's resolved owner is `{kind:"Job", name:"migrate-1"}` and `kube_job_annotations{cluster="cluster-alpha", namespace="shop", job_name="migrate-1", annotation_argocd_argoproj_io_tracking_id="migrations:batch/Job:shop/migrate-1"}` is present alongside a `kube_job_owner` series naming a CronJob with a different tracking-id
- **THEN** the emitted pod entity has `application="migrations"` (the Job's own annotation wins; the CronJob hop is not attempted)

#### Scenario: Pod with a bare Application name (no colon)

- **WHEN** a pod's controller annotation value is `checkout` (no `:`)
- **THEN** the emitted pod entity has `application="checkout"`

#### Scenario: Empty leading segment yields no Application

- **WHEN** a pod's controller annotation value is `:apps/Deployment:shop/checkout`
- **THEN** the emitted pod entity has a nil `application` (`data.application` omitted entirely)

#### Scenario: Deterministic pick on duplicate controller annotation series

- **WHEN** two `kube_deployment_annotations` series for the same `(cluster, namespace, deployment)` report tracking-ids `zeta:apps/Deployment:shop/x` and `alpha:apps/Deployment:shop/x`
- **THEN** every pod owned by that Deployment resolves `application="alpha"` (the lexically-smallest raw value) and the choice is identical across rebuilds

#### Scenario: Pod with no ArgoCD label

- **WHEN** a pod's resolved owner is `{kind:"Deployment", name:"checkout"}` and the matching `kube_deployment_annotations` series carries an empty `annotation_argocd_argoproj_io_tracking_id` (or no series matches)
- **THEN** the emitted pod entity has a nil `application` (`data.application` omitted entirely) and carries no application key in `labels`

#### Scenario: Pod with no controller owner

- **WHEN** no `kube_pod_owner` series with `owner_is_controller="true"` exists for a pod
- **THEN** the emitted pod entity has a nil `application` and no controller-annotation lookup is attempted

#### Scenario: Owner kind with no annotation family

- **WHEN** a pod's resolved owner is `{kind:"Rollout", name:"canary"}` or `{kind:"ReplicationController", name:"legacy"}`
- **THEN** the emitted pod entity keeps that `owner`, has a nil `application`, and the build does not fail

#### Scenario: ArgoCD label absent entirely

- **WHEN** the upstream contains `kube_pod_owner` series but none of the six controller-annotation families for the window
- **THEN** the reader produces a valid topology with no `application` on any pod and does not fail the build

### Requirement: Request-scoped upstream selectors

The build SHALL accept four request-scoped selector dimensions — `az`, `env`, `cluster`, `namespace` — each a set of values, and SHALL render them into the upstream PromQL queries as label matchers composed **with** (never replacing) each query's fixed, request-invariant selectors (`type=~"ExternalIP|InternalIP"`, `condition="Ready"`, `lun=""`, the service-graph sentinel and `edge_relation!="link"` matchers). Which dimensions reach which series is a hardcoded contract with no configuration surface:

- `az` and `env`: every kube-state-metrics, kubelet, and NetApp Harvest series.
- `cluster`: every kube-state-metrics and kubelet series. **Never** a Harvest series — Harvest's `cluster` label names the ONTAP cluster, not a Kubernetes cluster — and never a service-graph series.
- `namespace`: every kube-state-metrics and kubelet series that carries a `namespace` label — the pod-, claim-, Service-, EndpointSlice-, and **controller**-scoped series (the six controller-annotation families and `kube_job_owner` are namespaced like every other workload family). Never a node series, never a Harvest series, never a service-graph series.
- The three `traces_service_graph_*` series and the `up{}` probe SHALL carry **no** request-scoped matcher under any request.

Rendering SHALL be a pure function of the sorted, de-duplicated value set: one value renders `<key>="<value>"` (with `"` and `\` escaped); two or more render one fully-anchored alternation `<key>=~"<v1>|<v2>"` whose alternatives are regex-quoted and THEN string-escaped (a backslash introduced by regex-quoting is doubled, because a PromQL string literal rejects an unknown escape sequence); an empty set renders nothing. Matchers inside a selector SHALL appear in a fixed order (fixed selectors first, then `az`, `env`, `cluster`, `namespace`). The `cluster` value `unknown` renders `cluster=~"unknown|"` (see "Series missing the cluster label"), the one value that is not rendered as a plain literal. Series families that carry no request-scoped matcher for a dimension are narrowed by **reference** instead — a node is emitted only when a loaded pod is scheduled on it, an aggregate only when a loaded claim's `volumename` joins to it — as specified by the `graph-api` retention requirements.

A build with every dimension empty SHALL issue each query exactly as it is issued today. Zero rows under a non-empty dimension is a valid, empty topology — not a failure and not a retention miss.

#### Scenario: Fixed selector composed with request matchers

- **WHEN** a build runs with `az={zone-a}` and `cluster={cluster-alpha}`
- **THEN** the node-address query is issued as `last_over_time(kube_node_status_addresses{type=~"ExternalIP|InternalIP",az="zone-a",cluster="cluster-alpha"}[<window>])` and the node-condition query keeps `condition="Ready"` ahead of the request matchers

#### Scenario: Controller-annotation families receive all four dimensions

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={cluster-alpha}` and `namespace={shop}`
- **THEN** each of the six controller-annotation queries and the `kube_job_owner` query is issued with exactly `az="zone-a",env="prod",cluster="cluster-alpha",namespace="shop"` added, in that fixed order

#### Scenario: Harvest receives zone and environment but not cluster or namespace

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={cluster-alpha}`, `namespace={shop}`
- **THEN** every Harvest query (`volume_labels`, the `qos_*` families, `qos_policy_fixed_max_throughput_*`, `aggr_*`, `node_new_status`) is issued with exactly `az="zone-a",env="prod"` added (the `qos_*` families keeping `lun=""`), with no `cluster` and no `namespace` matcher

#### Scenario: Service-graph series are never narrowed

- **WHEN** a build runs with any non-empty combination of the four dimensions
- **THEN** the three `traces_service_graph_*` queries are issued exactly as for an unfiltered build (sentinel and link matchers only), and the `up{}` probe query is the bare `up`

#### Scenario: Multi-value rendering is order-free

- **WHEN** one build runs with `namespace={b, a}` and another with `namespace={a, b, a}`
- **THEN** both issue `namespace=~"a|b"` on every namespace-scoped series

#### Scenario: Regex metacharacters in a value are quoted

- **WHEN** a build runs with `env={prod.eu, prod-us}`
- **THEN** the rendered alternation is `env=~"prod-us|prod\\.eu"` — the metacharacter is regex-quoted AND the resulting backslash is escaped for the PromQL string literal, which rejects an unknown escape sequence — so a series with `env="prodXeu"` does not match

#### Scenario: Empty dimensions render nothing

- **WHEN** a build runs with all four dimensions empty
- **THEN** every issued query string is byte-identical to the query issued before request-scoped selectors existed
