## Context

See `proposal.md` — Why. The constraints that shape the approach:

- `ReadTopology` is one flat errgroup of 37 queries. The 13 Harvest legs are all
  `fetchOptional` (log-and-continue); the KSM legs are `fetch` (abort). All are
  issued together, and `parseTopology` — a pure function of the collected
  vectors — runs after the group completes.
- `resolveNetAppStorage` reads `volume_name` at exactly two sites: the
  `volume_labels` index (`pkg/build/netapp.go:89`) and `indexQoSFamily`
  (`:259`). Hop C keyed on the `(cluster, svm, policy_group)` triple recovered
  from hop B and never saw the volume key; D9 re-keys it onto hop A's
  `(cluster, svm)`.
- `promql.Render(q, window, keys, sel)` is a pure function; the empty-selector
  form of every query is pinned byte-for-byte by
  `pkg/promql/testdata/render-baseline.txt`. The Harvest family renders no
  request matcher at all (`dimsHarvest = dimAZRoute`).
- ONTAP volume names admit only letters, digits and `_`. A PV name is
  `pvc-<uuid>`. So the stock `volume` label can never equal a PV name, and the
  join cannot become a plain equality on `volume`.
- The `claims` list `resolveNetAppStorage` consumes is built from PVC *entities*,
  which exist only for claims a pod binds. The raw `kube_persistentvolumeclaim_info`
  vector carries `volumename` for every claim in scope, bound or not.

## Goals / Non-Goals

**Goals:**

- Make the storage chain resolve against stock Harvest output, with the
  PV-name → FlexVol-name mapping expressed as operator-configurable regex.
- Stop fetching QoS workload series the reader provably discards.
- Keep the response body byte-identical for any estate that resolves the same
  set of claims, and keep the build deterministic under concurrency.
- Keep `promql.Render` pure and its rendered baseline untouched.

**Non-Goals:**

- Narrowing `volume_labels`, the aggregate/controller health families or the
  hop-C policy families. Their cardinality scales with volume, aggregate and
  policy-group counts, not workload counts.
- Any fallback to the old `volume_name` label. The proposal removes it outright.
- Teaching the backend Trident's `storagePrefix`. The match modes below make the
  prefix unnecessary to know.
- Changing backend routing, the `queryFamily` table, or the `{lun=""}` contract.

## Decisions

### D1 — Rewrite the PV name, not the FlexVol name

The token derivation runs once per claim (`pvc-a-b` → `pvc_a_b`), not once per
Harvest series. One PV name therefore yields exactly one token, and no
collision rule over rewritten values is required. Rewriting the other direction
would need a rule that recovers a PV name from an arbitrary FlexVol name — which
forces an assumption about the FlexVol name's shape (the `pvc-<uuid>` five-group
regex) and admits many-to-one collisions that would need their own tie-break.

*Alternative considered:* rewrite `volume` into a PV name and keep the existing
hash-map equality join. Rejected for the shape assumption and the new collision
rule; the fast paths in D3 recover the map lookup anyway.

### D2 — Ordered regex rewrite rules, defaulting to `-` → `_`

Configuration is an ordered list of `<pattern>=<replacement>` rules applied with
`regexp.ReplaceAllString` in declaration order. Default: the single rule
`-` → `_`.

The default deliberately does not prepend `trident_`. `storagePrefix` is
per-backend configurable in Trident, can differ across backends in one cluster,
and D3's default match mode does not need it. A default that baked it in would
be wrong for any estate that changed it and would put a provisioner name in the
backend's default configuration.

`<pattern>=<replacement>` splits on the **first** `=`; a pattern needing a
literal `=` writes `\x3d`. Env form is semicolon-separated. Each pattern is
compiled at startup and a compile failure is fatal — the same class as
`--az-label` failing its label-name check.

*Alternative considered:* a fixed `strings.ReplaceAll(pv, "-", "_")` plus a
`storagePrefix` string. Rejected — it cannot express any other naming scheme,
and the user requirement is explicitly for regex flexibility.

### D3 — Four match modes; `suffix` is the default and the reason the prefix is unknowable

The token is compared against each `volume` value under one of:

| Mode | Semantics | Cost |
|---|---|---|
| `exact` | `volume == token` | hash map, O(M) |
| `suffix` | `strings.HasSuffix(volume, token)` | length-bucketed map, O(M) |
| `contains` | `strings.Contains(volume, token)` | scan, O(N·M) |
| `regex` | token compiled as a Go regexp, matched against `volume` | scan, O(N·M) |

