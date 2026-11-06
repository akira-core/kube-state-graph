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
