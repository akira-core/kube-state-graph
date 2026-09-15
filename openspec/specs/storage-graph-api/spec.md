# storage-graph-api Specification

## Purpose

Serves a storage-rooted flow graph — NetApp controller → aggregate → SVM → PVC → pod → Kubernetes node — with an I/O weight on every hop, so a Sankey diagram can answer "what runs on this filer?" and "which filer does this pod use?" from one endpoint.

## Requirements

### Requirement: Storage-flow graph endpoint

The server SHALL expose `GET /v1/storage-graph` returning a storage-flow graph for a caller-specified `[start, end]` window, in the same `{ apiVersion, clusters, elements: { nodes, edges } }` Cytoscape.js shape as `GET /v1/graph`. `start` and `end` SHALL be required and validated exactly as for `/v1/graph` (RFC 3339 or Unix seconds; `end > start`; `missing_start` / `missing_end` / `invalid_start` / `invalid_end` / `invalid_range`). The endpoint SHALL sit behind the same API-key authentication, the same per-build timeout (`--build-timeout` → 504 `timeout`), and the same upstream / outside-retention / cancelled error mapping as `/v1/graph`, and SHALL be described in the served OpenAPI document.

`az` and `env` SHALL be **required** and **single-valued**: a request lacking either SHALL be rejected 400 with `reason: "missing_az"` / `reason: "missing_env"`, and a request repeating either SHALL be rejected 400 with `reason: "invalid_scope"`. The two values SHALL be pushed upstream exactly as the `/v1/graph` selector-level `az` / `env` dimensions are (matchers on the Kubernetes families, backend selection for Harvest), so the body describes one estate: a filer shared across zones or environments is never merged into one diagram. `cluster` and `namespace` SHALL be accepted as optional, repeatable narrowing filters with `/v1/graph` semantics. `prune` SHALL be ignored (this endpoint applies its own reachability projection, never the connectivity prune). `edge_type` — withdrawn from `/v1/graph` as well, so no longer a parameter of any endpoint — and any other unknown parameter SHALL be ignored without error, whatever value they carry.

The top-level `clusters` array SHALL list the Kubernetes cluster identities present on emitted `pod` / `node` / `pvc` nodes and never an ONTAP cluster name.

#### Scenario: Successful request

- **WHEN** a client sends `GET /v1/storage-graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z&az=zone-a&env=prod&aggr=aggr1`
- **THEN** the server returns 200 with a body containing exactly `apiVersion: "v1"`, `clusters`, and `elements` with `nodes` and `edges`

#### Scenario: Missing az

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&env=prod`
- **THEN** the server returns 400 with `reason: "missing_az"`

#### Scenario: Repeated env

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod&env=dev`
- **THEN** the server returns 400 with `reason: "invalid_scope"` and a message naming `env`

#### Scenario: Zone and environment reach upstream

- **WHEN** a client sends `?az=zone-a&env=prod`
- **THEN** every kube-state-metrics, kubelet and `ALERTS` query carries `<az-key>="zone-a",<env-key>="prod"`, every Harvest query is issued only to the `harvest` backends whose `zones` include `zone-a` (or catch-alls) with no matcher, and no series from another zone or environment contributes to the body

#### Scenario: Unauthenticated request rejected when keys configured

- **WHEN** API keys are configured and a client sends `GET /v1/storage-graph` without `X-API-Key`
- **THEN** the server returns 401 exactly as `/v1/graph` would

