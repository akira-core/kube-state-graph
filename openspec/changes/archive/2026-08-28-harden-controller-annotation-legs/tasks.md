## 1. Equivalence pins (write first — they must pass before AND after)

- [x] 1.1 Add `TestResolveJobCronJobOwners_QuerySelectorIsOutputPreserving` to `pkg/build/topology_test.go`: build one vector holding CronJob-owned controller rows PLUS the rows the new selector excludes (`owner_kind="Job"`, `owner_kind="CronJob"` with `owner_is_controller="false"`, and a row missing `owner_is_controller`), and a second vector holding only the rows the selector admits; assert `resolveJobCronJobOwners` returns an identical map for both, and that the `missingClusterCounts` tally is identical too; verify it passes against the CURRENT unfiltered query.
- [x] 1.2 Add `TestResolveApplications_TrackingIDPresenceIsOutputPreserving` to `pkg/build/topology_test.go` covering the same property for the six annotation families: a vector with empty-tracking-id and absent-tracking-id series alongside annotated ones yields the same index and the same tally as the annotated-only vector (design Context — the reader skips before `keyOf`, so `mc.bucket` never sees them); verify it passes against the CURRENT unfiltered query.

## 2. PromQL fixed selectors

- [x] 2.1 Add `jobOwnerCronJobSelector` and `argoTrackingIDPresentSelector` constants to `pkg/promql/queries.go` beside `qosVolumeGranularitySelector` / `serviceGraphSentinelSelector`, each with a doc comment stating it is a request-invariant metric-selection contract and NOT a caller filter, and naming the reader whose behaviour it mirrors (design D1); verify `go build ./...` succeeds.
- [x] 2.2 Wrap the `QJobOwner` `Render` case in `braces(jobOwnerCronJobSelector)`; verify `go test ./pkg/promql/ -run TestRender` fails only on the baseline mismatch, not on a syntax error.
- [x] 2.3 Wrap all six controller-annotation `Render` cases in `braces(argoTrackingIDPresentSelector)`; verify each rendered string is `last_over_time(<metric>{annotation_argocd_argoproj_io_tracking_id!=""}[w])` for the empty selector.
- [x] 2.4 Update the seven rendered lines in `pkg/promql/testdata/render-baseline.txt`; verify `go test ./pkg/promql/ -run TestRender_EmptySelectorMatchesBaseline` passes and `git diff` on that file shows exactly seven changed lines.
- [x] 2.5 Extend `TestRender_ComposesFixedSelectorFirst` in `pkg/promql/selector_test.go` with a `kube_job_owner` case and a `kube_deployment_annotations` case asserting the fixed selector precedes the request matchers (spec scenario "Controller-annotation fixed selectors precede the request matchers"); verify both subtests pass.
- [x] 2.6 Confirm `TestQueryDims_ControllerAnnotationFamiliesAreNamespaced` still passes unchanged in its dims half, and update its rendered-form half to expect the fixed selector ahead of the four request matchers; verify the test still fails when a family is switched to `dimsClusterScoped` (the regression it exists to catch must survive this edit).

## 3. Degrade the two accumulating-cardinality legs

