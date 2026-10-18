# Spec Delta

## ADDED Requirements

### Requirement: Storage build reads the application-metadata metric by namespace

When the `app-metadata-source` metric is configured and served, every storage build SHALL read it, after pods and claims are loaded, once per loaded cluster: restricted to that cluster and, on the configured `namespace` label, to the namespaces its loaded pods and claims carry, composed with the request matchers. A namespace SHALL NOT be read in a cluster that did not load it. The restriction is data-derived: chunked, never widened, never rejected.

#### Scenario: Namespaces of pods and claims are read whole

- **WHEN** a storage build loads pods in namespace `shop` and an unmounted root claim in namespace `archive`
- **THEN** one application-metadata read carries that cluster's `cluster` matcher and the configured namespace label restricted to `archive|shop`, and returns every row of those namespaces in that cluster, including rows of pods the build did not load

#### Scenario: A namespace is read only in the cluster that loaded it

- **WHEN** a storage build loads pods in namespace `shop` of cluster `c1` and namespace `pay` of cluster `c2`, and cluster `c2` also has a namespace `shop`
- **THEN** one read carries `cluster="c1"` restricted to `shop`, another carries `cluster="c2"` restricted to `pay`, and no row of `c2`'s `shop` namespace is read

#### Scenario: Pod Application and group metadata come from the read

- **WHEN** the read returns a row naming Application `shop-v2`, owner `team-a` and user `alice` for loaded pod `shop/web-0`
- **THEN** `web-0` carries `data.application="shop-v2"`, the `shop` namespace group carries `data.namespace_owner="team-a"` and the `shop-v2` application group carries `data.deployed_by="alice"`

#### Scenario: No loaded namespace issues no read

- **WHEN** a storage build loads no pod and no claim
- **THEN** no application-metadata query is issued

#### Scenario: Unset or unserved metric issues no read

- **WHEN** the metric is unset, or no backend serves `appmeta`
- **THEN** no application-metadata query is issued and every pod resolves its Application through the controller chain

## MODIFIED Requirements

### Requirement: Application roots are tracked through their controllers and claims

When a `/v1/storage-graph` request's root kind is `application`, the build SHALL track two sets, the first fed by two candidate sources:

- the pods owned by a controller whose ArgoCD tracking-id names one of the root Applications, recovered in the three stages below, together with — when the `app-metadata-source` metric is configured and served — the `(cluster, namespace, pod)` of every application-metadata row whose `app` equals a root value;
- the claims carrying a root Application of their own, read from `kube_persistentvolumeclaim_annotations` restricted on `annotation_argocd_argoproj_io_tracking_id` exactly as stage 1 restricts the controller families.

The claim-binding family SHALL be read restricted to the recovered pods' `(namespace, pod)` pairs — the namespace the recovering `kube_pod_owner` row carries, one query per namespace — keeping only rows whose `(cluster, namespace, pod)` is a recovered pod, so a same-named pod elsewhere contributes no claim. The claims those rows bind, together with the own-annotated claims, SHALL form the tracked claim set of "Every root kind is tracked from its own tier to its claims". Every mounter of a tracked claim is then loaded by the mounter completion of "The tracked claims expand to both ends of the chain".

The recovery is a **candidate generator, never a judge**: a recovered pod is read exactly like any other scoped pod, its Application is resolved by the same controller-annotation rules every pod's is, and whether it is a root is decided solely by the projection over that resolved value. A recovered pod whose resolved Application is not a root value — a controller whose lexically-smallest tracking-id in the window names a different Application — is loaded and then dropped, so the body is a pure function of the forward resolution and never of the recovery.

The recovery SHALL run in three stages, each waiting on the previous alone. Stage 1 is derived from the request, so nothing precedes it:

**Stage 1 — controllers by tracking-id.** Each of the six controller-annotation families (`kube_deployment_annotations`, `kube_statefulset_annotations`, `kube_daemonset_annotations`, `kube_replicaset_annotations`, `kube_job_annotations`, `kube_cronjob_annotations`) SHALL be issued restricted on `annotation_argocd_argoproj_io_tracking_id` to exactly the values whose segment before the first `:` is one of the root values (a value with no `:` matches when it equals a root value verbatim), composed with the family's fixed selector and the request-scoped matchers — never replacing either. Root values SHALL be sorted, de-duplicated and escaped so a value carrying a regex metacharacter matches itself and nothing else. The stage yields one name set per kind from each family's identity label (`deployment`, `statefulset`, `daemonset`, `replicaset`, `job_name`, `cronjob`).

