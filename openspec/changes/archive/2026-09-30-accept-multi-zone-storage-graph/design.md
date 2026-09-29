# Design

## Context

See proposal.md — Why. The pieces a multi-zone storage build touches:

- **Parser.** `ParseStorageValues` (`pkg/kubegraph/parse.go`) calls
  `exactlyOne("az" | "env")`. Everything downstream already takes a value SET:
  `promql.Selector.AZ` / `.Env` are slices rendered sorted/de-duplicated, and
  `Router.QuerierFor` normalises `sel.AZ` into the backend selection.
- **Ids.** Kubernetes ids go through `build.clusterResolver`
  (`<az>-<env>-<cluster>`); NetApp ids are `netapp/<oc>/…`. The operator
  guarantees ONTAP cluster and controller names unique across the estate, so
  no id change is needed (the explore session's composition idea is dropped).
- **Keys that assume one zone.** `claimKey` already carries `(az, env,
  cluster, namespace, claim)`. `podSeriesKey` (`pkg/build/nodeseed.go`) does
  NOT — it keys `(raw cluster, namespace, pod)` and is used by
  `podsNewestOn` / `keepPodBindings` (node seed), `podKeysOf` (application
  recovery) and `readApplicationBindings`. Under two zones holding cluster
  `c1`, `podsNewestOn` elects ONE newest incarnation for both zones' `shop/db-0`
  and can hide the other zone's pod from its root node.
- **Join.** `resolveNetAppStorage` gathers each claim's `volume_labels`
  candidates by token (`candsByClaim`); `pickAggr` / `pickSVM` / the QoS scope
  and ceiling key all run over them. Neither `pvcVolume` nor
  `volumeLabelCandidate` carries a zone today. Alert matching already has the
  zone-agreement rule (`zoneOf`, `zoneAdmits`, `alertZone` in `alerts.go`).
- **Aggregate / SVM roots.** `topologyPlan.volumeAggrs` / `volumeSVMs` are bare
  name lists feeding the phase-1 chunker (`rootedVolumeLabelsChunks`), owner
  completion's "already read whole" test (`ownerCompletionTargets`,
  `uncoveredAggrPairs`), the flowless gauge read (`readFlowlessHarvest` →
  `issueHarvestByName`) and the projection (`resolveStorageRoots`, `named(n)`).

## Goals / Non-Goals

**Goals:**

- A multi-zone body equals the union of its single-zone bodies (spec
  "A multi-zone body is the union of its zones"), pinned by a test that builds
  all three and compares node and edge sets.
- Single-zone requests: identical queries, identical bodies, identical goldens.
- A qualified `aggr=` / `svm=` value reads and roots exactly one filer's
  component.

**Non-Goals:**

- Zone-qualified NetApp ids; a paired `(az, env)` parameter; making `az` /
  `env` optional; any change to `/v1/graph`'s parser.
- Per-zone partial results: a multi-zone build is still fail-closed across
  every backend it reaches.

## Decisions

### D1 — Parser: at least one, repeatable

Replace `exactlyOne` with `atLeastOne(param, values)` for `az` and `env`: empty
values dropped, none left ⇒ `missing_<param>` (same message), otherwise the
full set. Every value still goes through `validateSelectorValues`. The
`"must be single-valued"` message disappears. Order of checks (window → az →
env → roots) is unchanged.

### D2 — Row-to-object keys carry the cluster identity

`podSeriesKey` becomes `{az, env, cluster, namespace, pod}`, read through the
configured `LabelKeys` with `cluster` put through `bucketCluster` — exactly
`claimKey`'s construction, so the two keys agree on what "one cluster" is.
Every constructor (`podsNewestOn`, `keepPodBindings`, `podKeysOf`) takes the
keys; `podRefsOfKeys` still drops everything but `(namespace, pod)` for the
per-namespace query (the query spans the selected zones; the key filters the
rows). Sort order becomes `(az, env, cluster, namespace, pod)`.

In a single-zone request every row carries the same `(az, env)`, so every
key comparison — and every body — is unchanged. This is the only place the
audit found a raw-`cluster` key on a row-to-object match; `nodeseed.go`'s node
comparison keys on the root NODE name, which is intentionally
cluster-agnostic.

*Alternative rejected:* key on the composed identity string. The composition
needs the resolver built in `parseTopology`, which runs after these seeds;
the raw triple is available at read time and is what `claimKey` already uses.

### D3 — Zone-agreeing candidate set in the join

- A shared `zone{az, env}` type replaces `alertZone` (alerts keep their
  semantics; `zoneOf` / `zoneAdmits` move with it or are re-exported
  internally).
