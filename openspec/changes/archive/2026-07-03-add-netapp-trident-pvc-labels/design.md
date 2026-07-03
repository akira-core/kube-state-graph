# Design: add-netapp-trident-pvc-labels

## Context

PVC nodes are materialised in `parseTopology` (`pkg/build/topology.go`) from the
pod→PVC binding metric and enriched by joins over `kube_persistentvolumeclaim_info`
(StorageClass, via `resolvePVCStorageClass`) and
`kube_persistentvolumeclaim_annotations` (ArgoCD Application). Labels today:
`{cluster, namespace}` plus an optional `volume` key — the **pod-spec volume
name** from the binding metric, *not* the PersistentVolume name.

NetApp Trident deployments expose two KSM-shaped custom-resource metrics
(via kube-state-metrics custom-resource-state config over the
`tridentvolumes` / `tridentbackends` CRDs, or a compatible exporter):

- `kube_tridentvolume_info{name, backendUUID, ...}` — one series per
  TridentVolume CR; Trident names the CR **after the PV** (`pvc-<uid>`), so
  `name` == PV name.
- `kube_tridentbackend_info{backendUUID, svm, ...}` — one series per
  TridentBackend CR; `svm` is the ONTAP Storage VM serving that backend.

`kube_persistentvolumeclaim_info` (already fetched as `QPVCInfo`) carries the
`volumename` label — the bound PV name — which the reader currently ignores.
Chaining the three yields PVC → PV name → backendUUID → SVM entirely from data
already in (or addable to) the centralised VictoriaMetrics.

The topology fan-out currently issues 16 parallel queries; all joins in the
reader are per-cluster, deterministic (lexically-smallest on collision), and
OPTIONAL-metric-tolerant. This change follows those conventions exactly.

## Goals / Non-Goals

**Goals:**

- Surface the bound PV name and the NetApp SVM on PVC nodes as two additive
  `labels` keys: `volumename`, `svm`.
- Two new OPTIONAL topology queries (`kube_tridentvolume_info`,
  `kube_tridentbackend_info`), prefix-aware like every KSM-shaped series.
- Graceful degradation at every link of the chain; never a build failure,
  never an empty-string label value.
- Determinism (D6): every new map is a pure function of its input vector;
  golden bodies stay byte-stable for unchanged fixtures.

**Non-Goals:**

- No TridentBackend / SVM / PV **nodes** and no new **edge type** — the SVM is
  metadata on the PVC, not a graph entity (a future change may promote it).
- No typed attribute (`data.netapp`-style) — see D-T1.
- No other Trident facts (pool, state, size); only `svm` + PV name.
- No PromQL-side filtering or join pushdown — both new queries stay full-window
  `last_over_time` reads, joined in memory ("no filters pushed to PromQL").
- No change to PVC materialisation: the chain **enriches** PVCs that exist via
  the binding metric; it never creates one (same rule as `resolvePVCStorageClass`).

## Decisions

### D-T1: Plain labels, not a typed attribute

`volumename` and `svm` go into the PVC's `labels` map (strict
`map[string]string` — both are plain strings, so no type violation).

- Why not typed (the `ipaddress`/`owner`/`application` precedent): a typed
  attribute widens the sealed `GraphNode` interface across all six node types,
  the Cytoscape DTO, and every serialiser test — heavy for two identity-shaped
  strings. The typed precedents earn their weight (structured owner object,
  list-valued ipaddress/containers, cross-serialiser semantics); these are flat
  strings used for display/filtering, the exact class `labels` already carries
  (`cluster`, `namespace`, `volume`).
- The existing PVC `volume` label (pod-spec volume name) proves the pattern:
  storage-identity strings on PVC labels are established practice.
- Key names: `volumename` mirrors the upstream KSM label on
  `kube_persistentvolumeclaim_info` (discoverable, zero translation); `svm`
  mirrors the Trident backend label. **`volumename` ≠ `volume`** — the latter
  stays the pod-spec volume name; both may coexist on one PVC. Docs must call
  this out.

