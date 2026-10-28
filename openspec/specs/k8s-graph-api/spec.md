# k8s-graph-api Specification

## Purpose

Serves the Kubernetes neighbourhood of a set of pods or nodes — where the pods run, the claims they mount, and their namespace and ArgoCD Application grouping — in the Cytoscape.js body shared with the other graph endpoints, without reading or drawing any NetApp or service-graph data.

## Requirements

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

When a root kind's first read would take more queries than the `/v1/storage-graph` cap for the same kind under the shared chunk bounds (the byte budget and the alternation value bound), the request SHALL be rejected 400 `invalid_scope` before any upstream query, and the build SHALL NOT fall back to reading the family across the selected clusters.

#### Scenario: Too many node roots

- **WHEN** a client sends enough `node=` values that the first `kube_pod_info` read exceeds the cap
- **THEN** the server returns 400 with `reason: "invalid_scope"` and issues no upstream query

#### Scenario: Many short roots reach the cap by count

- **WHEN** a client sends more `node=` values than the cap's chunk count times 100, each short enough that the byte budget alone would still admit them
- **THEN** the server returns 400 with `reason: "invalid_scope"` and issues no upstream query, because every chunk also holds at most 100 values

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
- **THEN** it issues its reads of known pods and claims one query per namespace, in the order the `storage-graph-api` capability defines, none of this capability's grouping, ordering or early controller read applies to it, and its body is unchanged

### Requirement: Known pods and claims are read across namespaces in one restriction

A read of known pods or claims SHALL be issued one query per chunk, carrying the chunk's namespaces as one alternation and its pod or claim names as another, chunked under the shared byte budget and so that neither alternation holds more than 100 values, instead of one query per namespace. A row whose `(namespace, name)` pair the read did not request SHALL be discarded before any later read's restriction is computed from it.

#### Scenario: Several namespaces in one query

- **WHEN** a client roots at `node=worker-1` and the seed returns `shop/orders-0`, `shop/web-0` and `platform/redis-0`
- **THEN** incarnation completion is issued as one query restricted to `namespace=~"platform|shop"` and `pod=~"orders-0|redis-0|web-0"` beside the request matchers, not one query per namespace

#### Scenario: A cross pair is discarded

- **WHEN** that query also returns `platform/web-0`, which runs on `worker-2`
- **THEN** the `platform/web-0` row is discarded, no claim, node or controller is read for it, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: Claims are grouped the same way

- **WHEN** the build tracks `shop/data` and `platform/queue`
- **THEN** the claim-info, claim-annotation, kubelet and mounter-completion reads are each one query restricted to `namespace=~"platform|shop"` and `persistentvolumeclaim=~"data|queue"`, and a returned `shop/queue` or `platform/data` row is discarded

#### Scenario: A large pair set is chunked

- **WHEN** the requested pairs exceed the byte budget
- **THEN** they are split into several queries, each carrying only its own pairs' namespaces and names, the results merge in chunk order, and a single pair larger than the budget is still issued

#### Scenario: A node with more than 100 pods keeps every alternation at 100 values or fewer

- **WHEN** a client roots at `node=worker-1`, the seed returns 150 pods whose names together fit the byte budget, and they span 3 namespaces
- **THEN** incarnation completion, the claim-binding read and the `kube_pod_owner` read are each issued as 2 queries, each carrying at most 100 distinct pod names and its own pairs' namespaces, issued in the same round and merged in chunk order
- **AND** the body is byte-identical to the body the same build produces with the pods in one query

#### Scenario: The bound counts each alternation separately

- **WHEN** the pairs a later read requests — pods a seed or a mounter read returned, or tracked claims — hold 100 or fewer distinct names but more than 100 distinct namespaces
- **THEN** the pairs are still split so that no query's namespace alternation holds more than 100 values

#### Scenario: A pod root's first read follows the seed's split

- **WHEN** a client roots at `pod=` values
- **THEN** the roots' own reads follow the seed's split: no chunk's pod-name alternation holds more than 100 values, and each chunk's namespace alternation is the namespaces of its roots, taken from the request like a repeated `namespace` parameter and not bounded

### Requirement: Reads follow their real dependencies

Each read SHALL be issued once the reads its restriction depends on have returned. On a `node` root, incarnation completion, the claim-binding read and the `kube_pod_owner` read SHALL be issued together on the seed's `(namespace, pod)` pairs. On a `pod` root, `kube_pod_info` and `kube_pod_owner` of the roots SHALL be issued with the seed. Mounter completion adds no claim, so only the co-mounters' own reads SHALL wait for it.

