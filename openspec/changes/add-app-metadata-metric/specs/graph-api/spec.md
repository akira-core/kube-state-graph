# Spec Delta

## ADDED Requirements

### Requirement: Namespace and application group metadata

When the `app-metadata-source` capability resolves a namespace owner for a `namespace` group's `(cluster, namespace)`, that group SHALL carry `data.namespace_owner`; when it resolves a deployer for an `application` group's `(cluster, namespace, application)`, that group SHALL carry `data.deployed_by`. Both are `omitempty` strings outside `labels`, which stay `{}`; no other node type carries either.

#### Scenario: Namespace group carries its owner

- **WHEN** the metric resolves owner `team-a` for namespace `shop` in cluster `us-dev-c1` and the response contains that namespace group
- **THEN** the node `us-dev-c1/namespace/shop` carries `data.namespace_owner: "team-a"` and `data.labels` is `{}`

#### Scenario: Application group carries its deployer

- **WHEN** the metric resolves deployer `alice` for Application `checkout` in namespace `shop` of cluster `us-dev-c1`
- **THEN** the node `us-dev-c1/namespace/shop/application/checkout` carries `data.deployed_by: "alice"`

#### Scenario: Group exists only when the view needs it

- **WHEN** the metric resolves an owner for namespace `idle` but the projected view holds no pod, service or PVC in `idle`
- **THEN** the response contains no `idle` namespace group

#### Scenario: Unresolved values are omitted

- **WHEN** a namespace group or application group has no resolved owner or deployer, or the metric is unset
- **THEN** its `data` object carries no `namespace_owner` or `deployed_by` key, and the body is byte-identical to the pre-change shape

#### Scenario: Application group from the controller chain still gets its deployer

- **WHEN** every pod of Application `checkout` resolved it through the controller chain and a metric row carries `app="checkout"`, `user="alice"` in the same cluster and namespace
- **THEN** the `checkout` application group carries `data.deployed_by: "alice"`

#### Scenario: Real nodes never carry the group fields

- **WHEN** the response contains pod, service, PVC, K8s node, external or NetApp nodes
- **THEN** none of their `data` objects carries `namespace_owner` or `deployed_by`

## MODIFIED Requirements

### Requirement: Pod `application` and `containers` attributes

Every `data` object for a `type="pod"`, `type="service"`, or `type="pvc"` node SHALL be able to expose an `application` attribute, and every `type="pod"` node SHALL additionally be able to expose a `containers` attribute, all with `omitempty` semantics and all **outside `labels`** (which stays a strict `map[string]string`):

