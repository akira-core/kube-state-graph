# Deployment preconditions — NetApp Harvest storage graph

The PVC → ONTAP aggregate join is a single label match:

```
kube_persistentvolumeclaim_info.volumename  ==  volume_*.volume_name
```

`volumename` is the bound PersistentVolume name (stock kube-state-metrics).
`volume_name` is **not** a stock Harvest label. The deployment must stamp it
onto every Harvest volume-object series with a Prometheus relabel rule that
maps each FlexVol to the Kubernetes PV it backs.

## Relabel-rule blind spots (inherited by the graph)

1. **A FlexVol whose name does not match the rule** carries no `volume_name`.
   Its claim never joins: `volumename` is present, `svm` is absent, no
   `pvc-to-netapp-aggr` edge.
2. **Trident "economy" drivers** pack many claims into one shared FlexVol, so
   no per-claim volume series exists. Per-claim I/O figures do not exist.
3. **FlexGroup volumes** span aggregates; the matched series carries an empty
   `aggr` label. No aggregate edge can be drawn. `svm` may still resolve.

Unjoined claims (no match, or only empty-`aggr` matches) are counted once per
build as `slog.Warn("netapp_volume_join_miss", "count", n)` **iff** at least
one Harvest volume series was read. A non-NetApp deployment (zero volume
series) stays silent.

The PVC's retained `data.storageclass` is the operator's discriminator between
"this claim was never meant to have a NetApp backend" and "this claim should
have joined and did not". The builder does not interpret StorageClass names.

## Harvest series (verified against the Harvest metric catalogue)

| Series | Role |
|---|---|
| `volume_read_ops`, `volume_write_ops`, `volume_read_latency`, `volume_write_latency` | Join + I/O (verbatim; no `rate()`) |
| `aggr_new_status`, `aggr_space_used`, `aggr_space_total` | Aggregate health / usage |
| `node_new_status` | Controller health |

All ten Harvest/kubelet legs are OPTIONAL: a query error logs and continues
with an empty vector and never fails the build.

## Trident custom-resource-state config is removable

`kube_tridentvolume_info` / `kube_tridentbackend_info` are no longer queried.
The KSM CRS config over the `tridentvolumes` / `tridentbackends` CRDs can be
removed from the kube-state-metrics deployment after this upgrade.
