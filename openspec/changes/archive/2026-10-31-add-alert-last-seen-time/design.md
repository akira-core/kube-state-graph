# Design

## Context

The `ALERTS` leg is rendered as `last_over_time(ALERTS{alertstate="firing",…}[w])` and evaluated as an instant query at the aligned `end`. Each returned sample's value is always `1`, and the alert reader in `pkg/build/alerts.go` never reads it: it uses only the label set. Matched alerts become `graph.Alert{Name, State, Severity}`. `graph.SortAlerts` sorts them and removes duplicates on `(Name, Severity)`, keeping the first entry. `pkg/cytoscape` copies each one into `AlertDTO`. All three build paths attach alerts in the shared `Builder.freeze`, so one change there covers `/v1/graph`, `/v1/storage-graph`, `/v1/k8s-graph` and the in-process engine.

`promql.Querier` only returns instant vectors (`model.Vector`). The router, cross-backend merge, result cache and mocks are built around that type.

The backend already uses the MetricsQL `tlast_over_time` for two optional legs: `kube_pod_container_info` (picking the newest image) and the application-metadata metric (`deployed_at`). The integration suite already exercises it against a real VictoriaMetrics.

## Goals / Non-Goals

**Goals:**

- One timestamp per alert entry, taken from data the leg already reads, with no extra upstream query.
- The output stays a pure function of `(window, filters, upstream data)`, so goldens stay byte-stable.

**Non-Goals:**

- Activation time (`activeAt`), firing intervals, or every sample time. The user explicitly limited this change to the last occurrence.
- Changing the `state` field, alert matching, or the status fold.
- A Prometheus-portable *timestamp*. A portable store keeps the overlay through the fallback (D1) but gets no `time`.

## Decisions

### D1 — `tlast_over_time` on the existing leg

Rewrite the `QAlerts` render to `tlast_over_time(ALERTS{…}[w])`. It has the same series selection as `last_over_time` with the same selector (its label sets may lack `__name__`, which no reader keys on); the only difference is the value, which becomes the last-sample Unix timestamp in seconds. The reader then reads `s.Value`.

**Fallback.** When the query is rejected as invalid — the client reports a `bad_data` API error, which is what Prometheus answers for an unknown function — the leg retries ONCE with the previous portable render, `last_over_time(ALERTS{…}[w])`, with the same selector and the same query name (`ALERTS`), so self-metrics and spans are unchanged. That keeps every alert and its `data.status` contribution on a Prometheus backend; only `time` is lost, because the portable value `1` falls outside the window (D3). Any other error (dial, timeout, 5xx) does not retry, so a down store does not pay two timeouts per build. A caller-context error still fails the build exactly as before. The fallback is a separate exported render, not a new `Query` constant, so the `queryDims` / `queryFamily` tables and the render baseline are unchanged.

The fallback is per LEG, not per backend: the fan-out fails closed on any backend's error, so a mixed fleet (one Prometheus and one VictoriaMetrics `alerts` backend) retries the whole leg portably and loses `time` on every alert. A per-backend fallback would need the fan-out to know about query variants; it is not worth it for a configuration the project does not document.

Alternatives considered:

- **`max_over_time(timestamp(ALERTS{…})[w:step])`** (standard PromQL subquery). Portable to Prometheus, but its precision is the subquery step rather than the actual sample time, and a subquery over a long window is much heavier on the store. A second leg would also be needed unless the existing one were replaced, and replacing it would change how presence is detected. Rejected; the project's upstream is VictoriaMetrics.
- **A range-vector (matrix) read.** Gives the raw samples but needs a new `Querier` method, a matrix-aware merge and cache, and regenerated mocks. That is out of proportion for a single timestamp. It is the path to take if firing intervals are ever wanted.

### D2 — Wire form is an integer epoch second named `time`

This was the user's choice. Integer seconds sort and subtract directly in a consumer and match the unit of the `start` / `end` request parameters. It intentionally differs from `deployed_at` (an RFC 3339 string on application groups): deployment time is mostly read by people, while alert recency is mostly computed with.

Fractional seconds are truncated toward zero, so the value never claims a moment later than the sample. The name `time` is generic, so the spec pins its meaning as **last seen firing in the window**, not activation.

