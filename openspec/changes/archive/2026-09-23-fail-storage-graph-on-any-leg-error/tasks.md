## 1. Error plumbing

- [x] 1.1 Add `build.QueryError{Query, Err}` (with `Unwrap`) and a `Query` field on `build.Error`; make `classifyReadError` copy the name through; verify `pkg/build/errors_test.go` cases for timeout, cancel and upstream wrapped in `QueryError`
- [x] 1.2 Wrap every required-path query error with its bare family name at the return sites in `readTopology` (`fetch`), `issueScopedFamilies`, `readScopedQoS`, `issueVolumeLabelsQueries` and the application recovery; verify a unit test per site asserts `errors.As` yields the family
- [x] 1.3 Make `mapBuildError` write `upstream query failed: <family>` when `Query` is set; verify `internal/api` component tests assert the message and that no URL / host appears for a querier error that embeds one

## 2. Fail-closed plan

- [x] 2.1 Add `failClosed` to `topologyPlan` (true on `storagePlan`, false on `fullPlan`) and `plan.errorClass(query, default)` returning required for everything but `promql.QAlerts` when set; verify a table test over every `topologyLegs` entry for both plans
- [x] 2.2 Route the first-wave launch switch, the by-reference `scopedFamily.mode`, the QoS chunk loop, the volume-label phases and the application recovery through `errorClass`; verify `pkg/build` tests where a storage build fails on a `volume_labels`, `qos_read_ops` chunk, `aggr_space_used`, `node_labels`, `kubelet_volume_stats_used_bytes`, `kube_replicaset_annotations` chunk, `kube_job_annotations` recovery chunk error, and does NOT fail on an `ALERTS` error
- [x] 2.3 Verify `/v1/graph` is unchanged: existing degrade tests (`TestResolveJobCronJobOwners_*`, QoS chunk degrade, volume-label degrade under `fullPlan`) still pass untouched

## 3. Non-failures

- [x] 3.1 Verify storage builds return 200 for an empty `kube_job_annotations` vector, for a zone no `harvest` backend declares, and for the volume-label unbounded-root fallback

## 4. Tests that pinned degradation on the storage path

- [x] 4.1 Update storage-plan, `qosscope`, `volumelabelscope` and `appscope` tests that asserted a degraded 200 on the storage path to assert the 502 instead; verify `make test`
- [x] 4.2 Add `internal/api` component tests: storage 502 naming the family for a Harvest, a kubelet and an annotation failure; storage 200 on `ALERTS` failure; `/v1/graph` 200 on `volume_labels` failure

## 5. Documentation

- [x] 5.1 Add the storage fail-closed entry to `docs/BREAKING.md`; update `docs/upstream-metrics.md` and `docs/netapp-harvest-preconditions.md` where they call storage legs optional; update `README.md`, `README.zh-tw.md`, `CLAUDE.md`
- [x] 5.2 Update the `/v1/storage-graph` OpenAPI failure description (502 names the family) and run `make docs && make check-docs`

## 6. Gate

- [x] 6.1 Run `make lint vet test` and `openspec validate fail-storage-graph-on-any-leg-error --strict`; both clean
