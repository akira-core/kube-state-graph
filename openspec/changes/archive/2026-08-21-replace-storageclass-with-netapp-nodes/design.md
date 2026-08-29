# Design — replace-storageclass-with-netapp-nodes

## Context

See `proposal.md` — Why. The mechanics this design builds on, as they exist today:

- **Topology fan-out**: `pkg/build/topology.go` `ReadTopology` runs an 18-query
  `errgroup` (each leg `last_over_time(<series>[w])` evaluated at the window
  end), then a pure assembly pass bakes entities, typed attributes, and labels
  **before** `graph.NewGraph` freezes them. Three of those legs die with this
  change (`QStorageClassInfo`, `QTridentVolumeInfo`, `QTridentBackendInfo`).
- **Renderer**: `pkg/promql/queries.go` `Renderer{Prefix}` prepends
  `KSG_METRIC_PREFIX` to KSM-shaped series; the prefix is threaded through
  `build.Options`, `kubegraph.Options`, and `internal/config`. All of that
  plumbing is removed here.
- **Sealed node types**: `pkg/graph/node.go` — `GraphNode` with a fixed method
  set (`ID/Name/Type/Labels/IPAddress/Owner/Application/Containers/ReadyStatus`);
  serialisation goes through methods, never type switches. `StorageClassNode`
  is deleted; two NetApp types are added.
- **Edge metrics**: `graph.Edge.Metrics *EdgeMetrics` (RED; `Rate` non-pointer,
  invariant "present ⇒ rate > 0") serialised by `cytoscape.metricsDTO` with
  `round6`. The wire `metrics` object becomes a two-family union here.
- **Projection**: `graph.Project` — `connectivityExcluded` (pods/PVCs) +
  `infraNodePassesFilters` (deferred reference-driven admission for `node` /
  `storageclass`). The storageclass branch is replaced by a **transitive**
  NetApp chain (PVC → aggregate → controller).
- **Serialiser**: `pkg/cytoscape` synthesises group DTOs (`cluster`,
  `namespace`, `application`, `controller`) and assigns `data.parent` from each
  real node's own labels/attributes. It gains a `storage-cluster` group tier
  and the first **real-node compound parent** (`netapp-node` parenting
  `netapp-aggr`).
- **Upstream**: the centralised VictoriaMetrics already carries NetApp Harvest
  series (volume / QoS workload / QoS fixed-policy / aggregate / node objects)
  and kubelet volume stats. The deployment's relabel rule stamps `volume_name`
  (= the bound PV name) onto **both** the Harvest volume-object series and the
  QoS workload series.

The delta specs under `specs/` are the behaviour contract; this document covers
how to implement them.

## Goals / Non-Goals

**Goals:**

- One-key storage resolution: the PV name alone drives all three hops —
  `volume_labels` → aggregate id + owning controller + svm; QoS workload →
  six I/O figures + policy group; QoS fixed policy → throughput ceiling.
- Independently degradable hops: a missing QoS workload costs measurements, never
  the claim's storage topology.
- Deterministic, byte-stable output under every duplicate/conflict shape
  (duplicate PV names, conflicting `aggr`/`node`/`svm` values, takeover inside
  the window).
- Keep the RED invariant intact while widening the wire `metrics` object.
- Contain the real-node-compound-parent precedent break to exactly one tier.

**Non-Goals:**

- No SVM entity, no FlexVol entity, no aggregate→disk topology — the chain
  stops at `netapp-aggr` under `netapp-node`.
- No per-request Harvest filtering (the "no filters pushed to PromQL" rule is
  untouched; Harvest legs are request-invariant like every other leg).
- No FlexGroup support (blind spot recorded; degrades to coverage signal).
- No backward-compatibility shim for `KSG_METRIC_PREFIX` or the
  `storageclass` node type — this is a declared v1-breaking release.

## Decisions

### D1 — Harvest + kubelet legs join the existing `ReadTopology` errgroup

