# Spec Delta

## ADDED Requirements

### Requirement: PVC `qos` attribute

A `type="pvc"` node's `data` MAY carry a typed `qos` attribute — the declared QoS throughput ceiling of the policy group governing the claim's volume, as resolved by the `netapp-storage-graph` capability ("QoS fixed-policy throughput ceilings"). It is an object, serialised with `omitempty`, **outside `labels`**, and never encoded as a string map entry:

- `policy_group` — a `string`, the policy group the ceiling was keyed on. Always present when the object is present, never empty.
- `max_iops` — a JSON number, requests per second. Present iff the policy group's `qos_policy_fixed_max_throughput_iops` resolved.
- `max_bytes_per_sec` — a JSON number, bytes per second (the policy's MB/s figure × 1048576, the unit of the edge's `read_bytes_per_sec`). Present iff the policy group's `qos_policy_fixed_max_throughput_mbps` resolved.

The object SHALL be present iff at least one of `max_iops` / `max_bytes_per_sec` resolved; a claim whose ceiling did not resolve — no `svm`, no policy group on any in-scope workload, no fixed-policy series for the triple, no Harvest series in the window — SHALL carry no `qos` key at all, never an empty object, a `0`, or an "unlimited" sentinel. Presence does NOT require an I/O measurement or a `pvc-to-netapp-aggr` edge: a FlexGroup claim (SVM resolved, no aggregate) and a claim whose edge carries no measurement still carry `qos` when the ceiling resolved.

The attribute is a **copy**, not a replacement: the `max_iops` / `max_bytes_per_sec` fields of the claim's `pvc-to-netapp-aggr` edge `data.metrics` keep their own presence rule. Whenever both the node and the edge carry a field, the two values SHALL be equal. Both numbers SHALL be rounded to 6 significant digits at serialisation exactly as edge metrics are, and MAY appear in exponent form.

The attribute SHALL appear only on `type="pvc"` nodes. It is additive to the v1 wire contract: a PVC with no resolved ceiling produces a `data` object byte-identical to the pre-change shape, and for identical upstream data the attribute is byte-identical across rebuilds.

#### Scenario: PVC node carries its ceiling

- **WHEN** the response contains a PVC node whose volume is governed by policy group `gold-tier` with `max_throughput_iops = 5000` and `max_throughput_mbps = 250`
- **THEN** the node carries `data.qos: {"policy_group": "gold-tier", "max_iops": 5000, "max_bytes_per_sec": 262144000}`, and `data.labels` contains no `policy_group`, `max_iops` or `max_bytes_per_sec` key

#### Scenario: Node and edge agree

- **WHEN** a PVC node carries `data.qos.max_iops` and its `pvc-to-netapp-aggr` edge carries `data.metrics.max_iops`
- **THEN** the two values are equal, and likewise for `max_bytes_per_sec`

#### Scenario: Unresolved ceiling omits the attribute

- **WHEN** the response contains a PVC node whose volume is in no policy group, or that joined no Harvest series
- **THEN** its `data` has no `qos` key

#### Scenario: Partial ceiling keeps only the resolved field

- **WHEN** a PVC's policy group resolved `max_throughput_iops` but no `max_throughput_mbps` series
- **THEN** its `data.qos` equals `{"policy_group": <name>, "max_iops": <value>}` with no `max_bytes_per_sec` key

#### Scenario: FlexGroup PVC carries a ceiling without an edge

- **WHEN** a PVC's volume resolved an SVM but no aggregate, and its policy group's ceiling resolved
- **THEN** the PVC node carries `data.qos` and the response contains no `pvc-to-netapp-aggr` edge from it

#### Scenario: Only PVC nodes carry qos

- **WHEN** the response contains nodes of any type other than `pvc`
- **THEN** none of them carries a `qos` field