- `application` — a `string`, the node's ArgoCD Application name as resolved by the
  `cluster-topology-source` capability from the `annotation_argocd_argoproj_io_tracking_id`
  label that kube-state-metrics derives from the `argocd.argoproj.io/tracking-id`
  annotation: for `type="pod"` from the `app-metadata-source` capability's metric when
  it names a non-empty Application for the pod (verbatim, see "Pod ArgoCD Application
  attribute"), otherwise from the annotation series of the pod's **controller**
  (`kube_deployment_annotations`, `kube_statefulset_annotations`,
  `kube_daemonset_annotations`, `kube_replicaset_annotations`,
  `kube_job_annotations`, `kube_cronjob_annotations` — see "Pod ArgoCD Application
  attribute"), and for `type="service"` / `type="pvc"` from
  `kube_service_annotations` / `kube_persistentvolumeclaim_annotations` (see "Service
  and PVC ArgoCD Application resolution"). Wherever the annotation is the source, all
  three node types derive the value from the same `<app>:<group>/<kind>:<ns>/<name>`
  parse.
  Emitted only when the node has a resolved Application; omitted entirely otherwise
  (never an empty string). This attribute is **complementary** to the synthesised
  `type="application"` group node (which is derived from this same value — see
  "Cytoscape compound node grouping"); an existing consumer reading `data.application`
  on a pod is unaffected in shape, though a pod whose Application previously came from
  a non-standard pod-level `argocd_tracking_id` label now resolves it from the
  controller instead.
- `containers` — an array of objects `[{ name: string, image: string }]`, one per
  container, as resolved by the `cluster-topology-source` capability and ordered
  deterministically by `(name, image)`. Emitted only on `type="pod"` nodes and only
  when the pod has at least one resolved container; omitted entirely otherwise (never
  an empty array).

The `application` attribute SHALL appear only on `type="pod"`, `type="service"`, and
`type="pvc"` nodes. The `containers` attribute SHALL appear only on `type="pod"`
nodes. `type="node"`, `type="external"`, `type="netapp-aggr"`, `type="netapp-node"`,
and the synthesised `type="cluster"` / `type="storage-cluster"` / `type="namespace"`
/ `type="application"` / `type="controller"` group nodes SHALL NOT emit
`application` or `containers`. The
attributes SHALL NOT appear inside `labels`, and SHALL NOT be encoded as numbers or
booleans. Because both are `omitempty`, a node with neither a resolved Application nor
container info produces a `data` object byte-identical to the pre-change shape.

#### Scenario: Pod node carries application when resolved

- **WHEN** the response contains a pod node whose resolved controller is a Deployment carrying `annotation_argocd_argoproj_io_tracking_id` resolving to Application `checkout`
- **THEN** the corresponding `type="pod"` node carries `data.application: "checkout"` and `data.labels` contains no `annotation_argocd_argoproj_io_tracking_id` / `argocd_tracking_id` / `application` key

#### Scenario: Pod node omits application when its controller has no annotation

- **WHEN** the response contains a pod node whose resolved controller carries no `argocd.argoproj.io/tracking-id` annotation — including a pod whose own `kube_pod_owner` series carried a non-standard `argocd_tracking_id` label
- **THEN** the corresponding `type="pod"` node's `data` object includes no `application` field, and the pod nests under its `controller` group with no `application` group between it and its namespace

#### Scenario: Service node carries application when resolved

- **WHEN** the response contains a service node whose `kube_service_annotations` series carried `annotation_argocd_argoproj_io_tracking_id` resolving to Application `checkout`
- **THEN** the corresponding `type="service"` node carries `data.application: "checkout"` and `data.labels` contains no `annotation_argocd_argoproj_io_tracking_id` / `application` key

#### Scenario: PVC node carries application when resolved

- **WHEN** the response contains a PVC node whose `kube_persistentvolumeclaim_annotations` series carried `annotation_argocd_argoproj_io_tracking_id` resolving to Application `mongo`
- **THEN** the corresponding `type="pvc"` node carries `data.application: "mongo"` and `data.labels` contains no `annotation_argocd_argoproj_io_tracking_id` / `application` key

#### Scenario: PVC node carries inherited application from a mounting pod

- **WHEN** the response contains a PVC node that has no own `annotation_argocd_argoproj_io_tracking_id` annotation but is mounted (via a `pod-mounts-pvc` edge) by a pod whose controller resolves ArgoCD Application `checkout` (see cluster-topology-source "PVC ArgoCD Application inheritance from mounting pod")
- **THEN** the corresponding `type="pvc"` node carries `data.application: "checkout"` — indistinguishable from an annotation-sourced value — `data.labels` contains no `application` / tracking-id key, and the PVC nests under the `<cluster>/namespace/<ns>/application/checkout` compound group

#### Scenario: Pod node carries containers when resolved

- **WHEN** the response contains a pod node whose `kube_pod_container_info` series listed containers `app` (`reg/app:1.2`) and `sidecar` (`reg/proxy:0.9`)
- **THEN** the corresponding `type="pod"` node carries `data.containers: [{"name":"app","image":"reg/app:1.2"},{"name":"sidecar","image":"reg/proxy:0.9"}]` ordered by `(name, image)` and `data.labels` contains no container key

#### Scenario: Pod node omits application and containers when unresolved

- **WHEN** the response contains a pod node with no resolved ArgoCD Application and no container info
- **THEN** the corresponding `type="pod"` node's `data` object includes neither an `application` field nor a `containers` field

#### Scenario: Service and PVC omit application when unresolved

- **WHEN** the response contains a service node and a PVC node with no resolved ArgoCD Application
- **THEN** neither node's `data` object includes an `application` field, and neither includes a `containers` field

#### Scenario: Node, external, and storageclass never carry application or containers

- **WHEN** the response contains nodes of `type="node"`, `type="external"`, `type="netapp-aggr"`, or `type="netapp-node"` (the `storageclass` type this scenario formerly named is removed)
- **THEN** those node `data` objects include neither an `application` field nor a `containers` field

#### Scenario: Deterministic body with new attributes

- **WHEN** the same pod (same Application and container set) is produced by two consecutive builds for the same time bucket
- **THEN** the pod node's `data.application` and `data.containers` are byte-identical between the two builds, with `data.containers` ordered by `(name, image)`

#### Scenario: Pod node carries the metric's application

- **WHEN** the application-metadata metric names Application `shop-v2` for a pod whose Deployment's tracking-id resolves to `checkout`
- **THEN** the corresponding `type="pod"` node carries `data.application: "shop-v2"` and nests under the `<cluster>/namespace/<ns>/application/shop-v2` group