#### Scenario: Graph-only and withdrawn parameters are ignored

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod&prune=false&edge_type=not-a-type`
- **THEN** the server returns 200 with a body byte-identical to the same request without `prune` and `edge_type` — neither value is validated, and neither narrows or widens the body

### Requirement: Root selectors from either end of the flow

The endpoint SHALL accept the following optional, repeatable root selectors, each value a plain string validated like a `/v1/graph` selector value (≤ 253 bytes, valid UTF-8, no control characters; otherwise 400 `invalid_scope`):

- `ontap_cluster=<name>` — a storage root naming an ONTAP cluster (every controller, aggregate and SVM in it);
- `node=<name>` — matched against BOTH the ONTAP controller name and the Kubernetes node name; a hit on either tier makes that node a root on its own side, and a name present on both tiers makes both roots;
- `aggr=<name>` — a storage root naming an ONTAP aggregate;
- `svm=<name>` — a storage root naming an SVM;
- `pod=<namespace>/<pod-name>` — a workload root naming one pod; a value without exactly one `/` separating two non-empty segments SHALL be rejected 400 `invalid_scope`.

Values of one selector SHALL be OR-combined. Roots on the **storage side** (`ontap_cluster`, `aggr`, `svm`, and `node` hits on a controller) and roots on the **workload side** (`pod`, and `node` hits on a Kubernetes node) SHALL be **AND-combined across sides**: when both sides carry at least one root, a path is retained only if it touches a root on EACH side; when only one side carries roots, a path is retained if it touches any of them; when no root is given, every complete path in the selected estate is retained. Root names are matched exactly and case-sensitively. A storage root SHALL be matched across every ONTAP cluster the selected zone's Harvest backends return unless `ontap_cluster` narrows it; a workload root SHALL be matched across every Kubernetes cluster in the selected estate unless `cluster` narrows it.

#### Scenario: Storage root finds its consumers

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1` and two claims on `aggr1` are mounted by pods `shop/orders-0` and `shop/catalog-0`
- **THEN** the body contains the controller owning `aggr1`, `aggr1`, the SVMs of both claims, both PVCs, both pods and their Kubernetes nodes, and no other pod

