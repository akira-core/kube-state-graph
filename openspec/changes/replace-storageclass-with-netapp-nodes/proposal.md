## Why

The graph's storage view stops at the Kubernetes boundary. A PVC resolves to a
**StorageClass** — a provisioning *policy*, not a physical thing — and the NetApp
backend behind it surfaces only as two flat labels (`volumename`, `svm`) recovered
through a two-hop Trident custom-resource chain. None of it answers the questions
operators actually ask about storage: *which filer serves this claim, is it
healthy, how much of it are we using, and how fast is it?*

The centralised VictoriaMetrics already carries NetApp Harvest series that answer
all four, and the deployment already relabels each ONTAP volume with the
Kubernetes **PV name** it backs (`volume_name`). That one label collapses the
whole PVC → PV → FlexVol → aggregate → NetApp-node chain into a **single join**,
and the very same series carries the serving NetApp node — so topology and I/O
figures arrive together at no extra query cost. Replacing the StorageClass entity
with the physical node it stands in for turns the storage half of the graph from
a restatement of Kubernetes config into an actual view of the infrastructure.

## What Changes

### Added

- **Two NetApp graph node types** — `type="netapp-aggr"` (one per ONTAP
  aggregate, id `netapp/<ontap-cluster>/aggr/<aggr>`; aggregate names are
  cluster-wide unique in ONTAP, and the id deliberately excludes the owning
  node so an HA takeover moves ownership without changing identity) and
  `type="netapp-node"` (one per physical ONTAP controller, id
  `netapp/<ontap-cluster>/<node>`, materialised only when referenced by an
  emitted aggregate). Both belong to **no Kubernetes cluster**: the aggregate's
  `labels` carry `ontap_cluster` + `node` (the current owner, driving the
  compound nesting below) and the node's carry `ontap_cluster` — deliberately
  **not** `cluster` — so both stay out of the `clusters[]` response field and
  out of `?cluster=`'s domain while a shared filer remains **one** node set
  with edges from every cluster that uses it.
- **`pvc-to-netapp-aggr` edge type** — PVC to the ONTAP aggregate holding its
  FlexVol. Derived by joining `kube_persistentvolumeclaim_info`'s `volumename`
  to the Harvest volume series' `volume_name`; the aggregate comes from the
  `aggr` label and the owning controller from the `node` label on that **same
  series**, so **no separate topology query is needed**.
- **Six I/O measurements on that edge** — `read_ops`, `write_ops`,
  `read_latency_us`, `write_latency_us`, `read_bytes_per_sec`,
  `write_bytes_per_sec`, from `volume_read_ops` / `volume_write_ops` /
  `volume_read_latency` / `volume_write_latency` / `volume_read_data` /
  `volume_write_data`.
  Harvest already resolves ONTAP's base counters, so these are read **verbatim**
  (ops are already per-second, latency is already an average in microseconds, and
  the data families are already bytes per second) and
  are **never** wrapped in `rate()` — the opposite of the service-graph RED
  counters, and a distinction the specs must state explicitly.
- **Per-aggregate health and usage** — `data.health` from `aggr_new_status`
  (`1` = the aggregate is online, `0` = any other state; a 1:1 per-aggregate
  read, no cross-aggregate derivation) and `data.usage`
  `{used_bytes, capacity_bytes}` from `aggr_space_used` / `aggr_space_total` —
  the same usage shape the PVC gains from kubelet. Absence of a metric stays
  distinct from a reported unhealthy state.
- **NetApp node health** — `data.health` on the `netapp-node` from
  `node_new_status` (`1` = the controller is healthy, `0` = any other state).
  Same absence-is-not-unhealthy rule.
- **PVC `data.usage`** — `{used_bytes, capacity_bytes}` from
  `kubelet_volume_stats_used_bytes` / `kubelet_volume_stats_capacity_bytes`.
  This introduces **kubelet** as a fourth upstream metric family alongside
  kube-state-metrics, the KSM custom-resource-state config, and Harvest.
- **`type="storage-cluster"` compound group and the storage nesting chain** —
  a presentation-only Cytoscape group per ONTAP cluster, with the hierarchy
  `storage-cluster > netapp-node > netapp-aggr`: the synthesised group parents
  the **real** `netapp-node`, which in turn is the compound parent of its
  `netapp-aggr` nodes (via the aggregate's `labels.node`). This is the first
  place a **real node acts as a compound parent** — a deliberate break from the
  "relationships are edges, groups are synthesised" rule that the specs must
  state explicitly. An HA takeover moves an aggregate's parent (its
  `labels.node` follows the current owner) while its id stays put.
- **Join-coverage observability** — the PVC-to-volume join is only as complete as
  the deployment's relabel rule. Claims that should have matched but did not —
  including claims whose matched series carries an **empty `aggr` label** (the
  FlexGroup shape, where a volume spans aggregates and no single aggregate edge
  can be drawn) — must be counted and surfaced, following the
  `failed_total_label_set_mismatch` precedent, so an incomplete graph is never
  silently indistinguishable from a complete one.