`suffix` is the default because Trident's ONTAP drivers compose the name as
`<storagePrefix><pv-underscored>` with nothing appended — so a suffix match is
exactly right without knowing the prefix, while still rejecting a derived volume
whose name extends past the PV name (`trident_pvc_x_clone`), which `contains`
would wrongly accept.

The `suffix` fast path buckets claim tokens by byte length: `map[int]map[string]…`.
For each Harvest series, for each distinct token length `L`, the last `L` bytes
of `volume` are looked up. The number of distinct lengths is one in practice
(every `pvc-<uuid>` is the same length), so suffix matching costs one hash
lookup per series, not a scan. `contains` and `regex` have no such reduction and
are documented as scans.

### D4 — The QoS scope is computed from raw vectors, so `parseTopology` is untouched

Wave 2 needs only the set of FlexVol names worth asking about. That is a pure
function of two raw vectors and the rewrite configuration:

```
qosVolumeScope(pvcInfo, volumeLabels, rewriter) []string   // sorted, deduped
```

It reads `volumename` straight off `kube_persistentvolumeclaim_info` rather than
off resolved PVC entities. That is a **superset** of the entity claim list — an
unbound PVC contributes a name that no claim will later join — which is safe:
the scope only decides what is fetched, never what joins. The authoritative
join stays entirely inside `resolveNetAppStorage`, using the same rewriter and
match mode. `parseTopology` keeps its current signature and behaviour.

*Alternative considered:* split `parseTopology` so PVC entities resolve before
wave 2. Rejected — it would fracture a pure function that currently resolves
cluster identity, owners, applications and bindings in one pass, to gain a
marginally smaller name set.

### D5 — A dependency edge, not a barrier between waves

Wave 2 does not wait for all of wave 1. It waits on exactly the two legs its
scope function reads: `kube_persistentvolumeclaim_info` (required) and
`volume_labels` (optional). Those two run as ordinary wave-1 goroutines that
publish their vector and close a channel; one launcher goroutine — still inside
the single top-level errgroup, so fail-fast is preserved — waits on both,
computes the scope, and issues the batched QoS reads.

A global barrier would serialise the entire build behind its slowest KSM leg for
no reason. The dependency edge costs the Harvest tail only what it actually
depends on.

If `volume_labels` degraded (its vector is empty), the scope is empty. If the
scope is empty for any reason, **no `qos_*` query is issued at all** — hop A
matched nothing, so hop B could not have contributed. This mirrors the existing
rule that a selector loading no pods or services issues no
`traces_service_graph_*` queries.

### D6 — Batching by rendered-length budget, merged in chunk order

The alternation grows with the claim count and VictoriaMetrics caps query length
(`-search.maxQueryLen`). The sorted, de-duplicated name set is chunked greedily:
names are appended to the current chunk until adding the next would exceed
`--netapp-qos-scope-batch-bytes` (default 8192, comfortably under the common
16 KiB server default). A single name longer than the budget still gets its own
query rather than being dropped — a silently dropped claim is the failure class
this whole change exists to remove; an over-length query that upstream rejects
degrades that one chunk, visibly.

Chunks are issued concurrently, bounded by a compile-time
`qosScopeConcurrency` constant following the `routeResolveConcurrency`
precedent — a site-invariant tuning value, so no knob.

**Merge order is by chunk index, not completion order.** Each chunk writes into
a pre-sized slot and the family's vector is concatenated in index order.
`sumQoSIO` adds float64 candidates, so a completion-ordered merge would make the
summation order — and therefore the last bits of `read_ops` and friends —
depend on upstream timing. Chunk-index order makes the merged vector a pure
function of the scope, which is itself sorted.

### D7 — A separate rendering entry point; `Render` is not widened

`promql.Render` stays a pure function of `(query, window, keys, selector)` and
its baseline file does not move. Wave 2 uses a new, narrowly-named entry point
that composes the query's existing fixed selector with an anchored `volume`
alternation:

```
qos_read_ops{lun="",volume=~"trident_pvc_a|trident_pvc_b"}
```

Values are `regexp.QuoteMeta`-escaped and the set is sorted and de-duplicated by
the caller, reusing the same alternation renderer the `Selector` path already
uses. PromQL `=~` is fully anchored, so this is exact-match semantics — the
fuzzy matching of D3 happens once, in Go, during scope computation, and never
reaches the query layer.

