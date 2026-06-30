## MODIFIED Requirements

### Requirement: Namespace-filter retention of cluster-scoped infra nodes

`GET /v1/graph` projection SHALL treat `type="node"` and `type="storageclass"` nodes as cluster-scoped infrastructure nodes that carry no `namespace` label, and SHALL admit such a node to a response **iff it is referenced by an in-scope element** — a `type="node"` node when some in-scope pod is scheduled on it (its `labels.node`), and a `type="storageclass"` node when some in-scope PVC resolves to it — on **every** request shape (no filter, `?cluster=`, `?namespace=`). The default (no-filter) response therefore lists only the host nodes of pods that are in the graph and the StorageClasses backing in-scope PVCs; it SHALL NOT carry an orphan node that hosts no pod or a StorageClass that backs no PVC. The cluster filter applies to these nodes exactly as to other node types (the node's own `labels.cluster`).

The **one exception** is an explicit `?name=` filter: a `?name=<value>` request SHALL admit a `type="node"` or `type="storageclass"` node whose `Name()` equals `<value>` **even when it is referenced by no in-scope element** (an empty / `NotReady` node, or an unused StorageClass, stays directly queryable). When a `?name=` filter is active and does not name a given infra node, that node SHALL NOT be admitted by this rule; if it is instead the host of a named pod (or backs a named PVC) it re-enters the response as that edge's re-added partner under the unified edge-retention rule, not by this admission rule.

This retention is a node-admission rule of the projection over the freshly built `*Graph`; the build SHALL still load every node and StorageClass (the full-topology graph is built unchanged), so the pruning SHALL NOT alter the core graph, push any filter to PromQL, or change the determinism of the response. A **consequence** of this rule is that a podless node's `ready_status` / `ipaddress` and a PVC-less StorageClass's `provisioner` / `parameters` are absent from the default view and are obtained with `?name=`; there is no exception that keeps an unhealthy (`NotReady` / `Unknown`) podless node in the default view.

#### Scenario: Default view drops a podless node

- **WHEN** the built graph has a node `cluster-alpha/worker-9` on which no pod is scheduled and a client sends `GET /v1/graph` with no filter
- **THEN** the response does not contain `cluster-alpha/worker-9`

#### Scenario: Default view keeps a node hosting an in-graph pod

- **WHEN** a pod is scheduled on node `cluster-alpha/worker-0` and a client sends `GET /v1/graph` with no filter
- **THEN** the response contains `cluster-alpha/worker-0` and the `pod-to-node` edge from the pod to it

#### Scenario: Default view drops a PVC-less StorageClass

- **WHEN** a StorageClass `cluster-alpha/storageclass/unused` is backed by no PVC in the built graph and a client sends `GET /v1/graph` with no filter
- **THEN** the response does not contain `cluster-alpha/storageclass/unused`

#### Scenario: Cluster filter keeps only referenced infra nodes

- **WHEN** `?cluster=cluster-alpha` is sent and `cluster-alpha` has a node `worker-0` hosting a pod and a node `worker-1` hosting nothing
- **THEN** the response contains `cluster-alpha/worker-0` and not `cluster-alpha/worker-1`

#### Scenario: Name filter surfaces an unreferenced infra node

- **WHEN** node `cluster-alpha/worker-9` hosts no pod and a client sends `?name=worker-9`
- **THEN** the response contains `cluster-alpha/worker-9` (with its `ready_status` / `ipaddress` when resolved), admitted by the explicit name match despite being referenced by no in-scope pod

#### Scenario: Name filter on an unused StorageClass surfaces it

- **WHEN** StorageClass `cluster-alpha/storageclass/gp3` backs no in-scope PVC and a client sends `?name=gp3`
- **THEN** the response contains `cluster-alpha/storageclass/gp3` with its `provisioner` / `parameters` when resolved

#### Scenario: StorageClass retained when a filtered-in PVC references it

- **WHEN** the graph has a PVC in namespace `shop` resolving to StorageClass `cluster-alpha/storageclass/gp3` and a client sends `?namespace=shop`
- **THEN** the response contains the `shop` PVC, the `cluster-alpha/storageclass/gp3` node, and the `pvc-to-storageclass` edge between them

#### Scenario: K8s node retained when a filtered-in pod is scheduled on it

- **WHEN** a pod in namespace `shop` is scheduled on node `cluster-alpha/worker-0` and a client sends `?namespace=shop`
- **THEN** the response contains node `cluster-alpha/worker-0` and the `pod-to-node` edge from the pod to it

#### Scenario: Podless NotReady node is hidden by default (no health exception)

- **WHEN** node `cluster-alpha/worker-broken` hosts no pod and its `ready_status` is `NotReady` and a client sends `GET /v1/graph` with no filter
- **THEN** the response does not contain `cluster-alpha/worker-broken` (it is obtained with `?name=worker-broken`)
