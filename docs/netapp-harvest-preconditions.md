# Deployment preconditions — NetApp Harvest storage graph

This document is the install-side companion to the Harvest + kubelet table in
`README.md` and the complete catalog in
[`upstream-metrics.md`](upstream-metrics.md) — all 40 graph-input series plus
the `up` diagnostic probe.

## The join is derive-then-match, and needs no relabel rule

ONTAP volume names admit only letters, digits and `_`. A Kubernetes
PersistentVolume is named `pvc-<uuid>`. The two can therefore never be equal,
and the join is not a label equality:

```
kube_persistentvolumeclaim_info.volumename        (the bound PV name)
        │
        ▼  rewrite rules            (default: replace `-` with `_`)
   match token
        │
        ▼  match mode               (default: suffix)
volume_labels.volume  ==  qos_*.volume            (the ONTAP FlexVol name)
```

Both `volume_labels.volume` and `qos_*.volume` are **stock Harvest labels**. No
Prometheus relabel rule is required, and none is read: a rule that stamps a
PV-name label onto Harvest series is simply ignored.

The defaults resolve a stock Trident estate with no configuration at all.
Trident derives a FlexVol's name by prefixing the transformed PV name with its
backend's `storagePrefix`:

```
PV        pvc-8f0c1e2a-1234-5678-9abc-def012345678
FlexVol   trident_pvc_8f0c1e2a_1234_5678_9abc_def012345678
          └────────┘ the configured storagePrefix
```

The default **suffix** match is what makes the prefix irrelevant — the backend
never needs to be told what `storagePrefix` is set to, and an estate running
several backends with different prefixes still resolves.

### Configuration

| Setting | Default | Meaning |
|---|---|---|
| `--netapp-volume-key-rewrite` / `KSG_NETAPP_VOLUME_KEY_REWRITE` | `-=_` | Ordered `<regex>=<replacement>` rules producing the match token from the PV name. Repeat the flag for several rules; the env form is semicolon-separated. Each entry splits on its FIRST `=`; a pattern needing a literal `=` writes `\x3d`. The first flag occurrence REPLACES the default list rather than appending to it |
| `--netapp-volume-match-mode` / `KSG_NETAPP_VOLUME_MATCH_MODE` | `suffix` | `exact`, `suffix`, `contains`, or `regex` (the token is compiled as a regular expression) |
| `--netapp-qos-scope-batch-bytes` / `KSG_NETAPP_QOS_SCOPE_BATCH_BYTES` | `8192` | Byte budget for one scoped QoS query's `volume` alternation. A larger matched set is split across several queries |

An uncompilable pattern or an unknown match mode is a **startup failure**, never
a silent fallback to the defaults: a typo would otherwise resolve a different
estate than the operator declared.

Pick the mode by what the estate's FlexVol names look like:

- **`suffix`** — the FlexVol name ENDS with the transformed PV name. Every
  Trident ONTAP driver. Resolves without knowing `storagePrefix`, and rejects a
  clone or snapshot whose name extends past the PV name
  (`trident_pvc_x_clone`).
- **`exact`** — the FlexVol name IS the transformed PV name (a backend with an
  empty `storagePrefix`).
- **`contains`** — the transformed PV name appears anywhere in the FlexVol
  name. Accepts the suffixed-clone shape above, so a claim can pick up a
  derived volume's aggregate. Scans every claim per series.
- **`regex`** — the token is a regular expression. For naming schemes the other
  three cannot express; write the rewrite rules to produce the pattern. Also a
  scan.

`exact` and `suffix` resolve through a hash index and cost one lookup per
Harvest series. `contains` and `regex` cost claims × series.

### Tuning loop

1. Deploy with the defaults. Nothing to configure for stock Trident + stock
   Harvest.
2. Read `netapp_volume_join_miss` from the build logs. Zero means the
   derivation covers the estate.
3. If non-zero, look at what the filer actually calls its volumes:
   `count by (volume) (volume_labels)`, and compare with a claim's
   `volumename`. Adjust the rewrite rules or the match mode.
4. Once step 2 reports zero, any legacy `volume_name` relabel rule may be
   deleted from the Prometheus scrape config. Leaving it installed is harmless.

## Hops and how they degrade

The join runs in three hops that degrade independently: hop A (`volume_labels`)
decides the graph's shape, hop B (the QoS workload families) decides whether the
edge carries measurements, hop C (the QoS fixed policy) whether it also carries
a ceiling. A hop-B miss leaves a valid measurement-less edge — it never costs
the claim its storage topology.

