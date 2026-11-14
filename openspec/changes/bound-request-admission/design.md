# Design

## Context

See proposal.md — Why. The relevant current state:

- `pkg/kubegraph/parse.go` is the single parser for the three graph endpoints. `validateSelectorValues` bounds each value (≤253 bytes, valid UTF-8, no control character) but not the number of values. The four selector parameters render as ONE request-matcher alternation per dimension in every leg (`promql.appendMatcher`), never chunked. Parsers are config-free by rule: anything that needs a flag is enforced by the caller.
- `preparePodSeed` (`pkg/build/topologyplan.go`) caps a `pod=` root by the chunk count of its pod NAMES under the budget less the whole namespace alternation; `/v1/k8s-graph` reuses it through `k8sPlan`. The roots' namespace alternation is not bounded.
- `internal/api` mounts `otelgin`, `apiKeyMiddleware` and `spanEnrichMiddleware` on the `/v1` group. `runBuild` owns the `--build-timeout` context and cancels it when the build returns, so projection, serialisation and `json.Marshal` run under no deadline and do not observe a disconnected client.
- `cmd/kube-state-graph/main.go` constructs `http.Server` with timeouts but no `MaxHeaderBytes` (Go default 1 MiB).
- The RED (`redmetrics.go`), histogram and alert readers already drop NaN / ±Inf. The Harvest (`indexQoSFamily`, `indexPolicyCeilings`, `perfByNode`, `usageByAggr`) and kubelet (`resolvePVCUsage`) readers in `pkg/build/netapp.go` do not.

## Goals / Non-Goals

**Goals:**

- Every client-controlled multiplier on the request path has a server-side bound that fails fast (400 / 503) before any upstream query.
- No accepted request changes its body, its upstream queries or its fan-out; goldens and `storage_fanout.golden` do not move.
- Additive Go API only; `pkg/` keeps building on Go 1.27.1.

**Non-Goals:**

