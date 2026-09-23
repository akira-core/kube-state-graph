## Context

See proposal.md for motivation. Sequenced AFTER `fail-storage-graph-on-any-leg-error`: every error class below is that change's fail-closed class. Current shape of a storage-rooted
`/v1/storage-graph` read (after `scope-volume-labels-by-storage-root`,
`scope-controller-legs-by-reference`, `add-storage-graph-application-root`):

```
 wave 1 (az/env matchers, az-routed)                   later waves
 ────────────────────────────────────────────          ─────────────────────────────────────────
 kube_persistentvolumeclaim_info      (whole zone) ─┐
 volume_labels phase 1 {cluster,aggr} ──────────────┼─▶ phase 2 {volume=~".*tok"} ─▶ QoS ×6
 kube_pod_spec_volumes_pvc_info       (whole zone) ─┴─▶ pods (EVERY mounting pod)
 kube_persistentvolumeclaim_annotations (whole zone)        ├─▶ kube_node_* ×4
 kubelet_volume_stats_* ×2           (whole zone)           └─▶ controllers stage 1 ─▶ stage 2
 aggr_* / node_* / qos_policy_* / ALERTS
```

Constraints that shape the approach:

- `volume_labels` rows carry `cluster, node, aggr, svm, volume` together; the
  storage side of a path is one row, not a chain.
- The join is derive-then-match in the FORWARD direction (PV name → token →
  suffix/exact match on `volume`), operator-configurable, and it is the only
  judge of which aggregate / SVM a claim lands on (`pickAggr`, `pickSVM`,
  `pickOwner` are lexically-smallest over a claim's WHOLE candidate set).
- Kubernetes dynamic provisioning names a PV `pvc-<claim UID>`; a UID is unique
  across clusters. ONTAP volume names admit only letters, digits and `_`; PV
  names are DNS-1123 and contain no `_`.
- The claim-binding family is keyed by `persistentvolumeclaim`, not by PV name;
  only `kube_persistentvolumeclaim_info` carries `volumename`.
- `Builder` resolves ONE routing snapshot per build (routing D2); `az` selects
  backends for `ksm`, `kubelet`, `alerts` AND `harvest`.

## Goals / Non-Goals

**Goals:**

- A storage-exclusive-rooted request reads the Kubernetes side proportional to
  the claims on the rooted components, not to the zone.
- Claims on a rooted filer are found in every zone and environment.
- `svm=` gets the same restricted Harvest read `aggr=` has, without changing the
  owner vote.
- Every pick (aggregate, SVM, owner, QoS, ceiling) stays the forward join's.

**Non-Goals:**

- A `volume=` root (deferred; the hub makes it cheap to add later).
- Changing the cross-filer / DR same-name pick rule.
- Relaxing `az` / `env` outside hub mode, or on `/v1/graph`.
- Filtering root volumes out of `volume_labels` (considered, rejected: the
  extraction already ignores them, and a matcher would remove empty SVMs and
  `aggr0` from the inventory).
- Supporting exporters that label the claim-binding family with `claim_name`
  only, in hub mode (outside the documented label contract).
- A node-only (`node=` without a storage-exclusive root) hub path.

## Decisions

### D1 — Hub mode trigger

Hub mode is on iff ALL hold:

1. the plan is by-reference (`/v1/storage-graph` only);
2. the request carries at least one storage-exclusive root: `ontap_cluster=`,
   `aggr=` or `svm=`;
3. the configured match mode renders as an alternation branch (`exact` /
   `suffix`) — phase 2 must be able to complete every claim's candidate set;
4. the phase-1 restriction is bounded (≤ `maxRootedVolumeLabelChunks` queries
   across all phase-1 groups, see D2) and renderable.

When any fails the build is exactly today's (unrestricted Harvest topology read,
first-wave claim families under `az` / `env`).

The decision is taken ONCE, before the fan-out launches
(`topologyPlan.resolveVolumeLabelRead`), not when phase 1 returns: condition 4
is a pure function of the roots, the match mode and the byte budget, and the
decision also picks the request matchers (D7) and the routing snapshot (D8) of
every Kubernetes leg, which must be known before the first of them starts. A
build whose phase 1 would be unbounded therefore never withholds the claim
families at all — it issues them first-wave under the original selector, which
is exactly "today's build".