### D-T2: Two new query constants, rendered `last_over_time`, prefix-aware

`pkg/promql/queries.go` gains:

- `QTridentVolumeInfo Query = "kube_tridentvolume_info"`
- `QTridentBackendInfo Query = "kube_tridentbackend_info"`

Both render as `last_over_time(%s<metric>[%s])` — plain info-style gauges, no
selector (no request-invariant filter needed; all label reads happen at parse
time). Both are KSM-shaped custom-resource series, so `Renderer.Prefix`
applies — they join the KSM-prefix list in CLAUDE.md and the
`cluster-topology-source` spec. Constants stay bare metric names so
`query`/`query_name` self-metric and span dimensions are stable. Adding two new
*values* to the existing `query` label is additive (no new label — no D26
contract break).

`ReadTopology` errgroup grows 16 → 18 legs; `topologyVectors` gains
`TridentVolume`, `TridentBackend model.Vector`; `RawSeriesCount` gains both
entries.

### D-T3: Label contract for the Trident metrics (D26-style, fixed)

These are NOT stock KSM defaults. The operator provides them via KSM
custom-resource-state config (or any compatible exporter). The fixed contract
any exporter MUST honour:

- `kube_tridentvolume_info`: label `name` = TridentVolume CR name **= PV name**
  (Trident's own naming invariant), label `backendUUID` = owning backend UUID.
- `kube_tridentbackend_info`: label `backendUUID` = backend UUID, label `svm` =
  ONTAP SVM name.
- Both SHOULD carry `cluster` (the multi-cluster join key); a series missing it
  falls into the per-metric missing-cluster bucket (D-T5).

Label names are case-sensitive and used verbatim (`backendUUID` is a valid
Prometheus label name). Exporters emitting different label names (e.g. a
`labelsFromPath` variant) simply fail the join → graceful degradation, never an
error.

### D-T4: Join pipeline — three resolvers, applied at PVC assembly

All per-cluster, all deterministic, all built up-front in `parseTopology`
alongside the existing resolvers:

1. **PV name per PVC** — extend the existing `QPVCInfo` pass: refactor
   `resolvePVCStorageClass` into `resolvePVCInfo` returning
   `map[pvcKey]pvcInfoAttrs{storageClass, volumeName string}` in ONE iteration.
   Per-field independent picks (a series may carry `volumename` without
   `storageclass` and vice versa): skip empty values per field,
   lexically-smallest non-empty wins per field on duplicate
   `(cluster, namespace, claim)`. Existing StorageClass behaviour is unchanged
   (same skip/tie-break semantics, now field-scoped).
2. **backendUUID per PV** — `resolveTridentVolumeBackends(v.TridentVolume, mc)
   map[[2]string]string` keyed `(cluster, name)` → `backendUUID`; skip series
   with empty `name` or empty `backendUUID`; lexically-smallest `backendUUID`
   wins on duplicates.
3. **SVM per backend** — `resolveTridentBackendSVMs(v.TridentBackend, mc)
   map[[2]string]string` keyed `(cluster, backendUUID)` → `svm`; skip empty
   `backendUUID`/`svm`; lexically-smallest `svm` wins on duplicates.

At the per-PVC assembly site (where `LabelsValue` is built):

```
attrs := pvcInfo[pvcKey{cluster, ns, claim}]
if attrs.volumeName != "" {
    labels["volumename"] = attrs.volumeName
    if uuid := backendByPV[[2]string{cluster, attrs.volumeName}]; uuid != "" {
        if svm := svmByBackend[[2]string{cluster, uuid}]; svm != "" {
            labels["svm"] = svm
        }
    }
}
```

A label key is set only when its resolved value is non-empty — absence of any
link (no `volumename`, no TridentVolume row, no matching backend, empty `svm`)
omits the downstream key(s) silently. `svm` is impossible without `volumename`
by construction (the join is rooted at the PV name).

### D-T5: Missing-cluster bucketing

Both new readers tally samples with an empty `cluster` label through
`mc.bucket(<query>, cluster)` — identical to every existing reader. The join
then proceeds inside the bucketed cluster value, matching how a cluster-less
`kube_persistentvolumeclaim_info` series already behaves. One aggregated warn
per metric per build (existing `missingClusterCounts.warn`).

### D-T6: Projection / prune untouched

Labels are baked at build time, before `graph.NewGraph` freezes nodes. The
connectivity prune, D6 infra deferred-admission, `?name=` matching, and the
Cytoscape compound hierarchy are all unaffected (no new node/edge type, no new
group tier). `?name=<pvc>` continues to match on node name only — no
requirement to match on `svm`.

### D-T7: Test surface

- **Unit (`pkg/build`)**: resolver tests — per-field independent lexical-min in
  `resolvePVCInfo` (volumename present/storageclass absent and vice versa),
  empty-value skips, duplicate-series tie-breaks in both Trident resolvers,
  missing-cluster bucketing; assembly tests — full chain lands `volumename` +
  `svm`, each partial-chain permutation omits exactly the right key(s),
  coexistence of `volume` and `volumename` on one PVC.
- **Golden (`internal/api`)**: extend one fixture set with Trident series →
  regenerate with `-update`; fixtures without Trident data MUST stay
  byte-identical (degradation proof).
- **Integration (`internal/integration`)**: one scenario ingesting
  `kube_tridentvolume_info`/`kube_tridentbackend_info` fixture series and
  asserting both labels on the emitted PVC; the existing non-Trident suites
  double as absence coverage.

### D-T8: Docs

CLAUDE.md KSM-prefix list + a load-bearing bullet for the chain;
`cluster-topology-source` spec delta (queries, contract, degradation
scenarios); `graph-api` spec delta (PVC label surface). Swagger untouched
(labels are already an open string map in the DTO).

## Risks / Trade-offs

- **[Exporter label drift]** Operator's custom-resource-state config emits
  different label names (`backend_uuid`, `svmName`…) → joins silently miss, no
  `svm` label. Mitigation: D-T3 pins the contract in the spec + CLAUDE.md;
  degradation is silent-but-safe by design (`RawSeriesCount` self-metrics still
  show the series arriving, aiding diagnosis).
- **[`volume` vs `volumename` confusion]** Two similarly-named keys with
  different meanings on one node. Mitigation: explicit call-out in CLAUDE.md,
  spec, and resolver comments; renaming the existing `volume` key is a breaking
  change and out of scope.
- **[CR-name ≠ PV-name edge cases]** Imported/legacy Trident volumes whose CR
  name diverges from the PV name fail the stage-2 join → `volumename` present,
  `svm` absent. Accepted: matches Trident's documented naming for
  dynamically-provisioned volumes (the overwhelming case).
- **[Fan-out width]** +2 upstream queries per build (18 total). Marginal: both
  vectors are small (one series per PV / per backend) and the errgroup already
  parallelises; upstream VM search limits stay the cost bound.
- **[Stale backend mapping]** `last_over_time` over the window can surface a
  backendUUID→svm pairing that changed mid-window; lexical-min (not
  latest-seen) picks on duplicates. Accepted: backend/SVM reassignment is rare
  and the tie-break exists for HA-KSM duplicate series, not temporal churn
  (same trade-off as every other info-metric join in the reader).

## Migration Plan

Additive, backwards-compatible. Deploy = ship binary; clusters without Trident
metrics see zero body change except `volumename` now appearing wherever
`kube_persistentvolumeclaim_info` carries it. Rollback = revert. Operator
enablement (optional, per cluster): add the Trident custom-resource-state
config to KSM so the two metrics exist upstream.

## Open Questions

None. Label keys (`volumename`, `svm`), join-label contract (`name`,
`backendUUID`, `svm`), and labels-over-typed representation confirmed with the
requester.
