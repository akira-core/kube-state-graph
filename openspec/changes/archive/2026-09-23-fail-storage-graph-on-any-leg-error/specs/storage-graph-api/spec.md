## ADDED Requirements

### Requirement: Storage build fails closed on upstream query errors

A `/v1/storage-graph` build SHALL fail when ANY upstream query it issues returns an error — whatever the family, and whichever chunk, phase, stage or backend of that family failed — with the single exception of `ALERTS`. This covers every family `/v1/graph` reads as OPTIONAL or degrading: `volume_labels` (every phase), the six `qos_*` workload families (every chunk), the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, `node_total_data`, `kubelet_volume_stats_used_bytes`, `kubelet_volume_stats_capacity_bytes`, `kube_replicaset_annotations` and `kube_job_annotations` (by-reference read and application recovery alike). The per-family log-and-continue, per-chunk degrade and degraded-family hop-suppression rules of the `netapp-storage-graph` and `cluster-topology-source` capabilities SHALL apply to `/v1/graph` builds only. A storage body with a silently missing family is indistinguishable from a smaller estate — a filer with no flow, an aggregate with no I/O — so the endpoint SHALL NOT return one.

The failure SHALL be mapped exactly as a required-leg failure is: HTTP 502 with `reason: "upstream"`. The `message` SHALL name the family whose query failed (`upstream query failed: <family>`, the bare family name) and SHALL NOT carry an upstream URL, host, address or credential. A build that exceeds `--build-timeout` SHALL still map to 504 `timeout`, and caller cancellation to the existing `canceled` mapping. Every failed query SHALL still be logged server-side with its full error and counted on `kube_state_graph_upstream_query_failures_total{query}`. The in-process storage-graph engine surface SHALL return the same typed upstream error, carrying the family name.

`ALERTS` SHALL stay OPTIONAL: a failed `ALERTS` query is logged and counted, the build continues with no alert attached to any node, and every node's `status` is folded from its remaining signals.

The following SHALL NOT fail the build, because no query failed: a query that returns an empty vector (a family the deployment does not export, an annotation family that is not allowlisted, a scope that matched nothing); a requested zone that no backend serving the family declares (the existing empty-vector-and-warning rule); a restriction that falls back to an unrestricted read because it is unbounded or unrenderable; and a query the build does not issue.

`GET /v1/graph` SHALL keep every existing error class unchanged.

#### Scenario: A volume-label failure fails the storage request

- **WHEN** a client sends `GET /v1/storage-graph?...&az=zone-a&env=prod&aggr=aggr00` and the `volume_labels` query fails with an upstream error
- **THEN** the server returns 502 with `reason: "upstream"` and `message: "upstream query failed: volume_labels"`, and returns no graph body

#### Scenario: A QoS chunk failure fails the storage request

- **WHEN** the scoped QoS read is split into three chunks and the second `qos_read_ops` chunk fails
- **THEN** the server returns 502 with `reason: "upstream"` naming `qos_read_ops`

#### Scenario: A kubelet failure fails the storage request

- **WHEN** the `kubelet_volume_stats_used_bytes` query fails with an upstream error
- **THEN** the server returns 502 with `reason: "upstream"` naming `kubelet_volume_stats_used_bytes`

#### Scenario: One backend failing a Harvest query fails the storage request

- **WHEN** two backends serve `harvest` for the request's zone and one of them fails the `aggr_space_used` query
- **THEN** the server returns 502 with `reason: "upstream"` naming `aggr_space_used`, and the message names no backend URL or host

#### Scenario: An ALERTS failure does not fail the storage request

- **WHEN** the `ALERTS` query fails with an upstream error and every other query succeeds
- **THEN** the server returns 200, no node carries an alert, every node's `status` is folded from its remaining signals, and the failure is counted on `kube_state_graph_upstream_query_failures_total{query="ALERTS"}`

#### Scenario: An empty family is not a failure

- **WHEN** `kube_job_annotations` returns an empty vector because the deployment does not allowlist Job annotations
- **THEN** the server returns 200 and the Job-owned pods resolve their Application through the CronJob hop exactly as before this requirement

#### Scenario: A zone with no backend is not a failure

- **WHEN** a client sends `?az=zone-c&env=prod` and no backend serving `harvest` declares `zone-c`
- **THEN** the Harvest legs return empty vectors, the existing warning is logged, and the server returns 200

#### Scenario: The graph endpoint keeps degrading

- **WHEN** a client sends `GET /v1/graph` and the `volume_labels` query fails with an upstream error
- **THEN** the server returns 200 exactly as before this requirement, with no `pvc-to-netapp-aggr` edge

### Requirement: Storage build reads only what the body draws

The storage-graph build SHALL NOT issue the following kube-state-metrics families: `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`, `kube_service_annotations`. The body contains no `service` or `external` node and no `service-selects-pod` edge, so the four service-side families can contribute nothing to it, and it does not carry `containers` (see "Attributes and compound groups carry over"). A family the build does not issue SHALL be absent from the build's per-family series tally, never reported as zero.

Every other family the `/v1/graph` topology read issues falls into one of two classes. The **unrestricted** class is read exactly as `/v1/graph` reads it, under the request-scoped matchers alone: the claim-binding family `kube_pod_spec_volumes_persistentvolumeclaims_info` (the root every by-reference scope is computed from — nothing precedes it that could restrict it); `kube_persistentvolumeclaim_info`, `kube_persistentvolumeclaim_annotations` and the two kubelet volume-stats families (one series per claim the binding family already names, so a restriction could not select fewer than the binding read already does); every Harvest family except the volume-label topology family under a storage-side-rooted request (a storage root SHALL be drawable whether or not a claim reaches it, and an SVM is named by `volume_labels` alone; `volume_labels` alone leaves this class when the request names the components it should read, per "Storage-side roots restrict the Harvest topology read"); and `ALERTS` (already restricted to firing alerts). The **by-reference** class — the two pod families, the four Kubernetes-node families and the eight controller families below — is read restricted to the object names the families read before it actually carry. A by-reference family whose scope is empty SHALL NOT be issued at all and SHALL be absent from the tally; when issued, its tally entry is the count of series its restriction matched.

