# graph-api — delta for node-internal-ip-fallback

## MODIFIED Requirements

### Requirement: Node `ipaddress` attribute

Every `data` object for a node in the Cytoscape response SHALL expose a top-level `ipaddress` field of type `string[]` with `omitempty` semantics:

- `type="pod"` nodes SHALL carry the pod's IP from `kube_pod_info.pod_ip` (single-element slice) when the source metric surfaces it, and omit the field otherwise.
- `type="node"` nodes SHALL carry the K8s node's `ExternalIP` from `kube_node_status_addresses` (single-element slice) when present, falling back to the node's `InternalIP` (single-element slice) when no ExternalIP row exists, and omit the field only when neither address type is present. An ExternalIP SHALL always win over an InternalIP regardless of upstream sample order.
- `type="service"` nodes SHALL carry the service's `cluster_ip` from `kube_service_info` (single-element slice) when `cluster_ip` is not `"None"`, and omit the field for headless services (`cluster_ip="None"`) or when the metric does not surface it.
- `type="pvc"` and `type="external"` nodes SHALL NOT emit the `ipaddress` field.

The legacy `labels.pod_ip`, `labels.host_ip`, and `labels.external_ip` keys SHALL NOT appear on any node entry — they are replaced by the typed `ipaddress` attribute and the node entry respectively. A `labels.internal_ip` key SHALL NOT appear either — the InternalIP fallback surfaces only via `ipaddress`.

#### Scenario: Pod entry carries pod IP on ipaddress

- **WHEN** `kube_pod_info` exposes `pod_ip="10.244.0.10"` for a pod
- **THEN** the corresponding `type="pod"` node carries `data.ipaddress: ["10.244.0.10"]` and neither `data.labels.pod_ip` nor `data.labels.host_ip` is present

#### Scenario: Node entry carries ExternalIP on ipaddress

- **WHEN** `kube_node_status_addresses{type="ExternalIP",address="203.0.113.10"}` is present for a K8s node
- **THEN** the corresponding `type="node"` entry carries `data.ipaddress: ["203.0.113.10"]` and `data.labels.external_ip` is not present

#### Scenario: Node entry falls back to InternalIP on ipaddress

- **WHEN** a K8s node has no `kube_node_status_addresses{type="ExternalIP"}` row but `kube_node_status_addresses{type="InternalIP",address="10.0.0.7"}` is present
- **THEN** the corresponding `type="node"` entry carries `data.ipaddress: ["10.0.0.7"]` and neither `data.labels.internal_ip` nor `data.labels.external_ip` is present

#### Scenario: ExternalIP preferred over InternalIP

- **WHEN** a K8s node has both an `ExternalIP` row (`address="203.0.113.10"`) and an `InternalIP` row (`address="10.0.0.7"`) in `kube_node_status_addresses`
- **THEN** the corresponding `type="node"` entry carries `data.ipaddress: ["203.0.113.10"]`

#### Scenario: Service entry carries cluster IP on ipaddress

- **WHEN** `kube_service_info` exposes `cluster_ip="10.96.0.42"` for a service that a connection-string endpoint resolved to
- **THEN** the corresponding `type="service"` node carries `data.ipaddress: ["10.96.0.42"]`

#### Scenario: Headless service omits ipaddress

- **WHEN** `kube_service_info` exposes `cluster_ip="None"` for a service that a connection-string endpoint resolved to
- **THEN** the corresponding `type="service"` node's `data` object does not include an `ipaddress` field

#### Scenario: ipaddress omitted when source metric does not surface it

- **WHEN** a pod's `kube_pod_info` series omits `pod_ip`, or a K8s node has neither an `ExternalIP` nor an `InternalIP` row in `kube_node_status_addresses`
- **THEN** the corresponding node's `data` object does not include an `ipaddress` field

#### Scenario: PVC and external nodes never carry ipaddress

- **WHEN** the response contains nodes of `type="pvc"` or `type="external"`
- **THEN** those node `data` objects do not include an `ipaddress` field
