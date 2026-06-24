# Tasks: add-pod-application-containers

## 1. Query layer

- [x] 1.1 Add `QPodContainerInfo Query = "kube_pod_container_info"` constant in `pkg/promql/queries.go` and a `Render` case `tlast_over_time(%skube_pod_container_info[%s])` (prefix-aware; `tlast_over_time` so each image-variant series' value is its last-sample timestamp, enabling the latest-image pick — see design.md D-A4)
- [x] 1.2 Extend `pkg/promql/queries_test.go`: bare-name default render, `o11y_` prefix render, and `query_name` stays `kube_pod_container_info`

## 2. Graph types (sealed `GraphNode`)

- [x] 2.1 In `pkg/graph/node.go` add `type Container struct { Name, Image string }` (JSON tags `name` / `image`) next to `Owner`
- [x] 2.2 Add `Application() string` and `Containers() []Container` to the sealed `GraphNode` interface with doc comments (typed attributes, never in `labels`; only pods carry them — same precedent as `Owner()`)
- [x] 2.3 Add `ApplicationValue string` + `ContainersValue []Container` fields to `PodNode`; implement `Application()`/`Containers()` on `PodNode` (return the fields) and on `K8sNode` / `PVCNode` / `ServiceNode` / `ExternalNode` (return `""` / `nil`)
- [x] 2.4 Unit test in `pkg/graph`: non-pod kinds return `""` / `nil`; pod returns set values

## 3. Resolve layer (topology)

- [x] 3.1 Add `PodContainerInfo model.Vector` to the topology fetch struct in `pkg/build/topology.go`; add `g.Go(fetch(promql.QPodContainerInfo, &v.PodContainerInfo))` (11th leg) and a `RawSeriesCount` entry keyed by `string(promql.QPodContainerInfo)`
- [x] 3.2 Add `resolvePodContainers(vec, mc) map[podNameKey][]graph.Container`: key by `(cluster, namespace, pod)` via `mc.bucket(promql.QPodContainerInfo, ...)`, element `{Name: container, Image: image}`, skip empty-`image` series, dedupe per `container` taking the latest-seen image (greatest `s.Value`, the `tlast_over_time` last-sample timestamp; lexically-smallest image on exact tie), sort the per-pod slice by `(name, image)`
- [x] 3.3 Add `resolvePodApplications(vec) map[podNameKey]string` reading the `argocd_tracking_id` label off `v.PodOwner` independently of the controller pick (uses pure `bucketCluster` — `resolvePodOwners` owns the QPodOwner missing-cluster tally): Application = substring before first `:` (verbatim when no `:`), lexically-smallest non-empty value on per-pod collision, empty → absent
- [x] 3.4 Wire both maps into the pod-assembly loop (set `ApplicationValue` / `ContainersValue` on each `*graph.PodNode`), leaving `application` empty / `containers` nil when unresolved
- [x] 3.5 Unit tests in `pkg/build`: multi-container sorted; latest-seen image wins (recency, not lexical) + order-independent; exact last-seen tie → lexically-smallest; empty image skipped even if seen later; no container series → nil; full tracking-id → leading segment; bare value (no `:`) → verbatim; per-pod collision → lexically-smallest; absent `argocd_tracking_id` → empty; both metrics absent → valid build, no attrs

## 4. Serialiser

- [x] 4.1 In `pkg/cytoscape/cytoscape.go` add `Application string \`json:"application,omitempty"\`` and `Containers []graph.Container \`json:"containers,omitempty"\`` to the node `data` DTO; populate from `n.Application()` / `n.Containers()` alongside the existing `IPAddress` / `Owner` wiring
- [x] 4.2 Unit test in `pkg/cytoscape`: pod with both attrs serialises `data.application` + `data.containers` (ordered); pod with neither omits both keys; non-pod omits both

## 5. API surface verification

- [x] 5.1 Component test in `internal/api`: pod `data.application` + `data.containers` present from injected fixtures; `labels` carries no `argocd_tracking_id` / container key; non-pod nodes omit both
- [x] 5.2 Refresh golden snapshots only if fixtures change: not needed — existing golden inputs carry no ArgoCD/container series, so the `omitempty` attributes leave snapshots byte-identical (golden suite passes unchanged)

## 6. Integration

- [x] 6.1 Added `TestPodApplicationAndContainersAttributes` in `internal/integration` (dedicated `appcat` namespace to avoid collision with shared `checkout`/`cart` fixtures): ingests `kube_pod_container_info` (2-container pod) + `kube_pod_owner` with `argocd_tracking_id`; asserts the pod node's `application` and ordered `containers`, neither in labels, and a sibling pod omits both. Passes against a real VM container.

## 7. Docs / spec sync

- [x] 7.1 Updated CLAUDE.md: new load-bearing rule for `application`/`containers` typed attributes, added `kube_pod_container_info` to the `KSG_METRIC_PREFIX` list, extended the sealed-types accessor list (`Application()`/`Containers()`); `pkg/graph/node.go` interface carries doc comments for both methods
- [x] 7.2 Refreshed the handwritten `@Description` sample-response example in `handlers.go` (now shows `application`/`containers`) and ran `make docs` — swag auto-introspects `cytoscape.Body`, so `NodeData.application`/`containers` + the `graph.Container` schema landed in `docs/swagger.{json,yaml}` (regeneration is idempotent)
- [x] 7.3 `make build` ✅, `make vet` ✅, `make lint` ✅ (0 issues), `make test` ✅ (`-race -shuffle=on`, all packages `ok` incl. integration); `openspec validate --strict` ✅. `make check-docs` flags only the expected uncommitted-`docs/` drift (regeneration is idempotent / in sync with source) — resolved on commit. (`openspec verify` is not in this CLI version; `openspec validate` is the equivalent gate.)
