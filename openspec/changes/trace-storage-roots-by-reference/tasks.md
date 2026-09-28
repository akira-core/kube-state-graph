# Tasks

## 1. Suffix-only join (D14)

- [x] 1.1 In `pkg/build/volumekey.go` remove `VolumeMatchMode`, its constants, `VolumeMatchModes`, `DefaultVolumeMatchMode`, the mode argument of `NewVolumeKeyRewriter`, `volumeModeTokenScope` / `tokenScopeKind` and the scanning (`contains` / `regex`) match path. Keep the length-bucketed index for suffix alone. Verify `go test ./pkg/build/ -run 'Volume'` passes with `volumekey_test.go` rewritten to pin: the stock Trident suffix match, a FlexVol named exactly the token matching, the `_clone` rejection, custom rewrite rules, and an invalid pattern failing construction.
- [x] 1.2 Delete `NetAppVolumeMatchMode` from `internal/config` (field, default, `--netapp-volume-match-mode` flag, `KSG_NETAPP_VOLUME_MATCH_MODE` lookup), and update `Config.VolumeKeyRewriter` and `cmd/kube-state-graph/main.go`. Verify `internal/config` tests assert that `--netapp-volume-match-mode=contains` fails `Parse` as an unknown flag, and that a set `KSG_NETAPP_VOLUME_MATCH_MODE` changes nothing.
- [x] 1.3 Make every caller of the removed mode API compile against suffix-only semantics. The volume-label restriction treats the join as always renderable; group 4 rewrites that code. Verify `go build ./...`, `go vet ./...` and `make test` pass.
- [x] 1.4 Update `docs/netapp-harvest-preconditions.md` (drop the mode row and the four-mode guidance), `docs/BREAKING.md` (the mode removal with its per-mode migration) and the NetApp bullets of `CLAUDE.md`. Verify `grep -rn "match-mode\|match mode\|VolumeMatch" docs CLAUDE.md pkg internal cmd` returns only historical `openspec/` text.

## 2. Root contract: parse, scope, projection

- [x] 2.1 Reshape `graph.StorageRoots` into one `Kind` (`ontap_cluster`, `ontap_node`, `aggr`, `svm`, `node`, `pod`, `application`) plus its sorted, de-duplicated values (pods as `PodRef`). Update `NewStorageScope` to match. Verify `pkg/graph/storagescope_test.go` pins empty-value dropping, de-duplication, the malformed-pod error and the kind.
- [x] 2.2 Rewrite `kubegraph.ParseStorageValues`:
  - accept `ontap_node`;
  - validate window, then `az`, then `env`, then roots;
  - zero kinds → `missing_root`; two or more → `invalid_scope` naming the parameters;
  - delete `deriveStorageNamespaces`.

  Verify parser tests cover every parse-level scenario of "One root kind per request" and "Storage-flow graph endpoint", including "Zone is checked before the root" and "An ONTAP cluster no longer qualifies an aggregate".
- [x] 2.3 Rewrite `ProjectStorage` / `resolveStorageRoots` as single-kind reachability:
  - `node` → Kubernetes nodes only;
  - `ontap_node` → controllers;
  - `aggr` / `svm` → by name on every filer;
  - `ontap_cluster` → every entity of the filer;
  - application pod hits materialised, claim hits retention-only.

  Delete the three-group AND and `inOC`. Verify `pkg/graph/project_storage_test.go` covers each kind, flowless admission per kind, the `cluster` / `namespace` filters, and the application claim-hit rule, and that `pkg/graph/property_test.go` still passes.
- [x] 2.4 Bridge the read plan to the new roots type: `storagePlan` maps the one kind onto today's plan fields, with `ontap_node` read like today's node-only request. This keeps every existing storage test green before group 4 replaces the plan. Verify `go test ./pkg/build/ ./pkg/kubegraph/ ./internal/api/` passes with the existing storage goldens byte-identical.
- [x] 2.5 Update `internal/api`:
  - `writeParseError` covers `missing_root`;
  - the swag `@Param` annotations for the storage route: add `ontap_node`, make `node` Kubernetes-only, state the one-kind rule.

  Run `make docs`. Verify component tests for `missing_root`, mixed kinds and an empty-valued root, and `make check-docs`.
