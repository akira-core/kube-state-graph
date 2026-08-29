## MODIFIED Requirements

### Requirement: Coverage of the API contract

The container-integration suite SHALL contain at least one test for each of the following behaviours:

- A single-cluster graph rendering with `pod-mounts-pvc` edges and pod→node compound nesting derived from each pod's `labels.node` (no edge links a pod to its host K8s node).
- A multi-cluster graph with at least one `pod-calls-pod` edge whose source-node `labels.cluster` differs from its target-node `labels.cluster` (cross-cluster edge recovered via the topology pod-UID index), requested without a `cluster` filter.
- A connection-string client/server label containing `"://"` that does NOT resolve to an in-cluster pod/service producing an `external`-typed node with `labels={}` (D29).
- A headless per-pod connection string (`<pod>.<svc>.<ns>.svc.cluster.local`) resolving to its `type=service` node (the pod-hostname dropped) plus `service-selects-pod` fan-out edges — NOT to a specific pod.
- A ClusterIP-service connection string resolving to a `type=service` node plus `service-selects-pod` edges to its backing pods.
- The missing pod-UID human-label fallback producing an `external`-typed node (D27).
- An `az` / `env` filtered request whose fixtures stamp the configured labels on every topology family (kube-state-metrics, kubelet, Harvest): the response contains only the matching zone / environment's workload and infrastructure and `clusters` lists only its clusters.
- An `az` / `env` filtered request matching no series returning `200` with empty `elements` and `clusters: []` (not `outside_retention`).
- A `namespace` filtered request against fixtures spanning two namespaces: only the requested namespace's pods, claims, and services are loaded; K8s nodes and NetApp aggregates appear only by reference from that namespace's pods and claims.
- A `namespace` filtered request whose in-scope pod calls an out-of-namespace pod: the peer is rendered as `external/<server label>` with `labels={}`, no pod is synthesised, and a series between two out-of-namespace pods produces nothing.
- A `cluster` filtered request whose in-scope pod calls a pod in another cluster: the partner is `external/<server label>` and no other-cluster pod node is present.
- A `prune=false` request surfacing a connectivity-disconnected pod together with its `pod-to-node`, `pod-mounts-pvc`, and `pvc-to-netapp-aggr` chain, and a `prune=false` request with no filter surfacing a podless K8s node.
- `/v1/edge-types` returning the static catalogue.

Fixtures SHALL stamp `cluster` on every kube-state-metrics and kubelet series and `az` / `env` (under the default keys) on every topology family, so the selector-level filters can be exercised end-to-end against a real VictoriaMetrics.

#### Scenario: All listed behaviours covered

- **WHEN** an operator inspects `internal/integration/`
- **THEN** at least one `*_test.go` test exists (and passes) for each behaviour bullet above
