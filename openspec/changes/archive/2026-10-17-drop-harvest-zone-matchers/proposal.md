# Proposal

## Why

Every NetApp Harvest query carries the request's `az` and `env` matchers
(`read-storage-roots-through-volume-hub` D13, `dimsHarvest = dimAZ | dimEnv`).
That makes the Harvest side depend on the same zone labelling as
kube-state-metrics: a Harvest series that does not carry the configured `az` /
`env` labels, or carries a different pair than the claims it serves, matches
nothing under any filter — and `/v1/storage-graph` is *always* filtered — so its
filer silently drops out of every body.

The matcher is the wrong tool for a store that is already the zone boundary. A
filer is not owned by a Kubernetes zone: the same FlexVol (and the PVC bound to
it) can be visible under more than one `(az, env)` pair, and the operator
already expresses "which Harvest store answers which zone" through backend
`zones`. Stop filtering Harvest by `az` / `env` at the query, and treat a series
that exists under several `(az, env)` pairs as one thing: take one.

## What Changes

1. **Harvest queries render no `az` / `env` matcher.** `dimsHarvest` becomes a
   routing-only dimension again (the `dimAZRoute` bit D13 removed): the request's
   `az` still selects which `harvest` backends are asked (zone-declared backends
   plus catch-alls), but the query string issued to them is the bare, unfiltered
   one — on both `/v1/graph` and `/v1/storage-graph`, for the unrestricted legs
   and for every restricted `volume_labels` / `qos_*` / `aggr_*` / `node_*` read.
   `cluster` and `namespace` still never reach Harvest, and `env` no longer does
   either. `Family.AcceptsAZ()` stays true for `harvest`; `Family.RendersAZ()`
   becomes false for it.
2. **A claim's `volume_labels` candidates PREFER its own zone instead of being
   confined to it.** Zone agreement (`accept-multi-zone-storage-graph`) excluded a
   series whose complete `(az, env)` pair differs from the claim's. With the
   matcher gone that exclusion would re-impose the filter in Go and drop a claim
   whose filer is labelled with another pair. It becomes prefer-then-fall-back: a
   claim's candidates are the agreeing series when at least one exists, and every
   matched series otherwise. The picks over the candidates (`pickAggr`,
   `pickSVM`, the owner vote) are unchanged.
3. **QoS workload series that differ only by zone are read once.** Without the
   matcher the same workload can come back under several `(az, env)` pairs, and
   `sumQoSIO` adds every in-scope candidate — it would double the claim's I/O.
   A claim's QoS candidates (measurement AND policy-group pick) keep, for each
   workload (`ontap_cluster`, `svm`, `volume`, `lun`, family), the rows of ONE
   zone before they are summed: the claim's own zone, otherwise the rows that
   carry no pair, otherwise the lexically-smallest complete pair. Distinct
   workloads of one claim are all kept.
4. **Every Harvest series no longer MUST carry the configured `az` / `env`
   labels.** The D13 operator precondition and its BREAKING entry are withdrawn.
   A series that carries them still takes part in zone preference and in the
   zone-agreeing alert match.
5. **Docs and rules follow**: `CLAUDE.md`, `.claude/rules/{netapp-storage-join,
   upstream-queries}.md`, `docs/{BREAKING,upstream-metrics,
   netapp-harvest-preconditions,upstream-backend-routing}.md`, the READMEs and the
   `/v1/storage-graph` OpenAPI description.

**BREAKING** (relaxation, behavioural): a catch-all or multi-environment
`harvest` store now answers every Harvest query with ALL of its rows — a filer
labelled with another `(az, env)` can join a claim of the requested zone when no
series of the claim's own zone exists. Operators who relied on D13 to keep a
shared store from answering with another environment's filers must give that
environment its own `harvest` backend with `zones`.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `netapp-storage-graph`: **Harvest legs carry the request zone and
  environment** is replaced by a requirement that Harvest legs are zone-routed
  and carry NO request matcher; **PVC-to-NetApp-aggregate edge join** — the zone
  agreement rule becomes prefer-then-fall-back; **Harvest QoS workload series as
  the I/O source** — series differing only by zone collapse to one zone per
  workload.
- `cluster-topology-source`: **Request-scoped upstream selectors** and **Backend
  routing composes with request-scoped selectors** — `az` / `env` no longer reach
  Harvest as matchers (`az` still routes); the topology-series table row for
  Harvest follows.
- `storage-graph-api`: **Storage-flow graph endpoint** (and the scenarios that
  assert a Harvest `az` / `env` matcher) — Harvest queries are routed by the
  request's `az` set and carry no matcher; a storage body's Harvest side is no
  longer confined to the requested zone's rows.

`graph-api`, `upstream-backend-routing` and `metrics-label-query` already state
"Harvest is zone-routed and carries no `az` matcher" (they were not updated by
D13); this change makes them true again and needs no delta there.

## Impact

- `pkg/promql/selector.go`, `pkg/promql/queries.go` — `dimAZRoute` restored,
  `dimsHarvest = dimAZRoute`, `familyRendersAZ` / `familyAcceptsAZ` derivations
  and comments. The restricted Harvest renderers keep their `(keys, sel)`
  signatures (public API) and simply render no request matcher.
- `pkg/build/netapp.go`, `pkg/build/zone.go` — prefer-then-fall-back candidate
  narrowing; zone-tagged `qosCandidate` and the one-zone reduction before
  `pickPolicy` / `sumQoSIO`.
- `pkg/promql/testdata/render-baseline.txt` is unchanged (it renders the empty
  selector); tests that pin a Harvest `az` / `env` matcher flip to pin its
  absence (`pkg/promql/{selector,family,labelquery,scope,qosscope,volumelabels}_test.go`,
  `pkg/build/*zone*_test.go`, `internal/integration/*`).
- Docs: `CLAUDE.md`, `.claude/rules/*`, `docs/*`, READMEs, OpenAPI.
- No new request parameter, flag, node / edge type, self-metric or dependency.
  Response body shape and `data.*` attributes are unchanged; the body for an
  estate whose Harvest series are zone-consistent is unchanged.
