# Proposal

## Why

A `/v1/storage-graph` body tells an operator how much I/O each claim and tier carries — at the window end (`io_rollup=last`), at its peak (`max`) or on average (`avg`) — but not whether that load went up or down across the window. "Did this filer's write ops rise between `start` and `end`, and which claims drove it?" today takes two requests and a client-side diff of every edge. The Harvest QoS families are already per-second rates (ops/s, bytes/s) and per-operation averages (µs), so the useful change signal is the difference of the rate itself: `50 ops/s → 100 ops/s` is `+50 ops/s`.

## What Changes

- `GET /v1/storage-graph` accepts an optional `io_delta` parameter, `true` or `false` (default `false`). Absent, empty or `false` changes nothing: every query, body, golden and fan-out pin stays byte-identical.
- With `io_delta=true`, every `storage-flow` edge that carries a measurement also carries signed change fields in `data.metrics`: `read_ops_delta`, `write_ops_delta`, `read_bytes_per_sec_delta`, `write_bytes_per_sec_delta` on every tier (summed and split exactly like the flow weights), and `read_latency_us_delta`, `write_latency_us_delta` on the claim-level `svm-pvc` edge only. Each is the value at the window end minus the value at the window start, in the unit of its base field; positive is up, negative is down, `0` is unchanged.
- The change is computed per QoS series upstream with standard PromQL, `last_over_time(x[w]) - last_over_time(x[w] offset w)`, then read through the existing reader (LUN discard, one zone per workload, per-claim sum) behind one gate: a workload one of whose series appeared inside the window without a start value (a relabel, a re-zoned or new poller) contributes no delta, because the upstream still returns the old series' stale change. A series with no sample in the `w` before `start` has no delta: the field is omitted, never `0`.
- The delta is independent of `io_rollup`: it is always the last value at `end` minus the last value at `start`, so under `io_rollup=max` the edge carries the peak beside the end-to-start change.
- `io_delta=true` adds the six QoS workload families' delta queries to the scoped QoS wave (each scanning `2w`). They fail closed like every other storage-build query. No other family, node, edge, label or attribute changes.
- An unrecognised value or two different values is 400 `invalid_scope`, validated after `io_rollup`, before any query. A repeated identical value is accepted. The body does not echo `io_delta`.
- `GET /v1/graph` and `GET /v1/k8s-graph` ignore the parameter like any unknown one.
- Go embedders get the same switch additively: `build.StorageOptions.IODelta`, `kubegraph.StorageRequest.IODelta` filled by `ParseStorageValues`, honoured by `Engine.BuildStorageFromValues`. `BuildStorage` and every existing call keep their behaviour.
- Out of scope: capacity growth (`data.usage` deltas on PVCs and aggregates), window totals (`integrate()`), and any delta on `/v1/graph`, ceilings, gauges or controller performance counters.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `storage-graph-api`: new `io_delta` request parameter (values, default, validation order, ignored on the other endpoints, not echoed), the delta fields on `storage-flow` edges and how they sum tier to tier, the determinism tuple gains `io_delta`, the by-reference read's per-family series tally excludes the delta reads, and the in-process engine surface exposes the switch without breaking existing callers.
- `netapp-storage-graph`: a `/v1/storage-graph` build with `io_delta` reads each QoS workload family's end-minus-start change per series over the same volume scope, chunked so the twice-rendered alternation still fits the byte budget, and the reader turns it into per-claim deltas with the same rules as the measurements, after a gate that drops the delta of a workload whose series set changed inside the window.

## Impact

- **Code**: `pkg/promql` (scoped delta render), `pkg/build` (`StorageOptions.IODelta`, the storage plan carries it to `readScopedQoS`, six delta vectors, per-claim delta from the existing reader), `pkg/graph` (`IOMetrics` gains six delta fields; `ProjectStorage` sums and splits them), `pkg/cytoscape` (six `omitempty` fields on `EdgeMetricsDTO`), `pkg/kubegraph` (`StorageRequest.IODelta`, `ParseStorageValues`, `BuildStorageFromValues`), `internal/api` (storage handler passes the flag; swag annotations).
- **API**: additive. One optional query parameter on `GET /v1/storage-graph`, six optional `data.metrics` fields present only when it is `true`. `docs/swagger.{json,yaml}` regenerated.
- **Go API**: additive only — new fields on exported structs, no signature change, no interface change, no new mock. Built and tested on the `embedder-go` toolchain (Go 1.27.1).
- **Upstream load**: unchanged without `io_delta`. With it, six QoS families × the delta chunks more queries, each reading twice the window; standard PromQL, so any Prometheus-compatible harvest store serves them.
- **Docs**: `docs/upstream-metrics.md` (delta query form, semantics, fan-out), `.claude/rules/storage-graph.md`, `.claude/rules/netapp-storage-join.md`, `CLAUDE.md` request-surface line. No `docs/BREAKING.md` entry.
- **Tests**: existing goldens, `render-baseline.txt` and `pkg/build/testdata/storage_fanout.golden` unchanged; new parser, render, reader, projection, serialiser, handler, example and VictoriaMetrics integration tests.
