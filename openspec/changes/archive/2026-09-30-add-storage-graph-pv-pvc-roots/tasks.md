# Tasks

## 1. Scope and request parsing

- [x] 1.1 Add `graph.StorageRootPVC` / `StorageRootPV`, `ClaimRef{Namespace, Name}` with `String()`, `StorageRoots.Claims`, and a shared `namespacedRefs` helper used by both `podRefs` and the new claim parser; `Any()` counts `Claims`; verify `pkg/graph` unit tests cover sort/de-dup, empty-value drop, and rejection of `orders-data` / `shop/orders/data` / `/x` / `x/`
- [x] 1.2 Add `pvc` and `pv` to `storageRootParams` (order `…, pod, pvc, pv, application`) and to the `ParseStorageValues` validation list; verify `pkg/kubegraph/parse_storage_test.go` covers a valid `pvc=` / `pv=`, the malformed `pvc` 400, the `pvc`+`pv` mixed-kind message naming both, and that `missing_root` still fires with only empty values
- [x] 1.3 Document both parameters in the `/v1/storage-graph` swag annotations and run `make docs`; verify `make check-docs` is clean

## 2. Upstream rendering and the seed plan

- [x] 2.1 Generalise `promql.RenderPodsInNamespace` to a caller-named identity label (`RenderNamesInNamespace`), admit `QPVCInfo` to its allow-list, and switch the pod callers to pass `PodLabel`; verify existing pod-scope tests and `TestRender_EmptySelectorMatchesBaseline` pass unchanged and a new test pins `{namespace="shop",persistentvolumeclaim=~"cache|orders-data"}`
- [x] 2.2 Add `claimSeeded()`, the claim-root fields and `prepareClaimSeed` (per-namespace chunk count for `pvc`, one `ChunkScope` for `pv`, cap → `ReasonInvalidScope`) to `topologyPlan`, make `tracksByReference()` true for it, and call it from `buildStorage` beside the other `prepare*` steps; verify a unit test rejects an over-large `pvc` and `pv` set before any query (mock querier asserts zero calls)

## 3. Claim seed and wiring

- [x] 3.1 Implement `readClaimSeed` in `pkg/build/claimseed.go` (per-namespace `pvc` read, `volumename` `pv` read, both through `issueClaimKeyed`, rows filtered to root refs / root names, `recoverScopedPanic`); verify unit tests: a cross-namespace same-named claim is dropped, a `pv` value naming no claim loads nothing, the tally records the family when issued
- [x] 3.2 Wire the seed in `readTopology`: `readClaimSeed → pvcInfoDone`, `readHubClaimFamilies(pvcInfoDone) → bindingsDone + pvcAnnotationsDone`, `readWorkloadVolumeLabels(pvcInfoDone) → volumeLabelsFinal`; exclude claim-seeded plans from the workload-claims branch and drop the four beside-seed signals; verify `go test ./pkg/build/ -race -count=20 -run ClaimSeed` passes and a failing-seed test returns an error instead of blocking
- [x] 3.3 Extend `TestBuildStorage_FanOutLegCount` / `_Hub` with `pvc` and `pv` rows (no claim, one unmounted claim, one mounted claim with a controller) and record the counts in `docs/upstream-metrics.md`; verify the pins pass and the doc numbers match
- [x] 3.4 Add a parity test: for a mounted claim, the `?pvc=` body's paths equal the `?pod=` body's paths for that claim, and `?pv=` equals `?pvc=`; verify it passes

## 4. Sink claims and projection

- [x] 4.1 Pass `sinkUnmounted` (true iff claim-seeded) into `assembleStorageFlow` / `storageFlowEdges`, emitting `node-aggr`, `aggr-svm` and `svm-pvc` for an unmounted chain only then; also materialise the PVC node of a root claim no pod mounts (design D7); verify a `pkg/build` test shows the chain under a `pvc` plan and no unmounted chain under an `aggr` plan (existing storage goldens unchanged)
- [x] 4.2 In `pkg/graph/project_storage.go`, resolve `pvc` / `pv` roots into `workload` and a `claimRoots` set, build sink units (`podID=""`, `n=1`) for unmounted `svm-pvc` edges, and retain a sink unit only when its claim is a claim root; verify `pkg/graph` tests: sink retained under `pvc`, sink dropped under `aggr` on a hand-built graph, mixed-request weights (`node-aggr` 140 / `svm-pvc` 40 + 100 / `pvc-pod` 100), and the property test's pruned ⊆ inventory invariant still holds
- [x] 4.3 Add goldens `storage-graph-pvc-root-cytoscape.json` (mounted + unmounted sink claim) and `storage-graph-pv-root-cytoscape.json`, and a non-NetApp claim root case (PVC alone, no pods); verify `go test ./internal/api/ -run Golden` passes and no existing golden changed

## 5. Documentation

- [x] 5.1 Update CLAUDE.md's `/v1/storage-graph` request-surface paragraph and request-lifecycle diagram with the two root kinds, the claim seed, and the sink-claim rule; update `docs/BREAKING.md` only if a message text changed (it should not); verify by re-reading against the specs

## 6. Integration

- [x] 6.1 Add `internal/integration` coverage: `?pvc=` and `?pv=` against the VictoriaMetrics fixture (mounted, unmounted sink, static PV bound to a matching FlexVol); verify `go test ./internal/integration/ -run StorageGraph` passes with Docker available
- [x] 6.2 Run `make lint`, `make test`, `make verify-mocks`, `make check-docs`, `make check-route-containment` and `openspec validate add-storage-graph-pv-pvc-roots --strict`; verify all are clean
