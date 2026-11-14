# Tasks

## 1. Baselines

- [x] 1.1 Add `BenchmarkMergeVectors` (1 / 2 parts; 10k / 100k series; 12 / 30 labels) and `BenchmarkQueryCache_PutGet` (256 B / 16 KiB queries) in `pkg/promql/bench_test.go`. Record `-benchmem -count=5` before any production change.

## 2. Fan-out merge (C5)

- [x] 2.1 Tests:
  - a single non-empty part, including one beside empty siblings, is returned as the same backing array;
  - a nil sample is dropped into a new vector and the input is untouched;
  - only nils give `model.Vector{}`;
  - a hash collision between distinct label sets keeps both, through `seenIndex`.
- [x] 2.2 Implement the fast path, `withoutNilSamples`, and the `FastFingerprint` + `Metric.Equal` index. Re-run the benchmark.

## 3. Query-result cache (C7)

- [ ] 3.1 Tests:
  - the key is a digest, and distinct queries get distinct entries;
  - a long query's cost is charged per KiB, and the series gauge stays series;
  - `put` sweeps expired tail entries without counting evictions;
  - the sweep keeps live entries.
- [ ] 3.2 Make `cacheKey` a digest, charge query length, track `series` beside `size`, and add `sweepExpiredLocked`. Key in-flight coalescing by the digest. Re-run the benchmark.

## 4. Coalescing outlives its leader (P-3)

- [ ] 4.1 Tests:
  - a cancelled starting caller does not fail a live waiter, and no second fetch happens;
  - every waiter leaving cancels the shared fetch;
  - the shared fetch's deadline is the latest waiter's;
  - a waiter with no deadline makes it unbounded;
  - a deadline-cut flight surfaces the waiter's own `DeadlineExceeded`.
- [ ] 4.2 Replace `singleflight` in the guard with a refcounted flight map whose context is `WithoutCancel(leader)` and whose `Deadline()` reports the latest waiter deadline.

## 5. Evaluation timeout (P-4)

- [ ] 5.1 Tests:
  - 14.9 s left gives `timeout=14s`;
  - 1.2 s left gives no parameter;
  - no deadline gives no parameter;
  - a coalesced fetch sends the latest waiter's remaining time.
- [ ] 5.2 Pass `v1.WithTimeout` from `Client.Instant`.

## 6. Readiness probe (P-1)

- [ ] 6.1 Render `QUpProbe` as `count(up)`. Update `pkg/promql/testdata/render-baseline.txt`. `Engine.Probe` renders it rather than using the query name.
- [ ] 6.2 Coalesce `Router.ProbeAll` with `singleflight` and a 1 s outcome TTL that excludes deadline-cut outcomes. Tests:
  - concurrent probes issue one query per backend;
  - a second probe within 1 s is served, and one after 1 s is live;
  - a timed-out probe is not reused.

## 7. Client hardening

- [ ] 7.1 C1 item 4: truncate the span `db.statement` and the log `query` to 2 KiB on a rune boundary, and add the byte length and a 12-hex SHA-256 prefix. Tests use an in-memory exporter and a recording slog handler.
- [ ] 7.2 #9: add a response-size cap transport (`DefaultMaxResponseBytes`, `WithMaxResponseBytes`, `ErrResponseTooLarge`), and `--upstream-max-response-bytes` / `KSG_UPSTREAM_MAX_RESPONSE_BYTES` through `DefaultClientFactory(m, opts...)`. Tests cover the declared-length refusal, the streamed overrun, a body exactly at the cap, a disabled cap, and config parsing and validation.
- [ ] 7.3 #10: attach basic auth only on the same scheme and host. Test the `https`→`http` same-host case.

## 8. Rendering (#11, #13)

- [ ] 8.1 Use a package-level `strings.Replacer` that escapes `\`, `"`, `\n`, `\r`, `\t`, and other C0 / DEL bytes as `\xHH`. Correct the comment. Tests use a table, and the output round-trips through `strconv.Unquote`.

## 9. Reload (#7, O3)

- [ ] 9.1 Add `backendsfile.ReadWithContent` and `ReloaderOptions.Content`. `config.ReadBackendsFile` returns the content and the server passes it. Tests: a file changed between the startup parse and reloader construction is applied on the first tick, and with no `Content` the first tick swaps once.
- [ ] 9.2 `observability.Metrics.SetBackends` deletes the in-flight and slot-wait series of removed backends and ignores later observations for them. Tests cover removal, a kept backend's in-flight value surviving, and no resurrection at −1.

## 10. Documentation

- [ ] 10.1 Update `.claude/rules/upstream-queries.md`:
  - cache key and cost, the sweep;
  - coalescing (remove "retries once as leader");
  - the evaluation timeout;
  - the probe;
  - the response cap.
- [ ] 10.2 Update `docs/upstream-backend-routing.md`. Note `--upstream-max-response-bytes` in `docs/BREAKING.md`.
- [ ] 10.3 Run `openspec validate harden-upstream-guard`.
