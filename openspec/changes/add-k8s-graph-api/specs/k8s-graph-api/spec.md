# Spec Delta

## Purpose

Serves the Kubernetes neighbourhood of a set of pods or nodes — where the pods run, the claims they mount, and their namespace and ArgoCD Application grouping — in the Cytoscape.js body shared with the other graph endpoints, without reading or drawing any NetApp or service-graph data.

## ADDED Requirements

### Requirement: Kubernetes graph endpoint

The server SHALL expose `GET /v1/k8s-graph` returning a graph for a caller-specified `[start, end]` window in the `{ apiVersion, clusters, elements: { nodes, edges } }` Cytoscape.js shape of `GET /v1/graph`. `start` and `end` SHALL be required and validated, and `end` aligned, exactly as for `/v1/graph`, with the same `reason` codes.

#### Scenario: Well-formed request

- **WHEN** a client sends `GET /v1/k8s-graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z&cluster=c1&az=a&env=prod&node=worker-1`
- **THEN** the server returns 200 with a body whose top-level keys are exactly `apiVersion`, `clusters` and `elements`, and `apiVersion` is `"v1"`

#### Scenario: Window validation matches /v1/graph

- **WHEN** a client sends `GET /v1/k8s-graph` with `end` not after `start` and every other parameter valid
- **THEN** the server returns 400 with `reason: "invalid_range"`

### Requirement: Cluster, zone and environment are required

`cluster`, `az` and `env` SHALL each be required with at least one non-empty value and SHALL each be repeatable. A request lacking one SHALL be rejected 400 `missing_cluster`, `missing_az` or `missing_env`, checked in that order after `start` / `end` and before the root. Values SHALL be sorted and de-duplicated, so value order never changes the request.

#### Scenario: Missing cluster

- **WHEN** a client sends `GET /v1/k8s-graph?start=…&end=…&az=a&env=prod&node=worker-1`
- **THEN** the server returns 400 with `reason: "missing_cluster"` and issues no upstream query

#### Scenario: Empty value does not satisfy the requirement

- **WHEN** a client sends `cluster=` with every other parameter valid
- **THEN** the server returns 400 with `reason: "missing_cluster"`

#### Scenario: Value order is irrelevant

- **WHEN** two requests differ only in sending `cluster=c2&cluster=c1` versus `cluster=c1&cluster=c2`
- **THEN** both issue identical upstream queries and return byte-identical bodies

### Requirement: Selection is the product of cluster, zone and environment

The request SHALL select every cluster identity `<az>-<env>-<cluster>` whose components are each among the request's values. `cluster` SHALL be the RAW cluster name, as on `/v1/graph`; a composed identity sent as `cluster` SHALL select nothing. Selector values SHALL be validated like a `/v1/graph` selector value, else 400 `invalid_scope`.

#### Scenario: Several values form a product

- **WHEN** a client sends `az=a&az=b&env=prod&cluster=c1&node=worker-1` and both zones run a cluster `c1` with a node `worker-1`
- **THEN** `clusters` is `["a-prod-c1", "b-prod-c1"]` and both nodes are drawn, each under its own `cluster` group

#### Scenario: Identity sent as cluster

- **WHEN** a client sends `cluster=a-prod-c1&az=a&env=prod&node=worker-1`
- **THEN** the server returns 200 with empty `clusters` and no elements

#### Scenario: Control character in a value

- **WHEN** a client sends a `cluster` value containing a control character
- **THEN** the server returns 400 with `reason: "invalid_scope"`

### Requirement: Exactly one root kind, pod or node

A request SHALL carry exactly one root kind: `pod` or `node`, each repeatable with values OR-combined. Neither is 400 `missing_root`; both is 400 `invalid_scope` naming the two. A `pod` value SHALL be `<namespace>/<name>` with two non-empty segments, else 400 `invalid_scope`. A `node` value names a Kubernetes node. Every root value SHALL be validated like a selector value.

#### Scenario: No root

- **WHEN** a client sends `cluster`, `az` and `env` with no `pod` or `node`
- **THEN** the server returns 400 with `reason: "missing_root"` and issues no upstream query

