# Design

## Context

See proposal.md — Why. Relevant current state:

- Every upstream query the server issues goes through `promql.Router`
  (`cmd/` always builds one; with no `--backends-file` it is the implicit
  single-backend table). A build binds one `fanoutQuerier` over an immutable
  `routerState`, whose clients are keyed by `clientKey{url, username,
  password}` and carried across `Swap` when the key survives.
- Build-internal concurrency is bounded per wave only (`scopeConcurrency = 16`
  on scoped waves; the 37 first-wave topology legs and the 3 service-graph legs
  are unbounded). Nothing is shared across builds.
- The archived design and CLAUDE.md state three rules this change revises:
  "No server-side result cache", "no singleflight", and "No time-window
  alignment". The body-determinism contract, the fail-closed / degrade rules
  per leg, and the `kube_state_graph_upstream_query_*` label sets are NOT
  revised.
- `pkg/promql` is importable by embedders; its exported signatures
  (`NewRouter`, `Querier`, `Metrics`, `ClientFactory`) must not break.

## Goals / Non-Goals

**Goals:**
- Hard bound on in-flight queries per upstream store, per process, shared by
  every request kind, surviving routing-table reloads.
- Reuse identical upstream results across builds, including concurrent ones.
- Raise hit rate for "now"-style clients through opt-out end alignment.
- Zero observable body change from caching.
- An embedder using the public constructors gets the same three features with
  the same defaults as the server, without writing any option (D10).

**Non-Goals:**
- A distributed / cross-replica cache (Redis L2 remains the separate, future
  change). Each replica has its own cache and its own limit.
- Caching built graphs or response bodies.
- Per-backend limit overrides in the routing file, per-request fairness or
  priority classes (see Open Questions).
- Retries of failed upstream queries.
- Aligning the on-demand pod-application lookup (`ResolvePodApplication`
  keeps its caller-supplied instant; it still benefits from limit + cache).

## Decisions

### D1 — Guard lives in the Router, wrapping each per-store client

A new internal `guardedQuerier` wraps every client the `Router` builds, one
per `clientKey`, and is stored in `routerState.byKey` in place of the bare
client. `buildState` carries the guard (with its semaphore) across a `Swap`
exactly as it carries the client today, so a reload never resets in-flight
accounting for a surviving store, and two table backends naming one store
share one guard.

Call order inside the guard: **cache lookup → miss coalescing → slot acquire →
inner `Instant`**. A hit or a coalesced waiter therefore never holds a slot,
and the slot is held only for the real round-trip.

Alternatives: (a) limit inside the build's errgroups — cannot bound across
requests; (b) inside `promql.Client` — bypassed by any custom
`ClientFactory` (tests, embedders), and `Client` does not know about stores
shared by several backend names. The Router is the one place every server
query passes and that already owns store identity.

The guard type is exported as a constructor, `promql.Guard(q Querier, opts
...RouterOption) Querier`, so a caller that does not use a Router can still wrap
one store. `kubegraph.New` uses it for a plain `Querier` (D10). `pkg/build.New`
stays the unguarded low-level constructor: it is the seam the component,
golden and property tests drive with mocks, and it must not start caching
behind their backs.

### D2 — Semaphore: `golang.org/x/sync/semaphore.Weighted`

FIFO, context-aware, already in the module (`golang.org/x/sync` is a direct
dependency). Weight 1 per query. The limit is fixed at startup (flag), so no
resize path is needed. `0` ⇒ no semaphore object at all (nil check), not a
huge weight.

A waiting query that loses to its context returns `ctx.Err()` wrapped like any
query error; `build` already maps deadline/cancel to `504 timeout` /
cancellation, so no new `build.Reason` is needed.

### D3 — Probes bypass guard by family, not by flag

