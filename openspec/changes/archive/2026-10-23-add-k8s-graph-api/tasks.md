# Tasks

## 1. Kubernetes-only read plan and builder (`pkg/build`)

- [x] 1.1 Add `k8sSkippedLegs` (`storageSkippedLegs` plus every Harvest first-wave family, derived from `topologyLegs` and `promql.FamilyOf`) and a `kubernetesOnly` plan field; add `k8sPlan(roots)` = `storagePlan(roots)` with that skip set and `kubernetesOnly: true` (design D1); verify the existing `/v1/graph` and `/v1/storage-graph` fan-out and plan tests pass unchanged
- [x] 1.2 Gate the Harvest waves on `kubernetesOnly` in `readTopology`: do not launch `readWorkloadVolumeLabels` or `readReachedHarvest`, close the `volumeLabelsFinal` signal up front so `readScopedQoS` issues nothing; and make a `node` root's seed load the root node's claimless pods too (`RootNodePods`, design D1); verify with a fan-out test (modelled on `TestBuildStorage_FanOutLegCount`) that a `pod` k8s read issues exactly the storage plan's Kubernetes queries for the same root, that a `node` read issues the same families plus the pod-wave reads widened by exactly the root node's claimless pods, and that neither issues a Harvest, service-side, `kube_pod_container_info`, `traces_service_graph_*` or `up` query
- [x] 1.3 Add `Builder.BuildK8s(ctx, window, end, sel, roots)` per design D2 (only `pod` / `node` kinds, else `ReasonInvalidScope`; seed cap before any query; `assemble(topology, ServiceGraphResult{})`; alerts, status, freeze, identity / owner / deployer maps; no service-graph read, no `up{}` probe, no last-build gauges); verify with mock-querier tests whose fixture also holds Harvest and service-graph series that the graph holds only `pod` / `node` / `pvc` nodes and `pod-to-node` / `pod-mounts-pvc` edges, and that too many `node=` values return `invalid_scope` with no query issued
- [x] 1.4 Test attribute and grouping guarantees on a `BuildK8s` graph: pod `owner` + `application` via Deployment and via Job → CronJob, a shared claim inheriting the lexically-smallest Application over ALL its mounters (mounter completion), node `ready_status` / `ipaddress`, claim `usage` / `storageclass`, `alerts` / `status` on all three, `NamespaceOwners` / `ApplicationDeployers` with an application-metadata source; verify `go test ./pkg/build/ -run BuildK8s` passes
- [x] 1.5 Test fail-closed: an error on `kubelet_volume_stats_used_bytes`, on a `kube_replicaset_annotations` chunk and on `app_metadata` each fails `BuildK8s` with a `QueryError` naming the family; an `ALERTS` error yields an alert-less graph; an empty vector does not fail; and no build logs `netapp_volume_join_miss` / `netapp_qos_join_miss`; verify the tests pass
- [x] 1.6 Add a Router-based test (following `pkg/build/hubrouting_test.go`) with separate zone-`a` and zone-`b` backends: a k8s build for `az=a` sends nothing to zone `b`, and every query carries the request matchers its family accepts (`ALERTS` without `cluster`); verify it passes
- [x] 1.7 If the bake-before-freeze sequence is extracted into a shared helper (design D2), verify every existing `/v1/graph` and `/v1/storage-graph` golden passes without `-update`

## 2. Projection (`pkg/graph`)

- [x] 2.1 Add `ProjectK8s(g, scope StorageScope)` per design D3 (pods by root and filters, claims and nodes by reference, root nodes always, edges whose source is a kept pod, sorted); verify with table tests covering a pod root, a node root, a shared claim whose other mounter is not drawn, a podless node root, an unscheduled pod root, an unknown root, a `namespace` filter on a node root, a `cluster` filter across two identities, and a non-pod/node kind returning an empty view
- [x] 2.2 Add a property test (beside `property_test.go`) on random graphs: every kept edge's endpoints are kept, every kept claim / node hangs off a kept pod or is a root node, and the view is a pure function of `(g, scope)`; verify it passes under `-race`

