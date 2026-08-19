## Purpose

Surfaces the physical NetApp ONTAP storage behind Kubernetes claims: one graph node per ONTAP aggregate and per ONTAP controller, a PVC-to-aggregate edge derived from a single Harvest join, per-edge I/O measurements, per-aggregate health and usage, controller health, and a coverage signal for claims that should have joined but did not.

## ADDED Requirements

### Requirement: Harvest volume series as the storage join source

The builder SHALL consume the NetApp Harvest volume-object series `volume_read_ops`, `volume_write_ops`, `volume_read_latency`, `volume_write_latency`, `volume_read_data`, and `volume_write_data` from the same centralised VictoriaMetrics endpoint as every other series. The fixed, case-sensitive label contract each series MUST carry: `cluster` (the ONTAP cluster name — NOT a Kubernetes cluster; the two namespaces never mix), `node` (the ONTAP controller currently owning the containing aggregate), `aggr` (the containing aggregate), `svm` (the serving Storage Virtual Machine), and `volume_name` (the name of the Kubernetes PersistentVolume the FlexVol backs).

Values SHALL be read **verbatim**: Harvest already resolves ONTAP's base counters, so the ops series are per-second rates, the latency series are averages in microseconds, and the data series are throughput in bytes per second. The issued queries SHALL NOT wrap these series in `rate()` — the opposite of the service-graph RED counters, where the upstream series are raw counters.

`volume_name` is NOT a stock Harvest label — it is produced by the deployment's own Prometheus relabel rule mapping each FlexVol to the PV it backs. That relabel rule is a **deployment precondition** with three known blind spots the graph inherits: a FlexVol whose name does not match the rule carries no `volume_name` (its claim never joins); the Trident "economy" drivers pack many claims into one shared FlexVol, so no per-claim volume series exists at all; and a FlexGroup volume spans aggregates, so its series carries no single usable `aggr` label (no aggregate edge can be drawn — see the join-coverage requirement).

All six families are OPTIONAL. When none is present in the window — the normal case for a deployment without NetApp Harvest — the builder SHALL produce a valid graph with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` labels; PVC `volumename` labels are unaffected and the build SHALL NOT fail.

#### Scenario: Volume series read verbatim without rate()

- **WHEN** the builder issues the six Harvest volume queries for a window
- **THEN** each query string references the bare series (e.g. an aggregation over `volume_read_ops` evaluated at the window end) and none wraps the series in `rate()`

#### Scenario: Harvest absent entirely

- **WHEN** the upstream contains topology series but no `volume_read_ops`, `volume_write_ops`, `volume_read_latency`, `volume_write_latency`, `volume_read_data`, or `volume_write_data` series for the window
- **THEN** the build completes successfully with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` labels, while PVC `volumename` labels still resolve from `kube_persistentvolumeclaim_info`

### Requirement: NetApp aggregate entity

The builder SHALL materialise each ONTAP aggregate referenced by at least one joined claim as a `type="netapp-aggr"` graph node:

