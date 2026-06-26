## Why

The graph today exposes a single, shallow structural axis for pods — `cluster > node > pod` compound nesting (D31), with the K8s node as the pod's only grouping parent — and surfaces both StorageClass and ArgoCD Application as *metadata* (a serialiser-synthesised group inferred from PVC labels; a `data.application` string buried on each pod) rather than as navigable structure. Operators reason about workloads top-down — *which namespace, which ArgoCD app, which controller* — and about infrastructure relationally — *which node runs this pod, which StorageClass backs this PVC*. This change reshapes the graph to match that mental model:

- A **workload compound hierarchy** `cluster > namespace > application > controller > pod` so a viewer can collapse/expand by ArgoCD Application and controller, not just by node.
- **Pod → node** and **PVC → StorageClass** become explicit **edges** instead of compound nesting, freeing the single Cytoscape parent slot for the workload hierarchy while still showing the infra relationships (and giving K8s node nodes real edges again).
- A **StorageClass node** backed by the authoritative `kube_storageclass_info` series, carrying a typed `provisioner` attribute and a `parameters` object of the operator's NetApp/Ceph backing-storage values — see [[kube-storageclass-info-netapp-ceph-labels]].

All grouping levels reuse data already read (pod namespace/owner/application, PVC storageclass label, pod node label); only one new upstream query (`kube_storageclass_info`) is added.

## What Changes

> **Supersedes D31** (`cluster > node > pod` nesting; "no `pod-runs-on-node` edge; K8s node nodes carry no edges") and the StorageClass compound-grouping design (`cluster > storageclass > pvc`). Both are replaced below.

### Compound hierarchy (presentation-only)

`namespace`, `application`, and `controller` group nodes are **serialiser-synthesised DTOs** (the D31 precedent — not core `GraphNode`s), derived from attributes already on pods: the pod's namespace label, `Application()` (ArgoCD tracking-id, segment before first `:`), and `Owner()` (controller, ReplicaSet→Deployment-skipped per D34). The new `data.parent` nesting:

| element | `data.parent` |
|---|---|
| `cluster` group | — |
| `namespace` group | its `cluster` group |
| `application` group | its `namespace` group |
| `controller` group | its `application` group, else its `namespace` group |
| `pod` | its `controller` group, else `application`, else `namespace` (**skip absent levels**) |
| `service` | its `namespace` group |
| `pvc` | its `namespace` group |
| `node` (real node) | its `cluster` group |
| `storageclass` (real node) | its `cluster` group |
| `external` | — |

**Skip-absent-levels** is the confirmed fallback: a pod missing an Application nests `cluster > namespace > controller > pod`; missing a controller nests `cluster > namespace > application > pod`; missing both nests `cluster > namespace > pod`. Every pod always has at least `cluster > namespace > pod`. ID formats (design.md): `<cluster>/namespace/<ns>`, `<cluster>/<ns>/application/<app>`, `<cluster>/<ns>/controller/<kind>/<name>`.

### New edges (replace the old nestings)

