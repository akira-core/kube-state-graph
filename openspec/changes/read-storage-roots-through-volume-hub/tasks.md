## 0. Sequencing

- [x] 0.1 After `fail-storage-graph-on-any-leg-error` is archived, rebase this change's deltas: MODIFIED "Storage build reads only what it draws" → "Storage build reads only what the body draws" (carry that change's fail-closed text), REMOVED "Storage-side roots narrow the Harvest topology read" → REMOVED "Storage-side roots restrict the Harvest topology read", every quoted citation of the old names, and the `cluster-topology-source` blocks "Topology series consumed" / "Application-rooted recovery reads of the owner and annotation families" re-copied from the archived text with this change's edits re-applied; verify `openspec validate read-storage-roots-through-volume-hub --strict`

## 1. Query layer

- [x] 1.1 Add `scopedLabel` entries for the claim-keyed families — `QPVCInfo` → `volumename`; `QPVCBindings`, `QPVCAnnotations`, `QKubeletVolumeUsedBytes`, `QKubeletVolumeCapacityBytes` → `persistentvolumeclaim` — and export them as `promql.ClaimScopedQueries`; verify a `pkg/promql/scope_test.go` case renders each with its fixed selector (if any), the request matchers and the scope, in that order
- [x] 1.2 Add an SVM-rooted phase-1 renderer (`{cluster=~OC?, svm=~S}`) beside `RenderVolumeLabelsRooted`; verify `pkg/promql/volumelabels_test.go` pins svm-only, cluster+svm, escaping, and `ok == false` for an empty SVM set
- [x] 1.3 Add an owner-completion renderer taking one ONTAP cluster and its aggregate set (`{cluster="c", aggr=~"a|b"}`); verify a unit test pins the string, escaping, and `ok == false` for an empty set
- [x] 1.4 Add the optional `promql.FamilyZoneQuerierSource` upgrade (`QuerierForFamilyZones(sel, zoned ...Family)`) and implement it on `*Router` by binding ONE snapshot with a per-family zone decision on the bound `fanoutQuerier`; verify `pkg/promql/router_test.go` cases show a `harvest` query goes only to the zone's backends while a `ksm` / `kubelet` / `alerts` query from the same bound querier reaches every backend serving it, and that a reload between two calls on one bound querier does not change the snapshot
- [x] 1.5 Verify `queryDims`, the fixed-selector table and `pkg/promql/testdata/render-baseline.txt` are untouched by running `go test ./pkg/promql/ -run 'TestRender_EmptySelectorMatchesBaseline|TestQueryDims_EveryQueryListed|TestQueryFamily_EveryQueryListed'`

## 2. PV candidate extraction

- [x] 2.1 Add the pure extractor (`pvCandidates(volumes) []string`): every suffix starting with `pvc_` at position 0 or after `_`, `_` rewritten to `-`, sorted and de-duplicated; verify a table test covers `trident_pvc_ab12_cd34`, `pvc_ab12_cd34`, `x_pvc_pool_pvc_ab12` (two candidates), `trident_pvc_ab12_clone`, `vol0`, `svm_shop_root`, an uppercase `PVC_` (no candidate), and empty input

## 3. Read plan

- [x] 3.1 Carry the SVM roots' values (not only a presence bit) on `topologyPlan` from `storagePlan(roots)`, sorted and de-duplicated with empties dropped; verify a `build_storage_plan_test.go` case asserts map order never reaches the plan
- [x] 3.2 Rewrite `restrictsVolumeLabels`: at least one `ontap_cluster=` / `aggr=` / `svm=` root, a scopeable match mode; `node=` no longer disqualifies; verify unit cases for no root, `node=`-only (false), `svm=`-only, `aggr=`+`svm=`, `aggr=`+`node=`, `svm=`+`node=` (true), `contains` / `regex` (false), and `fullPlan` (false)
- [x] 3.3 Add the hub decision (`hubMode`) = restricted AND phase 1 bounded and renderable, recorded on `topologyVectors` once phase 1 resolves its shape; make `issuesFirstWave` withhold `promql.ClaimScopedQueries` when the plan COULD be hub, and fall back to issuing them first-wave-equivalent (whole-zone, original selector) when phase 1 falls back; verify unit cases assert which legs launch in each case and that `fullPlan` still pins 37 / 43 legs in `TestReadTopology_FanOutLegCount`

## 4. Harvest phases

- [x] 4.1 Extend phase 1 to issue the SVM group beside the aggregate / cluster group, with the chunk cap applied to the SUM of queries across groups, results merged in (group, chunk) order and de-duplicated by fingerprint; verify `volumelabelscope_test.go` cases for `aggr=`+`svm=` (both groups, overlap voted once), the combined chunk cap fallback, and group ordering
- [x] 4.2 Add owner completion (a chunk error fails the build): T = `(cluster, aggr)` pairs from SVM-group rows minus aggregates the aggregate group read whole; one chunked query per ONTAP cluster; gated on phase 1 alone; merged by fingerprint before the parse; never a candidate source; verify cases for the takeover vote (completion changes the owner to the unrestricted answer), an SVM on an already-rooted aggregate (no completion for it), and no `svm=` root (no completion)
- [x] 4.3 Keep phase 2 gated on phase 1 + the claim-info family, now fed by the hub-scoped claim-info rows; verify the clone and cross-filer parity fixtures of `volumelabelscope_test.go` still pass under hub mode

