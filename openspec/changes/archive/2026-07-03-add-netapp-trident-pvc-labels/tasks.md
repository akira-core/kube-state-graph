# Tasks: add-netapp-trident-pvc-labels

## 1. PromQL query constants (`pkg/promql`)

- [x] 1.1 Add `QTridentVolumeInfo Query = "kube_tridentvolume_info"` and `QTridentBackendInfo Query = "kube_tridentbackend_info"` to `queries.go` with doc comments covering: NOT stock KSM (custom-resource-state over Trident CRDs / compatible exporter), fixed label contract (`name` = TridentVolume CR name = PV name + `backendUUID`; `backendUUID` + `svm`), OPTIONAL with graceful degradation, prefix-aware via Renderer (D-T2/D-T3)
- [x] 1.2 Add `Render` cases for both: `last_over_time(%s<metric>[%s])`, prefix applied (KSM-shaped)
- [x] 1.3 Extend `queries_test.go`: bare render strings for both, prefixed render (`o11y_kube_tridentvolume_info` / `o11y_kube_tridentbackend_info`), and confirm service-graph/up remain unprefixed

## 2. Topology reader (`pkg/build`)

- [x] 2.1 Add `TridentVolume`, `TridentBackend model.Vector` fields to `topologyVectors`; add the two `g.Go(fetch(...))` legs in `ReadTopology` (16 → 18) and both `RawSeriesCount` entries
- [x] 2.2 Refactor `resolvePVCStorageClass` → `resolvePVCInfo` returning `map[pvcKey]pvcInfoAttrs{storageClass, volumeName string}` in one pass over `v.PVCInfo`: per-field independent empty-skip and lexically-smallest-wins on duplicate `(cluster, namespace, claim)`; existing StorageClass semantics unchanged; update the `parseTopology` call site and the `pvc-to-storageclass` edge feed
- [x] 2.3 Add `resolveTridentVolumeBackends(v.TridentVolume, mc) map[[2]string]string` — `(cluster, name)` → `backendUUID`; skip empty `name`/`backendUUID`; lexical-min on duplicates; `mc.bucket` for missing cluster (D-T4/D-T5)
- [x] 2.4 Add `resolveTridentBackendSVMs(v.TridentBackend, mc) map[[2]string]string` — `(cluster, backendUUID)` → `svm`; skip empty `backendUUID`/`svm`; lexical-min on duplicates; `mc.bucket` for missing cluster
- [x] 2.5 PVC assembly in `parseTopology`: set `labels["volumename"]` when the resolved PV name is non-empty; chain `(cluster, volumename)` → `backendUUID` → `svm` and set `labels["svm"]` only when every link is non-empty (never empty-string values; `svm` impossible without `volumename`)
- [x] 2.6 Unit tests (`topology_test.go` / new `trident_test.go`): resolver-level — per-field independence in `resolvePVCInfo` (volumename without storageclass and vice versa), empty-value skips, duplicate-series lexical-min at each of the three stages, missing-cluster bucketing; assembly-level — full chain lands both labels, each partial-chain permutation omits exactly the right key(s), `volume` and `volumename` coexist on one PVC, Trident metrics absent → valid topology with `volumename` only
- [x] 2.7 Extend the build determinism test with Trident fixture series (shuffled vector order → byte-identical labels)

## 3. Golden + integration coverage

- [x] 3.1 Golden (`internal/api`): add Trident series to one mock-querier fixture set (fixtureSet gains the two new queries — verify `newMockQuerier` fixture plumbing covers unknown queries with empty vectors so untouched tests stay green), run `go test ./internal/api/ -update -run Golden`, and confirm goldens WITHOUT Trident data are byte-identical to before (degradation proof)
- [x] 3.2 Integration (`internal/integration`): one scenario ingesting `kube_tridentvolume_info` + `kube_tridentbackend_info` fixture series via `POST /api/v1/import/prometheus` and asserting `data.labels.volumename` + `data.labels.svm` on the emitted PVC; rely on existing non-Trident suites as absence coverage (assert no `svm` key in one existing PVC assertion if cheap)

## 4. Docs + spec sync

- [x] 4.1 CLAUDE.md: add both metrics to the `KSG_METRIC_PREFIX` KSM-shaped list; add a load-bearing bullet for the PVC volumename/svm label chain including the `volume` vs `volumename` distinction and the fixed Trident label contract
- [x] 4.2 Confirm swagger/docs untouched (`labels` is an open string map in the DTO — expect no `make docs` diff); no mockery-registered interface changed — expect no `make mocks` diff

## 5. Verify

- [x] 5.1 `make test` (unit + component + golden + property; integration skips without Docker), `make vet`, `make lint`
- [x] 5.2 `openspec validate "add-netapp-trident-pvc-labels"` and re-read the two spec deltas against the implemented behaviour (every scenario has a matching test)
