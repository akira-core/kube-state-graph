## MODIFIED Requirements

### Requirement: The tracked claims expand to both ends of the chain

From the tracked claim set, the build SHALL read everything the body draws for those claims, restricted by reference, with four **completions** that keep each reader's whole-population rules intact. The body SHALL be **byte-identical** to the body an unrestricted read of the same zone produces under the same projection, for every root kind.

#### Scenario: A clone on a lexically-smaller aggregate keeps its pick

- **WHEN** a claim's derived token matches FlexVol `trident_pvc_x` on `aggr09` and a clone `snap_trident_pvc_x` on `aggr00`, and the request roots at `aggr=aggr09`
- **THEN** candidate completion loads both series, the aggregate pick resolves to `aggr00` exactly as an unrestricted read resolves it, the claim is not retained by the `aggr09` root, and the body is byte-identical to the unrestricted body

#### Scenario: A cross-filer FlexVol-name collision keeps its pick

- **WHEN** one FlexVol name exists on `ontap-prod` and on `ontap-lab`, the claim's token matches both, and the request is `?pod=` on a pod mounting that claim
- **THEN** candidate completion loads both series, the pick resolves to the lexically-smallest `(ontap-cluster, aggr)` exactly as an unrestricted read resolves it, and the body is byte-identical to the unrestricted body

#### Scenario: Takeover ownership survives a tracked read

- **WHEN** a request reaches a claim on `aggr09`, whose volume-label series disagree on the owning `node` because a takeover happened inside the window
- **THEN** owner completion loads every `aggr09` series, the vote resolves to the lexically-smallest non-empty `node` exactly as an unrestricted read resolves it, and the `node-aggr` tier names that controller

#### Scenario: Owner completion under an SVM root

- **WHEN** a client roots at `svm=svm_shop`, the `svm_shop` volumes on `aggr09` all name controller `ontap-prod-02`, and another SVM's volume on `aggr09` names `ontap-prod-01`
- **THEN** owner completion loads every `aggr09` series, the vote resolves to `ontap-prod-01`, and the `node-aggr` tier names that controller

#### Scenario: A shared claim keeps its split across nodes

- **WHEN** a client sends `?az=zone-a&env=prod&node=worker-1` and the RWX claim `shop/shared-data` is mounted by `orders-0` on `worker-1` and by `report-0` and `report-1` on `worker-2`
- **THEN** the claim-binding read by `(namespace, claim)` loads all three mounters, the body retains only the `orders-0` path, its `pvc-pod` edge carries one third of the claim's figures with `labels.attribution="split"`, and the body is byte-identical to the unrestricted body

#### Scenario: The QoS scope follows the tracked claims

- **WHEN** a build's tracked claims match 60 FlexVols, against 1200 an unrestricted build would match
- **THEN** the QoS workload queries restrict `volume` to those 60 names, and every I/O measurement on a retained path is identical to the unrestricted build's

#### Scenario: A large tracked set is chunked, never read across the zone

- **WHEN** a client roots at `ontap_cluster=` on a filer whose candidates would take many more claim-info queries than the concurrency bound
- **THEN** `kube_persistentvolumeclaim_info` is issued in as many chunks as the byte budget requires, merged in chunk order, and no query without a `volumename` restriction is issued

