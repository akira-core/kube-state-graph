# Tasks

## 1. Query render

- [x] 1.1 In `pkg/promql/queries.go`, render `QAlerts` as `tlast_over_time(ALERTS%s[%s])` and update its comment to say the value is the last-sample Unix timestamp (MetricsQL, the `QPodContainerInfo` precedent). Verify `pkg/promql/alerts_test.go` asserts the new string for the unfiltered and the `az` / `env` / `namespace` selectors.
- [x] 1.2 Refresh `pkg/promql/testdata/render-baseline.txt` and `pkg/build/testdata/storage_fanout.golden`, and update the literal `ALERTS` expectations in `internal/api/storage_graph_test.go`, `internal/api/k8s_graph_test.go` and `pkg/kubegraph/storage_namespace_test.go`. Verify `go test ./pkg/promql/ ./pkg/build/ ./pkg/kubegraph/ ./internal/api/ -run 'Render|Fanout|Storage|K8s|Namespace'` passes, and that the only diff in the baselines is `last_over_time(ALERTS` → `tlast_over_time(ALERTS`.

## 2. Graph model

- [x] 2.1 Add a `time.Time` last-seen field to `graph.Alert` (zero = unresolved), and make `graph.SortAlerts` keep the greatest time within each `(Name, Severity)` group (design D4). Verify with a `pkg/graph` unit test that feeds the same two duplicates in both orders and gets identical output carrying the later time.
- [x] 2.2 Check that `FoldStatus` ignores the new field. Verify with a `pkg/graph` test: a `critical` alert with an old time and one with a zero time both fold to `critical`.

## 3. Reader

- [x] 3.1 In `resolveAlerts` (`pkg/build/alerts.go`), set the entry's time from `s.Value`: truncate to whole seconds; NaN, ±Inf and ≤ 0 become the zero time and the alert is still attached (design D3). Verify with `pkg/build/alerts_test.go` cases for a normal value, a fractional value (truncated), and zero / negative / NaN / +Inf (attached, zero time).
- [x] 3.2 Add a `resolveAlerts` test where two series differ only in an unread label and carry different values. Verify the single entry carries the larger time.

## 4. Wire form and docs

- [x] 4.1 Add `Time int64 \`json:"time,omitempty"\`` to `cytoscape.AlertDTO` and set it in `alertsDTO` from the field's `Unix()`; a zero time serialises with no key. Verify with a `pkg/cytoscape` test asserting `"time":1761897720` for a resolved entry and no `time` key for a zero one.
- [x] 4.2 Give the hand-built alert fixtures in the `internal/api` goldens a shared in-window `Time` (they are `graph.Alert` nodes, not `vec()` samples, so no value-bearing helper is needed). Refresh every golden that carries `alerts` with `go test ./internal/api/ -update -run Golden`. Verify the golden diff is limited to new `"time"` keys holding the fixture values.
- [x] 4.3 Run `make docs` and commit `docs/swagger.{json,yaml}`. Verify `AlertDTO` lists `time` as an integer and that CI's docs-drift check passes locally (`make check-docs`).
- [x] 4.4 Update the `ALERTS` row in `docs/upstream-metrics.md` to `[{name, state, severity, time}]`, with `time` described as the last firing sample in the window in epoch seconds. Add a `docs/BREAKING.md` entry for a plain Prometheus `alerts` backend (superseded by 6.2 / 6.5: the portable fallback keeps the overlay, so the entry was removed). Verify the file renders the new text.

## 5. Integration

- [x] 5.1 In `internal/integration/k8s_graph_e2e_test.go`, assert that the `KgPodWarning` entry's `time` equals the fixture sample's timestamp (seconds) against the real VictoriaMetrics, which proves `tlast_over_time` returns the sample time end to end. Verify `go test ./internal/integration/ -run K8sGraph` passes with Docker available.
- [x] 5.2 Run `make ci` (lint, vuln, test, check-docs, verify-mocks, containment) and verify it is green.

## 6. Review follow-ups

- [x] 6.1 Window bound: carry the build window on `Topology` and pass it to `resolveAlerts`; a value whose whole second is outside `[start, end]` resolves to the zero time, and a `Topology` with no window skips the bound. Verify with `pkg/build/alerts_test.go` cases for in-window, before-start, after-end and value `1`, and an attached-but-untimed alert through a full build.
- [x] 6.2 Portable fallback: add `promql.RenderAlertsPortable` (same selector, `last_over_time`) and `promql.IsBadQuery` (a `bad_data` API error, reachable through the guard and fan-out wraps). In `topology_read.go`, retry the `ALERTS` leg once with the portable render on a bad-query error only. Verify: a mock answering the `tlast` string with `bad_data` yields alerts with no `time`; a dial error still yields no alerts and no second query; caller cancellation still fails the build.
- [x] 6.3 Cross-backend max: `mergeVectors` keeps the greater-valued copy of a duplicate for `FamilyAlerts`, without mutating a sample. Verify with a `pkg/promql` unit test over both part orders, and that other families still keep the first copy.
- [x] 6.4 Integration: assert `time` on the multi-backend alerts test against real VictoriaMetrics.
- [x] 6.5 Docs: remove the `docs/BREAKING.md` entry; describe the fallback, its per-build failure count, the per-leg mixed-fleet limit and the alerts merge rule in `docs/upstream-metrics.md`; add the alerts exception to the merge rule in `.claude/rules/upstream-queries.md`; say "same series selection" rather than "same series". Run `make ci` and verify it is green.