The thirteen new Harvest queries — one topology mapper (`volume_labels`), six
QoS workload I/O families (`qos_read_ops`, `qos_write_ops`, `qos_read_latency`,
`qos_write_latency`, `qos_read_data`, `qos_write_data`), two QoS fixed-policy
ceilings (`qos_policy_fixed_max_throughput_iops`,
`qos_policy_fixed_max_throughput_mbps`), and four aggregate/controller families
(`aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`) —
plus the two kubelet queries (`kubelet_volume_stats_used_bytes`,
`kubelet_volume_stats_capacity_bytes`) are added to the **existing**
`ReadTopology` fan-out (18 − 3 + 15 = 30 legs), not a third reader stage.

*Why:* the join is rooted at `kube_persistentvolumeclaim_info.volumename` and
its outputs (PVC `svm` label, PVC `usage`, aggregate/controller nodes, storage
edges) are all baked at topology assembly before `graph.NewGraph` — exactly
where PVC labels and attributes are baked today. A separate reader would add a
barrier and a cross-stage handoff for zero concurrency gain (the errgroup
already runs all legs in parallel).

*Alternative considered:* a `ReadStorage` stage parallel to
`ReadServiceGraph` — rejected; `ReadServiceGraph` is separate because it
depends on the **assembled** topology (pod-UID index), while the storage join
is itself assembly input.

All fifteen legs are OPTIONAL in the same sense as today's optional families: a
query error or empty vector degrades to absent nodes/edges/attributes, never a
build failure. NOTE: today a failed leg aborts the build (`g.Wait` error).
Harvest/kubelet legs MUST NOT — they wrap their fetch to log-and-continue
(empty vector on error), because a non-NetApp deployment must build cleanly
even if these series names 404 into empty vectors or the tenant blocks them.
Existing KSM legs keep their abort semantics (unchanged behaviour).

### D2 — Rendering: `last_over_time`, never `rate()`; Renderer loses `Prefix`

Every new leg renders as `last_over_time(<series>[<window>])` — the same shape
as the KSM gauges. Harvest has already resolved ONTAP base counters (ops are
per-second, latencies are averaged µs) and `aggr_new_status` /
`node_new_status` / space / kubelet series are gauges, so the reader takes the
**last observed value in the window, verbatim**. No `rate()`, no `avg_over_time`
(which would smear a takeover across owners), no `sum by` (label identity is
the join key). `volume_labels` is an info series — its value is discarded and
only its label set is consumed.

Two fixed selector facts ride on the rendered strings and are **not** caller
filters — they are request-invariant metric-selection contracts, the same class
as the D30 service-graph sentinel matcher and `kube_node_status_condition`'s
`condition="Ready"`, so the "no filters pushed to PromQL" rule is preserved:

- the six QoS I/O legs render as `last_over_time(qos_<family>{lun=""}[<window>])`.
  ONTAP collects a workload per LUN as well as per volume, and a LUN workload
  carries the `volume_name` of its containing FlexVol once the relabel rule has
  run — so an unfiltered read would sum LUN traffic on top of volume traffic for
  the same claim. `lun=""` is an exact, anchored match that also matches series
  carrying no `lun` label at all, so it stays correct against a Harvest template
  that omits the label entirely;
- the two `qos_policy_fixed_*` legs render bare — a policy object has no LUN
  dimension.

The one value NOT read verbatim is `qos_policy_fixed_max_throughput_mbps`:
`max_bytes_per_sec` is `mbps × 1048576`, so the ceiling carries the unit of the
measured `read_bytes_per_sec` / `write_bytes_per_sec` and the two compare with no
client-side arithmetic. Conversion happens once, in the resolver, before the
value reaches `IOMetrics`; the serialiser's `round6` then applies as to every
other field.

`Renderer` loses the `Prefix` field and becomes vestigial; the package-level
`promql.Render(q, window)` pure function is the single entry point.
`build.Options.MetricPrefix`, `kubegraph.Options.MetricPrefix`,
`internal/config.MetricPrefix` (+ its validation and flag/env wiring) are
deleted. `Query` constants stay bare series names, so `query=` self-metric
dimensions are unchanged for surviving queries; the three removed constants'
dimension values disappear with their queries.

### D3 — One resolver file owns the three-hop storage join (`pkg/build/netapp.go`)

A new assembly step `resolveNetAppStorage` (pure function, no I/O) consumes:

