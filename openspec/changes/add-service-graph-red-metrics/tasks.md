## 1. Graph model (`pkg/graph`)

- [x] 1.1 Add `EdgeMetrics` struct (`Rate float64`, `ErrorRate *float64`, `P90ServerMs *float64`) and the nullable `Metrics *EdgeMetrics` field on `Edge` in `pkg/graph/edge.go`, with doc comments stating the absent-vs-zero contract (design D2).
- [x] 1.2 Add the immutable `func (e *Edge) WithMetrics(m EdgeMetrics) *Edge` returning a copy; assert in its doc comment that `ID`/`Type`/`Source`/`Target`/`Labels` are unchanged.
- [x] 1.3 Unit-test in `pkg/graph`: `WithMetrics` leaves the original untouched, produces an identical `ID`, and `NewEdge` alone yields `Metrics == nil`.

## 2. Duration quantile helper (`pkg/build`)

- [x] 2.1 Create `pkg/build/histogram.go` with a pure `classicQuantile(q float64, buckets []bucket) (float64, bool)` implementing the cumulative-`le` convention with in-bucket linear interpolation (design D5).
- [x] 2.2 Make it return `ok=false` for: fewer than two boundaries, missing `+Inf`, zero total count, non-numeric or unparsable `le`, and absent `le` label.
- [x] 2.3 Clamp to the highest finite `le` when the quantile lands in the `+Inf` bucket.
- [x] 2.4 Table-driven unit tests in `pkg/build/histogram_test.go` covering interpolation at q=0.90, the `+Inf` clamp, single-bucket input, empty input, and unsorted bucket input (result must be sort-order independent).

## 3. PromQL queries (`pkg/promql`)

- [x] 3.1 Add `QServiceGraphFailedTotal` and `QServiceGraphServerSecondsBucket` `Query` constants with doc comments explaining that the prefix is NOT applied and that both are OPTIONAL.
- [x] 3.2 Add render case: failure counter as `rate(traces_service_graph_request_failed_total{<sentinel>,<podPair>}[w])` at raw label granularity.
- [x] 3.3 Add render case: duration as `sum by (cluster, client_k8s_pod_uid, server_k8s_pod_uid, le) (rate(traces_service_graph_request_server_seconds_bucket{<sentinel>,<podPair>}[w]))`.
- [x] 3.4 Extract `serviceGraphPodPairSelector = client_k8s_pod_uid!="",server_k8s_pod_uid!=""` as a named constant next to `serviceGraphSentinelSelector`, documenting it as a request-invariant metric-selection contract that is EXACTLY equivalent to the D1 attachment rule (design D6).
- [x] 3.5 Update `serviceGraphSentinelSelector`'s doc comment: the deferred-numeric-metrics note now describes existing call sites, not future ones.
- [x] 3.6 Extend `pkg/promql/queries_test.go` with exact-string assertions for both new renders, including that `Renderer{Prefix:"o11y_"}` does not prefix them.

## 4. Reader wiring (`pkg/build/servicegraph.go`)

- [x] 4.1 Fan the three service-graph queries out under an `errgroup` in `ReadServiceGraph`; the total query's error still returns a build error, the two new ones record their error and yield a nil vector (design D3).
- [x] 4.2 Keep the route prescan and `parseWithResolver` operating on the total vector exactly as today; pass the two new vectors (and their per-query error state) into the parse.
- [x] 4.3 Emit one aggregated log line naming which optional RED query degraded and why — distinguishing query error / metric absent / no usable `le` buckets — never per edge.
- [x] 4.4 Document the assumed upstream metric-name contract and its `add_metric_suffixes` dependency in `notes.md`, and confirm the miss degrades gracefully (design Risks).
- [ ] 4.5 **Pre-deploy, not implementable here:** confirm the exported names against the target store with `group by (__name__) ({__name__=~"traces_service_graph.*"})` and record the observed names in `notes.md`. The store was unreachable from the implementation environment — see `notes.md`. A mismatch costs `p90_server_ms` silently (logged), never a failed build.

## 5. RED join and aggregation (`pkg/build`)

- [x] 5.1 Create `pkg/build/redmetrics.go` holding the join keys, the accumulator, and the attach pass; keep `servicegraph.go` free of arithmetic.
- [x] 5.2 In `parseWithResolver`'s resolution loop, record for each **in-scope** contributing series (non-empty `client_k8s_pod_uid` AND non-empty `server_k8s_pod_uid`) both `seriesKey` (full label set minus `__name__`) → `pairKey` and `redKey{cluster, clientUID, serverUID}` → `pairKey` (design D1, D4).
- [x] 5.3 Mark a pair ineligible when any contributing series is out of scope, so a mixed UID-resolved / peer-resolved pair carries no metrics object at all rather than a partially-measured one.
- [x] 5.4 Accumulate each in-scope series' total rate onto its pair as a slice of contributions (not a running sum).
- [x] 5.5 Second pass over the failure vector: look up `seriesKey`, accumulate matched failure rates onto the pair.
- [x] 5.6 Second pass over the duration vector: look up `redKey`, sum bucket counts per `le` boundary onto the pair; several `redKey`s mapping to one pair sum together.
- [x] 5.7 Attach pass: sum contributions in ascending value order, compute `error_rate` (clamped to `[0,1]`, left nil when the failure query errored), compute `p90_server_ms` via `classicQuantile(0.90, ...)` × 1000 (nil when unavailable), and call `WithMetrics` on eligible edges only (design D9).
- [x] 5.8 Verify no metrics object is attached to `res.svcEdges`, `res.routeChainEdges`, `pod-calls-service` pairs, pairs with an external/service endpoint, or pairs with a peer-resolved endpoint.