## 5. Claim-keyed reads

- [x] 5.1 Add `pkg/build/claimscope.go`: `kube_persistentvolumeclaim_info` restricted on `volumename` to the candidates, gated on phase 1; empty candidates issue nothing; required error class; verify cases for a hit, no candidate (no query), and a chunk failure failing the build
- [x] 5.2 Issue the claim-binding, `kube_persistentvolumeclaim_annotations` and the two kubelet families restricted on `persistentvolumeclaim` to the loaded claim names, gated on the claim-info read, failing the build on any query error; verify cases for each family's scope, for no claim issuing none of them, and for a kubelet chunk error failing the build
- [x] 5.3 Filter the rows of those four families to the `(cluster, namespace, claim)` keys the claim-info read returned before the pod scope and the parse; verify a same-named `platform/data` binding loads no pod and builds no PVC node
- [x] 5.4 Add the bounded fallback (`maxHubClaimChunks`): an over-cap scope issues the family once with no restriction and filters rows in the reader; verify a case shows one unscoped query and a body byte-identical to the chunked build
- [x] 5.5 Re-wire `pvcInfoDone`, `bindingsDone` and `pvcAnnotationsDone` to close when the claim-keyed reads return (every return path), so the pod, application and QoS waves compute empty scopes instead of blocking; verify with `go test -race ./pkg/build/` and a case where the claim-info read fails

## 6. Relaxed selector and routing

- [x] 6.1 In `buildStorage`, derive `hubSel` (az / env cleared) and pass it to every kube-state-metrics, kubelet and `ALERTS` leg, the claim-keyed reads, the pod / node / controller waves and the application recovery when hub mode can engage; keep the original selector when it cannot; verify rendered-query assertions in `build_storage_plan_test.go` show no `<az-key>` / `<env-key>` matcher in hub mode and the usual matchers otherwise
- [x] 6.2 Bind the querier through `QuerierForFamilyZones(sel, FamilyHarvest)` when the source implements it, else `QuerierFor(sel)`, else the plain `Querier`; verify `routedquerier_test.go` cases for all three sources
- [x] 6.3 Confirm cluster identities stay per-zone for cross-zone claims; verify an identity test where `c1` exists in `zone-a` and `zone-b` yields two identities and `clusters[]` lists both

## 7. Coverage signal

- [x] 7.1 Emit `storage_root_claim_miss` with `reason="no_pv_candidate"` (phase-1 rows with a `volume`, zero candidates) and `reason="no_claim"` (candidates, zero claim-info rows), plus a Debug summary of volumes / candidates / claims / bindings on every hub build; verify log-capture cases for both reasons and for root volumes alone not firing

## 8. Output tests

- [x] 8.1 Extend the storage parity harness: a single-zone estate whose joined PVs are all `pvc-…` built in hub mode and with the pre-change read produces byte-identical bodies for `ontap_cluster=`, `aggr=`, `svm=`, `aggr=`+`svm=`, `aggr=`+`node=`, `aggr=`+`pod=`, `aggr=`+`application=`
- [x] 8.2 Add a two-zone fixture where the rooted filer serves a claim in the other zone; verify the hub body draws that claim's complete path and the pre-change read does not
- [x] 8.3 Add a static-PV fixture (`mongo-data-01` ↔ `mongo_data_01`); verify the hub body draws no path through it while `/v1/graph` still draws its `pvc-to-netapp-aggr` edge
- [x] 8.4 Update the fan-out pins in `build_storage_plan_test.go` for hub-mode leg counts (claim-keyed legs leave the first wave; owner completion appears only with `svm=`); verify the pins and the counts documented in `docs/upstream-metrics.md` agree
- [x] 8.5 Add a `/v1/storage-graph` integration case in `internal/integration` with two KSM zones behind the routing table and one zoned Harvest store, asserting the cross-zone path; verify it runs (not skipped) on CI

## 9. Documentation

- [x] 9.1 Add the hub-mode entry to `docs/BREAKING.md` (cross-zone bodies under storage-exclusive roots; static PVs not reached from storage roots)
- [x] 9.2 Update `docs/upstream-metrics.md` (storage fan-out waves, hub critical path), `docs/netapp-harvest-preconditions.md` (PV-name requirement for hub mode, `storage_root_claim_miss`, `claim_name`-only exporters), and `docs/upstream-backend-routing.md` (the family-zone upgrade)
- [x] 9.3 Update the `/v1/storage-graph` OpenAPI annotations for the az / env semantics in hub mode and run `make docs && make check-docs`
- [x] 9.4 Update `CLAUDE.md`, `README.md` and `README.zh-tw.md` where they state `svm=` / `node=` disable the restriction, that the claim families are unrestricted, or that storage bodies describe one zone; verify no remaining sentence contradicts the new behaviour

## 10. Gate

- [x] 10.1 Run `make lint vet test` and `openspec validate read-storage-roots-through-volume-hub --strict`; both clean
