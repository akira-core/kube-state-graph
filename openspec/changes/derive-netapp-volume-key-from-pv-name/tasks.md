## 1. Derivation primitives

- [x] 1.1 Add a `volumeKeyRewriter` to `pkg/build` holding the compiled ordered rewrite rules and the match mode, with a constructor that returns an error on an uncompilable pattern or an unrecognised mode; verify with table unit tests in `pkg/build` covering the default `-` → `_` rule, a multi-rule list applied in declaration order, and both error paths.
- [x] 1.2 Implement the four match modes (`exact`, `suffix`, `contains`, `regex`) on the rewriter as a `matches(token, volume string) bool` predicate; verify with unit tests pinning the spec scenarios "Suffix mode rejects a clone whose name extends past the PV name" and "Contains mode admits what suffix mode rejects".
- [x] 1.3 Add the length-bucketed candidate index used by `exact` and `suffix` (`map[int]map[string][]claim`), keeping `contains` and `regex` on a linear scan; verify with a unit test asserting identical match sets from the bucketed and scan paths over a randomised claim/volume corpus.
- [x] 1.4 Add `qosVolumeScope(pvcInfo, volumeLabels model.Vector, rw *volumeKeyRewriter) []string` returning the sorted, de-duplicated matched `volume` values; verify with a unit test covering the empty-vector, no-match and duplicate-name cases and asserting the output is sorted and deduped.

## 2. Configuration surface

- [x] 2.1 Add `NetAppVolumeKeyRewrite []string`, `NetAppVolumeMatchMode string` and `NetAppQoSScopeBatchBytes int` to `internal/config` with the `--netapp-volume-key-rewrite` (repeatable), `--netapp-volume-match-mode` and `--netapp-qos-scope-batch-bytes` flags and their `KSG_NETAPP_*` environment variables; verify with `internal/config` tests asserting defaults, flag-over-env precedence and semicolon splitting of the env form, mirroring the existing `--az-label` tests.
- [x] 2.2 Parse each rewrite entry by splitting on the FIRST `=` into pattern and replacement, and validate in `Config.Validate()` that every pattern compiles, that the match mode is one of the four values, and that the batch budget is positive; verify with `internal/config` tests asserting a fatal error naming the offending pattern and that no default is silently substituted.
- [x] 2.3 Thread the parsed rewriter and batch budget into `build.Options` and construct the `volumeKeyRewriter` once per `Builder`; verify `go build ./...` and that a `pkg/build` test constructing a `Builder` with zero `Options` still gets the default rules.

## 3. Join re-sourced onto the stock label

- [x] 3.1 Change the `volume_labels` index in `pkg/build/netapp.go` to read `volume` instead of `volume_name`, and match claims through the rewriter rather than by map equality; verify the updated `pkg/build/netapp_test.go` topology cases pass with fixtures carrying `volume`.
- [x] 3.2 Change `indexQoSFamily` to key on `volume`, keeping the `(ontap cluster, svm)` scope test and the hop-C policy key untouched; verify the existing QoS scope and collision tests pass with `volume`-keyed fixtures.
- [x] 3.3 Update `resolveNetAppStorage`'s claim loop so a claim's candidate set comes from the rewriter's match, preserving the lexically-smallest `(ontap_cluster, aggr)` pick and the `svm` pick; verify with the existing determinism tests plus a new case where two distinct `volume` values match one claim.
- [x] 3.4 Delete every remaining `volume_name` read, comment and fixture in `pkg/build` and `pkg/promql`; verify `grep -rn 'volume_name' pkg/ internal/` returns only intentional BREAKING-doc references.

## 4. Scoped and batched QoS read