#### Scenario: An empty tracked set issues no expansion

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1` and no candidate names a claim
- **THEN** no claim-binding, pod, Kubernetes-node, controller, QoS or fixed-policy query is issued, and the body contains `aggr1` and its owning controller only

#### Scenario: A failed chunk of any read fails the build

- **WHEN** one chunk of a root, candidate-completion, owner-completion or claim-side read fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming that family, the failure is counted on `kube_state_graph_upstream_query_failures_total`, and no body is returned

#### Scenario: The claim side is read by namespace and claim

- **WHEN** the tracked claim set is non-empty
- **THEN** `kube_persistentvolumeclaim_info` (when the root's reads did not already return the claim), `kube_persistentvolumeclaim_annotations`, the two kubelet volume-stats families and the claim-binding family are each restricted to the tracked `(namespace, claim)` pairs — grouped across namespaces, one query per chunk carrying the chunk's namespaces as an alternation on `namespace` and its tracked claim names as an alternation on `persistentvolumeclaim` — so a claim name is never read outside the namespaces that track it
- **AND** rows are kept only when their `(az, env, cluster, namespace, claim)` names a tracked claim, so a cross pair the two alternations admit — a tracked name in a namespace that tracks another claim — and a same-named claim of another cluster are discarded before any later read's restriction is computed from them, and still counted in the family's tally

#### Scenario: Claims in many namespaces share one query

- **WHEN** the tracked claims are `shop/orders-data` and `platform/redis-data`
- **THEN** each claim-keyed family is issued as one query restricted to `namespace=~"platform|shop"` and `persistentvolumeclaim=~"orders-data|redis-data"` beside the request matchers, not one query per namespace
- **AND** a node root whose 40 mounting pods live in 40 namespaces issues each claim-keyed family once, where a per-namespace read issued it 40 times, and the body is byte-identical

#### Scenario: Mounter completion loads every mounting pod

- **WHEN** the claim-binding read by `(namespace, claim)` names the pods mounting a tracked claim
- **THEN** EVERY such pod is loaded, because a shared claim's `pvc-pod` weight is split over the mounters present in the built graph and an unannotated claim inherits the Application of its mounters, so a mounter left unloaded would change both

#### Scenario: The workload side follows the mounters

- **WHEN** the mounters are known
- **THEN** their pods, their Kubernetes nodes and their controllers are read as "Storage build reads every family by reference" defines

#### Scenario: Candidate completion reads every FlexVol a claim can match

- **WHEN** the tracked claims are known
- **THEN** `volume_labels` is read restricted on `volume` to the derived tokens of every tracked claim, rendered as the suffix comparison `.*<token>`, so each claim's aggregate and SVM picks run over its WHOLE candidate set — a clone or a same-named FlexVol on another filer included
- **AND** its rows do not add claims to the tracked set

#### Scenario: Owner completion reads every touched aggregate whole

- **WHEN** any volume-label row of the build names an `(ONTAP cluster, aggregate)` pair
- **THEN** that aggregate is read whole, except an aggregate a read of the build already covered whole, so the owning-controller vote runs over every row of every aggregate the body can draw
- **AND** under an `svm` root this includes the aggregates candidate completion alone named, because the aggregate and SVM picks are separate and a claim retained through its SVM can land on such an aggregate
- **AND** its rows do not add claims

#### Scenario: The storage side is read last

- **WHEN** the volume-label reads are complete
- **THEN** the aggregate and controller gauge families, the QoS workload families and the fixed-policy families are read as "Storage build reads every family by reference" defines

#### Scenario: Volume-label reads are merged before the parse

- **WHEN** a build issued several volume-label reads
- **THEN** they are merged, de-duplicated by label set, before the parse, and every downstream consumer — the aggregate, SVM and owner picks, the inventory and the QoS `volume` scope — runs over the merged result

#### Scenario: The one permitted divergence from an unrestricted read

- **WHEN** an aggregate or controller is named by the volume-label family alone, with no gauge series of its own, outside what the build reached
- **THEN** that entity is not materialised, which can change whether an alert without a `cluster` label matches a unique entity; this is the only permitted divergence from the unrestricted body, and the stock Harvest templates name every one

#### Scenario: Data-derived scopes are chunked, never widened

- **WHEN** a scope derived from upstream data — tracked claims, mounters, touched aggregates, controllers, tokens — is read
- **THEN** it is chunked under the shared byte budget, issued with bounded concurrency and merged in chunk order however large it is, and it is NEVER replaced by a read across the zone
- **AND** an empty tracked set issues no claim-side, workload-side or storage-side query beyond the root's own reads

#### Scenario: Join coverage counts over the tracked claims

- **WHEN** the build reports the join-coverage signal of the `netapp-storage-graph` capability
- **THEN** it counts over the tracked claims exactly as `/v1/graph` counts over its loaded claims

### Requirement: Storage build reads every family by reference

The storage build SHALL NOT issue `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels` or `kube_service_annotations`. It SHALL read `ALERTS` under the request-scoped matchers alone, and every other family **by reference**: restricted to the object names the request's root or the reads before it actually carry.

#### Scenario: Skipped families never reach the upstream

- **WHEN** the upstream rejects every `kube_pod_container_info` query with a series-limit error and a client sends any valid `/v1/storage-graph` request
- **THEN** the storage build issues no `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels` or `kube_service_annotations` query, `kube_state_graph_upstream_query_failures_total` does not move, and the request returns 200

#### Scenario: Only ALERTS is read across the zone

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0`
- **THEN** every issued query except `ALERTS` carries a restriction on its family's identity label beyond the request-scoped matchers its family carries (`<az-key>="zone-a",<env-key>="prod"` on a kube-state-metrics or kubelet family, none on a Harvest family), and `ALERTS` carries the request-scoped matchers alone

