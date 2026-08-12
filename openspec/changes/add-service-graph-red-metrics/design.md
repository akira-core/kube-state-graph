## Context

See `proposal.md` — Why. Design-level state that shapes the approach:

- `ReadServiceGraph` issues exactly **one** PromQL query today (`rate(traces_service_graph_request_total{<sentinel>}[w])`), then optionally runs the pure route prescan, then calls `parseWithResolver`. All resolution is a pure function of `(vec, topology, routes)`; all I/O happens before the parse (D2 of `translate-global-fqdn-to-k8s-service`).
- `parseWithResolver` walks the vector once, resolves each side to node IDs, and accumulates a `pairs map[pairKey]aggEdge` keyed by `(srcID, tgtID)`. **N upstream series legitimately collapse onto one pair** — the existing `betterSrcCluster` tie-break exists precisely because of this.
- `graph.Edge` is a plain struct whose `ID` is `UUIDv5(edgeNamespace, "<type>|<source>|<target>")`. Adding a field cannot change any existing edge ID. Projection (`filterEdges`, `readdEdgePartners`) and `NewGraph` pass `*Edge` pointers through without copying, so a new field survives pruning, filtering, and traversal untouched.
- `Edge.Labels` is strict `map[string]string`; the promoted `graph-api` spec explicitly forbids numbers-as-strings there, and the promoted `pod-service-graph` spec's *Numeric metrics deferred from v1* requirement is the thing this change retires.
- The promoted *Pod-UID-resolved edge source* requirement **already declares** `traces_service_graph_request_failed_total` and `..._server_seconds_bucket` as series the reader SHALL consume, and `promql.serviceGraphSentinelSelector`'s doc comment **already mandates** that deferred numeric queries reuse the sentinel fragment. This change implements a contract that was written down and then not built.
- Golden tests demand byte-identical bodies for identical upstream data, so any float that reaches the wire must be reproducible bit-for-bit.

**Producer facts established during design** (they drive D5 and D7):

- The intended producer is the OTel-collector `servicegraph` connector. Its metric names match Tempo's (`traces_service_graph_request_total` / `_failed_total` / `_server_seconds` / `_client_seconds` / `_unpaired_spans_total` / `_dropped_spans_total`).
- Its default `latency_histogram_buckets` are `[2ms, 4ms, 6ms, 8ms, 10ms, 50ms, 100ms, 200ms, 400ms, 800ms, 1s, 1.4s, 2s, 5s, 10s, 15s]` — 16 boundaries with a 2 ms floor. Tempo's metrics-generator `service_graphs` default is far coarser: `[0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 6.4, 12.8]` seconds.
- Grafana's documented service-graph queries are `sum(rate(..._server_seconds_count[...])) by (client, server)` for rate and `histogram_quantile(.9, sum(rate(..._server_seconds_bucket[...])) by (server, le))` for latency — **server-observed, p90**.
- The connector's `store.ttl` defaults to 2 s and `store.max_items` to 1000; unpaired spans become `unpaired_spans_total` plus virtual-node edges. Its `virtual_node_peer_attributes` defaults to `[peer.service, db.name, db.system]`, so an unpaired peer is labelled from those attributes when present and only falls back to `unknown` otherwise.

## Goals / Non-Goals

**Goals:**

- Surface Rate / Errors / Duration on exactly the edges where the (series → edge) mapping is unambiguous and fully measured, with no invented numbers anywhere else.
- Match Grafana's service-graph **definition** for Duration (server-observed, p90) so the two tools measure the same quantity.
- Keep the two new upstream reads cheap enough to run unconditionally, and harmless enough to fail without consequence.
- Preserve every existing invariant: edge IDs, `labels` strictness, determinism, "no caller filters pushed to PromQL", `pkg/` importability, no new dependency.

**Non-Goals:**

- **No client-side duration.** `traces_service_graph_request_client_seconds_*` is not read. Adding a `p90_client_ms` later is a purely additive field.
- **No mean/average.** `_server_seconds_sum` / `_count` are not read. See D7 for why the coarse-bucket argument that would have justified an exact mean does not apply to the intended producer.
- **No native-histogram / `vmrange` bucket support.** Only classic cumulative `le` buckets are understood; anything else degrades to "no `p90_server_ms`".
- **No extra quantiles.** `p50` / `p95` / `p99` are not emitted.
- **No RED on peer-resolved endpoints**, including the Pod-IP branch. See D1.
- **No RED on non-pod-to-pod edges**, including `pod-calls-service`. See D1.
- **No span-metrics integration.** `spanmetrics` is per-span and carries no client/server pair, so it cannot produce edge metrics at all. Node-level RED from span metrics is a separate change, and joining it to pod nodes would key cardinality on `k8s.pod.uid × span.name × status.code`, which churns on every deploy.
- **No operator knob**, no feature flag. No caching, no change to windowing, no change to the route engine.

