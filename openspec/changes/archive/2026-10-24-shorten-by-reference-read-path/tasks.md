# Tasks

## 1. Safety nets

- [x] 1.1 Add `TestBuildStorage_FanOutUnchanged`: record the issued query names and rendered strings of `BuildStorage` for `pod` and `node` roots over `planEstate()`, compare them against a checked-in expectation, and verify it passes on the unmodified code (design D1).
- [x] 1.2 Add a lockstep round counter in `pkg/build` tests that wraps `promqlfake` and runs the build inside a `testing/synctest` bubble. Each query blocks on a bubble channel; after `synctest.Wait()` the counter records one round, releases every blocked query and repeats until the build returns. Keep each fixture wave under `scopeConcurrency` (16) queries so one logical round is never split. Verify it reports 7 for a Deployment-owned node root and 5 for a mounting pod root on the unmodified code, with no sleeps.
- [x] 1.3 Add a parse order-insensitivity test: shuffle each `topologyVectors` slot across keys, keeping each key's own rows in order, and assert a byte-identical serialised body over `planEstate()`. Verify it passes on the unmodified code (design D5).
- [x] 1.4 Add round-count tests for the spec's "completes in three rounds" and "completes in two rounds" scenarios using the lockstep querier, in two variants each: StatefulSet-owned pods (no second controller stage) and Deployment-owned pods. Verify they fail now (red).

## 2. Grouped `(namespace, name)` restriction

- [x] 2.1 Add a pair renderer to `pkg/promql`, generalising `RenderClaimBindingsByPod` to any scoped family and identity label: fixed selector, then request matchers, then the namespace alternation, then the name alternation. Verify with table tests for a single value, several values, escaping and the `unknown` cluster form, and confirm `render-baseline.txt` does not move.
- [x] 2.2 Add a pair chunker that sorts pairs by `(namespace, name)` and packs them greedily. Each chunk is charged the request-matcher cost plus its own namespace and name alternations, and a single over-budget pair is still issued. Verify with tests for budget edges, a single huge pair, and chunks staying namespace-local.
- [x] 2.3 Add a pair-filter helper that keeps the rows whose `(namespace, name)` was requested. Verify a unit test drops a cross pair and keeps input order.

## 3. k8s workload reader — node root

- [x] 3.1 Add `readK8sWorkload`. Under `kubernetesOnly`, `readTopology` launches it in place of the node / pod seed, `readWorkloadClaims` and the four scoped waves. It closes `bindingsDone`, `pvcInfoDone` and `pvcAnnotationsDone` on every return path. Verify the storage path is untouched by 1.1 passing and the k8s build still finishing with the QoS read issuing nothing.
- [x] 3.2 Implement node-root rounds r1–r3 (design D2):
  - r1: the seed.
  - r2: incarnation completion, bindings and `kube_pod_owner`, grouped on the seed pairs.
  - Go: pair filter, then `podsNewestOn` (the root-node pods) and the tracked claims.
  - r3: mounter completion, claim families, `app_metadata` from the root pod rows, `kube_node_*`, and controller stage A.

  Mark `ScopeIssued` / `ExtraSeriesCount` as the storage readers do. Verify `TestBuildK8s_PlanIsOutputPreserving` passes for every node-root case.
- [x] 3.3 Replace `TestBuildK8s_FanOutIsTheStoragePlansKubernetesHalf` with explicit k8s fan-out assertions. Cover the spec scenarios "Node root query set", "Node root reads its claimless pods", "Several namespaces in one query", "A cross pair is discarded", "Claims are grouped the same way" and "A large pair set is chunked". Keep the no-Harvest / no-service-family assertions.
- [x] 3.4 Add output-preservation cases for a rescheduled pod and for a cross pair whose same-named pod runs off the root node. Verify they pass with a byte-identical body.
- [x] 3.5 Update the `/v1/k8s-graph` section of `docs/upstream-metrics.md` and the k8s section of `.claude/rules/storage-graph.md` with the node-root rounds and the grouped restriction. Rewrite the rules file's "not a second reader … cannot drift" statement, and the matching `CLAUDE.md` line, to: shared semantic helpers, k8s-only orchestration (`readK8sWorkload`), drift pinned by `TestBuildK8s_PlanIsOutputPreserving` and `TestBuildStorage_FanOutUnchanged`. Verify the documented query order matches what the 3.3 fan-out assertions pin.

