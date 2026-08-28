## MODIFIED Requirements

### Requirement: Edge cluster label

For every emitted `pod-calls-pod` or `pod-calls-service` edge whose **client side resolves to a pod**, the reader SHALL set `labels.cluster` to a cluster **identity** (`cluster-topology-source`, "Cluster identity composed from zone and environment labels"): the client pod's own `labels.cluster` when the client resolved to a topology pod, otherwise the series' `cluster` label resolved through the identity ladder (composed from the series' own zone/environment labels when it carries both, adopted when the raw name is unambiguous in the build, verbatim otherwise). The label represents the cluster that originated the RPC. When the **client side resolves to a non-pod node** (service or external), the reader SHALL omit the `cluster` key from the edge's `labels` (non-pod endpoints are not cluster-scoped). The reader SHALL NOT emit `client_cluster` or `server_cluster` keys on edge `labels` (server-side cluster is derivable from `target` node's `labels.cluster`). The reader SHALL NOT encode a `cross_cluster` boolean inside `labels` (booleans are deferred to a future typed field); cross-cluster status is derived by comparing the resolved source and target nodes' `labels.cluster` values.

#### Scenario: Intra-cluster RPC

- **WHEN** the reader processes a series with `cluster="cluster-alpha"` whose `client_k8s_pod_uid` and `server_k8s_pod_uid` both resolve to pods in `cluster-alpha`
- **THEN** the emitted edge has `labels.cluster: "cluster-alpha"`, the `target` node's `labels.cluster` is also `"cluster-alpha"`, and the edge contains no `client_cluster`, `server_cluster`, or `cross_cluster` key

#### Scenario: Cross-cluster RPC

- **WHEN** the reader processes a series with `cluster="cluster-alpha"` whose `client_k8s_pod_uid` resolves to a pod in `cluster-alpha` and whose `server_k8s_pod_uid` resolves via the global UID index to a pod in `cluster-beta`
- **THEN** the emitted edge has `labels.cluster: "cluster-alpha"`, `source: "cluster-alpha/<client-uid>"`, `target: "cluster-beta/<server-uid>"`, and the cross-cluster status is detectable by comparing the source and target node `labels.cluster` values

#### Scenario: Edge label carries the client pod's identity

- **WHEN** the reader processes a series with `cluster="c1"` and no zone/environment labels whose `client_k8s_pod_uid` resolves to the topology pod `us-dev-c1/<client-uid>`
- **THEN** the emitted edge has `labels.cluster: "us-dev-c1"` and `source: "us-dev-c1/<client-uid>"`; the raw `c1` appears on no element

#### Scenario: Synthesised client resolves the trace label through the ladder

- **WHEN** the reader processes a series with `cluster="c1"` whose `client_k8s_pod_uid` is unknown to topology (a synthesised client pod), and the build's only identity for raw `c1` is `us-dev-c1`
- **THEN** the synthesised pod is `us-dev-c1/<client-uid>` with `labels.cluster: "us-dev-c1"` and the edge carries `labels.cluster: "us-dev-c1"`; when the build holds both `us-dev-c1` and `eu-prod-c1`, the synthesised pod and the edge carry the verbatim `c1` instead and the series is counted as unresolved
