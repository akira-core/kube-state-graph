# cluster-topology-source Specification

## Purpose
Defines how kube-state-graph reads Kubernetes topology — pods, nodes, PVCs, services, controllers and their typed attributes — from kube-state-metrics and kubelet series held in one or more VictoriaMetrics stores, using PromQL only and never the Kubernetes API. It fixes which series are read, how request-scoped selectors narrow them, how cluster identities are composed, and how each optional family degrades.

## Requirements

### Requirement: Centralised VictoriaMetrics as the only topology source

The topology reader SHALL fetch all pod, node, and PVC topology by issuing PromQL queries against one or more configured Prometheus-compatible endpoints pointing at VictoriaMetrics, which SHALL be its only source of cluster facts. The reader SHALL NOT call the Kubernetes API server, SHALL NOT scrape `kube-state-metrics` directly, and SHALL NOT use Kubernetes informers.

#### Scenario: Endpoint choice follows the routing table

- **WHEN** the reader issues a topology query
- **THEN** the endpoint it is issued to is decided by the routing table defined by the `upstream-backend-routing` capability
- **AND** when no routing table is configured, the single `--prom-url` endpoint serves every query

#### Scenario: No Kubernetes client at any endpoint count

- **WHEN** any number of endpoints is configured
- **THEN** the endpoints remain the reader's only source of cluster facts, and no Kubernetes client, no informer, no kubeconfig, and no per-cluster RBAC is involved at any point

#### Scenario: Single configured upstream

- **WHEN** the server starts with `--prom-url=http://vm.example:8428` and no routing table
- **THEN** every topology query is sent to `http://vm.example:8428` and no other HTTP destinations

#### Scenario: Routed upstreams

- **WHEN** the server starts with a routing table declaring `http://vm-a.example:8428` and `http://vm-b.example:8428`
- **THEN** every topology query is sent to one or both of those two endpoints as the routing table dictates, and to no other HTTP destinations

#### Scenario: No Kubernetes API access

- **WHEN** the server runs in any environment
- **THEN** the binary makes no requests to any `/api/*` Kubernetes API path and requires no Kubernetes ServiceAccount or kubeconfig

### Requirement: Topology series consumed

The topology reader SHALL consume at minimum the `kube-state-metrics` series listed in this requirement's scenarios, each carrying a `cluster` external label, and SHALL query every series at its bare (unprefixed) name — there is no configurable metric-name prefix. A request with no selector-level filter SHALL issue each query exactly as listed, with no request-scoped matcher.

#### Scenario: Tags name the request-scoped matchers a series receives

- **WHEN** a consumed series is tagged in this spec
- **THEN** **[AECN]** means it receives the `az`, `env`, `cluster`, `namespace` request-scoped matchers and **[AEC]** means it receives `az`, `env`, `cluster` (see "Request-scoped upstream selectors")

#### Scenario: Pod, node and claim-binding series consumed

- **WHEN** a `/v1/graph` build runs
- **THEN** the reader consumes `kube_pod_info{cluster, namespace, pod, uid, node, pod_ip, host_ip, ...}` **[AECN]**, surfacing `pod_ip` and `host_ip` when present
- **AND** it consumes `kube_node_info{cluster, node, ...}` **[AEC]**
- **AND** it consumes `kube_node_status_addresses{cluster, node, type=~"ExternalIP|InternalIP", address, ...}` **[AEC]**, whose anchored alternation selects exactly the two address types; ExternalIP is preferred and InternalIP is the fallback for the node `ipaddress` attribute
- **AND** it consumes `kube_pod_spec_volumes_persistentvolumeclaims_info{cluster, namespace, pod, volume, claim_name, ...}` **[AECN]**
- **AND** it consumes `kube_node_labels{cluster, node, label_*, ...}` **[AEC]**

#### Scenario: Optional service and endpointslice series consumed

- **WHEN** a `/v1/graph` build runs
- **THEN** the reader consumes the OPTIONAL `kube_service_info{cluster, namespace, service, cluster_ip, ...}` **[AECN]** and `kube_endpointslice_endpoints{cluster, namespace, endpointslice, address, targetref_kind, targetref_name, targetref_namespace, ...}` **[AECN]**, which feed the service/endpoint indexes
- **AND** it consumes the OPTIONAL `kube_endpointslice_labels{cluster, namespace, endpointslice, label_kubernetes_io_service_name, ...}` **[AECN]**, which joins each slice back to its owning service

#### Scenario: Optional owner series consumed

- **WHEN** a `/v1/graph` build runs
- **THEN** the reader consumes the OPTIONAL `kube_pod_owner{cluster, namespace, pod, owner_kind, owner_name, owner_is_controller, ...}` **[AECN]**, which feeds the pod controller-owner attribute and, through the resolved owner, the pod ArgoCD Application attribute
- **AND** it consumes the OPTIONAL `kube_replicaset_owner{cluster, namespace, replicaset, owner_kind, owner_name, ...}` **[AECN]**, which resolves a ReplicaSet pod owner up to its owning Deployment
- **AND** it consumes the OPTIONAL `kube_job_owner` **[AECN]** under the fixed matchers of "Job-owner series queried with fixed CronJob matchers"
- **AND** it SHALL NOT read any `argocd_tracking_id` label from `kube_pod_owner`; that label is no longer part of the series contract

#### Scenario: Optional claim, container and node-condition series consumed

- **WHEN** a `/v1/graph` build runs
- **THEN** the reader consumes the OPTIONAL `kube_persistentvolumeclaim_info{cluster, namespace, persistentvolumeclaim, storageclass, volumename, ...}` **[AECN]**, which feeds the PVC `storageclass` attribute and — via the `volumename` label — the PVC `volumename` label that roots the NetApp Harvest join defined by the `netapp-storage-graph` capability
- **AND** it consumes the OPTIONAL `kube_pod_container_info{cluster, namespace, pod, uid, container, image, ...}` **[AECN]**, one series per container, which feeds the per-pod container list attribute; its cardinality multiplies with image variants and with pod churn inside the window, so it is the first family to meet an upstream series limit (see "Query errors degrade only the high-cardinality topology legs")
- **AND** it consumes the OPTIONAL `kube_node_status_condition{cluster, node, condition="Ready", status, ...}` **[AEC]**, which feeds the K8s node `ready_status` attribute; the `condition="Ready"` selector is a fixed, request-invariant metric-selection contract, and the `status` label carries `true`/`false`/`unknown` **matched case-insensitively** (stock kube-state-metrics lowercases the value, while an exporter re-publishing the raw Kubernetes `v1.ConditionStatus` enum verbatim emits `True`/`False`/`Unknown`), with the active row's sample value being `1`

#### Scenario: Optional PVC and service annotation series consumed

- **WHEN** a `/v1/graph` build runs
- **THEN** the reader consumes the OPTIONAL `kube_persistentvolumeclaim_annotations{cluster, namespace, persistentvolumeclaim, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]**, which feeds the PVC ArgoCD Application attribute; its `annotation_argocd_argoproj_io_tracking_id` label is kube-state-metrics' sanitised form of the `argocd.argoproj.io/tracking-id` annotation and requires the operator's `--metric-annotations-allowlist=persistentvolumeclaims=[argocd.argoproj.io/tracking-id]`
- **AND** it consumes the OPTIONAL `kube_service_annotations{cluster, namespace, service, annotation_argocd_argoproj_io_tracking_id, ...}` **[AECN]**, which feeds the service ArgoCD Application attribute; its `annotation_argocd_argoproj_io_tracking_id` label requires the operator's `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id]`

#### Scenario: Kubelet and Harvest series matchers

- **WHEN** a build reads the kubelet volume-stats series of "PVC usage from kubelet volume stats"
- **THEN** those series are **[AECN]**
- **AND** the NetApp Harvest series of the `netapp-storage-graph` capability receive **no** request-scoped matcher (that capability's "Harvest legs are zone-routed and carry no request matcher"), while the `az` dimension still selects their backends (the `upstream-backend-routing` capability's zone rule)

#### Scenario: Missing optional family

- **WHEN** the upstream contains `kube_pod_info` and `kube_node_info` but no `kube_node_labels` series for the window
- **THEN** the reader produces a valid topology with empty `labels` maps on node entities and does not fail the build

#### Scenario: StorageClass info metric absent

- **WHEN** the upstream contains `kube_storageclass_info` series for the window
- **THEN** the reader never queries the metric (it is removed from the topology fan-out), no `storageclass` entity is materialised, and PVC entities still resolve their `storageclass` attribute from `kube_persistentvolumeclaim_info`

#### Scenario: Trident custom-resource metrics absent

- **WHEN** the upstream contains (or lacks) `kube_tridentvolume_info` / `kube_tridentbackend_info` series for the window
- **THEN** the reader never queries either metric (the Trident chain is removed from the topology fan-out) and the PVC `svm` label resolves solely via the `netapp-storage-graph` capability's `volume_labels` join

#### Scenario: Pod-level tracking-id label is not read

- **WHEN** the upstream contains `kube_pod_owner` series carrying a non-empty `argocd_tracking_id` label
- **THEN** the reader ignores that label entirely and resolves each pod's Application solely from its controller's `annotation_argocd_argoproj_io_tracking_id` — a pod whose controller carries no such annotation has no `application` attribute even though the pod-level label is populated

#### Scenario: Namespace-scoped series narrowed at the source

- **WHEN** a build runs with the namespace filter `shop` against an upstream holding pods in `shop` and `payments`
- **THEN** every **[AECN]** series is issued with `namespace="shop"`, every **[AEC]** series is issued without a namespace matcher, the topology holds only `shop` pods, claims, services, and endpoints, and every node of the loaded clusters

### Requirement: Controller-annotation families queried with a fixed tracking-id matcher

The reader SHALL consume six OPTIONAL **[AECN]** controller-annotation families that feed the pod ArgoCD Application attribute, each keyed by its own resource-identity label and each queried with the fixed, request-invariant matcher `annotation_argocd_argoproj_io_tracking_id!=""`. The matcher SHALL be output-preserving.

#### Scenario: Six controller-annotation families

- **WHEN** a `/v1/graph` build runs
- **THEN** the reader issues `kube_deployment_annotations{cluster, namespace, deployment, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]**
- **AND** `kube_statefulset_annotations{cluster, namespace, statefulset, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]**
- **AND** `kube_daemonset_annotations{cluster, namespace, daemonset, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]**
- **AND** `kube_replicaset_annotations{cluster, namespace, replicaset, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]**
- **AND** `kube_job_annotations{cluster, namespace, job_name, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]** — the Job family's identity label is `job_name`, not `job`
- **AND** `kube_cronjob_annotations{cluster, namespace, cronjob, annotation_argocd_argoproj_io_tracking_id!="", ...}` **[AECN]**
- **AND** each family carries the `annotation_argocd_argoproj_io_tracking_id` label only under the operator's `--metric-annotations-allowlist=<plural-resource>=[argocd.argoproj.io/tracking-id]`

#### Scenario: Fixed tracking-id matcher is output-preserving

- **WHEN** kube-state-metrics emits one series per workload object of a kind, whether or not the annotation is allowlisted or present
- **THEN** restricting the query to annotated objects resolves the same set of pods to the same Applications and leaves the missing-`cluster` diagnostic tally unchanged, because the reader discards every series whose tracking-id is empty before it is counted or keyed
- **AND** since PromQL treats an absent label as the empty string, a build against a kube-state-metrics with no annotation allowlist receives an empty vector rather than one series per workload object

#### Scenario: Annotation allowlist not configured yields an empty vector

