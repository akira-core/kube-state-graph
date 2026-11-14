# Design

## Context

See proposal.md — Why. The relevant current state:

- `pkg/build/routeprescan.go` `resolveRouteQueries` resolves a build's deduplicated, sorted route keys (capped at `maxRouteKeys = 512`) under an `errgroup` limited to `routeResolveConcurrency = 4`, each call bounded by `--route-resolve-timeout`. A key whose call fails while the build context is live is recorded `failed` (`route_engine_error`); a key the build deadline cut off gets no entry. When the resolver implements `build.BuildScopedRouteResolver`, the whole build goes through one scope.
- `pkg/route` `scopedResolver` memoises only `store.ClustersWithIngressIP` per `(ip, at)`, mutex-guarded, the read outside the lock (racing duplicates allowed). Everything after the probe — `loadSnapshot` (one `LoadTrafficAt` per IP), the in-memory 3-hop and gateway pick, `ScopedFor`, `translate.ListenerFor`, `Translate`, `matchcheck.Runner.Resolve` with ONE query — runs per key.
- `.claude/rules/route-resolution.md` (4b) says the shared `*Resolver` stays stateless because "an instance cache would leak"; (5) says "one translate + one check per resolution … no config-signature cache".
- `matchcheck.Runner.Resolve` already accepts a batch: it writes one test file with one case per query, and `parseActuals` requires a result for every case or fails the whole call.
- `store.CH` reads every version with `valid_from <= at` and dedups (max `ingest_seq` per slot) and filters liveness client-side, because a `valid_to` predicate in the scanning query drops a closing rewrite before dedup and lets its stale open twin win. `--route-store-unique-rows` restores the predicate for update-close writers and turns the client collapse count into an alarm.
- Baseline (`BenchmarkResolveRoute_SharedIngress`, 32 keys behind one IP, stage latencies scaled ~10× down): ~86 ms/build, 4 probes, 32 loads, 32 translates, 32 execs per build; the repeat-build benchmark is identical.

## Goals / Non-Goals

**Goals:**

- Do each expensive stage once per distinct input per build, and once per distinct key per instant across builds, with identical answers.
- A hard ceiling on concurrent istiod worlds and `router_check_tool` children in the process.
- Keep every degradation rule: errors carry no outcome, the build deadline leaves no entry, route resolution never fails a build.
- `pkg/build` stays free of `pkg/route`; resolvers without the new optional interfaces behave byte-for-byte as before.

**Non-Goals:**

- Caching across instants, or invalidating on store writes.
- New flags or metrics. Sizes are fixed constants, as `routeResolveConcurrency` already is.
- Changing `parseActuals`' all-or-nothing contract.

## Decisions

### D1. One coalescing memo primitive, failures never shared

`flightMemo[K, V]` (pkg/route) holds completed values and in-flight executions per key. A caller finding a value returns it; finding an execution waits for it (context-aware); otherwise it runs the work itself. A successful result is stored; a failed one is dropped, and **every waiter of a failed execution retries** under its own context instead of receiving the error. A leader panic releases its waiters (they retry) and re-panics on the leader's goroutine.

Retry-on-any-failure rather than share-unless-context-error: the two differ only during an outage, where retrying costs one extra read per waiter — the pre-change cost — and it removes the case where one key's per-call timeout becomes another build's `route_engine_error`.

*Rejected:* `golang.org/x/sync/singleflight` — it hands the leader's error, including the leader's own context cancellation, to every waiter, and its waiters cannot stop waiting on their own context. *Rejected:* keeping the probe memo's "racing duplicate is harmless" design — at 4 keys in flight it read the same IP four times per build (baseline: 4 probes/op).

### D2. Per-build memo of the snapshot load (C8)

`scopedResolver.loadTraffic` memoises `store.LoadTrafficAt` by `(cluster, ip, at.UnixMilli())`. The rows are read-only after the load: `loadSnapshot` copies them into its own union slices and `pkg/route/snapshot` never mutates a row. The memo lives and dies with the build scope.

*Rejected:* memoising the unioned snapshot per IP list instead — it saves only the in-memory union, and the per-IP load is what costs round-trips.

### D3. Per-build memo of the translated RouteConfiguration (C9 step 1)

