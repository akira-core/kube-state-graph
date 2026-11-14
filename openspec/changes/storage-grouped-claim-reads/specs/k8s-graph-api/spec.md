## MODIFIED Requirements

### Requirement: Reads by reference, Kubernetes families only

The build SHALL walk from the root and read every family by reference — each read restricted to the names the root or an earlier read carries — with the seeds' first reads, root-set caps, mounter completion and fail-closed rule `/v1/storage-graph` applies to a `pod` or `node` root, and additionally every pod running on a `node` root. It SHALL NOT issue a Harvest, service-side, container, service-graph or `up` query.

#### Scenario: Node root query set

- **WHEN** a client roots at `node=worker-1` and `shop/web-0` runs there
- **THEN** the first `kube_pod_info` query is restricted to `node="worker-1"`, incarnation completion, the claim-binding read and the `kube_pod_owner` read to `namespace="shop",pod="web-0"`, and no query names a Harvest family, a service-side family, `kube_pod_container_info`, `traces_service_graph_*` or `up`

#### Scenario: Node root reads its claimless pods

- **WHEN** a client roots at `node=worker-1`, and `web-0` runs there mounting no claim
- **THEN** the `kube_pod_owner` read names `web-0` as well as the claim-mounting pods, which a `/v1/storage-graph` request for the same node does not, and `kube_pod_info` is read for `web-0` once, by incarnation completion

#### Scenario: Zone routing and matchers

- **WHEN** a request selects `az=a` and the routing table declares a separate backend for zone `b`
- **THEN** no query reaches zone `b`'s backend, and every query carries the request matchers its family accepts (`ALERTS` takes no `cluster` matcher)

#### Scenario: Output preservation

- **WHEN** the same root, filters and window are projected over a build that read every Kubernetes family unrestricted
- **THEN** the body is byte-identical

#### Scenario: No NetApp coverage warning

- **WHEN** a build tracks claims
- **THEN** it logs neither `netapp_volume_join_miss` nor `netapp_qos_join_miss`

#### Scenario: The storage graph keeps its own reads

- **WHEN** a `/v1/storage-graph` request roots at the same `node` or `pod`
- **THEN** it groups its reads of known pods and claims across namespaces exactly as "Known pods and claims are read across namespaces in one restriction" defines, but issues them in the order the `storage-graph-api` capability defines: none of this capability's ordering, claimless-pod loading or early controller read applies to it, and its body is unchanged
