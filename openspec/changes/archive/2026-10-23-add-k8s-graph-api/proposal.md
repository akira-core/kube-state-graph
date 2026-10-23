# Proposal

## Why

Both graph endpoints today assume a NetApp estate. `GET /v1/graph` draws the
storage chain and the service graph together, and `GET /v1/storage-graph` is a
Sankey whose only weight is a Harvest QoS measurement. A consumer that wants the
plain Kubernetes picture around a pod or a node — where the pod runs, which
claims it mounts, what else runs on that node, and which namespace and ArgoCD
Application each piece belongs to — has to take a `/v1/storage-graph` body and
ignore its NetApp half, or take a `/v1/graph` body and strip NetApp, service and
call elements out of it, while the server still issues every Harvest (and, for
`/v1/graph`, service-graph) query to build what is thrown away.

## What Changes

- New route **`GET /v1/k8s-graph`**: the Kubernetes half of `/v1/storage-graph`
  — same request model, same by-reference reads, same fail-closed semantics —
  drawn with `/v1/graph`'s edge types instead of `storage-flow`, in the same
  `{apiVersion, clusters, elements}` Cytoscape.js body.
  - **Required**: `start`, `end` (validated exactly as on `/v1/graph`);
    `cluster`, `az`, `env`, each at least one non-empty value and repeatable
    (400 `missing_cluster` / `missing_az` / `missing_env`; the selected cluster
    identities are the Cartesian product, `cluster` the RAW name); and exactly
    one root kind (400 `missing_root` when none, `invalid_scope` when both).
  - **Root kinds**: `pod=<namespace>/<name>` and `node=<name>` (a Kubernetes
    node), each repeatable, validated as on `/v1/storage-graph`.
  - **Optional**: `namespace` (repeatable). Every other parameter — `prune`
    and the storage-graph-only root kinds included — is ignored.
- **Only what a root reaches is drawn.** A pod is drawn iff it is a root or
  runs on a root node; a claim iff a drawn pod mounts it; a Kubernetes node iff
  a drawn pod runs on it or it is a root. A root the upstream names is drawn
  even when nothing else is (an unscheduled root pod, a podless root node). An
  unmounted claim is never drawn.
- **Body.** Real nodes `pod`, `node` and `pvc`; edges `pod-to-node` and
  `pod-mounts-pvc`; the synthesised `cluster > namespace > application >
  controller` groups. Pods, nodes and claims carry the attributes
  `/v1/storage-graph` gives them — pod `ipaddress`, `owner`, `application`
  (ArgoCD), node `ipaddress` / `ready_status`, claim `application` (own
  annotation, else inherited from its mounters), `usage`, `storageclass`, and
  `alerts` / `status` on all three — and `namespace` / `application` groups
  keep `namespace_owner` / `deployed_by`. No `service`, `external` or NetApp
  node, no `storage-cluster` group, no other edge type, no `data.containers`,
  no `data.qos`, no PVC `labels.aggr` / `labels.svm`.
- **Reads only Kubernetes families, by reference.** The build walks from the
  root as `/v1/storage-graph` does for a `pod` / `node` root — except that a
  `node` root also loads every pod running on it, claim-mounting or not, which
  the storage body does not — and issues no Harvest, service-side,
  `kube_pod_container_info` or service-graph query.
- **Fails closed** on any upstream error except `ALERTS` (502
  `upstream query failed: <family>`). An unmatched root is an empty 200.
- **In-process engine.** `kubegraph.ParseK8sValues`, `Engine.BuildK8s` and
  `Engine.BuildK8sFromValues`, backed by `build.Builder.BuildK8s` and
  `graph.ProjectK8s`, with the same contract as the route.

## Capabilities

### New Capabilities

- `k8s-graph-api`: the `GET /v1/k8s-graph` endpoint and its in-process engine
  counterpart — request contract and root kinds, by-reference Kubernetes-only
  reads, root-reachability projection, body, and fail-closed error semantics.

### Modified Capabilities

None. `/v1/graph` and `/v1/storage-graph` are unchanged; the new capability
states which `storage-graph-api`, `graph-api` and `alert-overlay` rules its
reads, attributes and grouping follow.

## Impact

- **API**: one new `/v1/` route, committed for v1's lifetime. Auth
  (`X-API-Key`), `--build-timeout` (504), `--end-align`, the upstream guard,
  the routing table and the root-set chunk cap are shared with the other graph
  endpoints.
- **Code**: `pkg/build` (a Kubernetes-only variant of the storage read plan and
  `Builder.BuildK8s`), `pkg/graph` (`ProjectK8s`), `pkg/kubegraph` (parser,
  `Engine` methods), `internal/api` (handler, route, swag annotations),
  `docs/swagger.{json,yaml}` (regenerated).
- **Tests**: parser, plan / fan-out, projection, handler, golden and
  integration tests.
- **Docs**: `README.md`, `docs/upstream-metrics.md`, `CLAUDE.md`,
  `.claude/rules/storage-graph.md`, `.claude/rules/graph-model.md`.
- No new dependency, no new self-metric, no new edge or node type.