`node=` is no longer a disqualifier. The projection ANDs `node=` with the
storage-exclusive roots, so every retained unit already intersects a
storage-exclusive root and is reachable from the hub; a `node=` root naming an
ONTAP controller is materialised from the unrestricted `node_*` / `aggr_*`
families, and one naming a Kubernetes node still enters the node scope as today.

*Alternative rejected:* hub on any storage-side root including `node=` alone. A
node-only request retains a unit whose Kubernetes node is the root; finding it
needs pods before claims — the opposite direction — so it keeps today's read.

### D2 — Phase 1 is a union of query groups

| roots present | groups issued |
|---|---|
| `ontap_cluster` only | `{cluster=~OC}` (today) |
| `aggr` (± `ontap_cluster`) | `{cluster=~OC?, aggr=~A}` (today) |
| `svm` (± `ontap_cluster`) | `{cluster=~OC?, svm=~S}` (new) |
| `aggr` and `svm` | both groups, results unioned |

The projection UNIONS `aggr=` with `svm=` and narrows both by `ontap_cluster=`
(`inOC`), so the groups mirror it exactly. Each group chunks its larger
alternation as today; the chunk cap applies to the SUM over groups. Results merge
in (group, chunk) order and de-duplicate by label-set fingerprint (a volume on a
rooted aggregate AND a rooted SVM is returned by both groups).

### D3 — Owner completion for SVM groups

`pickOwner` votes over every `volume_labels` series of an aggregate. An SVM group
returns only that SVM's volumes on each aggregate it touches. After phase 1 the
build computes T = the `(cluster, aggr)` pairs named by SVM-group rows (non-empty
`aggr`), minus the aggregates the aggr group already read whole (an aggregate
named by an `aggr=` root inside the `ontap_cluster=` values). For T it issues
`volume_labels{cluster="<c>", aggr=~"<a1>|<a2>…"}`, one query per ONTAP cluster,
chunked, and merges the rows (fingerprint de-dup) before the parse.

Completion rows feed the owner vote and the inventory only. They are NOT fed to
PV extraction (D4): a claim on another SVM of a touched aggregate is on no rooted
component, so it cannot be retained.

Completion waits on phase 1 alone and runs in parallel with the claim chain; the
parse waits on it.

Phase-2-only aggregates need the same treatment under an `svm=` root.
`pickAggr` and `pickSVM` are separate picks, so a claim with a rooted-SVM
candidate on `aggr9` and a clone on another SVM's `aggr0` picks `aggr0` while
its SVM stays the rooted one: the unit is retained and draws `aggr0` with an
owner voted over the clone alone. After phase 2 returns, the build therefore
completes every aggregate a phase-2 row names that no earlier read covered. It
usually issues nothing (phase 2 mostly re-reads phase-1 series); `aggr=` and
`ontap_cluster=` roots never need it, because a unit they retain is on a
rooted aggregate or filer that phase 1 read whole.

*Alternatives rejected:* owner from the `aggr_*` gauge `node` (changes owner
semantics during HA takeover, moves goldens); skipping completion (vote drift on
exactly the takeover case the vote exists for).

### D4 — PV candidate extraction (generator, not judge)

For each phase-1 row's `volume` value `v`, for every index `i` where `v[i:]`
starts with `pvc_` and (`i == 0` or `v[i-1] == '_'`), emit
`strings.ReplaceAll(v[i:], "_", "-")`. Candidates are sorted and de-duplicated.

```
trident_pvc_ab12_cd34   → pvc-ab12-cd34
pvc_ab12_cd34           → pvc-ab12-cd34          (empty storagePrefix)
x_pvc_pool_pvc_ab12     → pvc-pool-pvc-ab12, pvc-ab12   (both; superset)
trident_pvc_ab12_clone  → pvc-ab12-clone          (names no PV → loads nothing)
vol0, svm1_root         → (none)
```