- [x] 3.1 Change the `kube_replicaset_annotations` and `kube_job_annotations` legs in `ReadTopology` from `g.Go(fetch(...))` to `g.Go(fetchOptional(...))`, leaving the other five controller legs on `fetch` (design D3); verify the fan-out split is now 20 `fetch` + 17 `fetchOptional` via `grep -c` on each and that the total is still 37.
- [x] 3.2 Confirm `TestReadTopology_FanOutLegCount` still asserts 37 and passes unchanged (the leg count does not move, only the failure mode); verify `go test ./pkg/build/ -run TestReadTopology_FanOutLegCount` passes with no edit to the test.
- [x] 3.3 Add `TestReadTopology_AccumulatingAnnotationLegDegrades` to `pkg/build/netapp_test.go` (beside the existing optional-leg tests): a `MockQuerier` that errors on `kube_replicaset_annotations` and on `kube_job_annotations` while every other leg succeeds; assert `ReadTopology` returns no error and both vectors are empty (spec scenario "Accumulating-cardinality family degrades on a query error").
- [x] 3.4 Add `TestReadTopology_RequiredAnnotationLegFailsBuild`: a `MockQuerier` that errors on `kube_deployment_annotations`, then one that errors on `kube_job_owner`; assert `ReadTopology` returns an error in both cases (spec scenario "A required annotation family still fails the build").
- [x] 3.5 Add `TestReadTopology_DegradingLegHonoursCallerCancellation`: an already-cancelled caller context with `kube_job_annotations` erroring; assert `ReadTopology` returns an error rather than degrading (spec scenario "Caller cancellation still fails a degrading family", exercising `optionalQueryFatal`'s `callerCtx` branch).
- [x] 3.6 Keep the degrade subtractive (design D5): add `topologyVectors.JobAnnotationsDegraded`, split `fetchOptional` into `fetchOptionalTracking(name, dst, degraded *bool)` that sets the flag ONLY on the swallowed-error path (every other optional leg passes `nil` through the thin `fetchOptional` wrapper), wire `kube_job_annotations` to it, and gate the Job → CronJob hop in `resolvePodApplications` on the flag being clear; verify `go build ./...` succeeds and no other leg's behaviour moves.
- [x] 3.7 Add `TestReadTopology_DegradedJobAnnotationsSuppressCronJobHop` to `pkg/build/netapp_test.go` as a matched PAIR of subtests over one fixture — a Job carrying its own tracking-id under a CronJob carrying a different one: with the leg erroring the pod resolves NO Application (and its `owner` still names the Job), with the leg returning an empty vector the hop still resolves the CronJob's Application (spec scenario "A degraded Job annotation family suppresses the CronJob hop"). Verify the first subtest fails before 3.6 and passes after, and that the second passes throughout — without the pair a broken fixture would make the suppression look correct.

## 4. Re-run the equivalence pins against the new queries

- [x] 4.1 Re-run the two tests from group 1 unchanged; verify both still pass — they are the proof that the pushdown changed no output, so a failure here means a matcher and its reader have diverged and the matcher is wrong.
- [x] 4.2 Run `go test ./internal/api/ -run TestGolden` WITHOUT `-update`; verify it passes and `git status` shows no change under `internal/api/testdata/golden/`.
- [x] 4.3 Run `go test ./internal/integration/ -run TestGraphSuite` with Docker available; verify the pod-Application and CronJob-hop cases still pass against real VictoriaMetrics, which is the only layer that exercises the rendered PromQL end to end.

## 5. Documentation

- [x] 5.1 Add the seven new fixed selectors to the fixed-selector table in `docs/upstream-metrics.md`, each with the "why" column explaining what the reader discards; verify the table lists every selector `Render` emits.
- [x] 5.2 Update the query-error matrix in `docs/upstream-metrics.md` from "22 kube-state-metrics topology queries → Fails the build" to the 20 / 2 split, naming the two degrading families and the accumulating-cardinality criterion (design D3); verify the matrix rows sum to 37 legs.
- [x] 5.3 Update the seven series rows in `docs/upstream-metrics.md`'s kube-state-metrics table to show the fixed selector in the metric column, matching how `kube_node_status_addresses` and the `qos_*` rows already display theirs; verify each row's selector matches the `Render` output byte-for-byte.
- [x] 5.4 Document the `RawSeriesCount` meaning change for the six annotation families in `docs/kube-state-metrics-preconditions.md`, next to the existing per-family verification probe it now equals (design D2); verify the doc states that `0` means "no annotated objects reach me", not "collector off".
- [x] 5.5 Update the abort-vs-log-and-continue sentence in `README.md` and `README.zh-tw.md` (currently "a query error on any of the 22 kube-state-metrics legs … fails the build") to the 20 / 2 split; verify both READMEs state the same counts and that no other count in either file drifts.
- [x] 5.6 Update the `CLAUDE.md` architecture bullet describing the topology fan-out so the abort/degrade split reads 20 + 17 rather than 22 + 15; verify the stated total is still 37.
- [x] 5.7 Document the hop suppression (design D5) in `docs/upstream-metrics.md`'s query-error matrix, `docs/BREAKING.md`'s operator-visible-outcome section, and the `CLAUDE.md` pod-`application` bullet, replacing the earlier "the degrade is NOT purely subtractive / the hop fires anyway" wording; verify no doc still claims a degraded `kube_job_annotations` attributes a pod to its CronJob.

## 6. Verification gate

- [x] 6.1 Run `make vet` and `make lint`; verify both report clean.
- [x] 6.2 Run `make test`; verify the full unit / component / golden / property suite passes with `-race -shuffle=on`.
- [x] 6.3 Run `make check-docs`; verify no OpenAPI drift (no handler or annotation changed, so the generated spec must be byte-identical).
- [x] 6.4 Run `openspec validate "harden-controller-annotation-legs"`; verify it reports the change valid with no outstanding artifact issues.
