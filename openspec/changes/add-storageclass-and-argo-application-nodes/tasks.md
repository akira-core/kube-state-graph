# Tasks

Order is dependency-first: graph model → query → build → projection → serialiser → docs → tests → verify. Follow the repo's TDD convention (write the failing test, then the implementation) for each unit-testable task.

## 1. Graph model (`pkg/graph`)

- [x] 1.1 Add `NodeTypeStorageClass = "storageclass"` to the node-type constants (`node.go`).
- [x] 1.2 Add the `StorageClassInfo` struct (`{ Provisioner string; Parameters map[string]string }`) and the `StorageClassInfo() *StorageClassInfo` method to the sealed `GraphNode` interface (D2).
- [x] 1.3 Implement `StorageClassInfo()` returning `nil` on `PodNode`, `K8sNode`, `PVCNode`, `ServiceNode`, `ExternalNode` (the `Owner()` precedent).
- [x] 1.4 Add the `StorageClassNode` concrete type implementing the full sealed interface: `Type()="storageclass"`, `Labels()` = `{cluster}`, `StorageClassInfo()` returns its own provisioner/parameters (nil when bare), and `IPAddress()=nil`, `Owner()=nil`, `Application()=""`, `Containers()=nil`, `ReadyStatus()=""`, `StorageClass()=""`.
- [x] 1.5 Add `StorageClassID(cluster, name) string = <cluster>/storageclass/<name>` and confirm `SortNodes` orders the new node by ID with no change.
- [x] 1.6 Add `EdgeTypePodToNode = "pod-to-node"` and `EdgeTypePVCToStorageClass = "pvc-to-storageclass"` to `edge.go`.
- [x] 1.7 Add the two `EdgeTypeDefinition` entries to the `EdgeTypes` registry (`registry.go`): pod→node and pvc→storageclass, both `Directed: true`, `MayCrossCluster: false`, `Labels: nil`, with the correct `SourceType`/`TargetType` node-type slices.
- [x] 1.8 Unit-test the new node (interface conformance, `StorageClassInfo` nil/non-nil), the edge-type registry entries, and deterministic edge IDs for the two new types.

## 2. Upstream query (`pkg/promql`)

- [x] 2.1 Add `QStorageClassInfo = "kube_storageclass_info"` (bare constant).
- [x] 2.2 Add the prefixed `Renderer` case rendering `last_over_time(<prefix>kube_storageclass_info[<window>]) @ <end>`.
- [x] 2.3 Unit-test the render with empty and non-empty prefix (matches the cluster-topology-source prefix scenario).

## 3. Topology build (`pkg/build`)

- [x] 3.1 Add the 14th `g.Go` fetch for `QStorageClassInfo` to the `ReadTopology` errgroup and a field on the raw-vectors struct.
- [x] 3.2 Implement `resolveStorageClassInfo(vec) map[scKey]*graph.StorageClassInfo`: resolve `provisioner` (native label) and the `parameters` keys with source-label fallback (`storagePools`→`pool`, `fsType`→`fsName`, `ClusterID`, `selector`), omitting empty values, `bucketCluster` for missing cluster, lexically-smallest on collision.
- [x] 3.3 Materialise `StorageClassNode`s into the `Topology` (one per observed `(cluster, storageclass)`), plus **bare** nodes for any storageclass referenced by a PVC but absent from `kube_storageclass_info`; append attributed nodes before bare ones so `NewGraph` keep-first dedup keeps the attributed node.
- [x] 3.4 Build `pod-to-node` edges in `TopologyEdges` for every pod with a non-empty `labels["node"]` (`source=pod.ID()`, `target=labels["node"]`, no labels), deduped by `(podID, nodeID)`.
- [x] 3.5 Build `pvc-to-storageclass` edges in `TopologyEdges` for every PVC with a non-empty resolved StorageClass (`target=StorageClassID(cluster, name)`, no labels), deduped by `(pvcID, scID)`.
- [x] 3.6 Wire the new StorageClass nodes into `assemble()`/`graph.NewGraph` (topology nodes appended before service-graph nodes per the existing ID-collision keep-first rule).
- [x] 3.7 Unit-test `resolveStorageClassInfo` (fallback order, omitted keys, deterministic collision pick, bare fallback) and the two new edge builders (scheduled/unscheduled pod, PVC with/without storageclass, deterministic IDs).

