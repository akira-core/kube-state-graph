## 1. Query layer

- [x] 1.1 Add `promql.RenderVolumeLabelsRooted(window, clusters, aggrs)` rendering `last_over_time(volume_labels{cluster=…,aggr=…}[w])` with the two matchers AND-combined in one selector (cluster first), reusing `normaliseValues` / `appendMatcher`; verify a new `pkg/promql` unit test pins the rendered string for cluster-only, aggr-only and both, pins `regexp.QuoteMeta` escaping, and pins `ok == false` when both value sets are empty
- [x] 1.2 Add `promql.RenderVolumeLabelsTokenScoped(window, tokens, anySuffix)` rendering the phase-2 `volume` alternation, prefixing each escaped token with `.*` when `anySuffix`; verify a unit test pins both forms, pins that escaping is applied to the token but not to the `.*`, and pins `ok == false` for an empty set
- [x] 1.3 Confirm `ChunkScope` is reused unchanged for both renderers by measuring the rendered branch cost including the `.*` prefix; verify a unit test splits a token set at the budget and shows a single over-budget token still gets its own chunk
- [x] 1.4 Verify `queryDims` and the fixed-selector table are untouched by running `go test ./pkg/promql/ -run 'TestRender_EmptySelectorMatchesBaseline|TestQueryDims_EveryQueryListed'` and confirming `pkg/promql/testdata/render-baseline.txt` does not move

## 2. Volume-key gate

- [x] 2.1 Expose the match mode's scopeability on `VolumeKeyRewriter` as a table-driven predicate (`exact` and `suffix` scopeable, `contains` and `regex` not) plus the `anySuffix` bit phase 2 needs; verify a `pkg/build/volumekey_test.go` case asserts every declared mode has an entry so a new mode must be classified rather than defaulted
- [x] 2.2 Expose the per-claim derived token for phase 2 without duplicating the derivation, reusing the same `token` the matcher builds from; verify a unit test shows the token a claim contributes to the phase-2 scope is byte-identical to the token `newVolumeMatcher` indexes it under

## 3. Read plan

- [x] 3.1 Carry the sorted `ontap_cluster` and `aggr` root values, and whether the request carries any `svm=` or `node=` root, on `topologyPlan` from `storagePlan(roots)`, alongside the existing pod and node roots; verify a `pkg/build/build_storage_plan_test.go` case asserts map iteration order never reaches the plan (sorted, de-duplicated, empties dropped)
- [x] 3.2 Add the plan predicate answering whether this build restricts the Harvest topology read — at least one `ontap_cluster=` or `aggr=` root, AND no `svm=` root, AND no `node=` root, AND a scopeable match mode — and wire the mode in from `Options.volumeKey()`; verify unit cases cover: no storage root, `svm=`-only, `node=`-only, `aggr=`+`svm=`, `aggr=`+`node=`, `contains` mode, `regex` mode (all false) and `aggr=`, `ontap_cluster=`, both, `aggr=`+`pod=` (all true)
- [x] 3.3 Make `fullPlan` answer false unconditionally so `/v1/graph` is structurally incapable of restricting this leg; verify `TestReadTopology_FanOutLegCount` still pins 37 / 43 legs unchanged

## 4. Phase 1

- [x] 4.1 Make the `QVolumeLabels` first-wave leg plan-dependent: unrestricted single query when the predicate is false, otherwise the rooted query (chunked by its larger alternation) issued under the bare family name with the family's existing `fetchOptional` error class; verify a unit test asserts an unrooted build issues exactly one bare query and a restricted build issues none
- [x] 4.2 Chunk the rooted query by its larger alternation (`aggr` when present, else `cluster`) under the shared budget, repeating the other verbatim, and merge chunks in chunk order; verify a unit test splits an aggregate set at the budget, shows the cluster matcher is present in every chunk, and shows a single over-budget value still gets its own chunk
- [x] 4.3 Report the merged series count under the one `volume_labels` tally key; verify a unit test asserts `RawSeriesCount["volume_labels"]` equals the merged length for a build issuing several queries

## 5. Phase 2