Every by-reference restriction SHALL be a sorted, de-duplicated, anchored alternation on the family's own identity label, **composed with** the family's fixed, request-invariant selector where it has one (`type=~"ExternalIP|InternalIP"`, `condition="Ready"`, `owner_kind="CronJob",owner_is_controller="true"`, `annotation_argocd_argoproj_io_tracking_id!=""`) and with the request-scoped matchers the family already carries — never replacing either. It is derived from upstream data and from the request's roots, not from a selector-level dimension, so the request-scoped selector table is unchanged. Every restriction SHALL be **chunked deterministically** under one byte budget shared with the QoS workload read, one query per chunk per family, results merged in **chunk order**, every chunk issued under the bare family name for self-metrics and span dimensions, and a single name SHALL always be issued even when it alone exceeds the budget. A chunk error of any of these families SHALL fail the build, as "Storage build fails closed on upstream query errors" requires — including `kube_replicaset_annotations` and `kube_job_annotations`, which `/v1/graph` reads as degrading families. Caller-originated cancellation SHALL fail the request whatever the family.

**Pods.** The build SHALL read `kube_pod_info` and `kube_pod_owner` restricted to the union of (a) the `pod` names of every claim-binding series it loaded, (b) the pod-name segment of every `pod=<namespace>/<pod-name>` root the request carries, and (c) the pod names the application recovery of "Application roots recover their pods before the pod read" returned. Under an `application=` root, (a) SHALL be narrowed to the binding pods of claims that are own-annotated with a root Application or mounted by a pod in (b) ∪ (c) — every mounter of such a claim included — as that requirement defines. The pod read SHALL wait on the claim-binding family and — when the request carries an `application=` root — on the recovery and on `kube_persistentvolumeclaim_annotations` alone, and SHALL NOT wait on families it does not read. Because a root has to be in the restriction to be loaded, the storage build SHALL receive the request's roots as an input. A `pod=` root that mounts no claim SHALL still be materialised, as "Roots are always materialised when the upstream knows them" requires; so SHALL every recovered pod that resolves a root Application. When the pod scope is empty — no (possibly narrowed) claim-binding pod, no `pod=` root, and no recovered name — no pod query is issued, no controller query is issued, and the body holds no complete path: it contains only the roots the storage side materialised and any `node=` root the Kubernetes-node read below still draws.

**Kubernetes nodes.** The build SHALL read `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels` and `kube_node_status_condition` restricted on `node` to the union of (a) the `node` label of every pod the pod read loaded and (b) every `node=` root the request carries. The node read SHALL wait on the pod read alone. A `node=` root naming a Kubernetes node that no loaded pod is scheduled on SHALL still be materialised, so a request with node roots and no mounting pod issues the node families for the roots alone. An unscheduled pod contributes no name.

**Controllers.** The build SHALL read the eight controller families in two stages, each restricted to the owner names the loaded pods carry. From every loaded `kube_pod_owner` series with `owner_is_controller="true"` and a non-empty `owner_name`, the first stage groups the names by `owner_kind` and issues, restricted on the family's identity label: `kube_replicaset_owner` and `kube_replicaset_annotations` on `replicaset` for the `ReplicaSet` names; `kube_job_owner` and `kube_job_annotations` on `job_name` for the `Job` names; `kube_statefulset_annotations` on `statefulset` for the `StatefulSet` names; `kube_daemonset_annotations` on `daemonset` for the `DaemonSet` names. The second stage, waiting on the first alone, issues `kube_deployment_annotations` on `deployment` for the union of the pods' direct `Deployment` owner names and the `owner_name` of every loaded `kube_replicaset_owner` series naming a `Deployment` owner, and `kube_cronjob_annotations` on `cronjob` for the union of the pods' direct `CronJob` owner names and the `owner_name` of every loaded `kube_job_owner` series. A kind no loaded pod is owned by, and a second-stage name set no first-stage series populated, SHALL issue no query for its families. The first stage SHALL wait on the pod read alone. An owner kind the reader resolves no Application for (`ReplicationController`, `Node`, a custom-resource controller) contributes no name and issues nothing.

The by-reference reads SHALL be **output-preserving**: the reader consults a node family only at the `(cluster, node)` of a loaded pod's node or a node root, consults `kube_replicaset_owner` only at the ReplicaSet names of loaded pods, consults the annotation families only at the resolved owner of a loaded pod (a Deployment recovered from its ReplicaSet, a CronJob recovered from its Job), and consults `kube_job_owner` only at the Job names of loaded pods — so every consulted key names an object in its scope, and the body SHALL be byte-identical to the body an unrestricted read would produce. Names are unique within a namespace (controllers, pods) or a cluster (nodes) only, so a restriction MAY admit a same-named object from another namespace or cluster inside the request's selectors; such a series is keyed under its own cluster and namespace, is consulted by no loaded pod, and SHALL leave the body unchanged.

#### Scenario: Pod read is restricted to mounting pods and roots

- **WHEN** a build for `?az=zone-a&env=prod&pod=shop/web-0` loads claim-binding series naming pods `orders-0`, `orders-1` and `catalog-0` while the estate holds 40000 pods
- **THEN** every issued `kube_pod_info` and `kube_pod_owner` query restricts `pod` to exactly `{catalog-0, orders-0, orders-1, web-0}` alongside `<az-key>="zone-a",<env-key>="prod"`, no series for any other pod is fetched, and the body is byte-identical to the body an unrestricted pod read would produce

#### Scenario: Pod read is restricted to mounting pods, roots and recovered pods