#### Scenario: Both kinds

- **WHEN** a client sends `pod=shop/web-0&node=worker-1`
- **THEN** the server returns 400 with `reason: "invalid_scope"` and a message naming `pod` and `node`

#### Scenario: Malformed pod ref

- **WHEN** a client sends `pod=web-0`
- **THEN** the server returns 400 with `reason: "invalid_scope"`

#### Scenario: Storage-graph root kind is not a root here

- **WHEN** a client sends `aggr=aggr1` as its only root-like parameter
- **THEN** the server returns 400 with `reason: "missing_root"`

### Requirement: Bounded root sets

When a root kind's first read would take more queries than the `/v1/storage-graph` cap for the same kind under the shared byte budget, the request SHALL be rejected 400 `invalid_scope` before any upstream query, and the build SHALL NOT fall back to reading the family across the selected clusters.

#### Scenario: Too many node roots

- **WHEN** a client sends enough `node=` values that the first `kube_pod_info` read exceeds the cap
- **THEN** the server returns 400 with `reason: "invalid_scope"` and issues no upstream query

### Requirement: Optional namespace filter

`namespace` SHALL be optional and repeatable, OR-combined, and validated like a selector value. It SHALL narrow drawn pods and claims to the listed namespaces. Every other parameter, `prune` included, SHALL be ignored without error.

#### Scenario: Namespace narrows a node root

- **WHEN** a request carries `node=worker-1&namespace=shop` and `worker-1` runs pods of `shop` and `platform`
- **THEN** the body carries `worker-1`, the `shop` pods and their claims, and no `platform` pod or claim

#### Scenario: prune is ignored

- **WHEN** two requests differ only in that one carries `prune=false`
- **THEN** both bodies are byte-identical

### Requirement: Only what a root reaches is drawn

A pod SHALL be drawn iff it is a `pod` root or its node is a `node` root, and it passes the `cluster` / `namespace` filters. A claim SHALL be drawn iff a drawn pod mounts it. A Kubernetes node SHALL be drawn iff a drawn pod runs on it or it is a `node` root. Nothing else SHALL be drawn; an unmounted claim never is.

#### Scenario: Pod root

- **WHEN** a request roots at `pod=shop/web-0`, which runs on `worker-1` and mounts `shop/data`, and `worker-1` also runs `shop/api-0`
- **THEN** the body carries `web-0`, `shop/data` and `worker-1`, the `pod-mounts-pvc` and `pod-to-node` edges of `web-0`, and not `api-0`

#### Scenario: Node root

- **WHEN** a request roots at `node=worker-1`, which runs `web-0` (mounting `shop/data`) and `job-0` (mounting nothing)
- **THEN** the body carries `worker-1`, both pods, `shop/data`, and every `pod-to-node` and `pod-mounts-pvc` edge among them

#### Scenario: Shared claim does not pull in its other mounters

- **WHEN** a request roots at `node=worker-1`, `orders-0` on `worker-1` mounts RWX claim `shop/shared`, and `report-0` on `worker-2` mounts it too
- **THEN** the body carries `orders-0`, `shop/shared` and `worker-1`, and not `report-0` or `worker-2`

#### Scenario: Pod root on another node's pods

- **WHEN** a request roots at `pod=shop/web-0` and `pod=shop/db-0`, running on `worker-1` and `worker-2`
- **THEN** both nodes are drawn and each carries only its root pod

### Requirement: Roots are drawn when the upstream names them

A root that a series read in the window names SHALL be drawn even when it reaches nothing else: an unscheduled `pod` root with no `pod-to-node` edge, a `node` root no pod runs on. A root no series names SHALL simply be absent, never an error and never a placeholder.

#### Scenario: Podless node root

- **WHEN** a request roots at `node=worker-9`, which `kube_node_info` names and no pod runs on
- **THEN** the body carries `worker-9` alone under its `cluster` group

#### Scenario: Unknown root

- **WHEN** a request roots at `pod=shop/nope` and no series names it
- **THEN** the server returns 200 with empty `nodes` and `edges`