## Decisions

### D1 — Attachment rule: trace-derived `pod-calls-pod` with two **UID-resolved** pod endpoints

An edge gets `metrics` iff `type == pod-calls-pod`, it came out of the `pairs` map (at least one series produced it), its contributing series carried a non-empty `client_k8s_pod_uid` **and** a non-empty `server_k8s_pod_uid`, **and both resolved endpoint ids name `type="pod"` nodes**.

*Why the last condition is not redundant with the other two.* It looks like it should be: a non-empty client UID always makes `resolveClient` return a pod, and a service target downgrades the edge type. Both inferences break on the **D33 self-loop UID normalisation**, which clears the `"://"` side's UID when the two UIDs are non-empty and equal:

- the raw-UID test is evaluated **before** normalisation (it has to be — it is what makes the query-layer pod-pair selector exactly equivalent), so it still reports "both UIDs present" for a series whose `"://"` side is about to be cleared;
- the cleared side then resolves through the connection-string path. If the URL is **unresolvable** it becomes an `external` node, and an external target does **not** change the edge type — only a *service* target does. So the pair survives as `pod-calls-pod` with an `external` endpoint.

`pkg/graph/registry.go` already records this shape: `pod-calls-pod` declares `SourceType: [Pod, Service, External]` and `TargetType: [Pod, External]`. The endpoint-type check (`sgResolver.isPodID`, covering `podByID` and `synthPods`) is therefore the only condition that actually enforces "both endpoints are pods". Regression coverage: `TestRED_D33ClearedUIDDoesNotAttachToExternal`, `TestRED_D33ClearedUIDDoesNotAttachToService`, and the parse-driven `TestRED_InvariantMetricsOnlyOnPodPairs`.

*Why the mapping must be total:* only in that shape does a set of upstream series map to one edge without aggregation that the response cannot express. Excluded shapes break it in different ways:

| Excluded shape | Why a number there would be fabricated |
|---|---|
| `service-selects-pod` | Synthesised fan-out. No series names the individual backing pod; splitting the service's traffic across N endpoints would be an invention. |
| `pod-calls-service` (D29 connection string) | The service node aggregates every caller-visible destination behind one identity, and the same series also spawns the fan-out — attributing the caller's rate to the service edge double-counts against the fan-out story. |
| `pod-calls-service` (route-resolved / ingress-chain hop) | The synthesised gateway-pod → backend hop has no series at all. |
| `pod-calls-pod` with an `external` endpoint | The external node collapses *all* traffic to a given label string, and `external/` ids are not cluster-scoped. |
| `pod-calls-pod` with a **peer-resolved** endpoint | See below. |
| `pod-mounts-pvc` / `pod-to-node` / `pvc-to-storageclass` | Topology-derived; no trace series exists. |

*Why peer-resolved endpoints are excluded even though they are pods:* the `server="unknown"` peer-address ladder — including the Pod-IP branch that resolves straight to a topology pod — runs **only because the connector could not pair a server span**. A peer-resolved endpoint is by construction an endpoint whose own side emitted no trace within `store.ttl`. The RED series therefore hold no measurement for it: the connector saw one half of the call. When both pods do emit traces the pairing succeeds, `server_k8s_pod_uid` is populated, and the edge collects RED through the ordinary path. So the exclusion does not lose "edges that had data" — it declines to dress a half-observed call as a fully measured one.

*Consequence, deliberately taken:* the query-layer matchers in D6 make the queried series population **exactly equal** to this rule. There is no gap in either direction, so no qualifying edge can be silently uncovered and no uncovered edge can be silently reported as `error_rate: 0`.

*Synthesised pods DO qualify* (non-empty UID unknown to topology). They are UID-resolved `type="pod"` nodes; excluding them would make an edge's metrics blink in and out as topology scrape coverage changes.

### D2 — Typed nullable field on `graph.Edge`, constructed immutably

