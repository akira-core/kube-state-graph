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

- Surface Rate / Errors / Duration on every edge the producer actually measured and whose two endpoints the reader could name (a pod or a service), with no invented numbers anywhere else and no call counted twice.
- Match Grafana's service-graph **definition** for Duration (server-observed, p90) so the two tools measure the same quantity.
- Keep the two new upstream reads cheap enough to run unconditionally, and harmless enough to fail without consequence.
- Preserve every existing invariant: edge IDs, `labels` strictness, determinism, "no caller filters pushed to PromQL", `pkg/` importability, no new dependency.

**Non-Goals:**

- **No client-side duration.** `traces_service_graph_request_client_seconds_*` is not read. Adding a `p90_client_ms` later is a purely additive field.
- **No mean/average.** `_server_seconds_sum` / `_count` are not read. See D7 for why the coarse-bucket argument that would have justified an exact mean does not apply to the intended producer.
- **No native-histogram / `vmrange` bucket support.** Only classic cumulative `le` buckets are understood; anything else degrades to "no `p90_server_ms`".
- **No extra quantiles.** `p50` / `p95` / `p99` are not emitted.
- **No RED on edges with an `external` endpoint**, and none on synthesised edges. See D1.
- **No RED on the ingress chain's caller → ingress entry hop** — the retained caller → backend edge carries the measurement for that call. See D1.
- **No RED derived from span-link series** (`edge_relation="link"`); the edge is emitted, the numbers are not. See D1b.
- **No span-metrics integration.** `spanmetrics` is per-span and carries no client/server pair, so it cannot produce edge metrics at all. Node-level RED from span metrics is a separate change, and joining it to pod nodes would key cardinality on `k8s.pod.uid × span.name × status.code`, which churns on every deploy.
- **No operator knob**, no feature flag. No caching, no change to windowing, no change to the route engine.

## Decisions

### D1 — Attachment rule: trace-derived edge, both endpoints resolved to a pod or a service

An edge gets `metrics` iff **all** of:

1. it came out of the `pairs` map — at least one `traces_service_graph_request_total` series produced it (**trace-derived**);
2. **both** resolved endpoint ids name a `type="pod"` node (real topology pod or synthesised pod) or a `type="service"` node;
3. it is not the route-hit ingress chain's **caller → ingress-service entry hop**;
4. it has at least one **in-scope** contributing series (D1b), and their summed rate is finite and `> 0`.

**How** the endpoint was identified is irrelevant. A pod UID, a `"://"` connection string resolved to a Service (D29), a `server="unknown"` peer address classified as a Service DNS name or matched against a Service `ClusterIP`, a peer address matched against a family Pod IP, and an Istio route-engine resolution to a backend Service all qualify. The edge `type` is likewise not a condition: both `pod-calls-pod` and `pod-calls-service` can carry metrics, and the type is a *consequence* of what the target resolved to, not an independent test.

*Why "resolved to a pod or a service" is the right line.* The endpoint-type test is the only one that actually holds. It cannot be replaced by a label-level predicate (a UID matcher) or by the edge type, and the **D33 self-loop UID normalisation** is the standing proof: it clears the `"://"` side's UID when the two UIDs are non-empty and equal, so a series whose raw labels carry both UIDs can still resolve one side through the connection-string path — and if that URL is unresolvable the side becomes an `external` node **without** changing the edge type (only a *service* target downgrades it). `pkg/graph/registry.go` records the shape: `pod-calls-pod` declares `SourceType: [Pod, Service, External]` and `TargetType: [Pod, External]`. So the resolved-node-type check (`sgResolver.isPodOrServiceID`, covering `podByID`, `synthPods`, and the materialised service nodes) is the enforcing condition. Regression coverage: `TestRED_D33ClearedUIDDoesNotAttachToExternal` (still valid — external stays excluded), `TestRED_D33ClearedUIDResolvedToServiceAttaches` (inverted from the previous expectation), and the parse-driven invariant test.