`scopedResolver` memoises `Translate` by `(cluster, joined destination IPs, gateway namespace, gateway name, RouteConfiguration name)`. Within a build `at` is fixed, the IP list determines the unioned snapshot (in IP order, which is why the list is not sorted for the key: row order feeds istiod's config order), the gateway identity determines `ScopedFor`'s configs, registry and proxy, and the RouteConfiguration name is the only thing `Translate` reads from port and host (`ListenerFor`). The `*RouteConfiguration` is shared read-only (`matchcheck` only marshals it).

*Rejected:* keying by a hash of the scoped configs ("config signature") — it would need a canonical serialisation of istio configs to be sound, for no extra sharing within one instant.

### D4. Batched matching behind an optional `build.BatchRouteResolver` (C9 step 2)

```go
type BatchRouteResolver interface {
	RouteResolver
	ResolveRoutes(ctx context.Context, reqs []RouteRequest, limits RouteBatchLimits) []RouteAnswer
}
type RouteBatchLimits struct { Concurrency int; PerCallTimeout time.Duration }
type RouteAnswer struct { Dest RouteDestination; Outcome RouteOutcome; Err error; Answered bool }
```

`resolveRouteQueries` upgrades to the scope, then to `BatchRouteResolver`, and hands it the whole (capped, sorted) key list with its own concurrency and per-call timeout. The engine runs in two phases: (1) each key's preparation — cache lookup, probe, load, 3-hop, gateway pick, listener gate, memoised translation — under the concurrency limit with its own per-call timeout, ending either in a final answer or in a pending match; (2) one matcher invocation per distinct translation memo key, each under its own per-call timeout, with the group's queries de-duplicated by `(host, path)`. A failed group invocation whose context is still live is retried one query per invocation, so `parseActuals`' all-or-nothing rule never fails a sibling. The prescan records answers with the existing rules: `Answered == false` (never reached) and errors while the build context has ended record nothing.

Per-call timeout: it bounds each phase per key independently, so a key's total wall time is at most two timeouts plus queueing. One timeout over both phases would make a group's deadline depend on which key arrived first.

*Rejected:* micro-batching inside `ResolveRoute` (collect queries for a short window, then exec) — wall-clock dependent grouping and an added latency floor. *Rejected:* teaching the prescan the engine's phases — `pkg/build` would learn about translations. *Rejected:* failing the whole group on a batch error — a parity change against per-key resolution.

### D5. Process-wide engine limit (C10 a)

`*Resolver` holds one counting semaphore of `runtime.GOMAXPROCS(0)` slots (at least 1). Every `Translate` and every matcher invocation, including batched and single-query retries, holds one slot for its duration. Acquisition selects on the caller's context; on expiry it returns `errEngineSaturated` wrapping the context's cause, an error with no outcome, so the key degrades to `route_engine_error`, is not cached (D6), and cannot open the `RouteNoGateway`-gated LB fallback. A slot is taken only by the caller doing the work — never while waiting on another caller's memo or cache flight — so waiting can never deadlock against the limit. `LoadTrafficAt` is not limited: the ClickHouse driver has its own connection pool, and the store is I/O, not CPU.

*Rejected:* a flag — the useful value tracks CPUs, which the process knows. *Rejected:* failing fast when saturated — a burst would degrade keys that a few milliseconds of waiting would answer; the per-call timeout already bounds the wait.

### D6. Cross-build result cache (C10 b) — relaxing "stateless `*Resolver`"

`resultCache` (pkg/route): TTL + LRU, 4096 entries, 60 s TTL, keyed by `(caller cluster, host, path, port, joined IPs, at.UnixMilli())`, holding `(RouteDestination, RouteOutcome)` — both plain values. Only successful answers are stored. Concurrent identical misses across builds coalesce with the same never-share-a-failure rule as D1. Both `ResolveRoute` and the batch path consult it (a batch key that finds another build's flight waits for it in phase 1; on that flight's failure it resolves alone).

`at` is the build's aligned window end (30 s grid by default), so repeated dashboard builds share it. Staleness: the answer for an instant can change only if the exporter later writes a version whose `valid_from` is at or before that instant — a late write. 60 s bounds that window, the same TTL the PromQL query cache uses.

The 4b rule existed because an unbounded instance cache leaks; a size-bounded LRU with TTL cannot. Memory: one entry is a key of a few short strings plus a `RouteDestination` — well under 1 KiB, so the cap is a few MiB at most.

*Rejected:* a per-build-only memo of whole answers — it does nothing for repeat builds, which is where the engine dominates once PromQL is cached. *Rejected:* caching errors or misses-with-errors — an outage would pin `route_engine_error` for a minute after recovery.

### D7. Server-side per-slot dedup, liveness in an outer query (P-6 option B)

Default mode renders every version read as

```sql
SELECT <cols> FROM (
  SELECT <cols> FROM <table> WHERE <scope> AND valid_from <= <at>
  ORDER BY ingest_seq DESC LIMIT 1 BY cluster, namespace, name, valid_from
) WHERE <at> < valid_to
```

The inner query is the same scan as today (immutable predicates only, skip indexes unchanged) and applies the ReplacingMergeTree rule; the outer query applies liveness to the survivors, so ended versions and stale twins no longer cross the wire. ClickHouse does not push a predicate from an outer query into a subquery that has `LIMIT BY` (the pushdown would change the result), which is what keeps the order dedup-then-liveness; the integration suite exercises the stale-twin case against a real server. Unique-rows mode keeps today's single-query shape. The client-side `dedupLiveAtCounted` stays in both modes; in default mode it now normally collapses nothing, so `CollapsedRows` reads 0 there (it was never an alarm in that mode).

*Rejected:* option C (envelope first, then `spec_json` for live slots) — two round-trips per hop. *Rejected:* `FINAL` — the ~10× latency the no-FINAL design avoided. *Rejected:* option A (keep shipping every version) — the owner chose B.

### D8. VirtualService load hygiene (#16)

The VS query keeps `hasAny(bound_gateways, <both binding forms>)` (index-friendly, unchanged) and adds `AND (hasAny(bound_gateways, <qualified refs>) OR has(<candidate gateway namespaces>, namespace))`: a bare `<name>` binding only means a gateway in the VS's own namespace, so a VS in a namespace with no candidate gateway can only bind by the qualified form. Still a superset of `snapshot.boundTo`, which re-applies the exact rule. A VS or Service row whose spec does not parse is skipped with a Warn naming `(cluster, namespace, name)`, and is dropped from the snapshot too — `ScopedFor` would otherwise fail on it later.

*Rejected:* an exact SQL rewrite (`arrayMap(g -> concat(namespace, '/', g), bound_gateways)`) — it defeats a skip index on `bound_gateways`.

### D9. `Translate(ctx, in)` and backoff (#17)

`Translate` takes a context, returns early if it has ended, and `waitSynced` polls with exponential backoff from 100 µs to a 2 ms cap, selecting on the context. The doc comment now says the call starts the krt controllers' goroutines and stops them before returning. This is a breaking change to an exported method in `pkg/route/translate` (`docs/BREAKING.md`).

*Rejected:* event-driven sync (`WaitUntilSynced`) — the krt collection API differs across the istio versions this code has tracked; backoff is enough to stop the spin.

### D10. Panic isolation (#15)

Every goroutine the prescan or the engine starts for a key or a group defers a recover that logs the panic with its stack and turns it into that key's (or each group member's) engine error. A memo or cache leader that panics releases its waiters first (D1). The prescan also recovers around a batch call that panics on its own goroutine, recording nothing for unanswered keys.