#### Scenario: Harvest gauges are read for the reached components only

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0`, the pod's claim resolves to `aggr1` on `ontap-prod`, owned by `ontap-prod-01`, in SVM `svm_shop`, and the filer holds 200 aggregates
- **THEN** the aggregate gauge queries carry `cluster="ontap-prod"` and `aggr=~"aggr1"`, the controller queries carry `cluster="ontap-prod"` and `node=~"ontap-prod-01"`, the fixed-policy queries carry `cluster="ontap-prod"` and `svm=~"svm_shop"`, and no other aggregate's or controller's series is fetched

#### Scenario: A rooted aggregate with no claim is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00`, no volume on `aggr00` matches any claim, and the aggregate and controller families name `aggr00` and its owner
- **THEN** the aggregate gauges are read for `aggr00` by name, the controller families for its owner, and the body contains `aggr00` with its health and usage attributes and its owning controller, and no path through them

#### Scenario: Cross-namespace name collision is harmless

- **WHEN** claim-binding series name `shop/web-0` and the estate also holds a claimless `platform/web-0`
- **THEN** the pod read carries `namespace="shop",pod="web-0"`, `platform/web-0` is never fetched, and the body is byte-identical to the body an unrestricted pod read would produce

#### Scenario: Pods are read one query per namespace

- **WHEN** the pods to read are `shop/orders-0`, `shop/web-0` and `platform/orders-0`
- **THEN** `kube_pod_info` and `kube_pod_owner` are each issued as one query `{namespace=~"platform|shop",pod=~"orders-0|web-0"}` beside the request matchers, not one query per namespace
- **AND** a returned `platform/web-0` row — a cross pair the two independent alternations admit — is discarded before the node, controller or claim reads are scoped from the result, and is still counted in the family's tally

#### Scenario: A same-named pod in another namespace never widens the node or controller read

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/postgres-0` and `platform/postgres-0`, owned by StatefulSet `pg-other`, runs on `worker-9`
- **THEN** no `kube_pod_info` or `kube_pod_owner` query admits `platform/postgres-0`, no Kubernetes-node query names `worker-9`, and no controller query names `pg-other`

#### Scenario: A pod chunk failure fails the build

- **WHEN** the pod restriction is split into three chunks and the second `kube_pod_info` chunk fails with an upstream error
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

#### Scenario: Node read is restricted to the loaded pods' nodes and node roots

- **WHEN** a client sends `?az=zone-a&env=prod&node=n9`, the pods on `n9` mount a claim also mounted by a pod on `n1`, and the estate holds 5000 nodes
- **THEN** every issued `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels` and `kube_node_status_condition` query restricts `node` to exactly `{n1, n9}` alongside its fixed selector and the request's matchers, no series for any other node is fetched, and the body is byte-identical to the body an unrestricted node read would produce

#### Scenario: Kubernetes node root with no mounting pod is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&node=n9`, no pod on `n9` mounts a claim, and `kube_node_info` names Kubernetes node `n9`
- **THEN** the four node families are issued restricted to `{n9}`, no claim-side or controller query is issued, and the body contains the node `n9` with its ordinary attributes and no edges