#### Scenario: Workload root finds its storage

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0` and that pod mounts one NetApp-backed claim on `(ontap-prod, ontap-prod-01, aggr1, svm_shop)`
- **THEN** the body contains exactly the chain `netapp/ontap-prod/ontap-prod-01 → netapp/ontap-prod/aggr/aggr1 → netapp/ontap-prod/svm/svm_shop → <pvc> → <pod> → <node>`

#### Scenario: node matches both tiers

- **WHEN** a client sends `?node=n1` and the estate holds an ONTAP controller `n1` and a Kubernetes node `n1`
- **THEN** every path through the controller `n1` OR through the Kubernetes node `n1` is retained

#### Scenario: Roots on both sides intersect

- **WHEN** a client sends `?aggr=aggr1&pod=shop/orders-0` and `shop/orders-0` mounts one claim on `aggr1` and one on `aggr2`
- **THEN** the body contains only the `aggr1` path to `shop/orders-0`; the `aggr2` chain and every other pod on `aggr1` are absent

#### Scenario: No root returns the estate

- **WHEN** a client sends `?az=zone-a&env=prod` with no root selector
- **THEN** the body contains every complete storage-flow path the selected estate's NetApp-backed, mounted claims form

#### Scenario: Malformed pod root

- **WHEN** a client sends `?pod=orders-0`
- **THEN** the server returns 400 with `reason: "invalid_scope"`

### Requirement: Roots are always materialised when the upstream knows them

A root the upstream names in the window SHALL appear in the body even when no flow passes through it. A storage root "exists" when at least one Harvest series read in the build names it (`volume_labels`, `node_labels`, `node_new_status`, the node performance counters, or the `aggr_*` families; an SVM only via `volume_labels`); a workload root exists when `kube_node_info` (node) or `kube_pod_info` (pod) names it in the selected estate. A flowless root SHALL be emitted with its ordinary attributes and its compound parent (an aggregate root also materialises the controller currently owning it, so that `data.parent` never dangles) and **no** edges. A root NO series names SHALL NOT be drawn: the body is simply empty of it, with no error and no marker.

#### Scenario: Aggregate with no claims still shows

- **WHEN** a client sends `?aggr=aggr9` and Harvest reports `aggr9` on `ontap-prod-02` but no loaded claim joins it
- **THEN** the body contains `netapp/ontap-prod/aggr/aggr9` (parent `netapp/ontap-prod/ontap-prod-02`, which is also present) and no edge

#### Scenario: Pod with no NetApp-backed claim still shows

- **WHEN** a client sends `?pod=shop/web-0` and that pod mounts no claim that joins the Harvest topology
- **THEN** the body contains the pod node (with its namespace / application / controller groups) and no edge

#### Scenario: Unknown root is not drawn

- **WHEN** a client sends `?aggr=typo` and no Harvest series in the window carries `aggr="typo"`
- **THEN** the server returns 200 with empty `nodes` and `edges` and an empty `clusters` array

### Requirement: Fixed tier chain and the `storage-flow` edge

The body SHALL express the storage chain as directed edges of ONE type, `storage-flow`, oriented storage → workload, one edge per adjacent pair on the fixed tier chain `netapp-node → netapp-aggr → netapp-svm → pvc → pod → node`. Each edge SHALL carry `labels.tier` naming its hop — exactly one of `node-aggr`, `aggr-svm`, `svm-pvc`, `pvc-pod`, `pod-node` — and its `id` SHALL be the UUIDv5 of `storage-flow|<source>|<target>` under the server's fixed edge namespace, so an edge is byte-stable across rebuilds. Each `(source, target)` pair SHALL appear at most once regardless of how many claims flow through it.

The chain SHALL be derived from the existing storage join: a claim's aggregate and SVM from its matched `volume_labels` series, its owning controller from that aggregate's `node`, its mounting pods from `kube_pod_spec_volumes_persistentvolumeclaims_info`, and each pod's node from `kube_pod_info`. A claim whose match resolved an SVM but no aggregate (the FlexGroup shape) SHALL enter the chain at the `svm-pvc` tier with no `node-aggr` / `aggr-svm` edge. A pod not scheduled on a node SHALL end its path at the `pvc-pod` tier. The body SHALL NOT contain `pod-mounts-pvc`, `pod-to-node`, `pvc-to-netapp-aggr`, `pod-calls-*`, or `service-selects-pod` edges, and SHALL NOT contain `service` or `external` nodes.

#### Scenario: One claim draws one path

- **WHEN** claim `shop/orders-data` joins `(ontap-prod, ontap-prod-01, aggr1, svm_shop)` and is mounted by pod `shop/orders-0` scheduled on `worker-1`
- **THEN** the body contains exactly five `storage-flow` edges with tiers `node-aggr` (`netapp/ontap-prod/ontap-prod-01` → `netapp/ontap-prod/aggr/aggr1`), `aggr-svm`, `svm-pvc`, `pvc-pod`, `pod-node` (`<pod> → <cluster>/worker-1`), and no edge of any other type

#### Scenario: Shared upstream hops are emitted once

- **WHEN** two claims on `aggr1` in `svm_shop` are mounted by different pods
- **THEN** the body contains one `node-aggr` edge and one `aggr-svm` edge, and two of each downstream tier

#### Scenario: FlexGroup claim starts at the SVM

- **WHEN** a claim's matched `volume_labels` series carries `svm="svm_big"` and an empty `aggr`
- **THEN** the claim's path begins with an `svm-pvc` edge from `netapp/ontap-prod/svm/svm_big` and no `node-aggr` or `aggr-svm` edge is emitted for it

#### Scenario: Edge id stable across rebuilds

- **WHEN** the same estate is built twice
- **THEN** every `storage-flow` edge carries the same `id` in both bodies

### Requirement: Flow weights on every tier

Every `storage-flow` edge on a path with at least one measured claim SHALL carry `data.metrics` with `read_ops`, `write_ops`, `read_bytes_per_sec` and `write_bytes_per_sec`, each the sum — over every claim whose path passes through that edge — of the claim's own I/O measurement from the storage join (the `pvc-to-netapp-aggr` figures of `/v1/graph`). The `svm-pvc` edge, being the claim-level edge, SHALL additionally carry the claim's `read_latency_us`, `write_latency_us` and, when resolved, `max_iops` / `max_bytes_per_sec`; no other tier SHALL carry latency or ceiling fields. A claim with no I/O measurement contributes nothing to any sum, and an edge whose every contributing claim is unmeasured SHALL carry no `metrics` key. Sums SHALL be accumulated in ascending contribution order and rounded to 6 significant digits at serialisation, so the wire form is order-independent.

A claim mounted by more than one pod SHALL have its weight **split equally** across the `pvc-pod` edges to its mounting pods (each receiving `1/n` of every summed figure), and each such `pvc-pod` edge SHALL carry `labels.attribution="split"`; a `pod-node` edge SHALL sum the (possibly split) `pvc-pod` weights of its pod. A `pvc-pod` edge from a singly-mounted claim SHALL carry no `attribution` label. Weights therefore conserve tier to tier: for every non-root interior node, the sum of incoming `read_ops` equals the sum of outgoing `read_ops` (likewise the other three figures), up to rounding.

#### Scenario: Weights conserve through the chain

- **WHEN** two claims on `aggr1` measure `read_ops` 100 and 250, in SVMs `svm_a` and `svm_b`, each mounted by one pod on distinct nodes
- **THEN** the `node-aggr` edge carries `read_ops: 350`, the two `aggr-svm` edges carry 100 and 250, and each downstream tier carries its claim's figure unchanged

#### Scenario: RWX claim split across its mounters

- **WHEN** one claim measuring `read_ops: 300` is mounted by three pods
- **THEN** each of the three `pvc-pod` edges carries `read_ops: 100` and `labels.attribution="split"`, and the `svm-pvc` edge carries `read_ops: 300` with no `attribution` label

#### Scenario: Latency and ceiling only on the claim-level edge

- **WHEN** a claim resolves `read_latency_us: 450` and `max_iops: 5000`
- **THEN** its `svm-pvc` edge carries both fields and its `node-aggr`, `aggr-svm`, `pvc-pod` and `pod-node` edges carry neither

#### Scenario: Unmeasured claim draws a weightless path

- **WHEN** a claim joins the topology but matches no QoS workload series
- **THEN** its path is emitted and none of its edges carries a `metrics` key, unless another measured claim shares an upstream hop — in which case that hop carries only the measured claim's figures

### Requirement: Storage-reachability projection

The body SHALL retain a node iff it lies on a **complete** storage-flow path (`netapp-node → … → pod`, or `netapp-svm → … → pod` for a FlexGroup claim) that satisfies the root rule of "Root selectors from either end of the flow", or it is a materialised root (or a root's real compound parent). An **unmounted** claim SHALL be dropped, and so SHALL any aggregate, SVM or controller reachable only through unmounted claims; a pod none of whose claims joins the Harvest topology SHALL be dropped; a Kubernetes node hosting only dropped pods SHALL be dropped. The `/v1/graph` connectivity prune SHALL NOT apply. `cluster` / `namespace` narrow the claim / pod / node side upstream and are re-applied at projection; a storage root is never dropped by them.

#### Scenario: Unmounted claim dropped with its lonely aggregate

- **WHEN** aggregate `aggr7` is joined only by claims no pod mounts and is not a root
- **THEN** neither the claims nor `aggr7` nor (if it owns nothing else retained) its controller appear in the body

#### Scenario: Namespace filter narrows the workload side only

- **WHEN** a client sends `?aggr=aggr1&namespace=shop` and `aggr1` serves claims in `shop` and `platform`
- **THEN** the body contains `aggr1`, its controller, and only the `shop` claims' SVMs, PVCs, pods and nodes

### Requirement: Attributes and compound groups carry over

Every retained real node SHALL carry the same `data` attributes it carries in `/v1/graph` — including `ipaddress`, `owner`, `application`, `ready_status`, `health`, `usage`, `storageclass`, `hardware`, `perf`, `alerts` and `status` — and the body SHALL include the same synthesised compound groups with the same `data.parent` rules (`cluster > namespace > application > controller > pod`, `cluster > namespace > [application >] pvc`, `cluster > node`, `storage-cluster > netapp-node > netapp-aggr`, `storage-cluster > netapp-svm`). Namespace and ArgoCD Application SHALL NOT be tiers of the flow; a consumer derives a namespace- or Application-level Sankey by walking `data.parent` and summing the conserved weights.

A storage-graph pod SHALL NOT carry `data.containers`: the storage build does not read the container family (see "Storage build reads only what it draws"), and a storage-flow consumer has no use for a container list. This is the ONE attribute on which the two bodies differ for the same pod.

#### Scenario: Pod keeps its attributes and groups

- **WHEN** a retained pod carries `data.application="checkout"` and `data.owner={kind:"StatefulSet", name:"orders"}` in `/v1/graph`
- **THEN** the storage-graph body carries the same attributes on that pod and its `data.parent` names the same controller group, whose ancestry reaches `cluster/<cluster>`

#### Scenario: No namespace or application tier

- **WHEN** any storage-graph body is inspected
- **THEN** no `storage-flow` edge has a `namespace` or `application` group node as source or target

#### Scenario: Pod carries no containers

- **WHEN** a retained pod carries `data.containers` in `/v1/graph`
- **THEN** the storage-graph body's node for that pod has no `containers` field, while its `owner`, `application`, `status` and `data.parent` are unchanged

### Requirement: Each claim names its aggregate

A PVC node in the body SHALL carry the same `labels.aggr` it carries in `/v1/graph` (see "PVC `aggr` label" in `graph-api`): the node id of the aggregate its volume sits on, absent for a FlexGroup claim. It is the one place the body states a claim's aggregate. The `aggr-svm` tier is shared by every claim an SVM holds on that aggregate, so once an SVM spans more than one aggregate, walking up from a PVC through its SVM cannot tell which aggregate the claim sits on.

Whenever a PVC carrying `labels.aggr` is in the body, the node it names SHALL be in the same body: it lies on the claim's path (`netapp-node → netapp-aggr → netapp-svm → pvc`), which the storage-reachability projection retains whole, and it has an `aggr-svm` edge to the PVC's SVM. No `storage-flow` edge SHALL carry a label naming a claim's aggregate: the tier chain and the edge labels (`tier`, `attribution`) are unchanged.

#### Scenario: An SVM spanning two aggregates

- **WHEN** SVM `svm_shop` on ONTAP cluster `ontap-prod` holds claim `shop/orders-data` on `aggr1` and claim `shop/catalog-data` on `aggr2`, both mounted, and a client sends `?az=zone-a&env=prod&svm=svm_shop`
- **THEN** the body carries an `aggr-svm` edge from each aggregate to `svm_shop` and one `svm-pvc` edge to each PVC; the `shop/orders-data` PVC carries `labels.aggr="netapp/ontap-prod/aggr/aggr1"`, the `shop/catalog-data` PVC carries `labels.aggr="netapp/ontap-prod/aggr/aggr2"`, and both aggregate nodes are in the body

#### Scenario: A storage root keeps each claim's own aggregate

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1` and `svm_shop` also holds mounted claims on `aggr2`
- **THEN** every PVC in the body carries `labels.aggr="netapp/ontap-prod/aggr/aggr1"`, and none of `svm_shop`'s `aggr2` claims is retained

