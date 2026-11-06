# Spec Delta

## ADDED Requirements

### Requirement: Storage-graph I/O delta parameter

`GET /v1/storage-graph` SHALL accept an optional `io_delta` parameter whose value is exactly `true` or `false`. Absent, or carrying only empty values, it SHALL be `false`. It SHALL be validated after `io_rollup`, and an unrecognised value or two different values SHALL be 400 `invalid_scope` before any upstream query.

#### Scenario: Default is off

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod&aggr=aggr1` with no `io_delta`, with `io_delta=` or with `io_delta=false`
- **THEN** the server returns 200 with a body and upstream query strings byte-identical to those the endpoint produced before the parameter existed

#### Scenario: Each accepted value

- **WHEN** a client sends `io_delta=true` or `io_delta=false` with an otherwise valid request
- **THEN** the server returns 200, and only `io_delta=true` adds the change fields "Storage-graph I/O delta fields" defines

#### Scenario: Unrecognised value

- **WHEN** a client sends `io_delta=1`, `io_delta=TRUE` or `io_delta=yes` with an otherwise valid request
- **THEN** the server returns 400 with `reason: "invalid_scope"` and a message naming `io_delta` and the accepted values `true` and `false`, and issues no upstream query

#### Scenario: Two different values

- **WHEN** a client sends `io_delta=true&io_delta=false`
- **THEN** the server returns 400 with `reason: "invalid_scope"` and issues no upstream query

#### Scenario: A repeated identical value is one value

- **WHEN** a client sends `io_delta=true&io_delta=true`, or `io_delta=&io_delta=true`
- **THEN** the request is accepted exactly as `io_delta=true`

#### Scenario: An oversized or control-character value is not echoed

- **WHEN** a client sends an `io_delta` value longer than the selector-value byte limit, or one that is not valid UTF-8 or carries a control character
- **THEN** the server returns 400 with `reason: "invalid_scope"` and the message the other selector parameters give for the same fault, naming `io_delta` but never repeating the value, and issues no upstream query

#### Scenario: The root and the rollup are checked first

- **WHEN** a client sends `io_delta=yes` with no root, or with `io_rollup=p99`
- **THEN** the server returns 400 with `reason: "missing_root"` for the missing root, and with a message naming `io_rollup` for the invalid rollup, because the window, `az`, `env`, the root and `io_rollup` are validated before `io_delta`

#### Scenario: The switch is not echoed

- **WHEN** a body is returned with `io_delta=true`
- **THEN** it carries no field naming the parameter, and its top-level shape is the shape of every other storage-graph body

#### Scenario: The other graph endpoints ignore it

- **WHEN** a client sends `io_delta=true`, or an invalid `io_delta`, to `GET /v1/graph` or `GET /v1/k8s-graph`
- **THEN** it is ignored without error like any unknown parameter, and no edge of either body carries a change field

### Requirement: Storage-graph I/O delta fields

With `io_delta=true`, a measured claim's `svm-pvc` edge SHALL carry `<field>_delta` beside each of `read_ops`, `write_ops`, `read_bytes_per_sec`, `write_bytes_per_sec`, `read_latency_us` and `write_latency_us` it carries: the claim's value at `end` minus its value at `start` as "QoS workload delta" of the `netapp-storage-graph` capability defines, signed, in the base field's unit, whatever the `io_rollup`.

#### Scenario: A rate that rose

- **WHEN** a claim's only `qos_read_ops` series has its last sample before `start` at `50` and its last sample in the window at `100`, and a client sends `io_delta=true`
- **THEN** its `svm-pvc` edge carries `read_ops: 100` and `read_ops_delta: 50`

#### Scenario: A throughput that fell

- **WHEN** a claim's `qos_write_data` series reads `4000000` at `start` and `1000000` at `end`
- **THEN** its `svm-pvc` edge carries `write_bytes_per_sec: 1000000` and `write_bytes_per_sec_delta: -3000000`

#### Scenario: An unchanged rate is zero, not absent

- **WHEN** a claim's `qos_read_ops` series reads `75` at both `start` and `end`
- **THEN** its `svm-pvc` edge carries `read_ops_delta: 0`

#### Scenario: Latency change on the claim-level edge

- **WHEN** a claim's `qos_read_latency` series reads `400` at `start` and `650` at `end`
- **THEN** its `svm-pvc` edge carries `read_latency_us_delta: 250`, and no other tier carries a latency delta

#### Scenario: No value at start means no delta

- **WHEN** a claim's QoS series have no sample in the `w` before `start` (the volume was created inside the window, or the series has a gap covering that span), `w` being `end - start`
- **THEN** its edges carry the measurements and no `_delta` field for those families — absence means the start value is unknown, never `0`

#### Scenario: The delta does not follow the rollup

- **WHEN** a claim's `qos_read_ops` series has its last sample before `start` at `50` and the samples `100`, `400`, `200` in that order inside the window, and a client sends `io_rollup=max&io_delta=true`
- **THEN** its `svm-pvc` edge carries `read_ops: 400` and `read_ops_delta: 150`, the change from the start value to the end value, not to the peak

#### Scenario: Equal to two plain requests

- **WHEN** a claim is measured by one volume-level workload carried by the same series throughout both windows, the window length `w` is a multiple of the end-alignment step, and a client sends one request with `io_delta=true` and two requests with `io_rollup=last` and no `io_delta`, one for the same window and one for the window of length `w` ending at the first request's aligned `start`
- **THEN** the claim's `read_ops_delta` in the first body equals its `read_ops` in the second minus its `read_ops` in the third, up to rounding, and likewise for the other five fields

#### Scenario: A change field never appears without its base field

- **WHEN** any `storage-flow` edge is serialised
- **THEN** it carries `<field>_delta` only when it also carries `<field>`, and an unmeasured claim's edges carry no change field

#### Scenario: Rounded like every measurement

- **WHEN** a change field is serialised
- **THEN** it is a JSON number rounded to 6 significant digits like every other `data.metrics` field, keeping its sign, and it is never placed in `labels`

### Requirement: Storage-graph I/O deltas sum like the weights

With `io_delta=true`, every tier above and below the claim SHALL carry the four flow change fields as it carries the flow weights: summed over the claims whose path passes through the edge, split equally across a claim's mounters, accumulated in ascending contribution order. A claim with no change value for a field SHALL contribute nothing to that field's sum.

#### Scenario: Changes sum up the chain

- **WHEN** two claims on `aggr1` carry `read_ops_delta` `50` and `-20`, in SVMs `svm_a` and `svm_b`
- **THEN** the `node-aggr` edge carries `read_ops_delta: 30`, the two `aggr-svm` edges carry `50` and `-20`, and each downstream tier carries its claim's change unchanged

#### Scenario: A multiply-mounted claim's change is split

- **WHEN** a claim carrying `read_ops_delta: 30` is mounted by three pods
- **THEN** each of its three `pvc-pod` edges carries `read_ops_delta: 10`, and its `svm-pvc` edge carries `30`

#### Scenario: A tier change covers the claims measured at both ends

- **WHEN** claim `A` on `aggr1` measures `read_ops: 100` with `read_ops_delta: 50`, and claim `B` on `aggr1` was created inside the window and measures `read_ops: 200` with no start value
- **THEN** the `node-aggr` edge carries `read_ops: 300` and `read_ops_delta: 50`: a tier change sums only the claims that carry one, so it is not the tier's end weight minus its start weight when a claim appeared inside the window

#### Scenario: Changes conserve tier to tier

- **WHEN** any non-root interior node of an `io_delta=true` body is inspected
- **THEN** the sum of its incoming `read_ops_delta` equals the sum of its outgoing `read_ops_delta`, likewise for the other three flow change fields, up to rounding

### Requirement: Storage-graph I/O delta reaches only the change fields

`io_delta=true` SHALL add only the six change fields inside `data.metrics` of `storage-flow` edges and the six QoS workload delta reads of "QoS workload delta"; it SHALL NOT change the node and edge sets, any other attribute or field, or any other upstream query. Every delta read SHALL fail the build on error, as every storage-build query except `ALERTS` does.

#### Scenario: Topology is independent of the switch

- **WHEN** the same request is sent with `io_delta=false` and `io_delta=true` against the same upstream state
- **THEN** the two bodies hold the same node ids, edge ids, labels, `data.parent` values and attributes, and every `data.metrics` field of the first appears with the same value in the second, which differs only by the added change fields

#### Scenario: The queries it adds

- **WHEN** a request with `io_delta=true` matches at least one FlexVol
- **THEN** it issues every query the same request with `io_delta=false` issues, plus one delta query per QoS workload family per delta chunk of the same `volume` scope, to the same backends; a scope that matched no FlexVol issues no delta query

#### Scenario: A delta query failure fails the storage request

- **WHEN** a client sends `io_delta=true` and one chunk of the `qos_read_ops` delta read fails with an upstream error
- **THEN** the server returns 502 with `reason: "upstream"` and `message: "upstream query failed: qos_read_ops"`, and returns no graph body

### Requirement: In-process storage-graph I/O delta

The in-process storage-graph engine surface SHALL let an embedder turn the I/O delta on both through the parameter-driven call and through the typed build call, and every call an embedder wrote before the switch existed SHALL keep compiling unchanged and SHALL issue no delta read.

#### Scenario: The parameter-driven call honours io_delta

- **WHEN** an embedder passes query parameters carrying `io_delta=true` to the in-process parameter-driven storage-graph call
- **THEN** it obtains the body `GET /v1/storage-graph` returns for the same parameters, and an invalid `io_delta` fails with a parse error carrying `reason: "invalid_scope"`

#### Scenario: The parsed request carries the switch

- **WHEN** an embedder parses storage-graph query parameters with the shared request parser
- **THEN** the parsed request carries the switch, off when the parameter is absent

#### Scenario: The typed build takes the switch in its options

- **WHEN** an embedder calls the typed storage build with explicit options turning the delta on
- **THEN** the six QoS workload delta reads are issued and the graph's `storage-flow` edges carry the change fields

#### Scenario: A pre-existing typed call is unchanged

- **WHEN** an embedder calls the typed storage build with the arguments, or the options value, it used before this change
- **THEN** the call compiles unchanged, issues the queries it issued before, and its graph carries no change field

#### Scenario: The engine builds on the embedder toolchain

- **WHEN** the reusable engine packages are built, vetted and tested on the minimum embedder Go toolchain
- **THEN** the delta surface and its examples compile and pass there

## MODIFIED Requirements

### Requirement: Deterministic storage-graph body

The body SHALL be byte-identical for identical `(window, az, env, roots, cluster, namespace, io_rollup, io_delta)` against identical upstream state, `roots` covering every root selector, `application` included, an absent `io_rollup` being identical to `last` and an absent `io_delta` being identical to `false`. Root selector values given in a different order or repeated SHALL produce an identical body and SHALL issue identical upstream queries.

#### Scenario: Root order does not matter

- **WHEN** a client sends `?aggr=b&aggr=a` and then `?aggr=a&aggr=b&aggr=a`
- **THEN** both bodies are byte-identical

#### Scenario: Application root order does not matter

- **WHEN** a client sends `?application=b&application=a` and then `?application=a&application=b&application=a`
- **THEN** both requests issue identical recovery queries and return byte-identical bodies

#### Scenario: The body is sorted and carries no build-time field

- **WHEN** a storage-graph body is serialised
- **THEN** nodes and edges are sorted as in `/v1/graph`, `clusters` is sorted, weights are order-independent, and the body carries no time-of-build or echo-of-input field

#### Scenario: Two rollups of one window are not confused

- **WHEN** a client sends a request with `io_rollup=max` and, within the query-result cache lifetime, the same request with `io_rollup=avg`
- **THEN** the second body carries the `avg` figures, never figures cached for the `max` request

#### Scenario: A delta body and a plain body are not confused

- **WHEN** a client sends a request with `io_delta=true` and, within the query-result cache lifetime, the same request without `io_delta`
- **THEN** the second body carries no change field and is byte-identical to the body the request returned before `io_delta` existed

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
