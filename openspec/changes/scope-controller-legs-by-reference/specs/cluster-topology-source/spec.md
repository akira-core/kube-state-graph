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
- `kube_job_owner{cluster, namespace, job_name, owner_kind="CronJob", owner_name, owner_is_controller="true", ...}` **[AECN]** (OPTIONAL — resolves a Job up to its owning CronJob **for ArgoCD Application resolution only**; it SHALL NOT alter the pod `owner` attribute. The `owner_kind="CronJob"` and `owner_is_controller="true"` matchers are a fixed, request-invariant metric-selection contract — the reader keeps exactly those rows and no other, so selecting them upstream is output-preserving)
- `kube_persistentvolumeclaim_info{cluster, namespace, persistentvolumeclaim, storageclass, volumename, ...}` **[AECN]** (OPTIONAL — feeds the PVC `storageclass` attribute and — via the `volumename` label — the PVC `volumename` label that roots the NetApp Harvest join defined by the `netapp-storage-graph` capability)
- `kube_pod_container_info{cluster, namespace, pod, uid, container, image, ...}` **[AECN]** (OPTIONAL — feeds the per-pod container list attribute; one series per container. The one topology family whose cardinality multiplies with image variants and with pod churn inside the window, so it is the first to meet an upstream series limit — see the query-error paragraph below)
- `kube_node_status_condition{cluster, node, condition="Ready", status, ...}` **[AEC]** (OPTIONAL — feeds the K8s node `ready_status` attribute; the `condition="Ready"` selector is a fixed, request-invariant metric-selection contract, and the `status` label carries `true`/`false`/`unknown` **matched case-insensitively** — stock kube-state-metrics lowercases the value, but an exporter re-publishing the raw Kubernetes `v1.ConditionStatus` enum verbatim emits `True`/`False`/`Unknown` — with the active row's sample value being `1`)
- `kube_persistentvolumeclaim_annotations{cluster, namespace, persistentvolumeclaim, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]** (OPTIONAL — feeds the PVC ArgoCD Application attribute; the `annotation_argocd_argoproj_io_tracking_id` label is kube-state-metrics' sanitised form of the `argocd.argoproj.io/tracking-id` annotation and requires the operator's `--metric-annotations-allowlist=persistentvolumeclaims=[argocd.argoproj.io/tracking-id]`)
- `kube_service_annotations{cluster, namespace, service, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]** (OPTIONAL — feeds the service ArgoCD Application attribute; the `annotation_argocd_argoproj_io_tracking_id` label requires the operator's `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id]`)

The following six **controller-annotation** families feed the pod ArgoCD Application attribute (see "Pod ArgoCD Application attribute"). Each is OPTIONAL and **[AECN]**, each carries the `annotation_argocd_argoproj_io_tracking_id` label under the operator's `--metric-annotations-allowlist=<plural-resource>=[argocd.argoproj.io/tracking-id]`, and each is keyed by its own resource-identity label — note that the Job family's identity label is `job_name`, not `job`.

Each of the six SHALL be queried with the fixed, request-invariant matcher `annotation_argocd_argoproj_io_tracking_id!=""`. kube-state-metrics emits one series per workload object of that kind whether or not the annotation is allowlisted or present, and the reader discards every series whose tracking-id is empty before it is counted or keyed, so restricting the query to annotated objects is **output-preserving** — the same set of pods resolves the same Applications, and the missing-`cluster` diagnostic tally is unchanged. PromQL treats an absent label as the empty string, so a build against a kube-state-metrics with no annotation allowlist receives an empty vector rather than one series per workload object.

- `kube_deployment_annotations{cluster, namespace, deployment, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]**
- `kube_statefulset_annotations{cluster, namespace, statefulset, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]**
- `kube_daemonset_annotations{cluster, namespace, daemonset, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]**
- `kube_replicaset_annotations{cluster, namespace, replicaset, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]**
- `kube_job_annotations{cluster, namespace, job_name, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]**
- `kube_cronjob_annotations{cluster, namespace, cronjob, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]**

The kubelet volume-stats series of the "PVC usage from kubelet volume stats" requirement are **[AECN]**. The NetApp Harvest series of the `netapp-storage-graph` capability receive **no** request-scoped matcher — the `az` dimension reaches them through backend selection only (the `upstream-backend-routing` capability's zone rule) and `env` does not reach them at all.

Every series above SHALL be queried at its bare (unprefixed) name — there is no configurable metric-name prefix. A request with no selector-level filter SHALL issue each query exactly as listed, with no request-scoped matcher.

Which of the families above a build issues, and how, depends on the endpoint. A `/v1/graph` build SHALL issue every family unrestricted. A `/v1/storage-graph` build SHALL issue the subset the `storage-graph-api` capability's "Storage build reads only what it draws" requirement defines: the families whose output that body cannot carry are not read at all; the pod families, the four Kubernetes-node families and the eight controller-owner / controller-annotation families are read **by reference** — each restricted to the object names the families read before it actually carry (claim bindings name the pods, the loaded pods name their nodes and owners, the resolved owners name their Deployments and CronJobs), composed with the family's fixed selector and the request-scoped matchers listed above; and the claim, Harvest and `ALERTS` families are read unrestricted. A family a build does not issue — including a by-reference family whose scope came out empty — SHALL be absent from that build's per-family series tally, never reported as zero: zero means "read, matched nothing", and a by-reference family's count is the count of series its restriction matched.

The three service/endpointslice families are OPTIONAL: when absent (kube-state-metrics not exporting services or endpointslices), the reader SHALL still build a valid topology, the service/endpoint indexes are simply empty, and connection-string resolution in the pod-service-graph reader degrades gracefully — `"://"` service endpoints that cannot be resolved against an empty index become `external/<label>` nodes. Under a selector-level filter the indexes hold only the in-scope services and the in-scope backing pods.

`kube_persistentvolumeclaim_info` is likewise OPTIONAL: when absent — or when no series matches a given PVC — the reader SHALL still build a valid topology, the affected PVC entities carry no `storageclass` attribute, no `volumename` label (and hence no `svm` label and no `pvc-to-netapp-aggr` edge — see the `netapp-storage-graph` capability), and the Cytoscape serialiser nests those PVCs under their namespace group (`cluster > namespace > pvc`) like any other PVC.

`kube_pod_container_info` is likewise OPTIONAL: when absent — or when no series matches a given pod, or when its query fails (see the query-error paragraph below) — the reader SHALL still build a valid topology, the affected pod entities carry no `containers` attribute, and the build does not fail.

The six controller-annotation families and `kube_job_owner` are likewise OPTIONAL: when absent — or when no series matches a given pod's resolved controller owner, or the matched series' `annotation_argocd_argoproj_io_tracking_id` label is empty — the reader SHALL still build a valid topology, the affected pod entities carry no `application` attribute and nest directly under their `controller` group (or their namespace group when they also have no controller owner), and the build does not fail. The reader SHALL NOT read any `argocd_tracking_id` label from `kube_pod_owner`; that label is no longer part of the series contract.

**Query error is a separate axis from an empty vector**, and the kube-state-metrics legs split on it. Three legs SHALL degrade on a query error: the failure is logged, the family is treated as an empty vector, and the build completes without what that family would have supplied. Two are the controller-annotation families whose cardinality **accumulates with history** rather than tracking the live object count — `kube_replicaset_annotations` (one series per ReplicaSet retained by a Deployment's `revisionHistoryLimit`) and `kube_job_annotations` (one per Job retained by a CronJob's history limits) — so they are the two that can exceed an upstream series or sample limit in an estate whose live object count is unremarkable, and losing an `application` string is never worth failing the whole graph. The third is `kube_pod_container_info`, whose cardinality **multiplies with the live object count**: one series per container per image variant, and — because the family is read over the whole window — one per pod that existed at any instant of it, so pod churn inflates it further. It is the largest kube-state-metrics family in any estate and the first to meet a memory-derived upstream series limit, and the `containers` attribute it feeds is a presentation attribute whose loss is subtractive. Every other kube-state-metrics leg — including the Deployment, StatefulSet, DaemonSet and CronJob annotation families and `kube_job_owner` — SHALL fail the build on a query error. Caller-originated cancellation — a build timeout or a disconnected client — SHALL still fail the request for the three degrading legs, since the caller is no longer waiting for any result.

A degrade SHALL be **subtractive**: it removes Applications the failed family would have supplied and never substitutes a different one. This requires one gate. The Job → CronJob hop is taken only when the Job carries no annotation of its own, a fact a family that was never read cannot establish — so when the `kube_job_annotations` query fails, the reader SHALL suppress the hop for that build rather than let every Job miss fall through to its owning CronJob's Application. A Job that genuinely carries no annotation of its own therefore also resolves no Application while that leg is degraded. `kube_replicaset_annotations` needs no equivalent gate: a bare ReplicaSet has no further ancestor to consult, so its miss resolves nothing either way.

`kube_node_status_condition` is likewise OPTIONAL: when absent — or when no `condition="Ready"` series matches a given node — the reader SHALL still build a valid topology, the affected K8s node entities carry no `ready_status` attribute, and the build does not fail.

`kube_persistentvolumeclaim_annotations` and `kube_service_annotations` are likewise OPTIONAL: when absent — or when no series matches a given `(cluster, namespace, claim)` / `(cluster, namespace, service)`, or its `annotation_argocd_argoproj_io_tracking_id` label is empty — the reader SHALL still build a valid topology, the affected PVC / service entities carry no `application` attribute and nest under their namespace group, and the build does not fail.

#### Scenario: All families queried

- **WHEN** a `/v1/graph` build runs against an upstream containing all families above
- **THEN** the reader emits exactly one PromQL query per family for the build, each evaluated at the caller-supplied `end` over `end - start`; a `/v1/storage-graph` build against the same upstream issues only the subset the `storage-graph-api` capability defines, its by-reference families one query per chunk of their scope

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

#### Scenario: Annotation allowlist not configured yields an empty vector

- **WHEN** kube-state-metrics runs the `deployments` collector but no `--metric-annotations-allowlist` entry for it, so every `kube_deployment_annotations` series lacks the `annotation_argocd_argoproj_io_tracking_id` label
- **THEN** the query returns no series at all (the fixed matcher excludes a series whose label is absent), no pod resolves an Application from that family, the build does not fail, and the outcome is indistinguishable from the family being absent

#### Scenario: Accumulating-cardinality family degrades on a query error

- **WHEN** the `kube_replicaset_annotations` or `kube_job_annotations` query fails upstream — a timeout, a 5xx, or a series/sample limit exceeded — while the caller's own deadline has not expired
- **THEN** the failure is logged, that family is treated as an empty vector, every other family still resolves, pods owned by a bare ReplicaSet (or a Job) carry no `application` attribute, and the request returns a valid `200` graph

#### Scenario: A degraded Job annotation family suppresses the CronJob hop

- **WHEN** the `kube_job_annotations` query fails upstream while `kube_job_owner` and `kube_cronjob_annotations` both resolve, and a pod is owned by a Job that carries its own tracking-id under a CronJob that carries a different one
- **THEN** the pod carries no `application` attribute — the hop is suppressed for the build, so the pod is never attributed to the CronJob's Application — while the same fixture with the Job family read but genuinely empty still resolves the CronJob's Application through the hop

#### Scenario: A required annotation family still fails the build

- **WHEN** the `kube_deployment_annotations`, `kube_statefulset_annotations`, `kube_daemonset_annotations`, `kube_cronjob_annotations`, or `kube_job_owner` query fails upstream
- **THEN** the build fails and the request returns an error, as it does for every other kube-state-metrics topology leg

#### Scenario: Caller cancellation still fails a degrading family

- **WHEN** the `kube_job_annotations` query fails because the caller's own context is already done — the build timeout elapsed or the client disconnected
- **THEN** the request fails rather than degrading, since the failure is the caller's deadline rather than an upstream fault

#### Scenario: Job-owner rows the reader discards are never fetched

- **WHEN** the upstream holds `kube_job_owner` series for Jobs owned by a CronJob and for Jobs with no controller owner or a non-CronJob owner
- **THEN** only the `owner_kind="CronJob"` AND `owner_is_controller="true"` series are fetched, and the resolved Job → CronJob index is identical to the one built by fetching every series and filtering in the reader

#### Scenario: Pod-level tracking-id label is not read

- **WHEN** the upstream contains `kube_pod_owner` series carrying a non-empty `argocd_tracking_id` label
- **THEN** the reader ignores that label entirely and resolves each pod's Application solely from its controller's `annotation_argocd_argoproj_io_tracking_id` — a pod whose controller carries no such annotation has no `application` attribute even though the pod-level label is populated

#### Scenario: Namespace-scoped series narrowed at the source

- **WHEN** a build runs with the namespace filter `shop` against an upstream holding pods in `shop` and `payments`
- **THEN** every **[AECN]** series is issued with `namespace="shop"`, every **[AEC]** series is issued without a namespace matcher, the topology holds only `shop` pods, claims, services, and endpoints, and every node of the loaded clusters

#### Scenario: Container info query error degrades

- **WHEN** the `kube_pod_container_info` query fails with an upstream error (for example a series-limit rejection) while every other family succeeds
- **THEN** the build completes, no pod entity carries a `containers` attribute, `kube_state_graph_upstream_query_failures_total{query="kube_pod_container_info"}` increments by 1, and every other attribute, node and edge is unchanged

#### Scenario: Caller cancellation still fails the container leg

- **WHEN** the caller's context is already cancelled when the `kube_pod_container_info` query returns its error
- **THEN** the build returns an error rather than degrading

#### Scenario: Storage build tallies a by-reference family by what it matched

- **WHEN** a `/v1/storage-graph` build loads pods owned by two StatefulSets and no other controller kind
- **THEN** the build's per-family tally carries `kube_statefulset_annotations` with the count of series its `statefulset` restriction matched and carries no key at all for `kube_replicaset_owner`, `kube_job_owner`, `kube_deployment_annotations`, `kube_daemonset_annotations`, `kube_replicaset_annotations`, `kube_job_annotations` or `kube_cronjob_annotations`