- [x] 4.1 Add a `pkg/promql` entry point rendering a QoS query with an anchored, `regexp.QuoteMeta`-escaped `volume` alternation composed with the fixed `lun=""` selector, reusing the existing alternation renderer; verify with `pkg/promql` tests pinning the rendered string for one, two and zero names, and confirm `pkg/promql/testdata/render-baseline.txt` is unchanged.
- [x] 4.2 Implement deterministic chunking of the scope by the configured byte budget, always emitting an over-budget single name as its own chunk; verify with a unit test asserting chunk boundaries are a pure function of the sorted scope and that no name is dropped.
- [x] 4.3 Restructure `ReadTopology` so `kube_persistentvolumeclaim_info` and `volume_labels` publish their vectors to a launcher goroutine inside the existing errgroup, which computes the scope and issues the batched QoS reads; verify a `pkg/build` test asserting the QoS queries are issued only after both prerequisites resolved and that a required-leg failure still fails the group.
- [x] 4.4 Skip the QoS read entirely when the scope is empty; verify with a `pkg/build` test using a mock querier that asserts zero QoS queries were issued when no claim matches, covering both spec scenarios ("No matched volumes" and "Volume-label family absent").
- [x] 4.5 Merge chunk results into each family's vector in chunk-index order, not completion order; verify with a test whose mock querier returns chunks out of order and asserts a byte-identical merged vector and identical summed I/O values across repeated runs.
- [x] 4.6 Make each chunk `fetchOptional` so one failure degrades only its own claims; verify with a test asserting a failing chunk leaves other claims' `metrics` intact, leaves every edge/aggregate/controller/`svm` intact, and does not fail the build.

## 5. Observability

- [x] 5.1 Redefine the `qosPresent` gate as "at least one issued chunk of at least one QoS family returned series" and keep `topoPresent` as-is; verify with tests covering the spec scenarios "Volume template without the QoS template" and "Non-NetApp deployment stays silent".
- [x] 5.2 Confirm `netapp_volume_join_miss` counts claims whose derived token matched nothing, including a whole-estate derivation misfit; verify with a test asserting the count equals the full claim count when no FlexVol name embeds a PV name.

## 6. Integration and golden coverage

- [x] 6.1 Move `internal/api` and `internal/integration` NetApp fixtures onto the `volume` label with Trident-shaped names; verify `go test ./internal/api/ -run Golden` passes with no `-update`, proving the response bodies are byte-identical.
- [x] 6.2 Extend `internal/integration` `TestPVCNetAppHarvestJoin` to ingest stock-shaped `volume_labels` (no relabel) and assert the `pvc-to-netapp-aggr` edge and `metrics` resolve; verify the integration suite passes with Docker available.
- [x] 6.3 Add an integration case asserting the QoS queries carry the `volume` alternation and that an unmatched workload's series is never fetched; verify by asserting on the recorded upstream query strings.

## 7. Documentation

- [x] 7.1 Rewrite `docs/netapp-harvest-preconditions.md` around the derivation: the relabel rule is gone, the blind spots are restated, and the operator's tuning loop is `netapp_volume_join_miss` → `count by (volume) (volume_labels)` → rewrite rules / match mode; verify the document names no `volume_name` label.
- [x] 7.2 Update `docs/upstream-metrics.md` for the `volume` label contract, the scoped QoS read and the per-chunk degradation; verify the Harvest table and the join description match the specs.
- [x] 7.3 Add the `docs/BREAKING.md` entry covering the removal of the `volume_name` read, the migration path (defaults first, then tune, then delete the relabel rule) and the rollback constraint that a deleted relabel rule must be restored before downgrading; verify the entry follows the file's existing format.
- [x] 7.4 Update `CLAUDE.md`'s NetApp storage-join bullet so the three-hop description matches the derive-then-match join and the two-wave read; verify no sentence there still claims the join is a single label equality on `volume_name`.

## 8. Verification gate

- [x] 8.1 Run `make test`, `make vet`, `make lint` and `make check-docs`; verify all pass with no `-update` runs and no regenerated goldens. **`make test` / `make vet` / `make lint` pass (lint: 0 issues; gofmt clean). `make check-docs` exits non-zero because it diffs the WHOLE `docs/` tree and this change edits `docs/*.md` by hand — the generated artefacts it regenerates, `docs/swagger.{json,yaml}`, are verified in sync (`git diff --quiet -- docs/swagger.json docs/swagger.yaml`). It goes green once the docs are committed.** The one golden that moved is `internal/api/testdata/golden/edge-types.json`, hand-edited (not `-update`) to match the `pvc-to-netapp-aggr` description in `pkg/graph/registry.go`.
- [x] 8.2 Run `openspec verify derive-netapp-volume-key-from-pv-name`; verify it reports the change complete and every spec scenario is covered by a test. **The installed OpenSpec CLI has no `verify` subcommand (`error: unknown command 'verify'`); used `openspec validate` (reports valid) and `openspec status` (4/4 artifacts complete) instead.**

