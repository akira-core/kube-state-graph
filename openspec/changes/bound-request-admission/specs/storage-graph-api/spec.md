# Spec Delta

## MODIFIED Requirements

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
- **THEN** `kube_pod_info` and `kube_pod_owner` are each issued as `{namespace="platform",pod="orders-0"}` and `{namespace="shop",pod=~"orders-0|web-0"}` beside the request matchers, `platform/web-0` is never read, and the merged vector follows namespace order

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

- **WHEN** a restriction renders its values as a literal alternation — every by-reference read, the per-namespace pod and claim reads, the pair-keyed Harvest reads and the root-derived reads on their identity label
- **THEN** no chunk carries more than 100 values in that alternation, whatever the byte budget admits, so the upstream resolves it by exact value lookup rather than by evaluating a regex against every value of the label
- **AND** the bound holds when the byte budget is unset or non-positive, and the split stays a pure function of the value set, so the body is byte-identical to the one an unbounded split produces
- **AND** a `pod` root's seed is split by `(namespace, name)` pair, so both alternations of every seed chunk are split by value count and hold at most 100 distinct values

#### Scenario: A pod root's namespace side is bounded by the root cap

- **WHEN** a `pod` root set's `(namespace, name)` pairs, chunked under the seed's byte budget (less the request matchers) and the alternation value bound counted separately for namespaces and names, take more chunks than the root cap
- **THEN** the request is rejected 400 with `reason: "invalid_scope"` before any upstream query, even when the pod NAMES alone fit the cap — one name paired with more than the cap's chunk count times 100 namespaces is such a set
- **AND** the chunks counted are the chunks issued: the seed renders one query per pair chunk, so a root set the cap accepts never renders an alternation past 100 values or the byte budget

#### Scenario: A pod root set is bounded by its pair count

- **WHEN** a `pod` root set holds more than 1600 distinct `(namespace, name)` pairs (the root chunk cap times 100), even when its pairs fit the chunk cap because they form a cross product of few namespaces and few names
- **THEN** the request is rejected 400 with `reason: "invalid_scope"` before any upstream query, on both `/v1/storage-graph` and `/v1/k8s-graph`
- **AND** a 40 x 40 cross product (1600 pairs) is accepted and seeded as one chunk

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
- **THEN** `kube_pod_info` and `kube_pod_owner` are restricted to the `(namespace, pod)` pairs of the mounters of the tracked claims, the `pod` roots, the pods a `node` root placed on a root node, and the pods an `application` root recovered — one query per namespace, carrying that namespace as an equality and its pod names as the alternation — because a pod is identified by its namespace and name, never by its name alone
- **AND** no pair outside the set is read, and a same-named pod in another namespace — with its node and its controllers, which the next waves scope from what this read returns — is never fetched

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