`queryDims` and `queryFamily` entries for the six queries are unchanged: they
stay in the `harvest` family with `dimAZRoute`, so routing, the zone boundary
and the merge-dedup rule all behave exactly as today. The scope matcher is
derived from upstream data, not from the request, so it is not a request filter
and does not disturb the `queryDims` contract.

### D8 — Degradation moves from per-family to per-chunk

Each chunk is `fetchOptional`. A failed chunk costs I/O measurements for the
claims in that chunk only, and deterministic chunking makes which claims those
are a pure function of the scope. The two coverage warnings keep their names,
counts and per-family gating; `qosPresent` becomes "at least one chunk of at
least one QoS family returned series", preserving its meaning.

Hop A and hop B stop being independently reachable — a hop-A miss means hop B is
never asked. The reader already behaved this way (it consults the QoS index only
for claims that resolved an aggregate), so no observable behaviour changes, but
the spec wording "three hops degrade independently" is restated to describe the
read as well as the parse.

### D9 — The ceiling triple is anchored on hop A for cluster and SVM; hop B supplies only the policy group

The `(ontap-cluster, svm, policy-group)` key stays a triple, but stops being
recovered wholesale from the workload series. Its cluster and SVM components
come from **hop A** — the picked aggregate's ONTAP cluster and the SVM the
`volume_labels` match resolved — and hop B is read for the `policy_group` label
alone.