The guard itself classifies the query NAME with `FamilyOf` and hands any
`FamilyProbe` query straight to its inner client. Doing it in the guard, not in
`fanoutQuerier.issue`, covers every path at once — the routed fan-out,
`Router.ProbeAll` (which calls the per-store clients directly), and a
`promql.Guard` wrapping a plain client, whose retention `up{}` never passes a
fan-out. A
saturated store must still answer `/readyz` truthfully, otherwise readiness
flaps under load and the orchestrator pulls replicas exactly when they are
needed. The retention `up{}` probe is a single cheap query; caching it would
risk classifying an empty graph against a stale health result.

### D4 — Cache: in-house LRU, series-count cost, per-entry TTL

One cache per Router, shared by all guards (one global budget). Structure:
`map[cacheKey]*list.Element` + `container/list`, one mutex. No new
dependency (CLAUDE.md dependency rule); the structure is ~100 lines.

- `cacheKey{store uint64, query string, ts int64 /*UnixNano*/}`. `store` is a
  per-Router monotonically assigned id for a `clientKey`, carried with the
  guard across swaps — so the key holds no credential (spec requirement) and a
  store removed and later re-added gets a new id (no stale reuse across a
  credential change).
- Cost = `max(1, len(vec))`, so empty results still consume budget and cannot
  grow the map unboundedly.
- TTL checked on read; expired entries are removed lazily on access and during
  eviction scans. No background goroutine (matches the "no background work
  unless configured" posture of the telemetry package).
- Series count, not bytes: bytes would need label-size accounting on every
  insert; series count is already computed (`len(vec)`) and the upstream
  series-limit histogram gives operators the distribution to size it.

Why the evaluation instant is in the key and not rounded inside the cache:
rounding in the cache would silently serve a different instant than the one
the build renders and the route engine resolves at. Alignment is done once,
up front (D7), so every consumer of `end` agrees.

### D5 — Cached vectors are shared read-only; return a fresh slice header

`model.Vector` is `[]*model.Sample`. On hit, the guard returns a new slice
(`slices.Clone`) of the cached pointers — cheap, and it makes append/reorder by
a reader safe. `*Sample` and its `Metric` map are shared and MUST NOT be
mutated by readers. Deep copy on every hit was rejected: it costs O(labels)
per series per hit, defeating much of the point.

Enforcement: an audit task over `pkg/build` for writes through a sample
(`s.Metric[...] =`, `delete(s.Metric, …)`, `s.Value =`), plus a test that runs
a representative build twice through a cache in race mode and asserts the
cached samples are unchanged (fingerprint + value snapshot before/after).

### D6 — Miss coalescing: `singleflight` + per-waiter context

`golang.org/x/sync/singleflight.Group.DoChan` keyed by the cache key. Each
caller `select`s on its own `ctx.Done()` and the result channel, so a waiter's
deadline is its own. The shared call runs under the FIRST caller's context
(spec). If that context ends, the shared result is a context error that is not
the waiters' own; a waiter whose own `ctx` is still live and who receives
`context.Canceled`/`DeadlineExceeded` from the shared call retries once as a
new leader. One retry is enough: the retry's leader is the waiter itself.

The cache lookup and the group join are two steps, and a flight can complete —
put its entry, then leave the group — between them; a caller arriving in that
window would start a second flight for a key that is already cached. So the
shared call re-reads the cache before fetching: a flight puts before the group
forgets its key, so the re-read sees the entry and no second upstream query is
issued. A caller served that way is counted as coalesced, not as a hit — its
miss was already counted and another caller's query answered it.

Rejected: running the shared call under `context.WithoutCancel` plus a fixed
timeout — it would let an abandoned query keep a slot after every caller left,
which is exactly the load this change exists to remove.

### D7 — Alignment is a pure helper applied after parsing

`kubegraph.AlignWindow(start, end time.Time, grid time.Duration) (time.Time,
time.Time)`: floors `end.UnixNano()` to a multiple of `grid` (Unix-epoch
aligned, as the spec requires — `time.Truncate` is relative to the zero time
and differs for grids that do not divide the epoch offset), sets
`delta := end - aligned`, returns `(start - delta, aligned)`. `grid <= 0` ⇒
identity.

It is applied in exactly two places, after `ParseValues` /
`ParseStorageValues` succeeded: the `internal/api` handlers (reading
`cfg.EndAlign`) and `kubegraph.Engine.BuildFromValues` /
`BuildStorageFromValues` (reading new `kubegraph.Options.EndAlign`; zero means
`kubegraph.DefaultEndAlign`, negative disables — D10). The parsers stay config-free, keeping the one-parser rule. Because
the aligned `end` becomes THE build instant, route resolution (`RouteRequest.At`)
and service-graph evaluation stay on one instant automatically.

### D8 — Configuration and defaults

| Flag | Env | Default (server AND library) | Exported constant |
|---|---|---|---|
| `--upstream-max-concurrency` | `KSG_UPSTREAM_MAX_CONCURRENCY` | 32 | `promql.DefaultMaxConcurrency` |
| `--query-cache-max-series` | `KSG_QUERY_CACHE_MAX_SERIES` | 100000 | `promql.DefaultQueryCacheMaxSeries` |
| `--query-cache-ttl` | `KSG_QUERY_CACHE_TTL` | 60s | `promql.DefaultQueryCacheTTL` |
| `--end-align` | `KSG_END_ALIGN` | 30s | `kubegraph.DefaultEndAlign` |

`internal/config.Defaults()` reads the exported constants, so the server's
defaults and the library's cannot drift. On the flag path `0` disables (an
operator writes the value explicitly). Validation: all four non-negative; TTL
must be > 0 when the cache is on.