#### Scenario: A FlexGroup claim names no aggregate

- **WHEN** a retained claim's volume is a FlexGroup, so its path starts at its `svm-pvc` edge
- **THEN** its PVC carries `labels.svm` and no `labels.aggr`, and no `node-aggr` or `aggr-svm` edge is emitted for it

#### Scenario: No storage-flow edge names a claim's aggregate

- **WHEN** any storage-graph body is inspected
- **THEN** every `storage-flow` edge's `labels` holds only `tier` and, on a split `pvc-pod` edge, `attribution`

### Requirement: Deterministic storage-graph body

The body SHALL be byte-identical for identical `(window, az, env, roots, cluster, namespace)` against identical upstream state: nodes and edges sorted as in `/v1/graph`, `clusters` sorted, weights order-independent, and no time-of-build or echo-of-input field. Root selector values given in a different order or repeated SHALL produce an identical body.

#### Scenario: Root order does not matter

- **WHEN** a client sends `?aggr=b&aggr=a` and then `?aggr=a&aggr=b&aggr=a`
- **THEN** both bodies are byte-identical

### Requirement: In-process storage-graph engine surface

The reusable graph engine SHALL expose the storage-graph build in-process with the same contract as the HTTP endpoint: a request parser accepting the endpoint's query parameters and returning the same validation failures (same `reason` codes), and a single call producing the identical Cytoscape body from those parameters, so an embedding module obtains byte-for-byte the `/v1/storage-graph` body without an HTTP hop. The HTTP handler SHALL use that same parser, so the request contract cannot drift between server and embedder.

