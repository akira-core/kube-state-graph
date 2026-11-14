# Proposal

## Why

Every graph endpoint accepts work whose cost is set by the client and bounded by nothing on the server side:

- The selector parameters `cluster`, `namespace`, `az` and `env` are repeatable and the parsers bound each value's LENGTH, never their COUNT. Every value lands in one request-matcher alternation that is never chunked, so one cheap request can render a matcher of tens of kilobytes into every one of the 37–44 legs of a build. Past the upstream's query-length limit each leg fails and is logged in full; below it, the long key fills the query-result cache whose budget counts series, not bytes; and on `/v1/storage-graph` / `/v1/k8s-graph` the matcher eats the shared byte budget until every data-derived chunk carries a single value. `http.Server.MaxHeaderBytes` is unset, so the request line can reach 1 MiB.
- A `pod=` root's NAME side is capped (the seed's chunk count), but its NAMESPACE side is taken from the roots like a repeated `namespace` parameter and is not bounded: one distinct pod name paired with ten thousand namespaces passes the cap in one chunk and drives the storage build into one query per namespace.
- `/v1/*` has no in-flight bound. The only concurrency control is the per-store upstream slot, which cache hits and coalesced waiters do not hold, and which says nothing about assemble / project / serialise / encode memory. Changing `start` by one second is a fresh cache key, so N concurrent clients hold N full graphs and bodies, and a heavy request turns a light one into a 504 instead of being shed.
- The window has no upper bound on the server side; bounding it is delegated to VictoriaMetrics' search limits, with no server knob for a deployment whose upstream does not enforce them.
- Smaller gaps in the same request path: a NaN / ±Inf Harvest or kubelet sample reaches `json.Marshal` and turns a valid build into a 500; `X-Request-ID` is echoed and logged verbatim at any length; `parseTimestamp` / `parsePrune` echo an arbitrarily long raw value in the 400 body; the projection, serialisation and encoding stages keep running after the client has gone or the build deadline has passed; and the access log writes every `cluster` value of a request.

## What Changes

- **Selector value count cap.** `ParseValues`, `ParseStorageValues` and `ParseK8sValues` reject a request whose `cluster`, `namespace`, `az` or `env` carries more than `promql.MaxAlternationValues` (100) DISTINCT non-empty values, as `400 invalid_scope`, before any query. Root parameters are unaffected (the chunk caps bound them). The required-parameter checks (`missing_az`, …) keep their precedence.
- **Request-line bound.** The server sets `http.Server.MaxHeaderBytes` to 256 KiB explicitly; the largest request every parser and chunk cap admits at the default byte budget fits inside it.
- **Pod root namespace side is bounded.** A `pod=` root set is also counted as `(namespace, name)` pairs chunked by `promql.ChunkNamePairs` under the seed's budget; more chunks than the root cap is `400 invalid_scope` before any query, on `/v1/storage-graph` and `/v1/k8s-graph`. The seed's actual split is unchanged.
- **Build admission control.** New `--max-inflight-builds` / `KSG_MAX_INFLIGHT_BUILDS` (default **32**, `0` disables). A semaphore on the `/v1` route group, after API-key authentication, admits at most that many graph requests at once; the next one is answered immediately with `503 Service Unavailable`, `reason: "overloaded"` and `Retry-After: 1`. `/livez`, `/readyz`, `/metrics`, `/docs` and `/openapi.*` are never limited. New self-metrics `kube_state_graph_inflight_builds` (gauge) and `kube_state_graph_admission_rejected_total` (counter), both unlabelled. A typed `build.ReasonOverloaded` and its `mapBuildError` entry carry the mapping. **BREAKING** (behavioural): the default is non-zero, so a deployment that today runs more than 32 concurrent graph requests per replica starts receiving 503s; `docs/BREAKING.md` records it.
- **Optional window cap.** New `--max-window` / `KSG_MAX_WINDOW` (duration, default `0` = off). When set, a request whose `end - start` (the caller's values) exceeds it is `400 invalid_range` on all three graph endpoints, after every parser check and before any query. The README lists VictoriaMetrics' `-search.maxQueryDuration` and `-search.maxSamplesPerQuery` as a deployment precondition either way.
- **Stage deadlines.** The `--build-timeout` deadline now bounds the whole graph request — build, projection, serialisation and encoding — and the server checks the request context between those stages, answering `504 timeout` or `499 canceled` instead of finishing work nobody will read.
- **Non-finite upstream values are dropped.** The Harvest and kubelet readers skip a NaN or ±Inf sample, exactly as the RED and alert readers already do, so a non-finite value can no longer make `json.Marshal` fail with a 500.
- **Bounded echoes.** An inbound `X-Request-ID` is kept only when it is 1–128 bytes of `[A-Za-z0-9._:=+/@-]`, else replaced by a generated id; the access log's `clusters` field carries at most the first 10 values, each cut to 64 bytes, with `clusters_truncated: true` when anything was cut; the `invalid_start` / `invalid_end` / `prune` 400 messages echo the raw value only when it passes the selector-value checks (≤253 bytes, valid UTF-8, no control character), as the `io_rollup` / `io_delta` messages already do.
- **Documentation only.** The embedder facade (`kubegraph.Engine`) has no default build deadline — documented, behaviour unchanged. Response compression stays the ingress's job. The `/docs` CSP keeps `script-src 'unsafe-inline'` (see design D9).

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `graph-api`: selector value count cap; request-line bound; build admission control (503 `overloaded`, `Retry-After`, flag, exempt routes, self-metrics); optional window cap replacing "no window cap"; the build deadline bounds every stage; non-finite upstream values dropped; bounded `X-Request-ID`, access-log `clusters` and 400 echoes.
- `storage-graph-api`: the `pod` root's namespace side is bounded by the root cap; the selector value cap, admission control and window cap apply.
- `k8s-graph-api`: the same `pod` root bound on its first read.

## Impact

- **Code**: `pkg/kubegraph` (parser value cap, bounded echoes), `pkg/build` (`ReasonOverloaded`, pod-root pair cap in `preparePodSeed`, non-finite drop in `netapp.go`), `internal/config` (`MaxInflightBuilds`, `MaxWindow`), `internal/observability` (two metrics), `internal/api` (admission middleware, window check, stage checks, request-id sanitising, access-log truncation, `NewHTTPServer` carrying `MaxHeaderBytes`, swag annotations), `cmd/kube-state-graph` (uses `api.NewHTTPServer`).
- **API**: one new status (503 `overloaded`) on every `/v1/*` graph route; new 400 paths (`invalid_scope` for >100 selector values or an over-cap pod root, `invalid_range` past `--max-window`). Response bodies of accepted requests are unchanged; goldens unchanged. `docs/swagger.{json,yaml}` regenerated.
- **Go API**: additive only (`build.ReasonOverloaded`). `pkg/` still builds on Go 1.27.1.
- **Upstream load**: unchanged for every accepted request; the storage fan-out golden does not move.
- **Docs**: `README.md` (new flags, VictoriaMetrics preconditions, embedder deadline), `docs/BREAKING.md` (admission default), `.claude/rules/upstream-queries.md`, `.claude/rules/storage-graph.md`.
