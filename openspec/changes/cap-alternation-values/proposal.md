# Proposal

## Why

`GET /v1/k8s-graph?node=<one node>` becomes very slow once that node runs about 100 pods or more. The seed `kube_pod_info{node="…"}` is an equality and stays cheap. Every later read of those pods restricts on `pod=~"p1|p2|…"`, and that alternation is chunked by **bytes only**. Under the default 8192-byte budget, 100–300 pod names fit in one chunk.

VictoriaMetrics resolves an anchored literal alternation through an exact-value index lookup only while it has at most 100 alternatives (`maxOrValues = 100` with a `len(a) > maxOrValues` check in `lib/regexutil/regexutil.go`, confirmed at both `master` and `v1.107.0`, the integration image). Past 100 it falls back to evaluating the regex against **every value of that label** in the index for the queried range. That cost scales with the whole estate's pod churn, not with the node, and it is paid by every read in round 2 and by the claim and controller reads in round 3. Because rounds are sequential, a few slow queries become several seconds of request latency.

## What Changes

- Every **literal** data-derived alternation is chunked under a **value bound** as well as the existing byte budget. A chunk closes when adding a value would exceed either bound. The bound is a fixed constant, `promql.MaxAlternationValues = 100`. It applies to:
  - `ChunkScope` / `ChunkScopeWithOverhead`: controller, node, claim-seed, application-recovery, application-metadata, `volume_labels` aggregate / SVM / cluster scopes and owner completion, and the `/v1/storage-graph` per-namespace reads.
  - `ChunkQoSVolumeScope`: the QoS `volume` scope, on `/v1/graph` as well.
  - `ChunkHarvestPairs`: the per-ONTAP-cluster name alternation.
  - `ChunkNamePairs`: the `/v1/k8s-graph` grouped `(namespace, name)` reads. The bound is counted per side, on distinct namespaces and on distinct names separately, because the upstream judges each label filter on its own.
- **Suffix-mode token alternations are excluded.** These are the `.*<token>` branches of the restricted `volume_labels` read. VictoriaMetrics extracts no or-values from a `.*`-prefixed branch at any count, so that read always takes the regex path. Splitting it further would multiply regex scans and buy nothing. It is chunked by bytes alone through a dedicated helper.
- The value bound also applies when `budget <= 0`. That path previously meant "one chunk holding everything", and the bound is independent of the byte budget.
- Response bodies are unchanged. Chunks are merged in chunk order, and the split stays a pure function of the value set. Only the number and grouping of upstream queries change, and no read moves to another round.
- **BEHAVIOUR CHANGE (request-derived caps):** The root-set caps (`maxRootedVolumeLabelChunks`, `maxApplicationRootChunks`, both `= scopeConcurrency = 16`) count chunks, and chunks are now bounded by value count too. A root set therefore admits at most about 16 × 100 = 1600 literal values per kind. A request with more short root values than that, which today fits in 16 byte-chunks, is now rejected 400 `invalid_scope`. No realistic client sends that many roots. The cap stays a pure function of the root values and is still decided before any query.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `k8s-graph-api`:
  - "Known pods and claims are read across namespaces in one restriction": a chunk is also bounded by distinct namespace count and distinct name count.
  - "Bounded root sets": the cap counts chunks under both bounds.
- `storage-graph-api`:
  - "Storage build reads every family by reference": the shared chunking rule gains the value bound and the suffix-token exclusion.
  - "Every root kind is tracked from its own tier to its claims": its "Root sets are bounded before any query" scenario counts chunks under both bounds.
- `netapp-storage-graph`: "Deterministic QoS chunking with per-chunk degradation" gains the value bound for the `volume` scope.

## Impact

- **Code**:
  - `pkg/promql/scope.go`, `pairscope.go`, `qosscope.go` and `harvestpair.go` get the chunkers.
  - `pkg/promql/volumelabels.go` and `pkg/build/volumelabelscope.go` move the suffix-token read onto a bytes-only helper.
  - Doc comments on `ChunkHarvestPairs` and `ChunkScope` change for the `budget <= 0` path.
- **Tests**:
  - New bound tests in `pkg/promql`.
  - `pkg/build` root-cap tests (`nodeseed_test.go`, `podseed_test.go`, `claimseed_test.go`, `appscope_test.go`, `qualifiedroots_test.go`, `build_k8s_test.go`, `flowless_test.go`) are re-checked for fixtures above 100 values.
  - `pkg/build/testdata/storage_fanout.golden` only if its fixture carries more than 100 values in one scope.
- **API surface**:
  - New exported constant `promql.MaxAlternationValues`.
  - Exported chunkers keep their signatures; their documented split now also honours the bound.
  - No new flag, environment variable or `Options` field.
- **Docs**: `.claude/rules/upstream-queries.md` (chunking rule), `.claude/rules/storage-graph.md` (grouped pair reads), `docs/upstream-metrics.md` if it states chunk sizing.
- **Operations**: More, narrower queries per build for scopes above 100 values. Each stays on the upstream's exact-lookup path, which is the point of the change.

## Non-goals and follow-ups

- **Request-derived matchers are not split.** A repeated `?namespace=` / `?cluster=`, the `ALERTS` namespace or-absent matcher, and the namespace side of the `/v1/storage-graph` pod-root seed render from request values, not data. Splitting them would change the request contract.
- **Lookback of the node seed.** `kube_pod_info{node}[w]` returns every pod that ran on the node during `w`, including completed Jobs and replaced incarnations. A long window inflates the scope. Shortening the seed's lookback independently of `w` is a semantic change and needs its own proposal.
- **Cache-stable chunk boundaries.** A greedy split shifts every later boundary when one pod is added or removed, so every chunk misses the query-result cache. Hash-bucketed chunks would confine a change to one chunk. Worth measuring after this change lands.
- **Per-round timing.** `readK8sWorkload` has no span per round, so a slow round is visible only as a sum of `prometheus.query` spans. A per-round span would make the next diagnosis direct.
- **Concurrency.** `scopeConcurrency` (16) and the per-store guard (32) are unchanged. Tune them only if `kube_state_graph_upstream_slot_wait_seconds` shows queueing after this change.
