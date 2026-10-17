# Design

## Context

See proposal.md — Why. Current state, as relevant here:

- `pkg/promql`: `dimsHarvest = dimAZ | dimEnv` (read-storage-roots-through-volume-hub
  D13). Matcher rendering is generic — `Render`, `requestMatchers` and
  `RequestMatcherCost` all read `queryDims` — so every unrestricted Harvest leg
  and every restricted renderer (`RenderVolumeLabels{Rooted,SVMRooted,ByNode,
  OwnerCompletion,TokenScoped}`, `RenderQoSVolumeScoped`, the `harvestpair`
  renderers) picks the matcher up from the one table entry. `Family.AcceptsAZ()`
  (routing) and `Family.RendersAZ()` (matcher; read by `Router.QueryLabels`) are
  both derived from `queryDims`; before D13 a routing-only `dimAZRoute` bit made
  them differ for `harvest`.
- `pkg/build/netapp.go` `resolveNetAppStorage`: hop A drops a matched
  `volume_labels` series from a claim's candidates when `zonesAgree(claim.zone,
  cand.zone)` is false (accept-multi-zone-storage-graph). Hop B indexes QoS rows
  by `volume` with no zone, and `sumQoSIO` SUMS every in-scope volume-level row.
  Hop C (`indexPolicyCeilings`), the aggregate / controller gauges and
  `node_labels` already define a duplicate rule (smallest value, degraded-if-any,
  smallest non-empty label) — written for "two Harvest instances scraping one
  filer".
- `pkg/build/alerts.go` `ontapZonesOf` reads every pair the raw Harvest vectors
  carry; it is not touched by anything below.
- Main specs `graph-api`, `upstream-backend-routing` and `metrics-label-query`
  were never updated by D13 and still describe "Harvest is zone-routed, no `az`
  matcher" — the state this change returns to.

## Goals / Non-Goals

**Goals:**

- No Harvest query carries a request-scoped matcher; `az` keeps routing.
- A series returned under several `(az, env)` pairs changes no figure: no I/O
  sum counts it twice, and every pick stays order-free (D6 of the original design).
- A claim still resolves among its own zone's FlexVols when they exist.

**Non-Goals:**

- No change to the Kubernetes side: every kube-state-metrics, kubelet and
  `ALERTS` query keeps its `az` / `env` matchers, and the storage build still
  loads no claim of an unselected zone.
- No change to the zone-agreeing alert match (`alert-overlay`).
- No new flag, no backend-file field, no change to `RenderVolumeLabels*` /
  `RenderQoSVolumeScoped` signatures.

## Decisions

### D1 — Restore the routing-only `dimAZRoute` bit

`dimsHarvest = dimAZRoute`. `render` and `Reaches` ignore the bit;
`familyAcceptsAZ = buildFamilyDim(dimAZ | dimAZRoute)`, `familyRendersAZ =
buildFamilyDim(dimAZ)`. Every Harvest query — unrestricted and restricted —
loses the matcher through the existing generic plumbing; `RequestMatcherCost`
returns 0 for Harvest, so the phase-1 / owner-completion / pair chunkers get the
bytes back. A `Router.QueryLabels` call naming family `harvest` with an `AZ`
is routed by it and renders no matcher (the `metrics-label-query` spec already
says so).

Alternatives:

- **Drop routing too (`dimsNone`).** Would make `Table.Select` ignore Harvest
  `zones`, fanning every `?az=` request to every Harvest store, and would turn a
  `QueryLabels(harvest, AZ)` call into a validation error — an embedder break
  nobody asked for. The request was to stop *filtering*, not to stop *routing*.
- **Drop only `env`.** `az` fails the same way (a filer labelled with another
  pair vanishes), so it would solve half the problem.
- **Opt-in flag.** A switch for removing a filter is configuration surface with
  no user; D13 declined the reverse flag for the same reason.

### D2 — Hop A: prefer the claim's zone, fall back to every match

Keep the single pass over `volume_labels`, but record each match with its
agreement bit instead of skipping disagreeing ones; after the pass, a claim's
candidate set (and the FlexVol names `volsByClaim` feeds to hop B) is its
agreeing matches when it has any, and all its matches otherwise. `volIndex` /
`allByAggr` stay unfiltered as today.

Alternatives:

- **Keep the strict exclusion.** Re-imposes the removed matcher in Go: a claim
  whose filer is labelled with another pair loses its edge, aggregate, SVM and
  QoS — the exact failure this change removes (D13's own rejected alternative,
  "filter Harvest rows in Go", in the opposite direction).
- **Drop zone agreement.** In a catch-all store a cross-zone FlexVol-token
  collision would again resolve to the lexically-smallest `(ontap_cluster,
  aggr)` regardless of the claim's zone, moving claims onto another zone's filer
  and breaking the multi-zone union property for ordinary estates.

### D3 — Hop B: one zone per workload, chosen among in-scope rows

`qosCandidate` gains `zone` (read by `zoneOf`; `indexQoSFamily` takes the label
keys) and `volume`. After `qosCandidatesFor`, `preferQoSZone` keys the rows that
pass `qosInScope(c, keyOC, svm)` by workload — `(ontap_cluster, svm, volume,
lun, family)` — and keeps, per key, the rows of ONE zone: the claim's zone, else
rows carrying no pair, else the smallest complete pair by `zone.compare`
(`zone.prefers`). The result feeds BOTH `pickPolicy` (via `resolveCeiling`) and
`sumQoSIO`, which therefore no longer re-test `qosInScope`. Choosing among
in-scope rows only means a zone that exists only on another filer can never
win and blank the measurement. Kept rows stay in input order, so the summation
order is still the caller's.

Rows with no pair rank above another zone's because `zonesAgree` treats an
unknown zone as agreeing: hop A prefers them for the same reason, so hop B
reading a disagreeing zone's policy group while hop A joined through an
agreeing row would describe two different views of one volume.

Why only hop B: it is the only reader that SUMS across rows. Hop A's picks are
lexical minima, which a duplicate differing only by zone cannot move; hop C,
the gauges and `node_labels` already reduce duplicates per key.

Alternatives:

- **One zone group per claim** (the first implementation). Drops genuine I/O:
  a claim matching two FlexVols, one labelled with its zone and one not, lost
  the second FlexVol's figures; a claim whose own-zone rows were LUN-only lost
  its volume-level measurement entirely and fired `netapp_qos_join_miss`.
- **De-duplicate every Harvest vector at the read boundary by label set minus
  `az` / `env`.** Duplicates from two pollers can also differ in poller labels
  (`datacenter`, `instance`), so the fingerprint would not collapse them; and
  stripping zones at the boundary would erase what D2 and `ontapZonesOf` read.
- **Per-family or per-series max/min.** A value-based pick is not tied to any
  zone view and changes with the figures themselves; the zone rule is a pure
  function of the labels.

### D4 — Spec mechanics

`netapp-storage-graph` "Harvest legs carry the request zone and environment"
is REMOVED and replaced by an ADDED requirement, the precedent
read-storage-roots-through-volume-hub set. `cluster-topology-source` "Request-
scoped upstream selectors" is MODIFIED in place because other requirements cite
it by name; OpenSpec refuses to drop a scenario from a MODIFIED block, so its
scenario "Harvest receives the zone and environment only" keeps its title with a
body stating the new behaviour (only `az` acts on Harvest, by routing).

## Risks / Trade-offs

- [A catch-all or multi-zone `harvest` store now returns every zone's rows to
  every request] → Larger Harvest vectors per filtered `/v1/graph`; the storage
  build is still by-reference so its rows stay bounded by the root and the
  tracked claims. Query strings no longer vary with `az` / `env`, so the query
  cache shares one Harvest result across zones. Operators who want the old
  isolation declare zone-scoped `harvest` backends (BREAKING.md).
- [A bare `aggr=` / `svm=` root in a catch-all store also roots the same-named
  component of a filer labelled with another zone] → Consistent with the spec
  ("every ONTAP cluster the selected zones' Harvest backends return"); use the
  qualified `<ontap_cluster>/<name>` root or zone-scoped backends.
- [Fallback join picks a foreign filer on a token collision when the claim has
  no own-zone series] → Only reachable with non-stock FlexVol naming AND
  zone-mislabelled Harvest; before this change the claim drew no edge at all.
  Same deterministic pick as an unlabelled estate.
- [Multi-zone union can differ] → Harvest carries no matcher, so a multi-zone
  request reads every selected zone's `harvest` backends. When another selected
  zone's backend holds a row the claim also accepts (a colliding FlexVol with no
  pair, any row for a claim with no own-zone row, or the claim's QoS workload
  under another pair), the multi-zone body picks among more candidates than any
  single-zone one. Exact whenever a claim's matching rows all live in its own
  zone's store; documented in the `storage-graph-api` spec.
- [Partial duplicate: the claim's zone lacks a family another zone has] → The
  family is a separate workload key, so it is read from the next-preferred zone;
  each family is still counted once.
- [Chunk boundaries move] → `RequestMatcherCost` is 0 for Harvest, so tests
  pinning Harvest chunk counts under a filtered selector may shift; they are
  updated with the matcher assertions.

## Migration Plan

No flag, no persisted state. Deploy; Harvest queries drop the matchers. An
estate whose Harvest labels agree with kube-state-metrics produces identical
bodies. Rollback is a revert. `docs/BREAKING.md` gains an entry superseding the
D13 entry ("NetApp Harvest queries carry the request's `az` / `env`").