## 6. Serialisation (`pkg/cytoscape`)

- [x] 6.1 Add `EdgeMetricsDTO` (`rate` float64, `error_rate` `*float64,omitempty`, `p90_server_ms` `*float64,omitempty`) and `Metrics *EdgeMetricsDTO \`json:"metrics,omitempty"\`` on `EdgeData`.
- [x] 6.2 Populate it from `Edge.Metrics`, applying the rounding here and only here: **6 significant digits** for all three fields, implemented as `strconv.ParseFloat(strconv.FormatFloat(v, 'g', 6, 64), 64)`. Do NOT use `math.Pow`/`math.Log10`-based decimal-place rounding — it double-rounds, is off-by-one at exact powers of ten, and is not guaranteed bit-identical across platforms (design D9).
- [x] 6.3 Unit tests: an edge without metrics serialises with no `metrics` key at all; a partial metrics object omits only the missing fields; all values are JSON numbers, never strings; `labels` gains no numeric key.
- [x] 6.4 Unit test the small-value contract: a `rate` of `3.86e-7` (one request over a 30-day window) serialises as a non-zero JSON number in exponent form, and `Marshal → Unmarshal → Marshal` is byte-identical. Same for a tiny non-zero `error_rate`.

## 7. Tests — build layer

- [x] 7.1 `pkg/build` unit tests for the attachment rule, one per spec scenario: both UID-resolved real pods, synth-pod target, **peer-resolved Pod-IP target (no metrics)**, external endpoint, `pod-calls-service`, `service-selects-pod`, route-chain hop, topology edges.
- [x] 7.2 Unit tests for aggregation: two in-scope series collapsing onto one edge sum their rates; a mixed in-scope / out-of-scope pair carries no metrics; `error_rate` uses the matching series set; `p90_server_ms` derives from summed buckets across two `redKey`s, not combined quantiles.
- [x] 7.3 Determinism test: shuffle the input vectors and assert byte-identical serialised output (extend `pkg/build/determinism_test.go`).
- [x] 7.4 Degradation tests: failure query errors → `error_rate` absent (not `0`); failure query succeeds with no match → `error_rate == 0`; duration query empty → `p90_server_ms` absent; duration series carry no `le` → `p90_server_ms` absent with a distinct logged reason; both error → metrics with only `rate`; total query errors → build still fails.
- [x] 7.5 Confirm the existing service-graph test corpus still passes unchanged (edges without RED data must serialise exactly as before).
- [x] 7.6 Regression tests for the D33 interaction: the self-loop UID normalisation clears the `"://"` side's UID **after** the raw-UID in-scope test has passed, and an unresolvable `"://"` target materialises an `external` node **without** downgrading the edge type — so neither the raw UIDs nor the edge type can reject the pair alone. Assert no metrics attach when the `"://"` side resolves to an external node, and none when it resolves to a service node. Verify both tests FAIL when the resolved-endpoint-type condition is removed.

## 8. Tests — API, golden, integration

- [x] 8.1 Add an invariant check that `Metrics` never appears on an edge whose source or target is not a pod node. It MUST drive the real parse (`pkg/build`, table of series shapes incl. the D33 UID-collision cases) — a `pkg/graph` version over a synthetic graph is tautological, since it can only seed attachments that already satisfy the rule, and cannot catch a resolution-path defect.
- [x] 8.2 Regenerate goldens: `go test ./internal/api/ -update -run Golden`; review the diff so only UID-resolved pod-to-pod edges changed.
- [x] 8.3 Add a dedicated golden fixture locking the **wire shape** in one body: an edge with full RED, an edge with partial RED, a metric-less edge, and **an edge whose `rate` is small enough to serialise in exponent form** — so small values are met in a snapshot rather than in production. The fixture is hand-constructed via `WithMetrics`, so it proves serialisation only; the attachment rule is proven by 7.1 / 7.6 and by the integration suite (8.6).
- [x] 8.4 Integration test in `internal/integration`: ingest hand-crafted `traces_service_graph_request_total` + `_failed_total` + `_server_seconds_bucket` fixture series and assert the emitted `data.metrics` values end-to-end, including the p90 interpolation.
- [x] 8.5 Integration test: ingest only `_total` (no failure counter, no histogram) and assert the build succeeds with `rate`-only metrics.
- [x] 8.6 Integration test: a `server="unknown"` series whose peer address resolves to a family Pod IP produces a `pod-calls-pod` edge with **no** `data.metrics` key.

