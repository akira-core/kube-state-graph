## Why

A `/v1/storage-graph` request rooted at a storage-side component reads exactly as
much as a request with no root at all. `graph.StorageRoots` reaches the build
through one field — the pod / node name scopes — and `ontap_cluster=`, `aggr=`
and `svm=` reach it through none, so every such request loads the whole filer's
`volume_labels` and then discards almost all of it at projection.

Measured against a synthetic estate (8,000 pods, 1,200 mounting a claim, a filer
holding 20,000 FlexVols across 24 aggregates), a request for `?aggr=aggr00`:

| | today |
|---|---|
| series read | 43,495 |
| of which `volume_labels` | 20,000 (46%) |
| nodes built | 2,644 |
| nodes surviving projection | **128** |

95.2% of the build is discarded. `volume_labels` is the build's `largest_leg`
and the one leg no request parameter narrows — `queryDims` gives the Harvest
family `dimAZRoute`, which selects a backend and renders no matcher. Under a
catch-all `harvest` backend, every storage request reads every FlexVol on every
filer, and the Go-side cost scales with it: the same build takes 14 ms against a
5,000-FlexVol filer and 42.4 ms against an 80,000-FlexVol one.

`ontap_cluster` and `aggr` are **stock labels on that exact series** —
`volume_labels` carries `cluster` (the ONTAP cluster) and `aggr`. Restricting the
read by them is an identity mapping from the request parameter to the matcher,
not a derivation.

## What Changes

1. **A storage-side-rooted storage-graph build restricts its Harvest topology
   read to the rooted components.** When the request carries `ontap_cluster=`
   and/or `aggr=`, `volume_labels` is issued as one query carrying
   `cluster=~"…"` and/or `aggr=~"…"` — the two matchers AND-combined, exactly as
   the projection combines the two roots (an `aggr=` root names an aggregate only
   *within* the `ontap_cluster=` values) — instead of one unrestricted query. A
   request carrying neither issues the unrestricted query exactly as today.

2. **A second Harvest phase restores the candidate set of every matched claim, so
   the body stays byte-identical.** The aggregate and SVM picks are
   lexically-smallest over a claim's *whole* candidate set, so a restricted read
   could place a claim on the rooted aggregate where an unrestricted read would
   place it on a lexically-smaller one (a Trident clone, or the same FlexVol name
   on two filers). After phase 1 the build knows which claims matched; phase 2
   re-reads `volume_labels` restricted to those claims' derived tokens —
   `volume=~".*<token>"` for the default suffix mode, `volume=~"<token>"` for
   exact mode — which is the **forward** derivation the join already computes and
   needs no inversion. The two results merge before the parse.

3. **An `svm=` or `node=` root anywhere in the request disables the
   restriction.** The aggregate's owning controller is a vote over **all** of that
   aggregate's `volume_labels` series (`netapp-storage-graph`, "NetApp aggregate
   entity" — takeover inside the window resolves to the lexically-smallest
   non-empty `node`). Restricting by `aggr` keeps every series of the rooted
   aggregate and leaves the vote intact; restricting by `svm` or `node` leaves the
   vote running over a subset, which can elect a different controller and move the
   `node-aggr` tier. And because the projection UNIONS `aggr=` with `svm=`, a
   request naming both retains units reached only through the SVM — a restriction
   by `aggr` alone would drop them. So a request carrying either root reads
   unrestricted, whatever else it carries.

4. **Non-default volume-match modes opt out.** Phase 2's token matcher expresses
   `exact` and `suffix` (`--netapp-volume-match-mode`, default `suffix`) exactly.
   `contains` and `regex` cannot be rendered into an anchored alternation without
   changing their semantics, so a build configured with either reads
   unrestricted, single-phase, as today.

5. **The join-coverage gate is corrected for a restricted read.**
   `netapp_volume_join_miss` counts every loaded claim that resolved no aggregate
   while `volume_labels` was non-empty. Under a restricted read every claim
   outside the rooted components is such a claim, so the signal would fire on
   almost the whole estate. It is suppressed for a restricted build, where "no
   aggregate" no longer means "coverage failure".

6. **The QoS workload wave gates on the merged result**, so its `volume` scope is
   computed from the same candidate set the parse uses. Its size falls with the
   restriction; no rule of the scoped QoS read changes.

