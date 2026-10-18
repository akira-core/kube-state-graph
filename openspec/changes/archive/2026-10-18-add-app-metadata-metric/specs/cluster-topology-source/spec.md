# Spec Delta

## MODIFIED Requirements

### Requirement: Pod ArgoCD Application attribute

The topology reader SHALL resolve each pod's **ArgoCD Application** from the `annotation_argocd_argoproj_io_tracking_id` label carried on the annotation series of the pod's **controller**, and surface it on the pod entity as a typed, nullable `application` attribute (a string), serialised as `data.application` (`omitempty`) and **never inside `labels`**. ArgoCD stamps `argocd.argoproj.io/tracking-id` on the workload objects it applies, never on the pods a controller spawns, so the controller is the only place the annotation exists; the reader SHALL NOT read any `argocd_tracking_id` label from `kube_pod_owner`.

**Application-metadata metric first.** When the `app-metadata-source` capability yields a non-empty Application for the pod's `(cluster, namespace, pod)`, that value SHALL be the pod's Application and the controller chain below SHALL NOT be consulted for it. Every other pod — including every pod when the metric is unset, unserved, or its `/v1/graph` query failed — SHALL resolve through the controller chain exactly as specified below. The controller-chain queries SHALL still be issued when the metric is configured.

**Controller key.** The lookup key SHALL be the pod's own resolved controller owner from "Pod controller-owner attribute with ReplicaSet skip" — `(cluster, namespace, owner_kind, owner_name)` — so a pod owned through a ReplicaSet is looked up against its already-resolved **Deployment**, and no additional owner hop is needed for that case. A pod with no controller owner SHALL resolve no Application.

**Controller-annotation index.** The reader SHALL build one index keyed `(cluster, namespace, kind, name)` from the six controller-annotation families, each contributing its own owner kind and identity label:

| Owner kind | Series | Identity label |
|---|---|---|
| `Deployment` | `kube_deployment_annotations` | `deployment` |
| `StatefulSet` | `kube_statefulset_annotations` | `statefulset` |
| `DaemonSet` | `kube_daemonset_annotations` | `daemonset` |
| `ReplicaSet` | `kube_replicaset_annotations` | `replicaset` |
| `Job` | `kube_job_annotations` | `job_name` |
| `CronJob` | `kube_cronjob_annotations` | `cronjob` |

A series whose identity label is empty SHALL be skipped. A series missing the `cluster` label SHALL be bucketed under `cluster="unknown"` (the same rule as every other topology series).

**Job → CronJob hop.** When the resolved owner kind is `Job` and the index holds no entry for that Job, the reader SHALL resolve the Job's owning CronJob via `kube_job_owner` keyed `(cluster, namespace, job_name=<owner_name>)` selecting the series with `owner_is_controller="true"` and `owner_kind="CronJob"`, and SHALL then look the index up again at `(cluster, namespace, "CronJob", <owner_name of the job_owner series>)`. This hop exists because the Kubernetes CronJob controller propagates only `spec.jobTemplate.metadata` annotations onto the Jobs it creates — never the CronJob object's own annotations — so ArgoCD's tracking-id never reaches the Job. A directly ArgoCD-managed Job is resolved by its own annotation before the hop is attempted. The hop is **resolution-only**: it SHALL NOT change the pod's `owner` attribute, which stays `{kind:"Job", name:<job>}`, nor its `controller` compound group. On a defensive collision (more than one controller CronJob owner for a Job) the lexically-smallest `owner_name` SHALL win.

**Application name.** The Application name SHALL be the substring of the tracking-id value **before the first `:`** (ArgoCD annotation-based tracking-id form `<app>:<group>/<kind>:<namespace>/<name>`); when the value contains no `:`, the **entire value** SHALL be surfaced verbatim; when the leading segment is empty (e.g. `:apps/Deployment:ns/x`) the controller SHALL resolve to **no** Application rather than to an empty string. This is byte-for-byte the parse the service and PVC resolvers use, so a workload, its Service and its PVC derive their Application from one grammar.

