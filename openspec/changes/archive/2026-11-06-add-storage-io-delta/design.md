# Design

## Context

See proposal.md — Why. The relevant current state, much of it laid down by `add-storage-io-rollup`:

- The six QoS workload families are read only in the scoped second wave, `readScopedQoS` (`pkg/build/qosscope.go`). It waits on `kube_persistentvolumeclaim_info` and `volume_labels`, computes the `volume` scope, chunks it with `promql.ChunkQoSVolumeScope(scope, opts.qosScopeBatchBytes())` (byte budget plus `promql.MaxAlternationValues`), and issues one `instantQoSChunk` per (family, chunk), rendered by `promql.RenderQoSVolumeScopedRollup`. Chunks merge in chunk order. Every query is evaluated at the aligned `end`, and the query NAME is the bare family, so self-metrics carry one label value per family.
- The reader (`resolveNetAppStorage`, `pkg/build/netapp.go`) indexes the six vectors into one `qosIndex` (`indexQoSFamily`), then per claim runs `preferQoSZone(qosCandidatesFor(...))` and `sumQoSIO`, which discards LUN rows and sums per family in sorted order. `resolveCeiling` reads the same candidates. The result is one `*graph.IOMetrics` per claim on its `pvc-to-netapp-aggr` hit, which the storage build copies onto the claim's `svm-pvc` edge.
- `graph.ProjectStorage` (`weightRetained` → `sumUnitShares` → `scaleFlow` / `addPtr`) sums the four flow figures over retained units, splitting by mounter count, and copies latency and ceiling from the claim onto `svm-pvc` only. `cytoscape.metricsDTO` rounds every field with `round6` and lets only measurements set `filled`; the ceilings deliberately do not.
- `topologyPlan.ioRollup` is set only by `BuildStorageWith`; `fullPlan` and `k8sPlan` leave it zero, so `/v1/graph` and `/v1/k8s-graph` cannot reach it. `kubegraph.ParseStorageValues` reads `io_rollup` after the root with `parseIORollup` (`validateSelectorValues`, `nonEmptySorted`, conflict → `invalid_scope`).
- `docs/upstream-metrics.md` states that no query joins two families, so every query stays selector-only and portable across Prometheus-compatible stores.
- The default QoS byte budget (`DefaultQoSScopeBatchBytes`, 8192) is sized for ONE alternation per query under the common VictoriaMetrics `-search.maxQueryLen` default.

## Goals / Non-Goals

**Goals:**

- One build-level switch threaded parser → options → plan → scoped QoS read, `false` as the zero value at every layer, so no existing query, body, golden or fan-out pin moves.
- Reuse the measurement's reader rules for the delta without forking them, adding only the one gate the delta needs (D2).
- Purely additive Go API: no exported signature change, no interface change, no new mock.

**Non-Goals:**

- Capacity growth (`data.usage` deltas), window totals (`integrate()`), smoothed endpoints (mean of a sub-window at each end), and percent change.
- A delta on `/v1/graph`'s `pvc-to-netapp-aggr` edge, on ceilings, on aggregate or controller gauges, or on controller performance counters.
- Making a tier delta equal to the tier's end weight minus its start weight when claims appear or disappear inside the window (see Risks).

## Decisions

### D1. Subtract per series upstream, with standard PromQL

Each delta query is

```promql
last_over_time(<family>{volume=~"…"}[w]) - last_over_time(<family>{volume=~"…"}[w] offset w)
```

