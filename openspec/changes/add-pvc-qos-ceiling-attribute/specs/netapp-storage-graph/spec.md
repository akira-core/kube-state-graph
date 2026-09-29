# Spec Delta

## MODIFIED Requirements

### Requirement: QoS fixed-policy throughput ceilings

The builder SHALL resolve each joined volume's declared throughput ceiling from the OPTIONAL Harvest QoS fixed-policy series `qos_policy_fixed_max_throughput_iops` and `qos_policy_fixed_max_throughput_mbps`. The fixed, case-sensitive label contract: `cluster` (the ONTAP cluster), `svm`, and the policy's own identity label naming the policy group, read as `name` with a `policy_group` fallback (Harvest spells it differently across templates; the contract pins the key's SHAPE, not the label's spelling).

A ceiling SHALL be resolved for every claim whose `volume_labels` match resolved an SVM — a claim that also resolved an aggregate AND a claim that resolved an SVM but no aggregate (the FlexGroup shape). The join key is the `(ontap-cluster, svm, policy-group)` triple, assembled from BOTH topology hops:

- `ontap-cluster` and `svm` come from **hop A** — the SVM the `volume_labels` match resolved (see "PVC svm label re-sourced from the Harvest join") and the ONTAP cluster it sits on: the ONTAP cluster of the claim's picked aggregate when an aggregate resolved (the SVM pick is scoped to that filer), otherwise the ONTAP cluster the SVM pick itself landed on. The ceiling is therefore anchored on the same filer and SVM the claim's storage chain points at.
- `policy-group` comes from **hop B** — the `policy_group` label of the claim's in-scope QoS workload candidates (those on the key's ONTAP cluster whose `svm`, when present, equals the key's), which is the ONLY upstream statement of which policy group governs THIS FlexVol; `volume_labels` carries no policy identity. Candidates SHALL be read at **both granularities**, LUN-level rows included: on a SAN backend the QoS policy is attached to the LUN, and the FlexVol's own workload then falls into the provisioner-independent built-in class the storage system assigns to unmanaged workloads (`User-Best_effort` on ONTAP), which by definition declares no ceiling and therefore has no fixed-policy series. Admitting LUN rows here cannot affect a measurement, since the I/O sum reads volume-level rows only.

  The pick SHALL **prefer a policy group the fixed-policy families actually hold** for the claim's `(ontap-cluster, svm)`, taking the lexically-smallest such value; when none resolves it SHALL fall back to the lexically-smallest non-empty value overall, which leaves the ceiling absent exactly as an unmatched key does. Both passes are order-free. The preference SHALL be data-driven — the builder SHALL NOT carry a hardcoded list of built-in class names, whose spelling varies by storage-system release — and it is what stops a built-in class that sorts ahead of the real policy from winning the pick.

Sourcing the cluster and SVM from hop A rather than from the workload series is what lets a workload series carrying a `policy_group` but NO `svm` label still resolve a ceiling, and what keeps the key on the picked filer under a cross-filer FlexVol-name collision.

`max_iops` is read verbatim. `max_bytes_per_sec` is the **one** figure in this capability NOT read verbatim: it is `qos_policy_fixed_max_throughput_mbps × 1048576`, converted so the ceiling carries the same unit as the measured `read_bytes_per_sec` / `write_bytes_per_sec` and the two compare without client-side arithmetic.

Each field SHALL be present iff its own family holds a series for the triple; on duplicate series for one triple the builder SHALL pick deterministically (the smallest numeric value). **An incomplete or unmatched key SHALL be ignored, never widened**: a claim whose hop A resolved no `svm`, whose in-scope workload candidates carry no non-empty `policy_group` (the volume is in no policy group, or the Harvest template omits the label), or whose triple matches no fixed-policy series SHALL resolve no ceiling. The builder SHALL NOT fall back to an SVM-wide or cluster-wide ceiling: a figure resolved from another policy group would name a limit this volume does not have. Absence means *no declared ceiling* and SHALL NEVER be rendered as a number, whether `0` or an "unlimited" sentinel. Failure of either query SHALL degrade gracefully and SHALL NOT fail the build.

The resolved ceiling SHALL surface in two places, both from the ONE resolution per claim so the two can never disagree:

- **On the claim's PVC node** as the `data.qos` attribute (the `graph-api` capability's "PVC `qos` attribute"), whenever a ceiling resolved — whether or not the claim carries any I/O measurement, and whether or not an aggregate resolved.
- **On the claim's `pvc-to-netapp-aggr` edge** as the `max_iops` and `max_bytes_per_sec` fields of `data.metrics`, only when that edge exists (an aggregate resolved) AND carries at least one measurement field. A ceiling field SHALL NEVER appear on an edge carrying no measurement field — on the edge the two ride together, or the ceiling is absent there.

Because the policy group is recovered from a matched workload series, a claim with no in-scope workload series at all resolves no ceiling in either place.

#### Scenario: Ceiling resolved from the volume's policy group