#### Scenario: Embedder and server agree

- **WHEN** the same query parameters are given to the in-process call and to `GET /v1/storage-graph` against the same upstream state
- **THEN** the two bodies are byte-identical, and an invalid parameter set fails both with the same `reason`

### Requirement: Storage build reads only what it draws

The storage-graph build SHALL NOT issue the following kube-state-metrics families: `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`, `kube_service_annotations`. The body contains no `service` or `external` node and no `service-selects-pod` edge, so the four service-side families can contribute nothing to it, and it does not carry `containers` (see "Attributes and compound groups carry over"). A family the build does not issue SHALL be absent from the build's per-family series tally, never reported as zero. Every other family the `/v1/graph` topology read issues — node, claim-binding, claim-info, claim-annotation, controller-owner, controller-annotation, Harvest, kubelet and `ALERTS` — SHALL be issued exactly as `/v1/graph` issues it.

The build SHALL read `kube_pod_info` and `kube_pod_owner` **by reference**: restricted to the union of (a) the `pod` names of every `kube_pod_spec_volumes_persistentvolumeclaims_info` series it loaded and (b) the pod-name segment of every `pod=<namespace>/<pod-name>` root the request carries. The restriction SHALL be a sorted, de-duplicated, anchored alternation on the `pod` label, **composed with** the request-scoped matchers those two families already carry (`az`, `env`, `cluster`, `namespace`) — never replacing them. It is derived from upstream data and from the request's roots, not from a selector-level dimension, so the request-scoped selector table is unchanged. The pod read SHALL wait on the claim-binding family alone and SHALL NOT wait on families it does not read.