Hop B has to stay in the key because it is the ONLY upstream statement of which
policy group governs a given FlexVol: `volume_labels` carries no policy identity
(confirmed against the Harvest volume template and the demo faker's rendering),
so the mapping exists nowhere else. Hop A has to own the other two because they
are properties of the volume the edge points at, not of whichever workload
series happened to match — which is what makes the following two cases resolve:

- A workload series carrying a `policy_group` but **no `svm` label** now
  contributes its policy. `qosInScope` already keeps such a candidate for
  measurement; keying the SVM off hop A lets the same candidate reach hop C.
- Under a cross-filer FlexVol-name collision the key follows the picked filer
  (D10), so it can never name another filer's SVM.

**An incomplete or unmatched key is ignored, never widened.** No `svm` from hop
A, no non-empty `policy_group` among the in-scope candidates, or a triple that
matches no fixed-policy series all leave both ceiling fields absent. There is
deliberately no SVM-wide or cluster-wide fallback: an SVM commonly holds several
policy groups (one per storage class is the ordinary Trident shape), so a
figure resolved from any other group would name a limit this volume does not
have. Reporting a `bronze` ceiling against a `gold` volume's measured
throughput is worse than reporting no ceiling — a consumer computing a
saturation ratio would read a healthy volume as many times over its limit,
while an absent field is already specified as "no declared ceiling" and renders
as nothing at all.

The attachment invariant is structural again, not merely positional: the policy
group rides on a matched workload series, so a ceiling cannot exist without a
measurement even before `applyCeiling`'s placement inside the `io != nil`
branch is considered.

*Alternatives considered:* key on `(ontap-cluster, svm)` alone with a per-figure
minimum across the SVM's policy groups — it makes a ceiling reachable for a
volume in no policy group, but at the cost of misattributing every volume in a
multi-policy SVM, which is the common shape. Fall back to that minimum only
when `policy_group` is empty — it keeps the precise value where one exists, but
gives `data.metrics` two incomparable meanings with nothing on the wire to
distinguish them; a consumer cannot tell a volume's own ceiling from its SVM's
most restrictive one.

### D10 — The SVM pick is scoped to the picked aggregate's ONTAP cluster

`pickSVM` ran over every matched `volume_labels` candidate and ran *before*
`pickAggr`. D9 makes that unsafe: the resolved `svm` is now paired with the
picked aggregate's ONTAP cluster twice — `qosInScope` rejects a workload whose
`svm` differs, and `applyCeiling` keys on the `(ontap-cluster, svm)` pair.

On a cross-filer token collision (blind spot 5: same-named FlexVols on two
filers sharing one VictoriaMetrics) the two picks could land on different
filers — `pickAggr` takes the lexically-smallest `(cluster, aggr)` pair while
`pickSVM` took the lexically-smallest `svm` from anywhere — so the edge lost
every in-scope workload, and, whenever the picked filer happened to host a
same-named SVM, the ceiling of an unrelated tenant was attached.

`pickAggr` therefore runs first and `pickSVM(cands, oc)` considers only
candidates on `oc`. The order also makes the file's two candidate picks
consistent: `pickNode` already scoped itself to `(oc, aggr)`.

Scoping to `oc` alone, not to `(oc, aggr)`: an SVM is a property of the volume,
not of the aggregate it sits on, so a volume observed on two aggregates of one
filer inside the window must still resolve it.

An empty `oc` — the FlexGroup shape, or no candidate carrying both a `cluster`
and an `aggr` — means there is neither an edge nor a ceiling, so there is no
filer to scope to and the pick stays unscoped. That preserves the existing
FlexGroup behaviour: a claim that draws no aggregate edge still gains its `svm`
label.

*Alternative considered:* scope the ceiling key instead, leaving `pickSVM`
unscoped. Rejected — it fixes hop C and leaves the (pre-existing) hop-B I/O
loss in place, and it would emit a PVC `svm` label naming a filer the claim's
own edge does not point at.

## Risks / Trade-offs

- **A deployment whose FlexVol names encode no PV name at all loses its storage
  chain.** The relabel rule could express an arbitrary hand-maintained mapping;
  regex rewrite rules cannot. → Documented as BREAKING with the migration path
  (express it in rules, or accept the miss); `netapp_volume_join_miss` reports
  the count, so the loss is visible rather than silent.
- **`contains` and `regex` modes are O(claims × volumes) scans.** → Both are
  opt-in; the default `suffix` mode and `exact` are O(volumes) through the
  bucketed index. Documented in the operator reference.
- **The extra dependency edge adds latency to the Harvest tail** when
  `kube_persistentvolumeclaim_info` is slow. → Wave 2's queries are far cheaper
  than the unfiltered reads they replace; the edge waits on two legs, not on the
  whole of wave 1.
- **Chunking multiplies query count** (6 families × N chunks). → Bounded
  concurrency, and each query is far narrower than the unfiltered read it
  replaces. Self-metrics keep one `query` label value per family, so the
  duration histogram now records several observations per build — a change in
  observation count, not in metric or label shape.
- **A `contains`-mode estate can pull a clone or snapshot volume into the
  scope**, inflating a claim's measured I/O. → Default is `suffix`, which
  rejects the suffixed-clone shape; the multi-candidate collapse rule
  (lexically-smallest `(ontap_cluster, aggr)`) is unchanged and deterministic.
- **The scope is a superset of the entity claim list** (unbound PVCs contribute
  names). → Harmless: it widens what is fetched, never what joins.
- **D10 narrows which series can supply a claim's `svm`.** A claim whose
  aggregate resolves on one filer while only the OTHER filer's series carries a
  non-empty `svm` now emits no `svm` label at all. → Correct: that value named a
  filer the claim's own edge does not point at, and emitting it cost the edge
  its I/O. `netapp_qos_join_miss` already covers the resulting measurement gap.
- **A volume in no QoS policy group still gets no ceiling.** D9 does not widen
  the key's reachability; it only re-anchors two of its three components. →
  Accepted: that is the pre-existing behaviour, no signal is emitted for it (a
  volume in no policy group is normal, not a defect), and the alternative —
  borrowing another policy group's figure — is a wrong value rather than a
  missing one.

## Migration Plan

1. Deploy with defaults (`-=_`, `suffix`). No configuration change is required
   for a stock Trident + stock Harvest estate.
2. Read `netapp_volume_join_miss` from the build logs. Zero means the derivation
   covers the estate.
3. If non-zero, adjust `--netapp-volume-key-rewrite` / `--netapp-volume-match-mode`
   against the actual FlexVol names (`count by (volume) (volume_labels)`).
4. Once step 2 reports zero, the Prometheus `volume_name` relabel rule may be
   deleted. Leaving it installed is harmless — the label is simply not read.

**Rollback:** revert the binary. The old build reads `volume_name`, so a
deployment that already deleted its relabel rule must restore it before rolling
back. Recorded in `docs/BREAKING.md`.

## Open Questions

- The default value of `--netapp-qos-scope-batch-bytes` assumes the common
  VictoriaMetrics `-search.maxQueryLen` default. Confirming the target
  deployment's configured value may move the default, but it changes no
  requirement, interface or task.
