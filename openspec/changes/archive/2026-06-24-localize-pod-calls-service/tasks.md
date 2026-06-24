## 1. Edge-type registry + cross-cluster flags (`pkg/graph`)

- [x] 1.1 In `pkg/graph/registry.go`, set `EdgeTypeServiceSelectsPod.MayCrossCluster = true` and `EdgeTypePodCallsService.MayCrossCluster = false` (the swap).
- [x] 1.2 Rewrite the three edge-type `Description` strings: `service-selects-pod` (local service node fans out to backing pods across same-family clusters holding the same-named Service; may cross cluster), `pod-calls-service` (resolves to a single service node in the caller's OWN cluster; always intra-cluster; no per-family node fan-out), `pod-calls-pod` (drop the unknown-family-fallback wording).
- [x] 1.3 Confirm `pkg/graph/graph.go` needs NO change — the registry-driven `neverCrossCluster` gate now auto-routes `service-selects-pod` through `isCrossCluster` and skips `pod-calls-service`. Verify by reading `EdgeCountByType` / `neverCrossCluster`.
- [x] 1.4 Update any `pkg/graph` registry/edge-types unit tests asserting the old `may_cross_cluster` values for these two types.

## 2. Resolver rewrite (`pkg/build/servicegraph.go`)

- [x] 2.1 Trim `sgResolver`: remove fields `svcHolderFamilies`, `knownFamilies`, `epVisibleClusters`. Keep `svcCandidates`, `endpointsByService`, `podByID`, `podByUID`, `externals`, `synthPods`, `services`, `svcEdges`.
- [x] 2.2 Remove types `holderFamily`, `nsSvcKey` and functions `loadedUniqueFamilyHolders`, `endpointBacked`.
- [x] 2.3 Simplify `parseServiceGraph` index build: keep building `svcCandidates` (sorted by cluster); delete the `svcHolderFamilies`, `knownFamilies` (`addKnownFamily`), and `epVisibleClusters` build loops.
- [x] 2.4 Rewrite `resolveServiceLevel(anchorCluster, ns, svc)`: compute `family = clusterFamilyKey(anchorCluster)`; from `svcCandidates[famSvcKey{family, ns, svc}]` select the candidate whose `cluster == anchorCluster` (else return nil → external); materialise ONE service node at the anchor via the split node-creator; then emit `service-selects-pod` edges over the UNION of `endpointsByService[{c, ns, svc}]` across ALL candidates `c`. Return `[localSvcID]`.
- [x] 2.5 Split `materializeService` into `materializeServiceNode(cluster, ns, svc, obs) string` (idempotent node creation only, anchor's own `ServiceObs` for `cluster_ip`) — the cross-cluster endpoint fan-out now lives in `resolveServiceLevel` (task 2.4). `addServiceEdge` reused unchanged.
- [x] 2.6 Update the doc comments on `resolveConnString` / `resolveServiceLevel` to describe single-local-node resolution + cross-cluster fan-out and the removal of the fallback/pruning.
- [x] 2.7 Confirm anchor threading is unchanged in `parseServiceGraph` (server-side `familyCluster` = client-pod cluster else trace label; client-side anchor = trace label) and that `betterSrcCluster` / edge-type-by-target logic still hold.

## 3. Build-layer unit tests (`pkg/build`)

- [x] 3.1 Rewrite `servicegraph_family_test.go`: replace per-family node-fan-out expectations with single-local-node + cross-cluster `service-selects-pod` union; cover anchor-holds-Service (one node, cross-cluster endpoints), anchor-lacks-Service → external, anchor-unknown-with-holder (fully-unlabelled) → resolves, anchor-unknown-without-holder → external, bogus-label anchor → external, out-of-family sibling not unioned, both-`"://"` single intra edge.
- [x] 3.2 Remove/replace tests for deleted machinery (`loadedUniqueFamilyHolders`, `endpointBacked`, unknown-family fallback, endpoint-backed pruning) in `servicegraph_test.go` / `servicegraph_fixes_test.go`.
- [x] 3.3 Add/adjust a determinism test asserting byte-stable output for the cross-cluster fan-out (sorted candidates + `SortEdges`).
- [x] 3.4 Run `go test ./pkg/build/... -race -count=1` green.

## 4. Component + integration tests

- [x] 4.1 Update `internal/api` component tests that exercise connection-string cross-cluster resolution to the new single-node + cross-cluster-`service-selects-pod` shape.
- [x] 4.2 Update `internal/integration` cross-cluster connection-string cases (e.g. `TestConnString*`) for the new behavior; ensure the dedicated namespaces/pods avoid cross-test series leakage.
- [x] 4.3 Regenerate golden files: `go test ./internal/api/ -update -run Golden`, then review the diff for the swapped cross-cluster buckets and single service node.
- [x] 4.4 Run `make test` (full `-race -shuffle=on`) green.

## 5. Docs + generated artifacts

- [x] 5.1 Update the CLAUDE.md D29 / connection-string-resolution and edge-types sections: single local service node, cross-cluster `service-selects-pod`, flag swap, removal of unknown-family fallback + endpoint-backed pruning.
- [x] 5.2 Update README (EN) and the Traditional-Chinese README to match.
- [x] 5.3 Regenerate OpenAPI: `make docs`; commit `docs/` (edge-type catalogue `may_cross_cluster` values change).
- [x] 5.4 Run `make check-docs` and `make verify-mocks` clean (mocks unaffected, but confirm).

## 6. Verification

- [x] 6.1 `make lint` 0 issues; `make vet` clean; `make vuln` clean.
- [x] 6.2 `openspec verify "localize-pod-calls-service"` passes.
- [x] 6.3 Full pre-push CI mirror green (lint + vet + vuln + `make test` + docs/mocks drift).