#### Scenario: Controller scope cross-namespace collision is harmless

- **WHEN** a loaded pod in `shop` is owned by StatefulSet `db` and the estate also holds StatefulSet `platform/db` carrying a different tracking-id
- **THEN** `kube_statefulset_annotations{statefulset="db"}` may return both series, the pod resolves the `shop/db` Application only, and the body is byte-identical to the body an unrestricted read would produce

#### Scenario: An annotation chunk failure fails the build

- **WHEN** one `kube_job_annotations` or `kube_replicaset_annotations` chunk fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming that family, and no body is returned

#### Scenario: The body carries nothing the skipped families feed

- **WHEN** any storage-graph body is inspected
- **THEN** it carries no `service` or `external` node, no `service-selects-pod` edge and no `containers` attribute
- **AND** `ALERTS` is the only family the build reads across the requested zone

#### Scenario: A by-reference restriction composes with the existing selectors

- **WHEN** a family is read by reference
- **THEN** the restriction is a sorted, de-duplicated, anchored alternation on the family's own identity label, **composed with** the family's fixed, request-invariant selector where it has one and with the request-scoped matchers the family already carries — never replacing either

#### Scenario: Pair-keyed families are read one query per ONTAP cluster

- **WHEN** a family is keyed by an `(ONTAP cluster, name)` pair
- **THEN** it is issued one query per ONTAP cluster, with that cluster as an equality and its names as the alternation, so no row outside the key set is read

#### Scenario: Restrictions are chunked under one shared byte budget

- **WHEN** a restriction is rendered
- **THEN** it is chunked deterministically under one byte budget shared by every data-derived alternation, one query per chunk per family, results merged in chunk order, each chunk issued under the bare family name for self-metrics and span dimensions
- **AND** a single value is always issued, even when it alone exceeds the budget

#### Scenario: A literal alternation holds at most 100 values

- **WHEN** a restriction renders its values as a literal alternation — every by-reference read, both alternations of the grouped pod and claim reads (each counted on its own distinct values), the pair-keyed Harvest reads and the root-derived reads on their identity label
- **THEN** no chunk carries more than 100 values in that alternation, whatever the byte budget admits, so the upstream resolves it by exact value lookup rather than by evaluating a regex against every value of the label
- **AND** the bound holds when the byte budget is unset or non-positive, and the split stays a pure function of the value set, so the body is byte-identical to the one an unbounded split produces
- **AND** the namespace alternation of a `pod` root's seed is taken from the request's roots, like a repeated `namespace` parameter, and is not bounded

#### Scenario: A suffix-token alternation is chunked by bytes alone

- **WHEN** the restricted `volume_labels` read renders FlexVol tokens as suffix branches (`.*<token>`)
- **THEN** it is chunked under the byte budget alone and is NOT split by value count, because the upstream resolves a `.*`-prefixed alternation by regex at any count and a further split would only multiply that evaluation

#### Scenario: An empty scope is neither issued nor tallied

- **WHEN** a by-reference family's scope is empty
- **THEN** it is not issued and is absent from the build's per-family series tally
- **AND** an issued family's tally entry is the total count of series its restrictions matched across every read of the build, except the end-minus-start reads of an `io_delta=true` build ("QoS workload delta" of the `netapp-storage-graph` capability), which are never tallied: a QoS workload family's entry counts its measurement read alone and does not move with `io_delta`

#### Scenario: Chunk errors and cancellation

- **WHEN** a chunk of a by-reference family fails, or the caller cancels the request
- **THEN** the chunk error fails the build, as "Storage build fails closed on upstream query errors" requires, and caller-originated cancellation fails the request whatever the family

