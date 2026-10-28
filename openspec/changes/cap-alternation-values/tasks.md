# Tasks

## 1. Bound the literal chunkers in `pkg/promql`

- [x] 1.1 Write failing tests first.
  - `pkg/promql/scope_test.go`: `ChunkScope` and `ChunkScopeWithOverhead` never return a chunk of more than `MaxAlternationValues` values, whether 101, 250 or 1000 short values that fit one byte chunk. They do so with `budget <= 0` too. Exactly 100 values stay one chunk. The split is identical across two calls and preserves input order.
  - `pkg/promql/qosscope_test.go` and `pkg/promql/harvestpair_test.go`: the same bound for `ChunkQoSVolumeScope` and per cluster for `ChunkHarvestPairs`, including its `budget <= 0` path.
  - Verify with `go test ./pkg/promql/ -run 'Chunk' -count=1`, which fails.
- [x] 1.2 Add `MaxAlternationValues = 100` to `pkg/promql/scope.go`. Its doc comment names VictoriaMetrics `lib/regexutil/regexutil.go` `maxOrValues`, the `len > maxOrValues` boundary, and the versions it was checked at (`master`, `v1.107.0`).
- [x] 1.3 Move the split into an unexported core, `chunkScope(values, budget, overhead, maxValues)`, where `maxValues <= 0` means unbounded. `ChunkScope` and `ChunkScopeWithOverhead` pass `MaxAlternationValues`. Update their doc comments and the `budget <= 0` sentence on `ChunkHarvestPairs`. Verify that the 1.1 tests pass.
- [x] 1.4 Add a test pinning `MaxAlternationValues == 100`, so that changing it is a deliberate edit. Verify with `go test ./pkg/promql/ -count=1 -race`.

## 2. Exclude suffix tokens and bound the k8s pair chunker

- [x] 2.1 Write failing tests first.
  - `pkg/promql/scope_test.go`: a new `ChunkSuffixTokenScope(tokens, budget)` splits on bytes alone. 250 short tokens that fit the budget stay one chunk, and `VolumeTokenBranchOverhead` is charged per token.
  - `pkg/promql/pairscope_test.go`: `ChunkNamePairs` closes a chunk when distinct names or distinct namespaces would exceed 100. 150 pods in one namespace give 2 chunks, with names 100 + 50 and namespaces 1 + 1. 120 pairs across 120 namespaces give a split with no namespace alternation over 100. 100 names in 3 namespaces stay one chunk.
  - Verify that `go test ./pkg/promql/ -run 'SuffixToken|ChunkNamePairs' -count=1` fails.
- [x] 2.2 Implement `ChunkSuffixTokenScope` on the core with `maxValues = 0`. Its doc comment explains why: VictoriaMetrics extracts no or-values from `.*`-prefixed branches. Switch `pkg/build/volumelabelscope.go`'s token read to it. Verify with `go test ./pkg/build/ -run 'VolumeLabel' -count=1`.
- [x] 2.3 Add the per-side value bound to `ChunkNamePairs` beside its byte accounting. Update its doc comment. Verify that the 2.1 tests pass.

## 3. Audit build-level tests and pin the k8s behaviour

- [x] 3.1 Add `pkg/build` tests:
  - **Node with 150 pods**: a `BuildK8s` node root whose seed returns 150 pods in 3 namespaces issues incarnation completion, bindings and `kube_pod_owner` as 2 queries each. No issued query's `pod=~` or `namespace=~` alternation carries more than 100 values. The body equals the one-chunk body, and the round count is unchanged.
  - **1601 node roots**: a request with 1601 short `node=` roots is `ReasonInvalidScope` before any query.
  - Put the tests beside `build_k8s_fanout_test.go` / `nodeseed_test.go` and use the mockery `MockQuerier` to capture the rendered queries.
- [x] 3.2 Run `go test ./pkg/build/ -count=1 -race` and triage every failure in the root-cap and fan-out tests: `nodeseed_test.go`, `podseed_test.go`, `claimseed_test.go`, `appscope_test.go`, `qualifiedroots_test.go`, `flowless_test.go`, `build_k8s_test.go`, `build_k8s_fanout_test.go`, `build_storage_plan_test.go`.
  - Change an expectation only where the fixture puts more than 100 values in one alternation.
  - Note each changed expectation in the commit message.
  - Refresh `pkg/build/testdata/storage_fanout.golden` with `-update-storage-fanout` only if its fixture crosses the bound.
- [x] 3.3 Run `go test ./internal/api/ -count=1` and check that goldens are untouched (`git status internal/api/testdata` clean). Bodies must not move.

## 4. Documentation

- [x] 4.1 Update the docs. Verify with `make check-docs`, plus a grep that no doc still says chunks are bounded by bytes alone.
  - `.claude/rules/upstream-queries.md`: the chunking rule, the value bound, and the suffix-token exception.
  - `.claude/rules/storage-graph.md`: the grouped pair reads are bounded per side.
  - `.claude/rules/netapp-storage-join.md`: the QoS chunking sentence.
  - `docs/upstream-metrics.md` and `docs/netapp-harvest-preconditions.md`, wherever they describe the 8192-byte split.
  - The `--netapp-qos-scope-batch-bytes` flag help in `internal/config/config.go`: mention the 100-value bound.
- [x] 4.2 Add a `docs/BREAKING.md` entry for D6. A root set of one kind beyond about 1600 short values is now 400 `invalid_scope`. Verify that the entry exists and links the change name.

## 5. Full verification

- [x] 5.1 Run `make ci` (lint, vuln, test, check-docs, verify-mocks, containment checks). It must be green.
- [x] 5.2 Optional manual check against a real VictoriaMetrics, such as the integration container or a staging store. Issue one captured k8s-graph query with 100 and with 101 pod names and record both latencies in the PR description, as evidence for the threshold. If no store is reachable, mark this as skipped.

## Workflow follow-up

- After merge: `/opsx:archive cap-alternation-values`.
- Follow-ups from proposal.md "Non-goals and follow-ups" (seed lookback, cache-stable chunk boundaries, per-round span, concurrency tuning) are separate changes.
