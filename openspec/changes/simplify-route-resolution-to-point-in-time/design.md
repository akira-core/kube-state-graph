# Design — simplify-route-resolution-to-point-in-time

Simplifies `translate-global-fqdn-to-k8s-service` (parent; its decisions referenced as P-Dn) and
its two extensions `ingress-lb-service-fallback` (S-Dn) and `route-hit-ingress-chain` (C-Dn) from
a range query over `[start, end]` to a single as-of query at `end`. Settled decisions:

## D1 — The instant is `end`, always

The engine evaluates at the build window's **`end`**, never `start`, never a midpoint, and it is
NOT configurable. Reasons: (a) `end` is exactly the instant the service-graph sample is evaluated
at (`rate(...[window]) @ end`), so the routing answer and the traffic evidence describe the same
moment; (b) `end` is the newest information in the window, so a config change during the window
resolves to the configuration currently in force; (c) a knob would multiply the outcome space for
no consumer — the graph carries one edge per key regardless.

`RouteRequest.Start`/`End` therefore collapse to a single `At time.Time`. The seam lives at
exactly one line in `ReadServiceGraph`: the PromQL side keeps `(window, end)` and the engine side
receives `end`. `routeKey` is unaffected — it deliberately never carried the time (constant per
build), so key dedup and determinism are unchanged.

## D2 — As-of store reads keep the no-FINAL pattern verbatim

The predicate changes; the duplicate-row discipline does NOT. Every read stays three-staged:

1. SQL carries only IMMUTABLE predicates — join keys plus `valid_from <= at` (was
   `valid_from < t1`). `valid_to` is still NEVER filtered in SQL, because the exporter closes a
   version by REWRITING the open row: pruning on `valid_to` would drop the closing rewrite before
   dedup and let its stale sentinel twin win unopposed (a dead version would look live).
2. Client-side dedup per version slot `(cluster, namespace, name, valid_from)` keeping max
   `ingest_seq` — the ReplacingMergeTree collapse, applied at read time.
3. The liveness test on the materialized `valid_to` applied AFTER dedup — `valid_from <= at <
   valid_to` (was the overlap test `valid_from < t1 && t0 < valid_to`).

`WithUniqueRows` (update-close writers only) keeps its meaning: it restores the `valid_to`
predicate in SQL as `at < valid_to`. Stages 2 and 3 remain as the zero-cost safety net and
`CollapsedRows` remains the writer-uniqueness alarm. The exporter-owned schema and
`validateSchema` are untouched — this change reads strictly fewer rows from the same tables.

## D3 — Deleting `ConfigSigAt` is safe because the cache it fed is gone

`ConfigSigAt` existed for ONE purpose: letting several version segments that share byte-identical
config collapse to a single translate + `router_check_tool` run. With a single instant there is
exactly one config, one translate, one check — the cache, the `segmentResult` type and the
signature are all dead weight. Its superset-invariant test
(`TestConfigSigCoversScopedInputs`) guarded a correctness hazard (a signature that ignored a
`ScopedFor` input could return a stale cluster) that **cannot exist without the cache**, so it is
deleted with it rather than left as a test of unused code. The dedup-adjacent risk it protected
against does not reappear: `ScopedFor` is now called once per resolution.

`Segments`/`Segment`, `outcomeRank`, `noteMiss`, the `hit`/`hitDest` accumulators and the
"latest hit wins" rule go for the same reason — a single evaluation produces exactly one outcome.
`GatewaysLiveAt` is deleted as already-dead code (no caller).

## D4 — Package rename `memwindow` → `snapshot`

The type models "the resource versions live at one instant", not "a window". Keeping the old
names would leave every future reader believing range semantics still exist — the exact confusion
this change removes. Hence `memwindow.Window` → `snapshot.Snapshot`, `store.TrafficWindow` →
`store.TrafficSnapshot`, `Store.LoadTrafficWindow` → `LoadTrafficAt`. `Store` is a mockery-managed
interface, so `make mocks` regenerates `pkg/route/store/mocks/`; `build.RouteResolver` itself is
unchanged in shape (only `RouteRequest`'s fields change), so no build-side mock churn.

## D5 — Ingress-LB identity becomes as-of, which narrows "ambiguous"

S-D6/S-D7 evaluated ingress-Service identity **window-wide** so that an identity change inside
the window degraded to `ambiguous_ingress_service`. That rule was a consequence of range
evaluation, not a real ambiguity: at any single instant an IP has one owner. Under as-of
evaluation `ResolveIPToIngressServices` returns only the rows live at `at`, so:

- **ambiguous** now means genuinely conflicting evidence — more than one identity carrying the IP
  simultaneously, or several destination IPs whose (singleton) identities disagree;
- an identity that was replaced earlier in the window resolves cleanly to the current owner.

The tri-state precedence itself (same-IP collision > any-empty > disagreement), the
`identityIncomplete` → keep-the-pipeline-miss rule, and C-D1's "an ambiguous identity NEVER
demotes a hit" are all preserved verbatim. So is S-D4's coarse "which LB Service owns this IP"
semantics and its gate on the folded miss being `no_gateway` — with a single evaluation the
"folded" miss is simply the one outcome produced.

## D6 — Everything else is deliberately untouched

P-D5 listener-port derivation, P-D10 `pickIngressCluster` (still one probe per IP, still never
unioning candidate sets), the host-aware `translate.ListenerFor` gate, P-D13's per-build probe
memo (its key simply becomes `(ip, at)`), C-D2's locked-cluster ingress fan-out, C-D3's
pure-precondition chain gating, P-D1 containment (`pkg/build` must not import `pkg/route`), and
the node/edge/attribute/`labels` surface. No PromQL change, no new outcome, no new diagnostic
reason, no config flag.

## Risks

- **Behaviour drift on mid-window config changes.** A route that was served at `start` but not at
  `end` now degrades to external instead of resolving from the earlier segment. This is intended
  (D1) and is the only outcome-visible difference; builds whose config was stable over the window
  are byte-for-byte identical, which the existing end-to-end suite (all fixtures evaluated at
  `fixedNow`) pins.
- **Lost historical fidelity.** Per-version forensic answers ("what did this route to at 11:03?")
  are no longer derivable from the graph build path. The store still holds the interval-versioned
  history, so a dedicated forensic query API remains possible as a separate change; the exporter
  contract does not change.