#### Scenario: Pods are read by namespace and name

- **WHEN** the build reads pods
- **THEN** `kube_pod_info` and `kube_pod_owner` are restricted to the `(namespace, pod)` pairs of the mounters of the tracked claims, the `pod` roots, the pods a `node` root placed on a root node, and the pods an `application` root recovered — grouped across namespaces, one query per chunk carrying the chunk's namespaces as one alternation and its pod names as another, chunked under the shared byte budget so that neither alternation holds more than 100 distinct values — because a pod is identified by its namespace and name, never by its name alone
- **AND** a row whose `(namespace, pod)` is outside the set — a cross pair the independent alternations admit — is discarded before the next waves are scoped from what this read returns, so a same-named pod in another namespace never reaches the node, controller or claim reads and never reaches the body

#### Scenario: Every read of known pods is pair-keyed

- **WHEN** the build reads a family for pods it already knows
- **THEN** the same `(namespace, pod)` keying applies: to the `node` root's incarnation completion and claim-binding read, and to the `application` root's claim-binding read

#### Scenario: Claimless root pods are still materialised

- **WHEN** a `pod` root mounts no claim, or a recovered pod resolves a root Application
- **THEN** the pod is still materialised

#### Scenario: Kubernetes nodes are read for the loaded pods and node roots

- **WHEN** the build reads Kubernetes nodes
- **THEN** `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels` and `kube_node_status_condition` are restricted on `node` to the nodes of the loaded pods and the `node` roots
- **AND** an unscheduled pod contributes no name

#### Scenario: First controller stage reads the pods' direct owners

- **WHEN** the build reads the controller families for the loaded pods
- **THEN** the eight controller families are read in two stages, each restricted to the owner names the loaded pods carry
- **AND** the first stage groups, from every loaded `kube_pod_owner` series with `owner_is_controller="true"` and a non-empty `owner_name`, the names by `owner_kind`, and issues `kube_replicaset_owner` and `kube_replicaset_annotations` on `replicaset` for the `ReplicaSet` names, `kube_job_owner` and `kube_job_annotations` on `job_name` for the `Job` names, `kube_statefulset_annotations` on `statefulset` for the `StatefulSet` names, and `kube_daemonset_annotations` on `daemonset` for the `DaemonSet` names

#### Scenario: Second controller stage reads the owners' owners

- **WHEN** the first controller stage has returned
- **THEN** the second stage, waiting on the first alone, issues `kube_deployment_annotations` on `deployment` for the union of the pods' direct `Deployment` owner names and the `owner_name` of every loaded `kube_replicaset_owner` series naming a `Deployment` owner
- **AND** `kube_cronjob_annotations` on `cronjob` for the union of the pods' direct `CronJob` owner names and the `owner_name` of every loaded `kube_job_owner` series

#### Scenario: Unpopulated controller scopes issue nothing

- **WHEN** no loaded pod is owned by a kind, a second-stage name set no first-stage series populated is empty, or an owner kind is one the reader resolves no Application for
- **THEN** the kind and the empty second-stage set issue no query, and the unresolved owner kind contributes no name

#### Scenario: Harvest aggregate gauges are read for the reached aggregates

- **WHEN** the build reads `aggr_new_status`, `aggr_space_used` and `aggr_space_total`
- **THEN** they are restricted to the `(ONTAP cluster, aggregate)` pairs the build's volume-label rows name, plus the aggregates an `aggr` root names (by name for a bare value, by `(ONTAP cluster, aggregate)` pair — one query per ONTAP cluster — for a qualified one) or an `ontap_cluster` root holds (by cluster)

#### Scenario: Harvest controller families are read for the reached controllers

- **WHEN** the build reads `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency` and `node_total_data`
- **THEN** they are restricted to the `(ONTAP cluster, controller)` pairs the build's volume-label rows and aggregate gauge rows name, plus an `ontap_node` root (by name) or an `ontap_cluster` root (by cluster)