## 3. Parser and engine (`pkg/kubegraph`)

- [x] 3.1 Generalise `oneStorageRoot` to take its parameter list, keeping `/v1/storage-graph` messages byte-identical; verify `parse_storage_test.go` passes unchanged
- [x] 3.2 Add `K8sRequest` and `ParseK8sValues` per design D4; verify with a table test in `parse_k8s_test.go` covering `missing_cluster` / `missing_az` / `missing_env` in that order, empty values, `missing_root`, `pod` + `node` → `invalid_scope` naming both, a malformed `pod` ref, `aggr=` alone → `missing_root`, over-long / control-character / invalid-UTF-8 values → `invalid_scope`, sorted de-duplicated selector sets, and `prune` ignored
- [x] 3.3 Add `Engine.BuildK8s` and `Engine.BuildK8sFromValues` (parse → `AlignWindow` → build → `ProjectK8s` → `cytoscape.Serialise`) and a runnable `Example`; verify an engine test that a request lacking a root returns `*ParseError{Reason: "missing_root"}` with no upstream query and a composed identity sent as `cluster` returns an empty body

## 4. HTTP endpoint (`internal/api`)

- [x] 4.1 Add `handleK8sGraph` per design D5 and register `GET /v1/k8s-graph` on the authenticated `v1` group; verify with component tests for the 200 shape, each `missing_*` 400, `invalid_scope` for both kinds, 401 without a key when keys are configured, 504 on build timeout, 502 `upstream query failed: <family>` on a non-`ALERTS` error, and an empty 200 (no `outside_retention`, no `up` query) for an unknown root
- [x] 4.2 Add swag annotations (product selection, RAW `cluster`, `pod` / `node` exactly one, `namespace` filters pods, what is drawn, mounted claims only, fail-closed; 400 / 401 / 502 / 504) and run `make docs`; verify `make check-docs` passes with `docs/swagger.{json,yaml}` committed
- [x] 4.3 Add goldens `k8s-graph-node-root-cytoscape.json` and `k8s-graph-pod-root-cytoscape.json` over an estate with two zones sharing a cluster name, a shared claim, a podless node, an unmounted claim and an ArgoCD-annotated Deployment / CronJob, following the existing hand-built-graph pattern (`stampFixtureStatuses`); verify they pass, repeat byte-identically, and every existing golden is unchanged without `-update`
- [x] 4.4 Add component tests that a `/v1/k8s-graph` request after a `/v1/graph` request leaves the last-build gauges at the `/v1/graph` values, and that the endpoint and `Engine.BuildK8sFromValues` return byte-identical JSON for the same parameters and upstream; verify both pass

## 5. Documentation

- [x] 5.1 Document `GET /v1/k8s-graph` in `README.md` and its fan-out and error class in `docs/upstream-metrics.md` beside the `/v1/storage-graph` sections; verify every family and count stated matches the 1.2 fan-out test
- [x] 5.2 Update `CLAUDE.md` (request lifecycle, request-surface bullet, `pkg/` engine list), `.claude/rules/storage-graph.md` (the k8s plan shares the `pod` / `node` seeds) and `.claude/rules/graph-model.md` (which endpoint emits which node / edge types); verify the method names and counts match the code

## 6. Integration

- [x] 6.1 Add an integration test in `internal/integration` that ingests a Kubernetes + Harvest fixture and asserts a `node=` and a `pod=` `/v1/k8s-graph` body carry the expected pods, nodes, mounted claims and Application groups and no NetApp element; verify it passes where Docker is available
- [x] 6.2 Run `make ci` and verify it passes (lint, vuln, race tests, `check-docs`, `verify-mocks`, containment checks)

## Workflow follow-up

- Run `/opsx:verify` against this change before archiving.
- Archive with `openspec archive add-k8s-graph-api` once review is satisfied.
