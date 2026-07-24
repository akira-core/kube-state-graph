## MODIFIED Requirements

### Requirement: Edge-type discovery endpoint

The server SHALL expose `GET /v1/edge-types` that returns the static catalogue of edge types this server can produce. The response SHALL list at least `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`, `pod-to-node`, and `pvc-to-storageclass`. Each catalogue entry SHALL describe `source_type` (one of `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`, `"storageclass"`, **or a JSON array of such strings** when more than one is permitted), `target_type` (same form as `source_type`), `directed`, `may_cross_cluster`, and a `labels` array enumerating the keys this edge type can emit on edge `labels`. The endpoint SHALL NOT issue any upstream calls and SHALL NOT depend on time-range or cluster parameters. The response SHALL include a long `Cache-Control: public, max-age=3600` header.

#### Scenario: Static catalogue

- **WHEN** a client sends `GET /v1/edge-types`
- **THEN** the response body contains an `edge_types` array including objects whose `type` values include `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`, `pod-to-node`, and `pvc-to-storageclass`

#### Scenario: pod-calls-pod marked may_cross_cluster

- **WHEN** a client inspects the catalogue entry for `pod-calls-pod`
- **THEN** its `may_cross_cluster` field is `true`, its `source_type` and `target_type` are arrays containing `"pod"` and `"external"`, and its `labels` array enumerates an entry whose `name` is `cluster` with `value_type: "string"` (representing the trace source cluster; cross-cluster status is detected by comparing the source/target nodes' `labels.cluster` rather than from edge labels)

#### Scenario: pod-calls-service catalogue entry

- **WHEN** a client inspects the catalogue entry for `pod-calls-service`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `true` (a `"://"` connection string resolves to a service node in the caller's OWN cluster, but the Istio route-resolution engine anchors on the selected ingress cluster, which may be a family sibling of the caller's), its `source_type` is an array containing `"pod"` and `"external"`, its `target_type` is `"service"` (or `["service"]`), and its `labels` array enumerates an entry whose `name` is `cluster` with `value_type: "string"` (omitted when the client side is non-pod)

#### Scenario: service-selects-pod catalogue entry

- **WHEN** a client inspects the catalogue entry for `service-selects-pod`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `true` (a local service node fans out to backing pods across same-family clusters holding the same-named Service, so the edge may connect a service to a pod in a different cluster of the caller's family), its `source_type` is `["service"]` (or `"service"`), and its `target_type` is `["pod"]` (or `"pod"`)

#### Scenario: pod-to-node catalogue entry

- **WHEN** a client inspects the catalogue entry for `pod-to-node`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false` (a pod and its scheduled node are always in the same cluster), its `source_type` is `["pod"]` (or `"pod"`), and its `target_type` is `["node"]` (or `"node"`)

#### Scenario: pvc-to-storageclass catalogue entry

- **WHEN** a client inspects the catalogue entry for `pvc-to-storageclass`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false` (a PVC and its StorageClass are always in the same cluster), its `source_type` is `["pvc"]` (or `"pvc"`), and its `target_type` is `["storageclass"]` (or `"storageclass"`)
