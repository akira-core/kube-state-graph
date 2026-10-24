---
paths:
  - "pkg/build/topologyplan*.go"
  - "pkg/build/build_storage*.go"
  - "pkg/build/claimseed*.go"
  - "pkg/build/*seed*.go"
  - "pkg/build/*scope*.go"
  - "pkg/build/expansion*.go"
  - "pkg/build/scopedread*.go"
  - "pkg/build/storageflow*.go"
  - "pkg/graph/storagescope*.go"
  - "pkg/graph/project_storage*.go"
  - "pkg/graph/project_k8s*.go"
  - "pkg/build/build_k8s*.go"
  - "pkg/build/k8s*.go"
  - "pkg/kubegraph/parse*.go"
  - "pkg/kubegraph/storage*.go"
---

# `GET /v1/storage-graph` and `GET /v1/k8s-graph`

Disclosed reference for `CLAUDE.md`. The text is the authoritative statement of each rule; `CLAUDE.md` carries only a one-line reminder.

## Build lifecycle

```
HTTP /v1/storage-graph?start=&end=&az=&env=&…
   │
   ▼
kubegraph.ParseStorageValues  ── StorageRequest{Start, End, Scope, Selector}
                                  az/env required, each repeatable — the selected zones are their Cartesian
                                  product (missing_az / missing_env);
                                  exactly one root kind (missing_root when none; invalid_scope when two or more)
   ▼
Builder.BuildStorage(…, roots) ── readTopology under storagePlan. The first wave is ALERTS
                                  alone. A per-kind seed walks the root to the claims it reaches
                                  (ontap_cluster / aggr / svm / ontap_node seed volume_labels, then
                                  pvCandidates → kube_persistentvolumeclaim_info{volumename}; node seeds
                                  kube_pod_info{node} plus incarnation completion; pod seeds bindings by
                                  namespace+pod; application seeds the three-stage controller recovery
                                  plus claim annotations by tracking-id, and — when the metric is
                                  configured and appmeta is served — a RenderAppMetadataByApp seed over
                                  the root Applications in that same wave (local vector; pod keys unioned
                                  with the controller-recovered pods before bindings; a candidate
                                  generator, not the D6 source); pvc / pv ARE the claim-info read
                                  itself — kube_persistentvolumeclaim_info{namespace,persistentvolumeclaim}
                                  per namespace, or {volumename} — with no volume_labels phase 1 in front
                                  of it). One expansion then walks that
                                  claim set both ways: claim families by (namespace, claim), one query per
                                  namespace (filtered to the tracked az/env/cluster/namespace/claim),
                                  mounter completion (bindings by (namespace, claim),
                                  then every mounter's pod), candidate completion (volume_labels by token)
                                  and owner completion (every touched aggregate re-read whole). Once pods
                                  and claim info are loaded, every by-reference root (not only
                                  application) issues the namespace-restricted read
                                  RenderAppMetadataByNamespace when the metric is configured and appmeta
                                  is served: one restriction per bucketed KSM cluster of those pods and
                                  claims — that cluster's matcher plus the sorted de-duplicated KSM
                                  namespaces it loaded, on the configured namespace label — chunked under
                                  the shared byte budget, in parallel with the controller read; the
                                  per-cluster vectors concatenated are the D6 source. Unset, unserved, or
                                  no loaded namespace issues no read. Kubernetes
                                  nodes and controllers are scoped from the loaded pods; Harvest gauges,
                                  controller families and fixed-policy ceilings are scoped to the reached
                                  components. A request-derived seed past the chunk cap is 400 invalid_scope
                                  before any query — the application-metadata seed's chunk count joins
                                  maxApplicationRootChunks, so a set the tracking-id cap accepts can still
                                  exceed the wider app-label cap, and an unserved appmeta family does not
                                  count those chunks. A data-derived scope, including the
                                  namespace-restricted app_metadata read, is chunked and never rejected
                                  and never read across the zone. Every issued query fails the build
                                  except ALERTS; the fail-closed list includes app_metadata (the
                                  namespace-restricted read and the application seed alike) — a chunk
                                  error is upstream query failed: app_metadata. skips ReadServiceGraph;
                                  assembleStorageFlow, attachAlerts,
                                  attachStatus; no up{} probe. Every Kubernetes query keeps the request's
                                  matchers and az routing, so it never reads another zone's store; Harvest
                                  queries are az-ROUTED only and carry no matcher (a catch-all harvest
                                  store returns every zone's rows — drop-harvest-zone-matchers D1)
   ▼
graph.ProjectStorage          ── reachability over storage-flow units + root-always;
                                  a pvc / pv root also keeps the SINK unit of a root claim no pod
                                  mounts (node-aggr → aggr-svm → svm-pvc, ending at the claim)
   ▼
cytoscape.Serialise
```

