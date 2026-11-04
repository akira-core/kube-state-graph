# Proposal

## Why

A `/v1/storage-graph` body weights its Sankey with each claim's Harvest QoS figures read as `last_over_time(...[window])` — the value at the window END — while every other object in the body (pods, bindings, claims) is "anything seen during the window". A caller asking "what did this filer push over the last hour" therefore gets the last scrape, and a short burst or a quiet tail decides the whole picture. Operators need the same window summarised as its last value, its peak, or its mean, and the Go engine's embedders need the same choice in-process.

## What Changes

- `GET /v1/storage-graph` accepts an optional `io_rollup` parameter: `last` (default), `max` or `avg`. It selects the PromQL range function — `last_over_time`, `max_over_time`, `avg_over_time` — applied to the six Harvest QoS workload queries (`qos_{read,write}_{ops,latency,data}`) and to nothing else.
- Absent or empty `io_rollup` is `last`, so every existing request, body, golden and upstream query string is unchanged.
- An unrecognised value, or two different values, is `400 invalid_scope`, checked after the root. A repeated identical value is accepted.
- Each QoS series is rolled up over the window FIRST; the existing per-claim sum, zone preference, LUN discard and tier-to-tier summation then run unchanged over the rolled-up values. Under `max` that makes each tier the sum of per-claim peaks (an upper bound on the tier's real peak, since peaks need not coincide), and latency under `avg` is an unweighted time mean; both are documented, not corrected.
- Unaffected: the QoS fixed-policy ceilings, aggregate and controller gauges, controller performance counters, kubelet usage and every Kubernetes family keep `last_over_time`. `GET /v1/graph` keeps `last` for its `pvc-to-netapp-aggr` I/O and ignores `io_rollup` like any unknown parameter; `GET /v1/k8s-graph` reads no Harvest and ignores it too.
- The body does not echo the rollup (no echo-of-input field).
- Go embedders get the same choice without breaking any existing call: a `promql.Rollup` type (`last` / `max` / `avg`, zero value = `last`) with a parser, an `IORollup` field on `kubegraph.StorageRequest` filled by `ParseStorageValues`, and sibling build methods `build.Builder.BuildStorageWith` / `kubegraph.Engine.BuildStorageWith` taking a `build.StorageOptions`. `BuildStorage` keeps its exact signature and means `last`. `Engine.BuildStorageFromValues` honours `io_rollup` with no embedder change. A rollup value outside the three is rejected before any query.
- The pod set of a storage body is unchanged and now stated explicitly: every pod name with a sample in the window, collapsed to its newest incarnation, drawn only when it mounts a reached claim — including the consequence that a window spanning a Deployment rollout gives one claim two mounters and splits its weight between them.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `storage-graph-api`: new `io_rollup` request parameter (values, default, validation and order, ignored on the other endpoints), the determinism tuple gains the rollup, flow weights are stated per rollup, the in-process engine surface exposes the rollup without breaking existing callers, and the window semantics of the pod set are stated as a scenario.
- `netapp-storage-graph`: the six QoS workload queries are rendered with the build's rollup function instead of always being evaluated at the window end; `/v1/graph` always uses `last`; the ceilings are never rolled up.

## Impact

- **Code**: `pkg/promql` (new `Rollup` type and parser; the scoped QoS render takes the rollup), `pkg/build` (`StorageOptions`, `BuildStorageWith`, the storage plan carries the rollup to `readScopedQoS`, rejection of an invalid value), `pkg/kubegraph` (`StorageRequest.IORollup`, `ParseStorageValues`, `Engine.BuildStorageWith`, `BuildStorageFromValues`), `internal/api` (the storage handler passes the parsed rollup; swag annotation for `io_rollup`).
- **API**: additive. One new optional query parameter on `GET /v1/storage-graph`; no response-shape change. `docs/swagger.{json,yaml}` regenerated.
- **Go API**: additive only — no exported signature changes; new exported identifiers in `pkg/promql`, `pkg/build`, `pkg/kubegraph`. Built and tested on the `embedder-go` CI toolchain (Go 1.27.1).
- **Upstream load**: unchanged query count and fan-out. `max_over_time` / `avg_over_time` scan the same samples as `last_over_time` over the same range; each rollup is a distinct rendered string, so the query-result cache keeps them apart.
- **Docs**: `docs/upstream-metrics.md` (QoS query form per rollup and its semantics). No `docs/BREAKING.md` entry.
- **Tests**: goldens and `pkg/build/testdata/storage_fanout.golden` unchanged; new parser, render, build and example tests.