- [x] 5.1 Add `pkg/build/volumelabelscope.go` computing the phase-2 token scope: the derived tokens of exactly the claims phase 1 matched, sorted and de-duplicated; verify a unit test shows a claim matching no phase-1 series contributes no token and a claim matching several contributes one
- [x] 5.2 Issue phase 2 as a second wave gated on phase 1 and `QPVCInfo` through the existing `signalWhenDone` channels, chunked under the shared budget, merged in chunk order into the same vector and de-duplicated with phase 1 by label-set fingerprint (a series both phases return must vote once in `pickOwner`); verify a unit test asserts phase 2 is not issued when the phase-2 scope is empty and that only the phase-1 query is issued (no phase 2) when phase 1 matched nothing
- [x] 5.3 Re-gate the QoS workload wave on the merged result rather than on phase 1, keeping every existing rule of the scoped QoS read; verify `pkg/build/qosscope_test.go` still passes and a new case shows the QoS `volume` scope is computed over the merge

## 6. Coverage signal

- [x] 6.1 Suppress `netapp_volume_join_miss` for a restricted build and leave `netapp_qos_join_miss` untouched; verify a unit test asserts the warning fires for an unrooted build with unmatched claims and does not fire for the same estate under `aggr=`

## 7. Output-preservation tests

- [x] 7.1 Extend the storage-plan parity harness so one estate is built twice — restricted and unrestricted — and the serialised bodies compared; verify the parity assertion passes for `aggr=`, for `ontap_cluster=`, and for both together
- [x] 7.2 Add a Trident-clone fixture: one claim whose token matches `trident_pvc_x` on a lexically-larger aggregate and `snap_trident_pvc_x` on a lexically-smaller one, rooted at the larger; verify the parity assertion fails without phase 2 and passes with it
- [x] 7.3 Add a cross-filer FlexVol-name collision fixture rooted at an aggregate on the lexically-larger ONTAP cluster; verify the parity assertion passes and the claim resolves to the lexically-smallest `(ontap_cluster, aggr)` in both builds
- [x] 7.4 Add an HA-takeover fixture where a rooted aggregate's series disagree on `node`; verify the owning controller and the `node-aggr` tier are identical in both builds
- [x] 7.5 Add a fixture where no volume on the rooted aggregate matches any claim while the aggregate families name it; verify the body contains the aggregate, its health and usage and its owning controller, phase 2 is not issued, and the body matches the unrestricted build
- [x] 7.6 Add opt-out fixtures for `svm=`-only, `node=`-only, `aggr=`+`svm=` (with an SVM claim on a different aggregate), `aggr=`+`node=`, `contains` and `regex`; verify each issues one bare unrestricted query and produces a body identical to today's
- [x] 7.7 Add a chunk-failure case for each phase using the fake querier's failure hook; verify the request still succeeds, the loss is confined to the claims that chunk carried, and the build does not fail

## 8. Integration

- [x] 8.1 Add a `/v1/storage-graph` integration case to the storage suite that compares a rooted request's body, on the default `suffix` mode (restricted read), against the same request under the `contains` mode (which reads `volume_labels` whole), over a fixture with a clone on a lexically-smaller aggregate; the suite has no facility for inspecting issued queries, so the query shape is pinned at unit level instead. **Ran on CI** (`test` job, Docker available): `internal/integration` passed in 27.7s with no skip, against 1.1s and 8 skips where Docker is absent
- [x] 8.2 Confirm `promqlfake` handles the phase-2 `.*<token>` alternation correctly under its anchored-regex semantics, extending its scope-value helper if the escape inversion needs it; verify `go test ./pkg/internal/promqlfake/` passes with a case covering the suffix form

## 9. Documentation

- [x] 9.1 Update `docs/upstream-metrics.md`'s storage fan-out section with the two Harvest phases, the roots that engage them and the match-mode gate; verify the rendered leg counts in that section match the fan-out pins in `pkg/build/build_storage_plan_test.go`
- [x] 9.2 Update `docs/netapp-harvest-preconditions.md` with the suppressed join-coverage signal and what still exercises it; verify the operator alerting guidance names the unrooted path
- [x] 9.3 Update `CLAUDE.md`, `README.md` and `README.zh-tw.md` where they state that Harvest legs carry no matcher and that roots never narrow a read; verify no remaining sentence in those files contradicts the new behaviour

## 11. Review hardening

Raised by `/code-review` after the first implementation pass and folded back
into the specs and design.

