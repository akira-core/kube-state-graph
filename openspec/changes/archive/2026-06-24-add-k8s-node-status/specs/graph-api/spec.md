# graph-api — delta for add-k8s-node-status

## ADDED Requirements

### Requirement: Node `ready_status` attribute

Every `data` object for a `type="node"` node in the Cytoscape response SHALL expose a top-level `ready_status` field of type `string` with `omitempty` semantics, carrying the node's Kubernetes Ready-condition status derived from `kube_node_status_condition{condition="Ready"}` (see cluster-topology-source Requirement: K8s node Ready-status attribute):

- The value SHALL be exactly one of `"Ready"`, `"NotReady"`, or `"Unknown"`.
- The field SHALL be omitted entirely when the source metric is absent, when the node has no `condition="Ready"` series, or when no status row is active — a node with no Ready-condition data carries no `ready_status` key, NOT `ready_status: ""` and NOT `ready_status: "Unknown"`.
- The literal `"Unknown"` SHALL appear only for the genuine Kubernetes state where the Ready condition's `status` label is `unknown` (kubelet not reporting); it is never a stand-in for missing data.
- `type="pod"`, `type="service"`, `type="pvc"`, and `type="external"` nodes SHALL NOT emit the `ready_status` field.

The Ready status SHALL NOT appear inside `labels` (which remain a strict `map[string]string` of typological metadata) — it is a typed attribute, the same precedent as `ipaddress` and `owner`.

#### Scenario: Ready node carries ready_status

- **WHEN** a K8s node's active `kube_node_status_condition{condition="Ready"}` series carries `status="true"`
- **THEN** the corresponding `type="node"` entry carries `data.ready_status: "Ready"` and no `ready_status` key in `data.labels`

#### Scenario: NotReady node carries ready_status

- **WHEN** a K8s node's active Ready-condition series carries `status="false"`
- **THEN** the corresponding `type="node"` entry carries `data.ready_status: "NotReady"`

#### Scenario: Unknown is distinct from omitted

- **WHEN** node A's active Ready-condition series carries `status="unknown"` and node B has no `kube_node_status_condition` series at all
- **THEN** node A's entry carries `data.ready_status: "Unknown"` while node B's `data` object does not include a `ready_status` field

#### Scenario: Non-node types never carry ready_status

- **WHEN** the response contains nodes of `type="pod"`, `type="service"`, `type="pvc"`, or `type="external"`
- **THEN** those node `data` objects do not include a `ready_status` field