## 4. Projection (`pkg/graph/project.go`)

- [x] 4.1 Extend the namespace-filter deferred-admission rule so `type="storageclass"` nodes are retained iff an in-scope PVC references them (build a `referencedStorageClasses` set alongside `hostNodes`); leave the existing K8s-node `hostNodes` rule intact (D6).
- [x] 4.2 Confirm the two new edge types route correctly through `traverse` (edge-type-aware BFS) and `filterEdges` (endpoint retention / re-add); `node` and `storageclass` partners re-add under the non-cluster filter path.
- [x] 4.3 Add `StorageClassNode` to the `name` filter match set (exact `Name()` equality).
- [x] 4.4 Unit-test: namespace filter retains/drops storageclass via PVC reference; `?name=<sc>` matches a StorageClass; `?name=<node>` now pulls scheduled pods via `pod-to-node`; `?edge_type=` interaction.

## 5. Serialiser (`pkg/cytoscape`)

- [x] 5.1 Remove the old synthesised `storageclass` group (the `nodeTypeStorageClass`/`storageClassParentID`/`scSeen` path) and the `cluster > node > pod` / `cluster > storageclass > pvc` parenting.
- [x] 5.2 Synthesise `namespace` / `application` / `controller` group DTOs from each emitted pod's `Labels()["namespace"]`, `Application()`, and `Owner()`, using path-encoded IDs (D4) so each group has exactly one parent.
- [x] 5.3 Implement the new `compoundParent` chain with skip-absent-levels: pod → controller→application→namespace; service/pvc → namespace; node/storageclass → cluster; external → none.
- [x] 5.4 Emit synthetic groups in tier order (cluster, namespace, application, controller), each tier sorted by ID, before real nodes (`SortNodes`); preserve byte-determinism (D10).
- [x] 5.5 Add `provisioner` (string, `omitempty`) and `parameters` (`map[string]string`, `omitempty`) to `NodeData`, populated from `n.StorageClassInfo()` (no type-switch — sealed method only).
- [x] 5.6 Unit-test: full hierarchy + each skip-absent case, service/pvc/node/storageclass parents, StorageClass payload (provisioner/parameters/bare), no-dangling-parent invariant, deterministic emission order.

## 6. Docs & contracts

- [x] 6.1 Run `make docs` and commit regenerated `docs/swagger.{json,yaml}` (new node type, two edge types, provisioner/parameters fields).
- [x] 6.2 Update `README.md` metric table (`kube_storageclass_info`) and edge table (`pod-to-node`, `pvc-to-storageclass`).
- [x] 6.3 Update `CLAUDE.md` to record that this change supersedes D31 (new hierarchy, pod→node and pvc→storageclass edges, real storageclass node with provisioner/parameters).

## 7. Tests (cross-cutting)

- [x] 7.1 Regenerate golden snapshots (`go test ./internal/api -update -run Golden`) and review the diff for the new hierarchy, the two edges, and the storageclass node payload.
- [x] 7.2 Extend the property test (`pkg/cytoscape/property_test.go`) to assert no dangling `data.parent` across all five group tiers.
- [x] 7.3 Add integration fixtures (`internal/integration`) ingesting `kube_storageclass_info` (with `provisioner` + NetApp/Ceph param labels), `kube_pod_owner` (`argocd_tracking_id`), and scheduled-pod `kube_pod_info`; assert the `cluster > namespace > application > controller > pod` nesting, the `pod-to-node` / `pvc-to-storageclass` edges, and the storageclass `provisioner`/`parameters` payload. (TestPVCStorageClassNodeAndEdge / TestPVCWithoutStorageClassNestsUnderNamespace, 29 integration tests pass against real VM.)
- [x] 7.4 Run `make test` (race + shuffle), `make vet`, `make lint`, `make vuln` — all green. (408 pass race+shuffle on non-integration packages; 29 integration; lint 0 issues; vuln none; vet clean.)

## 8. Verify & finalise

- [x] 8.1 `openspec validate "add-storageclass-and-argo-application-nodes"` passes. (`openspec verify` is not a command in this CLI version; `validate` is the structural check.)
- [x] 8.2 `make build` succeeds; `make docs` / `make verify-mocks` up-to-date. The real-VM integration round-trip (group 7.3) exercises `/v1/graph` end-to-end, superseding a manual smoke check.
