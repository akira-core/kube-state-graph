# Spec Delta

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
- **THEN** it issues its reads of known pods and claims one query per namespace, in the order the `storage-graph-api` capability defines, none of this capability's grouping, ordering or early controller read applies to it, and its body is unchanged

## ADDED Requirements

### Requirement: Known pods and claims are read across namespaces in one restriction

A read of known pods or claims SHALL be issued one query per chunk, carrying the chunk's namespaces as one alternation and its pod or claim names as another, chunked under the shared byte budget, instead of one query per namespace. A row whose `(namespace, name)` pair the read did not request SHALL be discarded before any later read's restriction is computed from it.

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
