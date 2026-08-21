# BREAKING changes — replace StorageClass nodes with NetApp nodes

This release is a declared v1 break. There is no compatibility shim.

## Removed from `/v1/graph` and `/v1/edge-types`

- Node type `storageclass` (ids `<cluster>/storageclass/<name>`).
- Edge type `pvc-to-storageclass`.
- Attributes `data.provisioner` / `data.parameters` that lived on StorageClass nodes.

The claim's StorageClass **name** survives as the PVC's own `data.storageclass`
(omitempty). Physical backend identity is the `pvc-to-netapp-aggr` edge to a
`netapp-aggr` node nested under a `netapp-node`.

## Removed configuration

`--metric-prefix` / `KSG_METRIC_PREFIX` / `kubegraph.Options.MetricPrefix` /
`build.Options.MetricPrefix` are gone. Every kube-state-metrics-shaped series
is queried at its bare name.

A deployment whose KSM series **are** published under an organisational prefix
will silently return an empty graph after upgrade. Re-publish those series at
their bare `kube_*` names (drop the prefixing relabel/fork) **before**
upgrading. Embedders (`graph-api-gateway`) must drop `Options.MetricPrefix` in
the same version bump.

## `data.metrics.rate` is schema-optional

The wire `metrics` object is now a union of the RED family (`rate`,
`error_rate`, `p90_server_ms`) and the I/O family (`read_ops`, `write_ops`,
`read_latency_us`, `write_latency_us`, `read_bytes_per_sec`,
`write_bytes_per_sec`, plus the declared QoS ceiling `max_iops` and
`max_bytes_per_sec`). At the OpenAPI schema level every field
is optional. RED behaviour is unchanged: a RED-family object always carries a
positive `rate`.

An absent ceiling field means the volume has **no declared ceiling** — it is
never emitted as `0` or an "unlimited" sentinel — and neither ceiling field can
appear without at least one measurement field.

## Removed upstream queries

`kube_storageclass_info`, `kube_tridentvolume_info`, and
`kube_tridentbackend_info` are no longer read. The KSM custom-resource-state
config that exported the Trident CRs is now removable. PVC `labels.svm` is
re-sourced from the Harvest `volume_labels` series (see
`docs/netapp-harvest-preconditions.md`).

## New upstream requirements — NetApp Harvest

The storage join reads three Harvest families, and the deployment's
`volume_name` relabel rule must now stamp **both** the volume-object and the
QoS workload series:

| Series | Hop | Effect if missing |
|---|---|---|
| `volume_labels` | A — topology | No `netapp-aggr` / `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, no PVC `svm` |
| `qos_{read,write}_{ops,latency,data}` | B — I/O | Edge is still emitted, carrying no `metrics` key |
| `qos_policy_fixed_max_throughput_{iops,mbps}` | C — ceiling | No `max_iops` / `max_bytes_per_sec` |

Required Harvest templates: volume instance labels, QoS workload, QoS fixed
policy. The QoS legs are issued at `{lun=""}` — without that matcher a LUN
workload, which carries its FlexVol's relabelled `volume_name`, would be summed
on top of the volume's own traffic.

Two aggregated coverage warnings replace the single one:
`netapp_volume_join_miss` (hop A) and `netapp_qos_join_miss` (hop B), each gated
on its own family having been read.

---

# BREAKING changes — push request filters into upstream PromQL

A second declared v1 break, shipped in the same release train. No compatibility
shim.

## Removed endpoint

`GET /v1/clusters` is gone, together with its `cluster_discovery` query and the
fixed one-hour discovery lookback. The cluster list is the `clusters` field of
any `/v1/graph` response, which now reflects the clusters present in the
PROJECTED view. A client still calling the route receives `404`.

## Withdrawn `/v1/graph` parameters

`name`, `root`, `depth`, and `direction` are withdrawn along with the bounded
BFS behind them. Like any unknown parameter they are now **ignored without
error**, so an old client receives the unanchored view instead of a 400 — check
callers that relied on anchoring rather than on the error. The `invalid_depth`
and `depth_too_large` 400 reasons no longer occur.

Replacement for the "surface one specific element" use case: `?prune=false`
(new, boolean, default `true`) turns the connectivity prune off and returns the
inventory, optionally narrowed by `cluster` / `namespace` / `az` / `env`.

## New `/v1/graph` parameters

`az` and `env` (repeatable, OR-combined) join `cluster` and `namespace` as
**selector-level** filters: they are rendered into the upstream queries as label
matchers instead of being applied over the built graph. The upstream label each
one binds to is configurable with `--az-label` / `KSG_AZ_LABEL` and
`--env-label` / `KSG_ENV_LABEL` (defaults `az` / `env`, validated as PromQL
label names, required to differ); the request parameter names never change.

**Operator precondition:** every topology family the request narrows must carry
the configured labels. A family that does not carry them vanishes under an
`az` / `env` filter, and the connectivity prune can then empty the graph. A
`selector_family_empty` Warn fires when kube-state-metrics matched but a family
a live dimension reaches returned nothing.

## Behaviour changes under a filter

- A filtered build **never synthesises a pod**. An endpoint whose pod the
  request did not load renders as `external/<label>`, so under `?cluster=` the
  cross-cluster partner is an `external` node, not a real pod — cross-cluster
  edge representation now requires BOTH clusters loaded.
- A service-graph series that touches no loaded workload contributes nothing.
- An empty filtered result is a `200` with `elements: []` and `clusters: []`,
  never `outside_retention` (which stays an unfiltered-build classification).

## In-process embedder (`pkg/`) signature changes

D32 embedders must update call sites:

| Before | After |
|---|---|
| `kubegraph.ParseValues(v) (start, end, scope, err)` | `kubegraph.ParseValues(v) (Request, error)` |
| `Engine.Build(ctx, window, end)` | `Engine.Build(ctx, window, end, promql.Selector{})` |
| `build.Builder.Build(ctx, window, end)` | `Build(ctx, window, end, promql.Selector{})` |
| `build.ReadTopology(ctx, q, window, end)` | `ReadTopology(ctx, q, window, end, promql.LabelKeys{}, promql.Selector{})` |
| `promql.Render(q, window)` | `Render(q, window, promql.LabelKeys{}, promql.Selector{})` |

`graph.Scope` is now `{Clusters, Namespaces, EdgeTypes, Inventory}`;
`graph.NewScope(clusters, namespaces, edgeTypes, inventory)` takes four
arguments. `Names`, `Root`, `Depth`, `Direction`, `MaxTraversalDepth`,
`graph.Direction` and `Scope.NameFilterActive` are removed. The zero values of
`promql.Selector` / `promql.LabelKeys` reproduce every pre-change query string
byte-for-byte, so an embedder that wants the old behaviour passes them and
changes nothing else.