Hop A and hop B are not independently *reachable*, though: hop B is issued only
for the FlexVol names hop A matched (see "The QoS read is scoped" below), so a
build where hop A matched nothing issues no hop-B query at all.

## Derivation blind spots (inherited by the graph)

1. **A FlexVol whose name embeds no PV name** under the configured derivation
   never joins: its claim keeps `volumename`, has no `svm`, and draws no
   `pvc-to-netapp-aggr` edge. This is what `netapp_volume_join_miss` counts.
2. **Trident "economy" drivers** pack many claims into one shared FlexVol (the
   PV maps to a qtree or LUN, not a volume), so no per-claim volume series
   exists. Per-claim I/O figures do not exist.
3. **FlexGroup volumes** span aggregates; the matched series carries an empty
   `aggr` label. No aggregate edge can be drawn. `svm` may still resolve.
4. **Volumes with no QoS workload.** ONTAP does not collect a workload for every
   volume, so hop B can miss where hop A hit. The claim keeps its edge,
   aggregate, controller and `svm` and simply carries no `metrics` key.
5. **A FlexVol name matched from two zones or environments.** The Harvest legs
   carry no `az` / `env` matcher (see below), so whenever one build reads more
   than one zone's Harvest series — an unfiltered request, a catch-all
   `harvest` backend, or any `?env=` request — two filers whose volume names
   both match one claim's token are both candidates, and the claim joins the
   lexically-smallest `(ontap_cluster, aggr)` with no warning. FlexVol names
   derived from Kubernetes PV names (`pvc-<uuid>`) do not collide; a
   hand-chosen naming scheme that does is the operator's risk. Everything the
   claim resolves stays on the filer that pick landed on: the `svm` label, the
   QoS workloads that measure the edge, and the `(ontap_cluster, svm)` pair the
   throughput ceiling is keyed on all come from that filer's series alone.

## Zone and environment labels are NOT required on Harvest

The `az` / `env` request filters are pushed down as PromQL matchers on the
kube-state-metrics and kubelet families only. The Harvest family is **routed**
by zone instead — `?az=` selects which `harvest` backend of the routing table is
asked (see `upstream-backend-routing.md`) and the query it receives carries no
request matcher. Stamping the configured `az` / `env` labels onto Harvest series
is therefore unnecessary; a deployment that already does so keeps working
unchanged, since the labels are simply not read. `?env=` has no effect on the
Harvest legs at all.

Two coverage signals, each gated on its OWN family being present:

- `slog.Warn("netapp_volume_join_miss", "count", n)` — claims with no hop-A
  match, or only empty-`aggr` matches, **iff** at least one `volume_labels`
  series was read.
- `slog.Warn("netapp_qos_join_miss", "count", n)` — claims that DID draw their
  edge but matched no QoS workload series, **iff** at least one QoS series was
  read. Under the scoped read that means "at least one issued chunk of at least
  one QoS family returned series"; a build that issued no QoS query at all is
  silent.

So a deployment running the volume template without the QoS template gets its
topology graph and no spurious I/O warning, and a non-NetApp deployment (neither
family) stays silent on both. No signal is emitted for a missing ceiling — an
SVM with no fixed-policy series is the normal case, not a defect.

The PVC's retained `data.storageclass` is the operator's discriminator between
"this claim was never meant to have a NetApp backend" and "this claim should
have joined and did not". The builder does not interpret StorageClass names.

## Harvest series (verified against the Harvest metric catalogue)

| Series | Hop | Role |
|---|---|---|
| `volume_labels` | A | Topology: aggregate, owning controller, `svm`. Info series — value ignored, labels only. Read UNFILTERED |
| `qos_read_ops`, `qos_write_ops`, `qos_read_latency`, `qos_write_latency`, `qos_read_data`, `qos_write_data` | B | I/O (verbatim; no `rate()`; data families are already bytes/s). Read SCOPED |
| `qos_policy_fixed_max_throughput_iops`, `qos_policy_fixed_max_throughput_mbps` | C | Declared ceiling `max_iops` / `max_bytes_per_sec` of the volume's own policy group, keyed on `(cluster, svm, policy_group)` — cluster and svm from hop A, policy group from hop B |
| `aggr_new_status`, `aggr_space_used`, `aggr_space_total` | — | Aggregate health / usage |
| `node_new_status` | — | Controller health |