#### Scenario: Node root with no co-mounter completes in three rounds

- **WHEN** a client roots at `node=worker-1`, every claim its pods mount is mounted by no pod elsewhere, and its pods are owned through ReplicaSets by Deployments
- **THEN** the build issues three sequential rounds of upstream reads: the seed; incarnation completion, the claim-binding read and `kube_pod_owner`; then the claim families, mounter completion, the application-metadata read, the node families and both controller stages together

#### Scenario: Pod root with no co-mounter completes in two rounds

- **WHEN** a client roots at `pod=shop/orders-0`, which mounts `shop/data` and no other pod mounts `shop/data`
- **THEN** the build issues two sequential rounds: the claim-binding seed with the root's `kube_pod_info` and `kube_pod_owner`; then the claim families, mounter completion, the application-metadata read, the node families and both controller stages together

#### Scenario: A rescheduled pod still follows its newest incarnation

- **WHEN** a client roots at `node=worker-1` and `shop/db-0` ran on `worker-1` early in the window and was recreated on `worker-2` before its end
- **THEN** the binding and owner rows of `shop/db-0` read beside incarnation completion are discarded, its claims are not tracked, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: A root set the cap accepts is never rejected later

- **WHEN** a `pod` root set fits the root-set cap
- **THEN** the roots' `kube_pod_info` and `kube_pod_owner` reads are chunked exactly as the seed is, and the request is never rejected after a query was issued

### Requirement: Co-mounters are read only when no earlier read loaded them

`kube_pod_info` and `kube_pod_owner` SHALL be read for a pod mounting a tracked claim only when no earlier read loaded it, and the node and controller families SHALL be read only for names no earlier read of the same family covered. When every mounter of every tracked claim is already loaded, the build SHALL issue no further pod, node or controller read for co-mounters.

#### Scenario: A shared claim adds the co-mounter's reads

- **WHEN** a client roots at `node=worker-1`, `orders-0` there mounts RWX claim `shop/shared`, and `report-0` on `worker-2` mounts it too
- **THEN** `report-0`'s `kube_pod_info` and `kube_pod_owner` are read after mounter completion, its controllers and `worker-2`'s node families after that, `shop/shared` inherits its Application over both mounters, and `report-0` and `worker-2` are not drawn

#### Scenario: A name is read once per family

- **WHEN** a co-mounter is owned by the same StatefulSet as a root-node pod
- **THEN** no second `kube_statefulset_annotations` query names that StatefulSet

### Requirement: The controller second stage is read ahead of the first

`kube_deployment_annotations` and `kube_cronjob_annotations` SHALL be issued with the first stage, restricted to the direct Deployment / CronJob owner names plus each first-stage ReplicaSet / Job name without its final `-` segment. Once the first stage returns, every name it resolves that the early read did not cover SHALL be read. A row of the early read on no loaded pod's owner chain SHALL leave the body unchanged.

#### Scenario: A Deployment is read with its ReplicaSet

- **WHEN** a loaded pod is owned by ReplicaSet `orders-7c9f8d`, which Deployment `orders` owns
- **THEN** `kube_replicaset_owner` restricted to `replicaset="orders-7c9f8d"` and `kube_deployment_annotations` restricted to `deployment="orders"` are issued in the same round, and no further Deployment annotation query is issued

#### Scenario: A CronJob is read with its Job

- **WHEN** a loaded pod is owned by Job `nightly-29318400`, which CronJob `nightly` controls
- **THEN** `kube_job_owner` restricted to `job_name="nightly-29318400"` and `kube_cronjob_annotations` restricted to `cronjob="nightly"` are issued in the same round

#### Scenario: A name outside the convention is read afterwards

- **WHEN** a loaded pod is owned by Job `manual-run`, created by hand from CronJob `nightly` and carrying no tracking-id of its own
- **THEN** the early read names `cronjob="manual"`, `kube_cronjob_annotations` is read for `nightly` once `kube_job_owner` returns, and the pod's `data.application` is `nightly`'s Application

#### Scenario: A wrong early name leaves the body unchanged

- **WHEN** a loaded pod is owned by ReplicaSet `api-5d4f` that no Deployment owns, and a Deployment `api` carrying a tracking-id exists in the same namespace
- **THEN** the early read may return `api`'s annotation row, the pod's `data.owner` stays `{kind: "ReplicaSet", name: "api-5d4f"}`, and the body is byte-identical to the body an unrestricted read produces

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
