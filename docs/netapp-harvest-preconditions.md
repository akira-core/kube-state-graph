# Deployment preconditions — NetApp Harvest storage graph

The PVC → ONTAP aggregate join is a single label match:

```
kube_persistentvolumeclaim_info.volumename  ==  volume_labels.volume_name   (topology)
                                            ==  qos_*.volume_name           (I/O)
```

`volumename` is the bound PersistentVolume name (stock kube-state-metrics).
`volume_name` is **not** a stock Harvest label. The deployment must stamp it
onto every Harvest volume-object series **and every QoS workload series** with a
Prometheus relabel rule that maps each FlexVol to the Kubernetes PV it backs.

The join runs in three hops that degrade independently: hop A (`volume_labels`)
decides the graph's shape, hop B (the QoS workload families) decides whether the
edge carries measurements, hop C (the QoS fixed policy) whether it also carries
a ceiling. A hop-B miss leaves a valid measurement-less edge — it never costs
the claim its storage topology.

## Relabel-rule blind spots (inherited by the graph)

1. **A FlexVol whose name does not match the rule** carries no `volume_name`.
   Its claim never joins: `volumename` is present, `svm` is absent, no
   `pvc-to-netapp-aggr` edge.
2. **Trident "economy" drivers** pack many claims into one shared FlexVol, so
   no per-claim volume series exists. Per-claim I/O figures do not exist.
3. **FlexGroup volumes** span aggregates; the matched series carries an empty
   `aggr` label. No aggregate edge can be drawn. `svm` may still resolve.
4. **Volumes with no QoS workload.** ONTAP does not collect a workload for every
   volume, so hop B can miss where hop A hit. The claim keeps its edge,
   aggregate, controller and `svm` and simply carries no `metrics` key.

Two coverage signals, each gated on its OWN family being present:

- `slog.Warn("netapp_volume_join_miss", "count", n)` — claims with no hop-A
  match, or only empty-`aggr` matches, **iff** at least one `volume_labels`
  series was read.
- `slog.Warn("netapp_qos_join_miss", "count", n)` — claims that DID draw their
  edge but matched no QoS workload series, **iff** at least one QoS series was
  read.

So a deployment running the volume template without the QoS template gets its
topology graph and no spurious I/O warning, and a non-NetApp deployment (neither
family) stays silent on both. No signal is emitted for a missing ceiling — a
volume in no QoS policy group is the normal case, not a defect.

The PVC's retained `data.storageclass` is the operator's discriminator between
"this claim was never meant to have a NetApp backend" and "this claim should
have joined and did not". The builder does not interpret StorageClass names.

## Harvest series (verified against the Harvest metric catalogue)

| Series | Hop | Role |
|---|---|---|
| `volume_labels` | A | Topology: aggregate, owning controller, `svm`. Info series — value ignored, labels only |
| `qos_read_ops`, `qos_write_ops`, `qos_read_latency`, `qos_write_latency`, `qos_read_data`, `qos_write_data` | B | I/O (verbatim; no `rate()`; data families are already bytes/s) + the volume's `policy_group` |
| `qos_policy_fixed_max_throughput_iops`, `qos_policy_fixed_max_throughput_mbps` | C | Declared ceiling `max_iops` / `max_bytes_per_sec` |
| `aggr_new_status`, `aggr_space_used`, `aggr_space_total` | — | Aggregate health / usage |
| `node_new_status` | — | Controller health |

Required Harvest templates: volume instance labels (for `volume_labels`), QoS
workload, and QoS fixed policy.

**Volume granularity is a query-layer contract.** The six QoS legs are issued as
`last_over_time(qos_<family>{lun=""}[<window>])`. ONTAP collects a workload per
LUN as well as per volume, and a LUN workload carries the `volume_name` of its
containing FlexVol once the relabel rule has run — without the matcher, LUN
traffic would be summed on top of volume traffic for the same claim. A PromQL
empty-string matcher also matches series carrying no `lun` label, so the
contract holds against a template that omits it.

**Unit conversion.** `qos_policy_fixed_max_throughput_mbps` is the ONE value not
read verbatim: `max_bytes_per_sec = mbps × 1048576`, so the ceiling shares the
unit of the measured `read_bytes_per_sec` / `write_bytes_per_sec`. The policy's
identity label is read as `name` with a `policy_group` fallback — Harvest names
it differently across templates.

All fifteen Harvest/kubelet legs are OPTIONAL: a query error logs and continues
with an empty vector and never fails the build.

## Trident custom-resource-state config is removable

`kube_tridentvolume_info` / `kube_tridentbackend_info` are no longer queried.
The KSM CRS config over the `tridentvolumes` / `tridentbackends` CRDs can be
removed from the kube-state-metrics deployment after this upgrade.
