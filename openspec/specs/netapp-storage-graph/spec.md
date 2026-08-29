# netapp-storage-graph Specification

## Purpose
Surfaces the physical NetApp ONTAP storage behind Kubernetes claims: one graph node per ONTAP aggregate and per ONTAP controller, a PVC-to-aggregate edge derived from a single Harvest label series, per-edge I/O measurements and the volume's declared throughput ceiling from the Harvest QoS families, per-aggregate health and usage, controller health, and coverage signals for claims that should have joined but did not.
## Requirements
### Requirement: Harvest volume-label series as the storage topology source

The builder SHALL consume the NetApp Harvest volume-object label series `volume_labels` from the same centralised VictoriaMetrics endpoint as every other series. The fixed, case-sensitive label contract it MUST carry: `cluster` (the ONTAP cluster name — NOT a Kubernetes cluster; the two namespaces never mix), `node` (the ONTAP controller currently owning the containing aggregate), `aggr` (the containing aggregate), `svm` (the serving Storage Virtual Machine), and `volume_name` (the name of the Kubernetes PersistentVolume the FlexVol backs). It is an **info series**: its sample value SHALL be ignored entirely and only its label set consumed.

This one series is the SOLE source of the graph's storage topology — the `pvc-to-netapp-aggr` edge, the `netapp-aggr` and `netapp-node` entities, and the PVC `svm` label all derive from it and from nothing else. The I/O measurements and the throughput ceiling ride on separate families (the two requirements below) and SHALL NOT contribute to any topological decision; conversely, a claim SHALL NEVER lose its storage topology because an I/O family failed to match.

The issued query SHALL read the series at the window end without `rate()`, in the same shape as every other Harvest leg.

`volume_name` is NOT a stock Harvest label — it is produced by the deployment's own Prometheus relabel rule mapping each FlexVol to the PV it backs, and that rule MUST stamp both this series and the QoS workload series below. The rule is a **deployment precondition** with three known blind spots the graph inherits: a FlexVol whose name does not match the rule carries no `volume_name` (its claim never joins); the Trident "economy" drivers pack many claims into one shared FlexVol, so no per-claim series exists at all; and a FlexGroup volume spans aggregates, so its series carries no single usable `aggr` label (no aggregate edge can be drawn — see the join-coverage requirement).

The family is OPTIONAL. When it is absent from the window — the normal case for a deployment without NetApp Harvest — the builder SHALL produce a valid graph with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` labels; PVC `volumename` labels are unaffected and the build SHALL NOT fail.

#### Scenario: Volume label series consumed for its labels only

- **WHEN** the builder issues the `volume_labels` query for a window
- **THEN** the query references the bare series evaluated at the window end, does not wrap it in `rate()`, and the resolver derives the aggregate, owning controller, and SVM from the matched series' labels while its sample value plays no part in any output

#### Scenario: Harvest absent entirely

- **WHEN** the upstream contains topology series but no `volume_labels` series for the window
- **THEN** the build completes successfully with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` labels, while PVC `volumename` labels still resolve from `kube_persistentvolumeclaim_info`

#### Scenario: I/O families present without the label series

- **WHEN** the upstream carries QoS workload series for a claim's PV name but no `volume_labels` series matches it
- **THEN** no `pvc-to-netapp-aggr` edge is emitted for that claim, no aggregate or controller is materialised from the QoS series, and the build does not fail

### Requirement: Harvest QoS workload series as the I/O source

The builder SHALL consume the NetApp Harvest QoS workload series `qos_read_ops`, `qos_write_ops`, `qos_read_latency`, `qos_write_latency`, `qos_read_data`, and `qos_write_data`. The fixed, case-sensitive label contract each series MUST carry: `cluster` (the ONTAP cluster name), `svm` (the serving Storage Virtual Machine), `policy_group` (the QoS policy group governing the workload — empty when the volume is in none), `lun` (empty for a volume-level workload), and `volume_name` (stamped by the same deployment relabel rule as the volume label series).

Every issued QoS query SHALL restrict the selector to **volume granularity** with the exact matcher `lun=""`. ONTAP collects a workload per LUN as well as per volume, and a LUN workload carries the `volume_name` of its containing FlexVol once the relabel rule has run, so an unrestricted read would sum LUN traffic on top of volume traffic for the same claim. This matcher is a fixed, **request-invariant metric-selection contract** — not a caller filter — of the same class as the service-graph sentinel matcher and `kube_node_status_condition{condition="Ready"}`, so the "no filters pushed to PromQL" rule is preserved. Because a PromQL empty-string matcher also matches series carrying no such label at all, the contract stays correct against a Harvest template that omits `lun` entirely.

