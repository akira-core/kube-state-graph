## MODIFIED Requirements

### Requirement: Storage build reads only what it draws

The storage-graph build SHALL NOT issue the following kube-state-metrics families: `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`, `kube_service_annotations`. The body contains no `service` or `external` node and no `service-selects-pod` edge, so the four service-side families can contribute nothing to it, and it does not carry `containers` (see "Attributes and compound groups carry over"). A family the build does not issue SHALL be absent from the build's per-family series tally, never reported as zero.

Every other family the `/v1/graph` topology read issues falls into one of two classes. The **unrestricted** class is read exactly as `/v1/graph` reads it, under the request-scoped matchers alone: the claim-binding family `kube_pod_spec_volumes_persistentvolumeclaims_info` (the root every by-reference scope is computed from — nothing precedes it that could restrict it); `kube_persistentvolumeclaim_info`, `kube_persistentvolumeclaim_annotations` and the two kubelet volume-stats families (one series per claim the binding family already names, so a restriction could not select fewer than the binding read already does); every Harvest family (a storage root SHALL be drawable whether or not a claim reaches it, and an SVM is named by `volume_labels` alone); and `ALERTS` (already restricted to firing alerts). The **by-reference** class — the two pod families, the four Kubernetes-node families and the eight controller families below — is read restricted to the object names the families read before it actually carry. A by-reference family whose scope is empty SHALL NOT be issued at all and SHALL be absent from the tally; when issued, its tally entry is the count of series its restriction matched.

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