```go
type EdgeMetrics struct {
    Rate        float64   // req/s, always > 0 when the struct is present
    ErrorRate   *float64  // nil = failure counter unreadable; 0 = read, no failures
    P90ServerMs *float64  // nil = no usable classic histogram
}

func (e *Edge) WithMetrics(m EdgeMetrics) *Edge  // returns a copy; ID unchanged
```

*Why pointers for two of three:* the spec distinguishes "absent" from `0` for `error_rate` (an unreadable counter must not read as a healthy edge) and needs `p90_server_ms` absent rather than `0`. `encoding/json`'s `omitempty` cannot distinguish `0` from unset on a bare `float64`. `Rate` is non-optional and always `> 0`, so it stays a plain `float64`.

*Why `WithMetrics` rather than a mutable setter or a wider `NewEdge`:* the repo's immutability rule, and `NewEdge` has ~8 call sites that must keep producing metric-less edges. `WithMetrics` returns a new `*Edge` sharing the same derived ID, so edge identity is provably unaffected.

*Alternative considered:* a parallel `map[edgeID]EdgeMetrics` on `ServiceGraphResult`, threaded to the serialiser. Rejected: it would have to survive `graph.NewGraph`, `graph.Project`, and the `pkg/kubegraph` facade as a second channel, and every one of those is a place for the map and the edge set to drift.

### D3 — Three parallel queries; the two new ones are non-fatal

`ReadServiceGraph` fans the three reads out under an `errgroup` (the topology reader already uses this shape), then runs the existing prescan and parse on the total vector. The total query keeps today's semantics — its error fails the build. The two new queries record their error, log it once with the reason, and yield a nil result.

*Why non-fatal:* the failure counter and the histogram are producer-configurable, and their exported names depend on the collector's Prometheus exporter suffix handling (see Risks). Making a decorative metric able to 500 the graph endpoint would be a strict regression.

### D4 — Asymmetric join: failures by exact series identity, histogram by pair identity

- **Failures**: queried **raw** (same label granularity as the total), joined to a contributing series by that series' full label set minus `__name__`. During the resolution loop the parse records `seriesKey → pairKey`; a second pass over the failure vector looks each series up and adds its rate to the pair's error numerator.
- **Histogram**: queried **pre-aggregated** as `sum by (cluster, client_k8s_pod_uid, server_k8s_pod_uid, le) (rate(..._bucket{...}[w]))`, joined to a pair by the `(cluster, client_k8s_pod_uid, server_k8s_pod_uid)` triple, which the parse records alongside the pair (`redKey → pairKey`, many-to-one).

*Why not symmetric?* Two different pressures:

- Exactness matters most for `error_rate`, because numerator and denominator must come from the **same** series set or the ratio is meaningless. Identity-joining the raw failure vector guarantees that. Its cardinality is bounded by the total vector's, which is already on the wire.
- Cardinality matters most for the histogram: raw buckets are `edge-cardinality × 16`, and the dimension set includes high-cardinality peer labels (`client_dns_answers`, `client_server_address`). Pre-aggregating upstream collapses that to `pod-pair × 16` before it crosses the wire, and bucket sums are exactly what the quantile needs anyway (D5).

*Why the parse records the join keys instead of a second resolution pass:* re-deriving `(srcID, tgtID)` from the triple would duplicate `resolveClient`'s cluster-recovery fallbacks (`podByID` → `podByUID`) and the D33 self-loop normalisation. Recording the mapping the resolver actually produced makes drift impossible by construction.

*Why `cluster` stays in the histogram group-by:* dropping it would merge trace-cluster variants upstream and give one bucket set per pod pair, but it would newly depend on pod UIDs being globally unique for the **client** side, where `resolveClient` treats `(cluster, uid)` as the primary key. Keeping `cluster` in the key and summing the variants' buckets in Go (D5) costs one map merge and adds no assumption.

### D5 — `p90_server_ms` computed in-process from summed classic buckets

A new pure helper (`pkg/build/histogram.go`) implements the classic cumulative-bucket quantile: sort by `le`, find the bucket containing `0.90 × count`, linearly interpolate inside it, clamp to the highest finite bound when the quantile lands in `+Inf`. Result × 1000 → milliseconds.

