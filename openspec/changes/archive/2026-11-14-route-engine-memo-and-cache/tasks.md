# Tasks

## 1. Baseline

- [x] 1.1 Put the resolver's translate and match stages behind unexported `translator` / `matcher` interfaces (`NewResolver` unchanged) and add counting, sleep-based fakes plus `BenchmarkResolveRoute_SharedIngress` (one cold build, 32 keys, one IP) and `BenchmarkResolveRoute_SharedIngressRepeat` (same build, same instant), reporting probes / loads / translates / execs per build. Record the baseline with `go test ./pkg/route -run '^$' -bench SharedIngress -benchmem -count=5`.

## 2. Robustness and hygiene

- [x] 2.1 `pkg/build` route goroutines recover a panic into that key's `route_engine_error` (test: a panicking resolver key degrades alone, the build completes).
- [x] 2.2 `BuildScopedRouteResolver` doc: the scope MUST be safe for concurrent use; a `pkg/build` test shows the prescan calls the scope concurrently, run under `-race`.
- [x] 2.3 `translate.Translator.Translate(ctx, in)`; `waitSynced` honours the context and backs off from 100 µs to 2 ms; fix the "starts no goroutines" comment; update every caller including `oracle_test.go` (`go vet -tags oracle ./pkg/route`) and `internal/integration`; `docs/BREAKING.md` entry. Test: an ended context returns its error.
- [x] 2.4 Malformed DSN errors never quote the DSN (test with a password containing `/` and one with a bad `%` escape; assert neither the DSN nor the password appears).
- [x] 2.5 Warn when a password is sent without TLS; `CH.TLS()`; `route_store_tls` in the startup log.

## 3. Per-build sharing

- [x] 3.1 `flightMemo` with never-shared failures, context-aware waiting and panic release; unit tests for hit, coalescing, error retry, waiter retry after a leader's context ends, and panic release, under `-race`.
- [x] 3.2 Move the probe memo onto `flightMemo`; memoise `LoadTrafficAt` per `(cluster, ip, at)` (C8). Tests: N keys sharing one IP load once; errors are not cached; outcomes equal per-key resolution.
- [x] 3.3 Memoise the translated RouteConfiguration per `(cluster, ips, gateway ns, gateway name, RC name)` (C9 step 1). Tests: N keys on one RC translate once; different ports translate separately; a translation error is retried.
- [x] 3.4 `-race` test driving one real scope with concurrent keys through a non-empty snapshot.

## 4. Process-wide limit

- [x] 4.1 `engineLimit` semaphore sized by `GOMAXPROCS` around every translate and matcher call; context-aware acquire returning an outcome-less error. Tests: peak in-flight never exceeds the limit across concurrent builds; a saturated caller whose context ends gets an error and no outcome.

## 5. Cross-build cache

- [x] 5.1 `resultCache` (4096 entries, 60 s TTL, LRU, successful answers only, coalesced misses, failed flights not shared). Unit tests: hit, miss, TTL expiry, LRU eviction, error not cached, coalescing, waiter retry after leader failure, under `-race`.
- [x] 5.2 Put it in front of `ResolveRoute` and the scope path. Test: a repeat build at the same instant does no store, translate or matcher work and returns the same answers; a different instant misses.
- [x] 5.3 Update `.claude/rules/route-resolution.md` (4b) and (5) and the `pkg/route` package / `scoped.go` / `routeprescan.go` comments.

## 6. Route-store reads

- [x] 6.1 Default-mode version reads dedup per slot server-side (`ORDER BY ingest_seq DESC LIMIT 1 BY cluster, namespace, name, valid_from`) inside, liveness outside; unique-rows mode unchanged. Unit tests on the rendered SQL (no `valid_to` in the deduplicating query in default mode; unique-rows shape unchanged).
- [x] 6.2 VirtualService SQL scopes bare bindings to candidate gateway namespaces; an unparseable VS or Service row is skipped with a Warn and dropped from the snapshot. Unit tests for the binding sets and the row skip.
- [x] 6.3 Update the `CollapsedRows` assertions in `internal/integration/route_e2e_test.go` for server-side dedup and run `go test ./internal/integration -run 'TestRouteStoreSuite' -count=1`.

## 7. Batched matching

- [x] 7.1 `build.BatchRouteResolver`, `RouteBatchLimits`, `RouteAnswer`; the prescan uses it when the scope implements it, with the existing record rules. `pkg/build` tests with a fake batch resolver: index equal to the per-key path, unanswered keys get no entry, errors after the build deadline record nothing, a panicking batch is recovered.
- [x] 7.2 `scopedResolver.ResolveRoutes`: phase 1 prepare under the limit and per-call timeout, phase 2 one matcher call per RC group with single-query retry on failure, recover in every goroutine. Tests: 32 keys on one RC → 1 exec; batch vs per-key parity over a mixed key set; a failing batch retries per query; a deadline leaves unanswered keys unanswered.
- [x] 7.3 Re-run the benchmarks, record the after numbers next to the baseline.

## 8. Verification

- [x] 8.1 `openspec validate route-engine-memo-and-cache`.
- [x] 8.2 `go build ./... && go vet ./...`, `go vet -tags oracle ./pkg/route`, `go test -race -count=1` on `pkg/route/...`, `pkg/build`, `cmd/kube-state-graph`, `internal/api`; `GOTOOLCHAIN=go1.27.1 go build ./pkg/...`; `gofmt -l .` empty; `make check-route-containment`.