| Excluded shape | Why a number there would be fabricated |
|---|---|
| any edge with an `external` endpoint | The external node collapses *all* traffic sharing one label string onto one identity, and `external/` ids are not cluster-scoped, so the number names no measurable dependency. |
| `service-selects-pod` | Synthesised fan-out. No series names the individual backing pod; splitting the service's traffic across N endpoints would be an invention. |
| ingress-chain gateway-pod → backend-service hop | Synthesised. No contributing series exists at all. |
| caller → ingress-service entry hop | Trace-derived and pod/service on both ends, but a *second projection of the same call* — see below. |
| `pod-mounts-pvc` / `pod-to-node` / `pvc-to-storageclass` | Topology-derived; no trace series exists. |

*Why the ingress chain's entry hop is excluded while the direct edge is kept.* On a route-engine hit, `routeIndexResolve` returns `[ingress, backend]` as the endpoint's resolution targets, so one series produces **two** trace-derived edges: caller → ingress service and caller → backend service. Both satisfy conditions 1 and 2. Attaching to both would make a single observed request contribute its rate twice to any sum taken over the chain, and no consumer can tell which of the two is the double. Exactly one must carry the measurement, and it is the one that names the actual destination: the caller → backend edge. The entry hop then behaves like the gateway-pod → backend hop it pairs with — chain topology, no numbers. Eligibility is decided at the point that knows it (`resolveRouteChain` / `routeIndexResolve` mark the entry hop's pair ineligible), not by inspecting node roles afterwards, so a service node that is *also* a legitimate D29 destination for some other caller is unaffected.

*Reversals from the first draft of this design, and why.* Two shapes previously excluded now qualify:

- **Peer-resolved endpoints** (the `server="unknown"` ladder, including the Pod-IP branch). The old argument was that the ladder runs only because the connector could not pair a server span, so the RED series hold "no measurement" for that endpoint. That is wrong about which side is measured: `_failed_total` and `_server_seconds_bucket` are emitted on the **same series identity** as `_total`, i.e. per client-observed edge, so whatever the connector recorded for the call it recorded on all three. The peer-resolved case loses the *server's* identity, not the measurement. Declining to report it left the operator with an edge that demonstrably carries traffic and no way to see how much — the exact gap this change exists to close.
- **`pod-calls-service` from D29 connection strings and from route resolution.** The old argument was double-counting "against the fan-out story". It does not hold: the `service-selects-pod` fan-out carries no metrics, so nothing is counted twice. The caller dialled a Service; the Service is the destination identity the caller actually observed, and it is the natural place for the caller's rate. (The one real double-count in this area is the ingress chain's two projections, handled above.)

*Synthesised pods DO qualify* (non-empty UID unknown to topology). Excluding them would make an edge's metrics blink in and out as topology scrape coverage changes.

### D1b — Span-link series are out of scope for measurement

A contributing series carrying the dimension `edge_relation="link"` is **out of scope**: it contributes to no rate, no error numerator, and no duration bucket. The **edge is still emitted** — it is a real dependency the graph should show.

*Why.* The dimension marks an edge the `servicegraph` connector materialised from a **span link** rather than from a paired client/server span. The two spans belong to different trace contexts and the "call" between them physically traverses a queue or a database, so the three RED series measure something that is not a request-response interaction: the duration spans a broker's dwell time, and the failure counter describes the link's own status, not the peer's. Reporting it next to a real RPC edge's numbers invites a comparison that is meaningless.

*Granularity: per series, not per pair.* An edge whose contributing series are a mix of link and non-link is measured over the **non-link subset only**, unlike the older "any out-of-scope series poisons the whole pair" rule that this replaces. The two cases differ: the old out-of-scope condition (a UID-less series) meant the *edge identity itself* was only partly UID-derived, so a partial sum would have described a different edge than the one emitted. Here both subsets describe the same `(source, target)` pair; the link subset simply measures a different kind of interaction over it. Summing only the non-link subset yields exactly "the request-response RED of this dependency", which is a well-defined quantity. An edge whose contributing series are **all** link therefore has an empty in-scope set, hence a zero rate, hence no `metrics` object at all (D1 condition 4) — the correct outcome, reached without a special case.

*Contract.* `edge_relation` is an operator-configured `dimensions` entry on the connector, not a stock label. Its name and the value `link` are a fixed, case-sensitive contract in the same class as the D30 sentinel values and the Trident label names (design D26) — no knob, no config surface. The connector's own `connection_type` label (`messaging_system` / `database` / `virtual_node`) is deliberately **not** given the same treatment: it describes the transport the connector inferred, not whether the two spans were paired, and a `database` edge built from a genuine client/server pair carries a real client-observed duration.

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