Required Harvest templates: volume instance labels (for `volume_labels`), QoS
workload, and QoS fixed policy.

## The QoS read is scoped

ONTAP collects a QoS workload for every volume on the filer, and the resolver
consults them only for claims that already matched a `volume_labels` series — so
an unfiltered read fetches series that are provably discarded before they are
read. The six QoS workload legs are therefore issued in a **second wave**,
after hop A, restricted to exactly the FlexVol names the loaded claims matched:

```
last_over_time(qos_read_ops{volume=~"trident_pvc_a|trident_pvc_b"}[5m])
```

Consequences worth knowing:

- **An empty scope issues no QoS query at all.** No claim matched, so hop A drew
  no edge for hop B to measure.
- The second wave waits on exactly two legs —
  `kube_persistentvolumeclaim_info` and `volume_labels` — not on the whole first
  wave, so it is not delayed by the slowest kube-state-metrics leg.
- A scope larger than `--netapp-qos-scope-batch-bytes` is split across several
  queries per family, since upstream installations cap query length
  (`-search.maxQueryLen`). Each chunk degrades on its own: a failed chunk costs
  I/O measurements only for the claims whose volumes it carried, and never an
  edge, aggregate, controller or `svm`.
- The `volume` restriction is derived from upstream data, not from the request.
  It is not an `az` / `env` / `cluster` / `namespace` matcher — but the claims
  that produced it were themselves loaded under the request's selectors, so this
  is the capability's "narrowed by reference" rule reaching the query layer.

**Volume granularity is a READER rule, not a matcher.** The QoS legs carry the
`volume` scope and nothing else; the reader sums only candidates whose `lun`
label is empty. ONTAP collects a workload per LUN as well as per volume and a
LUN workload carries the `volume` of its containing FlexVol, so the two must
never be summed for one claim — but the discard has to live in the reader,
because on a SAN backend that LUN row is the ONLY series naming the QoS policy.

**On `ontap-san`, the ceiling is reachable only through the LUN workload.** The
QoS policy is attached to the LUN, so the FlexVol's own workload falls into
ONTAP's built-in `User-Best_effort` class, which declares no ceiling and appears
in no `qos_policy_fixed_max_throughput_*` series. The policy pick therefore
reads both granularities and prefers a `policy_group` the fixed-policy families
actually hold — data-driven, never a hardcoded list of built-in class names.
If a SAN claim shows I/O but no `max_iops`, check that its LUN workload carries
a `policy_group` that `qos_policy_fixed_max_throughput_iops` also names:

```promql
count by (cluster, svm, policy_group) (qos_read_ops)   # both granularities
count by (cluster, svm, name) (qos_policy_fixed_max_throughput_iops)
```

A reader-side discard is also strictly stronger than the `lun=""` matcher it
replaces: an empty-string matcher admits series carrying no `lun` label at all,
so a template omitting `lun` on a LUN workload slipped straight through it.

**Unit conversion.** `qos_policy_fixed_max_throughput_mbps` is the ONE value not
read verbatim: `max_bytes_per_sec = mbps × 1048576`, so the ceiling shares the
unit of the measured `read_bytes_per_sec` / `write_bytes_per_sec`. The ceiling
is the volume's OWN policy group's figure, keyed on the
`(ontap_cluster, svm, policy_group)` triple — cluster and svm from hop A, policy
group from the workload series, and the fixed-policy series addressed by its
`name` (with a `policy_group` fallback for template variance). An incomplete or
unmatched key is ignored, never widened: a volume in no policy group carries no
ceiling rather than borrowing another group's figure from the same SVM.

**`svm` is required on the fixed-policy families.** A `qos_policy_fixed_max_*`
series carrying no `cluster` or no `svm` label is not indexed at all, so a
Harvest template that omits `svm` there loses EVERY ceiling in the estate
silently: no coverage signal fires, because hop B still matched and a missing
ceiling is a legitimate reading. The same holds for the policy's identity label
— a series carrying neither `name` nor `policy_group` cannot be keyed.

All fifteen Harvest/kubelet legs are OPTIONAL: a query error logs and continues
with an empty vector and never fails the build.

## Trident custom-resource-state config is removable

`kube_tridentvolume_info` / `kube_tridentbackend_info` are no longer queried.
The KSM CRS config over the `tridentvolumes` / `tridentbackends` CRDs can be
removed from the kube-state-metrics deployment after this upgrade.