Why a lenient superset instead of a strict `pvc-<uuid>` pattern: an
over-generated candidate costs one alternation branch and loads nothing, because
`kube_persistentvolumeclaim_info{volumename=~…}` returns only PVs that exist; and
the forward join still decides whether a loaded claim matches `v` at all. A
strict UUID pattern would add nothing but break every fixture that uses short PV
names.

Why independent of `--netapp-volume-key-rewrite`: an arbitrary ordered list of
regex rewrites is not invertible. The configured FORWARD derivation stays the
judge, so a custom rule set can only make hub mode find fewer claims (see Risks),
never attach a wrong one.

### D5 — Claim-keyed families become by-reference in hub mode

| family | scope label | scope values | waits on | error class |
|---|---|---|---|---|
| `kube_persistentvolumeclaim_info` | `volumename` | D4 candidates | phase 1 | fails the build |
| `kube_pod_spec_volumes_persistentvolumeclaims_info` | `persistentvolumeclaim` | loaded claim names | pvc_info | fails the build |
| `kube_persistentvolumeclaim_annotations` | `persistentvolumeclaim` | loaded claim names | pvc_info | fails the build |
| `kubelet_volume_stats_used_bytes` / `_capacity_bytes` | `persistentvolumeclaim` | loaded claim names | pvc_info | fails the build |

