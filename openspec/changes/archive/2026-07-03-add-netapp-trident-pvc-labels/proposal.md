# Proposal: add-netapp-trident-pvc-labels

## Why

PVC nodes today expose only `{cluster, namespace}` labels plus the StorageClass; for clusters backed by NetApp Trident there is no way to see **which PersistentVolume and which SVM (NetApp Storage VM)** actually serve a claim. Operators triaging storage incidents or capacity on ONTAP must manually chase PVC → PV → TridentVolume → TridentBackend across tooling. The chain is already fully observable in the centralised VictoriaMetrics via Trident's KSM-shaped custom-resource metrics — the graph just doesn't read them.

## What Changes

- **Two new optional topology queries** in the `ReadTopology` errgroup (KSM-shaped, so the `KSG_METRIC_PREFIX` knob applies to both):
  - `kube_tridentvolume_info` — joined per cluster on its volume name label equal to the PV name; yields the `backendUUID` label.
  - `kube_tridentbackend_info` — joined per cluster on its `backendUUID` label; yields the `svm` label.
- **PV-name extraction from an existing query**: `kube_persistentvolumeclaim_info` already loaded per PVC; additionally read its `volumename` label (the bound PV name). No new query for this step.
- **PVC labels gain two additive entries** (strict `map[string]string`, no typed-attribute change):
  - `volumename` — the bound PV name (from `kube_persistentvolumeclaim_info`), present whenever the PVC reports a non-empty `volumename`, independent of Trident.
  - `svm` — the NetApp SVM serving the claim, present only when the full chain resolves (PV name → TridentVolume → TridentBackend → `svm`).
- **Graceful degradation is mandatory**: both Trident metrics are OPTIONAL (absent on non-NetApp clusters, or on clusters whose KSM lacks the Trident custom-resource-state config). Absence, partial chains (PV without a TridentVolume row, TridentVolume whose `backendUUID` matches no backend, empty labels) degrade to omitting the affected label(s) — never a build failure, never an empty-string label value.
- All joins are **per-cluster** (a PV name is only unique within a cluster); resolution is deterministic (lexically-smallest value on duplicate-series collisions, matching existing join conventions).
- No new node type, no new edge type, no PromQL filter pushdown (both new queries stay full-window, request-invariant reads like the other topology queries).

## Capabilities

### New Capabilities

_None — this extends existing topology-read and graph-serialisation behaviour._

### Modified Capabilities

- `cluster-topology-source`: two new OPTIONAL KSM-shaped queries (`kube_tridentvolume_info`, `kube_tridentbackend_info`) added to the topology fan-out and to the metric-prefix contract; new PVC→PV→TridentVolume→TridentBackend join requirement with graceful-degradation scenarios.
- `graph-api`: PVC node `data.labels` may additively carry `volumename` and `svm`; both omitted (not empty) when unresolved.

## Impact

- `pkg/promql` — two new query constants (bare metric names; prefix threaded via `Renderer` as today).
- `pkg/build/topology.go` — errgroup grows to 15 queries; new resolver(s) for the Trident join chain; PVC label assembly extended.
- `pkg/build` topology struct/tests, `pkg/graph` tests — PVC `LabelsValue` fixtures gain the new keys where relevant (no `PVCNode` type change: labels map is already generic).
- `internal/api` golden tests — PVC label additions are additive; goldens regenerate with `-update` where fixtures include Trident data.
- `internal/integration` — new fixture series for `kube_tridentvolume_info` / `kube_tridentbackend_info` plus absent-metric degradation coverage.
- Docs: `CLAUDE.md` metric-prefix list + spec deltas for the two modified capabilities.
- Wire format: **additive only** — no v2 concern; deterministic-body rule unaffected (labels are sorted-key serialised as today).