### D4 — Symmetric join: both RED vectors by exact series identity

Both new vectors are queried at the **same raw label granularity as the total counter** (the histogram additionally carrying `le`) and both join to a contributing series by that series' **full label set minus `__name__`** — for the histogram, minus `le` as well. During the resolution loop the parse records a single `seriesKey → pairKey` map, which serves both second passes: the failure pass adds each matched series' rate to the pair's error numerator, the bucket pass adds each matched series' bucket count onto the pair's per-`le` accumulator.

*Why identity, for both.* The numerator and the denominator of `error_rate` must come from the same series set or the ratio is meaningless, and once the attachment rule (D1) no longer requires a UID on either side, **the same is true of the histogram**: there is no longer a low-cardinality key that identifies an edge. `(cluster, client_k8s_pod_uid, server_k8s_pod_uid)` collapses every peer-resolved endpoint of one client pod into a single group — one caller dialling three different connection-string destinations produces three edges and one bucket set, silently merged. Recovering the distinction would mean adding the peer-identifying dimensions (`server`, `client_server_address`, `client_network_peer_address`, `client_net_peer_name`, and `client_dns_answers`, which participates in route resolution) to the group-by, at which point the group-by must track, forever and exactly, the label set the resolver reads. A missed label is not a build failure — it is two unrelated edges quietly sharing a latency distribution. Series identity has no such coupling: it is correct by construction for any resolution path, present or future.

*Cost, accepted.* The histogram is no longer pre-aggregated upstream, so `edge-cardinality × 16` bucket series cross the wire instead of `pod-pair × 16`. See Risks. The mitigations that remain are the sentinel and link matchers, and the fact that the same dimension set is already on the wire for `_total`.

*One join map, two consumers.* Because `_total`, `_failed_total`, and `_bucket` carry the connector's identical `dimensions` set, the bucket series' label set minus `le` is exactly the total series' label set. If a producer diverges (a relabel applied to one metric family and not another), the join matches nothing — detected and logged per D10, for both vectors.

*Why the parse records the join key instead of a second resolution pass:* re-deriving `(srcID, tgtID)` from labels would duplicate `resolveClient`'s cluster-recovery fallbacks (`podByID` → `podByUID`), the peer-address ladder, the route index, and the D33 self-loop normalisation. Recording the mapping the resolver actually produced makes drift impossible by construction — and that argument only gets stronger now that four more resolution paths are in scope.

*Out-of-scope series record nothing.* A contributing series carrying `edge_relation="link"` (D1b) is skipped when the join map is built, so a link series' failure and bucket rows — which the query layer already excludes (D6) — could not be joined even if they arrived.

### D5 — `p90_server_ms` computed in-process from summed classic buckets

A new pure helper (`pkg/build/histogram.go`) implements the classic cumulative-bucket quantile: sort by `le`, find the bucket containing `0.90 × count`, linearly interpolate inside it, clamp to the highest finite bound when the quantile lands in `+Inf`. Result × 1000 → milliseconds.

*Why not `histogram_quantile(0.9, ...)` in PromQL:* several contributing series legitimately map to one edge (two series differing only in `connection_type`; a series whose `cluster` label is missing bucketed to `"unknown"` alongside a sibling carrying the real cluster, both recovering the same client pod via `podByUID`). Quantiles are not additive — averaging or maxing per-series p90s is wrong in a way that is invisible in the output. Bucket **counts** are additive, so summing buckets across the edge's in-scope contributing series first and computing one quantile is the only correct order.

*Why p90 and not p99:* Grafana's documented service-graph latency query uses `.9`, and matching the definition of the tool operators already use is worth more than a rounder-sounding number. The docs note the quantile is adjustable, so p90 is the *default presentation*, which is what parity means here.

*Degradation:* fewer than two distinct `le` boundaries, no `+Inf` bucket, a non-numeric `le`, or an absent `le` label (the native-histogram / `vmrange` case) all yield "no `p90_server_ms`" for the affected pair rather than a guess.

### D6 — The two new selectors carry request-invariant matchers

Both new selectors are `{<serviceGraphSentinelSelector>, edge_relation!="link"}`. The pod-pair matcher pair (`client_k8s_pod_uid!="", server_k8s_pod_uid!=""`) is **removed**: under D1 an endpoint no longer has to be UID-resolved, so that matcher would now filter away the failure and bucket series of edges that *do* qualify.

