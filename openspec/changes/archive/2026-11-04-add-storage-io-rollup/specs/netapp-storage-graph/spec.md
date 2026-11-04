# Spec Delta

## ADDED Requirements

### Requirement: QoS workload rollup

The six QoS workload queries SHALL be rendered with the build's rollup function over the whole window — `last_over_time` (`last`), `max_over_time` (`max`) or `avg_over_time` (`avg`) — and every other Harvest query SHALL keep `last_over_time`. A `/v1/graph` build SHALL always use `last`; a `/v1/storage-graph` build SHALL use its request's `io_rollup`.

#### Scenario: The rollup is the only difference in the query

- **WHEN** a storage build with rollup `max` issues the scoped QoS read for FlexVols `v_a` and `v_b`
- **THEN** each of the six queries is `max_over_time(<family>{volume=~"v_a|v_b"}[<window>])`, identical to the `last` query but for the function name, with the same `volume` scope, chunking, chunk order and backends

#### Scenario: Each series is rolled up before any sum

- **WHEN** a claim's in-scope volume-level candidates for `qos_read_ops` are two workloads whose series peak at `300` and `200` at different instants, under rollup `max`
- **THEN** the claim's `read_ops` is `500`: each series is reduced to its own peak upstream, and the reader then applies the LUN discard, the one-zone-per-workload choice and the order-independent sum to those reduced values exactly as under `last`

#### Scenario: A series present for part of the window

- **WHEN** a claim's `qos_write_ops` series has samples only in the last quarter of the window, averaging `80`, under rollup `avg`
- **THEN** the claim's `write_ops` is `80`, the mean over the samples the series has, not a mean that counts the rest of the window as zero

#### Scenario: Ceilings and gauges keep last

- **WHEN** a build issues `qos_policy_fixed_max_throughput_iops`, `qos_policy_fixed_max_throughput_mbps`, `volume_labels`, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels` or a `node_*` performance counter under any rollup
- **THEN** each is rendered with `last_over_time`, exactly as before this change

#### Scenario: The graph endpoint is always last

- **WHEN** a `/v1/graph` build issues the scoped QoS read
- **THEN** every QoS query is rendered with `last_over_time`, whatever parameters the request carried, and every `pvc-to-netapp-aggr` I/O figure is unchanged by this change

#### Scenario: Presence does not depend on the rollup

- **WHEN** a QoS workload series has at least one sample in the window
- **THEN** it is returned under every rollup, so the set of measured claims, the set of present `data.metrics` fields and the I/O-coverage signal are the same under `last`, `max` and `avg`

#### Scenario: Rollups are cached apart

- **WHEN** the same window's QoS read is issued under two different rollups within the query-result cache lifetime
- **THEN** the two are distinct cache entries, because the rendered query strings differ

## MODIFIED Requirements

### Requirement: Harvest QoS workload series as the I/O source

The builder SHALL read each claim's I/O from the Harvest QoS workload series `qos_read_ops`, `qos_write_ops`, `qos_read_latency`, `qos_write_latency`, `qos_read_data` and `qos_write_data` verbatim, rolled up over the window as "QoS workload rollup" defines, summing only volume-level candidates (empty `lun`) and keeping one zone per workload as "One zone per QoS workload" defines. All six families SHALL be OPTIONAL and independent of the topology source.

#### Scenario: QoS workload label contract

- **WHEN** the builder reads a QoS workload series
- **THEN** each series MUST carry, and the builder reads, the fixed, case-sensitive labels `cluster` (the ONTAP cluster name), `svm` (the serving Storage Virtual Machine), `policy_group` (the QoS policy group governing the workload — empty when the volume is in none), `lun` (empty for a volume-level workload) and `volume` (the ONTAP FlexVol name, the same stock label the volume-object family carries)
- **AND** no non-stock label is read beyond the configured `az` / `env` pair, which is read, when present, only to choose one zone per workload ("One zone per QoS workload")

#### Scenario: QoS queries restricted to volume granularity

- **WHEN** the builder issues the six Harvest QoS queries for a window
- **THEN** volume granularity is enforced by the reader, not the selector: no query string carries a `lun` matcher, each references the bare series rolled up over the window with the build's rollup function (`last_over_time` on every `/v1/graph` build and on a `/v1/storage-graph` build whose `io_rollup` is `last` or absent) restricted only by the `volume` scope, and none wraps the series in `rate()`
- **AND** the `volume` scope restriction of "Scoped and batched QoS workload read" is the ONLY matcher these queries carry

#### Scenario: LUN rows are discarded by the reader, not the selector

- **WHEN** ONTAP collects a workload per LUN as well as per volume and a LUN workload carries the `volume` of its containing FlexVol
- **THEN** the two are not summed together: the builder sums only candidates whose `lun` label is empty, and reads every candidate, LUN rows included, when resolving the policy group ("Ceiling key assembled from both topology hops")
- **AND** this reader discard is strictly stronger than a `lun=""` selector, which a Harvest template omitting `lun` on a LUN workload would slip past, and it is what makes the SAN policy reachable at all

#### Scenario: LUN workloads never contribute

- **WHEN** a claim's matched FlexVol is carried both by a volume-level QoS series (no `lun` label, `qos_read_ops` = `150`) and by a LUN-level QoS series (`lun="/vol/trident_pvc_9f3a/lun0"`, `qos_read_ops` = `90`)
- **THEN** the edge reports `read_ops: 150` — the LUN series reaches the reader, which discards it from every I/O sum

#### Scenario: A LUN-only FlexVol measures nothing

- **WHEN** a claim's matched FlexVol is carried by LUN-level QoS series only, with no volume-level row in the window
- **THEN** the edge is still emitted with no `metrics` key at all, and the claim is counted by the I/O-coverage signal — a LUN row is not a volume measurement

#### Scenario: QoS values are already resolved

- **WHEN** the builder reads the QoS workload series
- **THEN** it reads their values **verbatim**, since Harvest already resolves ONTAP's base counters: the ops series are per-second rates, the latency series are averages in microseconds, and the data series are throughput in bytes per second
- **AND** no issued query wraps them in `rate()`, the opposite of the service-graph RED counters, whose upstream series are raw counters; the rollup function reduces those resolved values over the window and never derives a rate

#### Scenario: Topology resolves without QoS

- **WHEN** a claim resolves its aggregate from `volume_labels` but no QoS workload series carries its matched FlexVol name
- **THEN** the graph still contains the `pvc-to-netapp-aggr` edge and its `netapp-aggr` / `netapp-node` nodes, the edge has no `metrics` key, and the claim is counted by the I/O-coverage signal
- **AND** the edge carries no `metrics` key at all whenever none of the six families matches a claim that DID resolve its topology
