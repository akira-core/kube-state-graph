## Context

See proposal.md — Why. Six properties of the current code shape this design.

**Roots already reach the build; only two of them are used.**
`Builder.BuildStorage(ctx, window, end, sel, roots graph.StorageRoots)` receives
the whole root set, and `storagePlan(roots)` projects it down to `podRoots` and
`nodeRoots`. `roots.ONTAPClusters` and `roots.Aggrs` are already in hand and are
simply dropped. No signature moves.

**The Harvest topology leg is a first-wave leg with no matcher.**
`topologyLegs` issues `QVolumeLabels` through `fetchOptional` under
`promql.Render`, which renders it bare: `queryDims[QVolumeLabels] = dimsHarvest
= dimAZRoute`, a routing-only bit. It is the build's `largest_leg` in any
NetApp estate.

**Two second waves already exist and share one engine.** `readScopedQoS` and
`issueScopedFamilies` both wait on `signalWhenDone` channels inside the one
errgroup, compute a scope, chunk it with `promql.ChunkScope` under
`Options.QoSScopeBatchBytes`, issue chunks under `scopeConcurrency`, and merge
**in chunk order**. A third wave costs no new machinery.

**The picks are lexically-smallest over a claim's whole candidate set.**
`pickAggr` returns the smallest `(ontapCluster, aggr)` among the claim's
`volumeLabelCandidate`s and `pickSVM` is then scoped to that filer. Both are
pure functions of the candidate slice, so **removing candidates can change the
answer** — the one correctness hazard this design has to close (D2).

**The owning-controller vote is per aggregate, over every series.**
`pickOwner(allByAggr[k], oc, aggr)` votes over `volIndex`, i.e. over every
volume-label series the build read, not only over the joining claim's. Which
root kinds may restrict the read follows directly from this (D3).

**The inventory has a second source.** `buildNetAppIndexes` materialises
aggregates and controllers from `v.AggrStatus` / `v.AggrSpaceUsed` /
`v.AggrSpaceTotal` and the six `node_*` families as well as from `volIndex`,
and those families stay unrestricted. That is what keeps a rooted component
drawable when no claim reaches it, and it is the direct answer to
`scope-controller-legs-by-reference` D8's first reason.

## Goals / Non-Goals

**Goals:**

- A storage-side-rooted `/v1/storage-graph` build reads volume-label series in
  proportion to the rooted components, not to the filer.
- Byte-identical bodies, including under a FlexVol-name collision that changes
  which aggregate a claim picks.
- The restriction is expressible with no inversion of the volume-key
  derivation, so `scope-controller-legs-by-reference` D8's second reason stands
  unchallenged.
- `queryDims`, `render-baseline.txt`, the request surface, the flag set, the
  `/v1/graph` fan-out and the dependency set are unchanged.

**Non-Goals:**

- Pushing `svm=` or `node=` down. Blocked by the owner vote (D3), not by
  effort. A later change may unblock `svm=` by re-sourcing the vote.
- Restricting any other Harvest family. The aggregate, controller and policy
  families are bounded by the filer's hardware, and restricting them is what
  would break root drawability.
- Re-rooting the pod scope on the matched claims. That is the larger win and a
  separate change: it inverts the build's wave order and needs a second plan
  shape. This change deliberately leaves the claim and pod side alone.
- A knob. Which roots restrict is a hardcoded property of the endpoint, like
  every other selector contract here.

## Decisions

### D1: One AND query, engaged only when every storage-side root is pushable

The projection does NOT combine the storage-side roots uniformly.
`resolveStorageRoots` gates `aggr` and `svm` matches on `inOC(n)`, so
`?ontap_cluster=A&aggr=x` roots exactly aggregate `x` **on filer A** — a
narrowing, i.e. an AND. `aggr` and `svm`, by contrast, both add to the same
`storageIDs` set and a unit is retained if it intersects any of it — a union.

Two consequences shape the query:

1. **`ontap_cluster=` and `aggr=` map onto ONE selector.** `{cluster=~"A",aggr=~"x"}`
   is exactly the projection's own rule, and it is the tightest correct read. Two
   queries merged would read every volume on filer A plus every `x` on any filer
   — a superset that costs more and roots nothing extra.
2. **A request carrying `svm=` or `node=` cannot restrict at all.** `aggr=x&svm=s`
   retains units reached only through the SVM `s`; a restriction by `aggr` alone
   would drop them and change the body. `svm=` cannot be pushed itself because of
   the owner vote (D3), and `node=` names a Kubernetes node as well and is
   admitted as a root regardless of flow (D3). So the predicate is "at least one
   of `ontap_cluster=` / `aggr=`, and no `svm=`, and no `node=`" — not merely
   "these two are pushable".

