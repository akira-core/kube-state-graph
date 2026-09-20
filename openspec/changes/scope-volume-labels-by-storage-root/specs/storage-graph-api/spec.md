## MODIFIED Requirements

### Requirement: Storage build reads only what it draws

The storage-graph build SHALL NOT issue the following kube-state-metrics families: `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`, `kube_service_annotations`. The body contains no `service` or `external` node and no `service-selects-pod` edge, so the four service-side families can contribute nothing to it, and it does not carry `containers` (see "Attributes and compound groups carry over"). A family the build does not issue SHALL be absent from the build's per-family series tally, never reported as zero.

Every other family the `/v1/graph` topology read issues falls into one of two classes. The **unrestricted** class is read exactly as `/v1/graph` reads it, under the request-scoped matchers alone: the claim-binding family `kube_pod_spec_volumes_persistentvolumeclaims_info` (the root every by-reference scope is computed from — nothing precedes it that could restrict it); `kube_persistentvolumeclaim_info`, `kube_persistentvolumeclaim_annotations` and the two kubelet volume-stats families (one series per claim the binding family already names, so a restriction could not select fewer than the binding read already does); every Harvest family except the volume-label topology family under a storage-side-rooted request (a storage root SHALL be drawable whether or not a claim reaches it, and an SVM is named by `volume_labels` alone; `volume_labels` alone leaves this class when the request names the components it should read, per "Storage-side roots narrow the Harvest topology read"); and `ALERTS` (already restricted to firing alerts). The **by-reference** class — the two pod families, the four Kubernetes-node families and the eight controller families below — is read restricted to the object names the families read before it actually carry. A by-reference family whose scope is empty SHALL NOT be issued at all and SHALL be absent from the tally; when issued, its tally entry is the count of series its restriction matched.

Every by-reference restriction SHALL be a sorted, de-duplicated, anchored alternation on the family's own identity label, **composed with** the family's fixed, request-invariant selector where it has one (`type=~"ExternalIP|InternalIP"`, `condition="Ready"`, `owner_kind="CronJob",owner_is_controller="true"`, `annotation_argocd_argoproj_io_tracking_id!=""`) and with the request-scoped matchers the family already carries — never replacing either. It is derived from upstream data and from the request's roots, not from a selector-level dimension, so the request-scoped selector table is unchanged. Every restriction SHALL be **chunked deterministically** under one byte budget shared with the QoS workload read, one query per chunk per family, results merged in **chunk order**, every chunk issued under the bare family name for self-metrics and span dimensions, and a single name SHALL always be issued even when it alone exceeds the budget. A chunk SHALL keep its family's error class: a chunk of a family whose unrestricted read fails the build (`kube_pod_info`, `kube_pod_owner`, the four Kubernetes-node families, `kube_replicaset_owner`, `kube_job_owner`, `kube_deployment_annotations`, `kube_statefulset_annotations`, `kube_daemonset_annotations`, `kube_cronjob_annotations`) fails the build exactly as that unrestricted error does; a chunk of a family whose unrestricted read degrades (`kube_replicaset_annotations`, `kube_job_annotations`) degrades on its own, costing only the Applications the names in that chunk would have supplied, and a degraded `kube_job_annotations` chunk suppresses the Job → CronJob hop for the whole build exactly as a degraded unrestricted read does. Caller-originated cancellation SHALL fail the request whatever the family.

**Pods.** The build SHALL read `kube_pod_info` and `kube_pod_owner` restricted to the union of (a) the `pod` names of every claim-binding series it loaded and (b) the pod-name segment of every `pod=<namespace>/<pod-name>` root the request carries. The pod read SHALL wait on the claim-binding family alone and SHALL NOT wait on families it does not read. Because a root has to be in the restriction to be loaded, the storage build SHALL receive the request's roots as an input. A `pod=` root that mounts no claim SHALL still be materialised, as "Roots are always materialised when the upstream knows them" requires. When the pod scope is empty — no claim-binding series was loaded and the request carries no `pod=` root — no pod query is issued, no controller query is issued, and the body holds no complete path: it contains only the roots the storage side materialised and any `node=` root the Kubernetes-node read below still draws.

**Kubernetes nodes.** The build SHALL read `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels` and `kube_node_status_condition` restricted on `node` to the union of (a) the `node` label of every pod the pod read loaded and (b) every `node=` root the request carries. The node read SHALL wait on the pod read alone. A `node=` root naming a Kubernetes node that no loaded pod is scheduled on SHALL still be materialised, so a request with node roots and no mounting pod issues the node families for the roots alone. An unscheduled pod contributes no name.

