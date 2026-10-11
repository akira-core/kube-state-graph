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
  - "pkg/kubegraph/parse*.go"
  - "pkg/kubegraph/storage*.go"
---

# `GET /v1/storage-graph`

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
                                  plus claim annotations by tracking-id; pvc / pv ARE the claim-info read
                                  itself — kube_persistentvolumeclaim_info{namespace,persistentvolumeclaim}
                                  per namespace, or {volumename} — with no volume_labels phase 1 in front
                                  of it). One expansion then walks that
                                  claim set both ways: claim families by (namespace, claim), one query per
                                  namespace (filtered to the tracked az/env/cluster/namespace/claim),
                                  mounter completion (bindings by (namespace, claim),
                                  then every mounter's pod), candidate completion (volume_labels by token)
                                  and owner completion (every touched aggregate re-read whole). Kubernetes
                                  nodes and controllers are scoped from the loaded pods; Harvest gauges,
                                  controller families and fixed-policy ceilings are scoped to the reached
                                  components. A request-derived seed past the chunk cap is 400 invalid_scope
                                  before any query; a data-derived scope is chunked and never read across
                                  the zone. skips ReadServiceGraph; assembleStorageFlow, attachAlerts,
                                  attachStatus; no up{} probe. Every query keeps the request's matchers
                                  and az routing, so a build never reads another zone's store
   ▼
graph.ProjectStorage          ── reachability over storage-flow units + root-always;
                                  a pvc / pv root also keeps the SINK unit of a root claim no pod
                                  mounts (node-aggr → aggr-svm → svm-pvc, ending at the claim)
   ▼
cytoscape.Serialise
```

## Request surface and root kinds

- **Request surface is `start`, `end`, `cluster`, `namespace`, `az`, `env`, `prune`** on `/v1/graph` — everything but `start` / `end` optional. `GET /v1/storage-graph` additionally **requires** `az` and `env` (`missing_az` / `missing_env`), each **repeatable** — the selected zones are their Cartesian product, rendered exactly as on `/v1/graph` (sorted, de-duplicated, a single value `key="v"`, several one anchored `key=~"a|b"`), and the parser stores the sorted value sets — which narrow its Kubernetes side, and the `az` SET also selects the harvest backends — and **requires** exactly one root kind (`missing_root` when none; `invalid_scope` when two or more, naming the parameters): repeatable `ontap_cluster`, `ontap_node` (an ONTAP controller), `aggr`, `svm` (a bare aggregate or SVM name matches every filer of the selected zones; `<ontap_cluster>/<name>` is the **qualified form** naming exactly one filer's component — bare and qualified values mix and OR-combine, a bare value subsumes a qualified one of its name, and a value with a `/` must split into two non-empty segments else `invalid_scope`; `ontap_cluster=` does not qualify them — it is another root kind), `pod=<ns>/<name>`, `pvc=<ns>/<claim>` (a PersistentVolumeClaim, in every cluster of the estate), `pv=<name>` (a PersistentVolume by its bare name, matched on the `volumename` of the claim bound to it — a statically provisioned volume included, since the seed never derives candidates from a FlexVol name), `application=<argo-app>`, or `node` (a Kubernetes node only — an ONTAP controller name sent as `node=` is an empty 200). **A `pvc` / `pv` root is a claim seed** (`topologyPlan.claimSeeded()`, `pkg/build/claimseed.go`): the seed IS the claim-side `kube_persistentvolumeclaim_info` read, restricted per namespace (`promql.RenderNamesInNamespace`, so a same-named claim in another namespace is never read) or on `volumename`, and filtered again in the reader; `readTopology` then wires it as `readClaimSeed → pvcInfoDone`, `readHubClaimFamilies` (claim families + mounter completion) and `readWorkloadVolumeLabels` (candidate + owner completion), and the workload-claims step is skipped. A root set past `maxRootedVolumeLabelChunks` is 400 `invalid_scope` before any query, the count summed across namespaces. **An unmounted root claim is a Sankey sink**: a claim with no `pod-mounts-pvc` binding has no PVC node at all, so a claim-seeded parse alone (`topologyVectors.MaterialiseUnboundClaims`) builds it from its claim-info row, `assembleStorageFlow` alone emits the `node-aggr` / `aggr-svm` / `svm-pvc` chain of an unmounted claim (reading `Topology.ClaimSeeded`, which the parse copies from that same flag, so the two cannot disagree), and `ProjectStorage` retains that sink unit only through a claim root that names it — its whole measurement rides the chain with no `pvc-pod` edge, and an unmounted claim that is NOT a root stays dropped under every root kind. `prune` is ignored there. `name`, `root`, `depth`, `direction` and `edge_type` are **withdrawn** (BREAKING) and, like any unknown parameter, ignored without error — their VALUE is never inspected, so an unregistered `edge_type` is a 200. An old client receives the unanchored, unfiltered view. `GET /v1/clusters` is **removed** (BREAKING) together with the `cluster_discovery` query — the cluster list is the `clusters` field of any `/v1/graph` response. `graph.Scope` is `{Clusters, Namespaces, Inventory}`; `traverse` / `MaxTraversalDepth` / `Direction` / `Names` are gone. `graph.StorageScope` is `{Clusters, Namespaces, Roots}`. **A multi-zone body is the UNION of its zones** (accept-multi-zone-storage-graph): for a fixed root, filters and window, the body of a request selecting several `(az, env)` combinations equals — element for element, flow weights included — the union of the single-combination bodies. Nothing merges across zones because every Kubernetes id is composed `<az>-<env>-<cluster>`, every NetApp id is qualified by its ONTAP cluster, and the two row-to-object keys that used to carry a raw `cluster` now carry the identity: `podSeriesKey{az, env, cluster, namespace, pod}` (`podSeriesKeyOf`, the pod twin of `claimKey`, so a cluster name reused in two zones is two clusters and a same-named pod of each is elected independently by `podsNewestOn`) and the claim → FlexVol join's zone agreement (`netapp-storage-join.md`, hop A). **The operator guarantees ONTAP cluster and controller names are unique across the estate** (`docs/netapp-harvest-preconditions.md`); aggregate and SVM names are NOT unique, hence the qualified root. A qualified value is `graph.ONTAPRef{ONTAPCluster, Name}` in `graph.StorageRoots.Qualified` (sorted, de-duplicated, a bare name subsuming its qualified twin at construction; `Names` holds the bare values only, `Any()` counts both); the plan carries them as `topologyPlan.volumeAggrPairs` / `volumeSVMPairs` (`map[ONTAP cluster][]name`) beside the bare `volumeAggrs` / `volumeSVMs`, and `ProjectStorage` matches an aggr / svm root by `named || qualified[(labels.ontap_cluster, name)]`. A multi-zone build stays fail-closed across every backend it reaches.
