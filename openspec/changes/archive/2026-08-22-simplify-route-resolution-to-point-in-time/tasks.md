# Tasks

Simplifies the Istio route-resolution engine from a `[start, end]` range query to a single as-of
query at the window's `end`: the store reads as-of, `memwindow` becomes `snapshot`, and the
resolver's segment loop + config-signature cache are deleted. TDD throughout: tests first (RED),
then implementation (GREEN).

## 1. Contract (`pkg/build`)

- [x] 1.1 `pkg/build/routeresolve.go`: replace `RouteRequest.Start, End time.Time` with
      `At time.Time` (doc: the build window's END; the engine evaluates at that one instant).
      Update the `RouteOutcome` / `RouteDestination.Ingress*` / `BuildScopedRouteResolver` docs
      from window/overlap wording to as-of wording (memo key becomes `(ip, at)`).
- [x] 1.2 `pkg/build/routeprescan.go`: `(k routeKey) request(at time.Time)`,
      `resolveRouteQueries(ctx, resolver, perCallTimeout, keys, at)`. `routeKey` unchanged.
- [x] 1.3 `pkg/build/servicegraph.go`: pass `end` (not `end.Add(-window)`) into
      `resolveRouteQueries`, with a comment marking the range-in / instant-out seam.
- [x] 1.4 `pkg/build/routeprescan_test.go`: `request(at)` signature; assert the produced
      `RouteRequest.At` equals the build's `end`. Sweep every `RouteRequest{Start:…, End:…}`
      literal in `pkg/build` tests.

## 2. Store as-of reads (`pkg/route/store`)

- [x] 2.1 `store.go`: `TrafficWindow` → `TrafficSnapshot`; `LoadTrafficWindow(ctx, cluster, ip,
      t0, t1)` → `LoadTrafficAt(ctx, cluster, ip, at)`; `ClustersWithIngressIP(ctx, ip, at)`.
      Docs restated as-of.
- [x] 2.2 `clickhouse_test.go` (RED): `versionRow.overlapsWindow` tests → `liveAt(at)` tests
      (`vf <= at < vt`; inclusive lower bound, exclusive upper). Dedup / collapse-count tests
      unchanged.
- [x] 2.3 `clickhouse.go`: `overlapsWindow(t0,t1)` → `liveAt(at)`;
      `dedupOverlapCounted(..., t0, t1)` → `dedupLiveAt(..., at)`; all five queries'
      `valid_from < {t1}` → `valid_from <= {at}`; `prune(t0)` → `prune(at)` (body unchanged:
      `AND {at} < valid_to`). Package doc rewritten for as-of while KEEPING the three-stage
      no-FINAL explanation and the `dt64Lit` bind trap note.

## 3. `pkg/route/memwindow` → `pkg/route/snapshot`

- [x] 3.1 `git mv pkg/route/memwindow pkg/route/snapshot`; `package snapshot`; `Window` →
      `Snapshot`; `New(rows store.TrafficSnapshot, at time.Time) *Snapshot`.
- [x] 3.2 Delete `Segment` / `Segments()`, `ConfigSigAt()` and `GatewaysLiveAt()` (dead code).
- [x] 3.3 Drop the `t` parameter from `ResolveIPToGateways(ip)`, `ScopedFor(gw)` and
      `ResolveIPToIngressServices(ip)` — all evaluate at `s.at`.
      `ResolveIPToIngressServices` changes from window-overlap to as-of; doc states that a
      superseded identity no longer causes ambiguity.
- [x] 3.4 Tests: delete `TestConfigSigCoversScopedInputs`, the version-boundary signature test
      and any `Segments` test; keep `TestScopedForBareGatewayRef` and
      `TestScopedForToleratesUnknownSpecFields`; add `TestScopedForSkipsVersionNotLiveAt` and an
      as-of case for `ResolveIPToIngressServices`.

## 4. Resolver (`pkg/route`)

- [x] 4.1 `resolver_test.go` (RED): `testStart/testEnd` → `testAt`; `testRequest` sets `At`; mock
      expectations use `(ip, at)` / `(cluster, ip, at)`. Delete `TestOutcomeRank`. Add
      `TestResolveRoute_UsesVersionLiveAtInstant` (a closed old Gateway version + a live one →
      the live one decides).
- [x] 4.2 `resolver.go`: linear pipeline — probe per IP → `pickIngressCluster` → `loadSnapshot`
      → `snapshot.New(rows, at)` → 3-hop candidates → `gwresolve.ResolveAmong` → `ScopedFor` →
      `translate.ListenerFor` gate → `Translate` → `matchcheck` → `ParseEnvoyCluster`. Delete
      `segmentResult`, the signature cache, `outcomeRank`, `noteMiss`, the hit accumulators and
      the latest-hit-wins rule. `loadWindow` → `loadSnapshot`; `candidatesAt` → `candidates`.
      Keep the `RouteNoGateway` gate on the LB fallback and the RouteHit ingress-identity stamp.
- [x] 4.3 `ingresslb.go`: type renames + as-of doc wording; tri-state precedence unchanged.
      `ingresslb_test.go`: `lbT0/lbT1` → `lbAt`; the "identity change within window" case splits
      into "two identities live at `at` → ambiguous" and "superseded identity → unique".
- [x] 4.4 `scoped.go`: `probeKey{ip string; at int64}`; `probe(ctx, ip, at)`; error-not-cached
      behaviour unchanged.

## 5. Mocks

- [x] 5.1 `make mocks` (regenerates `pkg/route/store/mocks/mock_store.go`); `make verify-mocks`
      clean.

## 6. Integration (`internal/integration/route_e2e_test.go`)

- [x] 6.1 `RouteStoreSuite`: `LoadTrafficAt(ctx, cluster, ip, fixedNow)`,
      `ClustersWithIngressIP(ctx, ip, fixedNow)`, `snapshot.New(w, fixedNow)`.
- [x] 6.2 `TestTrafficWindowThreeHopAndTranslate` → `TestTrafficSnapshotThreeHopAndTranslate`:
      no `Segments()` loop — one 3-hop + `ScopedFor` + translate at `fixedNow`.
- [x] 6.3 Assert the point-in-time semantics against the existing fixture: the Gateway version
      closed at `fixedNow - 1h` is NOT in the loaded snapshot; only the current version is.
- [x] 6.4 `TestNginxFallbackResolvesViaRealStore`: `RouteRequest{..., At: fixedNow}`.
      `TestUniqueRowsAgainstRewriteWriterResurrectsStaleRow` kept (as-of variant).
- [x] 6.5 `RouteSuite` (full chain) expectations unchanged — the no-regression proof.

## 7. Docs

- [x] 7.1 `CLAUDE.md` route bullet: rewrite (2), (4b), (5), (5b), (5c) for the single instant;
      drop the `ConfigSigAt` / segment / D13-window-memo wording.
- [x] 7.2 `docs/istio-virtualservice-routing-history-design.md`: note that the store stays
      interval-versioned (exporter contract unchanged) while kube-state-graph's graph-build
      consumer resolves as-of `end`; per-version forensic queries are a separate API.
- [x] 7.3 `docs/nginx-ingress-backend-resolution.md`: sync any window wording.

## 8. Verification

- [x] 8.1 `make build vet lint`, `go test ./pkg/... -count=1 -race`, `make test` (golden files
      must not change), `make verify-mocks`, `make check-route-containment`.
- [x] 8.2 Docker integration: `go test ./internal/integration/ -run
      'TestRouteStoreSuite|TestRouteSuite' -count=1` (full chain needs `KSG_ROUTER_CHECK_BIN`).

## Archive note: one scenario is deliberately dropped

This change was archived with `openspec archive --no-validate`. The validator refuses a
MODIFIED block that omits a scenario the promoted requirement still has — it cannot tell a
silent regression from an intentional retirement, and OpenSpec has no scenario-level
REMOVED.

The dropped scenario is "Identity change within the window degrades to external" on
"Ingress LB Service fallback for unresolved global FQDN peers". It asserts the OLD
window-based behaviour that D5 removes: with resolution pinned to a single instant, an
identity that ended before that instant is simply not a candidate, so the case resolves
instead of degrading. Two scenarios in this change replace it — "A superseded identity does
not make the IP ambiguous" and "Version churn of one identity still resolves". Keeping the
old title would have re-asserted behaviour the implementation no longer has.

Two other scenarios the validator flagged were genuine omissions, not retirements, and were
copied in before archiving: "Cross-namespace Gateway is not a candidate"
(scope-gateway-candidates-to-ingress-namespace) and "Identical host patterns resolve to the
lexically-smallest gateway" (harden-istio-route-resolution), both of which remain true under
point-in-time resolution.
