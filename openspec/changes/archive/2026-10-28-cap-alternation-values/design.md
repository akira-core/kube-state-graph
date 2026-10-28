# Design

## Context

See proposal.md (Why) for the latency mechanism. This section covers the state of the code.

Every data-derived restriction in `pkg/build` goes through one of four exported chunkers in `pkg/promql`. All four split on a byte budget only (`build.Options.QoSScopeBatchBytes`, default `DefaultQoSScopeBatchBytes = 8192`):

| Chunker | Callers |
|---|---|
| `ChunkScope` / `ChunkScopeWithOverhead` (`scope.go`) | `issueScopedFamilies` (node, controller, per-namespace pod / claim, application recovery, application metadata, flowless gauges); `prepareNodeSeed`, `preparePodSeed`, `prepareClaimSeed`; `rootedVolumeLabelsChunks`, `rootedNodeLabelChunks`; owner completion; `ChunkHarvestPairs`; the suffix-token `volume_labels` read (`volumelabelscope.go`, overhead `VolumeTokenBranchOverhead`) |
| `ChunkQoSVolumeScope` (`qosscope.go`) | the scoped QoS wave; a thin alias of `ChunkScope` |
| `ChunkHarvestPairs` (`harvestpair.go`) | the expansion's pair-keyed Harvest reads; calls `ChunkScope` per ONTAP cluster |
| `ChunkNamePairs` (`pairscope.go`) | `/v1/k8s-graph` grouped `(namespace, name)` reads through `chunksOfPairs` |

`budgetOverhead` on `scopedFamily` is set by no caller today, so every `issueScopedFamilies` read renders literal values.

The request-derived root caps (`maxRootedVolumeLabelChunks`, `maxApplicationRootChunks`, both `= scopeConcurrency = 16`) compare `len(chunks)` from these same chunkers. A change to the split therefore changes what the caps admit.

The upstream behaviour this design targets was verified against the VictoriaMetrics source at `master` and at `v1.107.0`, the version `internal/integration` runs. `lib/regexutil/regexutil.go` declares `const maxOrValues = 100`. `getOrValues` returns nil from its `OpAlternate` case when `len(a) > maxOrValues`. It also returns nil for any branch it cannot reduce to literals, such as `.*foo`, at any count. `lib/storage/tag_filters.go` then falls back to a regex match over every value of the label.

**Measured.** The setup:
- VictoriaMetrics `v1.107.0` in Docker, holding 300,000 `kube_pod_info` series: 300,000 distinct `pod` values across 50 namespaces and 500 nodes.
- Each query has the incarnation-read shape `last_over_time(kube_pod_info{az,env,namespace=~<50 values>,pod=~<n names>}[5m])`.
- `nocache=1`, distinct names on every run, median of 15 runs.

The medians were:

| Names in the alternation | Median latency |
|---|---|
| 50 | 2.7 ms |
| 100 | 2.0 ms |
| 101 | 117.4 ms |
| 150 | 111.8 ms |
| 200 | 122.4 ms |

The cliff sits exactly at the bound. Past it the cost tracks the label's total value count, not `n`. End-to-end `/v1/k8s-graph` latency was not measured.

## Goals / Non-Goals

**Goals:**

- Every literal alternation built from data or from roots holds at most 100 values, enforced in one place: the chunkers.
- The split stays a pure function of the value set, so bodies and goldens are unchanged.

**Non-Goals:**

- Splitting suffix-token (`.*<token>`) alternations.
- Changing the round structure, `scopeConcurrency`, or the per-store guard.
- Bounding request-scoped matchers or the namespace side of the `/v1/storage-graph` pod-root seed. See D7.

## Decisions

### D1. A compiled-in constant, not a knob

Add `promql.MaxAlternationValues = 100`, with a doc comment naming the VictoriaMetrics source and the `>` boundary.

- *Alternative: an `Options` field plus `--max-alternation-values`, mirroring the byte budget.* Rejected. The threshold is a property of the upstream query engine, not of a deployment. A value above 100 silently restores the regression this change removes, and nothing would flag it. The repo already compiles in request-invariant query contracts (`alertsFiringSelector`, the service-graph sentinel). If a store with a lower threshold ever appears, a knob is an additive change.
- *Alternative: 99, as a safety margin.* Rejected. The check is `>`, so exactly 100 values still take the exact-lookup path, and the bound is checked on the rendered value count, which the chunker controls exactly.

