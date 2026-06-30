## Context

Today the graph exposes one shallow pod-grouping axis — `cluster > node > pod` (D31) — synthesised entirely in the Cytoscape serialiser from each pod's `labels.node`, with **no** `pod-runs-on-node` edge and K8s `node` nodes carrying **no** edges. StorageClass is likewise presentation-only (the 2026-06-08 change, D4): a serialiser-synthesised `type="storageclass"` group whose membership is *inferred* from each PVC's `storageclass` label, `cluster > storageclass > pvc`. ArgoCD Application is only a `data.application` string attribute on pods (the 2026-06-13 change, D-A1/D-A6).

This change (motivation in `proposal.md`) reshapes the graph to a workload-oriented compound hierarchy and moves the two infrastructure relationships to explicit edges:

```
cluster > namespace > application > controller > pod      (skip absent levels)
cluster > namespace > { service, pvc }
cluster > { node, storageclass }                          (real nodes, edge-linked)
pod --pod-to-node--> node
pvc --pvc-to-storageclass--> storageclass
```

The relevant load-bearing facts from the current code:

- `graph.Edge{ID,Type,Source,Target,Labels}`; `NewEdge` derives a deterministic **UUIDv5** from `edgeNamespace` over the canonical `"<type>|<source>|<target>"`; `SortEdges` orders by ID (`pkg/graph/edge.go`).
- `EdgeTypeDefinition{Type, Description, SourceType []NodeType, TargetType []NodeType, Directed bool, MayCrossCluster bool, Labels []EdgeTypeLabel}`; `var EdgeTypes` (`pkg/graph/registry.go`) is served verbatim by `/v1/edge-types`.
- Topology edges are built in `pkg/build/topology_edges.go` (`TopologyEdges(t) []*graph.Edge`), appended in `build.go` `assemble()` before service-graph edges; `graph.NewGraph(nodes, edges, builtAt)` builds Forward/Reverse adjacency and **dedupes nodes keep-first**.
- ID grammars: `PodID=<cluster>/<uid>`, `K8sNodeID=<cluster>/<name>`, `PVCID=<cluster>/<ns>/<claim>`, `ServiceID=<cluster>/<ns>/<svc>`.
- `pod.Labels()["node"]` is **already** the full `K8sNodeID` (`<cluster>/<nodeName>`), set only when scheduled. `pod.Labels()` also carries `cluster`, `namespace`. `pvc.Labels()` carries `cluster`, `namespace`; its class is via `pvc.StorageClass()`. `K8sNode.Labels()` already carries its KSM-derived labels plus `cluster`.
- The Cytoscape serialiser (`Serialise(g, view)`) emits synthetic groups in a fixed order (cluster → storageclass → real nodes), guards `pod→node` parents against a `present` map of real-node IDs, and computes parents in `compoundParent(n, present)` by switching on `n.Type()`.
- Projection (`pkg/graph/project.go`): `traverse` (edge-type-aware BFS, no orphans) → `filterNodes` (cluster/namespace/name; defers K8s-node admission, builds `hostNodes` from in-scope pods' `labels.node`, retains a node iff it hosts an in-scope pod via `k8sNodePassesFilters`) → `filterEdges` (drops edges with filtered endpoints, edge-type filter, re-adds a single missing endpoint via the namespace-only `nodePassesNonClusterFilters`).

## Goals / Non-Goals

**Goals:**
- A `cluster > namespace > application > controller > pod` compound hierarchy, with **skip-absent-levels** fallback, derived purely from existing pod attributes (`labels.namespace`, `Application()`, `Owner()`).
- `service` and `pvc` nested under `namespace`; `node` and `storageclass` nested under `cluster`.
- StorageClass promoted to a real, first-class `GraphNode` sourced from `kube_storageclass_info`, carrying a typed `provisioner` attribute and a `parameters` object of the operator's NetApp/Ceph backing-storage values (`pool`/`fs`/`cluster_id`/`selector`).
- Two new core edge types — `pod-to-node` and `pvc-to-storageclass` — registered and routable through projection/traversal.
- Byte-deterministic output (D6) preserved; sealed `GraphNode` interface preserved (no new method); no new upstream dependency beyond the one `kube_storageclass_info` query.

**Non-Goals:**
- No native KSM `reclaim_policy` / `volume_binding_mode` attributes (explicitly excluded by the operator). `provisioner` IS surfaced (as a typed attribute).
- No new HTTP route; no change to the `{apiVersion, clusters, elements}` body shape or determinism contract.
- No service-graph (`pod-calls-*`, `service-selects-pod`) behaviour change.
- `application` / `namespace` / `controller` are **not** promoted to core `GraphNode`s or given edges — they remain presentation-only synthesised groups (only StorageClass becomes a real node, because it must anchor an edge).
- No retroactive `pod-runs-on-node` history; this is forward-only.

## Decisions

> **This change supersedes D31** (`cluster > node > pod` nesting; "no `pod-runs-on-node` edge type … K8s `node` nodes carry no edges") and the 2026-06-08 StorageClass-grouping decisions (D1–D6 of that change: PVC-inferred synthesised `storageclass` group, `cluster > storageclass > pvc`). Fresh numbering (colon style) per house convention.

### D1: StorageClass is a real `GraphNode`, sourced from `kube_storageclass_info`

A new sealed type `StorageClassNode` (`NodeTypeStorageClass = "storageclass"`) implementing `graph.GraphNode`. `ID()=StorageClassID(cluster, name)=<cluster>/storageclass/<name>` (cluster-scoped — StorageClass names are not globally unique); `Name()=<name>`; `Type()="storageclass"`; `IPAddress()=nil`, `Owner()=nil`, `Application()=""`, `Containers()=nil`, `ReadyStatus()=""`, `StorageClass()=""` (the accessor means "which class does this node *use*", n/a here); it implements the new `StorageClassInfo() *StorageClassInfo` accessor (D2) returning its own provisioner + parameters (nil when bare). Every other node type returns nil from `StorageClassInfo()`.

**Why:** the `pvc-to-storageclass` edge (D3) needs a real node to target — the core graph model only edges between real `GraphNode`s. Sourcing from `kube_storageclass_info` (rather than inferring from PVC labels) gives an authoritative identity and a home for the backing-storage attributes that PVC labels do not carry.

**Alternatives considered:**
- **Keep StorageClass a synthesised presentation group, PVC nests under it** (status quo, D4 2026-06-08) — *rejected:* the user requires PVCs under `namespace` and the StorageClass relationship as an edge; a group node cannot be an edge target, and a pod/pvc has only one compound parent.
- **StorageClass attributes as a `data.storageclass_info` typed attribute on PVCs** — *rejected:* duplicates the data on every PVC, and still gives no node to navigate to or edge to draw.

### D2: `provisioner` and a `parameters` object are typed attributes via a new `StorageClassInfo()` sealed method

`StorageClassNode.Labels()` carries `cluster` only. The StorageClass's provisioner and backing-storage parameters are surfaced as **typed attributes** (the `IPAddress()`/`Owner()` precedent), via a single new sealed-interface accessor:

```go
type StorageClassInfo struct { Provisioner string; Parameters map[string]string }
// on the sealed GraphNode interface:
StorageClassInfo() *StorageClassInfo   // nil for every non-storageclass node and for bare storageclass nodes
```

The serialiser emits `data.provisioner` (string, `omitempty`) from `.Provisioner` and `data.parameters` (object, `omitempty`) from `.Parameters`. Resolution in `resolveStorageClassInfo` reads `kube_storageclass_info`:

- `provisioner` ← the native KSM `provisioner` label.
- `parameters` is a `map[string]string` of the operator-allowlisted backing-storage labels, **first non-empty source label wins**, each key omitted when its source resolves empty:

| `parameters` key | source label (first non-empty) |
|---|---|
| `pool` | `storagePools`, else `pool` |
| `fs` | `fsType`, else `fsName` |
| `cluster_id` | `ClusterID` |
| `selector` | `selector` |

Per-`(cluster, storageclass)` collision (multiple series) resolves **lexically-smallest** for the provisioner and per parameter key, for determinism.

**Why:** the operator wants `provisioner` as a first-class field and the rest grouped in a named `parameters` object — a structured shape, not flat metadata. Typed attributes keep `labels` strictly typological (`{cluster}`) and mirror the `owner`/`ipaddress` precedent (derived values never in `labels`). One struct accessor (like `Owner() *Owner`) covers both fields, so the sealed interface grows by exactly **one** method returning nil for the other node kinds — no per-attribute method sprawl and no serialiser type-switch (the DTO reads `n.StorageClassInfo()`).

**Alternatives considered:**
- **Flat `pool`/`fs`/`cluster_id`/`selector` in `Labels()`** (no new method) — *rejected:* the operator wants `provisioner` distinct from a grouped `parameters` object, which `labels` (a flat strict map) cannot express, and it would pollute typological `labels` with derived values.

### D3: Two new core edge types — `pod-to-node` and `pvc-to-storageclass`

Add to `pkg/graph/edge.go` and the `EdgeTypes` registry:

```go
{Type: EdgeTypePodToNode, Description: "Pod is scheduled on a Kubernetes node.",
 SourceType: []NodeType{NodeTypePod}, TargetType: []NodeType{NodeTypeK8sNode},
 Directed: true, MayCrossCluster: false, Labels: nil},
{Type: EdgeTypePVCToStorageClass, Description: "PVC is provisioned by a StorageClass.",
 SourceType: []NodeType{NodeTypePVC}, TargetType: []NodeType{NodeTypeStorageClass},
 Directed: true, MayCrossCluster: false, Labels: nil},
```

Built in `TopologyEdges` (mirroring the `pod-mounts-pvc` loop):
- **pod-to-node:** for every pod with a non-empty `labels["node"]` → `NewEdge(EdgeTypePodToNode, pod.ID(), pod.Labels()["node"], nil)`. The target is already a valid `K8sNodeID`. Always intra-cluster (the node label is the pod's own cluster). Dedup by `(podID, nodeID)`.
- **pvc-to-storageclass:** for every PVC with `StorageClass() != ""` → `NewEdge(EdgeTypePVCToStorageClass, pvc.ID(), StorageClassID(pvcCluster, pvc.StorageClass()), nil)`. Intra-cluster. Dedup by `(pvcID, scID)`.

**Why:** edges free the single Cytoscape parent slot for the workload hierarchy while still expressing the infra relationships, and they give `node` nodes real edges again (so `?edge_type=pod-to-node` and traversal can reach them). Reusing `NewEdge`/`SortEdges` keeps the UUIDv5 + determinism contract intact for free.

**Alternatives considered:**
- **Carry pod→node and pvc→storageclass only as compound nesting** — *rejected:* the single-parent rule forbids a pod nesting under both its node and its controller; the user chose the workload hierarchy for nesting and edges for infra.

### D4: Workload hierarchy via path-encoded synthetic group IDs; per-pod independent parent chain (supersedes D31)

`namespace`, `application`, `controller` are presentation-only synthesised group nodes (the D31 precedent — never core `GraphNode`s), each with `labels={}`, emitted by the serialiser. **Each pod computes its own parent chain from its own `(namespace, Application(), Owner())` triple**; the set of group nodes is the set of distinct ancestry-path IDs across all in-scope pods. Path-encoded IDs make the tree unambiguous by construction (a node's ID encodes its full ancestry, so it has exactly one parent):

| group | ID grammar | parent |
|---|---|---|
| namespace | `<cluster>/namespace/<ns>` | cluster group |
| application | `<cluster>/namespace/<ns>/application/<app>` | namespace group |
| controller (app present) | `<cluster>/namespace/<ns>/application/<app>/controller/<kind>/<name>` | application group |
| controller (no app) | `<cluster>/namespace/<ns>/controller/<kind>/<name>` | namespace group |

**Pod `data.parent` = its controller group, else its application group, else its namespace group (skip-absent-levels).** `service` and `pvc` parent to their `<cluster>/namespace/<ns>` group; `node` and `storageclass` parent to their cluster group; `external` keeps no parent.

**Why:** per-pod independent derivation avoids any cross-pod agreement problem — a controller whose pods span two ArgoCD apps simply yields two app-scoped controller groups, each a clean subtree, rather than an ambiguous two-parent node. Path-encoding upholds the no-dangling-parent invariant without a `present`-map check (every referenced group ID was generated by the pod that references it). `Owner()` already collapses ReplicaSet→Deployment (D34), so `controller/<kind>/<name>` is the meaningful workload, not the bare ReplicaSet.

**Alternatives considered:**
- **Flat controller IDs `<cluster>/<ns>/controller/<kind>/<name>` with the app parent picked from pods** — *rejected:* needs a deterministic tie-break when a controller's pods disagree on app, and risks a dangling/!single parent; path-encoding sidesteps it.
- **Placeholder `no-application` / `no-controller` groups for uniform depth** (an option the user declined) — *rejected:* the user chose skip-absent-levels.

### D5: `labels.node` is retained as the pod contract field; pods no longer nest under nodes

`labels.node` (= `K8sNodeID`) stays on pods. It now drives (a) the `pod-to-node` edge (D3) and (b) the namespace-filter node-retention rule (D6) — but **not** nesting (pods nest under the workload hierarchy, D4).

**Why:** removing `labels.node` would break the namespace-retention rule and the edge source-of-truth; it is a stable contract field. Only its *presentation role* (compound parent) is dropped.

### D6: Cluster-scoped infra nodes (`node`, `storageclass`) are namespace-filter-retained iff referenced by an in-scope pod/pvc

Generalise the existing `k8sNodePassesFilters` deferred-admission rule. Under a `?namespace=` filter, a `node` or `storageclass` node (neither carries a `namespace` label) is retained **iff** some in-scope pod is scheduled on it (`hostNodes`, existing) / some in-scope PVC references it (new `referencedStorageClasses` set, built from surviving PVCs' `StorageClass()`). Without a namespace filter, behaviour is unchanged.

**Why:** these nodes have no namespace of their own; the current code already special-cases K8s nodes this way so that `cluster > node > pod` survives a namespace filter. The same reasoning extends to StorageClass so `pvc → storageclass` survives. Keeping it in `filterNodes` (not the serialiser) preserves "node admission is the one place namespace-awareness lives".

**Alternatives considered:**
- **Rely on `filterEdges` endpoint re-add** — *rejected:* re-add uses the namespace-only `nodePassesNonClusterFilters`, which a namespace-less infra node fails, so the edge (and node) would be dropped under namespace filters. Explicit retention is needed.

### D7: PVCs referencing a StorageClass absent from `kube_storageclass_info` synthesise a bare node

If a PVC's `StorageClass()` names a class with no `kube_storageclass_info` series, materialise a **bare** `StorageClassNode` (`labels={cluster}`, no attributes) so the `pvc-to-storageclass` edge has a target. Real (attributed) nodes are appended **before** bare ones; `NewGraph`'s keep-first node dedup makes the attributed node win on `(cluster, name)` collision.

**Why:** resilience and continuity — today's grouping is PVC-label-driven, so a class present on PVCs but absent from the (OPTIONAL) info metric must still appear. Dropping the edge would silently lose the relationship.

**Alternatives considered:** **drop the edge when the class is unknown** — *rejected:* loses operator-visible signal and regresses today's behaviour.

### D8: `kube_storageclass_info` is the 14th topology query and joins the `KSG_METRIC_PREFIX` family

Add `QStorageClassInfo = "kube_storageclass_info"` (bare constant) + a prefixed `Renderer` case `last_over_time(<prefix>kube_storageclass_info[<window>]) @ end`, a 14th `g.Go` in `ReadTopology`. OPTIONAL: absence yields no StorageClass nodes (PVCs fall to D7 bare nodes) and no build failure.

**Why:** consistent with every other KSM-shaped read; the prefix list grows 13→14. The bare `Q*` constant keeps `query_name=` self-metric/span dimensions stable across prefixes.

### D9: Pod `data.application` string attribute is retained alongside the `application` group

`PodNode.Application()` and its `data.application` serialisation are unchanged (additive change).

**Why:** non-breaking for existing consumers that read the attribute; the group node is a parallel, navigational representation, not a replacement.

### D10: Determinism and sealed-interface preservation

- Synthetic group nodes are emitted in a fixed tier order (cluster, namespace, application, controller), each tier sorted by ID; then real nodes via `SortNodes`; edges via `SortEdges`. StorageClass nodes (real) sort among real nodes by ID.
- The serialiser gains **no** node-attribute type-switch: group synthesis and attribute emission read only sealed-interface methods (`Labels()`, `Application()`, `Owner()`, `StorageClassInfo()`); the existing `compoundParent` `Type()` switch is extended, not replaced. The old `storageClassParentID`/`scSeen` synthesis is removed (StorageClass is now a real node).
- Exactly **one** new sealed-interface method (`StorageClassInfo()`, D2), returning nil for the other node kinds (the `Owner()` precedent). Two new `omitempty` DTO fields (`provisioner`, `parameters`). No timestamps/random IDs added.

**Why:** byte-identical output for identical `(window, filters, upstream-data)` is the D6 contract that golden/property tests enforce.

### D11: Service & PVC ArgoCD Application from `*_annotations` tracking-id metrics (extension, 2026-06-26)

Extend the workload hierarchy so `service` and `pvc` nodes also nest under the `application` group. Source the Application from two new OPTIONAL topology reads:

- `kube_service_annotations` joined `(cluster, namespace, service)` → service entity.
- `kube_persistentvolumeclaim_annotations` joined `(cluster, namespace, persistentvolumeclaim)` → PVC entity (the PVC entity's claim name derives from `kube_pod_spec_volumes_persistentvolumeclaims_info`'s `claim_name`, same key as PVC StorageClass resolution).

Both carry `annotation_argocd_argoproj_io_tracking_id` — kube-state-metrics' sanitised form of the `argocd.argoproj.io/tracking-id` annotation (`.`/`/` → `_`), gated on the operator's `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id],persistentvolumeclaims=[argocd.argoproj.io/tracking-id]`. The value is the Argo tracking-id `<app>:<group>/<kind>:<ns>/<name>`, so the Application is the **segment before the first `:`** — the **identical parse** to `resolvePodApplications` (no `:` → verbatim; empty leading segment → no Application). Per-entity collision picks the **lexically-smallest raw tracking-id** (mirrors the pod resolver; one map suffices). Resolution lives in `pkg/build/topology.go` (e.g. `resolveServiceApplications` / `resolvePVCApplications`, factored to share the pod's segment-before-`:` derivation), keyed via `bucketCluster` for missing-cluster. Two new `Q*` constants + prefixed `Renderer` cases (`last_over_time(<prefix>kube_*_annotations[<w>])`) join the `KSG_METRIC_PREFIX` family (14 → 16). PVC application resolves at topology assembly (PVC nodes are topology-built); **service** application is threaded into the connection-string resolver, since service nodes are materialised there (graph-api "Service node payload"), via a `(cluster, namespace, service) → app` index built in `ReadTopology`.

**Why:** the operator reasons about storage and service objects under the same ArgoCD-app axis as pods. ArgoCD's default tracking method is `annotation+label`; the annotation form is the canonical, collision-free tracking-id (the `app.kubernetes.io/instance` label is truncated to 63 chars and ambiguous), and it reuses the pod value grammar verbatim.

**Alternatives considered:**
- **`app.kubernetes.io/instance` label via `kube_*_labels`** — *rejected (user chose annotations):* the instance label is truncated/ambiguous and is a different value grammar; the annotation tracking-id reuses the pod parse exactly.
- **A new dedicated `application` index keyed independently of the existing entities** — *rejected:* the entities already carry `(cluster, namespace, name)`; a join on that key is the minimal addition.

### D12: `Application()` widens to pod/service/pvc; `data.application` surfaces on all three (supersedes D9 scope)

The sealed `graph.GraphNode.Application() string` accessor — previously meaningful only on `PodNode` — now also returns a resolved value on `ServiceNode` and `PVCNode` (still `""` for `K8sNode`, `ExternalNode`, `StorageClassNode`). **No new interface method.** Because the serialiser already emits `Application: n.Application()` for every node under `omitempty` (`pkg/cytoscape/cytoscape.go`), `data.application` surfaces on service and PVC nodes automatically once their `Application()` is non-empty — and it drives the `application` compound-group nesting for those nodes. The cytoscape `appSeen` collection and `compoundParent` are extended to derive the application group from any pod/service/pvc with a resolved Application (the `application` group id `applicationParentID(cluster, ns, app)` is already namespace-scoped, so it composes for service/pvc verbatim); `controller` groups stay pod-only.

**Why:** widening the existing accessor + the already-`omitempty` DTO field is the smallest coherent change — no gating logic, no new method, no new node attribute plumbing. Surfacing `data.application` on service/pvc is strictly additive (omitempty) and makes the *why* of a service's application nesting legible at the node, mirroring the pod precedent (D9, which surfaced both attribute and group).

**Alternatives considered:**
- **Parent-only (service/pvc use `Application()` for the compound parent but suppress `data.application`)** — *rejected:* requires gating the serialiser's already-uniform `Application` emission to pods only, more code for a strictly-less-useful result; the path-encoded group id already exposes the app name, so the attribute is a free, consistent superset.

**Projection:** none required — `application`/`namespace` groups are serialiser-only DTOs (not core nodes, not in projection/traversal); service & PVC already carry `namespace`, so the `?namespace=` filter is unaffected, and `?name=` matches real nodes only.

**Determinism:** the application group set is the union of app keys across pods/services/pvcs, still emitted in the fixed tier order sorted by id (D10) — byte-stable.

### D13: PVC inherits its mounting pod's ArgoCD Application when it has none of its own (extension, 2026-06-29)

When a PVC's own annotation path (D11) resolves **no** Application, the build derives one from the pods that mount it. The PVC entity, the pod entities, and the pod application index (`resolvePodApplications`) are all available at topology assembly; the pod↔PVC mount relationship comes from the **same** `kube_pod_spec_volumes_persistentvolumeclaims_info` vector that already produces the PVC entities and the `pod-mounts-pvc` edges, keyed `(cluster, namespace, pod)`. A single assembly-time pass:

1. For each PVC with an empty resolved Application, gather every mounting pod (the mount vector rows for the PVC's `(cluster, namespace, claim)`).
2. Look each mounting pod up in the pod application index (`(cluster, namespace, pod)` → app) and collect the **non-empty** Applications.
3. Set the PVC's `ApplicationValue` to the **lexically-smallest** of that set; leave it empty when the set is empty.

A PVC's **own** annotation-resolved Application (D11) **always wins** — inheritance fires only for an otherwise-app-less PVC. The inherited value is written to `PVCNode.ApplicationValue` **before** `graph.NewGraph` freezes the graph (so it never mutates an immutable node) and **before** any projection, and it surfaces through the existing `Application()` accessor — therefore `data.application` and the `application`-group nesting treat it **identically** to an annotation-sourced value (the user's "bake into `Application()` — both" choice).

**Why:** an ArgoCD-managed workload routinely owns a PVC that carries no tracking-id annotation of its own (the annotation is on the workload, not always propagated to the claim). Grouping such a PVC with its workload's Application is what the operator expects, and the mounting pod is the authoritative signal of which workload owns the storage. Reusing the mount vector + the already-built pod application index means **no new upstream query, no new metric, no new node/edge type, and no serialiser or projection change** — the smallest possible addition. The lexically-smallest tie-break mirrors every other collision rule in the codebase (D2, D11), keeping output byte-stable.

**Determinism / projection:** the result depends only on the **set** of mounting pods and their resolved Applications (selected lexically-smallest), independent of vector/edge iteration order, so it is byte-stable across rebuilds (D10). Inheritance is resolved once over the full graph at assembly, never per request, so it is unaffected by `?cluster=` / `?namespace=` / `?name=` projection (consistent with "build once, project many" and a future cache). Because `pod-mounts-pvc` is intra-cluster and a pod mounts a PVC in its own namespace, the inherited app never crosses cluster or namespace — the `<cluster>/namespace/<ns>/application/<app>` group composes verbatim under the PVC's own namespace.

**Alternatives considered:**
- **Walk the built `pod-mounts-pvc` edges + `PodNode.Application()` after `TopologyEdges`** — *equivalent result, rejected as the spec'd mechanism:* it would require a post-edge mutation of PVC nodes (or a second node-build pass) for no behavioural gain; joining the mount vector to the pod application index at assembly is simpler and provably the same set. (Implementations MAY use either; the spec pins only the resolved value.)
- **Inherit at serialise time (group-only, leave `data.application` empty)** — *rejected (user chose "bake into `Application()` — both"):* would require a serialiser-only group-hint diverging from `Application()`, more code for a strictly-less-useful, inconsistent result.
- **Propagate in the other direction (pod inherits PVC app)** — *rejected:* the pod is the workload identity; storage inherits from workload, not the reverse.

## Risks / Trade-offs

- **[Golden/contract churn]** Pod and PVC `data.parent` change, two edge types and a real `storageclass` node type appear, and the synthesised `storageclass` group disappears. → Regenerate goldens (`go test ./internal/api -update -run Golden`) and OpenAPI (`make docs`); call the parent/edge changes out in the changelog as a behaviour (not wire-schema) change. The `{apiVersion, clusters, elements}` shape and determinism are unchanged.
- **[Synthetic-group ↔ real-node ID collision]** A group ID like `<cluster>/namespace/<ns>` could in theory equal a real `PVCID`/`ServiceID` `<cluster>/<ns>/<name>` if a namespace were literally named `namespace` (and `<name>` matched). → Practically impossible with the literal `namespace`/`application`/`controller` path segments; the pre-existing `storageclass` group already carried this theoretical risk. Mitigation if ever needed: a reserved leading segment. Documented, not guarded.
- **[Application split across namespaces]** An ArgoCD app spanning two namespaces shows as two `application` groups (it nests under `namespace`). → Intended per the user's chosen `cluster > namespace > application` order; app identity is still legible via the group name.
- **[Controller split across apps]** A controller whose pods carry different `argocd_tracking_id`s yields multiple app-scoped controller groups. → Rare; the path-encoding makes it deterministic and dangling-free rather than ambiguous.
- **[Namespace filter drops infra nodes]** Without D6, `?namespace=` would strip all `node`/`storageclass` nodes and dangle their edges. → D6 retention rule mirrors the existing K8s-node rule; covered by new projection tests.
- **[Operator allowlist missing]** `kube_storageclass_info` params (`storagePools`, `fsType`, `ClusterID`, `selector`, …) and `argocd_tracking_id` are operator `--metric-labels-allowlist` responsibilities. → OPTIONAL/graceful: missing labels → omitted attributes / no application group; no build failure (D8).
- **[Service/PVC application allowlist (D11)]** `kube_service_annotations` / `kube_persistentvolumeclaim_annotations` require `--metric-annotations-allowlist` for the `argocd.argoproj.io/tracking-id` annotation; without it (or for non-Argo objects) the metric is absent. → OPTIONAL/graceful: no service/pvc `application` attribute, those nodes nest under their namespace group; no build failure.
- **[Golden churn from service/PVC re-parenting (D11/D12)]** A service/PVC with a resolved Application moves `data.parent` from its namespace group to the new application group, and a new `application` group may be synthesised from a service/pvc with no pod in it. → Regenerate goldens; behaviour (not wire-schema) change; the no-application case is byte-identical to today.

- **[Inherited PVC application is indistinguishable from a declared one (D13)]** A PVC nesting under an `application` group may have inherited the app from its mounting pod rather than declaring it via annotation; the wire output carries no marker of provenance (the user's chosen "bake into `Application()`" semantics). → Intended — the grouping is the product goal; if provenance is ever needed it is an additive future attribute, not a wire break. The no-mounting-pod / app-less-pod case is byte-identical to today.
- **[Golden churn from PVC inheritance (D13)]** A previously app-less PVC that mounts an ArgoCD pod now moves `data.parent` from its namespace group to the inherited `application` group and gains `data.application`. → Regenerate goldens; behaviour (not wire-schema) change; a PVC with its own annotation, or with no app-bearing mounting pod, is unaffected.

## Migration Plan

1. `pkg/graph`: add `NodeTypeStorageClass`, `StorageClassNode`, `StorageClassID`, the `StorageClassInfo` struct + `StorageClassInfo()` sealed-interface method (nil on all other node types); add `EdgeTypePodToNode`, `EdgeTypePVCToStorageClass` + two `EdgeTypes` registry entries.
2. `pkg/promql`: add `QStorageClassInfo` + prefixed `Renderer` case.
3. `pkg/build`: 14th `ReadTopology` query; `resolveStorageClassInfo`; emit StorageClass nodes (+ D7 bare fallback, keep-first dedup); build `pod-to-node` + `pvc-to-storageclass` edges in `TopologyEdges`.
4. `pkg/graph/project.go`: D6 retention for `storageclass` (and confirm `node`); ensure new edge types route through traverse/filter.
5. `pkg/cytoscape`: remove old `storageclass`-group + `cluster>node>pod` parenting; synthesise `namespace`/`application`/`controller` groups; new `compoundParent` chain (skip-absent-levels); fixed emission order.
6. Specs deltas (`graph-api`, `cluster-topology-source`); `make docs`; `README.md`/`CLAUDE.md` (supersede D31).
7. Tests: serialiser/edge unit, property no-dangling-parent across 5 tiers, golden `-update`, integration fixtures ingesting `kube_storageclass_info` + node/owner/app labels.

**Rollback:** revert the change set; edge IDs are deterministic so no persisted state migration. v1 routes/`edge-types` additions are backward-tolerant (additive); a consumer pinned to the old nesting must update parsing.

## Open Questions

- **Attribute shape (D2): RESOLVED** — `provisioner` is a typed string attribute and the backing-storage values live in a typed `parameters` object, both via the new `StorageClassInfo()` accessor; `labels` stays `{cluster}`.
- **`controller/<kind>/<name>` granularity: RESOLVED** — use `Owner()` (Deployment after ReplicaSet collapse). The intermediate ReplicaSet is **never** surfaced as a tier (per operator: "no replicaset").
- **`?edge_type=` interaction with infra nodes:** with D6 retention, a `node`/`storageclass` node can appear even when its only edge type is filtered out (retained as a host/referenced infra node). Confirm this is the intended projection semantics (consistent with today's K8s-node-under-namespace behaviour).