## 9. Docs and contract regeneration

- [x] 9.1 `README.md` "Service-graph metric" section: add `traces_service_graph_request_failed_total` and `traces_service_graph_request_server_seconds_bucket` rows to the existing `Metric | Used for | Labels read | Required?` table, with the exact label sets read (design D11).
- [x] 9.2 `README.md` prose: state the RED attachment rule (trace-derived, UID-resolved pod pair only), the p90/server definition and its Grafana-parity caveat (definition, not value), the absent-vs-zero `error_rate` contract, that both new metrics are optional and degrade gracefully, and that all three values are JSON numbers rounded to 6 significant digits which MAY appear in exponent form — consumers must not assume fixed-decimal rendering (`toFixed`) nor threshold small values away.
- [x] 9.3 `README.md`: note the producer-side prerequisites that determine coverage — `dimensions` must carry `client_k8s_pod_uid` / `server_k8s_pod_uid` on all three series, `add_metric_suffixes` must be on, and multi-replica collectors need trace-ID-aware routing or spans never pair.
- [x] 9.4 Update the `@`-annotations / model docs so `data.metrics` appears in the OpenAPI schema with the absent-vs-zero note for `error_rate` and the exponent-form / significant-digits note for all three fields, then run `make docs` and commit `docs/`.
- [x] 9.5 Verify `make check-docs` passes (docs-drift CI mirror).
- [x] 9.6 `CLAUDE.md`: add a compact glossary block ahead of the service-graph rules defining **trace-derived edge** / **synthesised edge**, **UID-resolved endpoint** / **peer-resolved endpoint**, **contributing series**, **RED scope**; unify the `synthesised` / `synthesized` spelling while there.
- [x] 9.7 `CLAUDE.md`: replace the "Numeric edge metrics … deferred to a future typed struct field" statement in the `labels` load-bearing rule with the new RED rule (attachment condition, three queries, sentinel + pod-pair selector reuse, graceful degradation), and add `data.metrics` to the serialiser contract bullet.
- [x] 9.8 Update the edge-type registry descriptions in `pkg/graph` if any of them claims numeric metrics are absent.

## 10. Verification

- [x] 10.1 `make build`, `make vet`, `make lint`.
- [x] 10.2 `make test` (with `-race -shuffle=on`) fully green.
- [x] 10.3 `make verify-mocks` and `make check-route-containment` still pass (no interface change expected — confirm).
- [x] 10.4 `openspec verify "add-service-graph-red-metrics"` before archiving.

## 11. Post-review hardening (from `/code-review xhigh --fix`)

- [x] 11.1 Reject non-finite values at every accumulation and attach point so one poisoned upstream sample degrades a single edge instead of making the whole response body unencodable (design D10). Regression: `TestRED_NonFiniteRateOmitsMetrics`, which also asserts the body still `json.Marshal`s.
- [x] 11.2 `accumulateFailures` returns matched / unmatched counts; a non-empty failure result that joined nothing logs once under `reason=failed_total_label_set_mismatch`. Zero failure series stays quiet — that is not the signature.
- [x] 11.3 `optionalQueryFatal(ctx, err)`: an OPTIONAL query's own error stays non-fatal, but an error raised while the CALLER's context is cancelled propagates, so a build that exceeds `--build-timeout` inside a RED query still returns 504 instead of a degraded 200. Removes the previously unreachable `g.Wait()` branch.
- [x] 11.4 Skip the RED join bookkeeping when its input is absent: `seriesKeyOf` / `redKey` are computed only when the corresponding join is active, `newRedJoin` allocates only the maps it needs, per-pair bucket maps are lazy.
- [x] 11.5 `parseLe` rejects `NaN` explicitly (a `NaN` map key can never be looked up again, so it would grow `buckets` without bound).
- [x] 11.6 Bucket counts accumulate as slices summed via `sumAscending` at attach, matching the order-free contract already applied to rates and failures (design D9).
- [x] 11.7 Incidental (pre-existing, both indexes): key the Pod-IP **and** Service-`ClusterIP` reverse indexes on `net.IP.String()`, and canonicalise the peer host the same way before lookup, so an IPv6 address written in a different textual form than the exporter's label still matches. No spec change — the promoted `resolve-unknown-server-ip-peer` / `-pod-ip-peer` semantics are unaltered; this only makes the lookup work as those specs already describe.
- [x] 11.8 `make lint` clean (the review pass introduced a `prealloc` violation in `redmetrics_test.go`; `go build` / `go vet` / `go test` alone do not catch it).