## Request surface and root kinds

- **Request surface is `start`, `end`, `cluster`, `namespace`, `az`, `env`, `prune`** on `/v1/graph` — everything but `start` / `end` optional. `GET /v1/storage-graph` additionally **requires** `az` and `env` (`missing_az` / `missing_env`), each **repeatable** — the selected zones are their Cartesian product, rendered exactly as on `/v1/graph` (sorted, de-duplicated, a single value `key="v"`, several one anchored `key=~"a|b"`), and the parser stores the sorted value sets — which narrow its Kubernetes side, and the `az` SET also selects the harvest backends — and **requires** exactly one root kind (`missing_root` when none; `invalid_scope` when two or more, naming the parameters): repeatable `ontap_cluster`, `ontap_node` (an ONTAP controller), `aggr`, `svm` (a bare aggregate or SVM name matches every filer of the selected zones; `<ontap_cluster>/<name>` is the **qualified form** naming exactly one filer's component — bare and qualified values mix and OR-combine, a bare value subsumes a qualified one of its name, and a value with a `/` must split into two non-empty segments else `invalid_scope`; `ontap_cluster=` does not qualify them — it is another root kind), `pod=<ns>/<name>`, `pvc=<ns>/<claim>` (a PersistentVolumeClaim, in every cluster of the estate), `pv=<name>` (a PersistentVolume by its bare name, matched on the `volumename` of the claim bound to it — a statically provisioned volume included, since the seed never derives candidates from a FlexVol name), `application=<argo-app>` (three-stage controller recovery plus claim annotations by tracking-id, and — when the metric is configured and `appmeta` is served — an application-metadata seed: `RenderAppMetadataByApp` over the root Applications, in parallel with stage 1 and the claim-annotation read, into a local vector whose pod keys are unioned with stage 3 before bindings; a seeded pod whose resolved Application is not a root value is dropped), or `node` (a Kubernetes node only — an ONTAP controller name sent as `node=` is an empty 200). **Every root kind, when the metric is configured and `appmeta` is served, also issues the namespace-restricted read** `RenderAppMetadataByNamespace` (`readScopedAppMetadata`) once per bucketed cluster of the loaded pods and claims after both are loaded, in parallel with the controller read — each restriction carries that cluster's matcher (`cluster=~"unknown|"` for the absent-or-unknown bucket) and only the namespaces loaded in that cluster, so another cluster's same-named namespace is never read; the value set is read from kube-state-metrics' `cluster` / `namespace` labels and the namespace matcher is the configured namespace label, and that vector — not the application seed — is the D6 source for pod Application, namespace owner and deployer. Unset or unserved issues neither read. A chunk error on either read fails closed as `upstream query failed: app_metadata`; `ALERTS` stays the only optional query. **A `pvc` / `pv` root is a claim seed** (`topologyPlan.claimSeeded()`, `pkg/build/claimseed.go`): the seed IS the claim-side `kube_persistentvolumeclaim_info` read, restricted per namespace (`promql.RenderNamesInNamespace`, so a same-named claim in another namespace is never read) or on `volumename`, and filtered again in the reader; `readTopology` then wires it as `readClaimSeed → pvcInfoDone`, `readHubClaimFamilies` (claim families + mounter completion) and `readWorkloadVolumeLabels` (candidate + owner completion), and the workload-claims step is skipped. A root set past `maxRootedVolumeLabelChunks` is 400 `invalid_scope` before any query, the count summed across namespaces. **An unmounted root claim is a Sankey sink**: a claim with no `pod-mounts-pvc` binding has no PVC node at all, so a claim-seeded parse alone (`topologyVectors.MaterialiseUnboundClaims`) builds it from its claim-info row, `assembleStorageFlow` alone emits the `node-aggr` / `aggr-svm` / `svm-pvc` chain of an unmounted claim (reading `Topology.ClaimSeeded`, which the parse copies from that same flag, so the two cannot disagree), and `ProjectStorage` retains that sink unit only through a claim root that names it — its whole measurement rides the chain with no `pvc-pod` edge, and an unmounted claim that is NOT a root stays dropped under every root kind. `prune` is ignored there. `name`, `root`, `depth`, `direction` and `edge_type` are **withdrawn** (BREAKING) and, like any unknown parameter, ignored without error — their VALUE is never inspected, so an unregistered `edge_type` is a 200. An old client receives the unanchored, unfiltered view. `GET /v1/clusters` is **removed** (BREAKING) together with the `cluster_discovery` query — the cluster list is the `clusters` field of any `/v1/graph` response. `graph.Scope` is `{Clusters, Namespaces, Inventory}`; `traverse` / `MaxTraversalDepth` / `Direction` / `Names` are gone. `graph.StorageScope` is `{Clusters, Namespaces, Roots}`. **A multi-zone body is the UNION of its zones** (accept-multi-zone-storage-graph): for a fixed root, filters and window, the body of a request selecting several `(az, env)` combinations equals — element for element, flow weights included — the union of the single-combination bodies. Nothing merges across zones because every Kubernetes id is composed `<az>-<env>-<cluster>`, every NetApp id is qualified by its ONTAP cluster, and the two row-to-object keys that used to carry a raw `cluster` now carry the identity: `podSeriesKey{az, env, cluster, namespace, pod}` (`podSeriesKeyOf`, the pod twin of `claimKey`, so a cluster name reused in two zones is two clusters and a same-named pod of each is elected independently by `podsNewestOn`) and the claim's zone preference on the Harvest side (`netapp-storage-join.md`: hop A candidates, hop B's one zone per workload). **Exception — Harvest rows in another selected zone's store.** Harvest carries no matcher, so a multi-zone request reads every selected zone's `harvest` backends at once. When a backend of ANOTHER selected zone holds a row the claim accepts — a colliding FlexVol carrying no pair, any row when the claim has no own-zone row, or the claim's QoS workload under another pair — the claim sees more candidates than in its single-zone body, and the hop A pick (`pickAggr`, lexically smallest) or hop B's smallest-pair choice can move, or a fallback join can appear (drop-harvest-zone-matchers, design Risks). The union stays exact whenever a claim's matching Harvest rows all live in its own zone's store, which is every estate without cross-zone FlexVol-token collisions or misplaced Harvest rows. **The operator guarantees ONTAP cluster and controller names are unique across the estate** (`docs/netapp-harvest-preconditions.md`); aggregate and SVM names are NOT unique, hence the qualified root. A qualified value is `graph.ONTAPRef{ONTAPCluster, Name}` in `graph.StorageRoots.Qualified` (sorted, de-duplicated, a bare name subsuming its qualified twin at construction; `Names` holds the bare values only, `Any()` counts both); the plan carries them as `topologyPlan.volumeAggrPairs` / `volumeSVMPairs` (`map[ONTAP cluster][]name`) beside the bare `volumeAggrs` / `volumeSVMs`, and `ProjectStorage` matches an aggr / svm root by `named || qualified[(labels.ontap_cluster, name)]`. A multi-zone build stays fail-closed across every backend it reaches.

