## ADDED Requirements

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

## MODIFIED Requirements

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