#### Scenario: Fixed-policy families are keyed on the tracked claims' SVMs

- **WHEN** the build reads the two `qos_policy_fixed_max_throughput_*` families
- **THEN** they are restricted to the `(ONTAP cluster, SVM)` pairs of the tracked claims, which — unlike keying on the policy group — lets them run beside the QoS workload read instead of after it

#### Scenario: QoS workload families follow the scoped read

- **WHEN** the build reads the six QoS workload families
- **THEN** they are restricted as the `netapp-storage-graph` capability's "Scoped and batched QoS workload read" defines

#### Scenario: Readers consult families only inside their restriction

- **WHEN** a reader consults a by-reference family
- **THEN** it does so only at keys inside that family's restriction — a node family at the node of a loaded pod or a root; an owner or annotation family at a loaded pod's owner chain; an aggregate gauge at an aggregate the volume-label rows or a root name; a controller family at an aggregate's owner or a root; a fixed-policy series at a tracked claim's SVM — so the body is byte-identical to the body an unrestricted read would produce

#### Scenario: A same-named object admitted elsewhere leaves the body unchanged

- **WHEN** a restriction admits a same-named object elsewhere inside the request's selectors, since names are unique within a namespace, a cluster or an ONTAP cluster only
- **THEN** that series is keyed under its own cluster, is consulted by no loaded object, and leaves the body unchanged

### Requirement: Application roots are tracked through their controllers and claims

When a `/v1/storage-graph` request's root kind is `application`, the build SHALL track two sets: the pods a three-stage recovery (and, when configured, the application-metadata seed) attributes to the root Applications, and the claims carrying a root Application of their own. The recovery SHALL be a **candidate generator, never a judge**.

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

#### Scenario: The two tracked sets and their sources

- **WHEN** the root kind is `application`
- **THEN** the first set is fed by two candidate sources: the pods owned by a controller whose ArgoCD tracking-id names one of the root Applications, recovered in three stages, together with — when the `app-metadata-source` metric is configured and served — the `(cluster, namespace, pod)` of every application-metadata row whose `app` equals a root value
- **AND** the second set is the claims carrying a root Application of their own, read from `kube_persistentvolumeclaim_annotations` restricted on `annotation_argocd_argoproj_io_tracking_id` exactly as stage 1 restricts the controller families

#### Scenario: Recovered pods' bindings name the tracked claims

- **WHEN** the recovered pods are known
- **THEN** the claim-binding family is read restricted to their `(namespace, pod)` pairs — the namespace the recovering `kube_pod_owner` row carries, grouped across namespaces as every read of known pods is — keeping only rows whose `(cluster, namespace, pod)` is a recovered pod, so a same-named pod elsewhere contributes no claim
- **AND** the claims those rows bind, together with the own-annotated claims, form the tracked claim set of "Every root kind is tracked from its own tier to its claims", and every mounter of a tracked claim is then loaded by the mounter completion of "The tracked claims expand to both ends of the chain"

#### Scenario: The forward resolution alone decides a root

- **WHEN** a pod is recovered
- **THEN** it is read exactly like any other scoped pod, its Application is resolved by the same controller-annotation rules every pod's is, and whether it is a root is decided solely by the projection over that resolved value
- **AND** a recovered pod whose resolved Application is not a root value — a controller whose lexically-smallest tracking-id in the window names a different Application — is loaded and then dropped, so the body is a pure function of the forward resolution and never of the recovery

#### Scenario: The stages are gated in order

- **WHEN** the recovery runs
- **THEN** it runs in three stages, each waiting on the previous alone, and stage 1 is derived from the request, so nothing precedes it

#### Scenario: Stage 1 reads controllers by tracking-id

