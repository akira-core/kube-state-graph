# Design

## Context

See proposal.md — Why. The requirements are in `specs/k8s-graph-api/spec.md`.

`Builder.BuildStorage` already does almost everything this endpoint needs for a
`pod` or `node` root. `storagePlan(roots)` is `byReference` and `failClosed`;
`readTopology` launches the root's seed (`readNodeSeed` / `readPodSeed`), then
the by-reference expansion: `readWorkloadClaims` (claim side by
`(namespace, claim)`, mounter completion), `readScopedPods`,
`readScopedNodes`, `readScopedControllers` and `readScopedAppMetadata`. Beside
them it launches three Harvest waves for a workload root:

- `readWorkloadVolumeLabels` — candidate completion and owner completion
  (`volume_labels` by claim token, then whole aggregates);
- `readReachedHarvest` — aggregate gauges, controller families and
  fixed-policy ceilings for what the claims reached;
- `readScopedQoS` — the six QoS workload families.

`readFlowlessHarvest` runs only for storage-side root kinds. First-wave legs a
plan names in `skip` are never issued; `storageSkippedLegs` already drops the
container family, the four service-side families and the unscoped
`app_metadata` leg.

The storage body is then assembled with `assembleStorageFlow` (`storage-flow`
edges) and projected with `graph.ProjectStorage` (complete NetApp-to-pod
paths). Neither fits a body without NetApp: there is no path to complete.
`assemble` (the `/v1/graph` assembler) already turns the same topology into
`pod-to-node` / `pod-mounts-pvc` edges, and with no Harvest and no service
graph it emits nothing else.

With `volume_labels` never read, `resolveNetAppStorage` sees `topoPresent` and
`qosPresent` false and fires neither join-miss warning, and no claim gains
`labels.aggr` / `labels.svm` or `data.qos`.

## Goals / Non-Goals

**Goals:**

- Reuse the storage plan's seeds, expansion, chunk caps, routing and
  fail-closed handling unchanged; add only a Kubernetes-only switch, a
  projection, a parser, engine methods and a handler.
- Leave every `/v1/graph` and `/v1/storage-graph` body, fan-out and golden
  byte-identical.

**Non-Goals:**

- `pvc`, `pv`, `application` or storage-side roots; unmounted claims; services,
  call edges or containers.
- A new error `Reason`, self-metric, node type or edge type.

## Decisions

### D1 — `k8sPlan(roots)`: the storage plan with Harvest switched off

```go
func k8sPlan(roots graph.StorageRoots) topologyPlan {
    p := storagePlan(roots)          // byReference, failClosed, node / pod seed
    p.skip = k8sSkippedLegs          // storageSkippedLegs ∪ every Harvest first-wave leg
    p.kubernetesOnly = true
    return p
}
```

`kubernetesOnly` gates the three Harvest launches in `readTopology`:
`readWorkloadVolumeLabels` and `readReachedHarvest` are not started, and the
`volumeLabelsFinal` signal they would close is closed up front, so
`readScopedQoS` computes an empty scope from the nil `VolumeLabels` and issues
nothing. The first-wave Harvest legs are skipped like any other; the skip set
is derived from `topologyLegs` and `promql.FamilyOf`, so a Harvest leg added
later is skipped with no edit. The gates keep the fan-out from issuing a
Harvest query, and `refuseHarvest` enforces it: a `kubernetesOnly` build reads
through a querier that fails any Harvest query before it reaches a store, so a
Harvest reader added without a gate turns the build red instead of reading the
harvest backends.
Everything else — seeds, incarnation completion, mounter completion, pods,
nodes, controllers, the namespace-restricted `app_metadata` read, kubelet,
`ALERTS` — runs as for `BuildStorage`, so the Kubernetes reads (and their chunk
caps and `invalid_scope` rejection) cannot drift between the two endpoints,
with ONE addition for a `node` root. The storage node seed keeps a pod only
when it mounts a claim (`keepPodBindings`), and the pod wave is scoped to the
pods those bindings name, so a storage build never loads a root node's claimless
pods — correct for a storage body, wrong for a graph of "what runs on this
node". Under `kubernetesOnly`, `readNodeSeed` therefore also records every pod
whose newest incarnation runs on a root node (`topologyVectors.RootNodePods`,
with their incarnation rows in `RootNodePodInfo`), and the pod wave
(`readRootNodePods`) is keyed by the full `(az, env, cluster, namespace, pod)`:
those pods plus every pod a kept binding names. A per-namespace read still
returns a same-named pod of another selected cluster; it is dropped, so it never
scopes the node, controller and application-metadata waves, which follow
unchanged. `kube_pod_info` is read only for the mounters — the root-node pods'
rows are incarnation completion's own, the same query over the same pairs. The
storage plan never sets the flag, so its fan-out and bodies are untouched.

`BuildK8s` accepts only `StorageRootPod` / `StorageRootNode`, each pod ref
carrying both a namespace and a name, and returns `ReasonInvalidScope`
otherwise, so an embedder cannot reach a seed whose purpose is a Harvest read
or a pod root that reads its name across every namespace. It is then
`buildStorage` under `k8sPlan`; the plan picks only the span, the log line and
the assembler.

*Alternative — the unscoped `fullPlan` with Harvest skipped (the previous
draft of this change).* Rejected now that the body is root-scoped: it reads
every pod of the selected clusters to draw a node's handful, and makes the
history-heavy annotation families fatal on a cluster-wide read.

*Alternative — a separate k8s seed implementation.* Rejected: it would
restate the node seed's incarnation completion and the expansion's mounter
completion, the two rules that keep the body byte-identical to an unrestricted
read.