- **WHEN** kube-state-metrics runs the `deployments` collector but no `--metric-annotations-allowlist` entry for it, so every `kube_deployment_annotations` series lacks the `annotation_argocd_argoproj_io_tracking_id` label
- **THEN** the query returns no series at all (the fixed matcher excludes a series whose label is absent), no pod resolves an Application from that family, the build does not fail, and the outcome is indistinguishable from the family being absent

### Requirement: Job-owner series queried with fixed CronJob matchers

The reader SHALL query `kube_job_owner` with the fixed, request-invariant matchers `owner_kind="CronJob"` and `owner_is_controller="true"`, keeping exactly those rows, and SHALL use it only to resolve a Job up to its owning CronJob for ArgoCD Application resolution. It SHALL NOT alter the pod `owner` attribute.

#### Scenario: Job-owner selector

- **WHEN** a build issues the Job-owner query
- **THEN** it reads `kube_job_owner{cluster, namespace, job_name, owner_kind="CronJob", owner_name, owner_is_controller="true", ...}` **[AECN]**
- **AND** selecting those rows upstream is output-preserving, because the reader keeps exactly those rows and no other

#### Scenario: Job-owner rows the reader discards are never fetched

- **WHEN** the upstream holds `kube_job_owner` series for Jobs owned by a CronJob and for Jobs with no controller owner or a non-CronJob owner
- **THEN** only the `owner_kind="CronJob"` AND `owner_is_controller="true"` series are fetched, and the resolved Job → CronJob index is identical to the one built by fetching every series and filtering in the reader

### Requirement: Topology family selection per endpoint

A `/v1/graph` build SHALL issue every topology family unrestricted. A `/v1/storage-graph` build SHALL issue only the subset that the `storage-graph-api` capability's "Storage build reads every family by reference" requirement defines, reading `ALERTS` under the request-scoped matchers alone and every other family by reference.

#### Scenario: Storage build reads only what its body can carry

- **WHEN** a `/v1/storage-graph` build runs
- **THEN** the families whose output that body cannot carry are not read at all
- **AND** `ALERTS` is the one family read across the whole requested zone
- **AND** every other family is read **by reference** — restricted to the object names the request's root or the families read before it actually carry — composed with the family's fixed selector and the request-scoped matchers of "Topology series consumed"

#### Scenario: By-reference restriction keys

- **WHEN** a `/v1/storage-graph` build reads a family by reference
- **THEN** pods are restricted by Kubernetes node or by `(namespace, pod)` pair, claim bindings by `(namespace, pod)` pair or by `(namespace, claim)` pair, and claims by PersistentVolume or by `(namespace, claim)` pair
- **AND** claim annotations are restricted by `(namespace, claim)` pair or by Application tracking-id, and the kubelet volume-stats families by `(namespace, claim)` pair
- **AND** Kubernetes nodes are restricted by name, controllers by owner name or tracking-id, and the Harvest families by ONTAP component, token or `(ONTAP cluster, SVM)`

#### Scenario: All families queried

- **WHEN** a `/v1/graph` build runs against an upstream containing all families above
- **THEN** the reader emits exactly one PromQL query per family for the build, each evaluated at the caller-supplied `end` over `end - start`; a `/v1/storage-graph` build against the same upstream issues only the subset the `storage-graph-api` capability defines, its by-reference families one query per chunk of their scope

#### Scenario: Storage build reads no family but ALERTS across the zone

- **WHEN** a `/v1/storage-graph` build for `?az=zone-a&env=prod&node=worker-1` runs
- **THEN** every query it issues other than `ALERTS` carries a restriction on the family's identity label (or labels) beyond the request-scoped matchers, and `ALERTS` carries the request-scoped matchers alone

### Requirement: Storage build fails closed on topology query errors

A `/v1/storage-graph` build SHALL fail on a query error of any topology family except `ALERTS`, including the families this spec calls OPTIONAL, as the `storage-graph-api` capability's "Storage build fails closed on upstream query errors" requires. An ABSENT OPTIONAL family — an empty vector — SHALL never fail a build.

#### Scenario: Optional family query error fails a storage build

- **WHEN** a `/v1/storage-graph` build issues an OPTIONAL topology family and that query returns an error
- **THEN** the build fails rather than degrading

#### Scenario: Empty optional family does not fail a storage build

- **WHEN** a `/v1/storage-graph` build issues an OPTIONAL topology family and the query succeeds with an empty vector
- **THEN** the build does not fail on that family

### Requirement: Per-family series tally

A family a build does not issue — including a by-reference family whose scope came out empty — SHALL be absent from that build's per-family series tally, never reported as zero. A by-reference family's count SHALL be the count of series its restriction matched.

#### Scenario: Zero means read and matched nothing

- **WHEN** a build issues a family and the query matches no series
- **THEN** that family's tally entry is zero, meaning "read, matched nothing"
- **AND** a family the build did not issue carries no tally key at all

#### Scenario: Storage build tallies a by-reference family by what it matched

- **WHEN** a `/v1/storage-graph` build loads pods owned by two StatefulSets and no other controller kind
- **THEN** the build's per-family tally carries `kube_statefulset_annotations` with the count of series its `statefulset` restriction matched and carries no key at all for `kube_replicaset_owner`, `kube_job_owner`, `kube_deployment_annotations`, `kube_daemonset_annotations`, `kube_replicaset_annotations`, `kube_job_annotations` or `kube_cronjob_annotations`

### Requirement: Optional topology families degrade when absent

When an OPTIONAL topology family is absent, or no series of it matches a given entity, the reader SHALL still build a valid topology, SHALL NOT fail the build, and SHALL omit only what that family would have supplied.

#### Scenario: Service and endpointslice families absent

- **WHEN** kube-state-metrics does not export services or endpointslices, so the three service/endpointslice families are absent
- **THEN** the service/endpoint indexes are simply empty and connection-string resolution in the pod-service-graph reader degrades gracefully: `"://"` service endpoints that cannot be resolved against an empty index become `external/<label>` nodes

#### Scenario: Indexes under a selector-level filter

- **WHEN** a build runs under a selector-level filter
- **THEN** the service/endpoint indexes hold only the in-scope services and the in-scope backing pods

#### Scenario: PVC info unmatched for one claim

- **WHEN** `kube_persistentvolumeclaim_info` is present but no series matches a given PVC
- **THEN** that PVC carries no `storageclass` attribute and no `volumename` label (and hence no `svm` label and no `pvc-to-netapp-aggr` edge — see the `netapp-storage-graph` capability)
- **AND** the Cytoscape serialiser nests that PVC under its namespace group (`cluster > namespace > pvc`) like any other PVC

#### Scenario: Controller annotation unmatched or empty

- **WHEN** no controller-annotation series matches a given pod's resolved controller owner, or the matched series' `annotation_argocd_argoproj_io_tracking_id` label is empty
- **THEN** that pod carries no `application` attribute and nests directly under its `controller` group (or its namespace group when it also has no controller owner), and the build does not fail

#### Scenario: Service and endpointslice metrics absent

- **WHEN** the upstream contains `kube_pod_info` and `kube_node_info` but no `kube_service_info`, `kube_endpointslice_endpoints`, or `kube_endpointslice_labels` series for the window
- **THEN** the reader produces a valid topology with empty service/endpoint indexes, the build does not fail, and any `"://"` connection-string endpoint that would otherwise resolve to an in-cluster service falls back to an `external/<label>` node with empty `labels`

#### Scenario: PVC info metric absent

- **WHEN** the upstream contains `kube_pod_spec_volumes_persistentvolumeclaims_info` but no `kube_persistentvolumeclaim_info` series for the window
- **THEN** the reader produces a valid topology in which every PVC entity has no `storageclass` attribute, carries no `volumename` or `svm` label, emits no `pvc-to-netapp-aggr` edge, the build does not fail, and the serialiser nests every PVC under its namespace group

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

### Requirement: Query errors degrade only the high-cardinality topology legs

A query error SHALL be handled separately from an empty vector. Outside the `/v1/storage-graph` fail-closed rule, a query error on `kube_replicaset_annotations`, `kube_job_annotations` or `kube_pod_container_info` SHALL be logged and that family treated as an empty vector, while every other kube-state-metrics leg SHALL fail the build. Caller-originated cancellation SHALL still fail the request.

#### Scenario: Accumulating-cardinality legs degrade

- **WHEN** an estate retains ReplicaSets under a Deployment's `revisionHistoryLimit` and Jobs under a CronJob's history limits
- **THEN** `kube_replicaset_annotations` (one series per retained ReplicaSet) and `kube_job_annotations` (one per retained Job) accumulate cardinality with history rather than tracking the live object count, so they can exceed an upstream series or sample limit in an estate whose live object count is unremarkable
- **AND** they degrade rather than fail, because losing an `application` string is never worth failing the whole graph

#### Scenario: Multiplying-cardinality leg degrades

- **WHEN** `kube_pod_container_info` is read over the whole window
- **THEN** its cardinality multiplies with the live object count — one series per container per image variant, and one per pod that existed at any instant of the window, so pod churn inflates it further — making it the largest kube-state-metrics family and the first to meet a memory-derived upstream series limit
- **AND** it degrades rather than fails, because the `containers` attribute it feeds is a presentation attribute whose loss is subtractive

#### Scenario: Degrading leg reports through the build without what it supplied

- **WHEN** one of the three degrading legs fails upstream
- **THEN** the failure is logged, the family is treated as an empty vector, and the build completes without what that family would have supplied
- **AND** caller-originated cancellation — a build timeout or a disconnected client — still fails the request on these three legs, since the caller is no longer waiting for any result

#### Scenario: Accumulating-cardinality family degrades on a query error

- **WHEN** the `kube_replicaset_annotations` or `kube_job_annotations` query fails upstream — a timeout, a 5xx, or a series/sample limit exceeded — while the caller's own deadline has not expired
- **THEN** the failure is logged, that family is treated as an empty vector, every other family still resolves, pods owned by a bare ReplicaSet (or a Job) carry no `application` attribute, and the request returns a valid `200` graph

#### Scenario: A required annotation family still fails the build

- **WHEN** the `kube_deployment_annotations`, `kube_statefulset_annotations`, `kube_daemonset_annotations`, `kube_cronjob_annotations`, or `kube_job_owner` query fails upstream
- **THEN** the build fails and the request returns an error, as it does for every other kube-state-metrics topology leg

#### Scenario: Caller cancellation still fails a degrading family

- **WHEN** the `kube_job_annotations` query fails because the caller's own context is already done — the build timeout elapsed or the client disconnected
- **THEN** the request fails rather than degrading, since the failure is the caller's deadline rather than an upstream fault

#### Scenario: Container info query error degrades

- **WHEN** the `kube_pod_container_info` query fails with an upstream error (for example a series-limit rejection) while every other family succeeds
- **THEN** the build completes, no pod entity carries a `containers` attribute, `kube_state_graph_upstream_query_failures_total{query="kube_pod_container_info"}` increments by 1, and every other attribute, node and edge is unchanged

#### Scenario: Caller cancellation still fails the container leg

- **WHEN** the caller's context is already cancelled when the `kube_pod_container_info` query returns its error
- **THEN** the build returns an error rather than degrading

### Requirement: Degraded Job annotation family suppresses the CronJob hop

A degrade SHALL be **subtractive**: it removes Applications the failed family would have supplied and never substitutes a different one. When the `kube_job_annotations` query fails, the reader SHALL suppress the Job → CronJob hop for that build rather than let every Job miss fall through to its owning CronJob's Application.

#### Scenario: Why the hop needs the Job family

- **WHEN** the `kube_job_annotations` family was not read because its query failed
- **THEN** the reader cannot establish that a Job carries no annotation of its own, the fact the hop is taken on, so the hop is suppressed
- **AND** a Job that genuinely carries no annotation of its own therefore also resolves no Application while that leg is degraded

#### Scenario: Bare ReplicaSet needs no gate