## `GET /v1/k8s-graph` reads the Kubernetes half in its own rounds

`GET /v1/k8s-graph` is the Kubernetes half of the storage build for a `pod` or `node` root, drawn with `/v1/graph`'s edge types (`pod-to-node`, `pod-mounts-pvc`). It shares the storage build's *semantics* and has its own *orchestration*. `k8sPlan(roots)` is `storagePlan(roots)` with `skip = k8sSkippedLegs` (the storage skips plus every Harvest first-wave leg, derived from `topologyLegs` and `promql.FamilyOf`) and `kubernetesOnly = true`, so the seeds' first reads (`kube_pod_info{node}`; the claim bindings of the root pods), the chunk caps (`invalid_scope` before any query), the routing and the fail-closed rule are the storage build's own. What a pod, a claim, a controller or a node IS comes from the same helpers (`podsNewestOn`, `keepPodBindings`, `keepRootBindings`, `trackedClaimKeys`, `controllerScope` / `deploymentScope` / `cronJobScope`, `nodeScope`, `appMetadataScopesOf`, `issueScopedFamilies`, `issueAppMetadata`), so the two endpoints cannot disagree about it. Only the ORDER and GROUPING of the reads is the k8s build's: under `topologyPlan.readsK8sWorkload()` `readTopology` launches one reader, `readK8sWorkload` (`pkg/build/k8sworkload.go`), in place of `readNodeSeed` / `readPodSeed`, `readWorkloadClaims` and the four by-reference waves (`readScopedPods`, `readScopedNodes`, `readScopedControllers`, `readScopedAppMetadata`), and wraps it so `bindingsDone`, `pvcInfoDone` and `pvcAnnotationsDone` close on every return path. Drift is pinned by `TestBuildK8s_PlanIsOutputPreserving` (every k8s body equals the one an unrestricted read of the zone projects) and `TestBuildStorage_FanOutUnchanged` (the storage build's queries, Harvest included, do not move) — `pkg/build/testdata/storage_fanout.golden`, refreshed only for an intended storage change with `-update-storage-fanout`, a flag deliberately not named `-update`.