Library surface: `promql.NewRouter(t, m, factory, opts ...RouterOption)` with
`WithMaxConcurrency(n)` and `WithQueryCache(maxSeries, ttl)` — variadic, so
every existing call compiles unchanged — and the four `kubegraph.Options`
fields of D10.

32 per store: the first topology wave (37 legs) of a single `/v1/graph`
already exceeds it slightly, so one build alone sees mild queueing on a single
store but never a VM `503`; VM's default `-search.maxConcurrentRequests`
scales with vmselect CPU count, so operators should set `limit × replicas ≤`
that value (documented).

### D9 — Metrics through optional upgrade interfaces

Same pattern as `RouterMetrics` / `SeriesMetrics`: new `promql.LimiterMetrics`
(`AddInflight(backend, delta)`, `ObserveSlotWait(backend, seconds)`) and
`promql.CacheMetrics` (`IncCacheHit`, `IncCacheMiss`, `IncCacheCoalesced`,
`IncCacheEviction`, `SetCacheSeries`), type-asserted, no-op when absent.
`promql.Metrics` is not widened.

New series: `kube_state_graph_upstream_inflight{backend}`,
`kube_state_graph_upstream_slot_wait_seconds{backend}`,
`kube_state_graph_query_cache_{hits,misses,coalesced,evictions}_total`,
`kube_state_graph_query_cache_series`. The backend label is the routing-table
name the fan-out used (the guard's instant method takes it from
`fanoutQuerier`, which already knows it); a store shared by two names reports
under whichever name issued each query.

Slot-wait span attribute: the guard stores the wait duration in the context
(package-private key); `Client.Instant` reads it and adds
`kube_state_graph.slot_wait_ms` to the existing `prometheus.query` span. A
custom factory's querier simply ignores it.

## Risks / Trade-offs

- [Serving data up to TTL old for a past instant — late-arriving samples,
  VM dedup/downsampling] → TTL bounded (60s default), and the key is an exact
  instant, so only re-reads of the same instant are affected; `0` disables.
- [Alignment moves the effective `end` back up to one grid step — BREAKING for
  clients that expect exactly-`end` semantics] → `--end-align=0` restores
  passthrough; `kubegraph.Options.EndAlign < 0` does the same for embedders;
  `docs/BREAKING.md` entry names both.
- [Embedders (`graph-api-gateway`) change behaviour on upgrade: alignment,
  caching and queueing switch on without a code change] → deliberate (the
  user asked for default-on in the library); BREAKING entry lists the exact
  opt-outs; the defaults are exported constants so an embedder can pin or
  inspect them.