### Requirement: Kubernetes-only element set

The body SHALL carry only real nodes of type `pod`, `node` and `pvc`, edges of type `pod-to-node` and `pod-mounts-pvc`, and the synthesised `cluster`, `namespace`, `application` and `controller` groups. It SHALL NOT carry `service`, `external`, `netapp-node`, `netapp-aggr` or `netapp-svm` nodes, `storage-cluster` groups, or any other edge type.

#### Scenario: NetApp estate present upstream

- **WHEN** the upstream holds Harvest series for a drawn claim's volume
- **THEN** the body carries no NetApp node, no `storage-cluster` group and no `pvc-to-netapp-aggr` edge

#### Scenario: Service graph present upstream

- **WHEN** the upstream holds `traces_service_graph_request_total` series between drawn pods
- **THEN** the body carries no `pod-calls-pod`, `pod-calls-service` or `service-selects-pod` edge and no `service` or `external` node

### Requirement: Kubernetes attributes and grouping match the storage graph

Each drawn pod, node and claim SHALL carry the `data` attributes it carries in `GET /v1/storage-graph` for the same root, and the body SHALL include the same compound groups and group metadata for them. `data.containers`, `data.qos` and the PVC labels `aggr` and `svm` SHALL be absent.

#### Scenario: Pod Application from its controller

- **WHEN** a drawn pod's Deployment carries an ArgoCD tracking-id `shop:apps/Deployment:shop/orders`
- **THEN** the pod carries `data.owner` `{kind: "Deployment", name: "orders"}` and `data.application` `"shop"`, and its `data.parent` is the `controller` group under the `shop` `application` group under its `namespace` group

#### Scenario: Job pod resolves through its CronJob

- **WHEN** a drawn pod's Job carries no tracking-id and the Job's controlling CronJob does
- **THEN** the pod's `data.application` is the CronJob's Application and its `data.owner` names the Job

#### Scenario: Claim inherits from every mounter, drawn or not

- **WHEN** a drawn claim carries no tracking-id, its drawn mounter resolves Application `shop`, and an undrawn mounter on another node resolves `billing`
- **THEN** the claim carries `data.application` `"billing"` — the lexically-smallest over all its mounters — exactly as `/v1/graph` and `/v1/storage-graph` resolve it

#### Scenario: Application-metadata metric configured

- **WHEN** the application-metadata metric is configured and served, and resolves an owner for drawn namespace `shop` and a deployer for drawn Application `shop`
- **THEN** the `shop` `namespace` group carries `namespace_owner` and the `shop` `application` group carries `deployed_by`

#### Scenario: Node and claim attributes

- **WHEN** a drawn node reports `Ready` and a drawn claim has kubelet usage and a StorageClass
- **THEN** the node carries `data.ready_status` `"Ready"` and `data.ipaddress`, and the claim carries `data.usage` and `data.storageclass`

#### Scenario: Status and alerts

- **WHEN** a firing alert matches a drawn pod
- **THEN** the pod carries `data.alerts` and a `data.status` folded as on `/v1/storage-graph`, and every drawn pod, node and claim carries `data.status`

### Requirement: Reads by reference, Kubernetes families only

The build SHALL walk from the root and read every family by reference as `/v1/storage-graph` does for a `pod` or `node` root, and additionally every pod running on a `node` root. It SHALL NOT issue a Harvest, `kube_service_info`, `kube_endpointslice_*`, `kube_service_annotations`, `kube_pod_container_info`, `traces_service_graph_*` or `up` query.

#### Scenario: Node root query set

- **WHEN** a client roots at `node=worker-1` and `shop/web-0` runs there
- **THEN** the first `kube_pod_info` query is restricted to `node="worker-1"`, incarnation completion and the claim-binding read to `namespace="shop",pod="web-0"`, and no query names a Harvest family, a service-side family, `kube_pod_container_info`, `traces_service_graph_*` or `up`

#### Scenario: Node root reads its claimless pods