**Stage 2 — the reverse hops.** `kube_replicaset_owner` SHALL be issued restricted to `owner_kind="Deployment"` and `owner_name` in the stage-1 Deployment names, yielding ReplicaSet names. `kube_job_owner` SHALL be issued restricted on `owner_name` to the stage-1 CronJob names beside its fixed selector, yielding Job names. A stage-1 name set that is empty SHALL issue no query for its hop.

**Stage 3 — pods by owner.** `kube_pod_owner` SHALL be issued restricted to `owner_is_controller="true"`, one query per owner kind whose name set is non-empty:

- `ReplicaSet` — stage-1 ReplicaSet names ∪ stage-2 ReplicaSet names;
- `Job` — stage-1 Job names ∪ stage-2 Job names;
- `StatefulSet` and `DaemonSet` — their stage-1 names;
- `Deployment` and `CronJob` — their stage-1 names, for a pod directly owned by either.

The recovered `pod` labels are the recovered pods.

Every restriction SHALL be chunked deterministically under the shared byte budget, one query per chunk, results merged in chunk order, each chunk issued under the bare family name. A chunk error on any recovery family — the six controller-annotation families, `kube_persistentvolumeclaim_annotations`, `kube_replicaset_owner`, `kube_job_owner` and `kube_pod_owner` — SHALL fail the build, as "Storage build fails closed on upstream query errors" requires. Caller-originated cancellation fails the request whatever the family.

**Application-metadata seed.** When the metric is configured and served, one application-metadata query SHALL be issued in parallel with stage 1, restricted on the configured `app` label to the sorted, de-duplicated, escaped root values (an exact match — the value is not a tracking-id), composed with the request-scoped matchers and chunked under the shared byte budget. Its pods SHALL be unioned with stage 3's before the claim-binding and pod reads. Like the recovery it is a candidate generator: a seeded pod is a root only if its resolved Application is a root value. A chunk error SHALL fail the build.

**Stage 1, the claim-annotation read and the application-metadata seed are request-derived.** When any of them would take more than the fixed maximum number of queries, the request SHALL be rejected 400 with `reason: "invalid_scope"` before any upstream query is issued, as "Every root kind is tracked from its own tier to its claims" requires of every root set. Stages 2 and 3 are derived from upstream data and SHALL NOT be bounded.

**Tally.** Series a stage returns SHALL be counted under the family's name in the build's per-family tally, added to what the by-reference read of the same family contributes.

The tracked set is output-preserving:

- a pod can be a root only if it resolves a root Application, and every such pod is recovered;
- a path is retained only through a pod hit, whose pod is recovered and whose claims are therefore tracked;
- or through a claim hit, where the claim is own-annotated (read by tracking-id) or inherits the Application from a mounter that is recovered.

An empty stage yields nothing downstream. When no controller and no claim carries a root Application, no binding, pod or later query is issued.

#### Scenario: A Deployment-managed stateless pod is recovered and drawn

- **WHEN** a client sends `?az=zone-a&env=prod&application=checkout`, Deployment `shop/web` carries tracking-id `checkout:apps/Deployment:shop/web`, `kube_replicaset_owner` names ReplicaSet `web-7d9f` as owned by Deployment `web`, `kube_pod_owner` names pod `web-7d9f-abc` as controlled by that ReplicaSet, and the pod mounts no claim
- **THEN** stage 1 issues the six annotation families each with a matcher on `annotation_argocd_argoproj_io_tracking_id` admitting exactly the values whose segment before the first `:` is `checkout`, alongside `annotation_argocd_argoproj_io_tracking_id!=""` and `<az-key>="zone-a",<env-key>="prod"`; stage 2 issues `kube_replicaset_owner` restricted to `owner_kind="Deployment"` and `owner_name` in `{web}` and no `kube_job_owner`; stage 3 issues `kube_pod_owner` restricted to `owner_is_controller="true"`, `owner_kind="ReplicaSet"` and `owner_name` in `{web-7d9f}`; the pod read restricts `pod` to `{web-7d9f-abc}` plus every mounter of a tracked claim; and the body contains `web-7d9f-abc` with `data.application="checkout"`, its compound parents, and no edge