A pod name is unique within a namespace but not across namespaces, so the restriction MAY admit a pod in another namespace that shares a name with a mounting pod or a root. Such a pod SHALL be harmless: it lies on no retained path and is not a root, so the projection drops it, and the body SHALL be byte-identical to the body an unrestricted pod read would produce — except that no pod carries `containers`.

When the scope is **empty** — no claim-binding series was loaded and the request carries no `pod=` root — the build SHALL issue **no** `kube_pod_info` or `kube_pod_owner` query at all, mirroring the empty-scope rule of the QoS workload read. The body then holds no complete path and contains only the roots the storage side materialised.

The alternation SHALL be **chunked deterministically** under the same byte budget as the QoS workload read, one query per chunk per family, results merged in **chunk order**. A single pod name SHALL always be issued even when it alone exceeds the budget. Unlike a QoS chunk, a pod chunk SHALL NOT degrade: the two pod families are topology, not decoration, so a failed chunk fails the build exactly as an unrestricted `kube_pod_info` query error does. Every chunk SHALL be issued under the bare family name for self-metrics and span dimensions.

Because a root has to be in the restriction to be loaded, the storage build SHALL receive the request's roots as an input. A `pod=` root that mounts no claim SHALL still be materialised, as "Roots are always materialised when the upstream knows them" requires.

#### Scenario: Pod read is restricted to mounting pods and roots