- **WHEN** a build for `?az=zone-a&env=prod&application=checkout` loads claim-binding series naming pods `orders-0` and `catalog-0` (unrelated claims, unannotated), the recovery returns `web-7d9f-abc` and `orders-0`, and `catalog-0`'s claim is mounted by nobody else
- **THEN** every issued `kube_pod_info` and `kube_pod_owner` query restricts `pod` to exactly `{orders-0, web-7d9f-abc}`, and the body is byte-identical to the body an unrestricted pod read would produce for the same root

#### Scenario: Empty scope issues no pod query

- **WHEN** a build for `?az=zone-a&env=prod&aggr=aggr1` loads no claim-binding series
- **THEN** no `kube_pod_info`, `kube_pod_owner`, Kubernetes-node or controller query is issued, the per-family tally carries none of those families, and the body contains `aggr1`, its owning controller and no other node

#### Scenario: Claimless pod root is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/web-0` and `shop/web-0` mounts no claim
- **THEN** the pod's name is in the restriction, `kube_pod_info` names it, and the body contains that pod with its ordinary attributes and compound parent and no edges

#### Scenario: Skipped families never reach the upstream

- **WHEN** the upstream rejects every `kube_pod_container_info` query with a series-limit error and a client sends any `/v1/storage-graph` request
- **THEN** the storage build issues no `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels` or `kube_service_annotations` query, `kube_state_graph_upstream_query_failures_total` does not move, and the request returns 200

#### Scenario: Cross-namespace name collision is harmless

- **WHEN** claim-binding series name `shop/web-0` and the estate also holds a claimless `platform/web-0`
- **THEN** `platform/web-0` may be fetched but does not appear in the body, which is byte-identical to the body an unrestricted pod read would produce

#### Scenario: A pod chunk failure fails the build

- **WHEN** the restriction is split into three chunks and the second `kube_pod_info` chunk fails with an upstream error
- **THEN** the build returns an error and the request is mapped as an upstream failure, exactly as an unrestricted `kube_pod_info` failure is

#### Scenario: Controller read is restricted to the loaded pods' owners

- **WHEN** the loaded pods are owned by StatefulSet `orders` and by ReplicaSet `web-7d9f` (whose `kube_replicaset_owner` series names Deployment `web`), while the estate holds thousands of other controllers
- **THEN** `kube_statefulset_annotations` restricts `statefulset` to `{orders}`, `kube_replicaset_owner` and `kube_replicaset_annotations` restrict `replicaset` to `{web-7d9f}`, `kube_deployment_annotations` restricts `deployment` to `{web}` and is issued only after the ReplicaSet read returned, every one of those queries also carries its fixed selector and the request's matchers, no `kube_job_owner`, `kube_job_annotations`, `kube_daemonset_annotations` or `kube_cronjob_annotations` query is issued, and the body is byte-identical to the body an unrestricted read would produce

#### Scenario: Accumulated Job history does not reach the storage build

- **WHEN** the upstream's index for the day holds 300000 `kube_job_owner{owner_kind="CronJob",owner_is_controller="true"}` series, more than its series limit, and the loaded pods are owned by Jobs `backup-28901` and `backup-28902`
- **THEN** `kube_job_owner` is issued restricted to `job_name=~"backup-28901|backup-28902"` alongside its fixed selector, returns two series, and the request returns 200 with each pod's Application resolved through its CronJob exactly as `/v1/graph` resolves it

#### Scenario: Job-owned mounting pod resolves its CronJob Application through two stages

- **WHEN** a loaded pod is owned by Job `nightly-28901`, that Job carries no annotation of its own, `kube_job_owner` names CronJob `nightly` as its controller, and `kube_cronjob_annotations{cronjob="nightly"}` carries `reports:batch/CronJob:batch/nightly`
- **THEN** the first stage issues `kube_job_owner` and `kube_job_annotations` restricted to `{nightly-28901}`, the second stage issues `kube_cronjob_annotations` restricted to `{nightly}` only after the first returned, and the pod carries `data.application="reports"` with `data.owner={kind:"Job", name:"nightly-28901"}` unchanged

#### Scenario: A kind no loaded pod is owned by issues no query

- **WHEN** every loaded pod is owned by a StatefulSet
- **THEN** no `kube_replicaset_owner`, `kube_job_owner`, `kube_deployment_annotations`, `kube_daemonset_annotations`, `kube_replicaset_annotations`, `kube_job_annotations` or `kube_cronjob_annotations` query is issued and the per-family tally carries none of them

#### Scenario: A required controller chunk failure fails the build

- **WHEN** the ReplicaSet restriction is split into two chunks and the second `kube_replicaset_owner` chunk fails with an upstream error
- **THEN** the build returns an error and the request is mapped as an upstream failure, exactly as an unrestricted `kube_replicaset_owner` failure is

#### Scenario: Node read is restricted to the pods' nodes and node roots

- **WHEN** a client sends `?az=zone-a&env=prod&node=n9` and the loaded pods are scheduled on Kubernetes nodes `n1` and `n2` while the estate holds 5000 nodes
- **THEN** every issued `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels` and `kube_node_status_condition` query restricts `node` to exactly `{n1, n2, n9}` alongside its fixed selector and the request's matchers, no series for any other node is fetched, and the body is byte-identical to the body an unrestricted node read would produce

#### Scenario: Kubernetes node root with no mounting pod is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&node=n9`, no claim-binding series is loaded, and `kube_node_info` names Kubernetes node `n9`
- **THEN** the four node families are issued restricted to `{n9}`, no pod or controller query is issued, and the body contains the node `n9` with its ordinary attributes and no edges

#### Scenario: Controller scope cross-namespace collision is harmless

- **WHEN** a loaded pod in `shop` is owned by StatefulSet `db` and the estate also holds StatefulSet `platform/db` carrying a different tracking-id
- **THEN** `kube_statefulset_annotations{statefulset="db"}` may return both series, the pod resolves the `shop/db` Application only, and the body is byte-identical to the body an unrestricted read would produce

#### Scenario: An annotation chunk failure fails the build

- **WHEN** one `kube_job_annotations` or `kube_replicaset_annotations` chunk fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming that family, and no body is returned