- **WHEN** the `kube_replicaset_annotations` query fails
- **THEN** no equivalent gate applies, because a bare ReplicaSet has no further ancestor to consult and its miss resolves nothing either way

#### Scenario: A degraded Job annotation family suppresses the CronJob hop

- **WHEN** the `kube_job_annotations` query fails upstream while `kube_job_owner` and `kube_cronjob_annotations` both resolve, and a pod is owned by a Job that carries its own tracking-id under a CronJob that carries a different one
- **THEN** the pod carries no `application` attribute — the hop is suppressed for the build, so the pod is never attributed to the CronJob's Application — while the same fixture with the Job family read but genuinely empty still resolves the CronJob's Application through the hop

### Requirement: Service and endpoint indexes

When the optional `kube_service_info`, `kube_endpointslice_endpoints`, and `kube_endpointslice_labels` families are present, the topology reader SHALL build two lookup INDEXES — ServicesByNameNS and EndpointsByService — that the pod-service-graph reader consults to resolve `"://"` connection-string endpoints. It SHALL NOT emit `service` nodes or `service-selects-pod` edges wholesale.

#### Scenario: Services are materialised on demand

- **WHEN** the indexes are built
- **THEN** `service` nodes and `service-selects-pod` edges are materialised ON DEMAND by the pod-service-graph reader, for referenced services only, to avoid graph bloat

#### Scenario: ServicesByNameNS index

- **WHEN** `kube_service_info` series are present
- **THEN** **ServicesByNameNS** is keyed by `(cluster, namespace, service)` and maps to the service facts from `kube_service_info`, including `cluster_ip`
- **AND** `cluster_ip` sets the service node's `ipaddress`, which is omitted when `cluster_ip="None"` for headless services

#### Scenario: EndpointsByService index

- **WHEN** endpointslice series are present
- **THEN** **EndpointsByService** is keyed by `(cluster, namespace, service)` and maps to the list of backing pods, the source of the Service → backing-pod fan-out
- **AND** each slice is joined back to its owning service via the `label_kubernetes_io_service_name` label on `kube_endpointslice_labels`, joined to `kube_endpointslice_endpoints` by `(cluster, namespace, endpointslice)`
- **AND** each endpoint is resolved to a topology pod by joining `(namespace, targetref_name)` against `kube_pod_info`, matching the pod by name within the namespace to recover its UID
- **AND** the per-endpoint `hostname` label is NOT consumed — there is no per-pod headless resolution

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
- **THEN** the reader emits both pod-info entries for the window (the prior and the current UID); see the "Pod restart handling within window" requirement

### Requirement: Cluster-scoped IDs

The reader SHALL produce topology entities whose stable identifiers are cluster-scoped, where `<cluster>` is the cluster **identity** of "Cluster identity composed from zone and environment labels" — `<az>-<env>-<cluster>` when the series composed one, the raw `cluster` label otherwise.

#### Scenario: Identifier shapes

- **WHEN** the reader emits a topology entity
- **THEN** a pod ID is `<cluster>/<pod-uid>` (the cluster identity and the `uid` label) and a K8s node ID is `<cluster>/<node>` (the cluster identity and the `node` label)
- **AND** a PVC ID is `<cluster>/<namespace>/<claim_name>`
- **AND** no StorageClass entity and no StorageClass ID exist: a claim's StorageClass name is the PVC's own `storageclass` attribute (from the `storageclass` label of `kube_persistentvolumeclaim_info`), scoped by the PVC's cluster-scoped ID

#### Scenario: Two clusters with same node name

- **WHEN** `kube_node_info{cluster="cluster-alpha", node="worker-0"}` and `kube_node_info{cluster="cluster-beta", node="worker-0"}` both exist in the window
- **THEN** the reader emits two distinct K8s node entities with IDs `cluster-alpha/worker-0` and `cluster-beta/worker-0`

#### Scenario: Two clusters with the same raw name in different zones

- **WHEN** `kube_node_info{cluster="c1", node="worker-0", az="us", env="dev"}` and `kube_node_info{cluster="c1", node="worker-0", az="eu", env="prod"}` both exist in the window
- **THEN** the reader emits two distinct K8s node entities with IDs `us-dev-c1/worker-0` and `eu-prod-c1/worker-0`

#### Scenario: Pod ID derives from uid label

- **WHEN** `kube_pod_info{cluster="cluster-alpha", uid="abc-123", ...}` is present
- **THEN** the reader emits a pod entity with ID `cluster-alpha/abc-123`

#### Scenario: Pod ID carries the composed identity

- **WHEN** `kube_pod_info{cluster="c1", uid="abc-123", az="us", env="dev", ...}` is present
- **THEN** the reader emits a pod entity with ID `us-dev-c1/abc-123` and `labels.cluster: "us-dev-c1"`

#### Scenario: Two clusters with same StorageClass name

- **WHEN** claims in `cluster-alpha` and `cluster-beta` both carry `storageclass="gp3"`, and `kube_storageclass_info{cluster="cluster-alpha", storageclass="gp3"}` / `kube_storageclass_info{cluster="cluster-beta", storageclass="gp3"}` exist upstream
- **THEN** the reader emits no StorageClass entity (`kube_storageclass_info` is not read); each PVC carries `storageclass: "gp3"` on its own ID, `cluster-alpha/<namespace>/<claim>` and `cluster-beta/<namespace>/<claim>`, so the two never merge

### Requirement: Canonical entity fields

Every emitted topology entity SHALL carry the canonical fields consumed by the graph API: `id`, `name`, `type`, `labels`, and `ipaddress` (for pods and K8s nodes). The reader SHALL derive them per entity kind as this requirement's scenarios state.

#### Scenario: Pod field sources

- **WHEN** the reader emits a pod entity
- **THEN** `name` = the `pod` label of `kube_pod_info` and `type` = `"pod"`
- **AND** `labels` includes `cluster`, `namespace`, `node` (the cluster-scoped node ID), and any K8s pod labels available from `kube_pod_labels` for that pod, under their original keys
- **AND** `ipaddress` = `[pod_ip]` from `kube_pod_info.pod_ip` when surfaced, otherwise empty / omitted; the `host_ip` series label is intentionally not surfaced on the pod entity, because the node's IP is exposed only via the K8s node entity
- **AND** when `kube_pod_owner` is available, the pod carries a typed nullable `owner` attribute (`{kind, name}`, serialised as `data.owner`, NOT a label) for its controller owner with the ReplicaSet skipped to the owning Deployment (see "Pod controller-owner attribute with ReplicaSet skip"), and `owner` is omitted entirely when the pod has no controller owner

#### Scenario: Same-UID pod samples are merged

- **WHEN** kube-state-metrics emits multiple `kube_pod_info` series for the same pod-UID with evolving label sets (e.g. earlier scrapes that lack `node` or `pod_ip`)
- **THEN** the reader merges labels across the same-UID samples and picks the newest non-empty `pod_ip`, so the emitted entity reflects the most informative observation

#### Scenario: K8s node field sources

- **WHEN** the reader emits a K8s node entity
- **THEN** `name` = the `node` label of `kube_node_info` and `type` = `"node"`
- **AND** `labels` includes `cluster` and any node labels from `kube_node_labels` for that node, the `label_*=` series translating to entries under their original key with the `label_` prefix removed
- **AND** `ipaddress` = `[external_ip]` from `kube_node_status_addresses{type="ExternalIP"}` when surfaced, falling back to `[internal_ip]` from `kube_node_status_addresses{type="InternalIP"}` when the node has no ExternalIP row, and is omitted only when neither address type is surfaced
- **AND** IPs are NOT carried inside `labels`

#### Scenario: K8s node address pick is deterministic

- **WHEN** a node has several `kube_node_status_addresses` rows
- **THEN** an ExternalIP row always wins over an InternalIP row regardless of upstream vector order
- **AND** within each address type, duplicate `(cluster, node)` samples resolve to the lexically-smallest address, so the emitted IP is a pure function of the data
- **AND** address types other than `ExternalIP` / `InternalIP` are ignored

#### Scenario: PVC field sources

- **WHEN** the reader emits a PVC entity
- **THEN** `name` = the `claim_name` label of `kube_pod_spec_volumes_persistentvolumeclaims_info` and `type` = `"pvc"`
- **AND** `labels` includes `cluster`, `namespace`, and `volume`
- **AND** `ipaddress` is not emitted

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

The topology reader SHALL resolve each pod's **controller owner** — the `kube_pod_owner` owner whose `owner_is_controller="true"` — and surface it as a typed, nullable `owner` attribute (`{kind, name}`), serialised as `data.owner` (`omitempty`) and **never inside `labels`**. An owner of `kind="ReplicaSet"` SHALL be resolved one level up via `kube_replicaset_owner`.

#### Scenario: Multiple controller owners pick deterministically

- **WHEN** multiple controller owners are reported for a single `(cluster, namespace, pod)`
- **THEN** the reader picks deterministically by the lexical order of `(kind, name)`, so the emitted entity is stable across rebuilds

#### Scenario: ReplicaSet skip

- **WHEN** the selected controller owner has `kind="ReplicaSet"`
- **THEN** the reader transparently skips the ReplicaSet and looks up `kube_replicaset_owner` keyed by `(cluster, namespace, replicaset=owner_name)`
- **AND** if a series with `owner_kind="Deployment"` exists for that ReplicaSet, the emitted `owner` is `{kind:"Deployment", name:<deployment>}`
- **AND** if no `kube_replicaset_owner` series exists for it (a bare ReplicaSet with no owning Deployment), the emitted `owner` remains `{kind:"ReplicaSet", name:<replicaset>}`

#### Scenario: Other owner kinds are surfaced verbatim

- **WHEN** the controller owner is of any other kind (`DaemonSet`, `StatefulSet`, `Job`, `Node` for static pods reported as a controller, etc.)
- **THEN** it is surfaced verbatim with no further resolution

#### Scenario: Missing owner emits nothing

- **WHEN** a pod has no controller owner (`kube_pod_owner` absent for the pod, or no series with `owner_is_controller="true"`)
- **THEN** the reader emits a nil `owner` so `data.owner` is omitted entirely, and emits no empty object, no empty-string fields, and no owner key in `labels`
- **AND** `kube_pod_owner` and `kube_replicaset_owner` are OPTIONAL: when absent the reader builds a valid topology with no `owner` on any pod and does not fail the build

#### Scenario: Owner adds no node or edge type

- **WHEN** a pod carries an `owner`
- **THEN** no new node or edge type is introduced — the owner is a typed attribute on the existing `type="pod"` node (the same precedent as the `ipaddress` attribute), keeping `labels` a strict `map[string]string` of typological metadata

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

When `last_over_time(kube_pod_info[...])` returns multiple `uid` values for the same `(cluster, namespace, pod)` tuple within the requested window, the reader SHALL retain ONLY the entity with the latest evaluation timestamp as the canonical pod and SHALL discard prior UIDs.

#### Scenario: No identity link is reconstructed

- **WHEN** a pod was deleted and recreated mid-window
- **THEN** the API does not attempt to reconstruct an identity link between the deleted pod and its replacement, because none is reliable once kubelet stops reporting the deleted UID

#### Scenario: Pod replaced mid-window collapses to latest UID

- **WHEN** the window includes a pod restart producing two distinct UIDs for the same `(cluster, namespace, pod)` tuple
- **THEN** the resulting topology contains exactly one pod entity, identified by the newest UID; the prior UID does not appear as a node and no synthetic edge is emitted

### Requirement: Series missing the cluster label

A topology series missing the `cluster` label SHALL be bucketed under the raw name `unknown`, which then resolves through the identity ladder like any other raw name. A request-scoped `cluster` filter whose value is `unknown` SHALL be rendered as the anchored alternation `cluster=~"unknown|"`, carrying both spellings of the bucket.