Every claim-keyed read, every volume-label phase and owner completion fails the build on a query error, per `fail-storage-graph-on-any-leg-error` (this change is sequenced after it; `ALERTS` is the storage build's only optional leg). Each is composed
with the family's fixed selector (if any) and the relaxed request matchers (D7).

Claim names are unique per namespace only, so the four claim-name scopes may
return same-named claims from other namespaces or clusters. Rows of those four
families are filtered in the reader to the `(cluster, namespace, claim)` keys the
loaded `kube_persistentvolumeclaim_info` rows name, BEFORE the pod scope is
computed and before the parse — so a same-named claim neither loads pods nor
creates a PVC node. `cluster` here is the claim's cluster IDENTITY as raw labels
— the `(az, env, cluster)` triple, read under the configured label keys —
because a hub read spans zones and one raw cluster name reused in two zones is
two clusters; `podScopeUnderApp`, which only ever sees one zone, compares the
raw `cluster` alone. The bindings are keyed on `persistentvolumeclaim` only,
never on `claim_name`, so the filter admits exactly what the restriction can —
which is also what keeps the D6 fallback's body identical to the chunked one.

An empty scope issues nothing: no candidate ⇒ no claim query; no claim ⇒ no
binding / annotation / kubelet query, no pod, node or controller query. The body
then holds the rooted storage entities only — today's "empty pod scope" outcome.

### D6 — Bounded claim scopes fall back to unscoped-and-filtered

Candidate and claim scopes are data-derived and can be large
(`ontap_cluster=` on a 20,000-volume filer). When a family's scope would take
more than `maxHubClaimChunks` queries (initially `scopeConcurrency`), that
family is issued ONCE with no scope — fixed selector plus relaxed request
matchers — and its rows are filtered in the reader to the same scope. The body is
identical either way; one wide read of a one-series-per-claim family is cheaper
than dozens of chunk round-trips. The fallback is logged at Debug with the
family and scope size.

### D7 — Relaxed selector in hub mode

The build derives `hubSel = sel` with `AZ` and `Env` cleared, keeping `Cluster`
and `Namespace`, and uses it for EVERY kube-state-metrics, kubelet and `ALERTS`
query of the build — the claim-keyed families, the pod / node / controller waves,
the application recovery, and the first-wave families that stay first-wave.
Harvest queries render no request matcher in any build, so nothing changes there.

Cluster identity is unaffected: `clusterResolver` composes each series' identity
from that series' own `az` / `env` labels, so two zones' `c1` stay
`zone-a-prod-c1` and `zone-b-prod-c1`, and `clusters[]` lists both.

`selector_family_empty` and every `Selector.Reaches` check use `hubSel`.

### D8 — One routing snapshot, zone applied to `harvest` only

`b.src.QuerierFor(sel)` binds one snapshot with `az` for every family. Hub mode
needs `az` for `harvest` and no `az` for `ksm`, `kubelet`, `alerts`. Two
`QuerierFor` calls would load two snapshots and can straddle a reload, breaking
routing D2 ("the build cannot probe a different set of stores than it read
from").

New OPTIONAL upgrade in `pkg/promql`:

```go
type FamilyZoneQuerierSource interface {
    QuerierSource
    // QuerierForFamilyZones binds ONE routing snapshot. Families in zoned
    // dispatch by sel.AZ; every other family dispatches with no zone.
    QuerierForFamilyZones(sel Selector, zoned ...Family) Querier
}
```

`*Router` implements it by giving the bound `fanoutQuerier` a per-family zone
decision. `Builder.buildStorage` type-asserts for it in hub mode and falls back
to `QuerierFor(sel)` for a plain `QuerierSource` (zone routing everywhere —
matchers still relaxed, so cross-zone discovery is limited to the stores the
zone reaches) and to the plain `Querier` otherwise. Same pattern as
`QuerierSource`, `Prober`, `RouterMetrics`, `SeriesMetrics`.

### D9 — Wave wiring and critical path

```
 L1  volume_labels phase 1 (groups)      aggr_* node_* qos_policy_* ALERTS   app recovery st.1
 L2  kube_persistentvolumeclaim_info{volumename}   owner completion           app recovery st.2
 L3  bindings / pvc_annotations / kubelet ×2 {persistentvolumeclaim}   phase 2 {volume=~tok}   st.3
 L4  pods                                          QoS ×6
 L5  kube_node_* ×4      controllers stage 1
 L6                      controllers stage 2
```

Critical path 6 RTT (today's restricted build: 5; unrestricted: 4). The Harvest
tail (phase 1 → 2 → QoS) is unchanged. Phase 2 keeps its inputs: phase 1 plus
the (now scoped) claim-info family, from which the loaded claims' forward tokens
are derived exactly as today.

Done-channels: `pvcInfoDone` closes when the scoped claim-info read returns;
`bindingsDone` / `pvcAnnotationsDone` when their scoped reads return; every
channel closes on every return path so a failed leg empties the downstream
scopes instead of blocking (existing `signalWhenDone` rule).

### D10 — Hub coverage signal

Root volumes (`vol0`, `<svm>_root`) never yield a candidate, so a per-volume miss
count is always non-zero and useless. The signal fires on the two whole-hub
failures that are silent today:

- `slog.Warn("storage_root_claim_miss", "reason", "no_pv_candidate", "volumes", n)`
  — phase 1 returned rows with a `volume` and none produced a candidate
  (FlexVol naming does not embed `pvc_`, e.g. a Trident `nameTemplate`);
- `slog.Warn("storage_root_claim_miss", "reason", "no_claim", "candidates", m)`
  — candidates were produced and the claim-info read returned none (claims live in
  a store the build does not reach, or PVs were renamed).

Counts of volumes, candidates, claims and bindings are also logged at Debug on
every hub build.

### D11 — Alert matching agrees on zone

D7 drops `az` / `env` from `ALERTS`, so a hub build reads every zone's firing
alerts while its Harvest read stays in the requested zone (D8). The Kubernetes
kinds were already safe: a cluster-qualified alert resolves through
`clusterResolver.identify`, which composes the identity from the ALERT's own
`az` / `env`, so a zone-b alert cannot find a zone-a pod. The NetApp kinds were
not — `matchAggr` and the controller side of `matchNodeShaped` compare the raw
`cluster` label against `ontap_cluster` alone, so a zone-b alert about an
equally named filer's `aggr1` landed on the zone-a aggregate and moved its
`data.status`.

The fix uses a fact the build already reads and throws away: every Harvest
series this estate carries is stamped with `az` / `env` beside the ONTAP
`cluster`. The topology read collects, per ONTAP cluster, the set of
`(az, env)` pairs its entity-naming series (`volume_labels`, `aggr_*`, the
controller families) carry — only series carrying BOTH configured labels count,
mirroring the identity ladder's compose step. The alert index then carries a
zone per candidate: the zone set of its ONTAP cluster for controllers and
aggregates, the components of its composed identity for pods, claims and
Kubernetes nodes. `resolveOneAlert` reads the alert's pair with the same
configured `LabelKeys` and:

- rejects a cluster-qualified NetApp candidate whose zone set is known and lacks
  the pair (the Kubernetes candidates need no check there — see above);
- filters the no-`cluster` uniqueness candidates of every kind to those whose
  zone is unknown or agrees, BEFORE `matchUnique`, so a same-named object in
  another zone neither absorbs the alert nor makes it ambiguous. This also
  covers the Kubernetes regression the relaxed read introduced: a request-zone
  pod whose alert carries no `cluster` stayed unique in a single-zone build and
  turned ambiguous once hub mode loaded another zone's same-named pod.

Unknown never excludes: an alert with no pair, or a candidate whose series
carried none, falls back to the label comparison. That keeps the
"Harvest series need not carry `az` / `env`" precondition of
`netapp-storage-graph` intact — the pair SCOPES matching when present and is
never required. The zones are keyed by ONTAP cluster rather than per entity
because a filer lives in one zone; a set rather than a single pair keeps an
estate that reuses a filer name across zones (whose ids already merge today)
no worse than before — the alert matches iff one of the merged filers shares its
zone.

Alternatives rejected:

- **Filter NetApp matches on the REQUEST's `az` / `env` in hub mode.** Correct
  only while the Harvest backends are zone-declared; a catch-all Harvest backend
  returns every zone's filers and the filter would drop their alerts. It also
  leaves the no-`cluster` Kubernetes regression in place.
- **Compose a NetApp identity `<az>-<env>-<ontap_cluster>` into the node ids.**
  Solves cross-zone filer-name reuse too, but moves every NetApp id, the PVC
  `labels.aggr` value and every golden, for an estate where filer names do not
  repeat. Deferred until one does.
- **Add `labels.az` / `labels.env` to nodes.** A wire change the matcher does
  not need; the zone lives in the build-internal index only.

## Risks / Trade-offs

- [Static PVs are not found from a storage root] → Documented limitation in
  `docs/netapp-harvest-preconditions.md` and `docs/BREAKING.md`, with the PromQL
  to size it. `/v1/graph` and non-hub storage requests still join them.
- [Operator uses a custom `--volume-name-prefix` or Trident `nameTemplate`] →
  Extraction yields nothing; D10 `no_pv_candidate` names it. Same class as the
  existing forward-derivation blind spots.
- [Cross-zone bodies break the "one estate per body" statement] → BREAKING note;
  scoped to hub mode; `clusters[]` lists every identity drawn.
- [SVM spread over every aggregate makes owner completion read nearly the whole
  filer] → No worse than today, where `svm=` reads the whole filer unrestricted.
- [Large `ontap_cluster=` root inflates claim scopes] → D6 bound falls back to
  one unscoped-and-filtered read per family.
- [One extra sequential round-trip] → Accepted; the Kubernetes waves shrink from
  zone-wide to claim-proportional.
- [Relaxed `ALERTS` reads every zone's firing alerts] → Firing-only, small; alerts
  still attach only to loaded entities, and D11 keeps another zone's alert off a
  same-named NetApp entity or no-`cluster` Kubernetes object. The overlay now
  depends on every zone's alert store; `ALERTS` stays optional, so an outage
  there degrades the overlay rather than failing the build.
- [Hub mode's Kubernetes reads fan out to every zone's backend and fail closed]
  → A storage-rooted request now fails when ANY zone's kube-state-metrics or
  kubelet store is down, not only the requested zone's. Accepted: a partial
  fan-out would draw a plausible, smaller, wrong body — the routing D6 rule.
  The 502 names the failing family; the backend shows in
  `kube_state_graph_backend_query_failures_total{backend}`.
- [An embedder's `QuerierSource` lacks the upgrade] → Falls back to zone routing
  everywhere; body correct for the zone, cross-zone discovery lost.
- [Claim-binding exporter labels only `claim_name`] → Out of the documented
  contract; hub mode draws no path for it. Noted in the preconditions doc.

## Migration Plan

No flag. Deploy; storage-rooted requests switch to hub mode. Rollback is a
revert; there is no persisted state. Clients that assumed one zone per storage
body must read `clusters[]` / `labels.cluster` instead.
