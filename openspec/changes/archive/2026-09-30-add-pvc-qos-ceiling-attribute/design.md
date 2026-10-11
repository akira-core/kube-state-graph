# Design

## Context

See proposal.md — Why. The relevant current shape:

- `resolveNetAppStorage` (`pkg/build/netapp.go`) runs the three-hop join once
  per build, shared by `/v1/graph` and `/v1/storage-graph`. Per claim it picks
  the aggregate (`pickAggr`), then the SVM scoped to that filer (`pickSVM`,
  which also returns the SVM's own ONTAP cluster), then — only when an
  aggregate resolved — sums hop B (`sumQoSIO`) and, only when that sum is
  non-nil, attaches the hop-C ceiling (`applyCeiling` keyed by
  `pickPolicy`). A FlexGroup claim `continue`s before hop B.
- The ceiling lives on `graph.IOMetrics` (`MaxIOPS`, `MaxBytesPerSec`), carried
  by the `pvc-to-netapp-aggr` edge. `/v1/storage-graph` reads claim I/O back
  from those edges (`storageflow.go`, `byPVC[e.Source] = … e.IO`) and
  `sumUnitShares` copies the ceiling onto the `svm-pvc` tier only when the
  claim contributed flow or latency.
- The scoped QoS read (`qosVolumeScope`) already covers every FlexVol name a
  loaded claim matched, FlexGroup included, so FlexGroup candidates are
  fetched and then discarded.
- `topology.go` stamps join results onto `PVCNode` (`svm`, `aggr` labels)
  before `graph.NewGraph` freezes the graph; that is the one place node
  attributes from the join are applied.
- `graph.GraphNode` is sealed; serialisation goes through its methods, never
  through a type switch (CLAUDE.md "Sealed graph types").

## Goals / Non-Goals

**Goals:**

- One ceiling resolution per claim, consumed by both the PVC node and the edge,
  so the two can never disagree (graph-api "Node and edge agree").
- FlexGroup claims resolve a ceiling with zero new upstream queries.
- Every existing edge body, flow weight and golden without a ceiling stays
  byte-identical.

**Non-Goals:**

- FlexGroup I/O **measurements** (no edge exists on `/v1/graph` to carry them;
  the storage-graph `svm-pvc` weight rules are untouched).
- Adaptive QoS policies, any `labels` change, any new query or self-metric.
- Changing `netapp_qos_join_miss` accounting: FlexGroup claims still never count
  toward it (there is no edge for hop B to measure).

## Decisions

### D1 — Resolve the ceiling per claim, before the aggregate gate

Split today's inline sequence into two per-claim results inside the existing
loop:

```
for each claim:
    oc, aggr   = pickAggr(cands)
    svmOC, svm = pickSVM(cands, oc)            // unchanged
    keyOC      = oc if oc != "" else svmOC     // FlexGroup: the SVM's filer
    qcands     = qosCandidatesFor(vols, qosIndex)
    ceil       = resolveCeiling(qcands, policyIndex, keyOC, svm)   // NEW, may be nil
    record ceilByPVC[claim] = ceil
    if oc == "" || aggr == "": count topo miss; continue           // unchanged gate
    io = sumQoSIO(qcands, oc, svm)
    if io != nil: copy ceil's fields onto io                       // edge rule unchanged
```

`resolveCeiling` is `pickPolicy` + the index lookup that `applyCeiling` does
today, returning `*graph.QoSCeiling` (nil on an incomplete or unmatched key —
the "ignored, never widened" rule moves with it verbatim). The edge keeps
`applyCeiling`'s semantics by copying from the resolved value, so the
measurement gate on the edge is exactly as before.

For a claim that resolved an aggregate, `svmOC == oc` by construction
(`pickSVM`'s scope), so `keyOC` is the same key as today — the only behaviour
change on those claims is that the node now gets the ceiling even when `io`
is nil.

*Alternative rejected:* derive the node value from the edge after the fact.
It cannot serve FlexGroup (no edge) or LUN-only claims (no measurement), which
are the two cases this change exists for.

### D2 — `graph.QoSCeiling` and a `QoS()` method on the sealed interface

```go
type QoSCeiling struct {
    PolicyGroup    string
    MaxIOPS        *float64
    MaxBytesPerSec *float64
}
```

`PVCNode` gains `QoSValue *QoSCeiling`; every node type implements
`QoS() *QoSCeiling` (nil for all but `PVCNode`). `netappResult` gains
`qosByPVC map[string]*graph.QoSCeiling`, stamped in `topology.go` beside the
`svm` / `aggr` labels. The floats are copied per claim, never aliased to the
policy index — one index entry serves every claim in the SVM (the same reason
`applyCeiling` copies today).

`resolveCeiling` returns nil unless at least one of the two fields resolved,
which is what makes "present iff at least one ceiling field resolved" a
structural property rather than a serialiser check.

*Alternatives rejected:* a type switch in `pkg/cytoscape` (violates the sealed
interface rule); reusing `IOMetrics` on the node (drags six measurement fields
and the edge-family semantics onto a node, and carries no policy name).

### D3 — Wire name `qos`, with `policy_group`

`data.qos` groups the ONTAP concept rather than naming one number, which leaves
room for an adaptive-policy extension without another top-level key.
`policy_group` is included because a ceiling is meaningless to an operator
without the policy it belongs to, and it is the value they would search ONTAP
for. The object is omitted when only a policy group is known (no fixed-policy
series): surfacing `User-Best_effort` or a policy with no fixed ceiling would
blur "absence means no declared ceiling".

DTO: `QoSDTO{PolicyGroup string "policy_group"; MaxIOPS *float64
"max_iops,omitempty"; MaxBytesPerSec *float64 "max_bytes_per_sec,omitempty"}`,
placed in `NodeData` directly after `storageclass`. Both numbers go through
`round6`, the edge's rounding, so node and edge serialise identical digits.
Nodes without the attribute keep byte-identical JSON (`omitempty`).

### D4 — The storage graph inherits it for free

`/v1/storage-graph` PVC nodes are the same `PVCNode` values the shared
topology built, so `data.qos` reaches them with no change to
`pkg/graph/project_storage.go` or `storageflow.go`. The `svm-pvc` tier keeps
copying the ceiling from the claim's edge I/O, which D1 fills from the same
resolved value, so storage-graph node/edge equality holds by construction.

## Risks / Trade-offs

- [A PVC now shows a ceiling while its edge shows none (LUN-only, FlexGroup,
  unmeasured storage-graph claim)] → documented in both specs as intended; the
  edge rule is unchanged, and the spec pins equality whenever both carry it.
- [Adding a method to `graph.GraphNode` touches all eight node types and any
  embedder switching on them] → the interface is sealed, so no external type
  implements it; the method is additive for callers.
- [FlexGroup key uses the SVM pick's own ONTAP cluster, which is the
  lexically-smallest cluster carrying the winning SVM name when a token
  collides across filers] → same filer the storage-flow chain already anchors
  that claim's SVM node on, so the ceiling and the drawn SVM stay consistent;
  a cross-filer collision is the documented fifth blind spot.
- [CLAUDE.md states "A ceiling can NEVER appear without a measurement —
  structurally"] → reword to scope it to the edge in the same change.

## Migration Plan

Additive wire change; no flag, no config. Goldens that carry a resolved
ceiling gain `data.qos` on the PVC node and are regenerated with `-update`
after review. Rollback is a plain revert.
