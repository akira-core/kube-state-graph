# Proposal

## Why

`GET /v1/storage-graph` can be rooted at an ONTAP component, a Kubernetes node,
a pod or an ArgoCD Application, but not at the object an operator most often
starts from when investigating storage: a PersistentVolumeClaim or its
PersistentVolume. Today the only way to see "which filer, aggregate and SVM back
`shop/orders-data`, and who mounts it" is to root at a mounting pod and hope the
claim is mounted — an unmounted claim is unreachable, and so is a claim bound to
a statically provisioned PV from any storage-side root (its PV name yields no
`pvc_` candidate).

Since #31 every root kind is a SEED that produces a tracked claim set, which a
shared expansion walks to both ends of the chain. A claim root and a volume root
are the two cheapest seeds possible: both are a single read of
`kube_persistentvolumeclaim_info`, which the storage-side seed already issues as
its second step.

## What Changes

- Two new root kinds on `GET /v1/storage-graph`, each repeatable, validated like
  every other root value, and subject to the one-root-kind rule (`missing_root`
  / `invalid_scope` unchanged in meaning; the mixed-kind message may name them):
  - **`pvc=<namespace>/<claim>`** — a PersistentVolumeClaim. Parsed like
    `pod=`: a value without exactly one `/` separating two non-empty segments is
    400 `invalid_scope`. Matched in every Kubernetes cluster of the selected
    estate.
  - **`pv=<name>`** — a PersistentVolume (cluster-scoped, bare name), matched
    through the `volumename` label of `kube_persistentvolumeclaim_info`; the
    root resolves to the claim(s) bound to it.
- **Seeds.** `pvc` reads `kube_persistentvolumeclaim_info` restricted on
  `namespace` + `persistentvolumeclaim` (one query per namespace, the claim
  names as an alternation — the pod-root keying precedent), keeping only rows
  whose `(namespace, claim)` is a root ref. `pv` reads it restricted on
  `volumename` — the storage-side seed's second step without phase 1. The
  returned claims are the tracked set; the existing expansion does the rest.
- **Static PVs are reachable** from `pv=` / `pvc=`: the seed does not go through
  `pvCandidates`. Whether the claim then lands on a FlexVol is still decided
  solely by the forward join, exactly as on `/v1/graph`.
- **Root materialisation.** A `pvc` / `pv` root "exists" when
  `kube_persistentvolumeclaim_info` names it in the window; it is then emitted
  with its ordinary attributes and compound parents even when no complete path
  passes through it. A root naming nothing is an empty 200 (no marker, no
  error). A PV bound to no claim is not drawn — PersistentVolume is not a node
  type and `kube_persistentvolume_info` is not read.
- **Unmounted root claims keep their storage-side path.** The projection
  drops every unmounted claim today, because a path is complete only when it
  reaches a pod. For a claim that is itself a `pvc` / `pv` root, a path ending
  at the claim counts as complete, so it keeps its
  `netapp-node → aggr → svm → pvc` chain. The claim is a Sankey **sink**: its
  whole measurement is summed onto its `svm-pvc` edge and every upstream hop,
  with no `pvc-pod` edge below it. Conservation still holds at every non-root
  interior node (the root claim is the only node whose inflow has no outflow).
  An unmounted claim that is NOT a root claim stays dropped under every root
  kind, so existing bodies are unchanged. A root claim whose volume joins no
  Harvest topology is materialised alone, with no edges and no mounting pods.
- **Bounded root sets.** Both first reads are request-derived scopes; a root set
  whose first read would exceed the existing chunk cap is 400 `invalid_scope`
  before any query, as for every other root kind.
- `graph.StorageRootKind` gains `pvc` and `pv`; `graph.StorageRoots` gains a
  claim-ref slice (the `PodRef` shape, a new `ClaimRef` or a shared namespaced
  ref type — design's call).

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `storage-graph-api`: "One root kind per request" (two new kinds and their
  value forms), "Every root kind is tracked from its own tier to its claims"
  (the two seeds; static PVs reachable from them), "Roots are always
  materialised when the upstream knows them" (existence rule for `pvc` / `pv`),
  "Storage-reachability projection" (retention through a claim root, and the
  unmounted-root-claim path), "Flow weights on every tier" (the sink claim's
  weight). The seed reads are specified in "Every root kind is tracked…"; the
  general by-reference rules of "Storage build reads every family by
  reference" and the expansion of "The tracked claims expand to both ends of
  the chain" already cover them unchanged.

## Impact

- `pkg/graph/storagescope.go` — new kinds, claim refs, parsing.
- `pkg/kubegraph/parse.go` — `storageRootParams`, validation list.
- `pkg/build/topologyplan.go`, a new `claimseed.go` beside `podseed.go` /
  `nodeseed.go`, and `pkg/promql` renderers for the two restricted claim-info
  reads (with their chunk/cap checks).
- `pkg/graph/project_storage.go` — root matching against PVC nodes (`pvc` by
  `(namespace, name)`, `pv` by `labels.volumename`) and sink units for
  unmounted root claims; `pkg/build/storageflow.go` — emit the storage-side
  chain of an unmounted root claim (today every unmounted chain is suppressed).
- OpenAPI parameter docs for `/v1/storage-graph`; fan-out pins in
  `TestBuildStorage_FanOutLegCount*`; new goldens / integration coverage.
- Additive: every existing request behaves exactly as before.