- the PVC→PV-name map already produced by `resolvePVCInfo`
- the `volume_labels` vector, keyed once into `volume_name → candidates` (hop A)
- the six QoS I/O vectors, keyed the same way (hop B)
- the two QoS fixed-policy vectors, keyed `(ontap_cluster, svm, policy)` (hop C)
- the aggregate status/space vectors, keyed `(ontap_cluster, aggr)`
- the node status vector, keyed `(ontap_cluster, node)`

and emits: per-PVC `svm` label values, per-PVC edge + `IOMetrics`, the
`netapp-aggr` node set, and the `netapp-node` node set.

The three hops are **independently degradable**, and that is the load-bearing
property of the split: hop A alone decides the graph's shape (edge + aggregate +
controller + `svm`), hop B decides whether that edge carries measurements, hop C
whether it also carries a ceiling. A hop-B miss therefore leaves a valid
measurement-less edge instead of erasing the claim's storage topology — the
opposite of the single-series design it replaces, where one miss cost
everything. Determinism rules, in resolution order:

1. **Aggregate pick per PV name** (hop A): lexically-smallest
   `(ontap_cluster, aggr)` pair across the matched `volume_labels` series.
   Empty-`aggr` candidates are excluded from the pick; if every candidate is
   empty-`aggr`, the claim draws no edge and counts toward the topology-coverage
   signal.
2. **Owner pick per aggregate** (hop A): lexically-smallest non-empty `node`
   across the matched `volume_labels` series of that aggregate
   (takeover-in-window collapses deterministically; the status series does NOT
   vote — it is OPTIONAL and must not change topology when absent).
3. **svm pick per PV name** (hop A): lexically-smallest non-empty `svm` across
   ALL matched `volume_labels` series (including empty-`aggr` ones — FlexGroup
   claims still get `svm`). `volume_labels` is the SOLE source of the PVC `svm`
   label; the QoS series' own `svm` is read only as part of hop C's join key and
   never as a fallback, so the label cannot depend on which I/O families matched.
4. **I/O values** (hop B): per family, sum over the in-scope QoS series in
   ascending value order (mirrors the RED contribution-sum rule); a field is
   present iff its family matched ≥ 1 series. Only an edge produced by hop A can
   carry them. *In scope* (`qosInScope`) means the candidate's `cluster` equals
   the picked aggregate's ONTAP cluster and its `svm` equals the claim's
   resolved SVM whenever both are non-empty — the hop-B analogue of the
   `(cluster, aggr)` scoping the single-series design needed, since a PV name
   colliding across two filers sharing one VictoriaMetrics would otherwise sum a
   foreign volume onto this edge. An `svm`-less candidate still measures the
   volume but cannot contribute a policy group.
5. **Policy pick per PV name** (hop B → C): lexically-smallest non-empty
   `(ontap_cluster, svm, policy_group)` triple across the in-scope QoS series,
   then looked up in the hop-C index. Each ceiling field is present iff its own
   family matched that triple; on duplicate series the smallest numeric value
   wins (mirrors the usage rule). `max_bytes_per_sec` is the converted `mbps`
   value (D2). Because `policy_group` is read off the QoS series, a ceiling can
   never appear on an edge that carries no measurements.
6. **Health**: per aggregate / per controller — all matched status samples
   `== 1` → `"online"`, any `!= 1` → `"degraded"`, none → absent.
7. **Usage**: per aggregate, smallest value per field on duplicates (mirrors
   the kubelet PVC-usage rule).

Node materialisation is strictly demand-driven: aggregates only from joined
claims, controllers only from emitted aggregates' `labels.node`. The resolver
returns sorted slices (by id) so assembly order is canonical.

### D4 — Sealed-interface extension: `Health()` and `Usage()` accessors

`GraphNode` gains two methods, following the `ReadyStatus()` precedent exactly:

- `Health() string` — `""` for every type except `NetAppAggrNode` /
  `NetAppNode`; `"online"` / `"degraded"` when resolved.
- `Usage() *UsageBytes` — `nil` for every type except `PVCNode` (kubelet) and
  `NetAppAggrNode` (aggr space). `graph.UsageBytes{UsedBytes, CapacityBytes
  *float64}` — pointer fields for per-field omission.

*Why one shared `Usage()` for PVC and aggregate:* the wire shape is declared
identical in the specs (`{used_bytes, capacity_bytes}`), so a single accessor
gives the serialiser one code path and makes shape drift a compile error.

