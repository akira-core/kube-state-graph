# Tasks

## 1. Selector value cap and request-line bound (C1)

- [ ] 1.1 Write failing `pkg/kubegraph` table tests: for each of `ParseValues`, `ParseStorageValues`, `ParseK8sValues` and each of `cluster` / `namespace` / `az` / `env`, 100 distinct values pass, 101 distinct values are `invalid_scope` naming the parameter, 101 values with 100 distinct pass, and a missing required parameter still wins. Verify RED.
- [ ] 1.2 Implement the distinct-value cap (design D1). Verify 1.1 passes.
- [ ] 1.3 Add an `internal/api` component test: 101 distinct `namespace` values on each endpoint return 400 `invalid_scope` and the mock querier is never called.
- [ ] 1.4 Add `api.MaxHeaderBytes` and `api.NewHTTPServer`; switch `cmd/kube-state-graph` to it. Add a test serving the production `http.Server` on a real listener: the largest legal request is not 431, an over-bound request line is 431 (design D2). Record the measured size in design D2.
- [ ] 1.5 Truncate the access log's `clusters` field; test with a captured slog handler.

## 2. Pod root namespace bound (P-7)

- [ ] 2.1 Write a failing `pkg/build` test: one pod name across 1601 namespaces is `ReasonInvalidScope` with no query issued, on `BuildStorage` and `BuildK8s`. Verify RED.
- [ ] 2.2 Add the `ChunkNamePairs` count to `preparePodSeed` (design D3). Verify 2.1 passes and `storage_fanout.golden` and the k8s round tests are unmodified and green.

## 3. Admission control (C3)

- [ ] 3.1 Add `MaxInflightBuilds` (flag, env, default 32, validation) to `internal/config` with tests.
- [ ] 3.2 Add `build.ReasonOverloaded`, its `mapBuildError` entry (503, `Retry-After: 1`), the two self-metrics, and the middleware (design D4).
- [ ] 3.3 Component tests: with a blocking querier and bound N, request N+1 is 503 `overloaded` with `Retry-After: 1` and issues no query; the gauge reads N while held and 0 after; the counter counts the rejection; `/livez`, `/readyz`, `/metrics`, `/docs`, `/openapi.json`, `/openapi.yaml` are served while full; `0` disables; a 401 still wins.
- [ ] 3.4 Swag `@Failure 503` on the three graph routes; `make docs`. `docs/BREAKING.md` entry.

## 4. Optional window cap (P-5)

- [ ] 4.1 Add `MaxWindow` to `internal/config` with tests.
- [ ] 4.2 Enforce in the three handlers after parsing (design D5); component tests on all three endpoints (over → `invalid_range`, equal → 200, a parse error wins).
- [ ] 4.3 Swag descriptions; README flag rows and the VictoriaMetrics precondition.

## 5. Stage deadlines (#41)

- [ ] 5.1 Hoist the build-timeout context into `serveGraph`; check it before project, serialise and encode (design D6).
- [ ] 5.2 In-package tests: a `run` that cancels the request context, and one that outlives the deadline, both return a graph; assert the projection is never invoked and the status is 499 / 504.

## 6. Smaller bounds (#2, #4, #12, #6, #14)

- [ ] 6.1 `pkg/build`: drop non-finite Harvest / kubelet samples (design D7); unit tests where a NaN read first would shadow a finite value; an `internal/api` test with a NaN usage sample returning 200.
- [ ] 6.2 `X-Request-ID` sanitising (design D8); boundary tests (128 / 129 bytes, bad charset, header echo).
- [ ] 6.3 Bounded echo in `parseTimestamp` / `parsePrune` (design D8); tests for byte-identical short messages and no echo of a long value.
- [ ] 6.4 `/docs` hash-based `script-src` (design D9); test that the header's hash matches the served inline script and `'unsafe-inline'` is gone from `script-src`.
- [ ] 6.5 Document the embedder's lack of a default build deadline on `kubegraph.Engine` and in the README.

## 7. Documentation and verification

- [ ] 7.1 Update `.claude/rules/upstream-queries.md` (no window cap by default; request matchers capped at 100 distinct values; pod root namespace side bounded by the cap) and `.claude/rules/storage-graph.md`.
- [ ] 7.2 `openspec validate bound-request-admission`; `go build ./... && go vet ./...`; `go test -race` on every touched package; `GOTOOLCHAIN=go1.27.1 go build ./pkg/...`; `gofmt -l .` empty; goldens unchanged.