- Truncating the upstream query text in the promql client's logs and spans (owned by the promql work stream).
- A byte-cost query cache, per-API-key rate limiting, or a `SetLimit` on the first-wave errgroup.
- Quantising `start` to a grid (P-5 option b) — it changes window semantics.
- Response compression (#3) — see D10.

## Decisions

### D1. The selector value cap is a parser rule, per parameter, on DISTINCT values, at 100

Each of `cluster`, `namespace`, `az`, `env` may carry at most `promql.MaxAlternationValues` (100) distinct non-empty values; more is `invalid_scope` with the message `<param> carries more than 100 distinct values`, never echoing a value. The check runs in all three parsers right after the per-value validation of the same parameter, so `missing_az` / `missing_env` / `missing_cluster` keep their precedence and a malformed value is still reported as such.

- Distinct, not raw: `atLeastOne` / the renderer already sort and de-duplicate, so a repeated identical value costs nothing upstream and must not be what trips the cap.
- 100 is the upstream's exact-lookup limit for one alternation (the same number the chunker already enforces for data-derived alternations), so a request matcher never forces VictoriaMetrics into a regex scan of every value of the label.
- *Alternative — a configurable cap.* Rejected: the parsers are config-free by rule, and the number is the upstream's, not a deployment's.
- *Alternative — bound the rendered matcher's total bytes.* Rejected for now: per-value length (253) × 100 already bounds each dimension to ~25 KiB rendered; a byte budget would need a second, less legible error. Root parameters keep their chunk caps and are not given a parser count cap (a legal root set holds up to ~1600 values).

### D2. `MaxHeaderBytes` = 256 KiB, owned by `internal/api`

`api.MaxHeaderBytes` is the constant and `api.NewHTTPServer(cfg, handler)` the one constructor `main.go` uses, so a test drives the exact production `http.Server` over a real listener (a `ResponseRecorder` would bypass the limit). The worst legal request at the default 8192-byte budget, measured by `TestMaxHeaderBytes_AdmitsTheLargestLegalRequest`, is the four selector parameters at 100 × 253 bytes plus a `/v1/storage-graph` `aggr=` root set of 512 253-byte values that exactly fills the 16-chunk cap: 236779 bytes (~231 KiB) of request line, inside 256 KiB (262144) with ~25 KiB left for headers. Harvest root reads carry no request matcher, so the selectors do not shrink the root budget there — this combination is the maximum. One more root is a 400 from the cap, not a 431.

- Prefixed roots: `pod=<ns>/<name>` (and qualified `aggr=` / `svm=` `<oc>/<name>`) repeat their prefix in every URL value while the chunk budget charges it once per chunk. With a Kubernetes-legal namespace (a DNS label, ≤63 bytes) the largest pod root set the caps admit is 1600 pods and 240090 bytes of request line (`TestMaxHeaderBytes_PrefixedRoots`) — inside the bound.
- Known limits, accepted: a PARSER-legal root set can still exceed the bound and receive 431 — a namespace or ONTAP-cluster prefix far longer than any real name (1600 `pod=` roots in one 170-byte namespace is ~400 KB, pinned by the same test), a `--netapp-qos-scope-batch-bytes` far above the default, or roots of non-ASCII bytes (percent-encoded three bytes per byte, charged one by the budget). 431 is an explicit, early refusal of a request no real estate produces; the bound itself is a given decision of this change.
- *Alternative — derive the bound from the budget.* Rejected: it couples an HTTP transport limit to a query-shaping knob, and a 1 MiB default is what this change exists to lower.

### D3. The pod root seed is split by `(namespace, name)` pair, and the pair count is capped

`preparePodSeed` splits the roots' pairs with `promql.ChunkNamePairs`, the split every grouped pair read uses, so BOTH alternations of every seed chunk hold at most `promql.MaxAlternationValues` distinct values and fit the byte budget (less `RequestMatcherCost(QPVCBindings)`). The chunks counted against `maxRootedVolumeLabelChunks` are the chunks issued, so an admitted root set never renders a query the bound did not account for. A chunk bounds its DISTINCT namespaces and names, not its pairs (a 100 x 100 cross product is one chunk), so the pair count is also capped: `maxPodRootPairs` = `maxRootedVolumeLabelChunks` x `MaxAlternationValues` = 1600, the most values any other root kind admits. A set past either bound is `ReasonInvalidScope` before any query. Both endpoints get it through the shared function; the k8s pod root's `kube_pod_info` / `kube_pod_owner` reads follow the same pair chunks.

- *Alternative — count pair chunks but keep the name-driven split (the first form of this decision).* Rejected: the chunks counted were not the chunks issued, so an admitted set could still render a namespace alternation of more than 100 values or past the byte budget, and one name paired with many namespaces was one name chunk carrying them all.
- *Alternative — a parser count cap on `pod=` values.* Rejected: the chunk caps are the single bound every other root kind uses; a second, count-only rule would disagree with them at the edges.

### D4. Admission control: a semaphore middleware, 503 `overloaded`, default 32

`--max-inflight-builds` / `KSG_MAX_INFLIGHT_BUILDS`, default 32, `0` disables, negative rejected by `config.Validate`. A buffered channel guards the `/v1` group; the middleware is the LAST in the group chain (after `otelgin`, `apiKeyMiddleware` and `spanEnrichMiddleware`), so a 401 is still a 401 and the 503's span is still marked by `spanEnrichMiddleware`. Acquisition is non-blocking: when full, the request is answered at once with 503, `reason: "overloaded"`, `Retry-After: 1`, through `mapBuildError(build.NewError(build.ReasonOverloaded, …))`, and `c.Abort()`. The slot and the `kube_state_graph_inflight_builds` gauge are released in a `defer`, so a panicking handler (recovered by the outer recovery middleware) cannot leak a slot. `kube_state_graph_admission_rejected_total` counts rejections. Both metrics are unlabelled.

- 503 not 429: the condition is server capacity, not a per-client quota; 429 implies the caller did something wrong and invites per-client backoff logic. `Retry-After: 1` because a build is seconds long and an immediate retry against the same replica is exactly what we are shedding.
- Default 32, not 0: matches `--upstream-max-concurrency`'s default so a replica cannot hold more graphs in memory than it can have upstream queries in flight. Non-zero is a behavioural break for a deployment above 32 concurrent graph requests per replica, recorded in `docs/BREAKING.md`; `0` restores the old behaviour.
- A typed `build.ReasonOverloaded` rather than a bare middleware `writeError`: the repository rule is that every HTTP failure mode is a `build.Reason` with a `mapBuildError` entry, so the reason/status table stays in one place. The builder never returns it.
- Not `BuildRejected{reason="overloaded"}`: adding a label VALUE to an existing counter would be harmless, but the rejection happens before any build, and a dedicated counter beside the gauge reads more directly.
- *Alternative — wait a short budget for a slot.* Rejected: queueing is what the upstream slot already does, and the point is to shed load before it is held.
- *Alternative — GOMAXPROCS-derived default.* Rejected: memory, not CPU, is what N concurrent graphs exhaust.

### D5. `--max-window` is enforced in `internal/api`, after parsing

`--max-window` / `KSG_MAX_WINDOW`, default 0 (off), negative rejected. The check runs in each handler after its parser succeeded and before `serveGraph`, on the caller's `end − start`; equal passes. The parsers stay config-free (rule), so `missing_root` and every other parse error win. `io_delta=true` reads twice the window upstream; the cap is on the requested window, and the README says so.

- *Alternative — enforce inside `kubegraph.Parse*Values` with an options argument.* Rejected: it would change exported signatures and break the config-free rule.
- *Alternative — also expose it on `kubegraph.Engine`.* Not now: an embedder controls its own requests; additive later if asked.

### D6. The build deadline covers every stage; checks between stages

`serveGraph` creates the `--build-timeout` context (cause `errBuildTimeout`) and passes it to `runBuild`, the projection, the serialisation and the encode. Before each of the three later stages it checks `ctx.Err()`: `DeadlineExceeded` → `ReasonTimeout` (504, counted in `BuildRejected{timeout}`), `Canceled` → `ReasonCanceled` (499). The stages themselves are pure CPU and are not made cancellable mid-way.

- *Alternative — check only the request context.* Rejected: `runBuild` cancels its context on return, so a check on the request context alone sees disconnects but never a passed deadline.
- *Alternative — thread ctx into `graph.Project` / `cytoscape.Serialise`.* Rejected: exported `pkg/` signature changes for a gain measured in the time of one stage.

### D7. Non-finite values are dropped in the readers

`pkg/build/netapp.go` gains `finite(v) bool`; every Harvest and kubelet value reader skips a sample failing it before keying it, mirroring `redmetrics.go`. Skipping (not writing) keeps cached samples untouched. A NaN read first can no longer shadow a finite value in the `!seen || v < out[k]` picks. Health readers are unchanged (they produce strings; NaN already reads as degraded). No other plan stream touches `netapp.go`, so the fix lives at the source rather than as an API-layer guard. A sum of finite values overflowing to Inf (`× 1048576` on a ceiling near `MaxFloat64`) is out of scope. Known side effect, accepted: a QoS workload row with a non-finite value is dropped whole, so it also stops being a candidate in the hop-C policy-group pick — a LUN row whose figure is NaN no longer contributes its `policy_group` to the claim's ceiling. Such a row carries no usable measurement either, and a series a store answers with NaN is already broken upstream; dropping it keeps the measurement and the ceiling read from the same row set.

### D8. Bounded echoes: request id, access log, 400 messages

- `X-Request-ID` is kept iff 1–128 bytes of `[A-Za-z0-9._:=+/@-]` — UUIDs, ULIDs, W3C trace ids, AWS `Root=1-…` minus `;` — else replaced by a UUID. Replacing rather than truncating keeps the id one value end to end.
- The access log's `clusters` keeps the first 10 values, each cut to 64 bytes at a UTF-8 boundary, plus `clusters_truncated: true` when cut. The values are pre-parse (a 400 is logged too), so they can be arbitrarily long.
- `parseTimestamp` / `parsePrune` echo the raw value only when `validateSelectorValues` would accept it, exactly the `io_rollup` / `io_delta` rule; a short malformed value keeps its byte-identical message.

### D9. `/docs` uses a hash-based `script-src`

The inline `Scalar.createApiReference` call is the only inline script on the page. `script-src` becomes `'self' 'sha256-<hash of that script>' https://cdn.jsdelivr.net`, the hash computed at package init from the same constant the page embeds, so the two cannot drift; `'unsafe-inline'` is dropped from `script-src` (`style-src` keeps it — Scalar injects styles).

Proof that Scalar works without inline script: the page was served with exactly this policy and loaded in Chromium (Playwright) against Scalar 1.58.0; the UI rendered the full reference (sidebar, every tag, operation pages), and the console carried no `script-src` violation. The only violations — `font-src` for `fonts.scalar.com` and `connect-src` for `api.scalar.com` — are identical under the current `'unsafe-inline'` policy, i.e. pre-existing and unrelated. A unit test pins that the served HTML's inline script hashes to the value in the header.

- *Alternative — keep `'unsafe-inline'`.* Rejected once the browser run showed it is not needed.
- *Alternative — move the init call to a same-origin `/docs/init.js`.* Rejected: a new route for a one-line script, where a hash costs nothing.

### D10. Kept as is: response compression (#3), embedder deadline (#14)

- Compression stays the ingress's job: the API is cluster-internal behind an ingress or the front door's nginx, both of which compress, and an in-process gzip would spend CPU on every body to save bandwidth that is not the bottleneck.
- `kubegraph.Engine` applies no default build deadline: an embedder runs the engine in-process and passes its own context. Documented on `Engine` and in the README; behaviour unchanged.

## Risks / Trade-offs

- [A deployment above 32 concurrent graph requests per replica sees 503s after upgrade] → `docs/BREAKING.md` entry, `KSG_MAX_INFLIGHT_BUILDS=0` restores the old behaviour, `kube_state_graph_admission_rejected_total` makes it visible.
- [A client sending >100 distinct values of a selector parameter breaks] → the upstream already resolves such an alternation by a full regex scan; the 400 names the parameter and the limit.
- [A pod root set accepted before is rejected now] → only one whose pairs exceed 16 chunks, i.e. far more namespaces than any one request needs.
- [The deadline now also cuts serialisation] → a request that previously finished its encode just after the build timeout now gets 504; the build timeout is already the documented bound.