- [x] 11.1 Cap the phase-1 restriction at `maxRootedVolumeLabelChunks` queries and fall back to the unrestricted read past it, since `ontap_cluster=` / `aggr=` are repeatable with no cap on their COUNT; verify unit cases cover 5000 near-maximum-length values, a cluster set that collapses the per-chunk budget to its floor, and a filer-sized set that stays well under
- [x] 11.2 Charge phase 1's repeated matcher at its RENDERED length via a new `promql.MatcherCost`, not as a raw join; verify a unit test shows a metacharacter-carrying filer name costs 30 bytes rendered against 15 joined, and that the chunked alternation still fits what the repeated matcher leaves
- [x] 11.3 Normalise the plan's root values so `restrictsVolumeLabels` and the renderer agree, and degrade rather than fail when a restriction cannot be rendered; verify an embedder passing `StorageRoots{ONTAPClusters: {"": {}}}` reads unrestricted and returns 200 instead of failing an OPTIONAL leg
- [x] 11.4 Give phase 2 a `cluster!~` exclusion when the restriction names ONLY ONTAP clusters, and never when it names an aggregate; verify both directions, including that an aggregate root still recovers a same-cluster candidate
- [x] 11.5 Count `netapp_volume_join_miss` per claim under a restriction — FlexGroup misses still report, claims that matched nothing do not — instead of suppressing the signal by request shape; verify the restricted and unrestricted counts differ by exactly the unmatched claim
- [x] 11.6 Restore the panic guard both phases lost by replacing `fetchOptional`, and give `issueVolumeLabelsQueries` the sibling cancellation `issueScopedFamilies` has (`errgroup.WithContext`); verify the package still passes under `-race`
- [x] 11.7 Thread the leg's own `l.dst` into the rooted read instead of hardcoding `v.VolumeLabels`, and read the rewriter from the `topologyVectors` field; verify `make test` (which runs `-race`) is clean — the field must be read directly, never through `volumeKey()`, whose value receiver copied the whole struct while sibling legs were writing it
- [x] 11.8 Make `topologyVectors.volumeKey()` a pointer receiver so that struct copy cannot be reintroduced from inside the fan-out; verify `go build ./...` and the race detector
- [x] 11.9 Extract the shared `pvcInfo → claims + matcher` step (`claimVolumeMatcher`) so the QoS scope and the phase-2 scope cannot drift on what a claim is; verify both readers still pass their own tests
- [x] 11.10 Pin that an aggregate reached only through phase 2 — whose owner vote is partial — is never drawn, the invariant that keeps `pickOwner` out of the body; verify the built graphs differ on that aggregate's `labels.node` while the projected bodies are identical
- [x] 11.11 Assert the integration test's control precondition (that `contains` and `suffix` select the same volumes on the shared VictoriaMetrics) on an unrooted request, so a later fixture cannot silently turn the control into a tautology

## 10. Verification

- [x] 10.1 Run `make test`, `make lint` and `make vet` and confirm all pass with no new findings
- [x] 10.2 Verify the implementation against the delta specs with the `openspec-verify-change` skill (`openspec verify` is not a CLI command) and confirm every requirement and scenario maps to code and a test
- [x] 10.3 Compare a rooted and an unrestricted `/v1/storage-graph` read of the same estate and window against a real VictoriaMetrics, and confirm the bodies agree on every node and edge the rooted request retains. **Substituted for the demo-repository run**: the demo estate this task was written against is not reachable from this working copy — the checkout at `~/Documents/instrumentation-demo` is the OTel instrumentation demo, which declares no `up`, `verify` or `redeploy-backend` target, carries no `kube-state-graph` submodule, and needs a `kind` binary that is not installed. `TestGraphSuite/TestStorageGraph_RootedVolumeLabelsMatchTheWholeFilerRead` makes the same comparison the demo run would: two servers over one ingested estate, one on the default `suffix` mode (restricted read) and one on `contains` (reads `volume_labels` whole), their bodies asserted equal for `aggr=`, `ontap_cluster=` and both together, with an unrooted control pinning that the two modes select the same volumes. Run locally with Docker: `go test ./internal/integration/ -run 'TestGraphSuite/TestStorageGraph' -count=1` passed in 31.42s with no skip. The demo run is weaker evidence only in estate realism, not in the property this task checks, and stays available to whoever has that environment