### Requirement: Application roots recover their pods before the pod read

When a `/v1/storage-graph` request carries at least one `application=` root, the build SHALL recover, before it reads pods, the names of every pod owned by a controller whose ArgoCD tracking-id names one of the root Applications, and SHALL add those names to the pod scope of "Storage build reads only what the body draws". The recovery is a **candidate generator, never a judge**: a recovered pod is read exactly like any other scoped pod, its Application is resolved by the same controller-annotation rules every pod's is, and whether it is a root is decided solely by the projection over that resolved value. A recovered pod whose resolved Application is not a root value — a controller whose lexically-smallest tracking-id in the window names a different Application — is loaded and then dropped, so the body is a pure function of the forward resolution and never of the recovery.

The recovery SHALL run in three stages, each waiting on the previous alone, and SHALL start with the first wave (its first stage is derived from the request, so nothing precedes it):

**Stage 1 — controllers by tracking-id.** Each of the six controller-annotation families (`kube_deployment_annotations`, `kube_statefulset_annotations`, `kube_daemonset_annotations`, `kube_replicaset_annotations`, `kube_job_annotations`, `kube_cronjob_annotations`) SHALL be issued restricted on `annotation_argocd_argoproj_io_tracking_id` to exactly the values whose segment before the first `:` is one of the root values (a value with no `:` matches when it equals a root value verbatim), composed with the family's fixed selector and the request-scoped matchers — never replacing either. Root values SHALL be sorted, de-duplicated and escaped so a value carrying a regex metacharacter matches itself and nothing else. The stage yields one name set per kind from each family's identity label (`deployment`, `statefulset`, `daemonset`, `replicaset`, `job_name`, `cronjob`).

**Stage 2 — the reverse hops.** `kube_replicaset_owner` SHALL be issued restricted to `owner_kind="Deployment"` and `owner_name` in the stage-1 Deployment names, yielding ReplicaSet names; `kube_job_owner` SHALL be issued restricted on `owner_name` to the stage-1 CronJob names beside its fixed selector, yielding Job names. A stage-1 name set that is empty SHALL issue no query for its hop.

**Stage 3 — pods by owner.** `kube_pod_owner` SHALL be issued restricted to `owner_is_controller="true"`, one query per owner kind whose name set is non-empty: `ReplicaSet` (stage-1 ReplicaSet names ∪ stage-2 ReplicaSet names), `Job` (stage-1 Job names ∪ stage-2 Job names), `StatefulSet`, `DaemonSet`, and — for a pod directly owned by either — `Deployment` and `CronJob` for the stage-1 names of those kinds. The recovered `pod` labels are the names the pod scope gains.

Every restriction SHALL be chunked deterministically under the byte budget shared with every other data-derived alternation, one query per chunk, results merged in chunk order, each chunk issued under the bare family name. A chunk error on any recovery family — the six controller-annotation families, `kube_replicaset_owner`, `kube_job_owner` and `kube_pod_owner` — SHALL fail the build, as "Storage build fails closed on upstream query errors" requires: a lost chunk would silently drop the pods of the Applications it carried. Caller-originated cancellation fails the request whatever the family.

**Stage 1 SHALL be bounded, or unrestricted.** `application=` is repeatable and the parser bounds each value's length, never their count, so stage 1 is a request-derived scope a client can inflate. When a family's root restriction would take more than a fixed maximum number of queries, that family SHALL be issued with its fixed selector and the request-scoped matchers only — the shape `/v1/graph` issues it in — and its rows filtered to the root Applications in the reader, and the build SHALL log that it did so. Stages 2 and 3 are derived from upstream data and need no bound.

**Tally.** Series a stage returns SHALL be counted under the family's name in the build's per-family tally, added to whatever the by-reference read of the same family contributes, so a family issued by both reports the total and a family the recovery issued is present even when the by-reference read did not issue it.

**The recovered names narrow the claim-binding half of the pod scope.** Under an application root every retained path must satisfy the root, so a claim-binding pod SHALL enter the pod scope only when its claim is (a) own-annotated (`kube_persistentvolumeclaim_annotations`) with a tracking-id whose Application segment is a root value, or (b) mounted by a recovered pod or by a `pod=` root pod. Case (b) SHALL admit EVERY mounter of such a claim, not only the recovered one: a shared claim's `pvc-pod` weight is split over the mounters present in the built graph, and an Application a claim inherits comes from its mounters, so a co-mounter that is not loaded would change the split and the inheritance. The pod read therefore also waits on `kube_persistentvolumeclaim_annotations` when the request carries an application root. The narrowing is output-preserving: a pod can be a root only if it resolves a root Application, and every such pod is in the recovered set; a path can be retained only through a pod hit (its pod is recovered) or a claim hit (the claim is own-annotated, or inherits from a mounter that is recovered), so every pod on a retained path — and every co-mounter its weights depend on — is in the scope.

A request with no `application=` root SHALL issue no recovery query, so its queries and body are unchanged. An empty stage yields nothing downstream: when no controller carries a root Application, stages 2 and 3 issue nothing, and the pod scope holds only the `pod=` roots and the binding pods of own-annotated claims.

#### Scenario: A Deployment-managed stateless pod is recovered and drawn

- **WHEN** a client sends `?az=zone-a&env=prod&application=checkout`, Deployment `shop/web` carries tracking-id `checkout:apps/Deployment:shop/web`, `kube_replicaset_owner` names ReplicaSet `web-7d9f` as owned by Deployment `web`, `kube_pod_owner` names pod `web-7d9f-abc` as controlled by that ReplicaSet, and the pod mounts no claim
- **THEN** stage 1 issues the six annotation families each with a matcher on `annotation_argocd_argoproj_io_tracking_id` admitting exactly the values whose segment before the first `:` is `checkout`, alongside `annotation_argocd_argoproj_io_tracking_id!=""` and `<az-key>="zone-a",<env-key>="prod"`; stage 2 issues `kube_replicaset_owner` restricted to `owner_kind="Deployment"` and `owner_name` in `{web}` and no `kube_job_owner`; stage 3 issues `kube_pod_owner` restricted to `owner_is_controller="true"`, `owner_kind="ReplicaSet"` and `owner_name` in `{web-7d9f}`; the pod read restricts `pod` to `{web-7d9f-abc}` plus every claim-binding pod; and the body contains `web-7d9f-abc` with `data.application="checkout"`, its compound parents, and no edge