*Why not `histogram_quantile(0.9, ...)` in PromQL:* several `(cluster, clientUID, serverUID)` triples can map to one edge (a series whose `cluster` label is missing bucketed to `"unknown"` alongside a sibling carrying the real cluster, both recovering the same client pod via `podByUID`). Quantiles are not additive — averaging or maxing per-triple p90s is wrong in a way that is invisible in the output. Bucket **counts** are additive, so summing buckets first and computing one quantile is the only correct order.

*Why p90 and not p99:* Grafana's documented service-graph latency query uses `.9`, and matching the definition of the tool operators already use is worth more than a rounder-sounding number. The docs note the quantile is adjustable, so p90 is the *default presentation*, which is what parity means here.

*Degradation:* fewer than two distinct `le` boundaries, no `+Inf` bucket, a non-numeric `le`, or an absent `le` label (the native-histogram / `vmrange` case) all yield "no `p90_server_ms`" for the affected pair rather than a guess.

### D6 — The two new selectors carry request-invariant matchers

Both new selectors are `{<serviceGraphSentinelSelector>, client_k8s_pod_uid!="", server_k8s_pod_uid!=""}`.

- The sentinel fragment is reused verbatim from `promql.serviceGraphSentinelSelector`, as its own doc comment requires, so the three series always describe one edge population.
- The UID-non-empty matchers are D1's attachment rule pushed to the query layer. This is the **same class** as the D30 sentinel and the `condition="Ready"` node selector: a fixed metric-selection contract that never varies per request, not a caller filter. The "no filters pushed to PromQL" rule is preserved, and a future cache serving any filter from one built graph is unaffected.

Because D1 now requires UID resolution on both sides, the matchers are **exact**, not a superset-safe prefilter: `client_k8s_pod_uid!=""` is equivalent to "the client side is a pod" (`resolveClient` returns `isPod` only for a non-empty UID), and `server_k8s_pod_uid!=""` is equivalent to "the server side is UID-resolved". That equivalence is what makes the "no silent gap" property in D1 provable rather than argued.

The configurable metric prefix is not applied (different exporter family — design D26).

### D7 — No mean, no `_sum`/`_count`

An exact pooled mean (`sum(_sum)/sum(_count)`) was considered as a bucket-independent companion to the quantile. It was rejected once the producer facts landed.

*Why it was tempting:* with Tempo's `service_graphs` defaults the lowest bucket boundary is 100 ms, so a service whose real p90 is 25 ms has every sample in bucket 0 and `histogram_quantile` interpolates from 0 to 0.1 s — reporting ~90 ms, a systematic ~3.6× overstatement for **every healthy fast service**. A mean would be exact there, and would also survive native histograms (`_sum`/`_count` exist when `_bucket` does not).

*Why it was rejected:* the intended producer is the OTel `servicegraph` connector, whose default buckets start at 2 ms with 16 boundaries. A 25 ms p90 lands in `[10ms, 50ms]`, where interpolation error is a few milliseconds. The systematic bias that justified the mean does not exist for this producer, and the cost — one more query, one more permanent v1 field, one more thing to explain — does not buy anything else.

*If the producer ever changes to Tempo's generator with default buckets*, the correct response is to set `histogram_buckets` on the producer, or to add `avg_server_ms` as a purely additive field. Recorded in Risks.

### D8 — No knob

Consistent with D29 / D30 / D33 and the rest of the reader, RED is hardcoded on. The escape hatch operators need is "my producer doesn't emit these", and D3's graceful degradation covers that automatically. If per-request cost becomes a problem, the right lever is a cache (already anticipated), not a flag that makes the response shape depend on deployment config.

### D9 — Serialisation and rounding

`pkg/cytoscape.EdgeData` gains `Metrics *EdgeMetricsDTO \`json:"metrics,omitempty"\`` with `rate float64` / `error_rate *float64,omitempty` / `p90_server_ms *float64,omitempty`. The DTO is populated from `Edge.Metrics` with rounding applied **at serialisation time only**, keeping `pkg/graph` values un-rounded and the rounding policy in exactly one place.

Floating-point addition is not associative and PromQL result ordering is not a contract, so contributions to a pair are accumulated into a slice and summed in **ascending value order**, making each sum a pure function of the multiset. Rounding then makes the bytes stable even if a future refactor perturbs summation order. `error_rate` is additionally clamped to `[0, 1]` (defensive: a producer restart mid-window can transiently make one counter's rate exceed another's).