**Determinism.** When more than one distinct non-empty tracking-id is observed for a single `(cluster, namespace, kind, name)`, the reader SHALL pick the **lexically-smallest** raw value whose derived Application is non-empty, so the emitted entity is stable and order-free across rebuilds (D6), mirroring the service and PVC resolvers.

**Unsupported owner kinds.** A pod whose resolved controller owner is of a kind with no kube-state-metrics annotation family — `ReplicationController` (no `kube_replicationcontroller_annotations` exists), `Node` (static / mirror pods), and any third-party CRD controller such as argo-rollouts `Rollout` or OpenKruise `CloneSet` — SHALL resolve no Application. The reader SHALL NOT fail the build, SHALL keep the pod's `owner` attribute, and SHALL NOT emit an empty string.

**Optionality.** The six controller-annotation families and `kube_job_owner` are OPTIONAL: when the annotation allowlist is not configured (their common state under stock kube-state-metrics) the vectors are empty, no pod resolves an Application, and the build SHALL NOT fail. When the label is absent, empty, or unmatched for a pod, the reader SHALL emit a nil `application` so `data.application` is omitted entirely — it SHALL NOT emit an empty string or any application key in `labels`.

This requirement introduces NO new node or edge type — the Application is a typed attribute on the existing `type="pod"` node (the same precedent as the `owner` attribute), keeping `labels` a strict `map[string]string` of typological metadata.

#### Scenario: Pod with a full ArgoCD tracking-id

- **WHEN** `kube_pod_owner{cluster="cluster-alpha", namespace="shop", pod="checkout-1", owner_kind="ReplicaSet", owner_name="checkout-7f9c", owner_is_controller="true"}`, `kube_replicaset_owner{cluster="cluster-alpha", namespace="shop", replicaset="checkout-7f9c", owner_kind="Deployment", owner_name="checkout"}` and `kube_deployment_annotations{cluster="cluster-alpha", namespace="shop", deployment="checkout", annotation_argocd_argoproj_io_tracking_id="storefront:apps/Deployment:shop/checkout"}` are present
- **THEN** the emitted pod entity has `application="storefront"` (the segment before the first `:`), `owner={kind:"Deployment", name:"checkout"}`, and no tracking-id key in `labels`

#### Scenario: Pod owned directly by a StatefulSet

- **WHEN** a pod's resolved owner is `{kind:"StatefulSet", name:"mongo"}` and `kube_statefulset_annotations{cluster="cluster-alpha", namespace="db", statefulset="mongo", annotation_argocd_argoproj_io_tracking_id="mongo:apps/StatefulSet:db/mongo"}` is present
- **THEN** the emitted pod entity has `application="mongo"`

#### Scenario: Pod owned directly by a DaemonSet

- **WHEN** a pod's resolved owner is `{kind:"DaemonSet", name:"fluentd"}` and `kube_daemonset_annotations{cluster="cluster-alpha", namespace="platform", daemonset="fluentd", annotation_argocd_argoproj_io_tracking_id="logging:apps/DaemonSet:platform/fluentd"}` is present
- **THEN** the emitted pod entity has `application="logging"`

#### Scenario: Pod owned by a bare ReplicaSet

- **WHEN** a pod's resolved owner is `{kind:"ReplicaSet", name:"adhoc-rs"}` (no owning Deployment) and `kube_replicaset_annotations{cluster="cluster-alpha", namespace="shop", replicaset="adhoc-rs", annotation_argocd_argoproj_io_tracking_id="adhoc:apps/ReplicaSet:shop/adhoc-rs"}` is present
- **THEN** the emitted pod entity has `application="adhoc"`

#### Scenario: Pod owned by a CronJob-created Job

- **WHEN** a pod's resolved owner is `{kind:"Job", name:"nightly-28901"}`, no `kube_job_annotations` series matches that Job, `kube_job_owner{cluster="cluster-alpha", namespace="batch", job_name="nightly-28901", owner_kind="CronJob", owner_name="nightly", owner_is_controller="true"}` is present, and `kube_cronjob_annotations{cluster="cluster-alpha", namespace="batch", cronjob="nightly", annotation_argocd_argoproj_io_tracking_id="reports:batch/CronJob:batch/nightly"}` is present
- **THEN** the emitted pod entity has `application="reports"` and its `owner` attribute remains `{kind:"Job", name:"nightly-28901"}` — the hop does not rewrite the owner