### D2 — `Builder.BuildK8s(ctx, window, end, sel, roots)`, assembled with `assemble`

`BuildK8s` prepares `k8sPlan(roots)` (the seed cap rejects before any query),
binds `querierFor(sel)`, reads, and calls `assemble(topology,
ServiceGraphResult{})`. It then runs the shared bake-before-freeze sequence
(`attachAlerts`, `attachStatus`, `graph.NewGraph`, then `ClusterIdentities`,
`NamespaceOwners`, `ApplicationDeployers`). No `ReadServiceGraph`, no `up{}`
probe, no `outside_retention`, and none of the three last-build gauges — the
adapters reset each vector on write, so a k8s build would erase the
`/v1/graph` series, the reason `BuildStorage` gives. Span
`kube-state-graph.build_k8s`, log `k8s graph built`.

The freeze sequence then appears three times. Extracting it into one helper is
in scope only if both existing bodies stay byte-identical.

### D3 — `graph.ProjectK8s(g, scope StorageScope)`: pod-centric reachability

A pure function of `(g, scope)`:

1. **Roots.** `node` roots resolve to `K8sNode` ids whose node name is a root
   value and whose raw cluster passes `scope.Clusters`; `pod` roots to
   `PodNode`s whose `(namespace, name)` is a root ref and which pass the
   cluster filter.
2. **Pods.** A pod is kept iff it is a resolved pod root or its `labels.node`
   is a resolved node root, AND it passes the cluster / namespace filters.
3. **Claims.** A PVC is kept iff a kept pod has a `pod-mounts-pvc` edge to it
   (and it passes the filters — always true, since the binding is
   same-cluster, same-namespace).
4. **Nodes.** A Kubernetes node is kept iff a kept pod has a `pod-to-node` edge
   to it, or it is a resolved node root.
5. **Edges.** Exactly the `pod-to-node` and `pod-mounts-pvc` edges whose source
   is a kept pod. SortNodes / SortEdges.

With only pod and node roots every kept claim and node hangs off a kept pod, so
"edge source is kept" is equivalent to "the edge lies on a path that touches a
root"; there is no claim root to make the two differ. Mounter completion's
extra pods (other mounters of a shared claim) are in the graph and drive the
claim's inherited Application, but no rule keeps them.

`ProjectK8s` takes the existing `StorageScope` (`Clusters`, `Namespaces`,
`Roots`) rather than a new type: the parser builds it with
`graph.NewStorageScope`, which already validates `pod` refs. It ignores root
kinds other than `pod` / `node` (an empty view), mirroring how
`ProjectStorage` treats a kind that resolved nothing.

*Alternative — `graph.Project` plus a post-filter.* Rejected: `Project`'s
admission is filter-driven, not root-driven, and the re-add pass in
`filterEdges` would resurrect excluded partners.

### D4 — Parser and engine in `pkg/kubegraph`

```go
type K8sRequest struct {
    Start, End time.Time
    Scope      graph.StorageScope
    Selector   promql.Selector
}
func ParseK8sValues(v url.Values) (K8sRequest, error)
func (e *Engine) BuildK8s(ctx, window, end, sel, roots graph.StorageRoots) (*graph.Graph, error)
func (e *Engine) BuildK8sFromValues(ctx, v url.Values) (cytoscape.Body, error)
```

`ParseK8sValues`: `parseWindow`; `atLeastOne` for `cluster`, `az`, `env` in
that order; `validateSelectorValues` over `cluster`, `namespace`, `az`, `env`,
`pod`, `node`; one root among `pod` / `node` — `oneStorageRoot` generalised to
take its parameter list, so the `missing_root` / `invalid_scope` messages are
produced by one function for both endpoints; `graph.NewStorageScope(cluster,
namespace, kind, values)`. The selector carries the sorted, de-duplicated
`cluster` / `az` / `env` / `namespace` sets.

### D5 — Handler

`handleK8sGraph` = `ParseK8sValues` → `writeParseError` (already maps any
`ParseError.Reason` to a 400 verbatim, so no `errors.go` entry) →
`runBuild(…, closure over s.builder.BuildK8s with req.Scope.Roots)` →
`mapBuildError` (`QueryError` is already 502 `upstream query failed:
<family>`; `invalid_scope` from the seed cap is already 400) →
`projectWithSpan("kube-state-graph.project_k8s", graph.ProjectK8s)` →
`cytoscape.Serialise`. Registered on the authenticated `v1` group, so auth,
`otelgin` and the request log follow.

## Risks / Trade-offs

- **[A shared claim's other mounters are read but not drawn]** Mounter
  completion loads every pod mounting a tracked claim, plus their nodes and
  controllers, to keep the claim's inherited Application identical to
  `/v1/graph`. → That is the storage plan's existing cost for the same roots;
  the reads are by reference and chunked.
- **[Product over-selection]** `az=a&az=b&cluster=c1&cluster=c2` selects four
  identities; a caller wanting `a-…-c1` and `b-…-c2` only sends two requests.
  → Stated in the swag description.
- **[Unmounted claims are invisible]** No root kind reaches one. → Stated in the
  spec; a `pvc` root would be a later, additive change.
- **[Coupling to the storage plan]** A change to the node or pod seed changes
  both endpoints. → Intended: the two must agree on what a node or pod root
  reads. The k8s fan-out test (tasks 1.x) pins the k8s side separately.
- **[Name proximity]** `kubegraph` (package) vs `k8s-graph` (route). → The Go
  surface is method names on the existing engine; there is no `k8sgraph`
  package.

## Migration Plan

Additive: one new route, new exported functions, no change to an existing
body, parameter or metric. Rollback is removing the route.