Two new **core** edge types are added to the `graph.EdgeTypes` registry and `/v1/edge-types` (the user's revised decision — this change **does** add edge types):

- **`pod-to-node`** — directed pod → its K8s node, derived from the pod's `node` label (already read). Emitted for **every** pod with a node label. Intra-cluster only (`may_cross_cluster: false`). Re-adds edges to K8s `node` nodes (reverses the D31 consequence).
- **`pvc-to-storageclass`** — directed PVC → its StorageClass node, derived from the PVC's `storageclass` label. Intra-cluster only (`may_cross_cluster: false`). This is how the StorageClass relationship survives now that PVCs nest under `namespace` rather than under StorageClass.

The existing `pod-mounts-pvc` edge already chains pod → PVC, so pod → PVC → StorageClass and pod → node are now fully edge-expressed.

### StorageClass becomes a real node

`StorageClass` is promoted from a serialiser-synthesised group DTO to a **real, first-class `GraphNode`** (new `type="storageclass"` core node, `NodeTypeStorageClass`), so it can be the target of the `pvc-to-storageclass` edge. It is sourced from the new `kube_storageclass_info` topology query, is **cluster-scoped** (`id=<cluster>/storageclass/<name>`, names not globally unique), nests under its cluster group, and carries two typed attributes (the `ipaddress`/`owner` precedent — **not** in `labels`, which stays `{cluster}`):

- `data.provisioner` (string, `omitempty`) — the StorageClass provisioner, from the native KSM `provisioner` label on `kube_storageclass_info`.
- `data.parameters` (object `map[string]string`, `omitempty`) — the NetApp/Ceph backing-storage values, each key emitted only when its source label resolves non-empty (first non-empty source label wins):
    - `pool` ← `storagePools`, else `pool`
    - `fs` ← `fsType`, else `fsName`
    - `cluster_id` ← `ClusterID`
    - `selector` ← `selector`

Native KSM `reclaim_policy` / `volume_binding_mode` remain **out of scope** (provisioner is now in scope). Lexically-smallest-wins determinism on per-`(cluster, storageclass)` collision is preserved for the provisioner and every parameter key.

- *Design decision (design.md, resolved):* a PVC whose `storageclass` label names a class absent from `kube_storageclass_info` synthesises a **bare** StorageClass node (`labels={cluster}`, no `provisioner`/`parameters`) so the edge has a target, matching today's PVC-driven behaviour.

### Application attribute retained

The existing pod `data.application` string attribute is **retained** (additive, non-breaking) alongside the new `application` group node.

### Metric prefix

`kube_storageclass_info` joins the `KSG_METRIC_PREFIX` KSM-shaped family (13 → 14 metrics). All other reads already participate.

## Capabilities

### New Capabilities

None — all changes extend existing capabilities.

### Modified Capabilities

- `cluster-topology-source`: add the `kube_storageclass_info` topology read (14th query) and StorageClass node materialisation with its `provisioner` attribute and `parameters` object; add `kube_storageclass_info` to the `KSG_METRIC_PREFIX` family; specify derivation of the `pod-to-node` edge (from the pod `node` label) and `pvc-to-storageclass` edge (from the PVC `storageclass` label). Existing PVC-StorageClass and pod-Application resolution requirements are amended.
- `graph-api`: add the real `"storageclass"` node type and its typed `provisioner` + `parameters` attributes; add `pod-to-node` and `pvc-to-storageclass` to the edge-type registry / `/v1/edge-types`; replace the D31 compound-grouping requirement with the new `cluster > namespace > application > controller > pod` hierarchy (plus `cluster > namespace > {service,pvc}`, `cluster > {node,storageclass}`) and the skip-absent-levels rule; record that K8s `node` nodes now carry `pod-to-node` edges. The pod `data.application` attribute requirement is amended to "retained alongside the new `application` group node".
- `pod-service-graph`: **review only** — confirm the new `pod-to-node` / `pvc-to-storageclass` topology edges do not collide with service-graph edge semantics; likely a clarifying note, not a requirement change. (Drop from deltas if no requirement text changes.)

## Impact

- **Code:**
  - `pkg/promql/queries.go` — `QStorageClassInfo` constant + prefixed `Renderer` case.
  - `pkg/graph/node.go` — `NodeTypeStorageClass` real node type with its typed `provisioner` + `parameters` attributes (via a new `StorageClassInfo()` sealed-interface accessor); `pkg/graph/edge.go` — `EdgeTypePodToNode`, `EdgeTypePVCToStorageClass` registry entries.
  - `pkg/build/topology.go` — 14th `ReadTopology` query; `resolveStorageClassInfo` (`(cluster, storageclass)` → `provisioner` from the native label + `parameters` object `{pool,fs,cluster_id,selector}` with source-label fallbacks); emit StorageClass nodes; build `pod-to-node` and `pvc-to-storageclass` edges in the edge builder.
  - `pkg/cytoscape/cytoscape.go` — synthesise `namespace`/`application`/`controller` group DTOs from pod attributes; new `data.parent` nesting + skip-absent-levels; emit StorageClass as a real node (remove the old synthesised storageclass group and the old `cluster > node > pod` / `cluster > storageclass > pvc` parenting).
  - `pkg/graph/project.go` — projection/traversal now sees real `storageclass` nodes and `pod-to-node` / `pvc-to-storageclass` edges; the namespace-filter "retain node hosting in-scope pod" rule (k8sNodePassesFilters) is revisited since pod→node is now an edge, not nesting.
- **Specs/docs:** delta specs for `graph-api` + `cluster-topology-source`; regenerate OpenAPI (`make docs`); update `README.md` metric/edge tables and `CLAUDE.md` (supersede D31).
- **Tests:** new golden snapshots (deep hierarchy, two new edges, storageclass node + attrs), serialiser/edge unit tests, property-test invariants (no dangling `data.parent` across 5 group levels), integration fixtures ingesting `kube_storageclass_info` + node/owner/app labels.
- **Contract / compatibility:** `/v1/edge-types` gains two entries (additive); `type` enum gains real `"storageclass"` plus synthetic `"namespace"`/`"application"`/`"controller"`. **Behaviour change (not a wire-schema break):** pod `data.parent` moves from the node group to the workload hierarchy, and PVC `data.parent` from the storageclass group to the namespace group; the pod→node and PVC→storageclass relationships now appear as edges. The `{apiVersion, clusters, elements}` shape and determinism contract are unchanged.
- **Upstream:** `kube_storageclass_info` is OPTIONAL (absence degrades gracefully); its NetApp/Ceph parameter labels and `argocd_tracking_id` remain the operator's `--metric-labels-allowlist` responsibility.
- **No new dependency, no new HTTP route.**
