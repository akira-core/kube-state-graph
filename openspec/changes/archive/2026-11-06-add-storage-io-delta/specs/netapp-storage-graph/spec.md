# Spec Delta

## ADDED Requirements

### Requirement: QoS workload delta

A `/v1/storage-graph` build with `io_delta=true` SHALL read, per QoS workload family over the measurement's `volume` scope, each series' last sample in the window minus its last in the equal span ending at `start`, and SHALL sum them per claim by the measurement's reader rules. A delta row SHALL count only if the delta read returned exactly the measured series (metric name aside) of its ONTAP cluster, `volume`, `lun` and family in any `svm` or zone. No other build SHALL issue a delta read.

#### Scenario: The delta query form

- **WHEN** a storage build with `io_delta=true` and a 5-minute window reads the delta of `qos_read_ops` for FlexVols `v_a` and `v_b`
- **THEN** the query is `last_over_time(qos_read_ops{volume=~"v_a|v_b"}[5m]) - last_over_time(qos_read_ops{volume=~"v_a|v_b"}[5m] offset 5m)`, evaluated at the build's aligned `end`, and the other five families substitute their metric name
- **AND** the `volume` scope is each selector's only matcher, no query carries a `lun` matcher, none wraps the series in `rate()`, none uses `delta()`, and the form is the same under every `io_rollup`

#### Scenario: Standard PromQL only

- **WHEN** a harvest backend is any Prometheus-compatible store
- **THEN** it can serve the delta read, because the query uses only `last_over_time`, `offset` and subtraction

#### Scenario: A series with no start value has no delta

- **WHEN** a QoS series has samples in the window but none in the `w` before `start`, `w` being `end - start`
- **THEN** the delta read returns no row for it, and the workload contributes no delta to its claim while still contributing its measurement

#### Scenario: A series whose labels changed has no delta

- **WHEN** a workload's series carries `policy_group="silver"` before `start` and until a moment inside the window, and `policy_group="gold"` from then on
- **THEN** the delta read returns a row for the OLD `silver` series, whose samples lie on both sides of `start`, and none for `gold`, which has no start sample
- **AND** the measurement read returns both series, so the reader discards the `silver` row: the workload contributes no delta to its claim, and its measurement is still read

#### Scenario: An unchanged series set keeps its delta

- **WHEN** one volume-level workload of a claim's `qos_read_ops` is carried by two series that differ only in a poller label, both with samples on both sides of `start`, measuring `60` and `40` with deltas `10` and `5`
- **THEN** the delta read returned exactly the series the measurement read returned, and the claim's `read_ops_delta` is `15`
- **AND** the comparison reads no label value: it compares whole label sets, without the metric name, only to tell whether two rows are the same series, so "QoS workload label contract" still names every label whose value the builder reads

#### Scenario: A series new inside the window leaves its workload with no delta

- **WHEN** a claim in zone `zone-a` matches one volume-level workload of `qos_read_ops` measured by a `zone-a` series and a `zone-b` series, each at `100`, and only the `zone-a` series has a sample before `start`, so the delta read returns `50` for `zone-a` and nothing for `zone-b` (a poller of another zone started scraping the workload inside the window)
- **THEN** the claim's `read_ops` is `100`, one zone per workload, and it carries no `read_ops_delta`: the series sets are compared across every zone, so a series without a start value drops the whole workload's delta even though the claim's own zone has one
- **AND** a delta row naming a series the measurement read did not return also leaves its workload with no delta

#### Scenario: An svm relabel inside the window leaves no delta

- **WHEN** a workload's `qos_read_ops` series carries no `svm` label before `start` and until a moment inside the window and `svm="svm_a"` from then on, or is renamed from `svm="svm_a"` to `svm="svm_b"` while `volume_labels` names both SVMs for the claim's FlexVol
- **THEN** the claim carries no `read_ops_delta`: the series are compared per ONTAP cluster, `volume`, `lun` and family across every `svm`, so the series that started inside the window is seen beside the old one whose stale change the delta read returned
- **AND** one FlexVol name served by two SVMs of one filer, both scraped throughout the window, keeps its delta, and the claim reads its own SVM's

#### Scenario: The label-change gate is per workload

- **WHEN** a claim matches FlexVols `v_a` and `v_b` for `qos_read_ops`, the `v_a` workload's labels changed inside the window, the `v_b` workload's delta is `-20`, and the `v_a` workload's `qos_write_ops` series is unchanged with a delta of `4`
- **THEN** the claim's `read_ops_delta` is `-20` and its `write_ops_delta` is `4`: only the changed workload of the changed family drops its delta, never the whole claim
- **AND** a LUN workload that appears inside the window leaves its FlexVol's volume-level delta in place

#### Scenario: The measurement's reader rules apply

- **WHEN** a claim's matched FlexVol is carried by a volume-level delta row of `50` and a LUN-level delta row of `30` for `qos_read_ops`, and the same workload's delta is returned once stamped with the claim's own zone and once with another zone
- **AND** the measurement read returned the same series for each workload, so every delta row passes the label-change gate
- **THEN** the claim's `read_ops_delta` is `50`: the LUN row is discarded, one zone is kept per workload as "One zone per QoS workload" defines, only rows passing "QoS candidates belong to the claim's own volume" contribute, and the sum is order-independent

#### Scenario: A claim sums the workloads that have a delta

- **WHEN** a claim matches two volume-level workloads for `qos_read_ops`, one with a delta of `10` and one with no sample before `start`
- **THEN** the claim's `read_ops_delta` is `10`, and its `read_ops` still sums both workloads' measurements

#### Scenario: The delta read is chunked within the byte budget

- **WHEN** the matched `volume` scope fits one measurement query but its alternation, rendered twice in a delta query, would exceed the scoped-read byte budget
- **THEN** the delta read splits the same sorted scope into more chunks, so the two alternations of each delta query together fit the budget and each holds at most 100 values, and merges the chunks in chunk order
- **AND** the merged result is identical to a single unchunked read of the same scope, and an empty scope issues no delta query

#### Scenario: Deltas never change the policy pick, the ceiling or the coverage signal

- **WHEN** a build reads the delta of the six families
- **THEN** the policy-group pick, the fixed-policy ceiling and the I/O-coverage signal read the measurement rows only, so each is identical to the same build without `io_delta`

#### Scenario: Other builds issue no delta read

- **WHEN** a `/v1/graph` build, a `/v1/k8s-graph` build or a `/v1/storage-graph` build without `io_delta=true` runs
- **THEN** it issues no delta read, and no `pvc-to-netapp-aggr` or `storage-flow` edge carries a change field