*Alternative considered:* serialiser type-switches on concrete types —
rejected; violates the "serialisation via sealed methods" rule.

`PVCNode` additionally surfaces `StorageClass()` (already exists) via a new
`data.storageclass` emission in the serialiser. `StorageClassNode` and its
`StorageClassInfo()` method are deleted; `NodeTypeStorageClass` is removed from
`node.go` and the registry.

### D5 — Edge metrics union: two typed values, one wire object

`graph.Edge` gains a second nullable field: `IO *IOMetrics` alongside the
existing `Metrics *EdgeMetrics`. `IOMetrics{ReadOps, WriteOps, ReadLatencyUs,
WriteLatencyUs, ReadBytesPerSec, WriteBytesPerSec, MaxIOPS, MaxBytesPerSec
*float64}` — all pointers (each field rides its own OPTIONAL family). The RED struct and its "present ⇒ Rate > 0" invariant are untouched.

At the boundary, `cytoscape.EdgeMetricsDTO` becomes the union:
`Rate` moves from `float64` to `*float64 omitempty` (the OpenAPI `required`
list drops it), and eight `omitempty` I/O fields are added (six measurements plus
the two ceilings). `metricsDTO` takes
`(m *graph.EdgeMetrics, io *graph.IOMetrics)` and fills exactly one family —
the builder never sets both (RED attaches in `servicegraph.go`, IO only in
`netapp.go`), so cross-family mixing is structurally impossible, and a
defensive precedence (RED wins) guards the impossible case. `round6` applies
to every field.

*Alternative considered:* separate `data.io_metrics` object — rejected in the
proposal: consumers keep one place to look (`data.metrics`).

### D6 — Registry and projection: transitive reference chain

`graph.EdgeTypes`: remove `pvc-to-storageclass`, add `pvc-to-netapp-aggr`
(`source: [pvc]`, `target: [netapp-aggr]`, `directed: true`,
`may_cross_cluster: false`, `labels: []`).

`graph.Project` admission (extending `infraNodePassesFilters`):

1. `netapp-aggr` admitted iff referenced by an **admitted** PVC via a
   `pvc-to-netapp-aggr` edge (same deferred-admission mechanics as `node`
   via `labels.node` today, driven off the edge index).
2. `netapp-node` admitted iff an admitted `netapp-aggr` names it in
   `labels.node` — computed **after** the aggregate pass (one extra
   admission wave; the reference set is a pure function of the admitted
   aggregates).
3. `?name=` escape hatch admits either type by `Name()` match, and an admitted
   aggregate ALWAYS pulls its owning controller in (the compound parent must
   exist — the dangling-parent guarantee moves from "path-encoded group ids"
   to this projection rule for the one real-parent tier).
4. Cluster filter: both types carry no `cluster` label — they pass the cluster
   check (like `external`) and are gated purely by reference.
5. `connectivityExcluded` is untouched — NetApp types are reference-gated, so
   pruned PVCs drop their aggregates (and then controllers) for free, exactly
   as pruned PVCs dropped StorageClasses.

### D7 — Serialiser: `storage-cluster` tier + one real compound parent

- `storage-cluster/<ontap-cluster>` groups are synthesised from the distinct
  `labels.ontap_cluster` of emitted `netapp-aggr` + `netapp-node` nodes;
  emitted in tier order **cluster, storage-cluster, namespace, application,
  controller**, each tier sorted by id.
- `compoundParent` gains: `netapp-node` → `storage-cluster/<ontap_cluster>`;
  `netapp-aggr` → `netapp/<ontap_cluster>/<labels.node>` — a **real node id**,
  legal in Cytoscape.js (any node may be a `parent`). The projection guarantee
  from D6.3 keeps it dangling-free.
- `ClusterNames()` already derives from `labels.cluster`, which both NetApp
  types lack — `clusters[]` exclusion falls out with no code. A unit test
  pins it anyway.
- `data.health`, `data.usage`, `data.storageclass` are emitted via the D4
  accessors with `omitempty`.

### D8 — Coverage signals: two aggregated Warns at resolve time

The two hops fail independently, so they are counted independently.
`resolveNetAppStorage` emits at most one Warn per signal per build — the
`failed_total_label_set_mismatch` pattern (aggregated, never per-claim, never
an error):

