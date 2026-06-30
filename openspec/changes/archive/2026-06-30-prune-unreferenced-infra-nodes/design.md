## Context

`pkg/graph/project.go` builds a per-request `View` over the full `*Graph`:
`traverse` → `filterNodes` → `filterEdges`. `filterNodes` admits pods/services/
PVCs/externals directly via `nodePassesFilters`, and **defers** the two
cluster-scoped infra node kinds (`K8sNode`, `StorageClassNode`) — neither carries
a `namespace` label — to `infraNodePassesFilters`. Before this change that
predicate kept every infra node **unless** a `?namespace=` filter was set, in
which case it required the node to be referenced by an in-scope element (a pod's
`labels.node` / a PVC's `StorageClass()`), recorded in the `hostNodes` /
`referencedSC` sets. Those reference sets were built **only** under a namespace
filter; with no filter the full topology listed every node and StorageClass,
including orphans (empty / control-plane nodes, unused StorageClasses).

This change generalises the referenced-only rule to every request (motivation in
`proposal.md`): the default workload graph should carry only the host nodes of
pods that are in the graph and the StorageClasses backing in-scope PVCs.

## Goals / Non-Goals

**Goals:**
- An infra node (`node` / `storageclass`) is admitted iff referenced by an
  in-scope element — on every request shape (default, `?cluster=`, `?namespace=`).
- An explicit `?name=<infra-node>` still surfaces an unreferenced infra node.
- Build, serialiser, determinism, and the "no filters pushed to PromQL" rule are
  unchanged; the full `*Graph` is still built (cache-friendly projection-only change).

**Non-Goals:**
- No build-time pruning (the node materialisation in `pkg/build` is untouched — a
  future cache must still be able to serve `?name=<empty-node>` from one built graph).
- No "unhealthy node always visible" exception: a podless `NotReady` / `Unknown`
  node is hidden by default like any other podless node (confirmed decision).
- No change to pod / service / PVC / external admission, to edge re-add, or to the
  wire shape.

## Decisions

### D1: Infra nodes are referenced-only on every request (generalises the namespace rule)

`filterNodes` builds `hostNodes` (in-scope pods' `labels.node`) and `referencedSC`
(in-scope PVCs' `StorageClassID(cluster, StorageClass())`) **unconditionally**, not
just under a namespace filter. `infraNodePassesFilters` then admits a deferred
`K8sNode` / `StorageClassNode` iff its id is in the relevant reference set.

**Why:** the workload graph's default view should not dangle orphan infra. The
reference sets are cheap to build for every request, and the rule already existed
for the namespace case — generalising it is the smallest correct change.

**Alternatives considered:**
- **Build-time pruning** (don't materialise an unreferenced node) — *rejected:* breaks
  the build-full-then-project model and makes `?name=<empty-node>` impossible to serve
  from a cached graph; the saving (only truly-empty nodes) is small because every pod
  is loaded so every host node is already referenced.
- **Drop infra nodes from the serialiser** — *rejected:* the serialiser is
  presentation-only; node admission is a projection concern (the one place
  namespace/reference-awareness lives).

### D2: Explicit `?name=` is the admission exception

When a name filter is active, `infraNodePassesFilters` admits the infra node iff its
`Name()` is in `scope.Names` — regardless of whether it is referenced. A name filter
that does not name the node drops it here; if it is the host of a *named* pod (or backs
a *named* PVC), `filterEdges` re-adds it as the `pod-to-node` / `pvc-to-storageclass`
edge partner (the existing unified re-add), so that path is unchanged.

**Why:** an operator must still be able to pull up a specific empty / broken node (its
`ready_status`, `ipaddress`) or an unused StorageClass (its `provisioner`,
`parameters`) by name. This preserves the existing "name filter matches a K8s node /
StorageClass" contract while the default view hides unreferenced infra.

### D3: No health-based exception

A podless node whose `ready_status` is `NotReady` / `Unknown` is hidden by default like
any other podless node. Node-health of *empty* nodes is out of the default workload
view; it is reachable with `?name=`.

**Why:** keeps the rule a single, predictable "referenced-or-named" test with no
attribute-dependent special case; the operator explicitly chose this over an
always-show-unhealthy exception.

### D4: Cluster filter still applies first; determinism preserved

`infraNodePassesFilters` checks the cluster filter before the name/reference test (an
infra node outside the scoped clusters is dropped regardless). The admitted node set is
still a pure function of the data; `SortNodes` / `SortEdges` are unchanged, so output
stays byte-deterministic.

## Risks / Trade-offs

- **[Default view loses orphan infra]** A consumer that relied on the default graph
  listing every node / StorageClass must switch to `?name=` for unreferenced ones. →
  Documented as a behaviour (not wire-schema) change; `?name=` covers the gap.
- **[Empty NotReady node hidden by default]** The just-shipped `ready_status` signal is
  not in the default view for *podless* nodes. → Intended (D3); host-node `ready_status`
  is unaffected, and `?name=` surfaces the empty node.
- **[Golden / test churn]** Any projection test or fixture asserting an orphan infra
  node in a non-name request changes. → One unit test generalised + two added; one
  integration test (`TestNodeReadyStatusAttribute`) switched to `?name=`. No golden
  fixture carried an orphan infra node, so golden output is unchanged.

## Migration Plan

1. `pkg/graph/project.go`: build `hostNodes` / `referencedSC` unconditionally;
   rewrite `infraNodePassesFilters` to (cluster) → (name exception) → (referenced).
2. `pkg/graph/project_test.go`: generalise the podless-node test; add the no-filter-drop
   and name-exception tests.
3. `internal/integration`: fetch the podless probe nodes in `TestNodeReadyStatusAttribute`
   via `?name=`.
4. `CLAUDE.md`: generalise the D6 infra-node retention description + consequence note.

**Rollback:** revert the `project.go` change; the rule is projection-only, no persisted
state and no edge-ID change.