Values SHALL be read **verbatim**: Harvest already resolves ONTAP's base counters, so the ops series are per-second rates, the latency series are averages in microseconds, and the data series are throughput in bytes per second. The issued queries SHALL NOT wrap these series in `rate()` — the opposite of the service-graph RED counters, where the upstream series are raw counters.

A matched QoS series SHALL contribute to a claim only when it belongs to the volume the edge was drawn for: its `cluster` MUST equal the ONTAP cluster of the picked aggregate, and its `svm` MUST equal the claim's resolved SVM whenever both are non-empty. A PV name colliding across two filers sharing one VictoriaMetrics would otherwise sum a foreign volume's throughput onto this edge. A candidate carrying no `svm` label still measures the volume and is kept, but cannot contribute a policy group.

All six families are OPTIONAL and independent of the topology source. When none matches a claim that DID resolve its topology, the builder SHALL still emit that claim's `pvc-to-netapp-aggr` edge with no `metrics` key at all, and SHALL count the claim toward the I/O-coverage signal. A volume for which ONTAP collects no QoS workload is the fourth known blind spot of this capability, alongside the three relabel-rule blind spots above.

#### Scenario: QoS queries restricted to volume granularity

- **WHEN** the builder issues the six Harvest QoS queries for a window
- **THEN** every query string carries the exact `lun=""` matcher, references the bare series evaluated at the window end, and none wraps the series in `rate()`

#### Scenario: LUN workloads never contribute

- **WHEN** a claim's PV name is carried both by a volume-level QoS series (`lun=""`, `qos_read_ops` = `150`) and by a LUN-level QoS series (`lun="/vol/pvc_9f3a/lun0"`, `qos_read_ops` = `90`)
- **THEN** the edge reports `read_ops: 150` — the LUN series is excluded at the query layer and never summed in

#### Scenario: A colliding PV name on another filer does not contribute

- **WHEN** a claim's PV name is carried by `volume_labels` series on both `ontap-a`/`aggr1` and `ontap-b`/`aggr9`, and by QoS series reporting `10` on `ontap-a` and `90` on `ontap-b`
- **THEN** the edge targets `netapp/ontap-a/aggr/aggr1` and reports `read_ops: 10` — the other filer's workload is not summed in

#### Scenario: Topology resolves without QoS

- **WHEN** a claim resolves its aggregate from `volume_labels` but no QoS workload series carries its PV name
- **THEN** the graph still contains the `pvc-to-netapp-aggr` edge and its `netapp-aggr` / `netapp-node` nodes, the edge has no `metrics` key, and the claim is counted by the I/O-coverage signal

### Requirement: QoS fixed-policy throughput ceilings

The builder SHALL resolve each joined volume's declared throughput ceiling from the OPTIONAL Harvest QoS fixed-policy series `qos_policy_fixed_max_throughput_iops` and `qos_policy_fixed_max_throughput_mbps`. The fixed, case-sensitive label contract: `cluster` (the ONTAP cluster), `svm`, and the policy's own identity label naming the policy group. The join key is the `(ontap-cluster, svm, policy-group)` triple recovered from the claim's matched QoS workload series, so a ceiling is reachable ONLY through a matched workload.

The resolved values SHALL surface as the `max_iops` and `max_bytes_per_sec` fields of the edge's `data.metrics` object. `max_iops` is read verbatim. `max_bytes_per_sec` is the **one** figure in this capability NOT read verbatim: it is `qos_policy_fixed_max_throughput_mbps × 1048576`, converted so the ceiling carries the same unit as the measured `read_bytes_per_sec` / `write_bytes_per_sec` and the two compare without client-side arithmetic.

Each field SHALL be present iff its own family matched the triple; on duplicate series for one triple the builder SHALL pick deterministically (the smallest numeric value). When a claim's matched QoS workload series disagree on the triple, the builder SHALL pick the lexically-smallest non-empty one. A volume in no QoS policy group (an empty `policy_group` label), a policy with no matching fixed-policy series, or an absent metric SHALL leave both fields absent — absence means *no declared ceiling* and SHALL NEVER be rendered as a number, whether `0` or an "unlimited" sentinel. Failure of either query SHALL degrade gracefully and SHALL NOT fail the build.