- **Topology** — claims with non-empty `volumename` whose hop-A join produced no
  edge (no `volume_labels` match at all, or only empty-`aggr` matches), **iff**
  at least one `volume_labels` series was read. Non-zero count ⇒ one
  `slog.Warn("netapp_volume_join_miss", "count", n)`.
- **I/O** — claims that DID draw an edge but matched no QoS workload series in
  any of the six families, **iff** at least one QoS series was read. Non-zero
  count ⇒ one `slog.Warn("netapp_qos_join_miss", "count", n)`.

Each signal is gated on its OWN family being present, so a deployment running
Harvest's volume template without the QoS template gets the topology graph and
no spurious I/O warning, and a non-NetApp deployment (neither family) stays
silent on both — absence of the upstream is not a coverage failure. No third
signal is added for hop C: a volume in no QoS policy group is the normal case,
not a defect.

### D9 — Test strategy

- **Unit** (`pkg/build/netapp_test.go`): hop-A join hit/miss, empty-`aggr`
  (FlexGroup), duplicate-series determinism at every pick, takeover-in-window,
  health mapping (incl. absence ≠ degraded), usage per-field, both
  coverage-count triggers. Hop B/C specifically: an edge drawn with NO QoS match
  carries no `metrics` key and trips only `netapp_qos_join_miss`; per-family
  presence/absence; multi-series ascending sum; policy-triple pick on conflicting
  `policy_group` values; ceiling per-field presence; the `mbps × 1048576`
  conversion; a QoS match whose policy resolves nothing (measurements, no
  ceiling); and the structural invariant that no ceiling field can appear
  without at least one measurement. `pkg/graph`: registry entry, projection transitive admission,
  `?name=` pull-in of the owning controller. `pkg/cytoscape`: tier order,
  real-parent assignment, metrics union DTO (RED-only, IO-only, neither).
- **Golden** (`internal/api/testdata/golden/`): replace
  `with-netapp-trident-cytoscape.json` with a `with-netapp-storage-cytoscape.json`
  covering aggr+node+edge+health+usage+svm; regenerate every golden touched by
  the storageclass removal and the `metrics` DTO change.
- **Property** (`pkg/graph/property_test.go`): extend generators with the two
  new types; invariant "aggr present ⇒ its parent controller present".
- **Integration** (`internal/integration/`): fixture Harvest + kubelet series
  through the VictoriaMetrics container; `TestPVCNetAppTridentLabels` becomes
  the Harvest-join equivalent; a no-Harvest suite run asserts byte-stable
  degradation.
- Deleted with their features: `trident_test.go`, `storageclass_test.go`
  (build/cytoscape/graph), prefix rendering tests.

### D10 — Landing order with `graph-api-gateway`

`kubegraph.Options.MetricPrefix` removal is an API break for the embedder.
Order: (1) this repo lands + tags; (2) `graph-api-gateway` bumps the
dependency and deletes its `MetricPrefix` wiring in the same PR. Until (2)
lands, the gateway simply stays on the old tag — Go module pinning makes the
coordination a normal version bump, not a lockstep deploy.

## Risks / Trade-offs

- **Harvest metric names drift by version/template** (`aggr_space_used` /
  `aggr_space_total` / `node_new_status` naming varies across Harvest
  releases) → verify all fifteen names against the production VictoriaMetrics
  (`/api/v1/label/__name__/values` grep) as the FIRST implementation task; a
  mismatch is a mechanical rename in specs + queries before any code depends
  on it.
- **The QoS policy label name is not the same on both sides of hop C** — the
  workload series carries `policy_group`, while `qos_policy_fixed_*` identifies
  the policy through its own instance label (`name` in the Harvest template that
  motivated this design) → the reader therefore reads `name` with a
  `policy_group` fallback rather than pinning one spelling, retiring the risk in
  code: the join SHAPE `(ontap_cluster, svm, policy)` is the contract, the
  spelling is not. Verification only confirms which spelling this deployment
  uses.
- **QoS workload coverage is strictly weaker than volume coverage** — every
  FlexVol has a volume-object series, but a QoS workload exists only where ONTAP
  collects one, so switching the I/O source can lose measurements that the
  `volume_*` families reported → this is exactly why hop A was split off onto
  `volume_labels`: the topology is unaffected, the loss is visible as
  `netapp_qos_join_miss`, and the edge degrades to no `metrics` rather than
  disappearing.