- **WHEN** stage 1 runs
- **THEN** each of the six controller-annotation families (`kube_deployment_annotations`, `kube_statefulset_annotations`, `kube_daemonset_annotations`, `kube_replicaset_annotations`, `kube_job_annotations`, `kube_cronjob_annotations`) is issued restricted on `annotation_argocd_argoproj_io_tracking_id` to exactly the values whose segment before the first `:` is one of the root values (a value with no `:` matches when it equals a root value verbatim), composed with the family's fixed selector and the request-scoped matchers — never replacing either
- **AND** root values are sorted, de-duplicated and escaped so a value carrying a regex metacharacter matches itself and nothing else
- **AND** the stage yields one name set per kind from each family's identity label (`deployment`, `statefulset`, `daemonset`, `replicaset`, `job_name`, `cronjob`)

#### Scenario: Stage 2 takes the reverse hops

- **WHEN** stage 2 runs
- **THEN** `kube_replicaset_owner` is issued restricted to `owner_kind="Deployment"` and `owner_name` in the stage-1 Deployment names, yielding ReplicaSet names, and `kube_job_owner` is issued restricted on `owner_name` to the stage-1 CronJob names beside its fixed selector, yielding Job names
- **AND** a stage-1 name set that is empty issues no query for its hop

#### Scenario: Stage 3 reads pods by owner

- **WHEN** stage 3 runs
- **THEN** `kube_pod_owner` is issued restricted to `owner_is_controller="true"`, one query per owner kind whose name set is non-empty: `ReplicaSet` for stage-1 ∪ stage-2 ReplicaSet names; `Job` for stage-1 ∪ stage-2 Job names; `StatefulSet` and `DaemonSet` for their stage-1 names; `Deployment` and `CronJob` for their stage-1 names, for a pod directly owned by either
- **AND** the recovered `pod` labels are the recovered pods

#### Scenario: Recovery reads are chunked and fail closed

- **WHEN** a recovery restriction is issued
- **THEN** it is chunked deterministically under the shared byte budget, one query per chunk, results merged in chunk order, each chunk issued under the bare family name
- **AND** a chunk error on any recovery family — the six controller-annotation families, `kube_persistentvolumeclaim_annotations`, `kube_replicaset_owner`, `kube_job_owner` and `kube_pod_owner` — fails the build, as "Storage build fails closed on upstream query errors" requires, and caller-originated cancellation fails the request whatever the family

#### Scenario: The application-metadata seed runs beside stage 1

- **WHEN** the metric is configured and served
- **THEN** one application-metadata query is issued in parallel with stage 1, restricted on the configured `app` label to the sorted, de-duplicated, escaped root values (an exact match — the value is not a tracking-id), composed with the request-scoped matchers and chunked under the shared byte budget, and its pods are unioned with stage 3's before the claim-binding and pod reads
- **AND** like the recovery it is a candidate generator: a seeded pod is a root only if its resolved Application is a root value, and a chunk error fails the build

#### Scenario: Only request-derived reads are bounded

- **WHEN** stage 1, the claim-annotation read or the application-metadata seed would take more than the fixed maximum number of queries
- **THEN** the request is rejected 400 with `reason: "invalid_scope"` before any upstream query is issued, as "Every root kind is tracked from its own tier to its claims" requires of every root set
- **AND** stages 2 and 3, derived from upstream data, are not bounded

#### Scenario: Recovery series are tallied with the by-reference read

- **WHEN** a recovery stage returns series
- **THEN** they are counted under the family's name in the build's per-family tally, added to what the by-reference read of the same family contributes

#### Scenario: The tracked set is output-preserving

- **WHEN** the projection retains a path for an `application` root
- **THEN** a pod can be a root only if it resolves a root Application, and every such pod is recovered
- **AND** a path is retained only through a pod hit, whose pod is recovered and whose claims are therefore tracked, or through a claim hit, where the claim is own-annotated (read by tracking-id) or inherits the Application from a mounter that is recovered

#### Scenario: An empty stage yields nothing downstream

- **WHEN** a stage returns no series
- **THEN** nothing downstream of it is issued on its account, and when no controller and no claim carries a root Application, no binding, pod or later query is issued