- The sentinel fragment is reused verbatim from `promql.serviceGraphSentinelSelector`, as its own doc comment requires, so the three series always describe one edge population.
- `edge_relation!="link"` is D1b pushed to the query layer, as a new named constant `serviceGraphLinkExclusionSelector`. Both are the **same class** as the D30 sentinel and the `condition="Ready"` node selector: fixed metric-selection contracts that never vary per request, not caller filters. The "no filters pushed to PromQL" rule is preserved, and a future cache serving any filter from one built graph is unaffected.
- PromQL `!=` treats an **absent** label as the empty string, so `edge_relation!="link"` retains every series that does not carry the dimension at all — which, for a producer that never configured it, is all of them. No operator has to add the dimension for this change to work.
- The `_total` selector deliberately does **not** gain the link matcher. The edge must still be emitted; only its measurement is suppressed. This is the one place where the three selectors legitimately differ, and it is why D1b is *also* enforced Go-side.

**The queried population is now a superset of the attachment population, and that is sound.** D1's conditions 2 and 3 (endpoint node type, chain entry hop) are functions of topology and of the route index — there is no label-level predicate for them, so exact equivalence is unattainable and was never worth manufacturing. The property that actually matters is one-directional and does hold: **every query-layer filter is mirrored exactly in Go**, so no eligible edge can have its failure or bucket series filtered away upstream and then be reported as `error_rate: 0`. A sentinel-excluded series produces no edge at all; a link-excluded series produces an edge that Go independently marks out of scope. The reverse direction — series returned for a pair that turns out ineligible — costs a few unused rows and nothing else.

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

Related diagnostic, same spirit: both joins are by exact series identity (D4), so a label-set divergence between `_total` and either companion — a relabel applied to one and not the other, a dimension added on one side — makes the join match nothing. For failures that means **every** edge reports `error_rate: 0`, indistinguishable from "no failures"; for buckets it means `p90_server_ms` is universally absent, indistinguishable from "the producer emits no histogram". Both accumulators therefore return matched / unmatched counts, and a non-empty result that matched nothing is logged once under its own reason, one per vector. Zero series is *not* that signature and stays quiet.

### D11 — Documented upstream contract

`README.md`'s "Service-graph metric" section gains the two new metrics in the existing `Metric | Used for | Labels read | Required?` table, plus prose stating the attachment rule, the p90/server definition, the Grafana-parity caveat (definition, not value), and the graceful-degradation behaviour. An operator configuring a `servicegraph` connector must be able to see which series and labels this API needs without reading Go source. The `dimensions` an operator must configure — including `edge_relation`, whose `link` value suppresses measurement (D1b) — are part of that table. `CLAUDE.md` gains a compact glossary block ahead of the service-graph rules defining **trace-derived edge** / **synthesised edge**, **UID-resolved endpoint** / **peer-resolved endpoint**, **contributing series**, **in-scope series**, and **RED scope** — terms this change makes load-bearing and which currently exist only as prose.

## Risks / Trade-offs

- **[Exported metric names may lack the `_seconds` suffix]** The OTel `servicegraph` connector emits a histogram named `traces_service_graph_request_server` with unit `s`; the Prometheus exporter appends `_seconds` only when `add_metric_suffixes` is enabled (the default). With it disabled the series is `traces_service_graph_request_server_bucket` and this change's hardcoded name matches nothing. → **Mitigation:** graceful degradation means no `p90_server_ms` rather than a failure, the reason is logged distinguishably, and README documents the expected names. A single `group by (__name__) ({__name__=~"traces_service_graph.*"})` against the target store confirms it; task 4.4 makes that an explicit implementation step.

- **[Pairing rate shapes which numbers exist, no longer whether they are shown]** `store.ttl` defaults to 2 s and `store.max_items` to 1000; more importantly, a client span and its server span must reach the **same** collector instance, which requires trace-ID-aware routing (`loadbalancing` exporter with `routing_key: traceID`) in a multi-replica deployment. Unpaired spans become `server="unknown"` edges. Under D1 those now carry RED whenever the peer address resolves to a pod or a service — the measurement is the client's, which is what the connector recorded. The residual caveat is definitional rather than a coverage gap: on an unpaired edge, `_server_seconds` reflects what the client observed, so `p90_server_ms` includes network time the server itself never saw. → **Mitigation:** documented in README alongside the producer prerequisites, so the difference reads as an observation-point property rather than a bug; an operator who wants strictly server-observed latency fixes pairing at the producer.