- **WHEN** a build for `?az=zone-a&env=prod&pod=shop/web-0` loads claim-binding series naming pods `orders-0`, `orders-1` and `catalog-0` while the estate holds 40000 pods
- **THEN** every issued `kube_pod_info` and `kube_pod_owner` query restricts `pod` to exactly `{catalog-0, orders-0, orders-1, web-0}` alongside `<az-key>="zone-a",<env-key>="prod"`, no series for any other pod is fetched, and the body is byte-identical to the body an unrestricted pod read would produce

#### Scenario: Empty scope issues no pod query

- **WHEN** a build for `?az=zone-a&env=prod&aggr=aggr1` loads no claim-binding series
- **THEN** no `kube_pod_info` or `kube_pod_owner` query is issued, the per-family tally carries neither family, and the body contains `aggr1`, its owning controller and no other node

#### Scenario: Claimless pod root is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/web-0` and `shop/web-0` mounts no claim
- **THEN** the pod's name is in the restriction, `kube_pod_info` names it, and the body contains that pod with its ordinary attributes and compound parent and no edges

#### Scenario: Skipped families never reach the upstream

- **WHEN** the upstream rejects every `kube_pod_container_info` query with a series-limit error and a client sends any `/v1/storage-graph` request
- **THEN** the storage build issues no `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels` or `kube_service_annotations` query, `kube_state_graph_upstream_query_failures_total` does not move, and the request returns 200

#### Scenario: Cross-namespace name collision is harmless

- **WHEN** claim-binding series name `shop/web-0` and the estate also holds a claimless `platform/web-0`
- **THEN** `platform/web-0` may be fetched but does not appear in the body, which is byte-identical to the body an unrestricted pod read would produce

#### Scenario: A pod chunk failure fails the build

- **WHEN** the restriction is split into three chunks and the second `kube_pod_info` chunk fails with an upstream error
- **THEN** the build returns an error and the request is mapped as an upstream failure, exactly as an unrestricted `kube_pod_info` failure is

### Requirement: Pod-only roots narrow the upstream read

When a `/v1/storage-graph` request carries at least one `pod=<namespace>/<pod-name>` root, no `ontap_cluster`, `aggr`, `svm` or `node` root, and no `namespace` parameter, the request parser SHALL derive the build's `namespace` selector from the roots: the sorted, de-duplicated set of their namespace segments, rendered into every namespaced kube-state-metrics, kubelet and `ALERTS` query exactly as an explicit `?namespace=` with those values would be. The derivation SHALL be output-preserving: with pod roots only, every retained path is anchored on a root pod, its claim lies in that pod's namespace, and every other pod on the path mounts the same claim in the same namespace, so no retained node lies outside the derived set; the Kubernetes node, Harvest and storage-inventory families are not namespaced and are read as before.

An explicit `namespace` parameter SHALL be used as given — never widened, intersected or replaced by the roots' namespaces; a root outside it is dropped by the projection as today. Any storage-side or `node` root SHALL suppress the derivation: those roots select paths across namespaces. The derived set SHALL be a pure function of the root set, so root order does not change the issued queries, and the in-process engine surface SHALL derive it identically, since both share one parser.

#### Scenario: Pod roots push their namespaces upstream

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0&pod=platform/redis-0`
- **THEN** every namespaced kube-state-metrics and kubelet query carries `namespace=~"platform|shop"`, the `ALERTS` query carries `namespace=~"platform|shop|"`, and the body is byte-identical to the body of the same request built without the derived selector

#### Scenario: Mixed roots do not derive a namespace

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0&aggr=aggr1`
- **THEN** no query carries a `namespace` matcher, and the body is the intersection the root rule already defines

#### Scenario: Explicit namespace wins over roots

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0&namespace=platform`
- **THEN** every namespaced query carries `namespace="platform"` only, and the root is absent from the body because it lies outside the namespace filter

#### Scenario: Root order does not change the queries

- **WHEN** a client sends `?pod=b/x&pod=a/y` and then `?pod=a/y&pod=b/x`
- **THEN** both requests render `namespace=~"a|b"` and return byte-identical bodies