- [x] 2.6 Update `kubegraph.Engine.BuildStorage` / `BuildStorageFromValues` for the new roots type. Verify the "Embedder and server agree" test passes for a valid request and for each new 400.
- [x] 2.7 Update `internal/integration/storage_graph_e2e_test.go` requests that were rootless or mixed kinds so each carries one root kind. Verify `go test ./internal/integration/ -run 'StorageGraph'` passes (Docker).
- [x] 2.8 Update `docs/BREAKING.md` (one root kind, `missing_root`, `node=` Kubernetes-only and the empty-200 hazard for a controller name, `ontap_node=`, `ontap_cluster=` no longer a qualifier, the `graph.StorageRoots` / engine signature change) and the request-surface bullet of `CLAUDE.md`. Verify each breaking bullet of `proposal.md` has a `BREAKING.md` entry.

## 3. Query layer: by-reference renderers

- [x] 3.1 Add renderers for the new scope keys:
  - `kube_pod_info` by `node`;
  - the claim-binding family by `namespace` + `pod`;
  - `kube_persistentvolumeclaim_annotations` by tracking-id (extend `trackingIDFamily`);
  - `volume_labels` by `node`;
  - the three `aggr_*` families by `(cluster, aggr)`;
  - the six controller families by `(cluster, node)`;
  - the two fixed-policy families by `(cluster, svm)`.

  Verify `pkg/promql` tests pin each rendered string (fixed selector, then request matchers, then scope), regex escaping, and `ok == false` for an empty set.
- [x] 3.2 Add per-ONTAP-cluster chunking for the pair-keyed renderers, charging the repeated `cluster` equality at its rendered length (the owner-completion precedent). Verify chunk-boundary tests and that one over-budget value is still issued alone.
- [x] 3.3 Verify the query tables are untouched: `go test ./pkg/promql/ -run 'TestRender_EmptySelectorMatchesBaseline|TestQueryDims_EveryQueryListed|TestQueryFamily_EveryQueryListed'` passes with `render-baseline.txt` unchanged.

## 4. Seeds: root → tracked claim set

- [x] 4.1 Build the parity harness before any seed lands. For a fixture corpus, compare `buildStorage` under the new tracking plan against `buildStorage` under `fullPlan` for every root kind, after `ProjectStorage` and `cytoscape.Serialise`, requiring byte-identical bodies. Corpus: takeover, clone, cross-filer FlexVol collision, an RWX claim across nodes, a pod rescheduled inside the window, an inherited Application, a FlexGroup claim, a flowless root of every kind. Verify the harness runs green against the bridged plan of 2.4.
- [x] 4.2 Replace the storage `topologyPlan`:
  - it carries kind + values;
  - its first wave launches `ALERTS` only;
  - `restrictsVolumeLabels`, `resolveVolumeLabelRead`, the `hub` flag and every unrestricted fallback are deleted;
  - add `build.ReasonInvalidScope`, mapped by `mapBuildError` to 400 `invalid_scope`, raised when a root kind's first read renders past the cap before any query is issued.

  Verify plan unit tests and an `internal/api` test showing an over-large `aggr=` set returns 400 with zero upstream calls.
- [x] 4.3 Storage seeds:
  - `ontap_cluster` / `aggr` / `svm` phase-1 reads;
  - the `ontap_node` seed (`{node}` read, touched aggregates read whole, owner-filtered claim source);
  - `pvCandidates` → `kube_persistentvolumeclaim_info{volumename}`;
  - `storage_root_claim_miss` with `no_claim` at Debug under a `cluster` / `namespace` filter.

  Verify seed tests for every storage-kind scenario of "Every root kind is tracked from its own tier to its claims" (including "A touched aggregate another controller owns contributes no claim"), plus parity cases.
- [x] 4.4 Kubernetes node seed: `kube_pod_info{node}`, then incarnation completion `kube_pod_info{pod}`, then claim bindings by pod for the pods whose newest incarnation runs on a root node. Verify the "reads pods by node" and "rescheduled inside the window" scenarios plus parity.
- [x] 4.5 Pod seed: bindings by `namespace` + `pod` filtered to the root refs, plus `kube_pod_info` for the roots. Verify the "A pod root reads its bindings by reference" scenario and the claimless-pod-root parity case.
- [x] 4.6 Application seed:
  - the existing three-stage recovery, plus `kube_persistentvolumeclaim_annotations` by tracking-id;
  - bindings by pod for the recovered pods;
  - a stage-1 / claim-annotation over-cap → `ReasonInvalidScope`;
  - delete `podScopeUnderApp`.

  Verify `appscope_test.go` covers every scenario of "Application roots are tracked through their controllers and claims" plus parity.
