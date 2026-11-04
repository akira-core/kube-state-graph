# Spec Delta

## ADDED Requirements

### Requirement: Storage-graph I/O rollup parameter

`GET /v1/storage-graph` SHALL accept an optional `io_rollup` parameter whose value is exactly one of `last`, `max` or `avg`, choosing how each claim's QoS I/O series is summarised over the window. Absent, or carrying only empty values, it SHALL be `last`. It SHALL be validated after the root, and an unrecognised value or two different values SHALL be 400 `invalid_scope`.

#### Scenario: Default is last

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod&aggr=aggr1` with no `io_rollup`, or with `io_rollup=`
- **THEN** the server returns 200 with a body and upstream query strings byte-identical to those the endpoint produced before the parameter existed

#### Scenario: Explicit last equals the default

- **WHEN** a client sends the same request once with `io_rollup=last` and once without `io_rollup`
- **THEN** both requests issue identical upstream queries and return byte-identical bodies

#### Scenario: Each accepted value

- **WHEN** a client sends `io_rollup=last`, `io_rollup=max` or `io_rollup=avg`
- **THEN** the server returns 200 and every claim's I/O is that rollup of its QoS series over the window, as "QoS workload rollup" of the `netapp-storage-graph` capability defines

#### Scenario: Unrecognised value

- **WHEN** a client sends `io_rollup=p99`, `io_rollup=MAX` or `io_rollup=sum` with an otherwise valid request
- **THEN** the server returns 400 with `reason: "invalid_scope"` and a message naming `io_rollup` and the accepted values, and issues no upstream query

#### Scenario: Two different values

- **WHEN** a client sends `io_rollup=max&io_rollup=avg`
- **THEN** the server returns 400 with `reason: "invalid_scope"`

#### Scenario: An oversized or control-character value is not echoed

- **WHEN** a client sends an `io_rollup` value longer than the selector-value byte limit, or one that is not valid UTF-8 or carries a control character
- **THEN** the server returns 400 with `reason: "invalid_scope"` and the message the other selector parameters give for the same fault, naming `io_rollup` but never repeating the value, and issues no upstream query

#### Scenario: A repeated identical value is one value

- **WHEN** a client sends `io_rollup=max&io_rollup=max`, or `io_rollup=&io_rollup=max`
- **THEN** the request is accepted exactly as `io_rollup=max`

#### Scenario: The root is checked first

- **WHEN** a client sends `?start=...&end=...&az=zone-a&env=prod&io_rollup=p99` with no root
- **THEN** the server returns 400 with `reason: "missing_root"`, because the window, `az`, `env` and the root are validated before `io_rollup`

#### Scenario: The rollup is not echoed

- **WHEN** a body is returned for any `io_rollup`
- **THEN** it carries no field naming the rollup, and its shape is the shape of every other storage-graph body

#### Scenario: The other graph endpoints ignore it

- **WHEN** a client sends `io_rollup=max`, or an invalid `io_rollup`, to `GET /v1/graph` or `GET /v1/k8s-graph`
- **THEN** it is ignored without error like any unknown parameter, and `GET /v1/graph`'s `pvc-to-netapp-aggr` I/O stays the `last` rollup

### Requirement: Storage-graph I/O rollup reaches only the flow measurements

The `io_rollup` of a `/v1/storage-graph` request SHALL change only the six QoS workload measurements (`read_ops`, `write_ops`, `read_latency_us`, `write_latency_us`, `read_bytes_per_sec`, `write_bytes_per_sec`) and the flow weights derived from them; it SHALL NOT change the node and edge sets, any other attribute, or the number of upstream queries issued.

#### Scenario: Topology is independent of the rollup

- **WHEN** the same request is sent with `io_rollup=last`, `max` and `avg` against the same upstream state
- **THEN** the three bodies hold the same node ids, edge ids, labels and `data.parent` values, and differ only inside `data.metrics` of `storage-flow` edges

#### Scenario: Ceilings and node attributes are not rolled up

- **WHEN** a claim's ceiling resolves `max_iops: 5000`, its PVC carries `data.usage`, and its aggregate and controller carry `data.usage`, `data.health` and `data.perf`
- **THEN** those values are identical under every `io_rollup`, being read at the window end exactly as before

#### Scenario: The query count does not move

- **WHEN** the same request is sent with each `io_rollup`
- **THEN** each issues the same number of upstream queries to the same backends, differing only in the range function of the six QoS workload queries

### Requirement: Storage-graph pod set spans the window

The pods of a `/v1/storage-graph` body SHALL be those with a sample in the window that mount a reached claim, one per `(cluster identity, namespace, pod name)` at its newest incarnation, independent of `io_rollup`. A claim's mounters SHALL be every such pod bound to it in the window, whether or not they ran at the same time.

#### Scenario: A pod deleted inside the window is drawn

- **WHEN** pod `shop/batch-x7k2p` mounts claim `shop/scratch` and is deleted halfway through the window
- **THEN** the body draws the pod and its `pvc-pod` edge, because its series has a sample in the window

#### Scenario: A recreated pod is one pod

- **WHEN** StatefulSet pod `shop/orders-0` is recreated inside the window with a new UID
- **THEN** the body draws one `shop/orders-0` pod, identified by the newest UID and placed on the newest incarnation's node

#### Scenario: A rollout inside the window splits a claim between two pods

- **WHEN** Deployment `shop/web` with strategy `Recreate` replaces pod `shop/web-7d9f-abcde` by `shop/web-6c4b-fghij` inside the window, both mounting claim `shop/web-data` measuring `read_ops: 200`
- **THEN** both pods are drawn, each `pvc-pod` edge carries `read_ops: 100` and `labels.attribution="split"`, and the `svm-pvc` edge carries `read_ops: 200`
- **AND** the split does not depend on whether the two pods ran at the same time

#### Scenario: The rollup does not change the pod set

- **WHEN** the same request is sent with `io_rollup=last` and `io_rollup=avg`
- **THEN** both bodies draw the same pods and the same `pvc-pod` edges, and only the weights on them differ

### Requirement: In-process storage-graph I/O rollup

The in-process storage-graph engine surface SHALL let an embedder choose the I/O rollup both through the parameter-driven call and through the typed build call, and every call an embedder wrote before the rollup existed SHALL keep compiling unchanged and SHALL produce the `last` rollup.

#### Scenario: The parameter-driven call honours io_rollup

- **WHEN** an embedder passes query parameters carrying `io_rollup=max` to the in-process parameter-driven storage-graph call
- **THEN** it obtains the body `GET /v1/storage-graph` returns for the same parameters, and an invalid `io_rollup` fails with a parse error carrying `reason: "invalid_scope"`

#### Scenario: The parsed request carries the rollup

- **WHEN** an embedder parses storage-graph query parameters with the shared request parser
- **THEN** the parsed request carries the rollup, `last` when the parameter is absent

#### Scenario: The typed build takes an explicit rollup

- **WHEN** an embedder calls the typed storage build with explicit options naming the `avg` rollup
- **THEN** the six QoS workload queries are rendered with `avg_over_time` and the graph carries the `avg` measurements

#### Scenario: A pre-existing typed call is unchanged

- **WHEN** an embedder calls the typed storage build with the arguments it took before this change
- **THEN** the call compiles unchanged, issues the queries it issued before, and returns the `last` rollup

#### Scenario: An embedder parses a rollup string

- **WHEN** an embedder holds a rollup as a string from its own configuration
- **THEN** the engine provides a parser accepting `last`, `max`, `avg` and the empty string (meaning `last`) and rejecting every other value with an error

#### Scenario: An invalid typed rollup fails before any query

- **WHEN** an embedder calls the typed storage build with a rollup value outside `last`, `max`, `avg` and the zero value
- **THEN** the build returns the invalid-scope build error before issuing any upstream query, and never silently falls back to `last`

#### Scenario: The engine builds on the embedder toolchain

- **WHEN** the reusable engine packages are built, vetted and tested on the minimum embedder Go toolchain
- **THEN** the rollup surface and its examples compile and pass there

## MODIFIED Requirements

### Requirement: Flow weights on every tier

Every `storage-flow` edge on a path with at least one measured claim SHALL carry `data.metrics` with `read_ops`, `write_ops`, `read_bytes_per_sec` and `write_bytes_per_sec`, each the sum — over every claim whose path passes through that edge — of the claim's own I/O measurement from the storage join under the request's `io_rollup` (under the default `last`, the `pvc-to-netapp-aggr` figures of `/v1/graph`).

#### Scenario: Weights conserve through the chain

- **WHEN** two claims on `aggr1` measure `read_ops` 100 and 250, in SVMs `svm_a` and `svm_b`, each mounted by one pod on distinct nodes
- **THEN** the `node-aggr` edge carries `read_ops: 350`, the two `aggr-svm` edges carry 100 and 250, and each downstream tier carries its claim's figure unchanged

#### Scenario: RWX claim split across its mounters

- **WHEN** one claim measuring `read_ops: 300` is mounted by three pods
- **THEN** each of the three `pvc-pod` edges carries `read_ops: 100` and `labels.attribution="split"`, and the `svm-pvc` edge carries `read_ops: 300` with no `attribution` label

#### Scenario: Latency and ceiling only on the claim-level edge

- **WHEN** a claim resolves `read_latency_us: 450` and `max_iops: 5000`
- **THEN** its `svm-pvc` edge carries both fields and its `node-aggr`, `aggr-svm`, `pvc-pod` and `pod-node` edges carry neither

#### Scenario: Unmounted root claim is a sink

- **WHEN** a client sends `?az=zone-a&env=prod&pvc=shop/orphan-data&pvc=shop/orders-data`, both claims sit on `aggr1` in `svm_shop`, `shop/orphan-data` measures `read_ops: 40` and no pod mounts it, and `shop/orders-data` measures `read_ops: 100` and is mounted by `shop/orders-0`
- **THEN** the `node-aggr` and `aggr-svm` edges carry `read_ops: 140`, the `svm-pvc` edges carry 40 and 100, the `pvc-pod` edge from `shop/orders-data` carries 100, and no edge leaves `shop/orphan-data`

#### Scenario: Unmeasured claim draws a weightless path

- **WHEN** a claim joins the topology but matches no QoS workload series
- **THEN** its path is emitted and none of its edges carries a `metrics` key, unless another measured claim shares an upstream hop — in which case that hop carries only the measured claim's figures

#### Scenario: Latency and ceiling are claim-level only

- **WHEN** a claim's measurement carries latency or a resolved ceiling
- **THEN** its `svm-pvc` edge, being the claim-level edge, additionally carries `read_latency_us`, `write_latency_us` and, when resolved, `max_iops` / `max_bytes_per_sec`, and no other tier carries a latency or ceiling field

#### Scenario: Unmeasured claims contribute nothing

- **WHEN** a claim has no I/O measurement
- **THEN** it contributes nothing to any sum, and an edge whose every contributing claim is unmeasured carries no `metrics` key

#### Scenario: Sums are order-independent on the wire

- **WHEN** an edge's figures are summed
- **THEN** they are accumulated in ascending contribution order and rounded to 6 significant digits at serialisation, so the wire form is order-independent

#### Scenario: A multiply-mounted claim is split equally

- **WHEN** a claim is mounted by `n` pods with `n` greater than one
- **THEN** its weight is **split equally** across the `pvc-pod` edges to its mounting pods, each receiving `1/n` of every summed figure and carrying `labels.attribution="split"`
- **AND** a `pvc-pod` edge from a singly-mounted claim carries no `attribution` label, and a `pod-node` edge sums the (possibly split) `pvc-pod` weights of its pod

#### Scenario: An unmounted root claim is summed upstream

- **WHEN** an **unmounted root claim** (see "Storage-reachability projection") is measured
- **THEN** its whole measurement is summed onto its `svm-pvc` edge and every upstream hop of its path exactly as a mounted claim's is, with no split and no `pvc-pod` edge below it

#### Scenario: Weights conserve tier to tier

- **WHEN** any non-root interior node of the body is inspected
- **THEN** the sum of its incoming `read_ops` equals the sum of its outgoing `read_ops`, likewise for the other three figures, up to rounding

#### Scenario: Weights follow the rollup

- **WHEN** a claim's only `qos_read_ops` series holds the samples `100`, `400`, `200` in that order inside the window
- **THEN** its `svm-pvc` edge carries `read_ops: 200` under `io_rollup=last`, `400` under `io_rollup=max` and `233.333` under `io_rollup=avg`

#### Scenario: Under max a tier sums the claims' own peaks

- **WHEN** two claims on `aggr1` peak at `read_ops` 400 and 300 at different instants of the window, and a client sends `io_rollup=max`
- **THEN** the `node-aggr` edge carries `read_ops: 700` — the sum of the two claims' peaks, which can exceed the highest `read_ops` the aggregate ever carried at one instant — and weights still conserve tier to tier

#### Scenario: Under avg latency is an unweighted time mean

- **WHEN** a claim's `qos_read_latency` series averages `500` over the window while most of its reads happened during a `2000` µs spike, and a client sends `io_rollup=avg`
- **THEN** its `svm-pvc` edge carries `read_latency_us: 500`, the time mean of the series, not a mean weighted by `read_ops`

### Requirement: Deterministic storage-graph body

The body SHALL be byte-identical for identical `(window, az, env, roots, cluster, namespace, io_rollup)` against identical upstream state, `roots` covering every root selector, `application` included, and an absent `io_rollup` being identical to `last`. Root selector values given in a different order or repeated SHALL produce an identical body and SHALL issue identical upstream queries.

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
