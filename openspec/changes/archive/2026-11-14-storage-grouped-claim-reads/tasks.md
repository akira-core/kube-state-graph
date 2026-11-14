# Tasks

## 1. Pin the cost and the cross-pair rule

- [x] 1.1 Add `TestBuildStorage_ManyNamespaceRootGroupsItsReads` (`pkg/build/storage_grouped_reads_test.go`): a node root whose 40 mounting pods live in 40 namespaces issues `kube_pod_info` 3 times (seed, incarnation, pod wave), `kube_pod_owner` once, the binding family twice and each claim family once, and still builds every pod and claim. Verify RED on the per-namespace reads (81 / 40 / 80 / 40 queries).
- [x] 1.2 Add `TestIssuePodFamilies_DropsCrossPairs`: a grouped read of `tenant-a/db-0` and `tenant-b/web-0` drops the `tenant-b/db-0` row the two alternations admit, issues one query, and tallies the dropped row in `ExtraSeriesCount`. Verify RED.

## 2. Grouped reads (`pkg/build`)

- [x] 2.1 Lift `k8sRead.issuePairReads` and `pairBudget` into free functions the methods delegate to (design D1, D3).
- [x] 2.2 Replace `issuePodFamiliesByNamespace` / `queryPodsByNamespace` with `issuePodFamiliesByPair` / `queryPodsByPair` over `issuePairReads`, keeping only requested pairs (`promql.KeepNamePairs`) and tallying the rest (design D2, D5); drop `groupPodRefs` for `podNamePairs`.
- [x] 2.3 Replace `issueClaimFamiliesByNamespace` with `issueClaimFamiliesByPair` over `issuePairReads`, keeping the `claimKey` filter and tallying what it drops (design D2).
- [x] 2.4 Leave a `pvc` root's seed on `RenderNamesInNamespace` (design D4).
- [x] 2.5 Verify 1.1 and 1.2 pass and `go test ./pkg/build ./pkg/kubegraph ./internal/api` passes after updating the per-namespace pins (`TestPodWave_GroupedAcrossNamespaces`, `TestVolumeHub_ClaimFamiliesAreGroupedAcrossNamespaces`, `TestPodSeed_ReadsBindingsByReference`, `TestParseStorageValues_PodRootDoesNotDeriveNamespace`, `TestStorageGraph_PodOnlyRootsNarrowTheRead`).

## 3. Pins and parity

- [x] 3.1 Check body parity: the serialised `ProjectStorage` bodies of 17 root shapes over `planEstate()` (node ×4, pod ×6, application ×2, aggr ×2, ontap_cluster, svm, pvc) are byte-identical before and after (design D6).
- [x] 3.2 Refresh `pkg/build/testdata/storage_fanout.golden` with `-update-storage-fanout` and confirm only Kubernetes pod- and claim-keyed lines moved.
- [x] 3.3 Verify `TestBuildK8s_PlanIsOutputPreserving`, the storage parity tests and `go test -race ./pkg/build ./pkg/kubegraph ./internal/api` pass.

## 4. Docs

- [x] 4.1 Update `docs/upstream-metrics.md` (claim-keyed and pod-keyed reads grouped; hub fan-out wording), `.claude/rules/storage-graph.md` (grouped restriction now shared; the `pvc` seed exception) and `.claude/rules/netapp-storage-join.md` (`issueClaimFamiliesByPair`).
- [x] 4.2 Run `openspec validate storage-grouped-claim-reads`.
