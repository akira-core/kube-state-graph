---
paths:
  - "pkg/build/netapp*.go"
  - "pkg/build/qosscope*.go"
  - "pkg/build/volumekey*.go"
  - "pkg/build/volumelabelscope*.go"
  - "pkg/build/claimscope*.go"
  - "pkg/build/flowless*.go"
  - "pkg/build/zone*.go"
  - "pkg/promql/qosscope*.go"
  - "pkg/promql/volumelabels*.go"
  - "pkg/promql/harvestpair*.go"
---

# NetApp storage join

Disclosed reference for `CLAUDE.md`. The text is the authoritative statement of each rule; `CLAUDE.md` carries only a one-line reminder.

## The three hops

- **NetApp storage join is three independently-degrading hops** (`pkg/build/netapp.go`
  `resolveNetAppStorage`), all rooted at the same key — the PVC's `volumename`
  (the bound PV name) **rewritten into a match token** and matched against the
  **stock** Harvest `volume` label (the ONTAP FlexVol name). It is NOT a label
  equality: ONTAP volume names admit only letters, digits and `_`, so a `volume`
  value can never equal a `pvc-<uuid>` PV name. The derivation is an ordered
  list of regex rewrite rules (`--netapp-volume-key-rewrite`), defaulting to
  "replace `-` with `_`", compared as a **suffix** — which resolves a stock
  Trident estate without the deployment declaring its `storagePrefix`, while
  rejecting a clone whose name extends past the PV name. A FlexVol named
  exactly the token matches. `pkg/build/volumekey.go` owns it and resolves
  suffix through a length-bucketed hash index (O(volumes)). An uncompilable
  pattern is a STARTUP failure (`internal/config`), never a silent fallback.
  **No relabel rule is required or read.** Hops:
  - **hop A `volume_labels`** — the SOLE source of storage *topology*: the
    `pvc-to-netapp-aggr` edge, the `netapp-aggr` / `netapp-node` entities, and
    the PVC `svm` label. An **info series**: its sample value is discarded, only
    its labels (`cluster` = ONTAP cluster, `node`, `aggr`, `svm`) are read.
    **Zone agreement** (accept-multi-zone-storage-graph): a claim's CANDIDATE set —
    the matched series every pick runs over (aggregate, SVM, the owner the edge
    names, the QoS scope, the ceiling key) — holds only series whose zone agrees
    with the claim's: `pvcVolume.zone` is the `(az, env)` pair of the claim's
    `kube_persistentvolumeclaim_info` series and `volumeLabelCandidate.zone` the
    series' own, both read through the configured label keys by `zoneOf` (the
    shared `zone` type in `pkg/build/zone.go`, which alerts use too), and
    `zonesAgree` excludes a series only when BOTH sides carry a complete pair and
    the pairs differ — an unknown zone never excludes. `volIndex` / `allByAggr`
    are NOT filtered, so the owner vote and the inventory see every series. It
    makes a cross-zone FlexVol-token collision (a Trident clone, a hand-chosen
    naming scheme) resolve within each claim's own zone; it applies to the shared
    join, so an unfiltered `/v1/graph` changes only in an estate holding such a
    collision.
    **Rooted read** (scope-volume-labels-by-storage-root,
    read-storage-roots-through-volume-hub): a `/v1/storage-graph` request rooted
    at `ontap_cluster=`, `aggr=` and/or `svm=` reads it restricted. Phase 1 is up
    to two query GROUPS mirroring the projection, which UNIONS `aggr=` with
    `svm=` and narrows both by `ontap_cluster=`: `{cluster=…,aggr=…}` and
    `{cluster=…,svm=…}` (the cluster matcher an AND — an `aggr=`/`svm=` root
    names a component only WITHIN the `ontap_cluster=` values; `{cluster=…}` alone
    for a cluster-only request), merged in (group, chunk) order de-duplicated by
    fingerprint. A QUALIFIED `aggr=` / `svm=` value adds one group per ONTAP
    cluster after the bare groups (`{cluster="oc",aggr=~…}` / `{cluster="oc",svm=~…}`,
    clusters in sorted order; `rootedVolumeLabelsChunks`), and the chunk cap
    counts every group; a qualified aggregate is read whole like a bare one
    (owner completion and `uncoveredAggrPairs` skip it), and its flowless gauges go
    through `issueHarvestPairMap` — assigned by the by-name read first, merged by the
    pair read after, so the order is load-bearing. **Owner completion** then re-reads whole every
    `(cluster, aggr)` an SVM-group row names, minus the aggregates an `aggr=`
    root already read whole, because `pickOwner` votes over EVERY series of an
    aggregate and an SVM group returns only its share (the takeover case). Phase 2
    re-reads `volume=~".*<token>"` for
    exactly the claims phase 1 matched, because `pickAggr` / `pickSVM` are
    lexically-smallest over a claim's WHOLE candidate set and a Trident clone or a
    same-named FlexVol on a second filer would otherwise move a claim onto or off
    the rooted aggregate. Under an `svm=` root, every aggregate phase 2 ALONE
    named is completed too, after phase 2: the two picks are separate, so a
    claim retained through its rooted SVM can land on such an aggregate and draw
    its controller. Completion and phase 2 are merged (fingerprint de-dup —
    a series two reads return must vote once in `pickOwner`) only AFTER the seed's
    claim read took its candidates from phase 1, so completion rows are never a
    claim source. It is the FORWARD derivation the join already computes. Phase 2
    is NOT issued when phase 1 matched no claim. A restriction naming ONLY ONTAP
    clusters gives phase 2 a `cluster!~"…"` for them — phase 1 read those filers
    whole. A seed that would take more than `maxRootedVolumeLabelChunks`
    (= `scopeConcurrency`) queries is rejected 400 `invalid_scope` before any
    query: the root parameters are repeatable and the parser bounds each value's
    LENGTH, never the count, so this is the one scope a client can inflate.
    A request carries one root kind, so `pod=`, `pvc=`, `pv=`, `application=` and
    `node=` do not compose with a storage root. The join is always suffix, so the token
    restriction is always renderable. `/v1/graph` never restricts
    (`fullPlan` answers false structurally). The one body-changing corner: an
    aggregate or controller named by `volume_labels` ALONE (no `aggr_*` /
    `node_*` series) outside the rooted components is not materialised, which can
    change whether a `cluster`-less alert matches a unique entity — the stock
    Harvest templates name every one, so it needs a non-stock estate.
    **A storage root's seed reads claims FROM the FlexVol name** (`claimscope.go`):
    `pvCandidates` turns every `pvc_` boundary suffix of a seeded `volume` into a
    PV name (a generator, never a judge; static PVs and custom volume-name
    prefixes are NOT reached from a storage root), `kube_persistentvolumeclaim_info`
    is scoped on `volumename`, and the expansion reads the other claim families
    one query per namespace on `(namespace, persistentvolumeclaim)`
    (`issueClaimFamiliesByNamespace`, shared by every seed kind's claim side and
    mounter completion — a claim name alone would be read across the estate),
    filtered to the loaded `(az, env, cluster, namespace, claim)` keys. A data-derived scope is chunked however large it is and never replaced
    by a read across the zone. `storage_root_claim_miss` (`no_pv_candidate` /
    `no_claim`) reports a seed that found nothing (`no_claim` drops to Debug under
    a `cluster=` / `namespace=` filter). **The build stays in the request's zones**
    (D7): `buildStorage` renders the request's FULL selector and binds
    `QuerierFor(sel)`, so no query reaches the store of a zone the request did
    not select and a filer shared across zones draws the selected zones' claims
    only — operators require that a request queries only its own az / env
    backends. Do NOT
    reintroduce an az/env-relaxed or per-family-zone-routed read. Alert
    matching also AGREES ON ZONE (D11, `pkg/build/alerts.go`), for the builds
    that do hold several zones (unfiltered `/v1/graph`, a multi-zone `/v1/storage-graph`, catch-all backends): an alert's `az`/`env` pair must agree with the
    candidate's zone — a Kubernetes object's composed identity, a NetApp
    controller/aggregate's `ontapZones` set (the pairs its entity-naming Harvest
    series carry, collected by `ontapZonesOf`, QoS excluded). The check runs on
    the cluster-qualified NetApp path and filters every kind's no-`cluster`
    candidates BEFORE `matchUnique`; an unknown zone on either side never
    excludes. It runs on every build path. Tests: `pkg/build/volumelabelscope_test.go`
    (parity across root shapes, clone, cross-filer collision, takeover, owner
    completion, chunking, degrade), `pkg/build/claimscope_test.go`,
    `pkg/build/hubrouting_test.go`, `pkg/promql/volumelabels_test.go`,
    `internal/integration` (`TestStorageGraphHubStaysInTheRequestZone`).
  - **hop B `qos_{read,write}_{ops,latency,data}`** — the six measured I/O
    figures. **Volume granularity is a READER rule, not a matcher** (D11): the
    queries carry the `volume` scope and nothing else, and `sumQoSIO` skips
    every candidate with a non-empty `lun`. ONTAP collects a workload per LUN as
    well as per volume and a LUN workload carries its FlexVol's `volume`, so the
    two must never be summed — but the LUN row has to be FETCHED, because on an
    `ontap-san` backend the QoS policy is attached to the LUN and the FlexVol's
    own workload sits in ONTAP's built-in `User-Best_effort` class, which
    declares no ceiling and has no fixed-policy series. A `lun=""` matcher hid
    the only route to a SAN ceiling; it was also never airtight, since an
    empty-string matcher admits rows carrying no `lun` label at all. Cardinality
    stays bounded because the six legs are issued ONLY through
    `RenderQoSVolumeScoped`. Candidates are further scoped in
    Go to the picked aggregate's ONTAP cluster (and its `svm` when both sides
    resolve one) so a colliding FlexVol name across two filers cannot merge.
    **These six legs are read in a SECOND WAVE** (`pkg/build/qosscope.go`
    `readScopedQoS`), issued only after hop A and restricted to exactly the
    FlexVol names the loaded claims matched
    (`promql.RenderQoSVolumeScoped` renders an anchored `volume=~"…"` as the
    query's ONLY matcher). The launcher waits on `kube_persistentvolumeclaim_info`
    and `volume_labels` ALONE — a dependency edge inside the one errgroup, not a
    barrier behind the whole first wave. An **empty scope issues no QoS query at
    all** (hop A drew no edge for hop B to measure), mirroring the
    service-graph short-circuit. A scope exceeding
    `--netapp-qos-scope-batch-bytes` (default 8192) is chunked deterministically
    (`promql.ChunkQoSVolumeScope`); a single over-budget name still gets its own
    query rather than being dropped. Chunks are issued concurrently under
    `scopeConcurrency` and merged **in chunk-index order, never completion
    order** — `sumQoSIO` adds float64s, so a timing-dependent merge would make
    the last bits of every I/O figure depend on which chunk answered first.
    On `/v1/graph` each chunk degrades on its own (log-and-continue), so a failed
    chunk costs I/O measurements only for the claims whose volumes it carried and
    never an edge, aggregate, controller or `svm`; on `/v1/storage-graph` a failed
    chunk fails the build. The `volume` alternation is derived
    from UPSTREAM DATA, not the request, so `queryDims` is unchanged and the
    Harvest family still renders no `az` / `env` / `cluster` / `namespace`
    matcher — but the claims that produced it were loaded under the request's
    selectors, which is "narrowed by reference" reaching the query layer.
  - **hop C `qos_policy_fixed_max_throughput_{iops,mbps}`** — the declared
    ceiling, joined on the `(ontap_cluster, svm, policy_group)` triple assembled
    from BOTH topology hops: **hop A** owns the ONTAP cluster of the picked
    aggregate (a FlexGroup claim, which resolved no aggregate, takes the ONTAP
    cluster the SVM pick itself landed on) and the SVM the `volume_labels` match
    resolved, **hop B** owns the
    `policy_group` (`volume_labels` carries no policy identity, so hop B is the
    only upstream statement of which policy governs this FlexVol). Anchoring the
    first two on hop A is what lets a workload series carrying a `policy_group`
    but NO `svm` still resolve a ceiling, and keeps the key on the filer the
    edge points at. The policy's identity label is read as `name` with a
    `policy_group` fallback (Harvest spells it differently across templates).
    The pick reads candidates at BOTH granularities and **prefers a
    `policy_group` the fixed-policy index actually holds**, falling back to the
    lexically-smallest non-empty value — data-driven rather than a hardcoded
    list of built-in class names, and load-bearing because `User-Best_effort`
    sorts BEFORE a name like `gold-tier`. An incomplete or unmatched key is
    **ignored, never widened** — no hop-A
    `svm`, no non-empty `policy_group`, or no matching series leaves both fields
    absent rather than borrowing another policy group's figure from the same
    SVM. `max_bytes_per_sec` is the **one** value not read verbatim:
    `mbps × 1048576` (`bytesPerMB`), so the ceiling shares the unit of
    `read_bytes_per_sec`.
  The hop split is load-bearing: a hop-B miss leaves a valid **measurement-less
  edge**, it never costs the claim its topology. On the EDGE a ceiling can NEVER
  appear without a measurement — structurally, because the attachment sits inside
  the `io != nil` branch (`applyCeiling`), with `metricsDTO` deliberately not
  letting a ceiling set `filled`. The **PVC node** carries the same ceiling as
  `data.qos` whenever one resolved, measured or not and aggregate or not
  (FlexGroup included): `resolveCeiling` runs ONCE per claim, before the
  aggregate gate, into `netappResult.qosByPVC` (stamped onto `PVCNode.QoSValue`
  beside the `svm` / `aggr` labels), and the edge copies its two figures from
  that value, so node and edge cannot disagree and never share a float cell. The
  node is decoupled from the measurement rule but NOT from hop B: the policy
  group is still recovered only FROM a matched workload series (LUN rows
  included), so a claim with no in-scope workload series has no `qos` in either
  place. A volume in no policy group carries no ceiling.
  Both `volumename` and `svm` are **plain labels**, set only when non-empty;
  `svm` is impossible without `volumename` and comes from hop A ONLY (hop B's
  own `svm` only SCOPES a workload candidate to the claim's volume in
  `qosInScope` — it is neither a join key nor a fallback since D9 re-keyed the
  ceiling's `svm` component onto hop A). The hop-A pick runs `pickAggr` FIRST and scopes
  `pickSVM` to the ONTAP cluster it landed on, so a cross-filer FlexVol-name
  collision cannot pair one filer's aggregate with another's SVM — which would
  reject every in-scope workload and key the ceiling on a foreign tenant (D10);
  with no aggregate resolved (FlexGroup) the pick is unscoped and the claim
  still gains its `svm`. **`volumename` ≠ `volume`**.
  A third plain label, **`aggr`**, is copied verbatim from the `target` of the
  same claim's `pvc-to-netapp-aggr` edge — never composed or re-derived — so it
  is an **opaque node id** (`netapp/<ontap_cluster>/aggr/<aggr>`), matched
  against a `netapp-aggr` node's `data.id` and never parsed for its aggregate
  name or ONTAP cluster. It is absent, never an empty string, whenever the
  claim resolved no aggregate (a FlexGroup volume, a join miss, a claim with no
  `volumename`, or a window without Harvest), and it is independent of `svm`:
  an empty-`svm` series still yields `aggr`, and a FlexGroup series yields
  `svm` with no `aggr`. Both `GET /v1/graph` and `GET /v1/storage-graph` carry
  it, stamped once in the shared topology beside `svm` — on `/v1/graph` it
  restates the `pvc-to-netapp-aggr` edge already in the body.
  All 20 Harvest/kubelet legs plus `ALERTS` are OPTIONAL (log-and-continue) on `/v1/graph`.
  **`/v1/storage-graph` fails closed** (fail-storage-graph-on-any-leg-error): `storagePlan.failClosed`
  makes every first-wave leg and every scoped QoS chunk required except `ALERTS`, and every wave
  only a by-reference plan issues (pods, nodes, controllers, application recovery, the
  seed's `volume_labels` incl. owner completion, and the expansion's claim-keyed reads) is required unconditionally — `scopedFamily` has no error class any more.
  A failed query is wrapped in `build.QueryError`; `build.Error.Query` carries the bare family
  name and `mapBuildError` writes `upstream query failed: <family>` (never upstream text).
  An empty vector still never fails a build. **Two** coverage
  warnings, each gated on its OWN family having been read:
  `slog.Warn("netapp_volume_join_miss", "count", n)` (hop-A miss or
  empty-`aggr`; under a restricted volume-label read counted only over claims
  that matched a series, so a FlexGroup still reports and a claim off the rooted
  components does not) and
  `slog.Warn("netapp_qos_join_miss", "count", n)` (edge drawn,
  no QoS match — under the scoped read its gate means "at least one issued chunk
  returned series", so a build that issued none is silent). No signal for a
  missing ceiling — an SVM with no fixed-policy series is normal. Tests: `pkg/build/volumekey_test.go`, `pkg/build/qosscope_test.go`,
  `pkg/build/netapp_test.go` (incl. the fan-out pin: 37 legs with no matched
  volume, 43 with one), `pkg/build/build_storage_plan_test.go` (the storage
  plan's parity pin and its by-reference fan-out pin: 18 legs with nothing
  named, growing per named pod/node/owner up to 38 with every controller kind
  present and a matched volume; an `application=` root adds 6 / 6+1+2 / 6+2+6
  as in docs/upstream-metrics.md; per-kind seed and expansion counts are pinned by
  `TestBuildStorage_FanOutLegCount` and `TestBuildStorage_FanOutLegCount_Hub`, whose
  `pvc` / `pv` rows pin 2 / 7 / 25 / 32 queries), `pkg/build/claimseed_test.go`,
  `pkg/build/podscope_test.go`,
  `pkg/build/appscope_test.go`, `pkg/build/nodescope_test.go`,
  `pkg/build/controllerscope_test.go`, `pkg/build/scopedread_test.go`,
  `pkg/promql/scope_test.go`, `pkg/promql/appscope_test.go`,
  `pkg/promql/qosscope_test.go`,
  `pkg/promql/queries_test.go` (`TestRender_QoSVolumeGranularity` pins the
  ABSENCE of any `lun` matcher),
  `internal/api/testdata/golden/with-netapp-storage-cytoscape.json`,
  `internal/integration` (`TestPVCNetAppHarvestJoin`).