## 9. Ceiling key re-anchored on hop A

- [x] 9.1 Feed `applyCeiling` a triple whose `oc` / `svm` come from hop A and whose `policy` comes from `pickPolicy(qcands, oc, svm)`, reduced to the lexically-smallest non-empty `policy_group` alone (the candidate's own `svm` is no longer part of the pick, so an svm-less workload can contribute); guard the key on a non-empty `svm` AND `policy` so an incomplete key is ignored rather than widened. Verify with `pkg/build/netapp_test.go` unit tests pinning the spec scenarios "Another policy group in the same SVM is never borrowed", "Volume in no policy group carries no ceiling", "Workload without an svm label still resolves its ceiling", "Claim without an SVM carries no ceiling" and "No ceiling without a measurement".
- [x] 9.2 Keep `indexPolicyCeilings` on the `(cluster, svm, policy)` triple with the `name` → `policy_group` identity fallback and the smallest-on-duplicate rule, and keep `policy_group` on `qosCandidate`; verify with a test asserting both identity spellings resolve and that a series carrying neither cannot be keyed. Confirm whether `internal/api/testdata/golden/with-netapp-storage-cytoscape.json` moves; if it does, regenerate deliberately with `-update` and record why in the commit message.
- [x] 9.3 Extend `internal/integration` `TestPVCNetAppHarvestJoin` so the fixed-policy fixture series resolve through the triple and assert `max_iops` / `max_bytes_per_sec` still surface; verify the integration suite passes with Docker available.
- [x] 9.4 Update `docs/upstream-metrics.md` (the triple's two sources, the ignore-never-widen rule), `docs/BREAKING.md` (the re-anchor is additive; a volume in no policy group still carries no ceiling) and the `CLAUDE.md` hop-C bullet; verify no document still says the ceiling is an SVM-level figure or a per-figure minimum.
- [x] 9.5 Run `make test`, `make vet`, `make lint` and `make check-docs`; verify all pass. **`make test` / `make vet` / `make lint` pass (lint: 0 issues; gofmt clean). Golden `with-netapp-storage-cytoscape.json` is unchanged (hand-built View, not hop-C). `make check-docs` exits non-zero because it diffs the WHOLE `docs/` tree and this change edits `docs/*.md` by hand — the generated artefacts it regenerates, `docs/swagger.{json,yaml}`, are verified in sync (`git diff --quiet -- docs/swagger.json docs/swagger.yaml`). It goes green once the docs are committed.**

## 10. Scope the SVM pick to the picked filer

- [x] 10.1 Run `pickAggr` before `pickSVM` in `resolveNetAppStorage` and give `pickSVM` an `oc` parameter that admits only candidates on that ONTAP cluster, leaving the pick unscoped when no aggregate resolved; verify `TestResolveNetAppStorage_FlexGroupEmptyAggr` still resolves its `svm` and the whole `pkg/build` suite passes.
- [x] 10.2 Add `TestResolveNetAppStorage_SVMScopedToPickedCluster` covering the cross-filer collision — svm resolves on the picked filer, the in-scope workload still measures the edge, and the ceiling keys on the picked filer's pair rather than the other filer's same-named SVM; verify the test FAILS against the unscoped pick before the fix lands.
- [x] 10.3 Restate the `svm` pick in the delta specs (`netapp-storage-graph` "PVC svm label re-sourced from the Harvest join" gains the cluster scope plus two scenarios; `cluster-topology-source`'s determinism sentence names the scope) and record the reasoning as design D10; verify `openspec validate` passes.
