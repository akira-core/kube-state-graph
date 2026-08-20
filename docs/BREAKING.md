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