- **`volume_labels` is an instance-label export, not a counter** — a Harvest
  template configured without instance-label export publishes no such series,
  which would silently cost the entire storage topology → verified in the first
  task; the documented fallback is any always-present per-volume gauge carrying
  the same label set (e.g. `volume_size_used`), a mechanical rename confined to
  one query constant and the specs' label-contract lines.
- **ONTAP's `mbps` may be 10⁶ or 2²⁰ bytes** — the conversion constant decides
  whether the ceiling reads 4.8 % low against a `read_bytes_per_sec` measured in
  true bytes → the factor is pinned in one resolver constant with the verified
  basis recorded next to it; a correction is one constant and the affected
  goldens.
- **The `lun=""` contract is load-bearing against double counting** — if a
  Harvest template stamps a non-empty `lun` on volume-level workloads, or the
  relabel rule stamps `volume_name` on a workload shape not considered here, the
  filter silently drops or doubles a claim's I/O → the first task samples
  `/api/v1/series` for the `lun` label's real distribution, and the resolver's
  per-family ascending sum keeps whatever survives deterministic.
- **Relabel-rule coverage is invisible upstream** (a claim silently missing
  from Harvest) → D8 coverage signal + retained `data.storageclass` as the
  operator's discriminator.
- **PV-name collision across ONTAP clusters sharing one VM** (two filers
  backing two K8s clusters could theoretically carry the same `volume_name`)
  → CSI PV names are UUID-derived, collision is negligible; the
  lexically-smallest pick keeps even that case deterministic rather than
  order-dependent.
- **Real-node compound parent is a precedent break** → scoped in specs to the
  single `netapp-node > netapp-aggr` tier with an explicit prohibition on new
  real-parent tiers; projection rule D6.3 keeps the tree dangling-free.
- **Optional-leg error semantics diverge from KSM legs** (D1: log-and-continue
  vs abort) → contained to the twelve new legs; a comment on each leg documents
  why, and a unit test pins a failing Harvest leg not failing the build.
- **`rate` moving to optional in the OpenAPI schema** can break strict
  consumers of the RED object → release note; the RED family behaviour is
  unchanged (present ⇒ rate > 0), only the schema-level `required` weakens.
- **Cardinality of the Harvest reads** (one `volume_labels` series per FlexVol,
  plus one QoS series per volume-level workload × 6 families, plus one policy
  series × 2 families) → bounded by filer volume count (thousands, not
  millions), and `lun=""` keeps the LUN workloads — usually the larger
  population — out of the vectors entirely; same raw-vector pattern as the RED
  histogram read; upstream search limits govern.

## Migration Plan

1. Land specs + implementation + regenerated `docs/` + goldens in one PR
   (deterministic-body tests force lockstep anyway).
2. **Release note (BREAKING)**: `storageclass` node type + `pvc-to-storageclass`
   edge removed; `KSG_METRIC_PREFIX` / `--metric-prefix` removed (prefixed-KSM
   deployments silently return empty graphs — must republish at bare names
   before upgrading); `data.metrics.rate` now optional at the schema level.
3. Deployment precondition doc: the Harvest relabel rule for `volume_name`,
   which must now stamp **both** the volume-object and the QoS workload series;
   the Harvest templates required (volume instance labels, QoS workload, QoS
   fixed policy); the `lun=""` volume-granularity contract; and the KSM
   custom-resource-state config for Trident becoming REMOVABLE
   (`kube_tridentvolume_info` / `kube_tridentbackend_info` no longer read).
4. `graph-api-gateway` version bump per D10.
5. Rollback = redeploy previous image; no stored state, no upstream schema
   changes, so rollback is total.

## Open Questions

- Exact Harvest metric names in the production deployment (see first risk) —
  resolution is a mechanical rename confined to specs' label-contract lines
  and `pkg/promql` constants; approach, task breakdown, and every other
  contract survive a rename unchanged.
- The hop-C policy label name (`name` vs `policy_group`), whether
  `volume_labels` is published by the deployment's Harvest templates, and
  ONTAP's `mbps` byte basis — all three are settled by the same first
  verification task and each resolves to a one-line change (index key, query
  constant, conversion constant respectively).
