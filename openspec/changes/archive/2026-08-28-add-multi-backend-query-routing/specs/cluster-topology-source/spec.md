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
- `kube_pod_container_info{cluster, namespace, pod, uid, container, image, ...}` **[AECN]** (OPTIONAL — feeds the per-pod container list attribute; one series per container)
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

The three service/endpointslice families are OPTIONAL: when absent (kube-state-metrics not exporting services or endpointslices), the reader SHALL still build a valid topology, the service/endpoint indexes are simply empty, and connection-string resolution in the pod-service-graph reader degrades gracefully — `"://"` service endpoints that cannot be resolved against an empty index become `external/<label>` nodes. Under a selector-level filter the indexes hold only the in-scope services and the in-scope backing pods.

`kube_persistentvolumeclaim_info` is likewise OPTIONAL: when absent — or when no series matches a given PVC — the reader SHALL still build a valid topology, the affected PVC entities carry no `storageclass` attribute, no `volumename` label (and hence no `svm` label and no `pvc-to-netapp-aggr` edge — see the `netapp-storage-graph` capability), and the Cytoscape serialiser nests those PVCs under their namespace group (`cluster > namespace > pvc`) like any other PVC.

`kube_pod_container_info` is likewise OPTIONAL: when absent — or when no series matches a given pod — the reader SHALL still build a valid topology, the affected pod entities carry no `containers` attribute, and the build does not fail.

The six controller-annotation families and `kube_job_owner` are likewise OPTIONAL: when absent — or when no series matches a given pod's resolved controller owner, or the matched series' `annotation_argocd_argoproj_io_tracking_id` label is empty — the reader SHALL still build a valid topology, the affected pod entities carry no `application` attribute and nest directly under their `controller` group (or their namespace group when they also have no controller owner), and the build does not fail. The reader SHALL NOT read any `argocd_tracking_id` label from `kube_pod_owner`; that label is no longer part of the series contract.

**Query error is a separate axis from an empty vector**, and the seven split on it. `kube_replicaset_annotations` and `kube_job_annotations` SHALL degrade on a query error: the failure is logged, the family is treated as an empty vector, and the build completes without the Applications that family would have supplied. The other five — the Deployment, StatefulSet, DaemonSet and CronJob annotation families and `kube_job_owner` — SHALL fail the build on a query error, as every other kube-state-metrics leg does. The two that degrade are the two whose cardinality **accumulates with history** rather than tracking the live object count (one series per ReplicaSet retained by a Deployment's `revisionHistoryLimit`, one per Job retained by a CronJob's history limits), so they are the two that can exceed an upstream series or sample limit in an estate whose live object count is unremarkable — and losing an `application` string is never worth failing the whole graph. Caller-originated cancellation — a build timeout or a disconnected client — SHALL still fail the request for these two, since the caller is no longer waiting for any result.

A degrade SHALL be **subtractive**: it removes Applications the failed family would have supplied and never substitutes a different one. This requires one gate. The Job → CronJob hop is taken only when the Job carries no annotation of its own, a fact a family that was never read cannot establish — so when the `kube_job_annotations` query fails, the reader SHALL suppress the hop for that build rather than let every Job miss fall through to its owning CronJob's Application. A Job that genuinely carries no annotation of its own therefore also resolves no Application while that leg is degraded. `kube_replicaset_annotations` needs no equivalent gate: a bare ReplicaSet has no further ancestor to consult, so its miss resolves nothing either way.

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

### Requirement: Centralised VictoriaMetrics as the only topology source

The topology reader SHALL fetch all pod, node, and PVC topology by issuing PromQL queries against one or more configured Prometheus-compatible endpoints, pointing at VictoriaMetrics. Which endpoint a given query is issued to is decided by the routing table defined by the `upstream-backend-routing` capability; when no routing table is configured, the single `--prom-url` endpoint serves every query. The reader SHALL NOT call the Kubernetes API server, SHALL NOT scrape `kube-state-metrics` directly, and SHALL NOT use Kubernetes informers.

Regardless of how many endpoints are configured, the endpoints are the reader's **only** source of cluster facts: no Kubernetes client, no informer, no kubeconfig, and no per-cluster RBAC is involved at any point.

#### Scenario: Single configured upstream

- **WHEN** the server starts with `--prom-url=http://vm.example:8428` and no routing table
- **THEN** every topology query is sent to `http://vm.example:8428` and no other HTTP destinations

#### Scenario: Routed upstreams

- **WHEN** the server starts with a routing table declaring `http://vm-a.example:8428` and `http://vm-b.example:8428`
- **THEN** every topology query is sent to one or both of those two endpoints as the routing table dictates, and to no other HTTP destinations

#### Scenario: No Kubernetes API access

- **WHEN** the server runs in any environment
- **THEN** the binary makes no requests to any `/api/*` Kubernetes API path and requires no Kubernetes ServiceAccount or kubeconfig

### Requirement: Optional basic-auth credentials for the upstream endpoint

The server SHALL support optional HTTP Basic Auth credentials for each upstream Prometheus-compatible endpoint, sourced **exclusively** from environment variables. No CLI flag SHALL exist for any credential value — credential-carrying flags leak through process listings and container specs; this is a deliberate exception to the env+flag dual-track configuration convention.

`KSG_PROM_USERNAME` and `KSG_PROM_PASSWORD` are the **global** pair. When both are set (non-empty), every outbound HTTP request to an upstream that declares no credentials of its own — topology queries, the service-graph queries, the Harvest queries, and the `/readyz` `up` probe — SHALL carry an `Authorization: Basic` header for those credentials. When both are unset, such requests SHALL carry no `Authorization` header and behaviour is unchanged from an unauthenticated deployment.