- [x] 4.7 Flowless-root reads per kind (aggregate gauges for `aggr` / `ontap_cluster`, controller families for `ontap_node` / `ontap_cluster`, Kubernetes-node families for `node`, `kube_pod_info` for `pod`). Verify each kind's "still drawn" scenario, including "Controller with no claims still shows".
- [x] 4.8 Add storage goldens for an `ontap_node=` root and a `node=` root beside the existing aggr / application / pod-root goldens. Verify `go test ./internal/api/ -run Golden` passes, and that the existing storage goldens are unchanged without `-update`.

## 5. Expansion: tracked claims → both ends

- [x] 5.1 Claim side by claim name, filtered to the tracked `(cluster, namespace, claim)` keys. Add mounter completion: bindings by claim, and every mounter's `kube_pod_info` / `kube_pod_owner`. Delete the `maxHubClaimChunks` fallback. Verify "A shared claim keeps its split across nodes", "A same-named claim in another namespace is filtered out" and "A large tracked set is chunked, never read across the zone".
- [x] 5.2 Candidate completion over every tracked claim's token. Owner completion over every aggregate any volume-label row names, minus aggregates already read whole, plus the phase-2-only aggregates under an `svm` root. Merge all volume-label reads by fingerprint; completion rows are never a claim source. Verify the clone, cross-filer, takeover and SVM-owner-completion scenarios plus parity.
- [x] 5.3 Rescope the Kubernetes-node and controller waves over all loaded pods (tracked mounters ∪ root pods ∪ recovered pods). Verify "Node read is restricted to the loaded pods' nodes and node roots" and the controller-stage scenarios.
- [x] 5.4 Harvest by reference:
  - aggregate gauges by the reached `(cluster, aggr)` pairs ∪ roots;
  - controller families by the reached `(cluster, controller)` pairs ∪ roots;
  - fixed-policy families by the tracked claims' `(cluster, svm)` pairs, beside the QoS workload read.

  Verify "Harvest gauges are read for the reached components only" plus parity for ceilings and controller hardware / perf.
- [x] 5.5 Verify:
  - an empty tracked set issues no expansion query;
  - every chunk error of every new read fails the build naming its family;
  - the per-family tally sums every read of a family and omits unissued families;
  - `go test -race ./pkg/build/` passes.
- [x] 5.6 Update `docs/upstream-metrics.md` (per-kind read plan and leg counts) and the storage-build bullets of `CLAUDE.md` (replace the hub-mode, rooted volume-label and pod-namespace-derivation text with seed + expansion + completions). Verify `grep -n "hub mode\|hub-mode\|deriveStorageNamespaces" CLAUDE.md docs` returns nothing.

## 6. Fan-out pins

- [x] 6.1 Rewrite `TestBuildStorage_FanOutLegCount*` as per-kind leg-set pins (seed, expansion, flowless-root reads) replacing the 18 / 38 / hub counts. Add a test asserting every non-`ALERTS` query of a storage build carries a restriction beyond the request matchers. Verify `go test ./pkg/build/ -run 'FanOut|OnlyAlertsUnrestricted'`.
- [x] 6.2 Rewrite `internal/integration` `TestStorageGraph_RootedVolumeLabelsMatchTheWholeFilerRead` into per-kind tracked-vs-`/v1/graph`-consistency checks, and add e2e cases for `ontap_node=`, `node=`, `missing_root` and mixed kinds. Verify `go test ./internal/integration/ -run 'StorageGraph'` passes (Docker).

## 7. Integration checks

- [x] 7.1 Run `make lint`, `make vet`, `make test`, `make check-docs`, `make verify-mocks` and `make check-route-containment`. Verify all pass.
- [x] 7.2 Run `openspec validate trace-storage-roots-by-reference --strict`. Verify it passes after any artifact touch-ups the implementation required.
- [ ] 7.3 In the demo, run `make redeploy-backend BACKEND_SRC=<this checkout>` and exercise `ontap_node=`, `node=`, `aggr=`, `svm=`, `ontap_cluster=`, `pod=` and `application=` requests plus `missing_root` / mixed-kind 400s. Verify every rooted body is non-empty where the estate has data and `node=<ONTAP controller>` is an empty 200. The frontend and `verify.sh` updates are follow-up changes in their own repositories, listed in the PR description.