- **[`virtual_node_peer_attributes` changes which resolution path fires]** Its default `[peer.service, db.name, db.system]` means an unpaired peer is labelled from those attributes when present, so `server` is *not* literally `"unknown"` and the peer-address enrichment ladder does not trigger; the endpoint takes the D27 human-label path to `external/<peer.service>` instead — which, under D1, is the one shape that still carries no metrics. Turning the attribute list off therefore *increases* RED coverage by routing the peer through the address ladder. → **Mitigation:** no code change needed (both outcomes are already specified), but documented, because it changes both the observed mix of `external` nodes and the measured fraction of the graph when migrating producers.

- **[Coarse producer buckets would make `p90_server_ms` systematically high]** Tempo's `service_graphs` default buckets start at 100 ms. → **Mitigation:** D7 records the analysis and the two remedies (tune the producer's `histogram_buckets`, or add an additive `avg_server_ms`); the intended producer is not affected.

- **[Upstream query cost, now dominated by the raw histogram]** Two more PromQL evaluations per `/v1/graph` request, and D4 gives up the upstream `sum by` on the histogram: roughly `edge-cardinality × bucket-count` series cross the wire (16 boundaries with the connector's defaults), against `pod-pair × 16` in the earlier draft. The dimension set includes high-cardinality peer labels (`client_dns_answers`, `client_server_address`), which the group-by used to collapse. → **Mitigation:** the alternative was silently merging unrelated edges' latency distributions (D4), which is a correctness cost, not a capacity one. What remains: the sentinel and link matchers prune the selector; the same dimension set is already on the wire for `_total`, so the multiplier is the bucket count, not a new cardinality class; all three queries run in parallel under one errgroup, so wall-clock grows by at most the slowest; the histogram is OPTIONAL, so an operator whose store cannot carry it loses `p90_server_ms` and nothing else. Bounded query cost remains delegated to VictoriaMetrics search limits — and a store that refuses the query degrades exactly as a missing metric does.

- **[`edge_relation` is an operator-configured dimension, not a stock label]** Nothing in the connector emits `edge_relation` by default; it exists only where the operator added it to `dimensions` and the instrumentation sets the corresponding span attribute. A deployment that marks its span-link edges under a different name, or not at all, gets no exclusion — queue/db virtual edges will carry RED as if they were RPCs. → **Mitigation:** absent-label semantics mean the matcher is inert rather than harmful (D6), the name is documented in README next to the required dimensions, and D1b records the reasoning so a future rename is a one-constant change. Deliberately not a knob, consistent with D8 and with every other fixed label contract in the reader.

- **[Golden-file churn]** Every golden fixture containing a UID-resolved pod-to-pod edge changes. → **Mitigation:** additive `omitempty` field, regenerated with `go test ./internal/api/ -update -run Golden` and reviewed as part of the change.

- **[Consumers reading `data` exhaustively]** A client that rejects unknown fields breaks on the new key. → **Mitigation:** the `graph-api` contract has always permitted additive `omitempty` `data` fields (`parent`, `ipaddress`, `owner`, `application`, `containers`, `ready_status`, `provisioner`, `parameters` all arrived this way); OpenAPI is regenerated.

- **[`error_rate` absent vs `0`]** Consumers that treat a missing key as `0` will read an unreadable failure counter as a healthy edge. → **Mitigation:** the distinction is normative in the spec, documented in the OpenAPI description and README, and the aggregated warn log names the failed query.

- **[Grafana parity is definitional, not numeric]** An operator comparing our `p90_server_ms` against Grafana's service-graph latency will see different numbers, because Grafana groups by `(client, server)` service names and this API groups by pod pair. → **Mitigation:** stated explicitly in the spec, the OpenAPI description, and README, so the difference reads as an aggregation level rather than a bug.

## Migration Plan

Single deploy, no data migration, no config change. The response body gains an additive `omitempty` field; older clients ignore it. Rollback is a plain revert — edge IDs, node ids, `labels`, and ordering are untouched by this change, so a rolled-back build produces the previous bytes exactly.
