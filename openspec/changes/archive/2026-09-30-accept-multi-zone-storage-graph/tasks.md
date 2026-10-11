# Tasks

## 1. Repeatable az / env

- [x] 1.1 Replace `exactlyOne` with `atLeastOne` for `az` / `env` in `ParseStorageValues` and set `Selector.AZ` / `.Env` to the full value sets; verify `pkg/kubegraph/parse_storage_test.go`: repeated `env` accepted, `?az=b&az=a` and `?az=a&az=b` yield equal selectors, empty-only values still `missing_az` / `missing_env`, window → az → env → root check order unchanged
- [x] 1.2 Update the `/v1/storage-graph` swag annotations (`az` / `env` repeatable) and run `make docs`; verify `make check-docs` is clean
- [x] 1.3 Add a `pkg/build` test that a two-zone storage build renders `<az-key>=~"zone-a|zone-b"` on every kube-state-metrics, kubelet, Harvest and `ALERTS` query and binds a querier over both zones; verify it passes

## 2. Identity-keyed row matching

- [x] 2.1 Extend `podSeriesKey` to `{az, env, cluster, namespace, pod}` (configured `LabelKeys`, `bucketCluster`) and thread the keys through `podsNewestOn`, `keepPodBindings`, `podKeysOf` and `readApplicationBindings`; verify existing node-seed / application tests pass unchanged and a new test shows two zones' `c1/shop/db-0` elected independently (the zone-a pod stays on `worker-1` while the zone-b one moved to `worker-2`)

## 3. Zone-agreeing join

- [x] 3.1 Introduce a shared `zone` type (replacing `alertZone`), add `zone` to `pvcVolume` (from the claim-info row) and to `volumeLabelCandidate`, pass `LabelKeys` into `resolveNetAppStorage`, and skip a candidate when both sides are zoned and differ — `volIndex` / `allByAggr` unfiltered; verify `pkg/build/netapp_test.go` scenarios: cross-zone collision picks the own-zone aggregate / SVM / QoS / ceiling, an unzoned series still competes, owner vote unaffected
- [x] 3.2 Verify every existing golden is unchanged (`go test ./internal/api/ -run Golden` without `-update`) and alert tests still pass after the `zone` refactor

## 4. Qualified aggr / svm roots

- [x] 4.1 Add `graph.ONTAPRef`, `StorageRoots.Qualified`, and parsing of `<oc>/<name>` for `aggr` / `svm` (bare subsumes qualified; malformed → error); verify `pkg/graph` and `pkg/kubegraph` tests cover valid, mixed, subsumed and malformed values (`ontap-prod/`, `/aggr1`, `a/b/c`)
- [x] 4.2 Add `volumeAggrPairs` / `volumeSVMPairs` to `topologyPlan`, emit per-ONTAP-cluster phase-1 groups in `rootedVolumeLabelsChunks` (cap counts all groups), and update `harvestSeed`, `flowlessAggrGauges`, `flowlessControllers`, `prepareFlowless`, `ownerCompletionTargets` and `uncoveredAggrPairs`; verify `pkg/build/volumelabelscope_test.go` pins the rendered queries of the spec's qualified-seed scenario and an over-large qualified set is rejected before any query
- [x] 4.3 Route qualified aggregates' flowless gauges through `issueHarvestPairMap`; verify a test that `aggr=ontap-prod/aggr9` reads `{cluster="ontap-prod",aggr="aggr9"}` gauges only and materialises no `ontap-lab` aggregate
- [x] 4.4 Match qualified values in `resolveStorageRoots`; verify `pkg/graph` projection tests: qualified aggregate roots one filer, bare + qualified SVM mix, and the property test's invariants still hold
- [x] 4.5 Add the qualified-root forms to the `/v1/storage-graph` swag docs and a golden `storage-graph-qualified-aggr-root-cytoscape.json`; verify `make check-docs` and the golden test pass

## 5. Union invariant

- [x] 5.1 Add the three-build union test of design D5 (zone-a, zone-b, both over one mocked two-zone estate with colliding names and a cross-node pod recreation); verify node-id and edge-id sets and edge weights of the multi-zone body equal the union
- [x] 5.2 Add a multi-zone golden `storage-graph-multi-zone-cytoscape.json`; verify the golden test passes

## 6. Documentation

- [x] 6.1 Update CLAUDE.md (storage-graph request surface: repeatable `az` / `env`, qualified `aggr` / `svm`, the union invariant, the zone-agreeing join in the NetApp bullet, D7 wording "the request's zone" → "the request's zones"), `docs/netapp-harvest-preconditions.md` (ONTAP cluster / controller names unique across the estate; zone-agreeing join), and `docs/BREAKING.md` (the `/v1/graph` body change under a cross-zone token collision); verify by re-reading against the specs

## 7. Integration

- [x] 7.1 Add an `internal/integration` test with two zone-scoped backends (`ksm` + `harvest` per zone): a multi-zone request draws both zones and equals the union of the per-zone requests, and a qualified `aggr=` root draws one filer; verify `go test ./internal/integration/ -run StorageGraph` passes with Docker available
- [x] 7.2 Run `make lint`, `make test`, `make verify-mocks`, `make check-docs` and `openspec validate accept-multi-zone-storage-graph --strict`; verify all are clean