`pod=` composes freely: the projection ANDs the workload group with the storage
group, so retained units are a subset of the units the restriction keeps.

Chunking: the larger alternation (`aggr` when present, else `cluster`) is
chunked under the shared budget and the other is repeated verbatim in every
chunk, charged at its RENDERED length (`promql.MatcherCost`) rather than as a
raw join — escaping roughly doubles a metacharacter-carrying filer name and the
`=~"…"` wrapper costs eleven bytes the raw values never show, so measuring the
values would leave the budget a suggestion. Chunks of a disjoint value split
return disjoint series, so no cross-chunk de-duplication is needed; the two
PHASES do overlap and are de-duplicated by label-set fingerprint at their
merge (D2).

**The restriction is capped, and falls back rather than fails.** This is the
only scope in the package derived from the REQUEST, and `?aggr=` /
`?ontap_cluster=` are repeatable with no cap on how many values a client may
send — `validateSelectorValues` bounds each value at 253 bytes and never the
count. Unchecked, a root set large enough to collapse the per-chunk budget to
its floor turns one `last_over_time(volume_labels[w])` into one query per
value. `maxRootedVolumeLabelChunks` (= `scopeConcurrency`) bounds the chunk
count; past it, and for a root set that normalises away entirely, the leg reads
UNRESTRICTED. Falling back is right rather than merely safe: it is exactly the
read this leg performed before the restriction existed, it is one query, and the
body is identical either way. Erroring would fail a build over an OPTIONAL
family; an empty vector would silently draw no storage.

**Alternative rejected — one query per root label, merged.** An earlier draft of
this design read the roots as OR-combined and would have loaded every volume of
the rooted filer plus the rooted aggregate name on every other filer. It is
correct but wasteful, and it misstates the projection.

### D2: A second phase recovers each matched claim's full candidate set

This is the correctness core. `pickAggr` is lexically-smallest over the claim's
candidates. Restricting the read removes candidates, so a claim whose token also
matches a FlexVol on a lexically-smaller aggregate — a Trident clone
(`snap_trident_pvc_x` suffix-matches the token `pvc_x` just as
`trident_pvc_x` does), or the same name on a second filer — would be placed on
the rooted aggregate by a phase-1-only build and elsewhere by an unrestricted
one. The rooted request would then retain a claim it should not.

The fix is to re-read the family for exactly the claims phase 1 matched,
restricted on `volume` to their derived tokens **in the forward direction**:

| mode | rendered branch per token |
|---|---|
| `suffix` (default) | `.*<token>` — PromQL anchors the alternation, so this is exactly "ends with" |
| `exact` | `<token>` |

This is not an inversion. The token is what `VolumeKeyRewriter.token(pvName)`
already computes from a PV name the build holds; the regex expresses the
comparison the Go-side matcher performs. D8's second reason — that the join
cannot be expressed "without the provisioner's prefix" — is about the reverse
direction (FlexVol name → PV name) and is untouched: the prefix is precisely
what `.*` stands in for.

Why it is sufficient: a claim can only be retained by a storage-side root if its
picked aggregate or SVM is a rooted id, which requires at least one candidate on
a rooted component, which means phase 1 matched it. So the set of claims whose
pick must be exact is a subset of the claims phase 1 matched, and phase 2
restores each of their candidate sets in full.

**Alternative rejected — accept the deviation.** It is small and arguably more
intuitive (a claim with a volume on the rooted aggregate showing up under it),
but every prior change here holds a byte-identical-body bar, and a
silently-different body under Trident clones is exactly the class of bug this
repo keeps hardening against.

**Alternative rejected — a phase 2 keyed on volume NAMES from phase 1.** It
returns the same series phase 1 already has: the clone is a different volume
name, so it is never recovered. Only the claim's token can reach it.

### D3: Only `ontap_cluster=` and `aggr=` restrict; `svm=` and `node=` do not

`pickOwner` votes over every volume-label series of an aggregate. Restricting
by `aggr` keeps **all** of a rooted aggregate's series, so the vote is over the
same population as an unrestricted read and the `node-aggr` tier is unchanged.
Restricting by `cluster` keeps all series of every aggregate on the filer, so
the same holds.

Restricting by `svm` keeps only the volumes of that SVM, so an aggregate serving
two SVMs votes over a subset and can elect a different controller — a body
change on a tier the Sankey draws. Restricting by `node` is worse: it keeps only
the series already naming the rooted controller, which makes the vote
self-fulfilling and hides a takeover the unrestricted read would surface.
`node=` is also matched against Kubernetes node names, so pushing it onto a
Harvest label would drop half the root's meaning.