- **WHEN** a claim's picked aggregate is on `cluster="ontap-prod"`, its `volume_labels` match resolved `svm="svm-prod"`, its in-scope QoS workload series carry `policy_group="gold-tier"`, and the fixed-policy series `qos_policy_fixed_max_throughput_iops{cluster="ontap-prod", svm="svm-prod", name="gold-tier"} = 5000` and `qos_policy_fixed_max_throughput_mbps{...} = 250` exist
- **THEN** the edge's `data.metrics` contains `max_iops: 5000` and `max_bytes_per_sec: 262144000`, and the claim's PVC node carries `data.qos` with `policy_group: "gold-tier"`, `max_iops: 5000` and `max_bytes_per_sec: 262144000`

#### Scenario: SAN claim resolves its ceiling through the LUN workload

- **WHEN** a claim's FlexVol carries a volume-level workload in the built-in `User-Best_effort` class (`qos_read_ops` = `150`, no fixed-policy series for that class) and a LUN-level workload in `gold-tier` (`qos_read_ops` = `90`), and `gold-tier` holds fixed-policy series for the claim's `(ontap-cluster, svm)`
- **THEN** the edge reports `read_ops: 150` (the LUN row is not summed) AND resolves `gold-tier`'s ceiling — the policy group is recovered from the LUN row, the only series naming it — and the PVC node's `data.qos.policy_group` is `"gold-tier"`

#### Scenario: A built-in class sorting first does not win the pick

- **WHEN** a claim's candidates offer both `User-Best_effort` (no fixed-policy series) and `gold-tier` (fixed-policy series present), and `User-Best_effort` sorts lexically before `gold-tier`
- **THEN** the ceiling resolves from `gold-tier` — the pick prefers a policy the fixed-policy families hold over a lexically-smaller one they do not

#### Scenario: Another policy group in the same SVM is never borrowed

- **WHEN** `svm-prod` also holds a `bronze-tier` policy with `max_throughput_iops = 100`, and the claim's workload series carry `policy_group="gold-tier"`
- **THEN** the edge and the PVC node both report `max_iops: 5000` — the ceiling is the volume's own policy group's, and the SVM's other policies are neither consulted nor minimised over

#### Scenario: Volume in no policy group carries no ceiling

- **WHEN** a claim's in-scope QoS workload series all carry an empty `policy_group` label, while its SVM does hold fixed-policy series for other groups
- **THEN** the edge's `data.metrics` carries its measurement fields and has neither a `max_iops` nor a `max_bytes_per_sec` key, and the PVC node has no `qos` key — an empty match is ignored, never widened to the SVM

#### Scenario: Workload without an svm label still resolves its ceiling

- **WHEN** a claim's hop-A match resolved `svm="svm-prod"` on `cluster="ontap-prod"` and its in-scope workload series carry `policy_group="gold-tier"` but no `svm` label of their own
- **THEN** the workload still measures the edge and the ceiling resolves from `(ontap-prod, svm-prod, gold-tier)` — the key's SVM component comes from hop A, not from the workload series

#### Scenario: Claim without an SVM carries no ceiling

- **WHEN** a claim's matched `volume_labels` series carries an empty `svm` label, while its workload series carry a `policy_group` and the ONTAP cluster holds fixed-policy series
- **THEN** the edge's `data.metrics` carries its measurement fields and has neither a `max_iops` nor a `max_bytes_per_sec` key, and the PVC node has no `qos` key — the triple is incomplete and is ignored

#### Scenario: Partial ceiling keeps only the resolved field

- **WHEN** the claim's triple matches `qos_policy_fixed_max_throughput_iops` but no `qos_policy_fixed_max_throughput_mbps` series
- **THEN** the edge's `data.metrics` contains `max_iops` and no `max_bytes_per_sec` key, and the PVC node's `data.qos` likewise carries `max_iops` and no `max_bytes_per_sec`

#### Scenario: No ceiling without a measurement

- **WHEN** a claim draws its edge from `volume_labels` but matches no QoS workload series
- **THEN** the edge has no `metrics` key at all and the PVC node has no `qos` key — with no workload series there is no policy group to key on

#### Scenario: FlexGroup claim resolves its ceiling on the node

- **WHEN** a claim's matched `volume_labels` series carries `svm="svm_big"` on `cluster="ontap-prod"` and an empty `aggr`, its in-scope workload series carry `policy_group="gold-tier"`, and `(ontap-prod, svm_big, gold-tier)` holds fixed-policy series with `max_throughput_iops = 8000`
- **THEN** the claim has no `pvc-to-netapp-aggr` edge, and its PVC node carries `data.qos` with `policy_group: "gold-tier"` and `max_iops: 8000`

#### Scenario: Ceiling on the node when the edge carries no measurement

- **WHEN** a claim resolved an aggregate and SVM, its only in-scope workload series are LUN-level rows carrying `policy_group="gold-tier"` (no volume-level row, so the edge carries no measurement), and `gold-tier` holds fixed-policy series for the claim's `(ontap-cluster, svm)`
- **THEN** the claim's `pvc-to-netapp-aggr` edge has no `metrics` key, and its PVC node carries `data.qos` with `policy_group: "gold-tier"` and the resolved ceiling fields