- [Tests that construct a Router or `kubegraph.Engine` and swap fixtures
  between calls with the same query/instant would now read a cached result] →
  such tests opt out explicitly (`WithQueryCache(0, 0)` / `Options` field
  `< 0`); `build.New`-based tests are unaffected by construction.
- [Memory: 100000 series ≈ 100–200 MB depending on label cardinality] → budget
  flag; resident-series gauge; set Pod memory limits accordingly.
- [Head-of-line blocking: a large storage build can hold all slots of a store
  and delay a small graph build] → FIFO per query interleaves builds at query
  granularity; per-request fairness deferred.
- [Limit is per replica; HPA scale-out multiplies upstream concurrency] →
  documented sizing rule; the limit still turns an unbounded fan-out into a
  bounded one.
- [A reader mutating a shared cached sample corrupts later bodies] → D5 audit +
  race-mode regression test; golden tests run with the cache enabled.
- [Coalesced waiter inherits a leader's cancellation] → one-shot retry (D6).
- [A flight completes between a caller's lookup and its join] → the shared
  call re-reads the cache before fetching (D6).
- [Timeouts rise instead of 503s when the store is slow] → intended: a queued
  query that cannot finish within `--build-timeout` fails as `504 timeout`,
  which is the correct signal; slot-wait histogram shows where time went.

## Migration Plan

1. Ship with defaults on (server and library). Operators wanting today's exact behaviour set
   `--upstream-max-concurrency=0 --query-cache-max-series=0 --end-align=0`.
   Embedders wanting it pass `WithMaxConcurrency(0)`, `WithQueryCache(0, 0)`
   to `NewRouter` and `EndAlign: -1` (plus `MaxConcurrency: -1`,
   `QueryCacheMaxSeries: -1` when handing `kubegraph.New` a plain `Querier`).
2. Before rollout, compare `--upstream-max-concurrency × replicas` with each
   vmselect's `-search.maxConcurrentRequests`.
3. Rollback: flags to `0` (no redeploy of a different image needed) or revert
   the image; no persisted state exists.

### D10 — Library defaults: zero means default, explicit value means override

The public constructors enable everything by default:

- `promql.NewRouter(t, m, factory)` with no `RouterOption` behaves as if
  `WithMaxConcurrency(DefaultMaxConcurrency)` and
  `WithQueryCache(DefaultQueryCacheMaxSeries, DefaultQueryCacheTTL)` were
  passed. `WithMaxConcurrency(0)` / `WithQueryCache(0, 0)` disable; a
  `WithQueryCache(n>0, ttl<=0)` uses the default TTL.
  `promql.SingleBackendTable` + `NewRouter` (the embedder's one-URL path) is
  therefore guarded with no extra code.
- `kubegraph.Options` gains `MaxConcurrency int`, `QueryCacheMaxSeries int`,
  `QueryCacheTTL time.Duration`, `EndAlign time.Duration`. Convention: **zero
  ⇒ the exported default, negative ⇒ disabled, positive ⇒ that value.** Zero
  must mean "default" because a Go struct literal cannot distinguish "unset"
  from "0", and default-on is the requirement; pointers were rejected as
  clumsy for an options struct every embedder writes by hand.
- `kubegraph.New(q, opts)`: if `q` is a `promql.QuerierSource` (a Router), it
  is used as-is — the Router already carries its own guard, configured at
  `NewRouter`, and `Options.MaxConcurrency` / `QueryCache*` are ignored (a
  documented rule, since wrapping a Router would hide `QuerierFor` and break
  az routing). Otherwise `q` is wrapped with `promql.Guard(q, …)` built from
  the three `Options` fields. `EndAlign` applies in both cases.
- `kubegraph.Engine.Probe` goes to the unwrapped querier (D3).
- `ResolvePodApplication` receives a `LabelQuerier` — in practice the Router —
  and is guarded through it; it is not aligned (Non-Goals).

## Open Questions

- Per-backend `maxConcurrency` override in the routing file — deferrable,
  additive to the backends-file schema.
- Whether to prioritise graph over storage builds when a store is saturated —
  deferrable; needs production wait-histogram data first.