## 4. k8s workload reader — pod root

- [x] 4.1 Implement pod-root rounds:
  - r1: the binding seed, plus the roots' `kube_pod_info` and `kube_pod_owner` issued in the seed's own chunk split.
  - r2: mounter completion, claim families, `app_metadata` from the root pod rows and tracked claims (design D4), `kube_node_*` and controller stage A.

  Verify every pod-root case of `TestBuildK8s_PlanIsOutputPreserving` passes.
- [x] 4.2 Add a test for the spec scenario "A root set the cap accepts is never rejected later": a pod root set at the cap issues the roots' pod reads in exactly the seed's chunk count. Verify `TestBuildK8s_RejectsBeforeAnyQuery` still passes.
- [x] 4.3 Add the pod-root rounds to the docs sections from 3.5. Verify the documented query order matches the pod-root fan-out assertions.

## 5. Co-mounter delta reads

- [x] 5.1 After mounter completion, read `kube_pod_info` / `kube_pod_owner` only for binding pods not already loaded. Then read the controller and node families only for names no earlier read of that family covered, appending in round order (design D5). Verify the "shared claim with an undrawn mounter" case of `TestBuildK8s_PlanIsOutputPreserving` and `TestBuildK8s_ClaimInheritsFromUndrawnMounter` pass.
- [x] 5.2 Add tests for the spec scenarios "A shared claim adds the co-mounter's reads" and "A name is read once per family", the latter asserting no second `kube_statefulset_annotations` query names the shared StatefulSet. Verify `TestBuildK8s_NodeRootPodWaveIsClusterKeyed` still passes.
- [x] 5.3 Verify the StatefulSet-owned variants of the 1.4 round-count tests now pass: 3 rounds for the node root, 2 for the pod root.

## 6. Early controller second stage

- [x] 6.1 Issue `kube_deployment_annotations` / `kube_cronjob_annotations` in stage A's round on candidate names: each ReplicaSet / Job name with its final `-` segment removed, plus the direct owners. After stage A, read the difference against `deploymentScope` / `cronJobScope` and append it (design D6). Verify the Deployment-owned variants of the 1.4 round-count tests now pass: 3 rounds for the node root, 2 for the pod root.
- [x] 6.2 Add tests for the spec scenarios "A Deployment is read with its ReplicaSet", "A CronJob is read with its Job", "A name outside the convention is read afterwards" and "A wrong early name leaves the body unchanged". Add matching output-preservation cases.
- [x] 6.3 Document the early second stage in the docs sections from 3.5. Update the `/v1/k8s-graph` line of the request lifecycle in `CLAUDE.md`. Verify the docs name every read the 6.2 tests pin, in the same order.

## 7. Integration

- [x] 7.1 Run `make ci` (lint, vuln, test with `-race -shuffle=on`, check-docs, verify-mocks, containment) and verify it passes.
- [x] 7.2 Run `internal/integration/k8s_graph_e2e_test.go` against the testcontainers VictoriaMetrics (`v1.107.0`). Verify they pass with unchanged bodies.
- [x] 7.3 Run `openspec validate shorten-by-reference-read-path --strict` and verify it reports the change valid.

## Workflow follow-up

- Run `/opsx:verify` against this change before archiving.
- After deploying, compare `kube-state-graph.build_k8s` span durations for `node` roots before and after on a real estate.
- Archive with `openspec archive shorten-by-reference-read-path` once review is complete.