**Controllers.** The build SHALL read the eight controller families in two stages, each restricted to the owner names the loaded pods carry. From every loaded `kube_pod_owner` series with `owner_is_controller="true"` and a non-empty `owner_name`, the first stage groups the names by `owner_kind` and issues, restricted on the family's identity label: `kube_replicaset_owner` and `kube_replicaset_annotations` on `replicaset` for the `ReplicaSet` names; `kube_job_owner` and `kube_job_annotations` on `job_name` for the `Job` names; `kube_statefulset_annotations` on `statefulset` for the `StatefulSet` names; `kube_daemonset_annotations` on `daemonset` for the `DaemonSet` names. The second stage, waiting on the first alone, issues `kube_deployment_annotations` on `deployment` for the union of the pods' direct `Deployment` owner names and the `owner_name` of every loaded `kube_replicaset_owner` series naming a `Deployment` owner, and `kube_cronjob_annotations` on `cronjob` for the union of the pods' direct `CronJob` owner names and the `owner_name` of every loaded `kube_job_owner` series. A kind no loaded pod is owned by, and a second-stage name set no first-stage series populated, SHALL issue no query for its families. The first stage SHALL wait on the pod read alone. An owner kind the reader resolves no Application for (`ReplicationController`, `Node`, a custom-resource controller) contributes no name and issues nothing.

The by-reference reads SHALL be **output-preserving**: the reader consults a node family only at the `(cluster, node)` of a loaded pod's node or a node root, consults `kube_replicaset_owner` only at the ReplicaSet names of loaded pods, consults the annotation families only at the resolved owner of a loaded pod (a Deployment recovered from its ReplicaSet, a CronJob recovered from its Job), and consults `kube_job_owner` only at the Job names of loaded pods — so every consulted key names an object in its scope, and the body SHALL be byte-identical to the body an unrestricted read would produce. Names are unique within a namespace (controllers, pods) or a cluster (nodes) only, so a restriction MAY admit a same-named object from another namespace or cluster inside the request's selectors; such a series is keyed under its own cluster and namespace, is consulted by no loaded pod, and SHALL leave the body unchanged.

#### Scenario: Pod read is restricted to mounting pods and roots

- **WHEN** a build for `?az=zone-a&env=prod&pod=shop/web-0` loads claim-binding series naming pods `orders-0`, `orders-1` and `catalog-0` while the estate holds 40000 pods
- **THEN** every issued `kube_pod_info` and `kube_pod_owner` query restricts `pod` to exactly `{catalog-0, orders-0, orders-1, web-0}` alongside `<az-key>="zone-a",<env-key>="prod"`, no series for any other pod is fetched, and the body is byte-identical to the body an unrestricted pod read would produce

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

#### Scenario: A degrading controller chunk degrades and suppresses the hop

- **WHEN** one `kube_job_annotations` chunk fails with an upstream error while every other query succeeds
- **THEN** the request returns 200, no pod owned by a Job resolves an Application for that build — neither from the Job family nor through its CronJob — every other pod's attributes are unchanged, and the failure is logged and counted on `kube_state_graph_upstream_query_failures_total{query="kube_job_annotations"}`

#### Scenario: Node read is restricted to the pods' nodes and node roots

- **WHEN** a client sends `?az=zone-a&env=prod&node=n9` and the loaded pods are scheduled on Kubernetes nodes `n1` and `n2` while the estate holds 5000 nodes
- **THEN** every issued `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels` and `kube_node_status_condition` query restricts `node` to exactly `{n1, n2, n9}` alongside its fixed selector and the request's matchers, no series for any other node is fetched, and the body is byte-identical to the body an unrestricted node read would produce

#### Scenario: Kubernetes node root with no mounting pod is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&node=n9`, no claim-binding series is loaded, and `kube_node_info` names Kubernetes node `n9`
- **THEN** the four node families are issued restricted to `{n9}`, no pod or controller query is issued, and the body contains the node `n9` with its ordinary attributes and no edges

#### Scenario: Controller scope cross-namespace collision is harmless

- **WHEN** a loaded pod in `shop` is owned by StatefulSet `db` and the estate also holds StatefulSet `platform/db` carrying a different tracking-id
- **THEN** `kube_statefulset_annotations{statefulset="db"}` may return both series, the pod resolves the `shop/db` Application only, and the body is byte-identical to the body an unrestricted read would produce

## ADDED Requirements

### Requirement: Storage-side roots narrow the Harvest topology read

When a `/v1/storage-graph` request carries at least one `ontap_cluster=` or `aggr=` root, the build SHALL read the Harvest volume-label topology family restricted to the rooted components instead of reading the whole filer, and the body SHALL be byte-identical to the body an unrestricted read produces for every estate whose aggregates and controllers are each named by their own Harvest gauge families (the stock `aggr_*` and `node_*` templates). An aggregate or controller named by the volume-label family alone, outside the rooted components, is not materialised by a restricted read; that can change whether an alert without a `cluster` label matches a unique entity, and is the only divergence.

