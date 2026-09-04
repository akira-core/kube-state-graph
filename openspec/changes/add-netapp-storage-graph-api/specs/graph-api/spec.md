## ADDED Requirements

### Requirement: Node `alerts` attribute

Each `type="pod"`, `type="node"`, `type="pvc"`, `type="netapp-node"` and `type="netapp-aggr"` node MAY carry a typed `data.alerts` array of `{ name, state, severity }` objects — the active alerts the `alert-overlay` capability matched to it — serialised with `omitempty` semantics and **never inside `labels`** (the `ipaddress` / `owner` / `ready_status` precedent). `severity` is omitted from an entry when empty. The array SHALL be sorted by `name` then `severity` and de-duplicated on that pair. `service`, `external`, `netapp-svm` and every synthesised group node SHALL NOT carry `alerts`. The attribute SHALL appear on `GET /v1/graph` and `GET /v1/storage-graph` alike, since it is resolved onto the built graph.

#### Scenario: Alerted pod serialised

- **WHEN** the build attached alerts `KubePodCrashLooping` (`warning`) and `HighMemory` (`critical`) to a pod
- **THEN** the pod's `data.alerts` equals `[{"name":"HighMemory","state":"firing","severity":"critical"},{"name":"KubePodCrashLooping","state":"firing","severity":"warning"}]` and `labels` carries no alert key

#### Scenario: Unalerted node has no key

- **WHEN** a node matched no alert
- **THEN** its `data` object has no `alerts` key, so a body with no alerts is byte-identical to the pre-change golden

### Requirement: NetApp node `hardware` and `perf` attributes

Each `type="netapp-node"` node MAY carry two typed, nullable objects, both `omitempty` and never inside `labels`: `data.hardware = { model, serial, version, vendor, location }` (strings, each omitted when unresolved) from the Harvest `node_labels` info series, and `data.perf = { cpu_busy_pct, total_ops, total_latency_us, total_bytes_per_sec }` (JSON numbers rounded to 6 significant digits, each omitted when unresolved) from the Harvest `system_node` counters — per the `netapp-storage-graph` "NetApp node entity and health" requirement. No other node type SHALL carry either. `data.health` on the same node SHALL remain the `node_new_status`-reported value and SHALL NOT be derived from `perf`. Both attributes SHALL appear on `GET /v1/graph` and `GET /v1/storage-graph` alike.

#### Scenario: Hardware and perf serialised

- **WHEN** a controller resolves `model="AFF-A400"`, `version="9.14.1"`, `cpu_busy_pct=72.5` and `total_ops=18500`
- **THEN** its `data.hardware` equals `{"model":"AFF-A400","version":"9.14.1"}` and its `data.perf` equals `{"cpu_busy_pct":72.5,"total_ops":18500}`

#### Scenario: Absent attributes omitted

- **WHEN** neither `node_labels` nor any `system_node` counter matches a controller
- **THEN** its `data` has neither a `hardware` nor a `perf` key

## MODIFIED Requirements

### Requirement: Edge-type discovery endpoint

The server SHALL expose `GET /v1/edge-types` that returns the static catalogue of edge types this server can produce. The response SHALL list at least `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`, `pod-to-node`, `pvc-to-netapp-aggr`, and `storage-flow`. Each catalogue entry SHALL describe `source_type` (one of `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`, `"netapp-aggr"`, `"netapp-node"`, `"netapp-svm"`, **or a JSON array of such strings** when more than one is permitted), `target_type` (same form as `source_type`), `directed`, `may_cross_cluster`, and a `labels` array enumerating the keys this edge type can emit on edge `labels`. The `pod-calls-pod` and `pod-calls-service` entries SHALL enumerate a `relation` label (`value_type: "string"`; emitted values `"link"` / `"transport"`, absent on ordinary edges); the `service-selects-pod` entry SHALL NOT. The `storage-flow` entry SHALL enumerate a `tier` label (`value_type: "string"`; emitted values `node-aggr`, `aggr-svm`, `svm-pvc`, `pvc-pod`, `pod-node`) and an `attribution` label (`value_type: "string"`; emitted value `"split"`, absent otherwise). The endpoint SHALL NOT issue any upstream calls and SHALL NOT depend on time-range or cluster parameters. The response SHALL include a long `Cache-Control: public, max-age=3600` header.