### D2. Enforce the bound inside the chunkers

A chunk closes when the next value would exceed the byte budget **or** the value bound. Every caller, including every root cap, therefore gets the bound without a call-site change.

- *Alternative: re-split chunks at each call site.* Rejected. There are more than a dozen call sites, and a caller added later would be uncapped by default.

### D3. `ChunkNamePairs` counts distinct values per side

The upstream judges each label filter on its own, so a chunk closes when adding the pair would push `len(namespaces)` **or** `len(names)` past the bound. It does not close on the pair count.

- *Alternative: cap the pair count at 100.* Safe, but it splits more than needed. Many pods in one namespace share one namespace value, and a pair cap would buy nothing there.

### D4. The suffix-token read gets its own bytes-only chunker

Add `promql.ChunkSuffixTokenScope(tokens []string, budget int) [][]string`. It splits on bytes alone, with `VolumeTokenBranchOverhead` built in. `volumelabelscope.go` switches to it. `ChunkScope` and `ChunkScopeWithOverhead` become bounded and share one unexported core, `chunkScope(values, budget, overhead, maxValues)`, in which `maxValues <= 0` means unbounded.

- *Alternative: infer "suffix" from `overhead > 0`.* Rejected. That couples an unrelated byte-accounting parameter to an upstream-engine fact, so a future literal family with a per-branch overhead would silently lose the bound.

### D5. The bound applies when `budget <= 0`

`budget <= 0` currently means "one chunk holding everything". The value bound does not depend on the byte budget, so it still applies. Update the doc comments of `ChunkScopeWithOverhead` and `ChunkHarvestPairs`, which document that path. Server configuration rejects a non-positive budget, so this matters only to embedders calling `pkg/promql` directly.

### D6. Root caps count bounded chunks, so they tighten

Each root kind's first read admits at most 16 chunks of at most 100 values. That is 1600 literal values per kind, or fewer when the byte budget binds first.

- *Alternative: compute the caps under the byte budget alone.* Rejected. The cap exists to bound the first read's fan-out before any query. Counting byte-chunks while issuing value-bounded chunks would let an admitted request issue more than 16 queries in one read.
- *Alternative: raise the cap.* Rejected. `scopeConcurrency` ties the cap to one bounded wave, and no realistic client sends more than 1600 roots of one kind.

This is the one externally visible change. A request that fits 16 byte-chunks but holds more than 1600 short values becomes 400 `invalid_scope`. The release notes say so.

### D7. The pod-root seed's namespace side is not bounded

`preparePodSeed` chunks root pod **names** with `ChunkScope`, which is now bounded, and derives each chunk's namespaces from the refs that carry those names. The namespace side can exceed 100 only when more than 100 namespaces are named across at most 100 pod names in one request. It is request-derived, the same class as a repeated `?namespace=`, and bounding it would change the seed's chunk split that `/v1/k8s-graph`'s pod root shares with its sibling reads. It is recorded here and left alone.

## Risks / Trade-offs

- **More queries per build.** A scope of n literal values now issues `ceil(n/100)` chunks or more, where it previously issued `ceil(bytes/8192)`. → Each query is an index lookup instead of a label-wide regex scan. The chunks of one read run in the same round under `scopeConcurrency`, so latency falls even though the query count rises. Watch `kube_state_graph_upstream_slot_wait_seconds` after rollout.
- **Cache entries.** More entries hold the same total series, so cache cost, which is counted in series, is unchanged.
- **The upstream changes its threshold.** → The constant's doc comment names the file and the version it was checked against, and a unit test pins 100 so that a change is deliberate.
- **Fixtures above 100 values in tests.** Root-cap and fan-out tests that build large scopes may now count more chunks. → Task 3 audits them. An expectation changes only where the fixture crosses 100 values in one alternation.

## Migration Plan

No data or configuration migration. Deploy as a normal release. Rollback is a revert. The release note states D6.
