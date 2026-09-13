## Context

See proposal.md — Why. The current state that shapes the approach:

- `resolveNetAppStorage` (`pkg/build/netapp.go`) resolves every claim once. `pickAggr` chooses its aggregate — the lexically-smallest `(ontap-cluster, aggr)` pair over the matched series with a non-empty `aggr` — and the resolver emits one `pvc-to-netapp-aggr` edge per claim into `netappResult.edges`. The SVM pick is scoped to that aggregate's filer and lands in `netappResult.svmByPVC`.
- `pkg/build/topology.go` stamps the PVC `svm` label from `svmByPVC` right after that call. The PVC objects it stamps are the ones both endpoints serialise: `/v1/graph` projects them, and `assembleStorageFlow` appends `topology.PVCs` as they are.
- The storage-flow assembler builds each claim's chain from `topology.StorageEdges` (which is `netappResult.edges`) and stamps the claim's aggregate id on its `svm-pvc` edge as `claim_aggr`. `ProjectStorage` reads it in `claimAggrOf`, with a unique-incoming-`aggr-svm` fallback for hand-built graphs, and deletes it in `projectedEdge`.
- Pods already carry a label that references another node by id, beside an edge that restates it: `labels.node = K8sNodeID(cluster, node)` next to the `pod-to-node` edge.
- Three goldens carry NetApp-backed claims: `with-netapp-storage-cytoscape.json`, `storage-graph-aggr-root-cytoscape.json` and `storage-graph-pod-root-cytoscape.json`. The storage-graph e2e fixture has no SVM spanning two aggregates: `svm_shop` sits on `aggr1` and `svm_other` on `aggr2`.

## Goals / Non-Goals

**Goals:**

- One source for a claim's aggregate on the wire: the label is derived from the edge the builder already emits, never from a second reading of `volume_labels`.
- No per-endpoint code path: stamp once, in the shared topology.
- Tests that fail if the label and the edge ever drift apart.

**Non-Goals:**

- Changing `ProjectStorage`, `claimAggrOf` or the `claim_aggr` key.
- Attributing a FlexGroup claim to aggregates; it keeps no `aggr`.
- Any new query, edge type, tier, or label on an edge.
- Client behaviour; that is the frontend change `sankey-svm-grouping`.

## Decisions

### D1. Stamp from the emitted edge, next to `svm`

In `topology.go`, after `resolveNetAppStorage` returns, index `netapp.edges` of type `pvc-to-netapp-aggr` by source (the PVC id) to target (the aggregate node id), and set `labels["aggr"]` for every PVC that has an entry — in the same loop that stamps `svm`.

This makes the label equal to the edge target by construction, and every absence case in the specs falls out of it with no condition of its own: a FlexGroup claim, a join miss, a claim with no `volumename` and a window without Harvest all have no edge, and a joined series with an empty `svm` has an edge but no SVM.

_Alternative — record the pick in a new `aggrByPVC` map on `netappResult`._ It holds the same value but is a second representation of one pick that has to be kept in step with the edge list. _Alternative — re-derive the aggregate from `volume_labels` at the stamp._ Rejected: a second pick can disagree with the first, which the `netapp-storage-graph` delta forbids.

The pick yields one aggregate per claim, so a PVC has at most one such edge. A unit test asserts that, rather than the stamp silently choosing between two.

### D2. The value is the edge's target id, copied rather than formatted

The stamp copies `edge.Target` as it is and never composes `netapp/<oc>/aggr/<aggr>` itself. Any later change to how aggregate ids are composed then moves the label, the edge and the node together. Clients treat the value as an opaque node id, matched against `data.id` and never parsed; `CLAUDE.md`'s PVC-label rule says so when it gains `aggr`.

### D3. Both endpoints carry it, because the PVC is one object

Stamping in the topology gives `/v1/graph` the label too, where it restates the `pvc-to-netapp-aggr` edge.

_Alternative — stamp only in `assembleStorageFlow`._ Rejected: the same PVC would carry different labels per endpoint, against the `storage-graph-api` rule that retained nodes carry what they carry in `/v1/graph`, and the reusable engine would need an endpoint-specific copy of each PVC. A label restating an edge already has precedent: pod `labels.node` beside `pod-to-node`.

### D4. Keep `claim_aggr` and `claimAggrOf` unchanged

The projection keeps reading the edge-stamped key. It and the new label come from `netappResult.edges` in the same build, so they cannot disagree.

_Alternative — have `ProjectStorage` read the PVC label and drop `claim_aggr`._ It would remove an internal key, but it changes the projection's input contract and its fallback for hand-built graphs — embedders of the reusable engine can build a graph whose PVCs carry no labels at all — for no change on the wire. The comment on `ClaimAggrLabel` gains a pointer to the public PVC label; its "never appears on the wire" still holds for the edge key.

### D5. Tests pin the contract where it can drift

- **Unit** (`pkg/build`): a resolved claim's label equals its edge target; with conflicting matched series it equals the picked target; a FlexGroup claim has `svm`, no `aggr` and no edge; a join miss, a claim with no `volumename` and a window without Harvest have no `aggr`; an empty `svm` leaves `aggr` set with no `svm`; no PVC has more than one `pvc-to-netapp-aggr` edge.
- **Golden**: refresh with `go test ./internal/api/ -update -run Golden`. The bar for review is a diff that only adds `"aggr"` to PVC nodes.
- **Integration**: extend the storage-graph e2e fixture so `svm_shop` holds a second mounted claim on `aggr2` (its `volume_labels` series, a PVC and a pod mounting it). Then assert that `?svm=svm_shop` returns two PVCs each naming its own aggregate, with both aggregates in the body; that `?aggr=aggr1` returns only PVCs naming `aggr1`; and that no `storage-flow` edge carries a label beyond `tier` and `attribution`.

## Risks / Trade-offs

- [Every NetApp-backed PVC gains a key, so a consumer that snapshots label maps sees a diff] → Additive under the v1 contract, where only non-additive schema changes are v2; the golden refresh is reviewed; no `docs/BREAKING.md` entry.
- [A later change moves the label or the edge but not both] → The label is stamped from the edge list itself, and the unit test asserts equality for every PVC.
- [A client parses the id to pull out names] → The value is documented as an opaque node id; the frontend matches it against node ids.
- [The new e2e claim disturbs existing storage-graph assertions, such as node or edge counts and weights] → Add it so existing scopes are unaffected where possible, and update any expected count deliberately in the same commit; run the full integration suite.
- [`/v1/graph` states a claim's aggregate twice, as a label and as an edge] → Accepted, with pod `labels.node` as the precedent.

## Migration Plan

- Additive: no configuration, no data migration. Ship before the frontend's `sankey-svm-grouping`, which degrades on bodies without the label.
- Rollback is a revert: clients lose the label and fall back as the frontend change specifies. Nothing is persisted.