`kubernetesOnly` also keeps three things off: `readWorkloadVolumeLabels` and `readReachedHarvest` are not launched, and `volumeLabelsFinal` is closed up front, so `readScopedQoS` computes an empty scope and issues nothing. Those gates keep the fan-out from issuing a Harvest query; the guarantee itself is `refuseHarvest`, which wraps the bound querier of a `kubernetesOnly` build so any Harvest query fails the build before it reaches a store (keeping the wrapped querier's `promql.FamilyServer`, which the `appmeta` decision reads). A Harvest reader added without a gate therefore turns every k8s build red instead of quietly reading the harvest backends. `Builder.BuildK8s` accepts only `StorageRootPod` / `StorageRootNode` with at least one value, every pod ref carrying both a namespace and a name (else `ReasonInvalidScope` before a querier is bound), and is then `buildStorage` under `k8sPlan`: the plan's `run()` picks the span (`kube-state-graph.build_k8s`), the log line, the `root_kind` attribute and the assembler (`assemble(topology, ServiceGraphResult{})`), and it ends in `Builder.freeze` — the bake-before-freeze sequence (`attachAlerts`, `attachStatus`, `graph.NewGraph`, cluster identities, group maps) that `Build` and `buildStorage` share. No `ReadServiceGraph`, no `up{}` probe, no `outside_retention`, and none of the three last-build gauges (the adapters reset each vector on write).

**Rounds** (`readK8sWorkload`; every read of a round is issued together, a round starts when the reads its restriction depends on have returned; `lockstep_test.go` counts them under `testing/synctest`). Mounter completion never adds a claim (`readMountersOf` keeps only rows whose claim is already tracked) and a mounter shares its claim's cluster and namespace, so only the co-mounters' own reads wait for it.

```
node root                                                   pod root
r1  kube_pod_info{node}  [+ ALERTS]                         r1  bindings || kube_pod_info || kube_pod_owner of the roots
r2  incarnation || bindings || kube_pod_owner                   (the seed's chunk split, preparePodSeed's cap)
    — each on the seed's pairs                              r2  = node root r3
r3  mounter completion || claim families by (ns, claim)
    || app_metadata (root pods' + tracked claims' namespaces)
    || kube_node_* || controllers, first stage + early second stage
r4  only for mounters no read loaded: their pod_info || pod_owner
    || the second stage of the roots' owners, if the early read missed
r5  only for those mounters: their controllers + early second stage || kube_node_* not yet read
r6  only if their early read missed: the second stage
```

Node root 3 rounds (4 on an early miss), with a co-mounter 5 (6 when the co-mounter's own early read misses); pod root 2 (3). `TestBuildK8s_NodeRootCompletesInThreeRounds`, `TestBuildK8s_PodRootCompletesInTwoRounds`, `TestBuildK8s_ASharedClaimAddsTheCoMountersReads` and `TestBuildK8s_AnEarlyMissCostsAtMostOneRound` pin the numbers.

- **Grouped `(namespace, name)` restriction.** A read of known pods or claims is one query per chunk carrying `namespace=~"a|b"` beside the name alternation (`promql.RenderNamePairs` / `RenderNamesAcrossNamespaces`, the generalisation of `RenderClaimBindingsByPod`), not one query per namespace. The alternations are independent, so a chunk can match a cross pair; `promql.KeepNamePairs` (a pod root's roots), the full `podSeriesKey` (a node root's pods) or the `claimKey` (claims) filter drops it before any later read's scope is computed from it, and `tallyDropped` still counts it. `promql.ChunkNamePairs` sorts by `(namespace, name)` and packs under the byte budget less `RequestMatcherCost`, each chunk charged for its own two alternations. `/v1/storage-graph` keeps its per-namespace reads (`RenderNamesInNamespace`).
- **A node root also loads the root node's claimless pods.** The storage node seed keeps a pod only when it mounts a claim (`keepPodBindings`), so a storage build never loads `web-0` running on the root node while mounting nothing — correct for a storage body, wrong for "what runs on this node". The k8s reader reads incarnation completion, the bindings and `kube_pod_owner` on the seed's pairs together and keeps the rows of the pods whose newest incarnation still runs on a root node (`podsNewestOn`); a pod that moved is dropped from all three, so its claims are not tracked (`TestBuildK8s_ARescheduledPodFollowsItsNewestIncarnation`). A node root identifies a pod by the full `podSeriesKey`, so a same-named pod of another selected cluster or zone, which the grouped read still returns, never scopes the node, controller or application-metadata read (`TestBuildK8s_NodeRootPodWaveIsClusterKeyed`). A pod root is cluster-agnostic by design (`pod=<ns>/<name>` names that pod in every selected cluster), so it identifies a pod by `(namespace, pod)` alone (`podSet`).
- **The application-metadata scope comes from the root pods and the tracked claims** (`appMetadataScopesOf`), not from the loaded pod and claim-info rows. For a node root the two sets are equal; for a pod root the new one is larger only when a tracked claim's cluster and namespace hold no root pod row, no co-mounter row and no claim-info row — where nothing is drawn and no mounter resolves an Application.
- **Co-mounters are a delta, and a name is read once per family.** After mounter completion the reader reads `kube_pod_info` / `kube_pod_owner` only for binding pods no earlier read loaded, then the controller and node families only for names no earlier read of the same family covered (`controllerReads.unrequested`, `k8sRead.nodesRead`), appending to the slots in round order. The parse is keyed, so the order of rows of different keys cannot change the body (`TestParseTopology_IsOrderInsensitiveAcrossKeys`).
- **The second controller stage is read ahead.** `kube_deployment_annotations` / `kube_cronjob_annotations` are issued with the first stage on the direct owner names plus each ReplicaSet / Job name with its final `-` segment removed (the Deployment controller names a ReplicaSet `<deployment>-<pod-template-hash>`, the CronJob controller a Job `<cronjob>-<scheduled-time>`); `secondStage` then reads whatever `deploymentScope` / `cronJobScope` resolve that the guess did not cover. A wrong guess leaves the body unchanged because the annotation indexes are read only through a loaded pod's owner chain (`TestBuildK8s_AWrongEarlyNameLeavesTheBodyUnchanged`). No query joins two families: VictoriaMetrics pushes a join's labels down only up to 100 values per label and Prometheus not at all, so a join is neither portable nor bounded.

`graph.ProjectK8s(g, scope)` is reachability over the built graph's `pod-to-node` / `pod-mounts-pvc` edges, a pure function of `(g, scope)`: a pod is kept iff it is a `pod` root or its `pod-to-node` edge targets a resolved `node` root, and it passes the `cluster` / `namespace` filters; a claim iff a kept pod mounts it; a node iff a kept pod runs on it or it is a root. A root the graph holds is always drawn; an unmounted claim never is; the other mounters of a shared claim are in `g` (they drive the claim's inherited Application) but are not kept. Roots are resolved by node id and edge, never by `labels.node`, so a node name reused by two cluster identities stays two nodes. `kubegraph.ParseK8sValues` requires `cluster`, `az` and `env` (the selected identities are their Cartesian product; `cluster` is the RAW name) and shares `oneStorageRoot` with `ParseStorageValues`, restricted to `node` / `pod`: a storage-graph-only kind alone is `missing_root`.