#### Scenario: Unknown bucket resolves through the identity ladder

- **WHEN** a series without a `cluster` label is bucketed
- **THEN** a series carrying both zone and environment labels composes to `<az>-<env>-unknown`, and one carrying neither resolves to the verbatim `unknown`

#### Scenario: Unknown bucket is reported on the observed-clusters gauge

- **WHEN** the build holds series without a `cluster` label
- **THEN** the reader surfaces their count via the `kube_state_graph_clusters_observed` gauge, and an identity with the raw component `unknown` appears in the gauge's label set

#### Scenario: Unknown filter always renders the alternation

- **WHEN** a request's `cluster` filter includes `unknown`, even as its only value
- **THEN** it renders `cluster=~"unknown|"` — the literal `unknown` and the empty string — because a series whose `cluster` label is literally `unknown` and one carrying no `cluster` label are indistinguishable after bucketing and both belong to the bucket, and PromQL's empty alternative matches a series that carries no such label
- **AND** any other value renders as the literal value
- **AND** at projection the same filter matches every identity whose raw component is `unknown`

#### Scenario: Legacy series without cluster label

- **WHEN** a `kube_pod_info` series has no `cluster` label
- **THEN** the resulting pod entity has `cluster: "unknown"` and contributes to the `unknown` value in the observed-clusters set

#### Scenario: Cluster-less series with zone and environment composes

- **WHEN** a `kube_pod_info` series has no `cluster` label but carries `az="us"` and `env="dev"`
- **THEN** the resulting pod entity has id `us-dev-unknown/<uid>` and `cluster: "us-dev-unknown"`, and a request with `?cluster=unknown` still loads it upstream and admits it at projection

#### Scenario: Filtering on the unknown bucket

- **WHEN** a build runs with the cluster filter `unknown` against an upstream where some `kube_pod_info` series carry no `cluster` label, one carries `cluster="unknown"`, and others carry `cluster="cluster-alpha"`
- **THEN** the query is issued with `cluster=~"unknown|"`, the unlabelled series AND the literally-`unknown` series are loaded, `cluster-alpha` is not, and the resulting pod entities all have `cluster: "unknown"`

### Requirement: Per-call upstream timeout

Each topology query SHALL be issued with a per-call context timeout (default 10 seconds, configurable). On timeout or non-2xx response, the reader SHALL increment `kube_state_graph_upstream_query_failures_total{query=<name>}` and propagate the error so the build aborts.

#### Scenario: Single query times out

- **WHEN** centralised VictoriaMetrics fails to respond to the `kube_node_labels` query within the per-call timeout
- **THEN** the failure counter for `query="kube_node_labels"` increments by 1 and the build returns an error

### Requirement: PVC StorageClass resolution

The topology reader SHALL resolve each PVC's StorageClass **name** from `kube_persistentvolumeclaim_info`, joined on `(cluster, namespace, persistentvolumeclaim)` to the PVC entity, and surface it as the PVC's typed `storageclass` attribute (`data.storageclass`, `omitempty`). It SHALL NOT be added to the PVC `labels` map, SHALL NOT drive any edge, and SHALL NOT materialise any node.

#### Scenario: StorageClass join and attribute

- **WHEN** a PVC entity derived from `kube_pod_spec_volumes_persistentvolumeclaims_info` (claim name from the `claim_name` label) matches a `kube_persistentvolumeclaim_info` series
- **THEN** the resolved name is the PVC's `storageclass` attribute as defined by graph-api "PVC `storageclass` and `usage` attributes"
- **AND** the StorageClass entity and the `pvc-to-storageclass` edge are removed

#### Scenario: StorageClass name discriminates join coverage

- **WHEN** an operator reads the `netapp-storage-graph` join-coverage signal
- **THEN** the retained StorageClass name distinguishes "this claim was never meant to have a NetApp backend" from "this claim should have joined and did not"

#### Scenario: StorageClass family optional and deterministic

- **WHEN** `kube_persistentvolumeclaim_info` is absent, or no series matches a given `(cluster, namespace, claim)`
- **THEN** that PVC's StorageClass name is empty (the attribute absent) and the build does not fail
- **AND** when more than one StorageClass value is reported for a single `(cluster, namespace, claim)`, the reader picks the lexically smallest StorageClass name, so the emitted attribute is byte-stable across rebuilds

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

The server SHALL support optional HTTP Basic Auth credentials for each upstream Prometheus-compatible endpoint, sourced **exclusively** from environment variables; no CLI flag SHALL exist for any credential value. Credential values SHALL NOT appear in any log line, trace span attribute, metric label, error message, or HTTP response body.

#### Scenario: Credentials are env-only by design

- **WHEN** an operator configures upstream credentials
- **THEN** only environment variables are accepted, because credential-carrying flags leak through process listings and container specs — a deliberate exception to the env+flag dual-track configuration convention

#### Scenario: Global pair scope

- **WHEN** `KSG_PROM_USERNAME` and `KSG_PROM_PASSWORD`, the **global** pair, are both set (non-empty)
- **THEN** every outbound HTTP request to an upstream that declares no credentials of its own — topology queries, the service-graph queries, the Harvest queries, and the `/readyz` `up` probe — carries an `Authorization: Basic` header for those credentials
- **AND** when both are unset, such requests carry no `Authorization` header and behaviour is unchanged from an unauthenticated deployment

#### Scenario: Per-backend pair named by environment variables

- **WHEN** a routed deployment gives an individual backend its own pair by naming the environment variables holding it, as the `upstream-backend-routing` capability's per-backend credential requirement specifies
- **THEN** that pair takes precedence over the global one for requests to that backend

#### Scenario: Half-configured global pair fails without echoing values

- **WHEN** exactly one of the two global variables is set (non-empty)
- **THEN** server startup fails with a validation error that names both environment variables but does NOT echo either value

#### Scenario: Credential rotation requires a restart

- **WHEN** a credential **value** changes
- **THEN** it takes effect only after a process restart — there is no hot reload for upstream credential values

#### Scenario: Credentials applied to all upstream queries

- **WHEN** the server starts with `KSG_PROM_USERNAME=ksg` and `KSG_PROM_PASSWORD=s3cret` and serves a `/v1/graph` request
- **THEN** every upstream HTTP request issued for the build (topology fan-out, service-graph, Harvest, and any readiness query) carries `Authorization: Basic` for `ksg:s3cret`

#### Scenario: Per-backend pair overrides the global pair

- **WHEN** the global pair is set and one backend declares its own credential variables
- **THEN** requests to that backend carry the backend's credentials and requests to every other backend carry the global pair

#### Scenario: No credentials configured

- **WHEN** the server starts with neither `KSG_PROM_USERNAME` nor `KSG_PROM_PASSWORD` set and no backend declares its own
- **THEN** upstream requests carry no `Authorization` header and startup validation passes

#### Scenario: Half-configured credentials rejected at startup

- **WHEN** the server starts with `KSG_PROM_USERNAME=ksg` and no `KSG_PROM_PASSWORD` (or vice versa)
- **THEN** `config.Validate` returns an error naming `KSG_PROM_USERNAME` and `KSG_PROM_PASSWORD`, the error does not contain the configured value, and the process exits non-zero before binding the listener

#### Scenario: No CLI flag exists for credentials

- **WHEN** the server is started with `--prom-username=x` or `--prom-password=x`
- **THEN** flag parsing fails with an unknown-flag error, because credentials are env-only

#### Scenario: Credentials never logged

- **WHEN** the server runs with credentials configured at any log level, including `debug`, and upstream queries succeed or fail
- **THEN** no emitted log line, span attribute, or error string contains any configured username or password

### Requirement: Pod container list attribute

The topology reader SHALL resolve each pod's **container list** from `kube_pod_container_info`, queried as `tlast_over_time(kube_pod_container_info[w])` so each series' value is its last-sample timestamp, and surface it as a typed, nullable `containers` attribute — an ordered list of `{name, image}` objects — serialised as `data.containers` (`omitempty`) and **never inside `labels`**.

#### Scenario: Container element fields

- **WHEN** a `kube_pod_container_info` series matches a pod by `(cluster, namespace, pod)`
- **THEN** the reader emits one list element with `name` taken from the `container` label and `image` taken from the `image` label
- **AND** the list is ordered deterministically by `(name, image)`, so the emitted entity is byte-identical across rebuilds

#### Scenario: Empty image never masks a populated sibling

- **WHEN** a series' `image` label is empty
- **THEN** the reader skips it, since it carries no information and must not mask a populated sibling

#### Scenario: Latest image wins on a mid-window change

- **WHEN** a single container reports more than one non-empty `image` in the window — a mid-window image change, where each image is a DISTINCT series
- **THEN** the reader picks the image with the **greatest last-sample timestamp** (the current image), breaking exact-timestamp ties by the lexically-smallest `image`

#### Scenario: Latest-image pick is reliable only near the wall clock

- **WHEN** the query window lies far in the past
- **THEN** VictoriaMetrics returns only one image-variant series per container regardless of rollup, and the reader surfaces whatever single variant VM returns — never worse than a fixed deterministic pick
- **AND** for windows near the real wall clock (the dominant case) the latest-image pick is reliable

#### Scenario: No container info emits no list

- **WHEN** `kube_pod_container_info` is absent or no series matches a given pod
- **THEN** the reader emits a nil `containers` so `data.containers` is omitted entirely, and emits no empty array and no container key in `labels`
- **AND** no new node or edge type is introduced — the container list is a typed attribute on the existing `type="pod"` node (the same precedent as the `ipaddress` and `owner` attributes), keeping `labels` a strict `map[string]string` of typological metadata

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

The topology reader SHALL resolve each pod's **ArgoCD Application** from the `annotation_argocd_argoproj_io_tracking_id` label of its **controller**'s annotation series and surface it as a typed, nullable `application` attribute (a string), serialised as `data.application` (`omitempty`) and **never inside `labels`**. It SHALL NOT read any `argocd_tracking_id` label from `kube_pod_owner`.

#### Scenario: The controller is the only annotation source

- **WHEN** ArgoCD applies a workload
- **THEN** it stamps `argocd.argoproj.io/tracking-id` on the workload objects it applies, never on the pods a controller spawns, so the controller is the only place the annotation exists

#### Scenario: Controller lookup key

- **WHEN** the reader resolves a pod's Application through its controller
- **THEN** the lookup key is the pod's own resolved controller owner from "Pod controller-owner attribute with ReplicaSet skip" — `(cluster, namespace, owner_kind, owner_name)` — so a pod owned through a ReplicaSet is looked up against its already-resolved **Deployment** and no additional owner hop is needed for that case
- **AND** a pod with no controller owner resolves no Application

#### Scenario: Controller-annotation index

- **WHEN** the reader builds the controller-annotation index
- **THEN** it builds one index keyed `(cluster, namespace, kind, name)` from the six controller-annotation families, each contributing its own owner kind and identity label
- **AND** owner kind `Deployment` reads `kube_deployment_annotations` by `deployment`, `StatefulSet` reads `kube_statefulset_annotations` by `statefulset`, and `DaemonSet` reads `kube_daemonset_annotations` by `daemonset`
- **AND** owner kind `ReplicaSet` reads `kube_replicaset_annotations` by `replicaset`, `Job` reads `kube_job_annotations` by `job_name`, and `CronJob` reads `kube_cronjob_annotations` by `cronjob`
- **AND** a series whose identity label is empty is skipped, and a series missing the `cluster` label is bucketed under `cluster="unknown"` (the same rule as every other topology series)

#### Scenario: Unconfigured allowlist and missing label emit no Application