**Phase 1 — the root restriction.** The build SHALL issue the family restricted by the request's `ontap_cluster=` and `aggr=` values: `ontap_cluster=` restricts the family's `cluster` label and `aggr=` restricts its `aggr` label. A request carrying both SHALL issue ONE query carrying both matchers, because the projection combines them as a narrowing — an `aggr=` root names an aggregate only within the `ontap_cluster=` values, so an aggregate of that name on another filer is not a root and its volumes need not be read. Each value set SHALL be rendered as a sorted, de-duplicated, anchored alternation, escaped so a value carrying a regex metacharacter matches itself and nothing else. The restriction is the ONLY matcher this query carries; Harvest takes no request-scoped selector.

The restriction is derived from the request's roots, not from a selector-level dimension, so the request-scoped selector table is unchanged and `az` still reaches Harvest through backend selection alone. A `pod=` root does not prevent it: the projection ANDs the workload roots with the storage roots, so every retained path is one the restriction keeps.

**Phase 2 — candidate recovery.** A claim's aggregate and SVM are picked lexically-smallest over that claim's WHOLE candidate set, so a phase-1-only read could place a claim on a rooted aggregate where an unrestricted read would place it on a lexically-smaller one — a clone whose FlexVol name also matches the claim's derived token, or the same FlexVol name on a second filer. After phase 1, and after the claim-info family has landed, the build SHALL therefore issue a second restricted read of the same family, restricted on `volume` to the derived tokens of exactly the claims phase 1 matched, expressed in the **forward** direction of the configured derivation — the direction the join already computes. Nothing inverts a FlexVol name back to a PersistentVolume name. The two phases' results SHALL be merged and de-duplicated by label set before the parse, and every downstream consumer — the aggregate and SVM picks, the owning-controller vote, the inventory, and the QoS workload read's `volume` scope — SHALL run over the merged result.

When phase 1 matched no claim, phase 2 SHALL NOT be issued.

**Roots stay drawable.** The aggregate, controller and policy Harvest families are unrestricted in every build, so a rooted component with no claim on it is materialised from those families exactly as it is today. A phase 1 that returns nothing therefore still draws its roots; it draws no path through them, which is the same outcome an unrestricted read gives for a component no claim reaches.

**Roots that SHALL disable the restriction.** A request carrying an `svm=` or a `node=` root SHALL read this family unrestricted and single-phase, whatever other roots it carries. The owning controller of an aggregate is a vote over ALL of that aggregate's volume-label series (see the `netapp-storage-graph` capability's "NetApp aggregate entity"); restricting by `aggr` keeps every series of a rooted aggregate and leaves the vote intact, while restricting by `svm` or `node` leaves it running over a subset that can elect a different controller and move the `node-aggr` tier. The projection also UNIONS `aggr=` with `svm=`, so a request naming both retains paths reached only through the SVM, which a restriction by `aggr` alone would drop. `node=` names a Kubernetes node as well as an ONTAP controller and is admitted as a root whether or not any path reaches it.

**Match modes that SHALL NOT restrict.** Phase 2 expresses the `exact` and `suffix` volume-match modes exactly. A build configured with `contains` or `regex` SHALL read the family unrestricted and single-phase, because those modes cannot be rendered into an anchored alternation without changing their semantics.

**Mechanics.** Each phase's restriction SHALL be chunked deterministically under the byte budget shared with the QoS workload read, one query per chunk, results merged in chunk order, each chunk issued under the bare family name so self-metrics and span dimensions carry one value per family however many queries a build issues. A single value SHALL always be issued even when it alone exceeds the budget. Phase 1 chunks ONE of its two alternations and repeats the other verbatim in every chunk; the repeated matcher SHALL be charged against the budget at its RENDERED length, escaping and wrapper included, so the budget bounds what is actually sent. Each chunk SHALL keep the family's existing error class: a failed chunk is logged and treated as an empty vector, exactly as a failed unrestricted read of this OPTIONAL family is. The per-family series tally SHALL report the merged series count under the one family name.

**A restriction SHALL be bounded, or not applied.** `ontap_cluster=` and `aggr=` are repeatable and the request parser bounds each value's length but never their COUNT, so this is the one scope a client can inflate. When the restriction would take more than a fixed maximum number of queries, the build SHALL read the family unrestricted and single-phase instead, and SHALL log that it did so. That is the read this leg performed before the restriction existed: one query, the same body, and a fan-out one request cannot enlarge. A restriction that cannot be rendered at all — every value normalising away, which an embedder filling the root sets directly can produce where the request parser cannot — SHALL take the same path. This family is OPTIONAL and SHALL NOT fail a build for either reason.

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

#### Scenario: A failed restricted chunk degrades

- **WHEN** one phase-1 chunk fails with an upstream error while every other query succeeds
- **THEN** the request returns 200, the claims whose volumes that chunk carried draw no aggregate edge, the failure is logged and counted on `kube_state_graph_upstream_query_failures_total{query="volume_labels"}`, and the build does not fail