### D3 — In-process type is `time.Time`

`graph.Alert` gains a `time.Time` field. A zero value means unresolved. This matches `graph.ApplicationMeta.DeployedAt` and keeps units out of the in-process API. The conversion to epoch seconds happens only in `pkg/cytoscape`.

A value that is NaN, ±Inf, below one second, or outside the request window `[start, end]` (whole seconds, both ends inclusive) resolves to the zero time, and the DTO then omits `time` (`int64` with `omitempty`). The alert is still attached: a missing timestamp is not a reason to drop the alert. The window bound is what turns the portable fallback's constant `1` — or an embedder's vector still carrying it — into "unresolved" instead of `"time":1`: a last sample inside the window cannot lie outside it. The build carries the window on `Topology` for this; a hand-built `Topology` with no window skips the bound and keeps the other checks.

### D4 — Collapse keeps the latest time

`SortAlerts` currently keeps the first entry of each `(Name, Severity)` group. It will keep the greatest time in the group instead. Taking the maximum does not depend on input order, so the body stays deterministic whatever order the vector held its series in. `Name`, `State` and `Severity` are equal within a group by construction (`State` is always `firing`), so only the time needs merging.

The same rule applies one layer down. When two backends serving the `alerts` family hold the same series (HA vmalert writing to both), `mergeVectors` keeps the copy with the GREATER value for that family instead of the lexically-smallest backend's. Otherwise a stalled remote write into the first backend would show an alert that is still firing as last seen long ago, and `SortAlerts` could not fix it because the second copy was dropped before the reader. Every other family keeps first-backend-wins; the disagreement is still counted and logged at Debug. The merge swaps which sample it keeps and never writes to one: cached samples are shared.

### D5 — Status fold untouched

`FoldStatus` keeps reading `Severity` only. An alert last seen early in the window still counts at full severity: the window's contract is "what was true in `[start, end]`", and adding recency to the verdict would be a different feature.

## Risks / Trade-offs

- [A plain Prometheus `alerts` backend rejects `tlast_over_time`] → The leg falls back to the portable read (D1): alerts and status survive, `time` is absent. Errors are never cached, so every build issues the rejected query first: the client logs one ERROR (`promql query failed`), the build one Warn (the retry), and `kube_state_graph_upstream_query_failures_total{query="ALERTS"}` and `kube_state_graph_upstream_backend_query_failures_total{backend}` each grow by one per build; an operator alert on either counter would fire. Documented in `docs/upstream-metrics.md`.
- [Mixed fleet] → One Prometheus and one VictoriaMetrics `alerts` backend lose `time` on every alert, because the fallback is per leg (D1).
- [Precision is one rule-evaluation interval] → `time` is when the alerting engine last wrote a firing sample, not the moment the condition stopped being true. The spec says "last seen firing".
- [`time` is at most the aligned `end`] → `end` is floored to the `--end-align` grid (default 30s) and results are cached for 60s, so a sample in the most recent 30–90s may not show yet. Presence detection already has this lag; it is not new.
- [Cross-backend disagreement becomes visible] → When two `alerts` backends hold the same series, their timestamps can differ. The merge keeps the greater (D4) and logs the conflict at Debug; for this family conflicts are routine under HA, which is why it stays at Debug.
- [`tlast_over_time` drops `__name__`] → VictoriaMetrics keeps the metric name for `last_over_time` but not for most other rollups. The reader keys on labels other than `__name__`, so matching is unaffected; the docs say the series *selection* is the same, not the label sets.
- [Test fixtures carry value `1`] → with no window (a hand-built `Topology`), a sample stamped `Value: 1` would resolve to `"time":1`. The `internal/api` goldens build their alerts as hand-built `graph.Alert` nodes (not through `vec()`), so they carry a shared realistic in-window `Time`; the `pkg/build` fixtures that go through sample vectors use `alertSample` (value 0, unresolved) or an explicit-value helper. Goldens show meaningful values instead of 1970.

## Migration Plan

Additive for HTTP consumers: a new optional key in an existing object, on every upstream (a portable one omits it). Swagger is regenerated with `make docs`. Deploy with no configuration change. To roll back, revert the commit; there is no persisted state.