- `id` SHALL be `netapp/<ontap-cluster>/aggr/<aggr>` (aggregate names are cluster-wide unique in ONTAP; the id deliberately **excludes the owning node**, so an HA takeover moves ownership without changing the aggregate's identity).
- `name` SHALL be `<aggr>` (the aggregate name).
- `labels` SHALL be exactly `{ontap_cluster: "<ontap-cluster>", node: "<node>"}` — the `node` value is the controller **currently owning** the aggregate (from the matched series' `node` label) and drives the compound nesting under the real `netapp-node` (graph-api "Cytoscape compound node grouping"). Deliberately **no `cluster` key** — the aggregate belongs to no Kubernetes cluster.

When matched series disagree on the owning `node` for one aggregate (e.g. a takeover inside the window), the builder SHALL pick deterministically — the lexically-smallest non-empty `node` value — so the emitted labels are byte-stable across rebuilds. Because the aggregate carries no `cluster` label, its ONTAP cluster name SHALL NEVER appear in the response's top-level `clusters[]` array, and a `?cluster=` filter SHALL never match it directly. A filer shared by several Kubernetes clusters SHALL yield **one** aggregate node per aggregate — the same id — with `pvc-to-netapp-aggr` edges arriving from every Kubernetes cluster that uses it.

A NetApp aggregate SHALL be materialised ONLY via the PVC join — never wholesale from Harvest series presence. It SHALL NOT carry `ipaddress`, `owner`, `application`, `containers`, or `ready_status`.

#### Scenario: Aggregate identity and labels

- **WHEN** a claim joins a Harvest volume series with `cluster="ontap-prod"`, `node="ontap-prod-01"`, `aggr="aggr1"`
- **THEN** the graph contains a node with `id="netapp/ontap-prod/aggr/aggr1"`, `name="aggr1"`, `type="netapp-aggr"`, and `labels={ontap_cluster:"ontap-prod", node:"ontap-prod-01"}` with no `cluster` key

#### Scenario: HA takeover moves ownership, not identity

- **WHEN** the same aggregate `aggr1` is observed owned by `ontap-prod-02` in a later window after a takeover
- **THEN** the node keeps `id="netapp/ontap-prod/aggr/aggr1"` while its `labels.node` (and hence its compound parent) becomes `ontap-prod-02`

#### Scenario: Unreferenced aggregate not materialised

- **WHEN** Harvest reports volume series for an aggregate whose `volume_name` values match no PVC's resolved PV name
- **THEN** no `netapp-aggr` node is materialised for that aggregate

#### Scenario: Shared filer is one node set across Kubernetes clusters

- **WHEN** a PVC in Kubernetes cluster `cluster-alpha` and a PVC in `cluster-beta` both join volume series on aggregate `(ontap-prod, aggr1)`
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

For every PVC entity whose resolved PV name (`volumename`) is non-empty, the builder SHALL join that PV name against the `volume_name` label of the Harvest volume series. On a match with a **non-empty `aggr` label** it SHALL emit one directed `pvc-to-netapp-aggr` edge from the PVC node to the NetApp aggregate node `netapp/<ontap-cluster>/aggr/<aggr>` derived from the same matched series' `cluster` and `aggr` labels — no separate topology query is issued. The join key is the PV name alone (CSI-provisioned PV names are UUID-derived, so cross-cluster collisions are not a practical concern).

The edge SHALL carry empty `labels` (`{}`), a deterministic UUIDv5 `id` (canonical input `<type>|<source>|<target>`), and SHALL de-duplicate by `(pvc, netapp-aggr)`. When matched series disagree on the containing aggregate for one PV name, the builder SHALL pick deterministically — the lexically-smallest `(ontap-cluster, aggr)` pair — so the emitted edge set is byte-stable across rebuilds, independent of vector order. A PVC with no resolved `volumename` SHALL emit no `pvc-to-netapp-aggr` edge. A matched series whose `aggr` label is **empty** (the FlexGroup shape) SHALL emit no edge and SHALL be counted by the join-coverage signal.

#### Scenario: Joined claim emits the edge

- **WHEN** PVC `cluster-alpha/db/data-mongo-0` resolves `volumename="pvc-9f3a"` and a `volume_read_ops` series carries `volume_name="pvc-9f3a"`, `cluster="ontap-prod"`, `node="ontap-prod-01"`, `aggr="aggr1"`
- **THEN** the graph contains a directed `pvc-to-netapp-aggr` edge from `cluster-alpha/db/data-mongo-0` to `netapp/ontap-prod/aggr/aggr1` with empty `labels`

#### Scenario: PVC without a PV name emits no edge

- **WHEN** a PVC entity has no resolved `volumename`
- **THEN** no `pvc-to-netapp-aggr` edge originates from it and the build does not fail

#### Scenario: Matched series with an empty aggr label emits no edge

- **WHEN** a claim's PV name matches a Harvest volume series whose `aggr` label is empty (a FlexGroup volume spanning aggregates)
- **THEN** no `pvc-to-netapp-aggr` edge is emitted for that claim, the claim is counted by the join-coverage signal, and the build does not fail

#### Scenario: Deterministic pick on conflicting aggregates

- **WHEN** two matched series for `volume_name="pvc-9f3a"` report `(ontap-prod, aggr-b)` and `(ontap-prod, aggr-a)`
- **THEN** the edge targets `netapp/ontap-prod/aggr/aggr-a` (the lexically-smallest pair) deterministically across rebuilds

#### Scenario: Edge id stable across rebuilds

- **WHEN** the same `(pvc, netapp-aggr)` join is produced by two consecutive builds for the same window
- **THEN** the edge `id` (UUIDv5 over `<type>|<source>|<target>`) is byte-identical between the two builds

### Requirement: I/O measurements on the storage edge

Each `pvc-to-netapp-aggr` edge SHALL be able to carry up to six I/O measurements in its `data.metrics` object, each sourced from its own Harvest family for the claim's PV name:

- `read_ops` ← `volume_read_ops` — read requests per second (verbatim).
- `write_ops` ← `volume_write_ops` — write requests per second (verbatim).
- `read_latency_us` ← `volume_read_latency` — average read latency in microseconds (verbatim).
- `write_latency_us` ← `volume_write_latency` — average write latency in microseconds (verbatim).
- `read_bytes_per_sec` ← `volume_read_data` — read throughput in bytes per second (verbatim).
- `write_bytes_per_sec` ← `volume_write_data` — write throughput in bytes per second (verbatim).

Each field SHALL be present iff its own family matched at least one series for the join key, and absent otherwise — an absent field is distinct from `0`. When a family matches more than one series for one join key, the value SHALL be summed over the matched series in ascending order so the result is order-independent. Values are JSON numbers rounded to 6 significant digits at serialisation (graph-api "Edge `metrics` attribute") and MAY appear in exponent form. The RED fields (`rate`, `error_rate`, `p90_server_ms`) SHALL NEVER appear on a `pvc-to-netapp-aggr` edge, and an edge whose every family is absent SHALL carry no `metrics` key at all.

#### Scenario: All six measurements present

- **WHEN** all six Harvest families carry a series for the joined PV name
- **THEN** the edge's `data.metrics` contains numeric `read_ops`, `write_ops`, `read_latency_us`, `write_latency_us`, `read_bytes_per_sec`, and `write_bytes_per_sec`, and none of `rate` / `error_rate` / `p90_server_ms`

#### Scenario: Missing family omits only its field

- **WHEN** the joined PV name matches `volume_read_ops`, `volume_write_ops`, `volume_read_data`, and `volume_write_data` series but no latency series
- **THEN** the edge's `data.metrics` contains `read_ops`, `write_ops`, `read_bytes_per_sec`, and `write_bytes_per_sec` and has no `read_latency_us` or `write_latency_us` key

#### Scenario: Verbatim values, no rate() derivation

- **WHEN** the matched `volume_read_ops` series' value is `150`, the matched `volume_read_latency` series' value is `830`, and the matched `volume_read_data` series' value is `5242880`
- **THEN** the edge reports `read_ops: 150`, `read_latency_us: 830`, and `read_bytes_per_sec: 5242880` — the upstream values verbatim, not re-derived

### Requirement: PVC svm label re-sourced from the Harvest join

When the PVC join resolves, the builder SHALL set the PVC entity's `svm` label from the `svm` label of the matched Harvest volume series — the Trident custom-resource chain is removed and this is the ONLY source of `svm`. The label's shape contract is unchanged from the Trident era: the key SHALL be set only when the resolved value is non-empty (absent, never empty-string), and `svm` SHALL never be present without `volumename` (the join is rooted at the PV name). When matched series disagree on `svm` for one join key, the builder SHALL pick the lexically-smallest non-empty value deterministically. The `svm` label resolves from any matched series regardless of its `aggr` label — a FlexGroup claim that draws no aggregate edge still gains its `svm`.

#### Scenario: svm resolved from the joined series

- **WHEN** PVC `cluster-alpha/db/data-mongo-0` joins a volume series carrying `svm="svm-prod"`
- **THEN** the PVC entity's `labels` contains `volumename` and `svm="svm-prod"`

#### Scenario: Join miss yields no svm

- **WHEN** a PVC resolves `volumename="pvc-9f3a"` but no Harvest volume series carries `volume_name="pvc-9f3a"`
- **THEN** the PVC entity carries `volumename` but no `svm` key, and the build does not fail

#### Scenario: Empty svm label on the joined series

- **WHEN** the joined volume series carries an empty `svm` label
- **THEN** the PVC entity carries no `svm` key — never an empty-string value

### Requirement: Join-coverage observability

The builder SHALL count claims that should have joined but did not: a PVC with a non-empty `volumename` that either matched no Harvest volume series, or matched only series whose `aggr` label is empty (the FlexGroup shape — the claim's `svm` may still resolve, but no aggregate edge can be drawn), **while at least one Harvest volume series was read in the build**. A non-zero per-build count SHALL be surfaced as one aggregated warning log (`netapp_volume_join_miss`, carrying the count — the `failed_total_label_set_mismatch` precedent), never one log per claim, and SHALL never fail the build. When no Harvest volume series exists in the window (a non-NetApp deployment), the signal SHALL stay silent — absence of the upstream is not a coverage failure.

The PVC's retained `data.storageclass` value is the operator's discriminator between "this claim was never meant to have a NetApp backend" and "this claim should have joined and did not"; the builder itself SHALL NOT interpret StorageClass names or filter the count by them.

#### Scenario: Unjoined claims counted and warned once

- **WHEN** a build reads a non-empty Harvest volume vector and three PVCs with non-empty `volumename` match no series
- **THEN** the build succeeds and emits one `netapp_volume_join_miss` warning carrying the count `3`

#### Scenario: Empty-aggr matches count toward the signal

- **WHEN** a build reads a non-empty Harvest volume vector and one PVC's only matched series carries an empty `aggr` label
- **THEN** that claim is included in the `netapp_volume_join_miss` count even though its `svm` label may have resolved

#### Scenario: Non-NetApp deployment stays silent

- **WHEN** a build reads zero Harvest volume series while PVCs carry `volumename` labels
- **THEN** no `netapp_volume_join_miss` warning is emitted and the build succeeds

#### Scenario: Full coverage emits no warning

- **WHEN** every PVC with a non-empty `volumename` joins a Harvest volume series with a non-empty `aggr` label
- **THEN** no `netapp_volume_join_miss` warning is emitted
