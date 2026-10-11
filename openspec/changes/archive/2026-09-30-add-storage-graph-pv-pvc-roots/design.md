# Design

## Context

See proposal.md — Why. Since trace-storage-roots-by-reference (#31) a storage
build is a seed plus a shared expansion, wired in `readTopology`
(`pkg/build/topology.go`) as one errgroup of goroutines ordered by
done-channels:

| root family | seed | claim side | candidate / owner completion |
|---|---|---|---|
| `ontap_cluster` / `aggr` / `svm` / `ontap_node` (`plan.rootedClaims()`) | `volume_labels` phase 1 → `pvCandidates` → `readHubClaimInfo` (claim-info by `volumename`, closes `pvcInfoDone`) | `readHubClaimFamilies` (bindings, annotations, kubelet by claim; closes `bindingsDone` / `pvcAnnotationsDone`) | `readVolumeLabelsTail` (phase 2 + owner completion) |
| `node` / `pod` / `application` (`tracksByReference() && !rootedClaims()`) | bindings (node / pod seed) or controller recovery | `readWorkloadClaims` (claim-info, annotations, kubelet by claim) | `readWorkloadVolumeLabels` (token read over EVERY loaded claim + owner completion) |

A claim root seeds exactly the claim-info read, so it wants the **hub's**
claim side (claim-info already loaded, then bindings by claim for mounter
completion) and the **workload roots'** candidate completion (no phase 1
exists, so every loaded claim's token must be read). Both halves already
exist as functions with the right inputs.

`pkg/build/storageflow.go` suppresses every unmounted chain
(`storageFlowEdges`), and `pkg/graph/project_storage.go`
`extractFlowUnits` skips an `svm-pvc` edge with no mounter. The projection's
conservation rule already exempts roots ("for every non-root interior node").

## Goals / Non-Goals

**Goals:**

- `pvc=` / `pv=` reuse the existing expansion unchanged; only a seed, a plan
  predicate and its wiring are new.
- Body parity: for a mounted root claim, `?pvc=` produces exactly the paths a
  `?pod=` root on its mounters produces for that claim.
- No existing request's reads, fan-out or body change.

**Non-Goals:**

- Reading `kube_persistentvolume_info` or materialising PersistentVolumes.
- Changing the projection of unmounted claims under any other root kind.
- A `pvc=` / `pv=` root on `/v1/graph` (it has no roots).

## Decisions

### D1 — Scope types: `ClaimRef`, not a reused `PodRef`

`graph.StorageRoots` gains `Claims []ClaimRef` (`{Namespace, Name}`, with
`String()`), used by `pvc`; `pv` values go in `Names` like every bare-name kind.
Parsing shares one unexported `namespacedRefs` helper with `podRefs`, so the
`<ns>/<name>` rule (exactly one `/`, two non-empty segments) and its error
message stay identical. `StorageRoots.Any()` counts `Claims`.

*Alternative rejected:* renaming `PodRef` to a generic `NamespacedRef` —
breaks the exported surface `graph-api-gateway` compiles against, for no
behavioural gain.

### D2 — The claim seed: one claim-info read, keyed

`readClaimSeed` (new `pkg/build/claimseed.go`) issues
`kube_persistentvolumeclaim_info` and closes `pvcInfoDone`:

- **`pvc`**: one query per namespace, `{namespace="<ns>",persistentvolumeclaim=~"<names>"}`
  beside the request matchers, via a generalisation of
  `promql.RenderPodsInNamespace` to a caller-named identity label
  (`RenderNamesInNamespace(q, …, label, names)`, with `QPVCInfo` admitted to its
  allow-list and the pod callers passing `PodLabel`). Per-namespace rendering
  reads no cross pair; rows are still filtered to root refs in the reader,
  because a request `namespace=` matcher composes ahead of the scope and the
  filter is the one statement of which rows are roots.
- **`pv`**: `RenderOnLabel(QPVCInfo, …, VolumeNameLabel, names)` — the same
  restriction `readHubClaimInfo` renders for candidates — rows filtered to
  `volumename ∈ roots`.

Chunking and the tally use `issueClaimKeyed`, exactly as the hub's claim-info
read does, so self-metrics, span names and `markScopeIssued` behave the same.

### D3 — Plan predicate and wiring

`topologyPlan` gains `claimRoots []graph.ClaimRef` / reuses `volumeNames` for
`pv`, and a predicate `claimSeeded()` (kind `pvc` or `pv` with a value).
`tracksByReference()` answers true for it. In `readTopology`:

```
readClaimSeed                         ──close──▶ pvcInfoDone
readHubClaimFamilies(pvcInfoDone)     ──close──▶ bindingsDone, pvcAnnotationsDone
readWorkloadVolumeLabels(pvcInfoDone) ──close──▶ volumeLabelsFinal
   └─ everything downstream (pods, nodes, controllers, reached Harvest,
      scoped QoS) is unchanged — it waits on those four channels already
```

The existing `tracksByReference() && !rootedClaims()` branch becomes
`… && !claimSeeded()`, and the beside-seed signal map drops `QPVCInfo`,
`QPVCBindings`, `QPVCAnnotations` and `QVolumeLabels` for a claim-seeded plan
(each is closed by exactly one goroutine above — closing twice panics, not
closing blocks). `readHubClaimFamilies` takes a zero `hubCoverage`; its Debug
line then reports `volumes=0 candidates=0`, which is accurate.

*Alternative rejected:* reusing `readWorkloadClaims`. It derives the claim set
from bindings, which a claim root does not have before the claim-info read;
feeding it the seed's claims would re-issue claim-info by name for claims
already loaded.

### D4 — Bounded root sets before any query

`prepareClaimSeed(window, budget, keys, sel)` mirrors `preparePodSeed`: budget
minus `RequestMatcherCost(QPVCInfo)`; for `pvc` also minus
`NamespaceEqualityCost(ns)` per namespace, summing each namespace's
`ChunkScope` count; for `pv` one `ChunkScope` over the names. More than
`maxRootedVolumeLabelChunks` chunks → `ReasonInvalidScope` /
`RootScopeCapMessage`, before a querier is bound. Called from `buildStorage`
beside the other `prepare*` steps.

### D5 — Sink claims: emitted by the build, admitted by the projection

- **Build.** `assembleStorageFlow` gains a `sinkUnmounted bool`, true iff the
  plan is claim-seeded. Under a claim-seeded plan EVERY tracked claim is a root
  claim (mounter completion loads pods, never further claims), so the builder
  needs no root matching: it emits `node-aggr`, `aggr-svm` and `svm-pvc` for an
  unmounted chain instead of `continue`. Every other plan passes false, so
  their built graphs — and `/v1/graph`, which never calls this — are
  unchanged.
- **Projection.** `extractFlowUnits` turns an `svm-pvc` edge with no mounter
  into a **sink unit** (`podID=""`, `n=1`, `io=claim.IO`, edges = the claim
  hop plus its upstream hops). `ProjectStorage` retains a sink unit ONLY when
  its claim is in the resolved claim-root set — never through a storage,
  workload or application root it happens to intersect — so a hand-built
  graph carrying unmounted chains still projects exactly as today under every
  other kind. `weightRetained` needs no change: `scaleFlow(io, 1)` is the
  whole measurement, and `sumUnitShares` already puts latency and ceiling on
  the claim tier.
- **Root resolution.** `resolveStorageRoots` adds `pvc` (PVC node with
  `(labels.namespace, Name())` in the refs) and `pv` (PVC node with
  `labels.volumename` in the names), both into `workload` so `admitRoot`
  materialises them under the cluster / namespace filters, and into a new
  `claimRoots` set the sink rule reads.

*Alternative rejected:* emitting unmounted chains for every plan and letting
the projection filter. It changes the built storage graph (and any embedder
calling `Builder.BuildStorage` directly) for every root kind, to serve one.

### D7 — An unmounted root claim needs a PVC node, so the parse builds one

Found while implementing D5: a PVC node is materialised only from a
`pod-mounts-pvc` binding (`parseTopology` builds nodes from `v.PVC`;
`kube_persistentvolumeclaim_info` "never materialises a PVC on its own"), and the
Harvest join — hence `SVMByPVC` and the `pvc-to-netapp-aggr` edge — runs over
those nodes. A claim no pod mounts therefore had no node and no storage chain to
hang a sink on.

`topologyVectors` gains `MaterialiseUnboundClaims`, set by `readTopology` iff the
plan is claim-seeded. Under it the parse ALSO builds a node for every claim the
claim-info read returned that no binding minted (sorted, so the node order never
depends on vector order), through the same constructor bindings use, and those
rows join the cluster-identity first pass. Every claim a claim-seeded build
tracks is a root claim, so this needs no root matching; every other plan leaves
it false, and `/v1/graph` never sets it, so no other body changes.

*Alternative rejected:* deriving the node in the projection. The join, the
`svm` / `aggr` labels, `usage` and `application` are all resolved at parse time
over PVC nodes; a projection-side node would have none of them.

### D6 — Parameter order and messages

`storageRootParams` becomes `ontap_cluster, ontap_node, aggr, svm, node, pod,
pvc, pv, application` (the mixed-kind message names parameters in this
order), and both new names join the validation list in `ParseStorageValues`.

## Risks / Trade-offs

- [A `pv=` value is matched verbatim against `volumename`; a PV named with
  characters the rewrite rules do not map still loads its claim but joins no
  FlexVol] → that is the forward join's answer, identical to `/v1/graph`; the
  claim is materialised alone (spec "Non-NetApp claim root still shows").
- [A sink claim's measurement lands on shared upstream hops, so a mixed
  `pvc=` request's `node-aggr` weight exceeds the sum of its `pvc-pod` weights]
  → intended and pinned by the "Unmounted root claim is a sink" scenario;
  conservation holds at every non-root interior node.
- [Wiring a fourth seed into `readTopology` risks a double-closed or
  never-closed channel] → each channel has exactly one closer per plan kind;
  the fan-out pin tests (`TestBuildStorage_FanOutLegCount*`) gain `pvc` / `pv`
  rows and run under `-race`, and a failing-seed test asserts the build returns
  instead of blocking.
- [`RenderPodsInNamespace` generalisation touches the pod wave's renderer] →
  the pod callers pass `PodLabel`; `pkg/promql/testdata/render-baseline.txt`
  and the existing pod-scope tests pin that their output is byte-identical.

## Migration Plan

Additive request surface; no config. New goldens for the two root kinds; no
existing golden changes. Rollback is a plain revert.
