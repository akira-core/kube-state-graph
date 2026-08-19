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
`write_bytes_per_sec`). At the OpenAPI schema level every field
is optional. RED behaviour is unchanged: a RED-family object always carries a
positive `rate`.

## Removed upstream queries

`kube_storageclass_info`, `kube_tridentvolume_info`, and
`kube_tridentbackend_info` are no longer read. The KSM custom-resource-state
config that exported the Trident CRs is now removable. PVC `labels.svm` is
re-sourced from the Harvest volume series (see
`docs/netapp-harvest-preconditions.md`).