Every storage-graph body is byte-identical to today's for an estate whose
aggregates and controllers are each named by their own Harvest gauge families
(the stock `aggr_*` and `node_*` templates); the one corner where that can differ
is spelled out in design.md Risks. `/v1/graph` is untouched
(it carries no roots and reads under `fullPlan`). `queryDims`,
`render-baseline.txt`, the request surface, the flag set and the dependency set
are unchanged. No new node type, edge type, request parameter or flag.

**Trade-off, stated up front:** a restricted build issues one more sequential
Harvest hop. In the measured estate that moves the Harvest chain's tail from
119 ms to roughly 144 ms at 25 ms RTT while cutting the read from 43,495 to about
900 series on that leg. The change is worth taking where an upstream series limit
is the binding constraint, which is the case this repo has been hardening for.

This change is sequenced AFTER `scope-controller-legs-by-reference`, whose
`storage-graph-api` delta modifies the same "Storage build reads only what it
draws" requirement and whose design D8 explicitly classifies `volume_labels` as
unrestricted. Sync or archive that change first.

**It answers D8's two stated reasons rather than overriding them.** D8 leaves the
leg unrestricted because (a) a storage root must be drawable with no claim, and
the family is the sole source of SVM names and the storage-side inventory, and
(b) the Harvest join is a derived-token suffix match the query layer cannot
express without the provisioner's prefix. On (a): the restriction is derived
**from the root itself**, so what it keeps is exactly what that root can draw —
a rootless request keeps the unrestricted read, and no root loses its inventory.
On (b): unchanged and unchallenged. Nothing here inverts a volume name back to a
PV name; the claim families stay unrestricted and the Go-side derive-then-match
runs exactly as before. Phase 2 renders the derivation in its forward direction,
from a token the build already holds.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `netapp-storage-graph`: **Harvest legs under request-scoped selectors** — the
  blanket "no request-scoped matcher of any kind" gains one scoped exception for
  `volume_labels` under a storage-side-rooted `/v1/storage-graph` build, stated as
  a root-derived scope rather than a selector-level dimension, with the `svm=` /
  `node=` exclusion and its owner-vote reason. **Harvest volume-label series as
  the storage topology source** — the "carries no restriction on `volume`" clause
  is qualified by phase 2's token restriction, and the single-query shape becomes
  one-or-two phases.
- `storage-graph-api`: **Storage build reads only what it draws** — the
  exhaustively-listed unrestricted set moves `volume_labels` into a conditionally
  restricted class. A new requirement, **Storage-side roots narrow the Harvest
  topology read**, states the restriction, the two-phase rule, the
  output-preservation argument, the `svm=` / `node=` and match-mode exclusions,
  chunking, error class and the empty-result rule.
`cluster-topology-source` is deliberately NOT modified. Its "Topology series
consumed" requirement states that Harvest series receive no **request-scoped
matcher**, which in that spec's own terminology is the `[AECN]` / `[AEC]`
selector dimensions. A root-derived scope is not one, exactly as the QoS read's
`volume` alternation is not — and that scope was added without amending this
requirement either. The statement stays literally true.

## Impact

- `pkg/promql/queries.go`, `pkg/promql/scope.go` — a Harvest-topology renderer
  taking the root values per label, and a token renderer for phase 2; both reuse
  `normaliseValues` / `appendMatcher` / `ChunkScope`. `queryDims` untouched, so
  `render-baseline.txt` does not move.
- `pkg/build/topologyplan.go` — `storagePlan` carries the ONTAP-cluster and
  aggregate roots and the resolved match mode; a predicate answering whether this
  build restricts the Harvest topology read.
- `pkg/build/volumelabelscope.go` (new) — phase 1's matcher set, phase 2's token
  scope, and the merge.
- `pkg/build/topology.go` — the Harvest topology leg becomes plan-dependent; the
  QoS wave's prerequisite becomes the merged result.
- `pkg/build/netapp.go` — the `netapp_volume_join_miss` gate.
- `pkg/build/build.go` — `BuildStorage` passes the storage-side roots into the
  plan (`graph.StorageRoots` already reaches it).
- Tests: parity pins (restricted vs unrestricted body, including a clone on a
  lexically-smaller aggregate and a cross-filer FlexVol-name collision), owner-vote
  preservation under `aggr=`, the `svm=` / `node=` and match-mode opt-outs, phase-2
  token rendering per mode, chunking and chunk-order, the empty-phase-1 rule, the
  suppressed join-miss signal, `/v1/graph` invariance, and a storage integration
  case.
- Docs: `docs/upstream-metrics.md` (storage fan-out), `docs/netapp-harvest-preconditions.md`,
  `README.md`, `README.zh-tw.md`, `CLAUDE.md`.