- **WHEN** the annotation allowlist is not configured (the common state under stock kube-state-metrics), so the six controller-annotation families and `kube_job_owner` return empty vectors
- **THEN** no pod resolves an Application from the controller chain and the build does not fail
- **AND** when the label is absent, empty, or unmatched for a pod, the reader emits a nil `application` so `data.application` is omitted entirely, never an empty string or any application key in `labels`

#### Scenario: Application adds no node or edge type

- **WHEN** a pod carries an `application`
- **THEN** no new node or edge type is introduced — the Application is a typed attribute on the existing `type="pod"` node (the same precedent as the `owner` attribute), keeping `labels` a strict `map[string]string` of typological metadata

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

#### Scenario: Pod with no ArgoCD label

- **WHEN** a pod's resolved owner is `{kind:"Deployment", name:"checkout"}` and the matching `kube_deployment_annotations` series carries an empty `annotation_argocd_argoproj_io_tracking_id` (or no series matches)
- **THEN** the emitted pod entity has a nil `application` (`data.application` omitted entirely) and carries no application key in `labels`

#### Scenario: Pod with no controller owner

- **WHEN** no `kube_pod_owner` series with `owner_is_controller="true"` exists for a pod and the application-metadata metric names no Application for it
- **THEN** the emitted pod entity has a nil `application` and no controller-annotation lookup is attempted

#### Scenario: ArgoCD label absent entirely

- **WHEN** the upstream contains `kube_pod_owner` series but none of the six controller-annotation families for the window
- **THEN** the reader produces a valid topology with no `application` on any pod and does not fail the build

### Requirement: Application-metadata metric takes precedence for the pod Application

When the `app-metadata-source` capability yields a non-empty Application for a pod's `(cluster, namespace, pod)`, that value SHALL be the pod's Application and the controller chain SHALL NOT be consulted for it. Every other pod SHALL resolve through the controller chain, and the controller-chain queries SHALL still be issued when the metric is configured.

#### Scenario: Unusable metric falls back to the chain for every pod

- **WHEN** the application-metadata metric is unset, unserved, or its `/v1/graph` query failed
- **THEN** every pod resolves its Application through the controller chain exactly as "Pod ArgoCD Application attribute" specifies

#### Scenario: Metric Application wins over the controller annotation

- **WHEN** the application-metadata metric names Application `shop-v2` for pod `checkout-1` and the pod's Deployment carries tracking-id `storefront:apps/Deployment:shop/checkout`
- **THEN** the emitted pod entity has `application="shop-v2"` and its `owner` is still `{kind:"Deployment", name:"checkout"}`

#### Scenario: Pod without a metric row falls back to its controller

- **WHEN** the application-metadata metric is configured but carries no row for pod `checkout-2`, whose Deployment carries tracking-id `storefront:apps/Deployment:shop/checkout`
- **THEN** the emitted pod entity has `application="storefront"`

#### Scenario: Metric resolves a pod the controller chain cannot

- **WHEN** a pod's resolved owner is `{kind:"Rollout", name:"canary"}` and the application-metadata metric names Application `canary-app` for it
- **THEN** the emitted pod entity has `application="canary-app"`

#### Scenario: PVC inheritance follows the metric value

- **WHEN** PVC `data-1` carries no Application of its own and is mounted only by a pod whose Application the metric resolves to `shop-v2`
- **THEN** the PVC inherits `application="shop-v2"`

### Requirement: Job to CronJob hop for the pod Application

When a pod's resolved owner kind is `Job` and the controller-annotation index holds no entry for that Job, the reader SHALL resolve the Job's owning CronJob via `kube_job_owner` and look the index up again for that CronJob. The hop SHALL be **resolution-only**: it SHALL NOT change the pod's `owner` attribute or its `controller` compound group.

#### Scenario: Hop lookup keys

- **WHEN** the hop is taken
- **THEN** the reader reads `kube_job_owner` keyed `(cluster, namespace, job_name=<owner_name>)`, selecting the series with `owner_is_controller="true"` and `owner_kind="CronJob"`, and looks the index up again at `(cluster, namespace, "CronJob", <owner_name of the job_owner series>)`
- **AND** the pod's `owner` stays `{kind:"Job", name:<job>}`

#### Scenario: Why the hop exists

- **WHEN** the Kubernetes CronJob controller creates a Job
- **THEN** it propagates only `spec.jobTemplate.metadata` annotations onto the Job — never the CronJob object's own annotations — so ArgoCD's tracking-id never reaches the Job and the hop recovers it from the CronJob
- **AND** a directly ArgoCD-managed Job is resolved by its own annotation before the hop is attempted

#### Scenario: Colliding CronJob owners

- **WHEN** more than one controller CronJob owner is reported for a Job (a defensive collision)
- **THEN** the lexically-smallest `owner_name` wins

#### Scenario: Pod owned by a CronJob-created Job

- **WHEN** a pod's resolved owner is `{kind:"Job", name:"nightly-28901"}`, no `kube_job_annotations` series matches that Job, `kube_job_owner{cluster="cluster-alpha", namespace="batch", job_name="nightly-28901", owner_kind="CronJob", owner_name="nightly", owner_is_controller="true"}` is present, and `kube_cronjob_annotations{cluster="cluster-alpha", namespace="batch", cronjob="nightly", annotation_argocd_argoproj_io_tracking_id="reports:batch/CronJob:batch/nightly"}` is present
- **THEN** the emitted pod entity has `application="reports"` and its `owner` attribute remains `{kind:"Job", name:"nightly-28901"}` — the hop does not rewrite the owner

#### Scenario: Directly managed Job resolves before the hop

- **WHEN** a pod's resolved owner is `{kind:"Job", name:"migrate-1"}` and `kube_job_annotations{cluster="cluster-alpha", namespace="shop", job_name="migrate-1", annotation_argocd_argoproj_io_tracking_id="migrations:batch/Job:shop/migrate-1"}` is present alongside a `kube_job_owner` series naming a CronJob with a different tracking-id
- **THEN** the emitted pod entity has `application="migrations"` (the Job's own annotation wins; the CronJob hop is not attempted)

### Requirement: Pod Application name parsed from the tracking-id

The pod Application name SHALL be the part of the tracking-id **before the first `:`**; a value with no `:` SHALL be used verbatim, and an empty leading segment SHALL resolve to **no** Application, never an empty string. Among several distinct non-empty tracking-ids for one controller, the reader SHALL pick the **lexically-smallest** raw value whose derived Application is non-empty.

#### Scenario: One grammar for workloads, services and claims

- **WHEN** a tracking-id in ArgoCD's annotation-based form `<app>:<group>/<kind>:<namespace>/<name>` is parsed
- **THEN** the parse is byte-for-byte the one the service and PVC resolvers use, so a workload, its Service and its PVC derive their Application from one grammar
- **AND** an empty leading segment such as `:apps/Deployment:ns/x` resolves the controller to no Application

#### Scenario: Duplicate tracking-ids pick order-free

- **WHEN** more than one distinct non-empty tracking-id is observed for a single `(cluster, namespace, kind, name)`
- **THEN** the lexically-smallest raw value with a non-empty derived Application is picked, so the emitted entity is stable and order-free across rebuilds, mirroring the service and PVC resolvers

#### Scenario: Pod with a bare Application name (no colon)

- **WHEN** a pod's controller annotation value is `checkout` (no `:`)
- **THEN** the emitted pod entity has `application="checkout"`

#### Scenario: Empty leading segment yields no Application

- **WHEN** a pod's controller annotation value is `:apps/Deployment:shop/checkout`
- **THEN** the emitted pod entity has a nil `application` (`data.application` omitted entirely)

#### Scenario: Deterministic pick on duplicate controller annotation series

- **WHEN** two `kube_deployment_annotations` series for the same `(cluster, namespace, deployment)` report tracking-ids `zeta:apps/Deployment:shop/x` and `alpha:apps/Deployment:shop/x`
- **THEN** every pod owned by that Deployment resolves `application="alpha"` (the lexically-smallest raw value) and the choice is identical across rebuilds

### Requirement: Unsupported owner kinds resolve no pod Application

A pod whose resolved controller owner is of a kind with no kube-state-metrics annotation family SHALL resolve no Application from the controller chain. The reader SHALL NOT fail the build, SHALL keep the pod's `owner` attribute, and SHALL NOT emit an empty string.

#### Scenario: Kinds without an annotation family

- **WHEN** a pod's resolved owner is a `ReplicationController` (no `kube_replicationcontroller_annotations` exists), a `Node` (static / mirror pods), or any third-party CRD controller such as argo-rollouts `Rollout` or OpenKruise `CloneSet`
- **THEN** the pod resolves no Application from the controller chain

#### Scenario: Owner kind with no annotation family

- **WHEN** a pod's resolved owner is `{kind:"Rollout", name:"canary"}` or `{kind:"ReplicationController", name:"legacy"}`
- **THEN** the emitted pod entity keeps that `owner`, has a nil `application`, and the build does not fail

### Requirement: K8s node Ready-status attribute

The topology reader SHALL resolve each K8s node's **Ready status** from `last_over_time(kube_node_status_condition{condition="Ready"}[w])` and surface it as a typed, nullable `ready_status` attribute (a string), serialised as `data.ready_status` (`omitempty`) and **never inside `labels`**. Absence SHALL be distinct from `"Unknown"`.

#### Scenario: Ready selector is fixed, not a caller filter

- **WHEN** the reader queries `kube_node_status_condition{condition="Ready"}`
- **THEN** the `condition="Ready"` selector is a fixed, request-invariant metric-selection contract (the same precedent as the node-address `type` selector), applied at the query layer for every build and NOT a caller filter, preserving the "no caller filters pushed to PromQL" contract

#### Scenario: Active Ready series maps to a status

- **WHEN** a `(cluster, node)` has an **active** `condition="Ready"` series — the one whose sample value is `1`
- **THEN** its `status` label maps **case-insensitively** `true` → `"Ready"`, `false` → `"NotReady"`, `unknown` → `"Unknown"`

#### Scenario: Status casing is canonicalised at the read site

- **WHEN** the `status` label arrives in either casing — stock kube-state-metrics lowercases it (`addConditionMetrics` → `strings.ToLower`), while an exporter re-publishing the raw Kubernetes `v1.ConditionStatus` enum verbatim emits `True`/`False`/`Unknown`
- **THEN** the reader canonicalises it to lowercase at the read site, since the casing is NOT pinned by the KSM-shaped contract, so both forms resolve and the guard, tie-break, and mapping all operate on one casing

#### Scenario: Multiple active Ready series pick deterministically

- **WHEN** more than one `condition="Ready"` series is active for the same `(cluster, node)` (a defensive case that does not occur in correct kube-state-metrics output, where exactly one Ready status is active at a time)
- **THEN** the reader picks the lexically-smallest canonicalised `status` label, so the emitted value is deterministic and order-free

#### Scenario: Absence never becomes Unknown

- **WHEN** the metric is absent, a node has no `condition="Ready"` series, or no Ready series is active
- **THEN** the reader emits a nil `ready_status` so `data.ready_status` is omitted entirely, emits no empty string, and does not substitute `"Unknown"` for missing data
- **AND** the literal `"Unknown"` value is reserved for the genuine Kubernetes state in which the Ready condition's `status` label is `unknown` (the node's kubelet has stopped reporting)
- **AND** `kube_node_status_condition` is OPTIONAL: when absent the reader builds a valid topology with no `ready_status` on any node and does not fail the build

#### Scenario: Ready status adds no node or edge type

- **WHEN** a K8s node carries a `ready_status`
- **THEN** no new node or edge type is introduced — the Ready status is a typed attribute on the existing `type="node"` node (the same precedent as the `ipaddress` attribute), keeping `labels` a strict `map[string]string` of typological metadata

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