#### Scenario: A CronJob-managed pod is recovered through the reverse Job hop

- **WHEN** a client sends `?application=reports`, CronJob `batch/nightly` carries tracking-id `reports:batch/CronJob:batch/nightly`, `kube_job_owner` names Job `nightly-28901` as controlled by it, and `kube_pod_owner` names pod `nightly-28901-x` as controlled by that Job
- **THEN** stage 2 issues `kube_job_owner` with `owner_kind="CronJob",owner_is_controller="true"` and `owner_name` in `{nightly}`, stage 3 issues `kube_pod_owner` for kind `Job` with `owner_name` in `{nightly-28901}`, and the body contains `nightly-28901-x` with `data.application="reports"` and `data.owner={kind:"Job", name:"nightly-28901"}`

#### Scenario: An over-admitted pod is loaded and dropped

- **WHEN** a client sends `?application=beta`, and StatefulSet `shop/db` carries two tracking-ids in the window, `alpha:apps/StatefulSet:shop/db` and `beta:apps/StatefulSet:shop/db`
- **THEN** stage 1 admits `db`, its pods are read, each resolves `data.application="alpha"` (the lexically-smallest tracking-id), none is a root, and the body is byte-identical to the body of the same request against an estate where `db` carries only the `alpha` tracking-id

#### Scenario: No controller carries the Application

- **WHEN** a client sends `?application=typo` and no controller-annotation series in the selected estate carries a tracking-id naming `typo`
- **THEN** stage 1 issues its six queries, no stage-2 or stage-3 query is issued, no claim is own-annotated with `typo` so no binding pod enters the scope, no pod query is issued, and the body is empty

#### Scenario: Only related binding pods are read under an application root

- **WHEN** a client sends `?application=checkout`, the recovery returns `orders-0`, and claim-binding series name `orders-0` (claim `orders-data`), `catalog-0` (claim `catalog-data`, unannotated, mounted by nobody else) and `ledger-0` (claim `ledger-data`, own-annotated `billing:…`)
- **THEN** every `kube_pod_info` and `kube_pod_owner` query restricts `pod` to exactly `{orders-0}`; `catalog-0` and `ledger-0` are never read; and the body is byte-identical to the body a build reading every binding pod produces

#### Scenario: Every mounter of a related claim is read

- **WHEN** a client sends `?application=checkout`, the recovery returns `orders-0`, and the RWX claim `shared-data` is mounted by `orders-0` and by `report-0`, whose controller resolves `alpha`
- **THEN** the pod scope is `{orders-0, report-0}`, the claim inherits `alpha` (the lexically-smallest mounter Application) so `report-0`'s path is not retained, and the `pvc-pod` edge to `orders-0` carries half the claim's figures with `labels.attribution="split"` — exactly as when every binding pod is read

#### Scenario: A co-mounter that sorts after the root is drawn through inheritance

- **WHEN** the same claim is instead mounted by `orders-0` (`checkout`) and `zeta-0` (`zeta`)
- **THEN** the claim inherits `checkout`, both `pvc-pod` paths are retained, and `zeta-0` is present as part of the claim's path with a split edge of its own

#### Scenario: A required stage failure fails the build

- **WHEN** a stage-3 `kube_pod_owner` chunk fails with an upstream error
- **THEN** the build returns an error and the request is mapped as an upstream failure, exactly as an unrestricted `kube_pod_owner` failure is

#### Scenario: An unbounded root set reads stage 1 unrestricted

- **WHEN** a client sends thousands of `application=` values, enough that a family's restriction would take more queries than the maximum
- **THEN** that family is issued once with `annotation_argocd_argoproj_io_tracking_id!=""` and the request-scoped matchers only, its rows are filtered to the root Applications in the reader, the build logs that the roots did not yield a bounded restriction, and the body is byte-identical to the body the restriction would have produced

#### Scenario: The namespace filter narrows the recovery

- **WHEN** a client sends `?application=checkout&namespace=shop` and `checkout` owns controllers in `shop` and `platform`
- **THEN** every stage-1, stage-2 and stage-3 query carries `namespace="shop"`, only the `shop` pods are recovered, and the body holds no `platform` pod