- **WHEN** a client roots at `node=worker-1`, and `web-0` runs there mounting no claim
- **THEN** the pod-scoped reads (`kube_pod_info`, `kube_pod_owner`) name `web-0` as well as the claim-mounting pods, which a `/v1/storage-graph` request for the same node does not

#### Scenario: Zone routing and matchers

- **WHEN** a request selects `az=a` and the routing table declares a separate backend for zone `b`
- **THEN** no query reaches zone `b`'s backend, and every query carries the request matchers its family accepts (`ALERTS` takes no `cluster` matcher)

#### Scenario: Output preservation

- **WHEN** the same root, filters and window are projected over a build that read every Kubernetes family unrestricted
- **THEN** the body is byte-identical

#### Scenario: No NetApp coverage warning

- **WHEN** a build tracks claims
- **THEN** it logs neither `netapp_volume_join_miss` nor `netapp_qos_join_miss`

### Requirement: Fails closed on any upstream error except ALERTS

A `/v1/k8s-graph` build SHALL fail when any upstream query it issues returns an error — any chunk, wave or backend, kubelet, controller-annotation and application-metadata families included — with 502 and the message `upstream query failed: <family>`. An `ALERTS` error SHALL degrade to an alert-less body. An empty vector SHALL NOT fail the build.

#### Scenario: Kubelet store error

- **WHEN** the `kubelet_volume_stats_used_bytes` query returns an upstream error
- **THEN** the request returns 502 with message `upstream query failed: kubelet_volume_stats_used_bytes`

#### Scenario: Annotation family error

- **WHEN** a `kube_replicaset_annotations` chunk returns an upstream error
- **THEN** the request returns 502 naming that family

#### Scenario: ALERTS error

- **WHEN** only the `ALERTS` query returns an upstream error
- **THEN** the request returns 200 and no node carries `data.alerts`

### Requirement: Empty result is an empty 200

A request whose root reaches nothing SHALL return 200 with empty `clusters` and no elements. It SHALL NOT return `outside_retention` and SHALL NOT issue the `up` probe.

#### Scenario: Unknown cluster

- **WHEN** a request names a `cluster` the upstream does not hold in the selected zones
- **THEN** the server returns 200 with `clusters: []` and empty `nodes` and `edges`

### Requirement: Deterministic body

The body SHALL be byte-identical for identical `(window, cluster, az, env, namespace, roots)` against identical upstream state, with nodes, edges and `clusters` sorted as in `/v1/graph` and no time-of-build or echo-of-input field.

#### Scenario: Repeat request

- **WHEN** the same request is sent twice against unchanged upstream data
- **THEN** the two bodies are byte-identical

### Requirement: Shared request handling

`GET /v1/k8s-graph` SHALL apply the API-key check, the `--build-timeout` deadline (504 `timeout`), end-time alignment and the per-store upstream guard exactly as `GET /v1/graph` does. A build SHALL NOT write the last-build node, edge or clusters-observed gauges.

#### Scenario: Build timeout

- **WHEN** a `/v1/k8s-graph` build exceeds `--build-timeout`
- **THEN** the request returns 504 with `reason: "timeout"`

#### Scenario: Missing API key

- **WHEN** API keys are configured and a request carries no `X-API-Key`
- **THEN** the request returns 401

#### Scenario: Gauges untouched

- **WHEN** a `/v1/graph` build sets the last-build gauges and a `/v1/k8s-graph` build follows
- **THEN** the gauges still hold the `/v1/graph` build's values

### Requirement: In-process engine parity

The reusable engine SHALL expose the build in-process: a parser accepting the endpoint's query parameters with the same validation failures and `reason` codes, a build call taking a window, a selector and the roots, a projection, and a single call producing the Cytoscape body the endpoint returns for the same parameters and upstream state.

#### Scenario: Engine and endpoint agree

- **WHEN** an embedder passes the query parameters of a `/v1/k8s-graph` request to the engine's single-call method against the same upstream state
- **THEN** it receives a body byte-identical to the endpoint's response

#### Scenario: Engine rejects a missing root

- **WHEN** an embedder passes parameters lacking `pod` and `node`
- **THEN** the engine returns a parse error with reason `missing_root` and issues no upstream query