evaluated at the aligned `end`. The right-hand side is the last sample in `(start - w, start]`, which is exactly what `io_rollup=last` reads for a window ending at `start`; so for a one-workload claim carried by one series throughout, the delta equals the difference of two plain requests (the spec's "Equal to two plain requests" scenario, and an integration-test oracle). The two operands have identical label sets apart from `__name__`, which arithmetic matching ignores, so the subtraction is one-to-one. A series with no start sample has no match and drops out — the "absent, never 0" rule falls out of the query. The query does NOT catch a label change inside the window: the old series matches itself across `start` and returns its stale change, so the reader gates it (D2).

It is two rollups of ONE selector, not a join of two families, so the "no query joins two families" rule and its portability argument still hold.

- *Alternative — PromQL `delta()` / `idelta()`.* Rejected: Prometheus `delta()` extrapolates to the window edges, and VictoriaMetrics' `delta()` reads the sample before the window only within its staleness interval and falls back to in-window heuristics. The same request would answer differently per store, and neither equals the two-request difference.
- *Alternative — MetricsQL `first_over_time`.* Rejected: VictoriaMetrics-only, and "the first sample after `start`" is not the value at `start`; a volume created inside the window would report its whole first rate as change.
- *Alternative — a start-value leg (`last_over_time(x[w] offset w)`) subtracted in Go.* Rejected: under `io_rollup=max` / `avg` the measurement legs are not the end value, so it costs twelve extra queries instead of six, and the per-series subtraction would move into Go for no gain — the upstream already pairs the two operands one-to-one. (The D2 gate compares `__name__`-insensitive fingerprints only to test set equality; it pairs nothing.)

### D2. The same reader, behind one label-change gate

The six delta vectors land in six new `topologyVectors` fields. `resolveNetAppStorage` indexes them into a second index with the same `indexQoSFamily`, and per claim runs the same `preferQoSZone(qosCandidatesFor(...), claim zone, keyOC, svm)` and `sumQoSIO` over it. The resulting `IOMetrics`' six measurement fields become the claim's six delta fields on its existing `IOMetrics`, attached only when that `IOMetrics` is non-nil — a delta never creates a measurement. `resolveCeiling`, the coverage counters (`qosPresent`, `qosMisses`) and `applyCeiling` read the measurement rows only.

**One gate, before any claim reads the delta index.** A series whose labels change inside the window (a policy group moved, an `svm` label added or renamed, a poller relabelled) ends as one series and continues as another. The OLD series has samples on both sides of `start`, so the upstream returns its stale change; the new one has no start sample and drops out. Read unchanged, the stale row would pass as the workload's delta. `keepUnchangedDeltaRows(deltaIndex, qosIndex)`, called right after the six delta `indexQoSFamily` calls, groups both indexes by `deltaGateKey` (ONTAP cluster, `volume`, `lun`, family — across every `svm` and every zone) and keeps a group's delta rows only when the set of distinct series in the delta index EQUALS the set in the measurement index; otherwise the whole group contributes no delta. The key is `qosWorkload` without the `svm`: a series whose `svm` label appears, disappears or is renamed would otherwise start a group of its own, and the old series' group would compare equal and pass its stale change. The ONTAP cluster stays so two filers never mix, and the `lun` stays so a LUN workload's churn never touches its volume's delta. A series is `qosCandidate.series`, the fingerprint of its label set minus `__name__` (`seriesFingerprint`), because a range function or an arithmetic result keeps or drops the metric name per store (VictoriaMetrics keeps it on `last_over_time` and drops it on `-`); the cached `Metric` is cloned before the name is deleted, never mutated. The fingerprint is computed only in a build that read at least one delta row, so `/v1/graph` and a storage build without `io_delta` pay nothing for it.

The delta's left operand reads the same window and selector as the measurement, so delta series are a subset of measured series, and the sets differ when a series of the group appeared inside the window without a start value: a relabel, a re-zoned poller, or a new poller of any zone that started scraping mid-window. All three leave the workload's start value unknown, so absence is the honest answer. Equal sets do not prove the change is fresh; the cases the gate cannot see are listed under Risks. A delta row naming a series the measurement lacks (two reads of a moving upstream) fails the same test. The gate is per group, never per claim: a claim's other FlexVols, its LUN workloads and its other families keep their deltas. The measurement, the ceiling, the policy pick and the coverage signals never read the delta index, so the gate cannot change them.

The zone choice then runs on the delta rows that survived the gate. A surviving group's delta series are exactly its measured series, so each workload has the same zones in both reads and `preferQoSZone`, deterministic over the claim's zone, that zone set and `qosInScope`, keeps the same zone for the delta as for the measurement.

- *Alternative — a query-side gate: join each delta series on a presence or `count by` of the workload identity without `policy_group`.* Rejected: more PromQL per family and chunk, an identity that has to be hand-maintained as label lists, store-dependent `__name__` handling in the join, and the reader already holds both reads.
- *Alternative — key the gate on `qosWorkload` itself (with the `svm`).* Rejected: a series whose `svm` label appears or is renamed inside the window lands in a group of its own, and the old series' group passes its stale change; `qosInScope` admits an `svm`-less row for every claim, so the claim reads it. Dropping the `svm` can only drop more rows, and the case it newly drops — one FlexVol name in two SVMs of one filer, one of them first scraped inside the window — is near-impossible under Trident's PV-UUID names.
- *Alternative — gate per (workload, zone), or restrict delta rows to the exact (workload, zone) the measurement kept.* Rejected: a relabel of `az` / `env` itself (a re-zoned poller) would look like an old zone with a valid change and a new zone with none, and the stale change would pass. Comparing across zones catches it, at the cost below (Risks).
- *Alternative — compare only the series count per workload.* Rejected: equal to the set test only while delta series are a subset of measured series; two reads of a moving upstream can return a delta series the measurement lacks beside a measured series the delta lacks, equal counts over different sets, and a fingerprint set costs no more.
- *Alternative — a delta reader of its own.* Rejected: two copies of the LUN discard, scope test and zone order would drift; the gate is one function in front of the shared reader.

### D3. The delta reuses the family's query name

Delta queries are issued under the bare family name (`qos_read_ops`, …), like every chunk and every rollup today. Routing (`queryFamily` → `harvest`), matcher rendering (`queryDims` → `dimsHarvest`, no request matcher) and the fail-closed error (`upstream query failed: qos_read_ops`) all follow with no new `Query` constant, no new arm in `promql.Render`, and `render-baseline.txt` untouched.

- *Alternative — six new `Query` constants (`qos_read_ops_delta`, …).* Rejected: six entries each in `queryDims`, `queryFamily`, the exhaustive `Render` switch (as explicit empty arms, the `app_metadata` precedent) and the baseline-test exclusion list, for the sole gain of separating self-metric label values. Cost recorded under Risks.

Consequence for tests: the name no longer identifies the answer. `pkg/internal/promqlfake` answers by name alone, so it gains a separate delta fixture set served when the query has the delta shape (an ` offset ` operand); `internal/api`'s `fixtureSet` dispatches by query substring and fails on two matching needles; every substring of a `last` measurement query is also a substring of its delta query, so it gains a delta-aware rule (a needle marked for the delta shape matches only delta queries, and an unmarked needle only non-delta ones), keeping every existing fixture's behaviour.

### D4. Delta chunks halve the byte budget

A delta query renders the `volume` alternation twice. The delta read chunks the same sorted scope with `promql.ChunkQoSVolumeScope(scope, budget/2)` when the budget is positive (unchanged when it is not), so the two alternations of one query together stay within the budget the operator set for one, and each alternation still holds at most `MaxAlternationValues` values. Delta chunks are issued in the same wave, under the same `scopeConcurrency` limit, and merged in chunk order. The measurement chunks are unchanged, so `io_delta=false` issues byte-identical queries.

- *Alternative — reuse the measurement chunks.* Rejected: a scope near the budget would produce delta queries near twice `-search.maxQueryLen`'s common default and fail closed.

### D5. Six delta fields on `graph.IOMetrics`, summed by the projection

`graph.IOMetrics` gains `ReadOpsDelta`, `WriteOpsDelta`, `ReadLatencyUsDelta`, `WriteLatencyUsDelta`, `ReadBytesPerSecDelta`, `WriteBytesPerSecDelta` (`*float64`). `scaleFlow` divides the four flow deltas by `n` alongside their base figures, `sumUnitShares` adds them with `addPtr` in the existing ascending contribution order, and the claim-level branch copies the two latency deltas with the latency. A unit whose base figure is present but whose delta is nil contributes nothing to the delta sum — the tier delta covers the claims that carry one. `cytoscape.EdgeMetricsDTO` gains the six `omitempty` fields (`read_ops_delta`, …); `metricsDTO` rounds them with `round6` and, like the ceilings, never lets them set `filled`.

`/v1/graph` needs no gate in the serialiser: its plan never issues the delta read, so its `IOMetrics` delta fields are always nil.

### D6. The switch is a bool on the existing options

```
pkg/build
  StorageOptions.IODelta bool          // zero value = off; BuildStorage passes StorageOptions{}
  topologyPlan.ioDelta bool            // set only by BuildStorageWith; fullPlan / k8sPlan leave it false
  readScopedQoS(..., rollup promql.Rollup, delta bool, prerequisites...)   // unexported

pkg/promql
  RenderQoSVolumeScopedDelta(q, window, keys, sel, volumes) (string, bool) // same ok rules as the rollup render

pkg/kubegraph
  StorageRequest.IODelta bool          // filled by ParseStorageValues
  BuildStorageFromValues               // passes IODelta through BuildStorageWith
```

A bool cannot hold an invalid value, so `BuildStorageWith` gains no new rejection. `ParseStorageValues` reads `io_delta` after `io_rollup` with a `parseIODelta` mirroring `parseIORollup`: `validateSelectorValues("io_delta", …)` first (never echo a hostile value), then `nonEmptySorted`; none → false, one `true` / `false` → that value, anything else or two distinct values → `&ParseError{"invalid_scope", "io_delta must be true or false, got …"}`. No new `build.Reason`, no new `mapBuildError` entry.

- *Alternative — `prune`'s single-value `v.Get`.* Rejected: `io_rollup` already set the storage endpoint's repeated-value rule (identical repeats accepted, conflicts rejected); one endpoint should not mix two rules.
- *Alternative — a fourth `io_rollup` value.* Rejected: a signed change cannot be a Sankey width, and it combines with every rollup rather than replacing one.

### D7. The delta vectors are not series-tallied

The delta vectors are not written to `ScopeIssued` or `ExtraSeriesCount`, so the per-family series tally and the build log's `largest_leg` keep describing the measurement read and do not move with `io_delta`. Each delta query is still observed per call by `kube_state_graph_upstream_query_result_series{query}` and the query-duration self-metrics, under its family name.

## Risks / Trade-offs

- [A delta is two single samples, each one Harvest poll's mean rate] → Documented as end-minus-start, not a trend; a client wanting a smoothed change compares two `io_rollup=avg` requests.
- [A tier delta is not the tier's end weight minus its start weight when a claim appears inside the window] → Spec scenario "A tier change covers the claims measured at both ends" states it; the claim-level fields stay exact.
- [A claim with several workloads, only some with a start value, sums a partial delta] → Spec scenario "A claim sums the workloads that have a delta"; a claim normally matches one volume-level workload.
- [A workload loses its whole delta when any of its series appeared inside the window without a start value: a label change (policy group moved, `svm` added or renamed, template change, poller relabelled or re-zoned) or a second poller, of any zone, that started scraping it mid-window — even when another zone's copy had a valid change; a Harvest exporter restart that changes its scrape labels does this for every workload on that poller] → Spec scenarios "A series whose labels changed has no delta", "An svm relabel inside the window leaves no delta" and "A series new inside the window leaves its workload with no delta"; the gate is per group, so other workloads keep their deltas, and the measurement is unaffected.
- [The gate cannot see every stale change] → Undetected, each passing the old series' change: a rename of a label the group is keyed on (a FlexVol renamed to another name the claim still matches, an ONTAP cluster or a `lun` renamed), which starts a group of its own; a series that moved away before `start` and back inside the window, whose two series both have samples on both sides of `start`; and a measurement served from the result cache (keyed by query text and instant, so an `io_delta=false` request can fill it) read before a relabel within ingestion lag of `end` that the fresh delta read cannot see either. Each needs a rare relabel at an unlucky moment; detecting them would need sample timestamps (`tlast_over_time`, VictoriaMetrics-only) or a per-claim gate that contradicts "A claim sums the workloads that have a delta".
- [`start - w` beyond retention returns no start values, so every delta is silently absent] → Documented in `docs/upstream-metrics.md`; absence is the honest answer, and the body still carries every measurement.
- [The delta read scans `2w` per series] → Only with `io_delta=true`; VictoriaMetrics search limits bound it as for every other leg.
- [Delta queries share the family's self-metric label and error message] → A failure still names the right family and backend set; the full query text is on the query span's `db.statement` and in the `promql query failed` log line, so a delta failure is still told apart.
- [Summing mixed-sign deltas can leave a float residue such as `1.42109e-14` where the exact answer is `0`] → Deterministic (ascending-order sums); accepted rather than adding an epsilon rule to the wire contract.
- [More delta chunks than measurement chunks for a large scope] → Pinned by the fan-out tests with `io_delta=true`; `io_delta=false` is pinned unchanged.

## Migration Plan

Additive; nothing to migrate. Without `io_delta` (or with `false`) every request, body and rendered query is byte-identical — the existing goldens, `pkg/promql/testdata/render-baseline.txt` and `pkg/build/testdata/storage_fanout.golden` are the proof and MUST NOT be regenerated. Rollback is reverting the change; a client that sends `io_delta=true` then gets it ignored as an unknown parameter and receives no change fields.