Phase 2 cannot rescue either: it recovers a claim's candidates, not an
aggregate's. And it is not enough that `svm=` / `node=` are individually
unpushable: their PRESENCE disables the restriction for the whole request (D1),
because the projection unions `aggr=` with `svm=` and admits a `node=` root
whether or not any flow reaches it.

**What would unblock `svm=` later:** re-source the vote from
`aggr_new_status` / `aggr_space_*`, which already carry `node`, are already read
unrestricted, and are bounded by the aggregate count. Today that is only
`gaugeOwner`, the fallback for an aggregate no volume names. Promoting it to a
co-equal source is a behaviour change to the "NetApp aggregate entity"
requirement and belongs in its own change.

### D4: `contains` and `regex` match modes opt out entirely

`RenderScoped`-style escaping (`regexp.QuoteMeta` + anchored alternation) makes
a literal match itself. `suffix` and `exact` are literal comparisons and render
exactly. `contains` would need `.*<token>.*`, which is expressible — but a
`contains` token is chosen precisely because the estate's naming is irregular,
and a `regex` token IS user-supplied RE2 whose own anchors (`^`, `$`) change
meaning inside a fully-anchored alternation. Rather than reason case by case,
both modes read unrestricted and single-phase.

This costs nothing in practice: `suffix` is the default and the mode the
documented Trident estate uses. The gate lives beside the mode on
`VolumeKeyRewriter`, so a future mode is classified rather than defaulted.

### D5: A third second-wave, gated on phase 1 and the claim-info family

Placement mirrors `readScopedQoS` exactly:

```
first wave:   volume_labels(phase 1, restricted) ‖ kube_persistentvolumeclaim_info ‖ …
                     │                                      │
                     └──────────────┬───────────────────────┘
                                    ▼
                     volume_labels(phase 2, token-restricted)
                                    ▼
                     merge → QoS wave (volume scope over the merge)
```

Phase 1 stays a first-wave leg because its restriction comes from the request,
not from data — nothing precedes it. Phase 2 needs the claims, so it waits on
`QPVCInfo` and on phase 1, through the existing `signalWhenDone` channels. The
QoS wave's prerequisite moves from "phase 1 landed" to "the merge landed", so
its `volume` scope is computed from the same candidate set the parse sees.

**Cost, stated plainly:** an unrooted build is unchanged; a rooted build adds
one sequential Harvest hop. In the measured estate that moves the Harvest
chain's tail from 119 ms to about 144 ms at 25 ms RTT, against a drop from
20,000 to roughly 900 series on that leg. Where an upstream series limit binds,
that trade is the whole point; where latency binds and the filer is small, the
restriction is simply not engaged because the request carries no storage root.

### D6: The tally stays one entry; chunks keep the family's error class

Both phases issue under the bare family name `volume_labels`, so
`kube_state_graph_upstream_query_duration_seconds`, `..._failures_total`,
`..._result_series` and the `prometheus.query` span keep one value per family
however many queries a build issues — the rule the QoS and by-reference waves
already follow. `RawSeriesCount["volume_labels"]` is the merged count.

The family is OPTIONAL (`fetchOptional`) today, so a chunk logs and continues.
A failed phase-1 chunk costs the aggregate edges of the claims whose volumes it
carried; a failed phase-2 chunk can leave a pick over an incomplete candidate
set. Both are strictly better than failing the build for a family whose absence
is the documented normal state of a non-NetApp deployment, and both are already
what an unrestricted failure does at whole-leg granularity.

### D5b: Phase 2 skips the clusters phase 1 read whole

A restriction by ONTAP cluster ALONE reads those filers whole, so every
candidate of every matched claim on them is already in hand and phase 2 has only
cross-filer candidates left to find. It therefore renders
`cluster!~"<the rooted clusters>"` beside its token alternation. Without it the
common cluster-rooted request pays a second pass over the family it just read —
and a `.*tok` branch cannot use a prefix index, so that pass is a full regex
scan of the `volume` label set, on the build's critical path (the QoS wave waits
on the merged result).

It is sound ONLY with no aggregate root. With one, phase 1 read part of a
cluster, and a candidate on another aggregate of that same cluster is precisely
what phase 2 exists to recover — excluding the cluster would reintroduce the
bug D2 closes.

### D7: The join-coverage signal counts what the restriction still explains

`resolveNetAppStorage` counts `netapp_volume_join_miss` for every loaded claim
resolving no aggregate, gated on `len(volumeLabels) > 0`. Under a restriction
that gate is met while nearly every claim legitimately resolves nothing, so the
signal would fire on the whole estate every request.