### Removed — **BREAKING**

- **The `storageclass` node type and the `pvc-to-storageclass` edge type**, with
  the `kube_storageclass_info` query and the `data.provisioner` /
  `data.parameters` attributes they fed.
- **The Trident custom-resource chain** — `kube_tridentvolume_info` and
  `kube_tridentbackend_info`. The PVC's `svm` label survives unchanged in shape;
  it is now read straight off the Harvest volume series, which carries `svm`
  natively. Two queries and a two-hop resolver disappear for identical output.
- **`KSG_METRIC_PREFIX`** and the `Renderer.Prefix` plumbing behind it, including
  the public `kubegraph.Options.MetricPrefix` field. All kube-state-metrics-shaped
  series are queried at their bare names. A deployment whose KSM series *are*
  prefixed will silently return an empty graph after this change, so the removal
  needs a release note, not just a changelog line.

### Retained

- **`PVCNode.StorageClass()`** stays, as the PVC's own `data.storageclass` value.
  It costs nothing — the label rides on `kube_persistentvolumeclaim_info`, which
  the join key `volumename` already forces us to read — and it is the only thing
  that distinguishes "this claim was never meant to have a NetApp backend" from
  "this claim should have joined and did not", which the coverage signal above
  depends on.

### Changed

- **`data.metrics` becomes a union at the wire, split by family in Go.** The
  `graph` layer keeps two typed values — the existing RED `EdgeMetrics` (whose
  "rate is present and positive" invariant is preserved intact) and a new I/O
  value — and they are merged into one `metrics` object only at the serialisation
  boundary, where this codebase already concentrates rounding policy. Consumers
  keep one place to look; cross-family mis-attachment stays a compile error. The
  OpenAPI schema's `rate` field moves from required to optional as a consequence.

## Capabilities

### New Capabilities

- `netapp-storage-graph`: the NetApp aggregate and node entities, their
  identity and cluster scoping, the PVC-to-aggregate edge and its join
  contract, the six I/O measurements, per-aggregate health and usage,
  controller health, and the join-coverage signal.

### Modified Capabilities

- `cluster-topology-source`: removes the StorageClass entity and the Trident
  custom-resource SVM chain; re-sources the PVC `svm` label; adds PVC usage from
  kubelet; drops the configurable metric-name prefix from every KSM-shaped query.
- `graph-api`: node- and edge-type registry changes, the `data.metrics` union and
  the new `data.usage` attribute, the `storage-cluster` compound group, and the
  rule keeping ONTAP cluster names out of `clusters[]` and `?cluster=`.

## Impact

**Code.** `pkg/promql` (ten queries added — six volume I/O, `aggr_new_status`,
`aggr_space_used`, `aggr_space_total`, `node_new_status` — three removed,
`Renderer` reduced to a pure function); `pkg/build` (new NetApp reader and join,
PVC usage resolver, storage-class and Trident resolvers deleted); `pkg/graph`
(two new node types, new edge type, new I/O value, registry entries,
`ClusterNames()` exclusion); `pkg/cytoscape` (merged metrics DTO, `usage`
attribute, `storage-cluster` group, and the first real-node compound parent —
`netapp-aggr` nests under the real `netapp-node`).

**API.** `/v1/edge-types` loses one entry and gains one. `/v1/graph` loses the
`storageclass` node type and gains `netapp-aggr` + `netapp-node`; `data.metrics`
widens; `data.usage` is new (on PVCs from kubelet and on aggregates from
Harvest); `data.health` is new. `docs/swagger.*` and every affected golden file
need regenerating.

**Configuration.** The metric-prefix flag and its environment variable are
removed. No new configuration is introduced — the join, the units, and the health
mapping are all hardcoded contracts, consistent with how this service already
treats upstream label names.

**Consumers.** `graph-api-gateway` embeds the engine through
`pkg/kubegraph` and sets `Options.MetricPrefix`; that field disappears, so the
two repositories must land in a coordinated order. The Grafana graph panel
already renders the I/O family in its edge tooltip; the two throughput fields
are a purely **additive** wire extension (an older panel ignores unknown keys),
so surfacing them there is a separate change in that repository, not a
coordination constraint on this one.

**Upstream dependencies.** Adds NetApp Harvest (volume, aggregate, and node
objects) and kubelet volume stats. The `volume_name` label is **not** stock
Harvest — it is produced by the deployment's own Prometheus relabel rule, and
the specs must record it as a deployment precondition together with its known
blind spots: volumes whose names do not match the rule, the "economy" Trident
drivers, where many claims share one FlexVol and per-claim I/O figures do not
exist at all, and FlexGroup volumes, which span aggregates so their matched
series carries no single usable `aggr` label (no aggregate edge can be drawn).