#### Scenario: A request without the root issues no recovery

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1`
- **THEN** no query restricted on `annotation_argocd_argoproj_io_tracking_id` or on `owner_name` is issued, and the queries and body are byte-identical to the build before this root existed

#### Scenario: The recovery is tallied under the family name

- **WHEN** a client sends `?application=checkout`, stage 1 returns two `kube_deployment_annotations` series, and the by-reference controller read later returns three for the loaded pods' Deployments
- **THEN** the per-family tally carries `kube_deployment_annotations` with `5`, and a family only the recovery issued is present with the count of series its restriction matched

#### Scenario: A stage-1 annotation chunk failure fails the build

- **WHEN** a stage-1 `kube_job_annotations` chunk fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming `kube_job_annotations`, and no body is returned

### Requirement: Storage-side roots restrict the Harvest topology read

When a `/v1/storage-graph` request carries at least one `ontap_cluster=` or `aggr=` root, the build SHALL read the Harvest volume-label topology family restricted to the rooted components instead of reading the whole filer, and the body SHALL be byte-identical to the body an unrestricted read produces for every estate whose aggregates and controllers are each named by their own Harvest gauge families (the stock `aggr_*` and `node_*` templates). An aggregate or controller named by the volume-label family alone, outside the rooted components, is not materialised by a restricted read; that can change whether an alert without a `cluster` label matches a unique entity, and is the only divergence.

**Phase 1 — the root restriction.** The build SHALL issue the family restricted by the request's `ontap_cluster=` and `aggr=` values: `ontap_cluster=` restricts the family's `cluster` label and `aggr=` restricts its `aggr` label. A request carrying both SHALL issue ONE query carrying both matchers, because the projection combines them as a narrowing — an `aggr=` root names an aggregate only within the `ontap_cluster=` values, so an aggregate of that name on another filer is not a root and its volumes need not be read. Each value set SHALL be rendered as a sorted, de-duplicated, anchored alternation, escaped so a value carrying a regex metacharacter matches itself and nothing else. The restriction is the ONLY matcher this query carries; Harvest takes no request-scoped selector.

The restriction is derived from the request's roots, not from a selector-level dimension, so the request-scoped selector table is unchanged and `az` still reaches Harvest through backend selection alone. A `pod=` root does not prevent it: the projection ANDs the workload roots with the storage roots, so every retained path is one the restriction keeps.

**Phase 2 — candidate recovery.** A claim's aggregate and SVM are picked lexically-smallest over that claim's WHOLE candidate set, so a phase-1-only read could place a claim on a rooted aggregate where an unrestricted read would place it on a lexically-smaller one — a clone whose FlexVol name also matches the claim's derived token, or the same FlexVol name on a second filer. After phase 1, and after the claim-info family has landed, the build SHALL therefore issue a second restricted read of the same family, restricted on `volume` to the derived tokens of exactly the claims phase 1 matched, expressed in the **forward** direction of the configured derivation — the direction the join already computes. Nothing inverts a FlexVol name back to a PersistentVolume name. The two phases' results SHALL be merged and de-duplicated by label set before the parse, and every downstream consumer — the aggregate and SVM picks, the owning-controller vote, the inventory, and the QoS workload read's `volume` scope — SHALL run over the merged result.

When phase 1 matched no claim, phase 2 SHALL NOT be issued.

**Roots stay drawable.** The aggregate, controller and policy Harvest families are unrestricted in every build, so a rooted component with no claim on it is materialised from those families exactly as it is today. A phase 1 that returns nothing therefore still draws its roots; it draws no path through them, which is the same outcome an unrestricted read gives for a component no claim reaches.

**Roots that SHALL disable the restriction.** A request carrying an `svm=` or a `node=` root SHALL read this family unrestricted and single-phase, whatever other roots it carries. The owning controller of an aggregate is a vote over ALL of that aggregate's volume-label series (see the `netapp-storage-graph` capability's "NetApp aggregate entity"); restricting by `aggr` keeps every series of a rooted aggregate and leaves the vote intact, while restricting by `svm` or `node` leaves it running over a subset that can elect a different controller and move the `node-aggr` tier. The projection also UNIONS `aggr=` with `svm=`, so a request naming both retains paths reached only through the SVM, which a restriction by `aggr` alone would drop. `node=` names a Kubernetes node as well as an ONTAP controller and is admitted as a root whether or not any path reaches it.

**Match modes that SHALL NOT restrict.** Phase 2 expresses the `exact` and `suffix` volume-match modes exactly. A build configured with `contains` or `regex` SHALL read the family unrestricted and single-phase, because those modes cannot be rendered into an anchored alternation without changing their semantics.

**Mechanics.** Each phase's restriction SHALL be chunked deterministically under the byte budget shared with the QoS workload read, one query per chunk, results merged in chunk order, each chunk issued under the bare family name so self-metrics and span dimensions carry one value per family however many queries a build issues. A single value SHALL always be issued even when it alone exceeds the budget. Phase 1 chunks ONE of its two alternations and repeats the other verbatim in every chunk; the repeated matcher SHALL be charged against the budget at its RENDERED length, escaping and wrapper included, so the budget bounds what is actually sent. A failed chunk of either phase SHALL fail the build, as "Storage build fails closed on upstream query errors" requires. The per-family series tally SHALL report the merged series count under the one family name.

**A restriction SHALL be bounded, or not applied.** `ontap_cluster=` and `aggr=` are repeatable and the request parser bounds each value's length but never their COUNT, so this is the one scope a client can inflate. When the restriction would take more than a fixed maximum number of queries, the build SHALL read the family unrestricted and single-phase instead, and SHALL log that it did so. That is the read this leg performed before the restriction existed: one query, the same body, and a fan-out one request cannot enlarge. A restriction that cannot be rendered at all — every value normalising away, which an embedder filling the root sets directly can produce where the request parser cannot — SHALL take the same path. Neither reason is a query error, and neither SHALL fail a build.

**Coverage signalling.** Under a restricted read the join-coverage signal that counts loaded claims resolving no aggregate SHALL count only the claims that matched at least one volume-label series. A claim that matched none is outside the rooted components — the request did not ask about it — and counting it would fire the signal on nearly the whole estate; a claim that DID match a series and still resolved no aggregate is a FlexGroup, which is a genuine coverage miss under either read and SHALL still be counted. An unrestricted read SHALL count both, unchanged.

`GET /v1/graph` carries no roots and SHALL always read this family unrestricted and single-phase.

#### Scenario: Aggregate root restricts the topology read

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00` against a filer holding 20000 volume-label series of which 834 carry `aggr="aggr00"`
- **THEN** phase 1 issues the family restricted to `aggr=~"aggr00"` and loads those 834 series, phase 2 issues it restricted to the derived tokens of the claims that matched, no other filer volume is loaded, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: A request with no storage-side root is unrestricted

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0`
- **THEN** the volume-label family is issued once with no matcher of any kind, no second phase is issued, and the read is exactly the read a build performs today

#### Scenario: Cluster and aggregate roots narrow one query

- **WHEN** a client sends `?az=zone-a&env=prod&ontap_cluster=ontap-prod&aggr=aggr00`
- **THEN** phase 1 issues one query carrying both `cluster=~"ontap-prod"` and `aggr=~"aggr00"`, a volume on an `aggr00` of a different filer is not loaded, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: Cluster root alone restricts by cluster

- **WHEN** a client sends `?az=zone-a&env=prod&ontap_cluster=ontap-prod`
- **THEN** phase 1 issues the family restricted to `cluster=~"ontap-prod"` alone, every SVM of that filer is loaded and is a root, and the body is byte-identical to the unrestricted body

#### Scenario: A clone on a lexically-smaller aggregate keeps its pick

- **WHEN** a claim's derived token matches FlexVol `trident_pvc_x` on `aggr09` and a clone `snap_trident_pvc_x` on `aggr00`, and the request roots at `aggr=aggr09`
- **THEN** phase 2 loads both series, the aggregate pick resolves to `aggr00` exactly as an unrestricted read resolves it, the claim is not retained by the `aggr09` root, and the body is byte-identical to the unrestricted body

#### Scenario: A cross-filer FlexVol-name collision keeps its pick

- **WHEN** one FlexVol name exists on `ontap-prod` and on `ontap-lab`, the claim's token matches both, and the request roots at `aggr=` on `ontap-prod` while `ontap-lab` sorts first
- **THEN** phase 2 loads the `ontap-lab` series too, the pick resolves to the lexically-smallest `(ontap-cluster, aggr)` exactly as an unrestricted read resolves it, and the body is byte-identical to the unrestricted body

#### Scenario: An SVM root does not restrict the read

- **WHEN** a client sends `?az=zone-a&env=prod&svm=svm_shop`
- **THEN** the volume-label family is issued unrestricted and single-phase, every aggregate's owning-controller vote runs over all of that aggregate's series, and the body is byte-identical to the body a build produces today

#### Scenario: An SVM root disables an aggregate root

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00&svm=svm_shop` and `svm_shop` holds a claim on `aggr09`
- **THEN** the volume-label family is issued unrestricted and single-phase, the `aggr09` claim is retained through its SVM exactly as in an unrestricted build, and the body is byte-identical to the body a build produces today