#### Scenario: Static catalogue

- **WHEN** a client sends `GET /v1/edge-types`
- **THEN** the response body contains an `edge_types` array including objects whose `type` values include `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`, `pod-to-node`, `pvc-to-netapp-aggr`, and `storage-flow`, and no `pvc-to-storageclass` entry

#### Scenario: pod-calls-pod marked may_cross_cluster

- **WHEN** a client inspects the catalogue entry for `pod-calls-pod`
- **THEN** its `may_cross_cluster` field is `true`, its `source_type` and `target_type` are arrays containing `"pod"` and `"external"`, and its `labels` array enumerates an entry whose `name` is `cluster` with `value_type: "string"` (representing the trace source cluster; cross-cluster status is detected by comparing the source/target nodes' `labels.cluster` rather than from edge labels) and an entry whose `name` is `relation` with `value_type: "string"` (the span-link relation marker — `"link"` for a logical producer→consumer edge, `"transport"` for a pod→broker network hop, absent otherwise)

#### Scenario: pod-calls-service catalogue entry

- **WHEN** a client inspects the catalogue entry for `pod-calls-service`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `true` (a `"://"` connection string resolves to a service node in the caller's OWN cluster, but the Istio route-resolution engine anchors on the selected ingress cluster, which may be a family sibling of the caller's), its `source_type` is an array containing `"pod"` and `"external"`, its `target_type` is `"service"` (or `["service"]`), and its `labels` array enumerates an entry whose `name` is `cluster` with `value_type: "string"` (omitted when the client side is non-pod) and an entry whose `name` is `relation` with `value_type: "string"` (same semantics as on `pod-calls-pod`)

#### Scenario: service-selects-pod catalogue entry

- **WHEN** a client inspects the catalogue entry for `service-selects-pod`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `true` (a local service node fans out to backing pods across same-family clusters holding the same-named Service, so the edge may connect a service to a pod in a different cluster of the caller's family), its `source_type` is `["service"]` (or `"service"`), its `target_type` is `["pod"]` (or `"pod"`), and its `labels` array does NOT enumerate a `relation` entry (a shared fan-out edge is never relation-marked)

#### Scenario: pod-to-node catalogue entry

- **WHEN** a client inspects the catalogue entry for `pod-to-node`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false` (a pod and its scheduled node are always in the same cluster), its `source_type` is `["pod"]` (or `"pod"`), and its `target_type` is `["node"]` (or `"node"`)

#### Scenario: pvc-to-storageclass catalogue entry

- **WHEN** a client inspects the catalogue for a `pvc-to-storageclass` entry
- **THEN** no such entry exists — the edge type is removed from the registry and replaced by `pvc-to-netapp-aggr`

#### Scenario: pvc-to-netapp-aggr catalogue entry

- **WHEN** a client inspects the catalogue entry for `pvc-to-netapp-aggr`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false` (the target NetApp aggregate belongs to no Kubernetes cluster, so the Kubernetes cross-cluster notion does not apply), its `source_type` is `["pvc"]` (or `"pvc"`), its `target_type` is `["netapp-aggr"]` (or `"netapp-aggr"`), and its `labels` array is empty

#### Scenario: storage-flow catalogue entry

- **WHEN** a client inspects the catalogue entry for `storage-flow`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false`, its `source_type` is `["netapp-node", "netapp-aggr", "netapp-svm", "pvc", "pod"]`, its `target_type` is `["netapp-aggr", "netapp-svm", "pvc", "pod", "node"]`, and its `labels` array enumerates `tier` and `attribution`, both `value_type: "string"`

#### Scenario: storage-flow accepted as an edge_type value

- **WHEN** a client sends `GET /v1/graph?...&edge_type=storage-flow`
- **THEN** the server returns 200 (the value is registered) with a body containing no edges, since `/v1/graph` never emits that type

### Requirement: Cytoscape compound node grouping

`GET /v1/graph` and `GET /v1/storage-graph` SHALL express a workload compound hierarchy `cluster > namespace > application > controller > pod` (with **skip-absent-levels**), plus `cluster > namespace > { service, pvc }`, `cluster > node`, and the storage chain `storage-cluster > netapp-node > netapp-aggr` and `storage-cluster > netapp-svm`, via an optional `data.parent` field. The `cluster`, `storage-cluster`, `namespace`, `application`, and `controller` group nodes are synthesised by the Cytoscape serialiser; this is a presentation concern that SHALL NOT affect the core graph, projection, or traversal.

In the storage chain, the middle tier is NOT a synthesised group: the **real** `type="netapp-node"` node acts as the compound parent of its `netapp-aggr` nodes. This is a deliberate, explicitly-scoped break from the "relationships are edges, compound parents are synthesised groups" rule (the rule that removed pod-under-node nesting): it applies to the `netapp-node > netapp-aggr` tier ONLY, and no other real node type SHALL acquire compound children. An SVM spans aggregates and controllers, so `netapp-svm` nests directly under its `storage-cluster` group and is never a compound child of a controller nor a compound parent of anything.

**Cluster group node.** For each distinct `labels.cluster` value present on an emitted node, the serialiser SHALL emit one `{ data: { id: "cluster/<cluster>", name: "<cluster>", type: "cluster", labels: {} } }` with no `parent` and no `ipaddress`.

**Storage-cluster group node.** For each distinct `labels.ontap_cluster` value present on an emitted `netapp-aggr`, `netapp-node` or `netapp-svm` node, the serialiser SHALL emit one `{ data: { id: "storage-cluster/<ontap-cluster>", name: "<ontap-cluster>", type: "storage-cluster", labels: {} } }` with no `parent` and no `ipaddress`, so the NetApp node set nests under its own filer rather than floating parentless like `external` nodes. A storage-cluster group is NOT a Kubernetes cluster group; its name never appears in `clusters[]`.

**Synthesised workload group nodes.** Derived from each emitted real node's own attributes: `namespace` and `application` groups from any `type="pod"`, `type="service"`, or `type="pvc"` node (via its `labels.namespace` and its resolved `data.application`); `controller` groups from `type="pod"` nodes only (via the pod's resolved `data.owner` `{kind, name}`). Each group node's `id` encodes its full ancestry path, so it has exactly one parent (the tree is unambiguous by construction and no `data.parent` can dangle):

- **namespace** — `{ id: "<cluster>/namespace/<ns>", name: "<ns>", type: "namespace", labels: {}, parent: "cluster/<cluster>" }`
- **application** (emitted for any pod, service, or PVC with a resolved Application) — `{ id: "<cluster>/namespace/<ns>/application/<app>", name: "<app>", type: "application", labels: {}, parent: "<cluster>/namespace/<ns>" }`
- **controller** (emitted only for pods with a resolved owner) — `{ id: "<cluster>/namespace/<ns>/application/<app>/controller/<kind>/<name>", ... , parent: <the application group id> }` when the pod also has a resolved Application, otherwise `{ id: "<cluster>/namespace/<ns>/controller/<kind>/<name>", ... , parent: "<cluster>/namespace/<ns>" }`. `name` is `<name>`, `type` is `"controller"`, `labels` is `{}`.

All synthesised group nodes carry `labels: {}` and no `ipaddress`, and SHALL be emitted in tier order (cluster, then storage-cluster, then namespace, then application, then controller), each tier ordered by `id`, before the non-group nodes, so the body stays byte-deterministic.

**`data.parent` assignment** (skip-absent-levels):

- `type="pod"` → its controller group id when the pod has a resolved owner; else its application group id when it has a resolved Application; else its namespace group id `<cluster>/namespace/<ns>`. (Every pod with a namespace always has at least the namespace group.)
- `type="service"`, `type="pvc"` → its application group id `<cluster>/namespace/<ns>/application/<app>` when it has a resolved Application; else its namespace group id `<cluster>/namespace/<ns>` (skip-absent-levels). Services and PVCs SHALL NOT nest under a `controller` group.
- `type="node"` → `cluster/<labels.cluster>`.
- `type="netapp-node"` → `storage-cluster/<labels.ontap_cluster>`.
- `type="netapp-aggr"` → the **real** NetApp node id `netapp/<labels.ontap_cluster>/<labels.node>` (the aggregate's current owner; an HA takeover moves the parent, never the aggregate's id).
- `type="netapp-svm"` → `storage-cluster/<labels.ontap_cluster>`.
- `type="external"` → omitted (no cluster identity).

The `parent` field SHALL use `omitempty` semantics. In `GET /v1/graph` the pod→node and pvc→aggregate relationships SHALL be expressed as the edges `pod-to-node` and `pvc-to-netapp-aggr`, and in `GET /v1/storage-graph` every tier relationship as a `storage-flow` edge — **not** as compound nesting; K8s `node`, `netapp-aggr` and `netapp-svm` nodes therefore carry edges and act as infrastructure-level nodes and edge endpoints (the `netapp-node` is a compound parent and, in `/v1/graph`, the target of no edge). Services and PVCs SHALL NOT be compound parents of pods.

This requirement **supersedes** the prior `storageclass` grouping and node behaviour: there is no `type="storageclass"` node (real or synthesised) and no `pvc-to-storageclass` edge in any form; the claim's StorageClass name survives only as the PVC's own `data.storageclass` attribute.

#### Scenario: Cluster group node synthesised

- **WHEN** the graph contains any node with `labels.cluster="cluster-alpha"`
- **THEN** the Cytoscape response contains a node `{ data: { id: "cluster/cluster-alpha", name: "cluster-alpha", type: "cluster", labels: {} } }` with no `parent` field

#### Scenario: Storage-cluster group node synthesised

- **WHEN** the response contains a `netapp-node` node with `labels.ontap_cluster="ontap-prod"`
- **THEN** the response contains a node `{ data: { id: "storage-cluster/ontap-prod", name: "ontap-prod", type: "storage-cluster", labels: {} } }` with no `parent` field, and the `netapp-node` node's `data.parent` equals `storage-cluster/ontap-prod`

#### Scenario: Aggregate nests under its real owning node

- **WHEN** the response contains a `netapp-aggr` node with `labels={ontap_cluster:"ontap-prod", node:"ontap-prod-01"}`
- **THEN** its `data.parent` equals `netapp/ontap-prod/ontap-prod-01` — the id of the **real** `netapp-node` node, which is present in the response and itself carries `data.parent="storage-cluster/ontap-prod"`

#### Scenario: SVM nests under the storage cluster

- **WHEN** a storage-graph response contains a `netapp-svm` node with `labels.ontap_cluster="ontap-prod"`
- **THEN** its `data.parent` equals `storage-cluster/ontap-prod`, the storage-cluster group is present even if no `netapp-node` is, and no node names the SVM as its `parent`

#### Scenario: Full workload hierarchy nesting

- **WHEN** a pod in `cluster-alpha` namespace `shop` carries `data.application="checkout"` and `data.owner={kind:"Deployment", name:"checkout"}`
- **THEN** the response contains group nodes with ids `cluster-alpha/namespace/shop` (parent `cluster/cluster-alpha`), `cluster-alpha/namespace/shop/application/checkout` (parent `cluster-alpha/namespace/shop`), and `cluster-alpha/namespace/shop/application/checkout/controller/Deployment/checkout` (parent `cluster-alpha/namespace/shop/application/checkout`), and the pod's `data.parent` equals `cluster-alpha/namespace/shop/application/checkout/controller/Deployment/checkout`

#### Scenario: Skip-absent-levels — controller but no application

- **WHEN** a pod in `cluster-alpha` namespace `shop` carries `data.owner={kind:"DaemonSet", name:"fluentd"}` and has no resolved Application
- **THEN** the pod's `data.parent` equals `cluster-alpha/namespace/shop/controller/DaemonSet/fluentd`, whose `data.parent` is `cluster-alpha/namespace/shop`; no `application` group node is synthesised for it

#### Scenario: Skip-absent-levels — neither application nor controller

- **WHEN** a pod in `cluster-alpha` namespace `shop` has neither a resolved Application nor a resolved owner
- **THEN** the pod's `data.parent` equals `cluster-alpha/namespace/shop` (the namespace group)

#### Scenario: Service and PVC nested under namespace when no Application

- **WHEN** the response contains a `type="service"` node and a `type="pvc"` node in `cluster-alpha` namespace `shop`, neither with a resolved ArgoCD Application
- **THEN** each carries `data.parent="cluster-alpha/namespace/shop"`, and neither is the `parent` of any pod

#### Scenario: Service and PVC nested under application when resolved

- **WHEN** the response contains a `type="service"` node and a `type="pvc"` node in `cluster-alpha` namespace `shop`, each with `data.application="checkout"`
- **THEN** each carries `data.parent="cluster-alpha/namespace/shop/application/checkout"`, the `cluster-alpha/namespace/shop/application/checkout` application group node (parent `cluster-alpha/namespace/shop`) is synthesised, and neither the service nor the PVC nests under a `controller` group

#### Scenario: Application group synthesised from a service/PVC even with no pod in it

- **WHEN** the only node carrying `data.application="checkout"` in `cluster-alpha` namespace `shop` is a `type="service"` node (no pod resolves that Application)
- **THEN** the response still contains the `cluster-alpha/namespace/shop/application/checkout` application group node with `parent="cluster-alpha/namespace/shop"`, and no `controller` group is synthesised under it

#### Scenario: Node and StorageClass nested under cluster

- **WHEN** the response contains a `type="node"` node in `cluster-alpha`
- **THEN** it carries `data.parent="cluster/cluster-alpha"`; no `type="storageclass"` node exists to nest (the type is removed — NetApp nodes nest under their `storage-cluster` group instead)

#### Scenario: pod→node and pvc→storageclass are edges, not nesting

- **WHEN** the response contains a scheduled pod and a PVC with a resolved StorageClass name
- **THEN** the pod→node relationship appears as a `pod-to-node` edge (not via `data.parent`), while the former pvc→storageclass relationship no longer exists in any form — no edge, no node, no nesting; the name survives only as the PVC's `data.storageclass` attribute

#### Scenario: pod→node and pvc→netapp-aggr are edges, not nesting

- **WHEN** the response contains a scheduled pod and a PVC whose claim joined a NetApp aggregate
- **THEN** the pod→node relationship appears as a `pod-to-node` edge (not via `data.parent`) and the pvc→aggregate relationship appears as a `pvc-to-netapp-aggr` edge (not via `data.parent`); the K8s `node` and the `netapp-aggr` are NOT compound parents of the pod / PVC — the only real-node compound parenting is the `netapp-node > netapp-aggr` tier

#### Scenario: external nodes have no parent

- **WHEN** the response contains a `type="external"` node
- **THEN** that node's `data` has no `parent` field, and no cluster group node is synthesised for an endpoint carrying no `cluster` label