Suppressing it wholesale for a restricted build would be the easy answer and is
the wrong one: it silences the signal on a request that excluded nothing (a
single-filer estate rooted at its only `ontap_cluster=`), and it pushes
operators onto a documented workaround for a signal the code can still emit
correctly. The gate is per CLAIM instead. A claim that matched no volume-label
series at all is off the rooted components — unknowable as a coverage failure —
and is not counted. A claim that matched a series and still resolved no
aggregate is a FlexGroup, which is a genuine miss under either read, and is.
An unrestricted read counts both, exactly as before.

`netapp_qos_join_miss` is unaffected — it is gated on a QoS vector being
non-empty and counts only claims that already drew an edge.

### D8: `cluster-topology-source` is not amended

Its "Topology series consumed" requirement says Harvest series receive no
**request-scoped matcher**, which in that spec's own notation is the `[AECN]` /
`[AEC]` selector dimensions. A root-derived scope is not one — and the QoS
`volume` alternation, which is the existing precedent for a non-selector scope
on a Harvest family, was added without amending it either. The statement stays
true; `netapp-storage-graph` is where the distinction is spelled out.

## Risks / Trade-offs

- [A rooted build costs one more Harvest RTT] → engaged only when the request
  carries a storage-side root, i.e. exactly when the read it replaces was
  largest; an unrooted build is byte- and timing-identical.
- [Phase 2's `.*<token>` regexes are slower upstream per series than an equality]
  → the alternation is one branch per MATCHED claim (tens, against the
  thousands of series it replaces), chunked under the existing budget; net
  selected-series count falls by more than an order of magnitude.
- [An aggregate reached ONLY through phase 2 votes for its owner over a partial
  series set] → `pickOwner` reads every series of an aggregate that reached the
  merged vector, so a clone's aggregate can elect a different controller than
  the whole filer would. It is never drawn: it is not a root, and the unit whose
  claim reached it does not intersect the rooted ids, so `ProjectStorage` drops
  it. That invariant lives in `pkg/graph`, so widening `resolveStorageRoots`
  would start drawing a controller chosen from a partial vote —
  `TestRootedVolumeLabels_PhaseTwoOnlyAggregateIsNeverDrawn` pins it, and the
  rooted aggregates themselves are unaffected because phase 1 reads all of
  their series.
- [A phase-2 chunk failure can leave a pick over an incomplete candidate set]
  → same degrade class the family already has; logged and counted under
  `volume_labels`; the alternative is failing a build for an OPTIONAL family.
- [Two root parameters issue two queries where one leg existed] → both are
  narrow; the merge de-duplicates by label set so `pickOwner` cannot double-vote.
- [Suppressing the join-miss signal hides a genuine derivation mismatch on
  rooted requests] → the signal still fires on every unrooted request, which is
  what `make verify` and `scripts/wait-ready.sh` issue; documented in
  `docs/netapp-harvest-preconditions.md`.
- [The restriction is request-derived, unlike every other Harvest scope] →
  precedent is `podRoots` / `nodeRoots` reaching the pod and node scopes
  (`harden-topology-read-cardinality` D7, which revised the original "roots
  never reach the build" stance for the same reasons: no result cache to widen,
  and the alternative is reading the whole estate).
- [The one body-changing corner] → the alert overlay decides bare-name
  uniqueness over the ASSEMBLED node set, and a restricted read shrinks the
  NetApp inventory to what `volume_labels` still names plus what the gauge
  families name. The set can differ only for an aggregate or controller that
  (1) sits outside the rooted components, (2) is named by `volume_labels` alone
  — no `aggr_*` / `node_*` series — and (3) shares its bare name with an entity
  an alert without a disambiguating `cluster` label also matches. Then an alert
  that was ambiguous (attached to neither) becomes unique and attaches. The
  stock Harvest `aggr_*` and `node_*` templates name every aggregate and
  controller, so an estate running them cannot hit it; the proposal and the
  spec scope their byte-identical obligation to such an estate rather than
  claim it unconditionally.
- [A future non-default match mode silently reads unrestricted] → the mode gate
  is a table beside the mode enum, so a new mode must be classified; a test pins
  that every mode has an entry.

## Migration Plan

Deploy with no configuration change; no persisted state and no schema. The
restriction engages on requests that already carry storage-side roots and is
invisible to every other request. Rollback is a revert — an unrestricted read
produces the same bodies.

Operators who alert on `netapp_volume_join_miss` should know it no longer fires
for storage-rooted requests; the unrooted `make verify` path still exercises it.

## Open Questions

- Whether the `svm=` unblock (D3's re-sourced owner vote) is worth its own
  change, or whether `svm=` roots are rare enough in practice to leave
  unrestricted indefinitely. It does not affect this change's specs or tasks.