Because the triple originates on the workload series, a ceiling field SHALL NEVER appear on an edge carrying no measurement field — the two ride together, or the ceiling is absent.

#### Scenario: Ceiling resolved from the volume's policy group

- **WHEN** a claim's matched QoS workload series carry `cluster="ontap-prod"`, `svm="svm-prod"`, `policy_group="gold-tier"`, and the fixed-policy series for that policy report `max_throughput_iops = 5000` and `max_throughput_mbps = 250`
- **THEN** the edge's `data.metrics` contains `max_iops: 5000` and `max_bytes_per_sec: 262144000`

#### Scenario: Volume in no policy group carries no ceiling

- **WHEN** a claim's matched QoS workload series carry an empty `policy_group` label
- **THEN** the edge's `data.metrics` carries its measurement fields and has neither a `max_iops` nor a `max_bytes_per_sec` key

#### Scenario: Partial ceiling keeps only the resolved field

- **WHEN** the claim's policy matches `qos_policy_fixed_max_throughput_iops` but no `qos_policy_fixed_max_throughput_mbps` series
- **THEN** the edge's `data.metrics` contains `max_iops` and no `max_bytes_per_sec` key

#### Scenario: No ceiling without a measurement

- **WHEN** a claim draws its edge from `volume_labels` but matches no QoS workload series
- **THEN** the edge has no `metrics` key at all — neither a measurement nor a ceiling field is emitted

### Requirement: NetApp aggregate entity

The builder SHALL materialise each ONTAP aggregate referenced by at least one joined claim as a `type="netapp-aggr"` graph node:

- `id` SHALL be `netapp/<ontap-cluster>/aggr/<aggr>` (aggregate names are cluster-wide unique in ONTAP; the id deliberately **excludes the owning node**, so an HA takeover moves ownership without changing the aggregate's identity).
- `name` SHALL be `<aggr>` (the aggregate name).
- `labels` SHALL be exactly `{ontap_cluster: "<ontap-cluster>", node: "<node>"}` — the `node` value is the controller **currently owning** the aggregate (from the matched `volume_labels` series' `node` label) and drives the compound nesting under the real `netapp-node` (graph-api "Cytoscape compound node grouping"). Deliberately **no `cluster` key** — the aggregate belongs to no Kubernetes cluster.

When matched `volume_labels` series disagree on the owning `node` for one aggregate (e.g. a takeover inside the window), the builder SHALL pick deterministically — the lexically-smallest non-empty `node` value — so the emitted labels are byte-stable across rebuilds. Because the aggregate carries no `cluster` label, its ONTAP cluster name SHALL NEVER appear in the response's top-level `clusters[]` array, and a `?cluster=` filter SHALL never match it directly. A filer shared by several Kubernetes clusters SHALL yield **one** aggregate node per aggregate — the same id — with `pvc-to-netapp-aggr` edges arriving from every Kubernetes cluster that uses it.

A NetApp aggregate SHALL be materialised ONLY via the PVC join — never wholesale from Harvest series presence. It SHALL NOT carry `ipaddress`, `owner`, `application`, `containers`, or `ready_status`.

#### Scenario: Aggregate identity and labels

- **WHEN** a claim joins a `volume_labels` series with `cluster="ontap-prod"`, `node="ontap-prod-01"`, `aggr="aggr1"`
- **THEN** the graph contains a node with `id="netapp/ontap-prod/aggr/aggr1"`, `name="aggr1"`, `type="netapp-aggr"`, and `labels={ontap_cluster:"ontap-prod", node:"ontap-prod-01"}` with no `cluster` key

#### Scenario: HA takeover moves ownership, not identity

- **WHEN** the same aggregate `aggr1` is observed owned by `ontap-prod-02` in a later window after a takeover
- **THEN** the node keeps `id="netapp/ontap-prod/aggr/aggr1"` while its `labels.node` (and hence its compound parent) becomes `ontap-prod-02`

#### Scenario: Unreferenced aggregate not materialised

- **WHEN** Harvest reports `volume_labels` series for an aggregate whose `volume_name` values match no PVC's resolved PV name
- **THEN** no `netapp-aggr` node is materialised for that aggregate

#### Scenario: Shared filer is one node set across Kubernetes clusters

- **WHEN** a PVC in Kubernetes cluster `cluster-alpha` and a PVC in `cluster-beta` both join `volume_labels` series on aggregate `(ontap-prod, aggr1)`
- **THEN** the graph contains exactly one `netapp/ontap-prod/aggr/aggr1` node with one `pvc-to-netapp-aggr` edge from each PVC, and no ONTAP cluster name appears in `clusters[]`

### Requirement: NetApp node entity and health

The builder SHALL materialise each ONTAP controller referenced by at least one emitted `netapp-aggr` node (its `labels.node`) as a `type="netapp-node"` graph node:

- `id` SHALL be `netapp/<ontap-cluster>/<node>`.
- `name` SHALL be `<node>` (the controller name).
- `labels` SHALL be exactly `{ontap_cluster: "<ontap-cluster>"}` — no `cluster` key; the same `clusters[]` / `?cluster=` exclusion as the aggregate.

The node's health SHALL derive from the OPTIONAL Harvest series `node_new_status` (fixed, case-sensitive labels: `cluster` — the ONTAP cluster, `node`; sample value `1` = the controller is healthy, any other value = not healthy), matched on `(ontap-cluster, node)`:

- the matched sample is `1` → `data.health = "online"`
- the matched sample is not `1` → `data.health = "degraded"`
- no matched series (or the metric absent entirely) → the `health` attribute is **omitted**.

Absence of data SHALL stay distinct from a reported unhealthy state — the builder SHALL NOT default a missing metric to `"degraded"` (the `ready_status` absent-vs-Unknown precedent). On duplicate series for one `(ontap-cluster, node)` the builder SHALL derive deterministically (any non-`1` sample → `"degraded"`, order-free). Failure of the `node_new_status` query SHALL degrade gracefully (health omitted on every node) and SHALL NOT fail the build.

A NetApp node SHALL be materialised ONLY via aggregate reference — never wholesale from Harvest series presence — and SHALL NOT carry `ipaddress`, `owner`, `application`, `containers`, or `ready_status`. It acts as the **compound parent** of its aggregates (graph-api "Cytoscape compound node grouping") and is the target of no edge.

#### Scenario: Node identity, labels, and health

- **WHEN** an emitted aggregate carries `labels.node="ontap-prod-01"` in ONTAP cluster `ontap-prod` and `node_new_status{cluster="ontap-prod", node="ontap-prod-01"}` has value `1`
- **THEN** the graph contains a node with `id="netapp/ontap-prod/ontap-prod-01"`, `name="ontap-prod-01"`, `type="netapp-node"`, `labels={ontap_cluster:"ontap-prod"}`, and `data.health="online"`

#### Scenario: Unhealthy controller

- **WHEN** `node_new_status` for a materialised controller has value `0`
- **THEN** that node carries `data.health="degraded"`

#### Scenario: Absent node-status metric omits the attribute

- **WHEN** no `node_new_status` series matches a materialised NetApp node (or the metric is absent from the window)
- **THEN** that node's `data` has no `health` key — absence is never conflated with `"degraded"`

#### Scenario: Controller not referenced by any aggregate is not materialised

- **WHEN** `node_new_status` reports a controller that owns no emitted aggregate
- **THEN** no `netapp-node` node is materialised for it

### Requirement: NetApp aggregate health and usage

Each materialised `netapp-aggr` node SHALL be able to carry two typed attributes, both `omitempty`, both outside `labels`:

- `health` — from the OPTIONAL Harvest series `aggr_new_status` (fixed, case-sensitive labels: `cluster`, `node`, `aggr`; sample value `1` = the aggregate is online, any other value = not online), matched on `(ontap-cluster, aggr)` — a 1:1 per-aggregate read, with NO cross-aggregate derivation: the sample is `1` → `"online"`; not `1` → `"degraded"`; no matched series → the attribute is **omitted** (absence is distinct from a reported unhealthy state, never conflated). On duplicate series the derivation is deterministic (any non-`1` sample → `"degraded"`, order-free).
- `usage` — an object `{used_bytes, capacity_bytes}` from the OPTIONAL Harvest series `aggr_space_used` / `aggr_space_total` (same label contract as `aggr_new_status`), matched on `(ontap-cluster, aggr)` — the **same shape** the PVC gains from kubelet volume stats. Per-field independent: each field present iff its own series matched; the object present iff at least one field resolved; values are JSON numbers (bytes), never strings. On duplicate series the builder SHALL pick deterministically (the smallest numeric value).

All three metrics are OPTIONAL; their absence degrades gracefully (attributes omitted) and SHALL NOT fail the build.

#### Scenario: Online aggregate with usage

- **WHEN** `aggr_new_status{cluster="ontap-prod", node="ontap-prod-01", aggr="aggr1"}` has value `1`, `aggr_space_used{...aggr="aggr1"}` is `700000000000`, and `aggr_space_total{...aggr="aggr1"}` is `1000000000000`
- **THEN** the `netapp/ontap-prod/aggr/aggr1` node carries `data.health: "online"` and `data.usage: {"used_bytes": 700000000000, "capacity_bytes": 1000000000000}`

#### Scenario: Offline aggregate marks health degraded

- **WHEN** `aggr_new_status` for a materialised aggregate has value `0`
- **THEN** that aggregate node carries `data.health: "degraded"`

#### Scenario: Absent status metric omits health

- **WHEN** no `aggr_new_status` series matches a materialised aggregate
- **THEN** that node's `data` has no `health` key — absence is never conflated with `"degraded"`

#### Scenario: Partial usage keeps only the resolved field

- **WHEN** an aggregate matches `aggr_space_total` but no `aggr_space_used` series
- **THEN** its `data.usage` equals `{"capacity_bytes": <value>}` with no `used_bytes` key

### Requirement: PVC-to-NetApp-aggregate edge join

For every PVC entity whose resolved PV name (`volumename`) is non-empty, the builder SHALL join that PV name against the `volume_name` label of the Harvest `volume_labels` series. On a match with a **non-empty `aggr` label** it SHALL emit one directed `pvc-to-netapp-aggr` edge from the PVC node to the NetApp aggregate node `netapp/<ontap-cluster>/aggr/<aggr>` derived from the same matched series' `cluster` and `aggr` labels — no separate topology query is issued. The edge is a pure function of this one family: whether it is drawn SHALL NOT depend on the QoS families, which only decide what it carries. The join key is the PV name alone (CSI-provisioned PV names are UUID-derived, so cross-cluster collisions are not a practical concern).

The edge SHALL carry empty `labels` (`{}`), a deterministic UUIDv5 `id` (canonical input `<type>|<source>|<target>`), and SHALL de-duplicate by `(pvc, netapp-aggr)`. When matched series disagree on the containing aggregate for one PV name, the builder SHALL pick deterministically — the lexically-smallest `(ontap-cluster, aggr)` pair — so the emitted edge set is byte-stable across rebuilds, independent of vector order. A PVC with no resolved `volumename` SHALL emit no `pvc-to-netapp-aggr` edge. A matched series whose `aggr` label is **empty** (the FlexGroup shape) SHALL emit no edge and SHALL be counted by the join-coverage signal.

#### Scenario: Joined claim emits the edge

- **WHEN** PVC `cluster-alpha/db/data-mongo-0` resolves `volumename="pvc-9f3a"` and a `volume_labels` series carries `volume_name="pvc-9f3a"`, `cluster="ontap-prod"`, `node="ontap-prod-01"`, `aggr="aggr1"`
- **THEN** the graph contains a directed `pvc-to-netapp-aggr` edge from `cluster-alpha/db/data-mongo-0` to `netapp/ontap-prod/aggr/aggr1` with empty `labels`

#### Scenario: PVC without a PV name emits no edge

- **WHEN** a PVC entity has no resolved `volumename`
- **THEN** no `pvc-to-netapp-aggr` edge originates from it and the build does not fail

#### Scenario: Matched series with an empty aggr label emits no edge

- **WHEN** a claim's PV name matches a `volume_labels` series whose `aggr` label is empty (a FlexGroup volume spanning aggregates)
- **THEN** no `pvc-to-netapp-aggr` edge is emitted for that claim, the claim is counted by the join-coverage signal, and the build does not fail

#### Scenario: Deterministic pick on conflicting aggregates

- **WHEN** two matched series for `volume_name="pvc-9f3a"` report `(ontap-prod, aggr-b)` and `(ontap-prod, aggr-a)`
- **THEN** the edge targets `netapp/ontap-prod/aggr/aggr-a` (the lexically-smallest pair) deterministically across rebuilds

#### Scenario: Edge id stable across rebuilds

- **WHEN** the same `(pvc, netapp-aggr)` join is produced by two consecutive builds for the same window
- **THEN** the edge `id` (UUIDv5 over `<type>|<source>|<target>`) is byte-identical between the two builds

### Requirement: I/O measurements on the storage edge

Each `pvc-to-netapp-aggr` edge SHALL be able to carry up to six I/O measurements in its `data.metrics` object, each sourced from its own Harvest QoS workload family (read at `lun=""` — see "Harvest QoS workload series as the I/O source") for the claim's PV name:

- `read_ops` ← `qos_read_ops` — read requests per second (verbatim).
- `write_ops` ← `qos_write_ops` — write requests per second (verbatim).
- `read_latency_us` ← `qos_read_latency` — average read latency in microseconds (verbatim).
- `write_latency_us` ← `qos_write_latency` — average write latency in microseconds (verbatim).
- `read_bytes_per_sec` ← `qos_read_data` — read throughput in bytes per second (verbatim).
- `write_bytes_per_sec` ← `qos_write_data` — write throughput in bytes per second (verbatim).

The same `data.metrics` object additionally carries the volume's declared ceiling — `max_iops` and `max_bytes_per_sec` — under the separate presence rules of "QoS fixed-policy throughput ceilings". Measurements and ceiling are ONE family; a single edge MAY carry any subset of the eight fields, subject to the rule that no ceiling field appears without at least one measurement.

Each measurement field SHALL be present iff its own family matched at least one series for the join key, and absent otherwise — an absent field is distinct from `0`. When a family matches more than one series for one join key, the value SHALL be summed over the matched series in ascending order so the result is order-independent. Values are JSON numbers rounded to 6 significant digits at serialisation (graph-api "Edge `metrics` attribute") and MAY appear in exponent form. The RED fields (`rate`, `error_rate`, `p90_server_ms`) SHALL NEVER appear on a `pvc-to-netapp-aggr` edge, and an edge on which every I/O field is absent SHALL carry no `metrics` key at all.

#### Scenario: All six measurements present

- **WHEN** all six Harvest QoS families carry a volume-level series for the joined PV name
- **THEN** the edge's `data.metrics` contains numeric `read_ops`, `write_ops`, `read_latency_us`, `write_latency_us`, `read_bytes_per_sec`, and `write_bytes_per_sec`, and none of `rate` / `error_rate` / `p90_server_ms`

#### Scenario: Missing family omits only its field

- **WHEN** the joined PV name matches `qos_read_ops`, `qos_write_ops`, `qos_read_data`, and `qos_write_data` series but no latency series
- **THEN** the edge's `data.metrics` contains `read_ops`, `write_ops`, `read_bytes_per_sec`, and `write_bytes_per_sec` and has no `read_latency_us` or `write_latency_us` key

#### Scenario: Verbatim values, no rate() derivation

- **WHEN** the matched `qos_read_ops` series' value is `150`, the matched `qos_read_latency` series' value is `830`, and the matched `qos_read_data` series' value is `5242880`
- **THEN** the edge reports `read_ops: 150`, `read_latency_us: 830`, and `read_bytes_per_sec: 5242880` — the upstream values verbatim, not re-derived

#### Scenario: Measurements and ceiling share one object

- **WHEN** a joined claim matches all six QoS families and its policy group resolves both fixed-policy series
- **THEN** the edge's single `data.metrics` object carries the six measurements together with `max_iops` and `max_bytes_per_sec`, and no RED field

### Requirement: PVC svm label re-sourced from the Harvest join

When the PVC join resolves, the builder SHALL set the PVC entity's `svm` label from the `svm` label of the matched Harvest `volume_labels` series — the Trident custom-resource chain is removed and this is the ONLY source of `svm`. The QoS workload series carry an `svm` label of their own, but it is read solely as part of the fixed-policy join key and SHALL NEVER serve as a fallback, so the emitted label cannot depend on which I/O families matched. The label's shape contract is unchanged from the Trident era: the key SHALL be set only when the resolved value is non-empty (absent, never empty-string), and `svm` SHALL never be present without `volumename` (the join is rooted at the PV name). When matched series disagree on `svm` for one join key, the builder SHALL pick the lexically-smallest non-empty value deterministically. The `svm` label resolves from any matched `volume_labels` series regardless of its `aggr` label — a FlexGroup claim that draws no aggregate edge still gains its `svm`.

#### Scenario: A QoS series on another SVM neither overrides nor measures

- **WHEN** a claim's `volume_labels` series carries `svm="svm-a"` while the only QoS series for its PV name carries `svm="svm-b"`
- **THEN** the PVC entity's `svm` label is `svm-a`, the edge carries no `metrics` key, and the claim is counted by the I/O-coverage signal

#### Scenario: svm resolved from the joined series

- **WHEN** PVC `cluster-alpha/db/data-mongo-0` joins a `volume_labels` series carrying `svm="svm-prod"`
- **THEN** the PVC entity's `labels` contains `volumename` and `svm="svm-prod"`

#### Scenario: Join miss yields no svm

- **WHEN** a PVC resolves `volumename="pvc-9f3a"` but no `volume_labels` series carries `volume_name="pvc-9f3a"`
- **THEN** the PVC entity carries `volumename` but no `svm` key, and the build does not fail

#### Scenario: Empty svm label on the joined series

- **WHEN** the joined `volume_labels` series carries an empty `svm` label
- **THEN** the PVC entity carries no `svm` key — never an empty-string value

### Requirement: Join-coverage observability

The join has two independently failing halves, and the builder SHALL count and surface each on its own. Both counts are per build, each is surfaced as ONE aggregated warning log carrying its count (the `failed_total_label_set_mismatch` precedent) rather than one log per claim, and neither SHALL ever fail the build:

- **Topology coverage** (`netapp_volume_join_miss`) — a PVC with a non-empty `volumename` that either matched no `volume_labels` series, or matched only series whose `aggr` label is empty (the FlexGroup shape — the claim's `svm` may still resolve, but no aggregate edge can be drawn), **while at least one `volume_labels` series was read in the build**.
- **I/O coverage** (`netapp_qos_join_miss`) — a claim that DID draw its `pvc-to-netapp-aggr` edge but matched no series in any of the six QoS workload families, leaving that edge with no measurements at all, **while at least one QoS workload series was read in the build**.

Each signal is gated on its OWN family being present in the window. A deployment running Harvest's volume template without the QoS template therefore gets its storage topology and no spurious I/O warning; a non-NetApp deployment (neither family present) stays silent on both — absence of the upstream is not a coverage failure. No signal is emitted for an unresolved throughput ceiling: a volume in no QoS policy group is the normal case, not a defect.

The PVC's retained `data.storageclass` value is the operator's discriminator between "this claim was never meant to have a NetApp backend" and "this claim should have joined and did not"; the builder itself SHALL NOT interpret StorageClass names or filter either count by them.

#### Scenario: Unjoined claims counted and warned once

- **WHEN** a build reads a non-empty `volume_labels` vector and three PVCs with non-empty `volumename` match no series
- **THEN** the build succeeds and emits one `netapp_volume_join_miss` warning carrying the count `3`

#### Scenario: Empty-aggr matches count toward the topology signal

- **WHEN** a build reads a non-empty `volume_labels` vector and one PVC's only matched series carries an empty `aggr` label
- **THEN** that claim is included in the `netapp_volume_join_miss` count even though its `svm` label may have resolved

#### Scenario: Measurement-less edges counted separately

- **WHEN** a build reads a non-empty QoS workload vector and two claims draw their aggregate edge but match no QoS workload series
- **THEN** the build succeeds, both edges are emitted without a `metrics` key, one `netapp_qos_join_miss` warning carries the count `2`, and the `netapp_volume_join_miss` count is unaffected

#### Scenario: Volume template without the QoS template

- **WHEN** a build reads `volume_labels` series but zero QoS workload series
- **THEN** every joined claim's edge is emitted without a `metrics` key and no `netapp_qos_join_miss` warning is emitted

#### Scenario: Non-NetApp deployment stays silent

- **WHEN** a build reads zero `volume_labels` series and zero QoS workload series while PVCs carry `volumename` labels
- **THEN** neither warning is emitted and the build succeeds

#### Scenario: Full coverage emits no warning

- **WHEN** every PVC with a non-empty `volumename` joins a `volume_labels` series with a non-empty `aggr` label and matches at least one QoS workload family
- **THEN** neither warning is emitted

### Requirement: Harvest legs under request-scoped selectors

Every NetApp Harvest query the builder issues — `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, and `node_new_status` — SHALL carry **no request-scoped matcher of any kind**: not `az`, not `env`, and never `cluster` or `namespace`. Harvest's `cluster` label is the **ONTAP** cluster name and never a Kubernetes cluster, so a Kubernetes `cluster` value pushed into it would match nothing; Harvest carries no `namespace` at all; and the `az` dimension reaches Harvest through backend selection alone (below), so a Harvest series need not carry the configured `az` / `env` labels. The `qos_*` families keep their fixed `lun=""` selector, which is therefore the only selector any Harvest query carries under any request.

These queries constitute the `harvest` query family of the `upstream-backend-routing` capability, so they MAY be served by a different upstream installation from the kube-state-metrics and kubelet legs. The family is **zone-routed**: a request's `az` values select which `harvest` backends are asked — those whose `zones` intersect the request, plus any catch-all — under the same rule as the `ksm` and `kubelet` families. Unlike those families, the selected zone is NOT additionally rendered as a matcher: for Harvest the zone boundary is the store, not a label on the series. The `env` dimension has no routing counterpart and SHALL have no effect on the Harvest legs whatsoever. Routing changes **which** installation answers a Harvest query; it changes neither the query string, the three-hop join, nor the per-hop degradation below.

Within a filtered build the storage chain is therefore narrowed **by reference**: an aggregate and its owning controller materialise only when a **loaded** claim's `volumename` joins a `volume_labels` series, so a `cluster`, `namespace`, or `env` filter reaches the NetApp graph solely through the claims it loads, and an `az` filter reaches it through the claims it loads plus the backends it selects. A filer shared across clusters, zones, or environments is one node set, reached from whichever loaded claims join it. Under a catch-all `harvest` backend, or under any `env` value, the Harvest read is the whole estate and the narrowing is by reference alone; a `volume_name` carried by volumes in two zones or environments resolves to the lexically-smallest `(ontap_cluster, aggr)` — the same collision rule an unfiltered build already applies, since an unfiltered build reads every zone.

#### Scenario: Cluster and namespace filters never reach Harvest

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** every Harvest query is issued exactly as in an unfiltered build (the `qos_*` families with `lun=""` only), and the aggregates in the response are exactly those joined by the loaded `cluster-alpha` / `shop` claims

#### Scenario: Zone filter reaches Harvest

- **WHEN** two backends serve `harvest`, declaring `zones: [zone-a]` and `zones: [zone-b]`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued only to the `zone-a` backend, as the bare unfiltered query string (`last_over_time(volume_labels[<window>])`, the `qos_*` families with `lun=""` only) carrying no `az` matcher; a `volume_labels` series held by the `zone-b` backend is not loaded even if a loaded claim's `volumename` would join it

#### Scenario: Catch-all Harvest backend under a zone filter

- **WHEN** one backend serves `harvest` with no `zones` declared and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued to that backend as the bare unfiltered query string, every zone's Harvest series is loaded, and the aggregates in the response are exactly those joined by the loaded `zone-a` claims

#### Scenario: Environment filter does not reach Harvest

- **WHEN** a build runs with `env={prod}` against a single `harvest` backend holding series stamped `env="prod"` and `env="dev"`
- **THEN** every Harvest query is issued as the bare unfiltered query string, both environments' series are loaded, and a loaded `prod` claim whose `volumename` joins a `volume_labels` series stamped `env="dev"` still receives its `pvc-to-netapp-aggr` edge

#### Scenario: Harvest lacking the environment label under an env filter

- **WHEN** the kube-state-metrics series carry `az="zone-a"`, the Harvest series carry no `az` and no `env` label at all, and a build runs with `az={zone-a}` or `env={prod}`
- **THEN** every Harvest leg returns its rows, the `pvc-to-netapp-aggr` edges are drawn for the loaded claims that join, and the selector-coverage Warn never names a Harvest family

#### Scenario: Harvest served by its own upstream

- **WHEN** the routing table declares one backend serving `harvest` at `http://vm-netapp.example:8428` and another serving every other family at `http://vm-k8s.example:8428`
- **THEN** the thirteen Harvest queries are issued only to `http://vm-netapp.example:8428`, the kube-state-metrics and kubelet queries only to `http://vm-k8s.example:8428`, and the resulting `pvc-to-netapp-aggr` edges join claims read from one upstream to volumes read from the other

#### Scenario: Unfiltered build merges Harvest across backends

- **WHEN** two backends serve `harvest` for different zones and a build runs with no `az` value
- **THEN** every Harvest query is issued to both and the results are merged, so aggregates from both zones can join their claims in one graph

#### Scenario: Shared filer reached by reference from either filtered cluster

- **WHEN** claims in `cluster-alpha` and `cluster-beta` both join `netapp/ontap-prod/aggr/aggr1` and a build runs with `cluster={cluster-alpha}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` are materialised with a `pvc-to-netapp-aggr` edge from the `cluster-alpha` claim only; a `cluster={cluster-beta}` build materialises the same two nodes from the `cluster-beta` claim