**Rounding is to 6 significant digits, not to a fixed number of decimal places, and the same rule applies to all three fields.** A fixed decimal precision breaks on the small end, and the break is not hypothetical: `rate` is req/s, so its magnitude scales inversely with the caller's window. One request in a 30-day window is `3.86e-7` req/s, which rounds to `0` at 6 dp — contradicting this spec's own "`rate` is strictly greater than zero by construction" and presenting an edge that exists as carrying no traffic. `error_rate` fails the same way for a rarer reason but a worse consequence: one failure on a 5000 req/s edge is `≈6.7e-8`, and rounding that to `0` collides with the value's *defined* meaning ("read successfully, no failures"), reintroducing exactly the absent-vs-zero conflation D1 was built to prevent. Significant digits make the error relative instead of absolute, so a non-zero value can never become `0`, at any window length or traffic level.

*Implementation pin:* the rounding MUST go through decimal formatting, not floating-point exponentiation:

```go
func round6(v float64) float64 {
    r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 6, 64), 64)
    return r
}
```

The obvious `math.Round(v*math.Pow(10, k)) / math.Pow(10, k)` form (with `k` derived from `math.Log10`) is wrong three ways: `Log10` can be off by one at exact powers of ten, `Pow`'s result is itself inexact, and the multiply/divide double-rounds. `FormatFloat` with `'g'` and precision 6 performs correct decimal rounding in pure Go with no libm dependency, so it is deterministic across platforms — which the golden tests require.

*Wire format, verified rather than assumed:* `encoding/json` formats a `float64` with `strconv.AppendFloat(..., -1, 64)` (shortest round-trip), selecting `'e'` only when `abs < 1e-6` or `abs >= 1e21`, then normalises `e-09` to `e-9`. So `0.000278` serialises as `0.000278` and `3.86e-7` serialises as `3.86e-7`. Three consequences:

1. Exponent notation is legal JSON (RFC 8259) and every conformant parser accepts it.
2. The float64 → JSON → float64 round-trip is lossless (shortest-round-trip guarantees it), so a cached or re-parsed body never drifts, and the shortest representation of `ParseFloat` applied to a 6-significant-digit decimal is that same decimal — the two mechanisms compose without a second rounding.
3. Go's `1e-6` switchover matches JavaScript's `Number.prototype.toString`, so a `JSON.parse` → `JSON.stringify` round-trip in a browser consumer reproduces the same text.

The remaining hazard is on the consumer side and is a documentation obligation, not an encoding one: `toFixed(3)` renders `3.86e-7` as `"0.000"`, and a threshold like `rate > 0.001` silently drops long-window edges. README and the OpenAPI description must state that all three values are JSON numbers that may appear in exponent form, and that `0` is semantically distinct from a very small non-zero value.

`graph.Project` / `filterEdges` / `readdEdgePartners` pass `*Edge` pointers through unchanged, so projection, pruning, and traversal need no edits.

### D10 — Non-finite samples degrade one edge, never the request

`NaN` and `+Inf` are representable in Prometheus exposition and **not** representable in JSON, so `encoding/json` refuses to marshal them. Without a guard, one poisoned upstream sample would fail `writeJSON` for the entire `/v1/graph` body — a 500 for the whole graph caused by a single series. The existing parse-loop guard does not catch it: it is written `if !(s.Value > 0) { continue }` (deliberately, so `NaN` is dropped), and `+Inf > 0` is true.

The guard is therefore applied at three points, all biased the same way — **drop the measurement, keep the graph**:

1. accumulation skips non-finite contributions on both the failure and duration vectors;
2. `attachMetrics` omits the whole metrics object when the aggregated `rate` is not a finite positive number;
3. `p90_server_ms` is omitted when the computed quantile is not finite.

`error_rate` folds `NaN` into the existing `[0, 1]` clamp (a `NaN` numerator or denominator resolves to `0`, alongside the negative case).

Related diagnostic, same spirit: the failure join is by exact series identity (D4), so a label-set divergence between `_total` and `_failed_total` — a relabel applied to one and not the other, a dimension added on one side — makes **every** edge report `error_rate: 0`, which is indistinguishable from "no failures". `accumulateFailures` therefore returns matched / unmatched counts, and a non-empty failure result that matched nothing is logged once under its own reason. Zero failure series is *not* that signature and stays quiet.

### D11 — Documented upstream contract