#### Scenario: A node root does not restrict the read

- **WHEN** a client sends `?az=zone-a&env=prod&node=ontap-prod-01`
- **THEN** the volume-label family is issued unrestricted and single-phase, and the root still resolves against both the ONTAP controller and the Kubernetes node of that name

#### Scenario: A node root disables an aggregate root

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00&node=ontap-prod-01`
- **THEN** the volume-label family is issued unrestricted and single-phase and the body is byte-identical to the body a build produces today

#### Scenario: A pod root composes with an aggregate root

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00&pod=shop/orders-0`
- **THEN** phase 1 is restricted to `aggr=~"aggr00"`, the pod root joins the pod scope as it does today, and the body is byte-identical to the unrestricted body

#### Scenario: A non-default match mode opts out

- **WHEN** the deployment configures the `contains` volume-match mode and a client sends `?az=zone-a&env=prod&aggr=aggr00`
- **THEN** the volume-label family is issued unrestricted and single-phase and the body is byte-identical to the body a build produces today

#### Scenario: A rooted aggregate with no claim is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00`, no volume on `aggr00` matches any loaded claim, and the aggregate and controller families name `aggr00` and its owner
- **THEN** phase 2 is not issued, and the body contains `aggr00` with its health and usage attributes and its owning controller, and no path through them

#### Scenario: Takeover ownership survives the restriction

- **WHEN** a rooted aggregate's volume-label series disagree on the owning `node` because a takeover happened inside the window
- **THEN** phase 1 loads every series of that aggregate, the vote resolves to the lexically-smallest non-empty `node` exactly as an unrestricted read resolves it, and the `node-aggr` tier names that controller

#### Scenario: Join-coverage signal counts only what the restriction still explains

- **WHEN** a restricted build loads claims of which one joins, one matches a FlexGroup series carrying no `aggr`, and the rest match no series at all
- **THEN** the join-coverage warning counts the FlexGroup claim and not the claims that matched nothing, and the same estate read unrestricted counts both

#### Scenario: An unbounded root set reads the family unrestricted

- **WHEN** a client sends thousands of `aggr=` values, enough that the restriction would take more queries than the maximum
- **THEN** the build issues one unrestricted `volume_labels` query and no second phase, logs that the roots did not yield a bounded restriction, returns 200, and the body is byte-identical to the body the restriction would have produced

#### Scenario: A cluster-only restriction does not re-read what it already has

- **WHEN** a client sends `?az=zone-a&env=prod&ontap_cluster=ontap-prod` and a matched claim also has a candidate on `ontap-lab`
- **THEN** phase 2 excludes `ontap-prod` from its own selector, loads the `ontap-lab` candidate, and the aggregate pick is the one the whole-filer read makes; a request that also names an `aggr=` root excludes no cluster, because phase 1 then read only part of one

#### Scenario: The QoS scope narrows with the topology read

- **WHEN** a restricted build's merged volume-label result names 60 FlexVols that loaded claims matched, against 1200 in an unrestricted build
- **THEN** the QoS workload queries restrict `volume` to those 60 names, are chunked under the same budget, and every I/O measurement on a retained path is identical to the unrestricted build's

#### Scenario: A failed restricted chunk fails the build

- **WHEN** one phase-1 or phase-2 chunk fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming `volume_labels`, the failure is counted on `kube_state_graph_upstream_query_failures_total{query="volume_labels"}`, and no body is returned

## MODIFIED Requirements

### Requirement: Roots are always materialised when the upstream knows them

A root the upstream names in the window SHALL appear in the body even when no flow passes through it. A storage root "exists" when at least one Harvest series read in the build names it (`volume_labels`, `node_labels`, `node_new_status`, the node performance counters, or the `aggr_*` families; an SVM only via `volume_labels`); a workload root exists when `kube_node_info` (node) or `kube_pod_info` (pod) names it in the selected estate. A flowless root SHALL be emitted with its ordinary attributes and its compound parent (an aggregate root also materialises the controller currently owning it, so that `data.parent` never dangles) and **no** edges. A root NO series names SHALL NOT be drawn: the body is simply empty of it, with no error and no marker.

