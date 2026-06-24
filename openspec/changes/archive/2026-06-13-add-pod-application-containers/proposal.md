## Why

Operators viewing the pod graph cannot today see two facts that are first-class in
day-to-day Kubernetes operations: **which ArgoCD Application owns a pod**, and
**which container images a pod is actually running**. Both are already present in
the centralised kube-state-metrics feed (the ArgoCD tracking-id is exposed on
`kube_pod_owner` via the operator's labels-allowlist; per-container name/image is
exposed by `kube_pod_container_info`), so surfacing them is a pure join — no new
upstream dependency and no Kubernetes API access. Adding them lets the graph
answer "what app is this?" and "what image is this running?" without leaving the
topology view.

## What Changes

- **New `data.application` attribute on `type="pod"` nodes** — the ArgoCD
  Application name, derived from the ArgoCD tracking-id label carried on
  `kube_pod_owner`. Read from the **same** `kube_pod_owner` query already issued
  for the controller-owner attribute (no new PromQL query). Typed, nullable,
  `omitempty` — omitted entirely for pods not managed by ArgoCD (no empty string).
  Surfaced as a top-level `data` attribute, **never inside `labels`** — same
  precedent as `owner` / `ipaddress` / `storageclass`.
- **New `data.containers` attribute on `type="pod"` nodes** — an array of
  `{ name, image }` objects, one per container, read from a new
  `kube_pod_container_info` topology query (the build fan-out grows from 10 → 11
  parallel PromQL queries). Deterministically ordered (sorted by `(name, image)`)
  so the response body stays byte-identical across rebuilds; `omitempty` so a pod
  with no observed container info (e.g. a synthesised service-graph pod) omits the
  field. Typed `data` attribute, never inside `labels`.
- **`kube_pod_container_info` joins the `KSG_METRIC_PREFIX` set** — the new metric
  is kube-state-metrics-shaped, so the configurable upstream prefix applies to it
  (the existing `kube_*` series the prefix already covers gains one entry). The
  per-build self-metric gains one `query_name` dimension value for the new query.
- Both new reads are **OPTIONAL**: absent series degrade gracefully (no
  `application` / `containers` on the affected pods) and never fail the build —
  same tolerance contract as the existing owner / PVC / service families.
- No breaking changes: both attributes are additive `omitempty` fields. Existing
  golden bodies for pods without ArgoCD/container data are unchanged.

## Capabilities

### New Capabilities
<!-- None — both attributes are additive typed attributes on the existing pod node. -->

### Modified Capabilities
- `cluster-topology-source`: the **Topology series consumed** and **Configurable
  upstream metric-name prefix** requirements gain `kube_pod_container_info`; a new
  requirement specifies resolving the per-container `{name, image}` list and the
  ArgoCD Application name (the latter extracted from the ArgoCD tracking-id label
  on `kube_pod_owner`, read alongside the existing controller-owner resolution).
- `graph-api`: the **Cytoscape.js response shape** requirement gains the optional
  `data.application` (string) and `data.containers` (`[{name, image}]`) attributes
  on pod nodes, with `omitempty` and deterministic-ordering semantics — same
  typed-attribute precedent as the **Node `ipaddress` attribute** requirement.

## Impact

- **`pkg/graph/node.go`**: extend the sealed `GraphNode` interface with
  `Application() string` and `Containers() []Container` (returning `""` / `nil` for
  the four non-pod kinds, matching the `Owner()` / `StorageClass()` precedent); add
  a `Container` struct (`{Name, Image string}`) and the backing fields on
  `PodNode`.
- **`pkg/build/topology.go`**: new `kube_pod_container_info` query + parse into a
  sorted per-pod container list; extend the `kube_pod_owner` resolution to also
  pull the ArgoCD tracking-id label and derive the Application name; wire both onto
  the assembled `PodNode`.
- **`pkg/build`** (builder fan-out): add the 11th parallel query to the topology
  errgroup.
- **`pkg/promql`**: new `QPodContainerInfo` query constant; the new metric joins
  the prefix-rendered set.
- **`pkg/cytoscape`**: serialise `data.application` and `data.containers`
  (`omitempty`).
- **`internal/api`**: golden testdata refresh; OpenAPI `@`-annotations + `make
  docs` regeneration for the new pod-node fields.
- **Tests**: unit (topology parse/sort/omitempty, Application derivation), golden
  snapshots, and `internal/integration` fixtures carrying
  `kube_pod_container_info` and an ArgoCD-labelled `kube_pod_owner`.
- No new dependencies; no Kubernetes API access; no new HTTP route or edge type.