#### Scenario: A CronJob-managed pod is recovered through the reverse Job hop

- **WHEN** a client sends `?application=reports`, CronJob `batch/nightly` carries tracking-id `reports:batch/CronJob:batch/nightly`, `kube_job_owner` names Job `nightly-28901` as controlled by it, and `kube_pod_owner` names pod `nightly-28901-x` as controlled by that Job
- **THEN** stage 2 issues `kube_job_owner` with `owner_kind="CronJob",owner_is_controller="true"` and `owner_name` in `{nightly}`, stage 3 issues `kube_pod_owner` for kind `Job` with `owner_name` in `{nightly-28901}`, and the body contains `nightly-28901-x` with `data.application="reports"` and `data.owner={kind:"Job", name:"nightly-28901"}`

#### Scenario: An over-admitted pod is loaded and dropped

- **WHEN** a client sends `?application=beta`, and StatefulSet `shop/db` carries two tracking-ids in the window, `alpha:apps/StatefulSet:shop/db` and `beta:apps/StatefulSet:shop/db`
- **THEN** stage 1 admits `db`, its pods are read, each resolves `data.application="alpha"` (the lexically-smallest tracking-id), none is a root, and the body is byte-identical to the body of the same request against an estate where `db` carries only the `alpha` tracking-id

#### Scenario: No controller or claim carries the Application

- **WHEN** a client sends `?application=typo`, no controller-annotation or claim-annotation series in the selected estate carries a tracking-id naming `typo`, and no application-metadata row carries `app="typo"`
- **THEN** stage 1, the claim-annotation read and the application-metadata seed issue their queries, no stage-2, stage-3, claim-binding or pod query is issued, and the body is empty

#### Scenario: Only the recovered pods' bindings are read

- **WHEN** a client sends `?application=checkout`, the recovery returns `orders-0`, and the estate also holds pods `catalog-0` (claim `catalog-data`, unannotated, mounted by nobody else) and `ledger-0` (claim `ledger-data`, own-annotated `billing:…`)
- **THEN** the claim-binding query by pod is restricted to `namespace="shop",pod="orders-0"`, `catalog-0` and `ledger-0` are never read, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: A same-named pod in another namespace contributes no claim

- **WHEN** a client sends `?application=checkout`, the recovery returns `shop/web-0`, and `platform/web-0`, owned by a controller carrying no root Application, mounts claim `platform/platform-data`
- **THEN** the claim-binding query by pod carries `namespace="shop",pod="web-0"`, `platform-data` is never tracked, and no claim-side, pod, node or Harvest query is issued on its account

#### Scenario: Every mounter of a tracked claim is read

- **WHEN** a client sends `?application=checkout`, the recovery returns `orders-0`, and the RWX claim `shared-data` is mounted by `orders-0` and by `report-0`, whose controller resolves `alpha`
- **THEN** the pod scope is `{orders-0, report-0}`, the claim inherits `alpha` (the lexically-smallest mounter Application) so `report-0`'s path is not retained, and the `pvc-pod` edge to `orders-0` carries half the claim's figures with `labels.attribution="split"` — exactly as when every binding pod is read

#### Scenario: A co-mounter that sorts after the root is drawn through inheritance

- **WHEN** the same claim is instead mounted by `orders-0` (`checkout`) and `zeta-0` (`zeta`)
- **THEN** the claim inherits `checkout`, both `pvc-pod` paths are retained, and `zeta-0` is present as part of the claim's path with a split edge of its own

#### Scenario: A claim annotated with the Application is tracked without its pods

- **WHEN** a client sends `?application=billing`, claim `shop/ledger-data` carries tracking-id `billing:…`, and its only mounter `shop/ledger-0` resolves `ledger`
- **THEN** the claim-annotation read tracks `shop/ledger-data`, mounter completion loads `shop/ledger-0`, and the claim's complete path is retained

#### Scenario: A required stage failure fails the build

- **WHEN** a stage-3 `kube_pod_owner` chunk fails with an upstream error
- **THEN** the build returns an error and the request is mapped as an upstream failure, exactly as an unrestricted `kube_pod_owner` failure is

#### Scenario: An over-large Application set is rejected