#### Scenario: Directly managed Job resolves before the hop

- **WHEN** a pod's resolved owner is `{kind:"Job", name:"migrate-1"}` and `kube_job_annotations{cluster="cluster-alpha", namespace="shop", job_name="migrate-1", annotation_argocd_argoproj_io_tracking_id="migrations:batch/Job:shop/migrate-1"}` is present alongside a `kube_job_owner` series naming a CronJob with a different tracking-id
- **THEN** the emitted pod entity has `application="migrations"` (the Job's own annotation wins; the CronJob hop is not attempted)

#### Scenario: Pod with a bare Application name (no colon)

- **WHEN** a pod's controller annotation value is `checkout` (no `:`)
- **THEN** the emitted pod entity has `application="checkout"`

#### Scenario: Empty leading segment yields no Application

- **WHEN** a pod's controller annotation value is `:apps/Deployment:shop/checkout`
- **THEN** the emitted pod entity has a nil `application` (`data.application` omitted entirely)

#### Scenario: Deterministic pick on duplicate controller annotation series

- **WHEN** two `kube_deployment_annotations` series for the same `(cluster, namespace, deployment)` report tracking-ids `zeta:apps/Deployment:shop/x` and `alpha:apps/Deployment:shop/x`
- **THEN** every pod owned by that Deployment resolves `application="alpha"` (the lexically-smallest raw value) and the choice is identical across rebuilds

#### Scenario: Pod with no ArgoCD label

- **WHEN** a pod's resolved owner is `{kind:"Deployment", name:"checkout"}` and the matching `kube_deployment_annotations` series carries an empty `annotation_argocd_argoproj_io_tracking_id` (or no series matches)
- **THEN** the emitted pod entity has a nil `application` (`data.application` omitted entirely) and carries no application key in `labels`

#### Scenario: Pod with no controller owner

- **WHEN** no `kube_pod_owner` series with `owner_is_controller="true"` exists for a pod and the application-metadata metric names no Application for it
- **THEN** the emitted pod entity has a nil `application` and no controller-annotation lookup is attempted

#### Scenario: Owner kind with no annotation family

- **WHEN** a pod's resolved owner is `{kind:"Rollout", name:"canary"}` or `{kind:"ReplicationController", name:"legacy"}`
- **THEN** the emitted pod entity keeps that `owner`, has a nil `application`, and the build does not fail

#### Scenario: ArgoCD label absent entirely

- **WHEN** the upstream contains `kube_pod_owner` series but none of the six controller-annotation families for the window
- **THEN** the reader produces a valid topology with no `application` on any pod and does not fail the build


#### Scenario: Metric Application wins over the controller annotation

- **WHEN** the application-metadata metric names Application `shop-v2` for pod `checkout-1` and the pod's Deployment carries tracking-id `storefront:apps/Deployment:shop/checkout`
- **THEN** the emitted pod entity has `application="shop-v2"` and its `owner` is still `{kind:"Deployment", name:"checkout"}`

#### Scenario: Pod without a metric row falls back to its controller

- **WHEN** the application-metadata metric is configured but carries no row for pod `checkout-2`, whose Deployment carries tracking-id `storefront:apps/Deployment:shop/checkout`
- **THEN** the emitted pod entity has `application="storefront"`

#### Scenario: Metric resolves a pod the controller chain cannot

- **WHEN** a pod's resolved owner is `{kind:"Rollout", name:"canary"}` and the application-metadata metric names Application `canary-app` for it
- **THEN** the emitted pod entity has `application="canary-app"`

#### Scenario: PVC inheritance follows the metric value

- **WHEN** PVC `data-1` carries no Application of its own and is mounted only by a pod whose Application the metric resolves to `shop-v2`
- **THEN** the PVC inherits `application="shop-v2"`
