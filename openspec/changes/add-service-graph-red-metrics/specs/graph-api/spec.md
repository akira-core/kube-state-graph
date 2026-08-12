## MODIFIED Requirements

### Requirement: Cytoscape.js response shape

`GET /v1/graph` SHALL return a JSON document in Cytoscape.js shape: `{ apiVersion, clusters, elements: { nodes, edges } }`. The body SHALL NOT contain time-varying or echo-of-input fields, so identical inputs against the same upstream state produce byte-identical bodies.

Each **node** SHALL be `{ data: { id, name, type, labels } }`:
- `id` SHALL be a cluster-scoped composite for pods / K8s nodes / PVCs / services / StorageClasses (pods: `<cluster>/<pod-uid>`; nodes: `<cluster>/<node-name>`; PVCs: `<cluster>/<namespace>/<claim>`; services: `<cluster>/<namespace>/<service>`; StorageClasses: `<cluster>/storageclass/<name>`). For external nodes (unresolvable `"://"` connection-string endpoints or missing-UID human-label fallback), `id` SHALL be `external/<label-value>` (no cluster prefix).
- `name` SHALL be the human-readable pod / node / PVC / service / StorageClass name. For external nodes, `name` SHALL be the verbatim `client` or `server` label value from the source service-graph series.
- `type` SHALL be one of the strings `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`, `"storageclass"`. The Cytoscape serialiser additionally synthesises `"cluster"`, `"namespace"`, `"application"`, and `"controller"` group nodes for compound nesting (see "Cytoscape compound node grouping").
- `data` MAY carry an optional `parent` field (`omitempty`) referencing the `id` of the node's Cytoscape compound container — see "Cytoscape compound node grouping".
- `labels` SHALL be a JSON object whose values are strings only (`map[string]string`). For pod / K8s node / PVC / service / StorageClass nodes it SHALL include at minimum a `cluster` entry; for pods, PVCs, and services it SHALL also include a `namespace` entry; for pods it SHALL include `node` (the cluster-scoped node ID), and SHALL include `pod_ip` and `host_ip` whenever the upstream `kube_pod_info` series carried them; for K8s nodes it SHALL include `external_ip` when the upstream provided one. **For external nodes**, `labels` SHALL be an empty object `{}` (no `cluster` key).

Each **edge** SHALL be `{ data: { id, type, source, target, labels } }`:
- `id` SHALL be a UUID, RFC 4122 compliant, encoded as a lowercase canonical string.
- `type` SHALL be one of the registered edge types from `/v1/edge-types`.
- `source` and `target` SHALL each match the `id` of a node present in the same response's `elements.nodes`.
- `labels` SHALL be a JSON object whose values are strings only (`map[string]string`). The exact key set per edge type is defined by the `pod-service-graph` and `cluster-topology-source` capabilities.
- `data` MAY carry an optional `metrics` object (`omitempty`) holding the edge's RED measurements — see "Edge `metrics` attribute".

Implementations SHALL NOT encode booleans or numbers as strings inside `labels`. Boolean flags remain deferred to a future typed field and are NOT part of the v1 contract. Numeric measurements are carried exclusively on the typed `data.metrics` object defined below — never inside `labels`.

#### Scenario: Pod node payload

- **WHEN** the response contains a pod node
- **THEN** its `data.type` equals `"pod"`, its `data.id` matches `<cluster>/<pod-uid>`, its `data.name` equals the pod's metadata name, and `data.labels.cluster` matches the cluster prefix in the ID

#### Scenario: Pod node payload includes pod_ip and host_ip when upstream emits them

- **WHEN** the response contains a pod node whose source `kube_pod_info` series carried `pod_ip` and `host_ip`
- **THEN** `data.labels.pod_ip` equals the upstream `pod_ip` value and `data.labels.host_ip` equals the upstream `host_ip` value

#### Scenario: K8s node payload

- **WHEN** the response contains a Kubernetes-node node
- **THEN** its `data.type` equals `"node"`, its `data.id` matches `<cluster>/<node-name>`, its `data.name` equals the node's metadata name, and `data.labels.external_ip` is present whenever the upstream metric provided one

#### Scenario: PVC node payload

- **WHEN** the response contains a PVC node
- **THEN** its `data.type` equals `"pvc"`, its `data.id` matches `<cluster>/<namespace>/<claim>`, its `data.name` equals the claim name, and `data.labels.namespace` equals the PVC namespace

#### Scenario: PVC node carries no storageclass attribute

- **WHEN** the response contains a PVC node whose StorageClass was resolved from `kube_persistentvolumeclaim_info`
- **THEN** the PVC node's `data` has no `storageclass` field and its `labels` has no `storageclass` key — the StorageClass surfaces only via a `pvc-to-storageclass` edge to the real `type="storageclass"` node (not via `data.parent` and not as a synthesised group node)

#### Scenario: Service node payload

- **WHEN** the response contains a service node (a connection-string endpoint that resolved to an in-cluster service via `kube_service_info`)
- **THEN** its `data.type` equals `"service"`, its `data.id` matches `<cluster>/<namespace>/<service>`, its `data.name` equals the service name, `data.labels.cluster` matches the cluster prefix in the ID, `data.labels.namespace` equals the service namespace, and `data.ipaddress` equals `[cluster_ip]` whenever the upstream `kube_service_info` `cluster_ip` value is not `"None"`