`README.md`'s "Service-graph metric" section gains the two new metrics in the existing `Metric | Used for | Labels read | Required?` table, plus prose stating the attachment rule, the p90/server definition, the Grafana-parity caveat (definition, not value), and the graceful-degradation behaviour. An operator configuring a `servicegraph` connector must be able to see which series and labels this API needs without reading Go source. `CLAUDE.md` gains a compact glossary block ahead of the service-graph rules defining **trace-derived edge** / **synthesised edge**, **UID-resolved endpoint** / **peer-resolved endpoint**, **contributing series**, and **RED scope** — terms this change makes load-bearing and which currently exist only as prose.

## Risks / Trade-offs

- **[Exported metric names may lack the `_seconds` suffix]** The OTel `servicegraph` connector emits a histogram named `traces_service_graph_request_server` with unit `s`; the Prometheus exporter appends `_seconds` only when `add_metric_suffixes` is enabled (the default). With it disabled the series is `traces_service_graph_request_server_bucket` and this change's hardcoded name matches nothing. → **Mitigation:** graceful degradation means no `p90_server_ms` rather than a failure, the reason is logged distinguishably, and README documents the expected names. A single `group by (__name__) ({__name__=~"traces_service_graph.*"})` against the target store confirms it; task 4.4 makes that an explicit implementation step.

- **[Pairing rate is the real cap on RED coverage]** `store.ttl` defaults to 2 s and `store.max_items` to 1000; more importantly, a client span and its server span must reach the **same** collector instance, which requires trace-ID-aware routing (`loadbalancing` exporter with `routing_key: traceID`) in a multi-replica deployment. Unpaired spans become `server="unknown"` virtual-node edges, which D1 gives no metrics. → **Mitigation:** out of this repo's control, but documented in README so a low-coverage graph is diagnosed at the producer rather than debugged here.

- **[`virtual_node_peer_attributes` changes which resolution path fires]** Its default `[peer.service, db.name, db.system]` means an unpaired peer is labelled from those attributes when present, so `server` is *not* literally `"unknown"` and the peer-address enrichment ladder does not trigger; the endpoint takes the D27 human-label path to `external/<peer.service>` instead. → **Mitigation:** no code change needed (both outcomes are already specified), but documented, because it changes the observed mix of `external` nodes when migrating producers.

- **[Coarse producer buckets would make `p90_server_ms` systematically high]** Tempo's `service_graphs` default buckets start at 100 ms. → **Mitigation:** D7 records the analysis and the two remedies (tune the producer's `histogram_buckets`, or add an additive `avg_server_ms`); the intended producer is not affected.

- **[Upstream query cost]** Two more PromQL evaluations per `/v1/graph` request, one over a histogram. → **Mitigation:** the histogram is aggregated upstream so only `pod-pair × 16` crosses the wire; both selectors drop UID-less series; all three run in parallel under one errgroup, so wall-clock grows by at most the slowest query. Bounded query cost remains delegated to VictoriaMetrics search limits.

- **[Golden-file churn]** Every golden fixture containing a UID-resolved pod-to-pod edge changes. → **Mitigation:** additive `omitempty` field, regenerated with `go test ./internal/api/ -update -run Golden` and reviewed as part of the change.

- **[Consumers reading `data` exhaustively]** A client that rejects unknown fields breaks on the new key. → **Mitigation:** the `graph-api` contract has always permitted additive `omitempty` `data` fields (`parent`, `ipaddress`, `owner`, `application`, `containers`, `ready_status`, `provisioner`, `parameters` all arrived this way); OpenAPI is regenerated.

- **[`error_rate` absent vs `0`]** Consumers that treat a missing key as `0` will read an unreadable failure counter as a healthy edge. → **Mitigation:** the distinction is normative in the spec, documented in the OpenAPI description and README, and the aggregated warn log names the failed query.

- **[Grafana parity is definitional, not numeric]** An operator comparing our `p90_server_ms` against Grafana's service-graph latency will see different numbers, because Grafana groups by `(client, server)` service names and this API groups by pod pair. → **Mitigation:** stated explicitly in the spec, the OpenAPI description, and README, so the difference reads as an aggregation level rather than a bug.

## Migration Plan

Single deploy, no data migration, no config change. The response body gains an additive `omitempty` field; older clients ignore it. Rollback is a plain revert — edge IDs, node ids, `labels`, and ordering are untouched by this change, so a rolled-back build produces the previous bytes exactly.