- `pvcVolume` gains `zone` (with `zoned bool`), filled in `topology.go` from
  the claim's `kube_persistentvolumeclaim_info` series — the row `volumename`
  already comes from — through `zoneOf`.
- `volumeLabelCandidate` gains `zone` / `zoned` from its series.
- In `resolveNetAppStorage`, the append into `candsByClaim[ci]` (and
  `volsByClaim`, which feeds the QoS candidates) is skipped when both sides are
  zoned and differ. `volIndex` / `allByAggr` are NOT filtered, so the owner vote
  and the inventory see every series.
- `resolveNetAppStorage` gains the `LabelKeys` it needs (passed from
  `parseTopology`, which has them).

The QoS scope (`qosVolumeScope`) and phase 2's token read stay supersets —
they only fetch; the filter decides. Joining then yields, for a zoned claim,
exactly the candidates a single-zone build of its zone would have fetched.

*Alternative rejected:* filtering at query time (one Harvest read per zone).
Multiplies Harvest queries by the zone count, and still needs the in-memory
rule for the unfiltered `/v1/graph`.

### D4 — Qualified roots as `(ONTAP cluster → names)` maps

- `graph.StorageRoots` gains `Qualified []ONTAPRef` (`{ONTAPCluster, Name}`,
  sorted, de-duplicated) for `aggr` / `svm`; bare values stay in `Names`. A
  qualified ref whose name is also bare is dropped at construction (the bare
  value subsumes it). Parsing reuses the `<a>/<b>` splitter of `pod=`.
- `topologyPlan` gains `volumeAggrPairs` / `volumeSVMPairs map[string][]string`.
  `harvestSeed`, `flowlessAggrGauges`, `flowlessControllers` and
  `prepareFlowless` answer from bare OR qualified.
- **Phase 1.** `rootedVolumeLabelsChunks` emits, after the bare groups, one
  group per ONTAP cluster in sorted order — `groupAggr` with `clusters=[oc]`
  (resp. `groupSVM`) — rendered by the existing `RenderVolumeLabelsRooted` /
  `RenderVolumeLabelsSVMRooted` (a one-element cluster set renders as an
  equality). The chunk cap counts every group. The `ontap_cluster=` narrowing
  the renderers support is unrelated (it is a different root kind) and stays
  empty here.
- **Owner completion.** `ownerCompletionTargets` / `uncoveredAggrPairs` treat a
  qualified `(oc, aggr)` as read whole, like a bare aggregate.
- **Flowless gauges.** Qualified aggregates go through `issueHarvestPairMap`
  (the `(ONTAP cluster → names)` reader the reached-component read already
  uses); bare ones stay on `issueHarvestByName`. Controller names are derived
  from their rows exactly as today.
- **Projection.** `resolveStorageRoots` for `aggr` / `svm` matches
  `named(n) || qualified[(labels.ontap_cluster, name)]`.

### D5 — Proving the union invariant

A `pkg/build` test drives one mocked two-zone estate (cluster `c1` in both
zones, colliding aggregate and FlexVol names, a pod recreated across nodes in
one zone) through `BuildStorage` + `ProjectStorage` three times — zone-a,
zone-b, both — and asserts node-id and edge-id sets of the third equal the
union of the first two, with equal edge weights. The mock routes by the
rendered `az` matcher, so it also checks that the multi-zone selector reaches
every family. An integration test does the same against two zone-scoped
VictoriaMetrics backends.

## Risks / Trade-offs

- [More zones ⇒ more backends ⇒ higher chance one fails the whole build
  (fail-closed)] → intended: a partial multi-zone body would read as a smaller
  estate. Operators wanting resilience issue per-zone requests.
- [A zone alternation lengthens every matcher, so the root chunk cap is reached
  with fewer root values] → the cap and its 400 are unchanged in kind; the
  budget already charges `RequestMatcherCost`.
- [Cartesian product includes combinations the caller did not intend] →
  documented; such a combination only contributes what the estate holds under
  it.
- [Zone-agreeing join changes an unfiltered `/v1/graph` body when a FlexVol
  token collides across zones] → recorded in `docs/BREAKING.md`; stock
  UUID-derived PV names do not collide.
- [Harvest series without `az` / `env` keep joining any claim] → "unknown never
  excludes" preserves today's behaviour for them; D13 already requires the
  labels for filtered reads.

## Migration Plan

Additive for clients (a previously rejected request now succeeds). Goldens
unchanged; new goldens for a multi-zone and a qualified-root request. Rollback
is a plain revert.
