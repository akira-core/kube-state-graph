## 1. PromQL query layer

- [x] 1.1 Add seven `Query` constants to `pkg/promql/queries.go` — `QDeploymentAnnotations`, `QStatefulSetAnnotations`, `QDaemonSetAnnotations`, `QReplicaSetAnnotations`, `QJobAnnotations`, `QCronJobAnnotations`, `QJobOwner` — each with a doc comment naming its identity label (`deployment` / `statefulset` / `daemonset` / `replicaset` / `job_name` / `cronjob` / `job_name`; call out that the Job families use `job_name`, NOT `job`) and the `--metric-annotations-allowlist` precondition; verify `go build ./...` succeeds.
- [x] 1.2 Add seven `Render` cases emitting `last_over_time(<bare metric name>[w])` with no fixed selector; verify `go test ./pkg/promql/ -run 'TestRender'` passes.
- [x] 1.3 Add seven `queryDims` entries as `dimsNamespaced` (design D5: all four dimensions); verify `go test ./pkg/promql/ -run TestQueryDims_EveryQueryListed` passes with no "has no queryDims entry" error.
- [x] 1.4 Append the seven rendered lines to `pkg/promql/testdata/render-baseline.txt` (tab-separated `<query-name>\t<rendered PromQL>`, matching the existing `kube_service_annotations` row's style); verify `go test ./pkg/promql/ -run TestRender_EmptySelectorMatchesBaseline` passes.
- [x] 1.5 Add `TestQueryDims_ControllerAnnotationFamiliesAreNamespaced` to `pkg/promql/selector_test.go` pinning all seven families to `dimsNamespaced` AND to the rendered `az,env,cluster,namespace` matcher set (spec scenario "Controller-annotation families receive all four dimensions"; design D5 — the sibling of the existing `dimsNone` / Harvest group pins); verify it fails when any family is switched to `dimsClusterScoped`.

## 2. Topology fan-out

- [x] 2.1 Add seven `model.Vector` fields to `topologyVectors` in `pkg/build/topology.go` and seven `g.Go(fetch(...))` legs in `ReadTopology` — `fetch`, not `fetchOptional`, per design D4 — plus seven `RawSeriesCount` entries; verify `go build ./...` succeeds.
- [x] 2.2 Update the fan-out pin in `pkg/build/netapp_test.go` (`TestReadTopology_FanOutLegCount`) and its comment from 30 to 37 legs; verify `go test ./pkg/build/ -run TestReadTopology_FanOutLegCount` passes.

## 3. Controller-annotation resolution

- [x] 3.1 Add a `controllerKey{cluster, namespace, kind, name}` type and `resolveControllerApplications(v topologyVectors, mc missingClusterCounts) map[controllerKey]string` that calls the existing generic `resolveApplications` once per annotation family with a `keyOf` stamping the constant owner kind, then merges the six disjoint results (design D1); verify a new unit test asserts one entry per family and that the merge is independent of call order.
- [x] 3.2 Add `resolveJobCronJobOwners(vec model.Vector, mc missingClusterCounts) map[jobKey]string` over `kube_job_owner`, keeping only `owner_kind="CronJob"` AND `owner_is_controller="true"` and picking the lexically-smallest `owner_name` on collision (design D3); verify a unit test covers the controller filter and the collision pick.
- [x] 3.3 Rewrite `resolvePodApplications` to take `(owners map[podNameKey]ownerRef, ctrlApps map[controllerKey]string, jobCronJobs map[jobKey]string)` and resolve per pod: look up the pod's own resolved owner, and on a miss where the owner kind is `Job` follow the CronJob hop and look up again (design D2 — own annotation first, hop second); verify `git diff pkg/build/topology.go` shows `resolvePodOwners` and `rsToDeployment` unchanged.
- [x] 3.4 Delete the `argocd_tracking_id` read and the `bucketCluster`-instead-of-`mc.bucket` workaround comment, switching every new family to `mc.bucket(promql.Q…, …)` (design D6); verify no `argocd_tracking_id` reference remains in production code via `grep -rn argocd_tracking_id pkg/ --include='*.go' | grep -v _test.go` (task 4.7 deliberately keeps one in a test fixture).
- [x] 3.5 Wire the three resolvers into `parseTopology` so each pod's `ApplicationValue` is set before `graph.NewGraph` freezes the nodes, leaving the PVC inheritance pass (`pvcInheritedApps`) untouched; verify `go test ./pkg/build/ -run TestParseTopology_PVCInheritsApplicationFromMountingPod` still passes after its fixture is migrated in task 4.5.

## 4. Unit tests

- [x] 4.1 Add one `pkg/build/topology_test.go` case per supported owner kind (Deployment via ReplicaSet, StatefulSet, DaemonSet, bare ReplicaSet) asserting the resolved `application` and that no tracking-id key leaks into `Labels()`; verify all four pass.
- [x] 4.2 Add a CronJob-hop case (Job owner, no `kube_job_annotations` match, `kube_job_owner` → CronJob, `kube_cronjob_annotations` carries the tracking-id) asserting `application` resolves AND `Owner()` is still `{kind:"Job", name:<job>}`; verify it passes.
- [x] 4.3 Add a "Job's own annotation wins before the hop" case (both `kube_job_annotations` and a CronJob with a different tracking-id present) asserting the Job's own Application is chosen; verify it passes.
- [x] 4.4 Replace `TestParseTopology_PodApplicationFromNonControllerRow` — its premise (a tracking-id surviving on a non-controller `kube_pod_owner` row) no longer exists — with a case asserting a pod with no controller owner resolves no `application` and no lookup is attempted; verify the old test name is gone and the new one passes.
- [x] 4.5 Retarget `TestParseTopology_PodApplicationAttribute` and `TestParseTopology_PodApplicationDeterministic` onto controller-annotation fixtures, keeping the no-colon-verbatim, empty-leading-segment, and lexically-smallest-raw assertions; verify both pass.
- [x] 4.6 Add an unsupported-owner-kind case (`Rollout` and `ReplicationController`) asserting the pod keeps its `Owner()`, resolves no `application`, and the parse does not fail; verify it passes.
- [x] 4.7 Add a case asserting a populated pod-level `argocd_tracking_id` on `kube_pod_owner` is ignored (spec scenario "Pod-level tracking-id label is not read"); verify it passes.

## 5. Component and integration tests

- [x] 5.1 Migrate `internal/api/server_application_containers_test.go` from the `kube_pod_owner` `argocd_tracking_id` fixture to a `kube_deployment_annotations` fixture, keeping its assertion that no tracking-id key appears in `data.labels`; verify `go test ./internal/api/ -run TestGraphEndpoint_PodApplicationAndContainers` passes.
- [x] 5.2 Migrate `internal/integration/graph_e2e_test.go`'s `TestPodApplicationAndContainersAttributes` ingest to emit `kube_deployment_annotations` (or the DaemonSet family, matching its existing owner fixture) instead of the pod-level label; verify the case passes against the VictoriaMetrics container.
- [x] 5.3 Migrate `TestPVCInheritsApplicationFromMountingPod`'s two pods to resolve their Applications from controller annotations; verify the inherited-vs-own precedence assertions still hold.
- [x] 5.4 Add an integration case for the CronJob hop (`kube_pod_owner` Job → `kube_job_owner` CronJob → `kube_cronjob_annotations`) asserting `data.application` is set, `data.owner` is still the Job, and the pod nests under the expected `application` / `controller` compound groups; verify it passes.
- [x] 5.5 Confirm the golden files are unaffected — `buildWithStorageClass` in `internal/api/golden_test.go` constructs `graph.PodNode{ApplicationValue: …}` directly and never exercises the topology reader; verify `go test ./internal/api/ -run TestGolden` passes **without** `-update` and `git status` shows no change under `internal/api/testdata/golden/`.

## 6. Documentation

- [x] 6.1 Add the seven series to `README.md`'s Topology metrics table (used-for, labels read, required?) and correct the `kube_pod_owner` row to drop `argocd_tracking_id`; verify the stated series count in the paragraph below the table is updated from 15 to 22.
- [x] 6.2 Update `docs/kube-state-metrics-preconditions.md`'s "Series → collector → RBAC" table and header count, and add `deployments`, `statefulsets`, `daemonsets`, `jobs`, `cronjobs` to the `collectors` list, the seven series to `metricAllowlist`, and the six annotation entries to `metricAnnotationsAllowList`; verify the collector count in the opening sentence matches the table.
- [x] 6.3 Replace the "Pod-level ArgoCD Application is not reachable by KSM config" section with the supported controller path — the owner-kind → series table, the Job→CronJob hop, and the explicitly unsupported kinds (`ReplicationController`, `Node`, CRD controllers); verify the section no longer claims the value is unreachable.
- [x] 6.4 Add the per-family cardinality guidance from design "Risks / Trade-offs" (an operator who runs no ArgoCD-managed bare ReplicaSets or Jobs may omit those two entries from the allowlist and degrade gracefully); verify the guidance names the per-resource allowlist as the lever.
- [x] 6.5 Extend the verification-PromQL snippets at the end of `docs/kube-state-metrics-preconditions.md` with a `count(kube_<controller>_annotations{annotation_argocd_argoproj_io_tracking_id!=""})` probe per family; verify each snippet names a series this change actually queries.

## 7. Verification gate

- [x] 7.1 Run `make lint` and `make vet`; verify both report clean.
- [x] 7.2 Run `make test`; verify the full unit / component / golden / property suite passes with `-race -shuffle=on`.
- [x] 7.3 Run the integration suite with Docker available (`go test ./internal/integration/ -run TestGraphSuite`); verify the migrated and new Application cases pass.
- [x] 7.4 Run `openspec validate "resolve-pod-application-from-controller"`; verify it reports the change valid with no outstanding artifact or task issues.