A routed deployment MAY additionally give an individual backend its own pair by naming the environment variables holding it, as specified by the `upstream-backend-routing` capability's per-backend credential requirement; that pair takes precedence over the global one for requests to that backend.

Setting exactly one of the two global variables (non-empty) SHALL fail server startup with a validation error that names both environment variables but does NOT echo either value.

Credential values SHALL NOT appear in any log line, trace span attribute, metric label, error message, or HTTP response body. Rotation of a credential **value** requires a process restart — there is no hot reload for upstream credential values.

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

### Requirement: Request-scoped upstream selectors

The build SHALL accept four request-scoped selector dimensions — `az`, `env`, `cluster`, `namespace` — each a set of values, and SHALL render them into the upstream PromQL queries as label matchers composed **with** (never replacing) each query's fixed, request-invariant selectors (`type=~"ExternalIP|InternalIP"`, `condition="Ready"`, `lun=""`, `owner_kind="CronJob",owner_is_controller="true"` on `kube_job_owner`, `annotation_argocd_argoproj_io_tracking_id!=""` on each of the six controller-annotation families, the service-graph sentinel and `edge_relation!="link"` matchers). Which dimensions reach which series is a hardcoded contract with no configuration surface:

- `az` and `env`: every kube-state-metrics and kubelet series. **Never** a NetApp Harvest series — the `az` dimension reaches Harvest through backend selection only (the `upstream-backend-routing` capability's zone rule), and `env` does not reach Harvest at all.
- `cluster`: every kube-state-metrics and kubelet series. **Never** a Harvest series — Harvest's `cluster` label names the ONTAP cluster, not a Kubernetes cluster — and never a service-graph series.
- `namespace`: every kube-state-metrics and kubelet series that carries a `namespace` label — the pod-, claim-, Service-, EndpointSlice-, and **controller**-scoped series (the six controller-annotation families and `kube_job_owner` are namespaced like every other workload family). Never a node series, never a Harvest series, never a service-graph series.
- Every NetApp Harvest series, the three `traces_service_graph_*` series, and the `up{}` probe SHALL carry **no** request-scoped matcher under any request. The `qos_*` families keep their fixed `lun=""` selector.

Rendering SHALL be a pure function of the sorted, de-duplicated value set: one value renders `<key>="<value>"` (with `"` and `\` escaped); two or more render one fully-anchored alternation `<key>=~"<v1>|<v2>"` whose alternatives are regex-quoted and THEN string-escaped (a backslash introduced by regex-quoting is doubled, because a PromQL string literal rejects an unknown escape sequence); an empty set renders nothing. Matchers inside a selector SHALL appear in a fixed order (fixed selectors first, then `az`, `env`, `cluster`, `namespace`). The `cluster` value `unknown` renders `cluster=~"unknown|"` (see "Series missing the cluster label"), the one value that is not rendered as a plain literal. Series families that carry no request-scoped matcher for a dimension are narrowed by **reference** instead — a node is emitted only when a loaded pod is scheduled on it, an aggregate only when a loaded claim's `volumename` joins to it — as specified by the `graph-api` retention requirements.

A build with every dimension empty SHALL add **no** request-scoped matcher to any query, so each query renders exactly its fixed form. Zero rows under a non-empty dimension is a valid, empty topology — not a failure and not a retention miss.

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

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={cluster-alpha}`, `namespace={shop}`
- **THEN** every Harvest query (`volume_labels`, the `qos_*` families, `qos_policy_fixed_max_throughput_*`, `aggr_*`, `node_new_status`) is issued exactly as in an unfiltered build — the `qos_*` families with `lun=""` only — with no `az`, `env`, `cluster`, or `namespace` matcher; the `az` value reaches Harvest only as backend selection (which `harvest` backends the query is issued to) and the `env` value does not reach it at all

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
- **THEN** every issued query string carries only its own fixed selector — byte-identical to the recorded unfiltered rendering, with no request-scoped matcher appended

## ADDED Requirements

### Requirement: Backend routing composes with request-scoped selectors

Backend selection and PromQL matcher rendering SHALL be independent, composed mechanisms. The `az` dimension SHALL continue to be rendered as a label matcher on every query that accepts it — exactly as specified by "Request-scoped upstream selectors" — **in addition to** selecting which backends the query is issued to. Neither mechanism SHALL substitute for the other: routing narrows which store is asked, the matcher narrows what that store returns. The `harvest` family is the one family where the two diverge: it is zone-routed yet accepts no `az` matcher, so backend selection is the only effect `az` has on it (see the `netapp-storage-graph` capability).

The rendered query string for a given query SHALL be identical across every backend the query is fanned out to. A per-backend query variant SHALL NOT exist.

The `env`, `cluster`, and `namespace` dimensions SHALL play no part in backend selection.

#### Scenario: Zone matcher still rendered under routing

- **WHEN** a request carries `az=zone-a` and the routing table sends `ksm` queries for that zone only to backend `zone-a`
- **THEN** the query issued to `zone-a` still carries the `az="zone-a"` matcher

#### Scenario: Identical query string across backends

- **WHEN** a request with no `az` fans `kube_pod_info` out to three backends
- **THEN** all three receive byte-identical query strings

#### Scenario: Namespace filter does not route

- **WHEN** a request carries `namespace=shop` and no `az`
- **THEN** every backend serving the family is selected, exactly as for a request carrying neither parameter
