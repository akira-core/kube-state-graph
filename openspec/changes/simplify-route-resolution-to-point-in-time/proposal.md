# Simplify route resolution to a point-in-time query

## Why

The Istio route-resolution engine (`translate-global-fqdn-to-k8s-service`, extended by
`ingress-lb-service-fallback` and `route-hit-ingress-chain`) answers its question over the
build's whole `[start, end]` window. That range semantics costs three layers of machinery:

- `store.LoadTrafficWindow` loads EVERY resource version **overlapping** the window
  (`valid_from < t1`, client-side dedup, then `t0 < valid_to`);
- `memwindow.Segments()` slices the window at every version boundary and re-runs the IP 3-hop +
  gateway disambiguation per segment;
- because each segment could otherwise re-translate the same config, a content-signature cache
  (`ConfigSigAt`, `segmentResult`) exists purely to dedupe them — guarded by its own
  superset invariant test (`TestConfigSigCoversScopedInputs`) — plus a "latest hit wins" rule and
  an `outcomeRank` fold to reduce many segment outcomes back to one.

None of that multiplicity is consumed. One `/v1/graph` build needs exactly ONE answer per route
key ("which Service is this global FQDN?"), and the answer it should give is the one that was
true at the end of the request window — the same instant the service-graph `rate(...) @ end`
sample is evaluated at. The range semantics also leaks into unrelated rules: the ingress-LB
identity dedup degrades to *ambiguous* when a Service identity merely CHANGED inside the window,
which is an artefact of window-wide evaluation rather than a real ambiguity.

## What Changes

- The engine's time input becomes a single **instant `at`**, fixed to the build window's **`end`**.
  `RouteRequest.Start`/`End` collapse to `RouteRequest.At`. `ReadServiceGraph` keeps its
  `(window, end)` inputs — the PromQL range is unchanged — and passes only `end` to the engine.
- The store read becomes **as-of**: `LoadTrafficWindow` → `LoadTrafficAt(cluster, ip, at)` and
  `ClustersWithIngressIP(ip, at)`, both selecting the versions **live at `at`**
  (`valid_from <= at < valid_to`). `TrafficWindow` → `TrafficSnapshot`. The **no-FINAL
  duplicate-row pattern is unchanged** (SQL carries only immutable predicates; client-side dedup
  per version slot by max `ingest_seq`; the `valid_to` liveness test applied AFTER dedup), as is
  the `uniqueRows` pruned mode and the exporter-owned schema — only the predicate changes.
- `pkg/route/memwindow` becomes `pkg/route/snapshot`, evaluated at one instant:
  `Segments`/`Segment`, `ConfigSigAt` (and its invariant test) and the now-unused
  `GatewaysLiveAt` are **deleted**; `ResolveIPToGateways`, `ScopedFor` and
  `ResolveIPToIngressServices` lose their `t` parameter.
- `Resolver.resolve` becomes linear: probe per IP → select ingress cluster → load snapshot →
  3-hop → host disambiguation → listener gate → translate → `router_check_tool` → parse. The
  segment loop, the signature cache, "latest hit wins" and `outcomeRank` are **deleted**; the
  single evaluation yields exactly one outcome.
- The ingress-LB identity rule becomes as-of: ambiguity now means "more than one identity carries
  the IP **at `at`**" or "the IPs disagree" — an identity that merely changed earlier in the
  window is no longer ambiguous.
- Untouched: ingress-cluster selection (family-first `pickIngressCluster`), listener-port
  derivation, host-aware RouteConfiguration selection, the LB Service fallback and its gate on
  `no_gateway`, the RouteHit ingress chain, `pkg/build` not importing `pkg/route`, the store
  schema, and every node type / edge type / attribute / `labels` key.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: "Istio route resolution of global FQDN peers", "Ingress LB Service
  fallback for unresolved global FQDN peers" and "Full ingress chain for routed global-FQDN
  hits" change their time input from the build's `[start, end]` window to the single instant
  `end`, and their candidate/uniqueness rules from window-overlap to as-of liveness.

## Impact

- **Modified**: `pkg/build/routeresolve.go` (`RouteRequest.At`, doc wording),
  `pkg/build/routeprescan.go` (`request(at)`, `resolveRouteQueries(..., at)`),
  `pkg/build/servicegraph.go` (passes `end`, not `end.Add(-window)`),
  `pkg/route/store/{store,clickhouse}.go` (as-of reads, `TrafficSnapshot`, `LoadTrafficAt`),
  `pkg/route/resolver.go`, `pkg/route/ingresslb.go`, `pkg/route/scoped.go`,
  `pkg/route/store/mocks/` (regenerated), `CLAUDE.md`, `docs/*`.
- **Renamed**: `pkg/route/memwindow` → `pkg/route/snapshot`.
- **Behaviour**: for any build whose ingress config did not change inside the request window
  (the overwhelmingly common case) the emitted graph is byte-for-byte unchanged. When the config
  DID change inside the window, the answer is now the configuration live at `end` instead of the
  latest segment that produced a hit — which for a hit is the same version, and for a config that
  stopped routing the host before `end` correctly degrades to external.
- **Dependencies**: none added or removed. `make check-route-containment` unchanged.
- **Store**: no schema change; strictly fewer rows read per query.
