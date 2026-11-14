# Proposal

## Why

The opt-in Istio route engine (`--route-store-dsn`) costs the most per key of anything on the `/v1/graph` request path, and it repeats that cost wherever the answer is already known:

- **Within one build.** Keys are distinct FQDNs, but many of them sit behind one ingress IP and one gateway. Each key still re-loads the same ClickHouse snapshot (`LoadTrafficAt`, about five round-trips), rebuilds the same istiod world for the same RouteConfiguration, and forks `router_check_tool` (~50–60 ms) once per key, though the tool takes a whole batch of queries per invocation. With 32 keys behind one IP the benchmark build does 32 loads, 32 translations and 32 execs where one of each is enough.
- **Across builds.** The answer is a pure function of the key and the aligned window end, but nothing survives a build. A dashboard refreshing inside one end-alignment step re-runs the whole engine, after PromQL itself has already been served from the query cache.
- **Across concurrent requests.** `routeResolveConcurrency = 4` is per build. R concurrent builds run up to 4R istiod worlds and 4R child processes, with no ceiling.
- **In the store.** Every snapshot read ships every historical version of every scoped resource, `spec_json` included, because the no-FINAL reader deduplicates and filters liveness client-side. A VirtualService bound by bare gateway name is loaded from every namespace, and one row whose spec does not parse fails the whole IP.

Five smaller defects in the same area are fixed alongside: a panic in a route goroutine would crash the process (no recover); `BuildScopedRouteResolver`'s doc says the scope is called serially when the prescan calls it concurrently; `Translate` takes no context and busy-polls every 100 µs while its controllers sync; a malformed DSN's parse error can echo the DSN, credentials included; and nothing warns when ClickHouse credentials go over a plaintext connection.

## What Changes

- **Per-build memoisation (C8, C9 step 1).** The build scope memoises, besides the ingress probe it already memoises, the snapshot load per `(cluster, ip, instant)` and the translated RouteConfiguration per `(cluster, destination IPs, gateway namespace, gateway name, RouteConfiguration name)`. Concurrent identical work is coalesced; a failure is never memoised and never handed to a caller whose own context is still live.
- **Batched matching (C9 step 2).** A new optional `build.BatchRouteResolver` lets the prescan hand a build's whole key set to the engine at once. The engine prepares every key, groups the keys that reach the matcher by RouteConfiguration, and runs one `router_check_tool` invocation per group. A failed batch is retried one query at a time, so one bad query never fails its siblings. Resolvers without the interface, mocks and embedders included, run exactly as before.
- **Process-wide engine limit (C10 a).** Every translation and every matcher invocation, across all concurrent builds, takes a slot from one limit sized from `GOMAXPROCS`. Waiting honours the caller's context; a caller that cannot get a slot in time gets an error with no outcome and degrades to an external node (`route_engine_error`), never to a routing miss.
- **Cross-build result cache (C10 b).** A bounded TTL + LRU cache in front of resolution, keyed by `(caller cluster, host, path, port, destination IPs, instant in ms)`: 4096 entries, 60 s TTL, successful answers only, concurrent identical misses across builds coalesced. This relaxes the "the shared `*Resolver` stays stateless" rule; the bound answers the leak that rule guarded against.
- **Server-side version dedup (P-6, option B).** In the default read mode every version read keeps the newest row per version slot on the server (`ORDER BY ingest_seq DESC LIMIT 1 BY cluster, namespace, name, valid_from`) and only then filters liveness (`at < valid_to`) in an outer query, so ended versions and stale rewrite twins no longer cross the wire. `--route-store-unique-rows` keeps its pruned SQL unchanged; the client-side dedup and liveness check stay as the safety net.
- **VirtualService load hygiene (#16).** The SQL loads a bare-name-bound VirtualService only from a namespace that holds a candidate gateway (qualified bindings load from any namespace, as before). A VirtualService or Service row whose spec does not parse is skipped with a Warn instead of failing the IP.
- **Robustness (#15, #17).** Route goroutines recover panics into a per-key engine error. `Translate` takes a `context.Context`, stops when it ends, and waits for controller sync with an exponential backoff instead of a 100 µs busy-poll.
- **Credential hygiene (#18, #19).** A malformed route-store DSN is reported without echoing the DSN or any part of it. Opening the store warns when credentials will be sent over a non-TLS connection, and the startup log adds a `route_store_tls` boolean.
- **Docs (#25).** `BuildScopedRouteResolver` states the scope MUST be safe for concurrent use; a `-race` test pins it.

No HTTP surface, response body, golden, flag, metric or PromQL query changes. Answers are unchanged except where a route-store row that used to fail the whole IP is now skipped.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `pod-service-graph`: route-resolution work sharing within a build, batched matching, the process-wide engine limit, the cross-build result cache, server-side version dedup, bad-row tolerance and namespace-scoped bare bindings in the route-store read, panic isolation per key, and the route-store credential rules (no DSN echo, plaintext warning, `route_store_tls` in the startup log).

## Impact

- **Code**: `pkg/route` (memo and cache helpers, engine limit, batch resolution, translator/matcher seams), `pkg/route/translate` (`Translate(ctx, in)`, backoff sync wait), `pkg/route/store` (SQL wrapper, VS scoping, row skipping, DSN error redaction, TLS warning, `TLS()`), `pkg/build` (`BatchRouteResolver`, `RouteAnswer`, `RouteBatchLimits`, prescan batch branch and recover, doc fix), `cmd/kube-state-graph` (`route_store_tls` log field).
- **Go API**: additive in `pkg/build` (one optional interface, two structs). `translate.Translator.Translate` gains a leading `ctx` parameter — a breaking change to an exported method in `pkg/route/translate`, which only `cmd/` and opting-in embedders link; recorded in `docs/BREAKING.md`. `store.CH` gains `TLS() bool`. No new dependency (`golang.org/x/sync` is already direct).
- **Upstream load**: fewer ClickHouse round-trips, istiod translations and `router_check_tool` execs per build, a hard ceiling on concurrent engine work, and smaller result sets from ClickHouse.
- **Docs**: `.claude/rules/route-resolution.md` points (2), (4b), (5); `docs/BREAKING.md`.
- **Tests**: new `pkg/route` tests (memo counts, error and context non-sharing, batch parity, engine-limit ceiling and saturation, cache hit/miss/TTL/LRU/coalescing, concurrent scope under `-race`), `pkg/route/store` (SQL shape, DSN redaction, TLS warning, bare-binding SQL), `pkg/route/translate` (context cancellation), `pkg/build` (batch branch, panic recovery, concurrent scope); two `internal/integration` `CollapsedRows` assertions updated for server-side dedup.