- **WHEN** a client sends thousands of `application=` values, enough that a stage-1 family's restriction would take more queries than the maximum
- **THEN** the server returns 400 with `reason: "invalid_scope"`, no upstream query is issued, and no family is read across the zone in its place

#### Scenario: The namespace filter narrows the recovery

- **WHEN** a client sends `?application=checkout&namespace=shop` and `checkout` owns controllers in `shop` and `platform`
- **THEN** every stage-1, stage-2 and stage-3 query and the claim-annotation query carry `namespace="shop"`, only the `shop` pods and claims are tracked, and the body holds no `platform` pod

#### Scenario: A request with another root kind issues no recovery

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1`
- **THEN** no query restricted on `annotation_argocd_argoproj_io_tracking_id` or on `owner_name`, and no application-metadata query restricted on the `app` label, is issued

#### Scenario: The recovery is tallied under the family name

- **WHEN** a client sends `?application=checkout`, stage 1 returns two `kube_deployment_annotations` series, and the by-reference controller read later returns three for the loaded pods' Deployments
- **THEN** the per-family tally carries `kube_deployment_annotations` with `5`, and a family only the recovery issued is present with the count of series its restriction matched

#### Scenario: A stage-1 annotation chunk failure fails the build

- **WHEN** a stage-1 `kube_job_annotations` chunk fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming `kube_job_annotations`, and no body is returned

#### Scenario: A pod named only by the metric is seeded and drawn

- **WHEN** the metric is configured, a client sends `?application=canary-app`, no controller carries a tracking-id naming `canary-app`, and an application-metadata row carries `app="canary-app"` for pod `shop/canary-0` (owned by a `Rollout`), which mounts claim `shop/canary-data`
- **THEN** the seed returns `shop/canary-0`, its claim is tracked, and the body contains `canary-0` with `data.application="canary-app"` and its complete storage path

#### Scenario: A seeded pod whose resolved Application differs is dropped

- **WHEN** a client sends `?application=x`, and pod `shop/web-0` has application-metadata rows with `app` values `x` and `w` in the window
- **THEN** the seed loads `web-0`, it resolves `data.application="w"` (the lexically-smallest value), it is not a root, and the body equals the same request against an estate where its only row carries `app="w"`

#### Scenario: Unserved metric issues no seed

- **WHEN** the metric is configured but no backend serves `appmeta`, and a client sends `?application=checkout`
- **THEN** no application-metadata query is issued and the body equals that of a deployment with the metric unset

### Requirement: Attributes and compound groups carry over

Every retained real node SHALL carry the same `data` attributes it carries in `/v1/graph` — including `ipaddress`, `owner`, `application`, `ready_status`, `health`, `usage`, `storageclass`, `qos`, `hardware`, `perf`, `alerts` and `status` — and the body SHALL include the same synthesised compound groups with the same `data.parent` rules and the same `namespace_owner` / `deployed_by` values (graph-api "Namespace and application group metadata") (`cluster > namespace > application > controller > pod`, `cluster > namespace > [application >] pvc`, `cluster > node`, `storage-cluster > netapp-node > netapp-aggr`, `storage-cluster > netapp-svm`). Namespace and ArgoCD Application SHALL NOT be tiers of the flow; a consumer derives a namespace- or Application-level Sankey by walking `data.parent` and summing the conserved weights.

A PVC's `qos` attribute is independent of its flow: it SHALL be carried whenever the claim's ceiling resolved, including when the claim's `svm-pvc` edge carries no `metrics` (no flow and no latency to report) and when the claim enters the chain at the SVM (the FlexGroup shape). Whenever the `svm-pvc` edge carries `max_iops` / `max_bytes_per_sec`, the PVC's `data.qos` carries the same values.

A storage-graph pod SHALL NOT carry `data.containers`: the storage build does not read the container family (see "Storage build reads every family by reference"), and a storage-flow consumer has no use for a container list. This is the ONE attribute on which the two bodies differ for the same pod.

#### Scenario: Pod keeps its attributes and groups

- **WHEN** a retained pod carries `data.application="checkout"` and `data.owner={kind:"StatefulSet", name:"orders"}` in `/v1/graph`
- **THEN** the storage-graph body carries the same attributes on that pod and its `data.parent` names the same controller group, whose ancestry reaches `cluster/<cluster>`

#### Scenario: No namespace or application tier

- **WHEN** any storage-graph body is inspected
- **THEN** no `storage-flow` edge has a `namespace` or `application` group node as source or target

#### Scenario: Pod carries no containers

- **WHEN** a retained pod carries `data.containers` in `/v1/graph`
- **THEN** the storage-graph body's node for that pod has no `containers` field, while its `owner`, `application`, `status` and `data.parent` are unchanged

#### Scenario: PVC keeps its ceiling when its claim-level edge is unweighted

- **WHEN** a retained claim's ceiling resolved to policy group `gold-tier` with `max_iops: 5000`, and its `svm-pvc` edge carries no `metrics` because the claim reported neither flow nor latency in the window
- **THEN** the PVC node carries `data.qos: {"policy_group": "gold-tier", "max_iops": 5000}` and the `svm-pvc` edge has no `metrics` key

#### Scenario: FlexGroup PVC carries its ceiling in the storage graph

- **WHEN** a retained claim enters the chain at `netapp/ontap-prod/svm/svm_big` (no aggregate) and its ceiling resolved
- **THEN** the PVC node carries `data.qos` with the resolved fields, exactly as it does in `/v1/graph`

#### Scenario: Group metadata matches the graph endpoint

- **WHEN** `/v1/graph` and `/v1/storage-graph` for the same window and zone both render namespace group `us-dev-c1/namespace/shop` and application group `us-dev-c1/namespace/shop/application/checkout`
- **THEN** both bodies carry the same `data.namespace_owner` on the namespace group and the same `data.deployed_by` on the application group

### Requirement: Storage build fails closed on upstream query errors

A `/v1/storage-graph` build SHALL fail when ANY upstream query it issues returns an error — whatever the family, and whichever chunk, phase, stage or backend of that family failed — with the single exception of `ALERTS`. This covers every family `/v1/graph` reads as OPTIONAL or degrading: `volume_labels` (every read), the six `qos_*` workload families (every chunk), the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, `node_total_data`, `kubelet_volume_stats_used_bytes`, `kubelet_volume_stats_capacity_bytes`, `kube_replicaset_annotations` and `kube_job_annotations` (by-reference read and application recovery alike), and `app_metadata` (the namespace-restricted read and the application seed alike). The per-family log-and-continue, per-chunk degrade and degraded-family hop-suppression rules of the `netapp-storage-graph` and `cluster-topology-source` capabilities SHALL apply to `/v1/graph` builds only. A storage body with a silently missing family is indistinguishable from a smaller estate — a filer with no flow, an aggregate with no I/O — so the endpoint SHALL NOT return one.

The failure SHALL be mapped exactly as a required-leg failure is: HTTP 502 with `reason: "upstream"`. The `message` SHALL name the family whose query failed (`upstream query failed: <family>`, the bare family name) and SHALL NOT carry an upstream URL, host, address or credential. A build that exceeds `--build-timeout` SHALL still map to 504 `timeout`, and caller cancellation to the existing `canceled` mapping. Every failed query SHALL still be logged server-side with its full error and counted on `kube_state_graph_upstream_query_failures_total{query}`. The in-process storage-graph engine surface SHALL return the same typed upstream error, carrying the family name.

`ALERTS` SHALL stay OPTIONAL: a failed `ALERTS` query is logged and counted, the build continues with no alert attached to any node, and every node's `status` is folded from its remaining signals.

The following SHALL NOT fail the build, because no query failed: a query that returns an empty vector (a family the deployment does not export, an annotation family that is not allowlisted, a scope that matched nothing); a requested zone that no backend serving the family declares (the existing empty-vector-and-warning rule); and a query the build does not issue.

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

- **WHEN** a client sends `?az=zone-c&env=prod&aggr=aggr1` and no backend serving `harvest` declares `zone-c`
- **THEN** the Harvest legs return empty vectors, the existing warning is logged, and the server returns 200

#### Scenario: The graph endpoint keeps degrading

- **WHEN** a client sends `GET /v1/graph` and the `volume_labels` query fails with an upstream error
- **THEN** the server returns 200 exactly as before this requirement, with no `pvc-to-netapp-aggr` edge

#### Scenario: An application-metadata failure fails the storage request

- **WHEN** the metric is configured and a chunk of the namespace-restricted application-metadata read fails
- **THEN** the server returns 502 with `reason: "upstream"` and `message: "upstream query failed: app_metadata"`, naming neither the configured metric nor a backend URL
