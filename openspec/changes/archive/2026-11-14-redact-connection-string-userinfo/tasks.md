# Tasks

## 1. Strip userinfo from connection-string external nodes

- [x] 1.1 Write failing tests in `pkg/build/servicegraph_userinfo_test.go`. `TestStripConnStringUserinfo` covers user+password, username only, empty username, jdbc double scheme, multi-host, an unescaped `@` in the password, a password `url.Parse` rejects, authority ending at `?` / `#`, a nested `"://"`, and `@` in path / query / fragment kept. A no-alloc test covers labels without userinfo. `TestParseServiceGraph_ConnStringExternalStripsUserinfo` checks the node id and name on all three fallbacks, the client side, and a filtered build, and checks that a credentialled label naming a held Service still resolves. `TestParseServiceGraph_ConnStringCredentialsCollapse` checks one node and one edge. Verify they fail (RED).
- [x] 1.2 Implement `stripConnStringUserinfo` (design D1, D2) and apply it at the three `r.external(label)` calls in `resolveConnString`, never inside `r.external` and never to the label `connStringHost` reads (design D3). Verify 1.1 passes.

## 2. Strip userinfo from Debug records

- [x] 2.1 Write the failing `TestParseServiceGraph_DebugLogsNeverCarryUserinfo`. It captures Debug JSON output for the external fallback (three reasons), resolved-to-service, shadowed-by-UID on each side, client-side external, wholly-empty side, and the peer ladder, and asserts that the secret and the username appear nowhere in the output. Verify RED.
- [x] 2.2 Build `sgTrace` from the stripped labels, and strip the local label at the wholly-empty-side drop, `noteConnStringShadowed`, and the resolved-to-service line (design D4). Verify 2.1 passes.

## 3. Gate per-series Debug argument construction (audit #26, design D5)

- [x] 3.1 Commit `BenchmarkParseServiceGraph_DebugOff` / `_DebugOn` first and record the baseline.
- [x] 3.2 Write failing zero-allocation tests: with an Info-level handler, `noteExternal`, `noteConnStringShadowed` and the missing-UID fallback allocate nothing beyond resolution itself.
- [x] 3.3 Add `debugEnabled`, split `noteExternal` into `countExternal` and `logExternal`, and gate every per-series Debug site in `servicegraph.go`. Counters and the rollback journal stay unconditional. Verify 3.2 passes and the benchmark's allocations drop.

## 4. Docs and verification

- [x] 4.1 Update `.claude/rules/service-graph-resolution.md` (the external `name` drops userinfo) and add a `docs/BREAKING.md` entry.
- [x] 4.2 Verify `go test ./internal/api -run Golden` passes with `git diff --exit-code internal/api/testdata` clean, and that `openspec validate redact-connection-string-userinfo` passes.
