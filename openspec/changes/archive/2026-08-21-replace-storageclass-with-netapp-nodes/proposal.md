## Why

The graph's storage view stops at the Kubernetes boundary. A PVC resolves to a
**StorageClass** — a provisioning *policy*, not a physical thing — and the NetApp
backend behind it surfaces only as two flat labels (`volumename`, `svm`) recovered
through a two-hop Trident custom-resource chain. None of it answers the questions
operators actually ask about storage: *which filer serves this claim, is it
healthy, how much of it are we using, and how fast is it?*

The centralised VictoriaMetrics already carries NetApp Harvest series that answer
all four, and the deployment already relabels every ONTAP **volume-object and QoS
workload** series with the Kubernetes **PV name** it backs (`volume_name`). That
one label collapses the whole PVC → PV → FlexVol → aggregate → NetApp-node chain
into a **single join key**, resolved off that key in three hops: the volume label
series carries the containing aggregate, its owning controller, and the serving
SVM; the QoS workload series carries the live I/O figures and the volume's QoS
policy group; and that policy group names the fixed throughput ceiling the volume
is capped at. Replacing the StorageClass entity with the physical node it stands
in for turns the storage half of the graph from a restatement of Kubernetes
config into an actual view of the infrastructure — one that shows not just how
fast a claim is going, but how fast it is *allowed* to go.

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
  to the `volume_name` label of the Harvest **`volume_labels`** series; the
  aggregate comes from that series' `aggr` label, the owning controller from its
  `node` label, and the serving SVM from its `svm` label — **one series carries
  the entire storage topology**, so no separate topology query is needed. The
  series' own value is never read; it is consumed purely for its labels.
- **Six I/O measurements on that edge** — `read_ops`, `write_ops`,
  `read_latency_us`, `write_latency_us`, `read_bytes_per_sec`,
  `write_bytes_per_sec`, from the Harvest **QoS workload** families
  `qos_read_ops` / `qos_write_ops` / `qos_read_latency` / `qos_write_latency` /
  `qos_read_data` / `qos_write_data`, read at **volume granularity only**
  (`{lun=""}`): an ONTAP LUN workload carries the same relabelled `volume_name`
  as the FlexVol containing it, so an unfiltered read would double-count the
  claim's I/O. Harvest already resolves ONTAP's base counters, so these are read
  **verbatim** (ops are already per-second, latency is already an average in
  microseconds, and the data families are already bytes per second) and are
  **never** wrapped in `rate()` — the opposite of the service-graph RED counters,
  and a distinction the specs must state explicitly.
- **The volume's throughput ceiling on that same edge** — `max_iops` and
  `max_bytes_per_sec`, recovered in a second hop off the I/O series: the QoS
  workload carries the volume's `policy_group`, and
  `qos_policy_fixed_max_throughput_iops` / `qos_policy_fixed_max_throughput_mbps`
  carry that policy's fixed limits. The ceiling is what makes the measurement
  actionable — `1200` read ops only answers an operator's question next to the
  `5000` its policy caps it at. The `mbps` value is the **one** figure converted
  rather than read verbatim (× 1048576 → bytes per second), so the ceiling and
  `read_bytes_per_sec` / `write_bytes_per_sec` share one unit and compare without
  client-side arithmetic; the specs must state that exception explicitly. A
  volume in no QoS policy group carries neither field — absence means *no
  declared ceiling*, and is never rendered as a number.
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
- **Join-coverage observability, in two independent dimensions** — the join is
  only as complete as the deployment's relabel rule, and it now has two halves
  that fail separately. (1) *Topology*: claims that should have matched a
  `volume_labels` series but did not — including claims whose matched series
  carries an **empty `aggr` label** (the FlexGroup shape, where a volume spans
  aggregates and no single aggregate edge can be drawn) — draw no edge at all.
  (2) *I/O*: claims that DID draw an aggregate edge but matched no QoS workload
  series — a volume ONTAP collects no workload for — leave that edge with no
  measurements. Each is counted and surfaced on its own, following the
  `failed_total_label_set_mismatch` precedent, so neither an edgeless claim nor a
  measurement-less edge is ever silently indistinguishable from a complete one.

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

**Code.** `pkg/promql` (thirteen queries added — `volume_labels`, the six
`qos_*` I/O families at `{lun=""}`, the two `qos_policy_fixed_max_throughput_*`
ceilings, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`,
`node_new_status` — three removed, `Renderer` reduced to a pure function); `pkg/build` (new NetApp reader and join,
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
and the two ceiling fields are purely **additive** wire extensions (an older
panel ignores unknown keys) and the I/O source swap is invisible to it (the field
names do not change), so surfacing them there is a separate change in that
repository, not a coordination constraint on this one.

**Upstream dependencies.** Adds NetApp Harvest (volume, QoS workload, QoS
fixed-policy, aggregate, and node objects) and kubelet volume stats. The
`volume_name` label is **not** stock Harvest — it is produced by the deployment's
own Prometheus relabel rule, which must now stamp **both** the volume-object and
the QoS workload series, and the specs must record it as a deployment
precondition together with its known blind spots: volumes whose names do not
match the rule; the "economy" Trident drivers, where many claims share one
FlexVol and per-claim figures do not exist at all; FlexGroup volumes, which span
aggregates so their matched series carries no single usable `aggr` label (no
aggregate edge can be drawn); and volumes for which ONTAP collects no QoS
workload, which draw their edge but carry no I/O and no ceiling.
