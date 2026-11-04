# Design

## Context

See proposal.md — Why. The relevant current state:

- Every topology leg is an instant query at the aligned `end` over a range `[w]`, `w = end - start`. The six QoS workload legs are issued ONLY through `promql.RenderQoSVolumeScoped` (`pkg/promql/qosscope.go`), which hardcodes `last_over_time`, called from `instantQoSChunk` inside `readScopedQoS` (`pkg/build/qosscope.go`). The unscoped `promql.Render` arms for the same six constants (`pkg/promql/queries.go`) feed `render-baseline.txt` and are not on any build path for QoS.
- `readScopedQoS` is shared by `/v1/graph` (`fullPlan`) and `/v1/storage-graph` (`storagePlan`). `/v1/k8s-graph` never reaches it (`kubernetesOnly` closes `volumeLabelsFinal` and `refuseHarvest` wraps the querier).
- The reader (`sumQoSIO`, `preferQoSZone`, `pickPolicy`) consumes one value per series and does not know how that value was produced. `ProjectStorage` (`sumUnitShares`) sums ops/bytes across tiers and keeps latency and ceiling claim-level.
- `Builder.BuildStorage(ctx, window, end, sel, roots)` and `Engine.BuildStorage(...)` are exported `pkg/` signatures that embedders call; CI's `embedder-go` job builds, vets and tests `pkg/` on Go 1.27.1 with `GOTOOLCHAIN=local`.
- The query-result cache keys on `(store id, rendered query, eval instant)`.

## Goals / Non-Goals

**Goals:**

- One build-level knob, threaded parser → plan → scoped QoS render, with `last` as the zero value at every layer so no existing body, golden, fan-out pin or rendered query moves.
- Purely additive Go API: no exported signature changes, no interface widening, no new mock.

**Non-Goals:**