An `application=` root exists when at least one pod loaded in the selected estate resolves that Application, and **every** such pod SHALL be materialised with its ordinary attributes and compound parents and no edges — including pods that mount no claim, which the build loads for exactly this purpose (see "Application roots recover their pods before the pod read") — so a stateless Application returns its pods rather than an empty body. A claim carrying a root Application SHALL NOT be materialised on its own: it participates in path retention only, and appears only on a retained path. An Application no loaded pod resolves is not drawn.

#### Scenario: Aggregate with no claims still shows

- **WHEN** a client sends `?aggr=aggr9` and Harvest reports `aggr9` on `ontap-prod-02` but no loaded claim joins it
- **THEN** the body contains `netapp/ontap-prod/aggr/aggr9` (parent `netapp/ontap-prod/ontap-prod-02`, which is also present) and no edge

#### Scenario: Pod with no NetApp-backed claim still shows

- **WHEN** a client sends `?pod=shop/web-0` and that pod mounts no claim that joins the Harvest topology
- **THEN** the body contains the pod node (with its namespace / application / controller groups) and no edge

#### Scenario: Stateless Application still shows

- **WHEN** a client sends `?az=zone-a&env=prod&application=checkout` and the three pods resolving `checkout` mount no claim
- **THEN** the body contains the three pods, each with `data.application="checkout"`, their controller, application, namespace and cluster groups, and no edge

#### Scenario: Unknown root is not drawn

- **WHEN** a client sends `?aggr=typo` and no Harvest series in the window carries `aggr="typo"`
- **THEN** the server returns 200 with empty `nodes` and `edges` and an empty `clusters` array

#### Scenario: Unknown Application is not drawn

- **WHEN** a client sends `?application=typo` and no loaded pod resolves that Application
- **THEN** the server returns 200 with empty `nodes` and `edges` and an empty `clusters` array

### Requirement: Attributes and compound groups carry over

Every retained real node SHALL carry the same `data` attributes it carries in `/v1/graph` — including `ipaddress`, `owner`, `application`, `ready_status`, `health`, `usage`, `storageclass`, `hardware`, `perf`, `alerts` and `status` — and the body SHALL include the same synthesised compound groups with the same `data.parent` rules (`cluster > namespace > application > controller > pod`, `cluster > namespace > [application >] pvc`, `cluster > node`, `storage-cluster > netapp-node > netapp-aggr`, `storage-cluster > netapp-svm`). Namespace and ArgoCD Application SHALL NOT be tiers of the flow; a consumer derives a namespace- or Application-level Sankey by walking `data.parent` and summing the conserved weights.

A storage-graph pod SHALL NOT carry `data.containers`: the storage build does not read the container family (see "Storage build reads only what the body draws"), and a storage-flow consumer has no use for a container list. This is the ONE attribute on which the two bodies differ for the same pod.

#### Scenario: Pod keeps its attributes and groups

- **WHEN** a retained pod carries `data.application="checkout"` and `data.owner={kind:"StatefulSet", name:"orders"}` in `/v1/graph`
- **THEN** the storage-graph body carries the same attributes on that pod and its `data.parent` names the same controller group, whose ancestry reaches `cluster/<cluster>`

#### Scenario: No namespace or application tier

- **WHEN** any storage-graph body is inspected
- **THEN** no `storage-flow` edge has a `namespace` or `application` group node as source or target

#### Scenario: Pod carries no containers

- **WHEN** a retained pod carries `data.containers` in `/v1/graph`
- **THEN** the storage-graph body's node for that pod has no `containers` field, while its `owner`, `application`, `status` and `data.parent` are unchanged

### Requirement: Application roots compose with the Harvest restriction

An `application=` root SHALL NOT disable the restricted volume-label read of "Storage-side roots restrict the Harvest topology read": it is a workload-side root, the projection ANDs the workload roots with the storage roots, so every path an `ontap_cluster=` / `aggr=` restriction keeps is one the application root can still retain, and the body is byte-identical to the unrestricted body exactly as it is for `pod=`.

#### Scenario: An application root composes with an aggregate root

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00&application=checkout`
- **THEN** phase 1 of the volume-label read is restricted to `aggr=~"aggr00"`, the application recovery runs as it does without the aggregate root, the body holds only the `checkout` paths on `aggr00`, and it is byte-identical to the body an unrestricted volume-label read produces

## REMOVED Requirements

### Requirement: Storage build reads only what it draws

**Reason**: Its chunk error classes let `kube_replicaset_annotations` and `kube_job_annotations` degrade inside a storage build, and its scenario "A degrading controller chunk degrades and suppresses the hop" pins that. The storage build now fails closed, so the requirement is replaced under a new name without that scenario; every other clause is carried over verbatim.

**Migration**: Replaced by "Storage build reads only what the body draws"; no client action beyond handling 502 `upstream` where a degraded 200 was returned before.

### Requirement: Application roots recover their pods upstream

**Reason**: Its error classes let stage-1 `kube_replicaset_annotations` and `kube_job_annotations` chunks degrade, pinned by the scenario "A degrading stage-1 family degrades alone". The storage build now fails closed, so the requirement is replaced under a new name without that scenario; every other clause is carried over verbatim.

**Migration**: Replaced by "Application roots recover their pods before the pod read"; no client action beyond handling 502 `upstream` where a degraded 200 was returned before.

### Requirement: Storage-side roots narrow the Harvest topology read

**Reason**: Its mechanics let a failed `volume_labels` chunk degrade to an empty vector, pinned by the scenario "A failed restricted chunk degrades". The storage build now fails closed, so the requirement is replaced under a new name without that scenario; every other clause is carried over verbatim.

**Migration**: Replaced by "Storage-side roots restrict the Harvest topology read"; no client action beyond handling 502 `upstream` where a degraded 200 was returned before.