### D11. DSN redaction and plaintext warning (#18, #19)

A `*url.Error` from `clickhouse.ParseDSN` is replaced by a fixed message ("route-store DSN is not a valid URL; not shown because it may carry credentials"): unwrapping is not enough, since the inner error can quote an authority fragment (a password containing `/` splits the authority and surfaces as an invalid port). Other `ParseDSN` errors name a query parameter and its value, not the userinfo, and pass through. After `applyAuth`, if the effective password is non-empty and the options carry no TLS config, `Open` logs a Warn without values. `CH.TLS()` reports whether TLS is configured, and the startup log adds `route_store_tls`.

### D12. Documentation of the build scope (#25)

`BuildScopedRouteResolver`'s doc now says the scope MUST be safe for concurrent use (the prescan calls it from up to `routeResolveConcurrency` goroutines). A `pkg/build` test proves the prescan does call it concurrently and a `pkg/route` `-race` test drives the real scope with concurrent keys through a non-empty snapshot.

## Risks / Trade-offs

- **Stale answers for up to 60 s** if the exporter writes late at or before an instant already resolved. Accepted, as for the PromQL cache.
- **Batch parity rests on `router_check_tool` answering each case independently of its siblings.** Envoy's matcher evaluates each test case against the same immutable RouteConfiguration, so a case's answer cannot depend on another case. The real batch output is exercised by `TestRouteSuite` only where `router_check_tool` is installed (CI).
- **Two timeouts per key in batch mode** (D4). Bounded and documented.
- **The engine limit can turn a burst into `route_engine_error` keys** where previously they would have run (slowly). That is the intended degradation; the alternative was unbounded process fan-out.
- **`CollapsedRows` in default mode now normally reads 0.** It was documented as expected-nonzero noise in that mode; the integration assertions that relied on it now assert the outcome (the dead version stays dead) instead.

## Migration Plan

No configuration change. Embedders calling `translate.Translator.Translate` add a context argument. Rollback is a revert; no persisted state.