The topology reader SHALL emit one directed topology relationship edge, `pod-to-node`, in addition to `pod-mounts-pvc`, using deterministic UUIDv5 edge IDs (canonical input `<type>|<source>|<target>`) and de-duplicating by `(type, source, target)` so the emitted set is byte-stable across rebuilds.

#### Scenario: pod-to-node edge shape

- **WHEN** a pod's `labels.node` (the cluster-scoped node ID) is non-empty, i.e. the pod is scheduled
- **THEN** the reader emits one **`pod-to-node`** edge from the pod node ID to that node ID, carrying no `labels`
- **AND** the edge is always intra-cluster (the node is in the pod's own cluster), so `may_cross_cluster` is `false`

#### Scenario: Edges the topology reader does not emit

- **WHEN** the topology reader emits relationship edges
- **THEN** it emits no `pvc-to-storageclass` edge, which is removed
- **AND** the `pvc-to-netapp-aggr` edge is NOT a topology relationship edge — it is derived from the Harvest volume join and defined by the `netapp-storage-graph` capability

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

The topology reader SHALL resolve an ArgoCD Application name for service and PVC entities from the `annotation_argocd_argoproj_io_tracking_id` label of `kube_service_annotations` and `kube_persistentvolumeclaim_annotations`, surface it on the node's typed `application` attribute, and drive its `application` compound-group nesting. It SHALL NOT be added to the entity's `labels` map.

#### Scenario: Annotation join keys

- **WHEN** the reader resolves a service or PVC Application
- **THEN** `kube_service_annotations` is joined on `(cluster, namespace, service)` to the service entity and `kube_persistentvolumeclaim_annotations` on `(cluster, namespace, persistentvolumeclaim)` to the PVC entity, whose claim name derives from the `claim_name` label of `kube_pod_spec_volumes_persistentvolumeclaims_info`
- **AND** a series missing the `cluster` label is bucketed under `cluster="unknown"` (the same rule as every other topology series)
- **AND** the attribute is the one of graph-api "Pod `application` and `containers` attributes", and the nesting the one of graph-api "Cytoscape compound node grouping"

#### Scenario: Same grammar as the pod Application

- **WHEN** a service or PVC tracking-id is parsed
- **THEN** the Application is derived **identically to the pod ArgoCD Application** (graph-api "Pod `application` and `containers` attributes"): the segment **before the first `:`** of the ArgoCD `<app>:<group>/<kind>:<ns>/<name>` form, or the whole value when it has no `:`
- **AND** a value whose leading segment is empty resolves to **no** Application — the entity is absent from the application index, never present-but-empty

#### Scenario: Annotation families optional and deterministic

- **WHEN** `kube_service_annotations` or `kube_persistentvolumeclaim_annotations` is absent, no series matches a given entity, or the matched series has an empty `annotation_argocd_argoproj_io_tracking_id` label
- **THEN** that entity carries no Application name from this path and the build does not fail
- **AND** when more than one non-empty tracking-id is reported for a single entity, the reader picks the lexically smallest raw tracking-id value, mirroring the pod resolver, so the resolved Application is byte-stable across rebuilds

#### Scenario: PVC fallback by inheritance

- **WHEN** this annotation path resolves no Application for a **PVC** (the annotation series is absent, unmatched, empty, or its leading segment is empty)
- **THEN** a fallback MAY still resolve one by inheritance from a mounting pod — see "PVC ArgoCD Application inheritance from mounting pod"
- **AND** that fallback never applies to service entities

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

When a PVC entity has **no** ArgoCD Application resolved from its own annotation, the topology reader SHALL **inherit** the **lexically-smallest non-empty** Application among the pods that mount it via `pod-mounts-pvc` edges. Inheritance is a strictly-ordered fallback: a PVC's own annotation-resolved Application SHALL ALWAYS win and SHALL NEVER be overridden by inheritance.

#### Scenario: Inheritance candidates

- **WHEN** the "Service and PVC ArgoCD Application resolution" path produced no Application for a PVC — the `kube_persistentvolumeclaim_annotations` series is absent, unmatched, has an empty `annotation_argocd_argoproj_io_tracking_id` label, or that label's leading segment is empty
- **THEN** for every `pod-mounts-pvc` edge incident to the PVC (source = a pod entity, target = the PVC entity), the mounting pod's own resolved Application (see "Pod ArgoCD Application attribute") is a candidate
- **AND** a mounting pod with no resolved Application contributes no candidate

#### Scenario: Inherited Application is indistinguishable on the wire

- **WHEN** a PVC inherits an Application
- **THEN** it is baked onto the PVC node's typed `application` attribute (graph-api "Pod `application` and `containers` attributes") and drives the PVC's `application` compound-group nesting (graph-api "Cytoscape compound node grouping") **identically** to an annotation-resolved one, so the wire output is **indistinguishable** from a natively-resolved PVC Application
- **AND** the inherited value is not added to the PVC's `labels` map

#### Scenario: Inheritance never crosses cluster or namespace

- **WHEN** a PVC inherits an Application
- **THEN** the source pod is in the PVC's own cluster and namespace, because `pod-mounts-pvc` is always intra-cluster and a pod mounts a PVC in its own namespace
- **AND** the inherited Application's compound group (`<cluster>/namespace/<ns>/application/<app>`) nests under the PVC's own namespace

#### Scenario: Inheritance is computed once over the full graph

- **WHEN** a request carries a `?cluster=` / `?namespace=` / `?name=` filter that would drop the mounting pod from the response
- **THEN** the PVC's resolved Application is unchanged, because inheritance is resolved at build time over the fully-assembled graph (all pod entities and all `pod-mounts-pvc` edges present), **before** any projection/filter, and never recomputed per request — consistent with "build once, project many"
- **AND** the result depends only on the **set** of mounting pods and their resolved Applications, independent of edge iteration order, so it is byte-stable across rebuilds

#### Scenario: PVC with no mounting pod

- **WHEN** a PVC has no incident `pod-mounts-pvc` edge, or every mounting pod has no resolved Application
- **THEN** the PVC carries no Application, nests under its namespace group, and the build does not fail

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

The topology reader SHALL surface each PVC's bound **PersistentVolume name** as the `volumename` label and, when the NetApp Harvest join resolves, the serving **ONTAP SVM** as the `svm` label — additive entries in the PVC's strict `map[string]string` `labels`. Each key SHALL be set only when its value is non-empty, and `svm` SHALL never be present without `volumename`.

#### Scenario: volumename source

- **WHEN** a PVC entity matches a `kube_persistentvolumeclaim_info` series on `(cluster, namespace, persistentvolumeclaim)` — the same join as PVC StorageClass resolution
- **THEN** `volumename` is read from that series' `volumename` label
- **AND** the `volumename` and `storageclass` reads are per-field independent: a series may carry `volumename` without `storageclass` and vice versa

#### Scenario: svm source

- **WHEN** the `netapp-storage-graph` capability's Harvest join, rooted at the resolved PV name, matches
- **THEN** `svm` is taken from the matched series: the join derives a match token from the PV name and matches it against the **stock** `volume` label of the `volume_labels` series
- **AND** the `svm` label rides on the same matched series that provides the serving aggregate and controller, never on the QoS families that carry the edge's I/O

#### Scenario: Three distinct volume names

- **WHEN** a PVC entity carries both `volume` and `volumename`
- **THEN** `volumename` is DISTINCT from the existing `volume` key (the pod-spec volume name from `kube_pod_spec_volumes_persistentvolumeclaims_info`); both MAY coexist and neither replaces the other
- **AND** neither is the Harvest `volume` label, which names an ONTAP FlexVol and is never surfaced on a PVC entity

#### Scenario: Every link degrades gracefully

- **WHEN** `kube_persistentvolumeclaim_info` is absent, a join finds no match, or a required label is empty
- **THEN** the affected key(s) are simply omitted — the reader still builds a valid topology, does not fail the build, and emits no empty-string label value
- **AND** a PV name that the configured derivation cannot map onto any FlexVol name is one such no-match: the entity keeps `volumename` and loses only `svm`
- **AND** the join only ENRICHES PVC entities that exist via the pod→PVC binding metric; it never materialises a PVC on its own

#### Scenario: Deterministic pick at every stage

- **WHEN** duplicate series are reported at either stage
- **THEN** the reader picks the lexically-smallest non-empty value per stage — `volumename` per `(cluster, namespace, claim)`; `svm` per matched volume among the series on the picked aggregate's own ONTAP cluster, per the `netapp-storage-graph` capability — so the labels are a pure function of the upstream data, independent of vector order
- **AND** the labels are baked at build time before any projection

#### Scenario: Full chain resolves volumename and svm

- **WHEN** the upstream provides a PVC entity `cluster-alpha/db/data-mongo-0` with `kube_persistentvolumeclaim_info{cluster="cluster-alpha", namespace="db", persistentvolumeclaim="data-mongo-0", volumename="pvc-9f3a"}` and a Harvest `volume_labels` series with `volume="trident_pvc_9f3a", svm="svm-prod"`
- **THEN** the emitted PVC entity's `labels` contains `volumename="pvc-9f3a"` and `svm="svm-prod"`

#### Scenario: PV without a TridentVolume row yields volumename only

- **WHEN** a PVC resolves `volumename="pvc-9f3a"` but no Harvest `volume_labels` series has a `volume` label its derived token matches
- **THEN** the emitted PVC entity's `labels` contains `volumename="pvc-9f3a"` and no `svm` key, and the build does not fail

#### Scenario: TridentVolume without a matching backend yields no svm

- **WHEN** the Harvest `volume_labels` series matched by a PVC's derived token carries an empty `svm` label
- **THEN** the emitted PVC entity carries `volumename` but no `svm` key — never an empty-string value — and the build does not fail

#### Scenario: Trident metrics absent entirely

- **WHEN** the upstream contains `kube_persistentvolumeclaim_info` (with `volumename` labels) and no Harvest `volume_labels` series for the window
- **THEN** the reader produces a valid topology in which PVC entities carry `volumename` but no `svm` key, and the build does not fail

#### Scenario: PVC info without volumename yields neither label

- **WHEN** a PVC's `kube_persistentvolumeclaim_info` series carries no (or an empty) `volumename` label, or no info series matches the PVC at all
- **THEN** the emitted PVC entity carries neither a `volumename` nor an `svm` key — no empty-string value is emitted — and the build does not fail

#### Scenario: volumename is independent of storageclass on the same series

- **WHEN** a `kube_persistentvolumeclaim_info` series carries `volumename="pvc-9f3a"` but an empty `storageclass` label
- **THEN** the emitted PVC entity carries `labels.volumename="pvc-9f3a"` while no `storageclass` attribute is emitted for it (and vice versa: a series with `storageclass` but no `volumename` drives the attribute without the label)

#### Scenario: volume and volumename coexist

- **WHEN** a PVC entity derives `volume="data"` from the pod→PVC binding metric and resolves `volumename="pvc-9f3a"` from `kube_persistentvolumeclaim_info`
- **THEN** the emitted PVC entity's `labels` contains both `volume="data"` (the pod-spec volume name) and `volumename="pvc-9f3a"` (the bound PV name) as distinct keys, and neither carries the ONTAP FlexVol name

#### Scenario: Deterministic pick on duplicate series at every stage

- **WHEN** the upstream reports two `kube_persistentvolumeclaim_info` series for `(cluster-alpha, db, data-mongo-0)` with `volumename="pvc-b"` and `volumename="pvc-a"`, and two Harvest `volume_labels` series matched by the `pvc-a` token with `svm="svm-b"` and `svm="svm-a"`
- **THEN** the reader resolves `volumename="pvc-a"` and `svm="svm-a"` (the lexically-smallest non-empty value at each stage) deterministically across rebuilds, independent of upstream vector order

### Requirement: PVC usage from kubelet volume stats

The topology reader SHALL resolve each PVC's storage usage from `kubelet_volume_stats_used_bytes{cluster, namespace, persistentvolumeclaim, ...}` and `kubelet_volume_stats_capacity_bytes{cluster, namespace, persistentvolumeclaim, ...}`, joined on `(cluster, namespace, persistentvolumeclaim)` to the PVC entity, into its `usage` attribute. The values SHALL never appear inside `labels`.

#### Scenario: Kubelet is a further upstream family

- **WHEN** the reader reads the kubelet volume-stats series
- **THEN** **kubelet** is a further upstream metric family alongside kube-state-metrics and NetApp Harvest (whose volume-label, QoS workload, QoS fixed-policy, aggregate, and node objects are read by the `netapp-storage-graph` capability)
- **AND** the label contract is fixed and case-sensitive, and a series missing the `cluster` label is bucketed under `cluster="unknown"` (the same rule as every other topology series)

#### Scenario: Usage fields are per-field independent

- **WHEN** either kubelet series matches a PVC
- **THEN** `used_bytes` resolves from the first series and `capacity_bytes` from the second, and the PVC's `usage` attribute (graph-api "PVC `storageclass` and `usage` attributes") is present iff at least one field resolved, with an unresolved field omitted from the object
- **AND** both series are OPTIONAL: when a metric is absent or no series matches a PVC, the affected field(s) are omitted, the reader still builds a valid topology, and the build does not fail
- **AND** on duplicate series for one `(cluster, namespace, claim)` the reader picks the smallest numeric value, so the attribute is byte-stable across rebuilds

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

### Requirement: Request-scoped upstream selectors

The build SHALL accept four request-scoped selector dimensions — `az`, `env`, `cluster`, `namespace` — each a set of values, and SHALL render them into the upstream PromQL queries as label matchers composed **with** (never replacing) each query's fixed, request-invariant selectors. Which dimensions reach which series SHALL be a hardcoded contract with no configuration surface.

#### Scenario: Fixed selectors the request matchers compose with

- **WHEN** a query is rendered under a request-scoped selector
- **THEN** its fixed selectors are kept: `type=~"ExternalIP|InternalIP"`, `condition="Ready"`, `lun=""`, `owner_kind="CronJob",owner_is_controller="true"` on `kube_job_owner`, `annotation_argocd_argoproj_io_tracking_id!=""` on each of the six controller-annotation families, and the service-graph sentinel and `edge_relation!="link"` matchers

#### Scenario: Zone, environment and cluster reach

- **WHEN** `az`, `env` or `cluster` is non-empty
- **THEN** it renders on every kube-state-metrics and kubelet series and never on a service-graph series
- **AND** `az` and `env` **never** render on a NetApp Harvest series — the `harvest` store is the zone boundary for the `az` it is routed by (see "Backend routing composes with request-scoped selectors"), and a Harvest series is not required to carry either label
- **AND** `cluster` **never** renders on a Harvest series, because Harvest's `cluster` label names the ONTAP cluster, not a Kubernetes cluster

#### Scenario: Namespace reach

- **WHEN** `namespace` is non-empty
- **THEN** it renders on every kube-state-metrics and kubelet series that carries a `namespace` label — the pod-, claim-, Service-, EndpointSlice-, and **controller**-scoped series, the six controller-annotation families and `kube_job_owner` being namespaced like every other workload family
- **AND** it never renders on a node series, a Harvest series, or a service-graph series

#### Scenario: Series that take no request matcher

- **WHEN** any request runs
- **THEN** the three `traces_service_graph_*` series, the `up{}` probe and every NetApp Harvest series carry **no** request-scoped matcher, and the `qos_*` families keep their fixed `lun=""` selector

#### Scenario: Unmatched families are narrowed by reference

- **WHEN** a series family carries no request-scoped matcher for a dimension
- **THEN** it is narrowed by **reference** instead — a node is emitted only when a loaded pod is scheduled on it, an aggregate only when a loaded claim's `volumename` joins to it — as specified by the `graph-api` retention requirements
- **AND** zero rows under a non-empty dimension is a valid, empty topology — not a failure and not a retention miss

#### Scenario: Fixed selector composed with request matchers

- **WHEN** a build runs with `az={zone-a}` and `cluster={cluster-alpha}`
- **THEN** the node-address query is issued as `last_over_time(kube_node_status_addresses{type=~"ExternalIP|InternalIP",az="zone-a",cluster="cluster-alpha"}[<window>])` and the node-condition query keeps `condition="Ready"` ahead of the request matchers

#### Scenario: Controller-annotation families receive all four dimensions

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={cluster-alpha}` and `namespace={shop}`
- **THEN** each of the six controller-annotation queries and the `kube_job_owner` query is issued with exactly `az="zone-a",env="prod",cluster="cluster-alpha",namespace="shop"` added, in that fixed order, following that query's own fixed selector

#### Scenario: Controller-annotation fixed selectors precede the request matchers

- **WHEN** a build runs with `namespace={shop}`
- **THEN** the Deployment annotation query is issued as `last_over_time(kube_deployment_annotations{annotation_argocd_argoproj_io_tracking_id!="",namespace="shop"}[<window>])` and the Job-owner query as `last_over_time(kube_job_owner{owner_kind="CronJob",owner_is_controller="true",namespace="shop"}[<window>])` — the fixed selector first, the request matcher composed after it and never replacing it

#### Scenario: Harvest receives no request-scoped matcher

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** every Harvest query is issued exactly as in an unfiltered build, with no `cluster` or `namespace` matcher

#### Scenario: Harvest receives the zone and environment only

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={cluster-alpha}`, `namespace={shop}`
- **THEN** no Harvest query (`volume_labels`, the `qos_*` families, `qos_policy_fixed_max_throughput_*`, `aggr_*`, the `node_*` families) carries any of the four as a matcher — the `aggr_new_status` query is `last_over_time(aggr_new_status[<window>])`, exactly as in an unfiltered build — and of the four only `az` acts on Harvest directly, by selecting which `harvest` backends are asked; `env`, `cluster` and `namespace` reach it solely through the claims they load

#### Scenario: Service-graph series are never narrowed

- **WHEN** a build runs with any non-empty combination of the four dimensions
- **THEN** the three `traces_service_graph_*` queries are issued exactly as for an unfiltered build (sentinel and link matchers only), and the `up{}` probe query is the bare `up`

### Requirement: Request-scoped matcher rendering

Request-scoped matcher rendering SHALL be a pure function of the sorted, de-duplicated value set: one value renders `<key>="<value>"`, two or more render one fully-anchored alternation `<key>=~"<v1>|<v2>"`, and an empty set renders nothing. A build with every dimension empty SHALL add **no** request-scoped matcher to any query, so each query renders exactly its fixed form.

#### Scenario: Values are escaped for PromQL

- **WHEN** a matcher value is rendered
- **THEN** a single value has `"` and `\` escaped
- **AND** the alternatives of an alternation are regex-quoted and THEN string-escaped: a backslash introduced by regex-quoting is doubled, because a PromQL string literal rejects an unknown escape sequence

#### Scenario: Matchers appear in a fixed order

- **WHEN** a selector carries fixed and request-scoped matchers
- **THEN** the fixed selectors come first, then `az`, `env`, `cluster`, `namespace`

#### Scenario: The unknown cluster is the one non-literal value

- **WHEN** the `cluster` value is `unknown`
- **THEN** it renders `cluster=~"unknown|"` (see "Series missing the cluster label"), the one value that is not rendered as a plain literal

#### Scenario: Multi-value rendering is order-free

- **WHEN** one build runs with `namespace={b, a}` and another with `namespace={a, b, a}`
- **THEN** both issue `namespace=~"a|b"` on every namespace-scoped series

#### Scenario: Regex metacharacters in a value are quoted

- **WHEN** a build runs with `env={prod.eu, prod-us}`
- **THEN** the rendered alternation is `env=~"prod-us|prod\\.eu"` — the metacharacter is regex-quoted AND the resulting backslash is escaped for the PromQL string literal, which rejects an unknown escape sequence — so a series with `env="prodXeu"` does not match

#### Scenario: Empty dimensions render nothing

- **WHEN** a build runs with all four dimensions empty
- **THEN** every issued query string carries only its own fixed selector — byte-identical to the recorded unfiltered rendering, with no request-scoped matcher appended

### Requirement: Configurable `az` / `env` label keys

The upstream label names that the `az` and `env` dimensions bind to SHALL default to `az` and `env` and SHALL be overridable per deployment via `KSG_AZ_LABEL` / `KSG_ENV_LABEL` and `--az-label` / `--env-label`, the flag overriding the environment. The same two keys SHALL name the labels a cluster identity is composed from.

#### Scenario: Flag overrides environment

- **WHEN** both the environment variable and the flag set a key
- **THEN** the flag wins, following the existing precedence

#### Scenario: Keys are validated at startup

- **WHEN** the server starts
- **THEN** each configured key is validated as a PromQL label name (`[a-zA-Z_][a-zA-Z0-9_]*`), and an invalid key fails startup with an error naming the setting
- **AND** the two keys must be distinct

#### Scenario: Request parameters and embedders are unaffected by rebinding

- **WHEN** a deployment rebinds either key
- **THEN** the request parameter names (`az`, `env`) do not change
- **AND** the engine's embeddable options expose the same two keys, so an in-process consumer configures them identically

#### Scenario: Matcher and identity move together

- **WHEN** a key is rebound
- **THEN** both the matcher a request renders and the identity a series composes ("Cluster identity composed from zone and environment labels") move to the new label together, so a deployment can never filter on one label while identifying by another

#### Scenario: Defaults apply when unset

- **WHEN** the server starts with neither variable nor flag set and a request carries `az=zone-a`
- **THEN** the rendered matcher is `az="zone-a"`

#### Scenario: Environment variable rebinds the key

- **WHEN** the server starts with `KSG_ENV_LABEL=deployment_tier` and a request carries `env=prod`
- **THEN** the rendered matcher is `deployment_tier="prod"`

#### Scenario: Rebound key drives the identity read

- **WHEN** the server starts with `KSG_ENV_LABEL=deployment_tier` and `c1`'s series carry `az="us"` and `deployment_tier="dev"` but also an unrelated `env="ignored"` label
- **THEN** the composed identity is `us-dev-c1`

#### Scenario: Invalid key fails startup

- **WHEN** the server starts with `KSG_AZ_LABEL=topology.kubernetes.io/zone`
- **THEN** startup fails with an error naming `KSG_AZ_LABEL` and stating that the value is not a valid PromQL label name

#### Scenario: Identical keys are rejected

- **WHEN** the server starts with `--az-label=scope --env-label=scope`
- **THEN** startup fails with an error stating that the two keys must differ

### Requirement: Backend routing composes with request-scoped selectors

Backend selection and PromQL matcher rendering SHALL be independent, composed mechanisms: the `az` dimension SHALL still be rendered as a label matcher on every query that accepts it **in addition to** selecting the backends it is issued to. The rendered query string SHALL be identical across every backend a query is fanned out to; a per-backend query variant SHALL NOT exist.

#### Scenario: Neither mechanism substitutes for the other

- **WHEN** a zone-routed family takes the `az` matcher
- **THEN** routing narrows which store is asked and the matcher narrows what that store returns
- **AND** every zone-routed family except `harvest` — `ksm`, `kubelet` and `alerts` — takes the matcher, while `harvest` is zone-routed WITHOUT it: the store a Harvest query is routed to is that zone's filter, and the query string carries no `az` (or `env`) matcher

#### Scenario: Only the zone selects backends

- **WHEN** a request carries `env`, `cluster` or `namespace`
- **THEN** those dimensions play no part in backend selection

#### Scenario: Zone matcher still rendered under routing

- **WHEN** a request carries `az=zone-a` and the routing table sends `ksm` queries for that zone only to backend `zone-a`
- **THEN** the query issued to `zone-a` still carries the `az="zone-a"` matcher

#### Scenario: Harvest routed by zone without a matcher

- **WHEN** a request carries `az=zone-a` and the routing table sends `harvest` queries for that zone only to backend `netapp-a`
- **THEN** `netapp-a` receives each Harvest query as the same string an unfiltered build issues, carrying no `az` matcher

#### Scenario: Identical query string across backends

- **WHEN** a request with no `az` fans `kube_pod_info` out to three backends
- **THEN** all three receive byte-identical query strings

#### Scenario: Namespace filter does not route

- **WHEN** a request carries `namespace=shop` and no `az`
- **THEN** every backend serving the family is selected, exactly as for a request carrying neither parameter

### Requirement: Cluster identity composed from zone and environment labels

The topology reader SHALL identify a Kubernetes cluster by the composite `<az>-<env>-<cluster>`, composed when a series' `cluster` label is read, from that series' own zone and environment labels under the configured keys ("Configurable `az` / `env` label keys"). Every cluster-scoped structure SHALL be keyed on this identity, so two clusters sharing a raw name in different zones or environments are distinct.

#### Scenario: Structures keyed on the identity

- **WHEN** the reader keys a cluster-scoped structure
- **THEN** it uses the identity for entity ids, entity `cluster` labels, every join key and index, the cluster-family key, the observed-clusters set, and the `cluster` label of the self-metric gauges

#### Scenario: Identity table is exposed on the graph

- **WHEN** a build completes
- **THEN** the built graph exposes the identity table (identity → `{az, env, name}`), and the raw-name component is recoverable from any identity in it

#### Scenario: Cluster-family key over the identity

- **WHEN** the `pod-service-graph` capability computes a cluster-family key
- **THEN** it computes it over the identity string with its existing rule, so a family is scoped to one zone and one environment

#### Scenario: Estate without zone and environment pairs is unchanged

- **WHEN** a build's series carry no zone/environment pair
- **THEN** every identity, id, label and index is produced exactly as without composition

#### Scenario: Same raw name in two zones yields two clusters

- **WHEN** `kube_node_info{cluster="c1",node="worker-0",az="us",env="dev"}` and `kube_node_info{cluster="c1",node="worker-0",az="eu",env="prod"}` both exist in the window
- **THEN** the reader emits two distinct K8s node entities with ids `us-dev-c1/worker-0` and `eu-prod-c1/worker-0`, each with `labels.cluster` equal to its own identity, and the observed-clusters set holds both `us-dev-c1` and `eu-prod-c1`

#### Scenario: Every cluster-scoped structure carries the identity

- **WHEN** `cluster-alpha`'s pod, node, service and claim series all carry `az="zone-a"` and `env="prod"`
- **THEN** pod ids are `zone-a-prod-cluster-alpha/<uid>`, PVC ids `zone-a-prod-cluster-alpha/<ns>/<claim>`, the service index is keyed `(zone-a-prod-cluster-alpha, <ns>, <svc>)`, a pod's `labels.node` is `zone-a-prod-cluster-alpha/<node>`, and the observed-clusters set holds `zone-a-prod-cluster-alpha` and not `cluster-alpha`

#### Scenario: Cluster family is scoped to zone and environment

- **WHEN** the build holds identities `us-dev-c1`, `us-dev-c2`, `eu-prod-c1` and `eu-prod-c2`
- **THEN** `us-dev-c1` and `us-dev-c2` share a family key (`us-dev-c0`), `eu-prod-c1` and `eu-prod-c2` share another (`eu-prod-c0`), and no cross-family fan-out or ingress-cluster candidacy exists between the two families

#### Scenario: Unstamped estate is unchanged

- **WHEN** no series in the build carries a zone or environment label
- **THEN** every identity equals the raw `cluster` label, the identity table is empty, no warning is logged, and every id, label and index is byte-identical to the pre-change reader

### Requirement: Cluster names resolve through one identity ladder

Every cluster name the reader encounters — on a topology or kubelet series, on a service-graph series' `cluster` label, or arriving from the route store — SHALL be resolved through one ladder: compose, then adopt, then verbatim. The identity table SHALL be built once per build from the four entity-minting families, and the outcome SHALL NOT depend on series order.

#### Scenario: Ladder steps

- **WHEN** a cluster name is resolved
- **THEN** step 1, **Compose**: a series carrying non-empty values for both configured keys resolves to `<az>-<env>-<raw>`
- **AND** step 2, **Adopt**: otherwise, when the raw name maps to exactly one identity in the build's identity table, it resolves to that identity
- **AND** step 3, **Verbatim**: otherwise it resolves to the raw name itself (the `unknown` bucket when the `cluster` label is absent), and the reader counts such series per metric and logs one aggregated `cluster_identity_unresolved` warning per affected metric per build

#### Scenario: Identity table sources

- **WHEN** the identity table is built
- **THEN** it is the set of step-1 compositions, keyed by raw name, from the four families that mint cluster-labelled entities — `kube_pod_info`, `kube_node_info`, `kube_service_info`, `kube_pod_spec_volumes_persistentvolumeclaims_info`
- **AND** every other family resolves through the table and does not add to it

#### Scenario: Unambiguous raw name adopts the identity

- **WHEN** the only cluster identity composed from a raw `cluster="c1"` in the build is `us-dev-c1`, and a `kubelet_volume_stats_used_bytes{cluster="c1",...}` series carries neither zone nor environment label
- **THEN** the kubelet series resolves to `us-dev-c1`, its usage joins the `us-dev-c1/<ns>/<claim>` PVC, and no `cluster_identity_unresolved` warning is logged

#### Scenario: Ambiguous raw name stays verbatim and is warned

- **WHEN** the build composed both `us-dev-c1` and `eu-prod-c1` from raw `c1`, and a `kube_pod_owner{cluster="c1",...}` series carries neither zone nor environment label
- **THEN** that series resolves to the verbatim `c1`, joins neither identity (the affected pods keep no `owner` from it), and the build logs one `cluster_identity_unresolved` warning naming `kube_pod_owner` with the count of such series

#### Scenario: Partially-stamped series never composes

- **WHEN** a `kube_pod_info{cluster="c1",az="us"}` series carries a zone but no environment label and no identity for raw `c1` exists in the build
- **THEN** the pod's cluster is the verbatim `c1` (never `us--c1` or `us-c1`), and the series is counted as unresolved

#### Scenario: Identity table is built only from entity families

- **WHEN** every `kube_pod_info` series of `c1` carries `az="us",env="dev"` while a `kube_pod_owner` series of `c1` carries `az="eu",env="prod"`
- **THEN** the identity table maps `c1` to `us-dev-c1` only, the owner series composes to `eu-prod-c1` by step 1 and joins nothing, and no second `c1` cluster is created by it

#### Scenario: Foreign cluster names resolve through the same ladder

- **WHEN** a service-graph series carries `cluster="c1"` with no zone/environment labels and the build's only identity for `c1` is `us-dev-c1`, and a route-store destination names cluster `c1`
- **THEN** both resolve to `us-dev-c1`, the trace-cluster anchor and the route destination look up topology under `us-dev-c1`, and neither is counted as unresolved

### Requirement: Application-rooted recovery reads of the owner and annotation families

Under a `/v1/storage-graph` request carrying an `application=` root, the topology reader SHALL issue — beside its by-reference reads — a recovery of the Application's pods and claims over the same families, each restricted by a stage-specific key. `/v1/graph`, and a `/v1/storage-graph` request without an `application=` root, SHALL issue none of these reads.

#### Scenario: Recovery stages are defined by the storage graph

- **WHEN** the recovery runs
- **THEN** its stages, their gating, error classes and bound are those of the `storage-graph-api` capability's "Application roots are tracked through their controllers and claims"
- **AND** on that endpoint every recovery query error fails the build, per its "Storage build fails closed on upstream query errors"

#### Scenario: Recovery restriction per family

- **WHEN** the recovery reads the owner and annotation families
- **THEN** the six controller-annotation families are restricted on `annotation_argocd_argoproj_io_tracking_id` to the values whose segment before the first `:` is a root Application, beside their fixed `annotation_argocd_argoproj_io_tracking_id!=""` matcher
- **AND** `kube_replicaset_owner` is restricted to `owner_kind="Deployment"` and `owner_name` in the recovered Deployment names
- **AND** `kube_job_owner` is restricted on `owner_name` to the recovered CronJob names, beside its fixed `owner_kind="CronJob",owner_is_controller="true"` matcher
- **AND** `kube_pod_owner` is restricted to `owner_is_controller="true"`, one query per `owner_kind`, with `owner_name` in that kind's recovered names
- **AND** `kube_persistentvolumeclaim_annotations` is restricted on `annotation_argocd_argoproj_io_tracking_id` to the same values as the six controller-annotation families — the claims carrying a root Application of their own

#### Scenario: Recovery query shape and tally

- **WHEN** a recovery query is issued
- **THEN** it carries the family's request-scoped matchers (**[AECN]**) exactly as its by-reference read does and is issued at the family's bare name
- **AND** its returned series are counted under the family's name in the build's per-family tally, added to what the by-reference read of the same family contributes

#### Scenario: Recovery yields names only

- **WHEN** the recovery returns series
- **THEN** it does not write into the vectors the by-reference reads parse; it yields pod and claim names only
- **AND** every attribute of a recovered pod — `owner`, `application`, `status` — is resolved from the by-reference reads exactly as for a claim-binding pod

#### Scenario: Recovery queries compose the fixed selector, the request matchers and the recovery key

- **WHEN** a `/v1/storage-graph` build for `?az=zone-a&env=prod&cluster=c1&application=checkout` runs
- **THEN** every recovery query on `kube_deployment_annotations` carries `annotation_argocd_argoproj_io_tracking_id!=""`, `<az-key>="zone-a",<env-key>="prod",cluster="c1"` and the tracking-id restriction; the `kube_job_owner` recovery query carries `owner_kind="CronJob",owner_is_controller="true"`, the same request matchers and an `owner_name` restriction; and the `kube_pod_owner` recovery queries carry `owner_is_controller="true"`, one `owner_kind` equality each, the request matchers and an `owner_name` restriction

#### Scenario: A recovered pod's attributes come from the by-reference reads

- **WHEN** the recovery returns pod `web-7d9f-abc` and the by-reference `kube_pod_owner` read for that name returns its ReplicaSet owner
- **THEN** the pod's `data.owner` and `data.application` are resolved from the by-reference `kube_pod_owner`, `kube_replicaset_owner` and `kube_deployment_annotations` reads, byte-identical to the attributes the same pod carries when loaded through a claim binding

#### Scenario: Recovery series are tallied under the family name

- **WHEN** a `/v1/storage-graph` build's recovery returns two `kube_deployment_annotations` series and its by-reference controller read returns three
- **THEN** the per-family tally carries `kube_deployment_annotations` with `5`

#### Scenario: Claims carrying the Application are recovered by tracking-id

- **WHEN** a `/v1/storage-graph` build for `?az=zone-a&env=prod&application=billing` runs and claim `shop/ledger-data` carries tracking-id `billing:/PersistentVolumeClaim:shop/ledger-data`
- **THEN** a `kube_persistentvolumeclaim_annotations` query restricted to the tracking-id values whose segment before the first `:` is `billing` is issued beside the request-scoped matchers, it returns the `shop/ledger-data` series, and the claim's `data.application` is still resolved by the ordinary claim-annotation rules