- Weighted latency (`sum(lat·ops)/sum(ops)`), percentiles, or any rollup beyond the three — each needs a subquery or a second series and is neither portable nor bounded.
- Per-tier "true peak" under `max` (peak of the aggregate's summed series). It would need the aggregate-level series summed upstream before the rollup — a different query shape per tier and a break of conservation.
- Rolling up ceilings, gauges, perf counters, kubelet usage or Kubernetes families.
- Changing which pods a body draws, or the RWX / rollout split rule.
- Exposing the rollup on `/v1/graph`.

## Decisions

### D1. The rollup is a function name, chosen at render time

`promql.Rollup` is a string type with constants `RollupLast = "last"`, `RollupMax = "max"`, `RollupAvg = "avg"`; the zero value `""` means `last`. A method maps it to `last_over_time` / `max_over_time` / `avg_over_time` and is the ONE place the mapping lives. `promql.ParseRollup(s string) (Rollup, error)` accepts the three literals and `""` (returning `RollupLast`) and rejects every other value; `Rollup.Valid()` accepts the three plus the zero value.

The scoped QoS render gains the rollup. `RenderQoSVolumeScoped` keeps its exported signature and renders `last`; a new `RenderQoSVolumeScopedRollup(q, window, keys, sel, volumes, rollup)` takes it explicitly, and the former delegates to the latter with the zero value. `readScopedQoS` calls the new one.

- *Alternative — change `RenderQoSVolumeScoped`'s signature.* Rejected: it is exported from `pkg/promql`, so the change would break any embedder rendering it.
- *Alternative — a `Rollup` field on `promql.Selector`.* Rejected: the selector is the REQUEST-matcher contract (`queryDims`, `Reaches`, `RequestMatcherCost`, cache keys of every leg); a field that only six queries read would leak into every one of them and into the `/v1/graph` path.
- *Alternative — an int enum.* Rejected: a string maps 1:1 to the wire value, so the parser, the error message and an embedder's config all spell it the same way.

### D2. The plan carries the rollup; `/v1/graph` cannot set it

`topologyPlan` gains `ioRollup promql.Rollup`. Only the storage path sets it; `fullPlan` and `k8sPlan` leave it zero. `readTopology` passes `plan.ioRollup` to `readScopedQoS` (one new parameter on an unexported function). `/v1/graph` therefore renders `last` structurally — there is no code path from its parser to the field.

### D3. Additive build entry points

```
pkg/build
  type StorageOptions struct { IORollup promql.Rollup }
  func (b *Builder) BuildStorage(ctx, window, end, sel, roots) (*graph.Graph, error)          // unchanged; = BuildStorageWith(..., StorageOptions{})
  func (b *Builder) BuildStorageWith(ctx, window, end, sel, roots, opts StorageOptions) (*graph.Graph, error)

pkg/kubegraph
  StorageRequest.IORollup promql.Rollup                       // filled by ParseStorageValues
  func (e *Engine) BuildStorageWith(ctx, window, end, sel, roots, opts build.StorageOptions) (*graph.Graph, error)
  BuildStorageFromValues                                      // passes req.IORollup through BuildStorageWith
```

`BuildStorageWith` rejects `!opts.IORollup.Valid()` with a `build.Error{Reason: ReasonInvalidScope}` before `plan.prepare`, i.e. before a querier is bound — the same place and class as `BuildK8s` rejecting a malformed root. It never coerces an unknown value to `last`.

- *Alternative — variadic `opts ...StorageOption` on `BuildStorage`.* Rejected: call sites compile, but `*Engine` / `*Builder` would stop satisfying any embedder-declared interface naming the old method signature, and `gorelease` reports the change as incompatible.
- *Alternative — functional options.* Rejected: one field today; a struct is simpler and still extensible by adding fields.
- `kubegraph` already exposes `pkg/promql` and `pkg/graph` types; exposing `build.StorageOptions` adds `pkg/build` to an embedder's imports only for the typed call. Acceptable: `pkg/build` is already a public package with no `internal/*` imports.

### D4. Parser placement and error

`ParseStorageValues` reads `io_rollup` after the root (`oneStorageRoot` + `NewStorageScope`), so the established order window → `az` → `env` → root is unchanged and a new parameter cannot pre-empt an existing error. Each value first passes `validateSelectorValues` — the 253-byte, UTF-8 and control-character bounds every selector parameter has — so a hostile value is rejected with the shared selector message and is never echoed back. Empty values are then dropped and the rest de-duplicated (`nonEmptySorted`, shared with `atLeastOne`); more than one distinct value, or a value `ParseRollup` rejects, is `&ParseError{"invalid_scope", "io_rollup must be one of last, max, avg, got …"}` — the `prune` precedent. The wording comes from `promql.InvalidRollupMessage`, the one source the parser, `BuildStorageWith` and `ParseRollup` share, so the accepted values are listed in one place (`rollupChoices`). No new `build.Reason` and no new `mapBuildError` entry. `internal/api`'s storage handler calls `BuildStorageWith(..., build.StorageOptions{IORollup: req.IORollup})`.

### D5. Roll up per series, then reuse the reader unchanged

The rollup happens upstream, per series; the Go reader, the zone choice, the LUN discard, the per-claim sum and the projection's tier sums are untouched. This keeps the change to a render point and is linear-correct for `last` and `avg`. For `max` the tiers become sums of per-series peaks; the specs state this instead of hiding it, and the Non-Goals explain why a true per-tier peak is out of scope.

### D6. The body does not echo the rollup

The deterministic-body rule forbids echo-of-input fields. The caller knows what it asked for; the determinism tuple gains `io_rollup` instead.

## Risks / Trade-offs

- [`max` reads as "the aggregate's peak" in a UI] → Specs and `docs/upstream-metrics.md` name it "sum of per-claim peaks"; any UI label is the consumer's.
- [`avg` latency is unweighted] → Documented; a spike with few reads weighs the same as a quiet period with many.
- [A series present for part of the window is averaged over its own samples] → Documented; a claim created late in a long window reports its own mean, not a diluted one.
- [Cache entries triple for a client cycling all three rollups] → Bounded by the existing series-count LRU; QoS vectors are small (one series per matched workload per family).
- [`max_over_time` / `avg_over_time` cost over a long window] → Same samples as `last_over_time` over the same range; VictoriaMetrics search limits still bound it.
- [The SPA's Sankey URL already has its own `mode`] → The wire name `io_rollup` avoids the collision; the frontend maps its own control onto it when it adopts the parameter.
- [An embedder passes a typo'd `promql.Rollup("Max")`] → `BuildStorageWith` returns `ReasonInvalidScope` before any query; `ParseRollup` is the supported way to turn a string into a value.

## Migration Plan

Additive; nothing to migrate. Without `io_rollup` every request, body and rendered query is byte-identical — the existing goldens, `render-baseline.txt` and `pkg/build/testdata/storage_fanout.golden` are the proof and MUST NOT be regenerated. Rollback is reverting the change; a client that sent `io_rollup` then gets it ignored as an unknown parameter and receives `last`.