#### Scenario: External node payload (unresolvable connection-string endpoint)

- **WHEN** the response contains an external node produced by an unresolvable `"://"` connection-string endpoint (a `client` or `server` label containing `"://"` whose host did not resolve to an in-cluster service)
- **THEN** its `data.type` equals `"external"`, its `data.id` equals `external/<value>`, its `data.name` equals `<value>` (the verbatim service-graph `client` or `server` label), and `data.labels` equals `{}`

#### Scenario: External node payload (missing-UID fallback)

- **WHEN** the response contains an external node produced by the missing-UID human-label fallback (a service-graph series whose `client_k8s_pod_uid` or `server_k8s_pod_uid` was empty but the corresponding `client`/`server` label was populated and contained no `"://"`)
- **THEN** its `data.type` equals `"external"`, its `data.id` equals `external/<value>`, its `data.name` equals `<value>`, and `data.labels` equals `{}`

#### Scenario: Edge payload references existing nodes

- **WHEN** the response contains any edge
- **THEN** both `data.source` and `data.target` SHALL match the `data.id` of a node present in the same response's `elements.nodes`

#### Scenario: Edge id is a UUID

- **WHEN** the response contains any edge
- **THEN** `data.id` matches the regex `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

#### Scenario: Edge id is stable across rebuilds

- **WHEN** the same logical edge (same `type`, `source`, `target`) is produced by two consecutive builds for the same time bucket
- **THEN** `data.id` is byte-identical between the two builds

#### Scenario: Edge labels never carry numbers

- **WHEN** the response contains an edge that carries a `data.metrics` object
- **THEN** its `data.labels` still contains only string values and no `rate`, `error_rate`, or `p90_server_ms` key

## ADDED Requirements

### Requirement: Edge `metrics` attribute

An edge's `data` MAY carry an optional `metrics` object (`omitempty`) holding the edge's RED measurements for the requested window. The key SHALL be **absent entirely** — never `null`, never an empty object — on every edge that has no measurements, and the presence rule is defined by the `pod-service-graph` capability (in short: only trace-derived `pod-calls-pod` edges whose `source` and `target` were both resolved from a non-empty pod UID).

When present, the object SHALL contain:

- `rate` (number, REQUIRED) — requests per second over the window, strictly greater than zero.
- `error_rate` (number, OPTIONAL, `omitempty` semantics via absence) — the failed fraction in `[0, 1]`. Absent when the upstream failure counter could not be read; `0` when it was read and reported no failures.
- `p90_server_ms` (number, OPTIONAL, absent when unavailable) — the 90th percentile server-observed request duration in milliseconds. The quantile and observation side match Grafana's documented service-graph queries by definition; the values are not expected to equal Grafana's numerically, because Grafana aggregates by service name while this API aggregates by pod pair.

All three SHALL be JSON numbers, never strings. Each value SHALL be rounded to a fixed number of **significant digits** — not decimal places — so that the "Deterministic response body" requirement continues to hold byte-for-byte while a non-zero value can never be rendered as `0`. Consequently a value MAY appear in JSON exponent form (for example `3.86e-7`), which is legal JSON; consumers MUST NOT assume a fixed-decimal rendering, and MUST treat `0` as semantically distinct from a very small non-zero value. The presence or absence of `metrics` SHALL NOT affect the edge's `id`, `type`, `source`, `target`, or `labels`, and SHALL NOT affect node or edge ordering.

#### Scenario: Pod-to-pod edge carries RED metrics

- **WHEN** the response contains a `pod-calls-pod` edge whose `source` and `target` are both pod nodes and whose upstream series carried request, failure, and duration data
- **THEN** its `data.metrics` is an object with numeric `rate`, `error_rate`, and `p90_server_ms` fields

#### Scenario: Edge without measurements omits the key

- **WHEN** the response contains a `service-selects-pod`, `pod-to-node`, `pvc-to-storageclass`, `pod-mounts-pvc`, or `pod-calls-service` edge
- **THEN** its `data` object has no `metrics` key at all (not `null`, not `{}`)

#### Scenario: Partial measurements omit only the missing fields

- **WHEN** a qualifying pod-to-pod edge has request data but the upstream duration histogram is unavailable
- **THEN** its `data.metrics` contains `rate` and `error_rate` but no `p90_server_ms` key

#### Scenario: Metrics are JSON numbers

- **WHEN** any edge carries `data.metrics`
- **THEN** every value inside it is a JSON number, not a string

#### Scenario: Very small values render in exponent form and survive a round-trip

- **WHEN** an edge's `rate` is small enough that its rendering falls below the JSON encoder's fixed-notation threshold
- **THEN** the value is emitted as a JSON number in exponent form, it is not `0`, and parsing the body and re-serialising it reproduces the identical number

#### Scenario: Metrics do not perturb determinism

- **WHEN** two builds run over identical upstream data
- **THEN** both response bodies are byte-identical, including every `data.metrics` value
