## Why

The pod `application` attribute is read from an `argocd_tracking_id` label on
`kube_pod_owner` — a label **no kube-state-metrics build produces**. It existed
because a previous deployment ran a customised exporter that copied each pod's
`metadata.annotations["argocd.argoproj.io/tracking-id"]` onto that series. That
exporter is gone and is not coming back; the deployment is now stock
kube-state-metrics only, so `resolvePodApplications` reads an always-empty label
and **every pod silently loses `data.application`**, its `application` compound
group, and the PVC Application-inheritance fallback that borrows from mounting
pods.

Copying the annotation onto the pod was never reproducible with stock KSM in the
first place: ArgoCD stamps `argocd.argoproj.io/tracking-id` on the resources it
applies — the Deployment / StatefulSet / DaemonSet / CronJob — **not** on the
pods a controller spawns, and neither `--metric-labels-allowlist` nor
`--metric-annotations-allowlist` can enrich `kube_pod_owner` from another
resource's annotations. Services and PVCs are unaffected precisely because they
*are* ArgoCD-managed objects and carry the annotation themselves. The pod's
Application must therefore be resolved the way ArgoCD actually models ownership:
from the pod's **controller**.

## What Changes

- **BREAKING — the pod-level `argocd_tracking_id` label is withdrawn as a
  source.** `kube_pod_owner` is no longer read for the Application at all and the
  label leaves the series contract. A deployment still synthesising that label
  (custom exporter, or a recording rule joining `kube_pod_labels`) loses pod
  Applications unless it adopts the controller-annotation path below. This is
  deliberate: the label has no stock producer, so keeping a dead precedence tier
  would only preserve an approximation (a Helm release name) that outranks the
  real ArgoCD tracking-id.
- **Add every controller-annotation family kube-state-metrics can export** to the
  topology fan-out, so **all** pod controller kinds are covered:
  `kube_deployment_annotations`, `kube_statefulset_annotations`,
  `kube_daemonset_annotations`, `kube_replicaset_annotations`,
  `kube_job_annotations`, `kube_cronjob_annotations` — each carrying the
  `annotation_argocd_argoproj_io_tracking_id` label KSM emits under
  `--metric-annotations-allowlist`.
- **Add `kube_job_owner`** to resolve a Job-owned pod up to its owning CronJob.
  The Kubernetes CronJob controller copies only `spec.jobTemplate.metadata`
  annotations onto the Jobs it creates, never the CronJob object's own
  annotations, so ArgoCD's tracking-id never reaches the Job — the hop is the
  only way a CronJob's pods can resolve an Application. The `ReadTopology`
  errgroup grows from **30 legs to 37**.
- **Resolve a pod's Application from its already-resolved controller owner.** The
  D34 ReplicaSet→Deployment skip means the owner a pod already carries *is* the
  ArgoCD-managed object in the Deployment case, so this is a single lookup —
  `(cluster, namespace, owner_kind, owner_name)` against the controller-annotation
  index — with the Job→CronJob hop as the one fallback when a Job carries no
  annotation of its own.
- **`data.owner` is unchanged.** The Job→CronJob hop is resolution-only: a
  CronJob-owned pod keeps `owner={kind:"Job", …}` and its existing `controller`
  compound group. Rewriting the owner the way ReplicaSet→Deployment does would
  change output that consumers already depend on, which this change does not do.
- **Kinds that cannot be covered are stated, not silently dropped.**
  `ReplicationController` has no `kube_replicationcontroller_annotations` family
  in kube-state-metrics; `Node` (static / mirror pods) and third-party CRD
  controllers (argo-rollouts `Rollout`, OpenKruise `CloneSet`, …) have none
  either. Pods owned by those kinds keep their `owner` and carry no
  `application`.
- **Application-name parsing is unchanged** — the segment before the first `:`
  of the `<app>:<group>/<kind>:<namespace>/<name>` tracking-id, verbatim when the
  value contains no `:`, absent when the leading segment is empty — so a pod, its
  Service and its PVC now derive their Application from the *same* grammar and
  the *same* ArgoCD annotation, and the `application` compound group finally
  groups all three consistently.
- **Deterministic and order-free**, per D6: the controller-annotation index picks
  the lexically-smallest raw tracking-id per `(cluster, namespace, kind, name)`,
  mirroring the existing service / PVC resolvers.
- All seven new families are **OPTIONAL** and degrade exactly like the existing
  `kube_service_annotations` / `kube_persistentvolumeclaim_annotations` legs:
  absent, unmatched, or empty ⇒ no `application`, never a build failure.
- **Documentation** — `README.md`'s topology-metrics table and
  `docs/kube-state-metrics-preconditions.md` gain the seven series, the extra KSM
  collectors they need (`deployments`, `statefulsets`, `daemonsets`,
  `replicasets` already present, `jobs`, `cronjobs`), the RBAC that implies, and
  the `--metric-annotations-allowlist` entries. The existing "Pod-level ArgoCD
  Application is not reachable by KSM config" section is replaced by the
  supported path.
- No new node type, edge type, `labels` key, request parameter, or dependency.
  `data.application` keeps its shape and `omitempty` semantics.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `cluster-topology-source`: three requirements change.
  - *Topology series consumed* — adds the six controller-annotation families and
    `kube_job_owner` as OPTIONAL **[AECN]** families with their degradation rule,
    and drops `argocd_tracking_id` from the `kube_pod_owner` label contract.
  - *Pod ArgoCD Application attribute* — the source becomes the controller's
    `annotation_argocd_argoproj_io_tracking_id`; the requirement gains the
    owner-kind coverage table, the Job→CronJob hop, the deterministic pick, and
    the statement that the hop does not alter `data.owner`. The
    `argocd_tracking_id` source and its scenarios are removed.
  - *Request-scoped upstream selectors* — the `namespace` dimension's family
    enumeration is extended to the controller-scoped series.
- `graph-api`: one requirement changes.
  - *Pod `application` and `containers` attributes* — the sentence naming the
    pod Application's source moves from "the `argocd_tracking_id` label on
    `kube_pod_owner`" to the controller's
    `annotation_argocd_argoproj_io_tracking_id`. The wire shape is unchanged.

## Impact

**Code**

- `pkg/promql/queries.go` — seven `Query` constants, seven `Render` cases, seven
  `queryDims` entries (the dims table is test-enforced to cover every constant).
- `pkg/build/topology.go` — seven `Vectors` fields, seven `ReadTopology`
  errgroup legs, a `(cluster, namespace, kind, name)` controller-annotation index
  and a Job→CronJob index, consumed by a rewritten `resolvePodApplications`
  (reusing the `argoAppName` helper and the `ownerRef` the controller pick
  already produces). The generic `resolveApplications` helper keeps serving the
  service / PVC resolvers; the pod resolver no longer shares it, since it keys on
  the owner rather than on the series' own identity labels.
- `pkg/build/netapp_test.go` — the 30-leg fan-out pin becomes 37.
- Unit tests in `pkg/build/topology_test.go` (one per owner kind plus the hop and
  the unsupported-kind degrade), an `internal/integration` case, and the golden
  files whose fixture pods carry an Application.

**Docs**

- `README.md` topology-metrics table; `docs/kube-state-metrics-preconditions.md`
  (series → collector → RBAC table, `collectors`, `metricAllowlist`,
  `metricAnnotationsAllowList`, and the now-obsolete "not reachable by KSM
  config" section).

**Operators**

- Pod Applications now require the `deployments`, `statefulsets`, `daemonsets`,
  `replicasets`, `jobs` and `cronjobs` KSM collectors (widening the generated
  ClusterRole to those `apps` / `batch` resources) plus
  `--metric-annotations-allowlist=deployments=[argocd.argoproj.io/tracking-id],…`
  for each. Without them pods simply carry no `application`, as they already do
  today with stock KSM.
- The `kube-state-graph-demo` repository must replace its vmalert recording rule
  (which synthesises `argocd_tracking_id` from `label_app_kubernetes_io_instance`)
  with a real `argocd.argoproj.io/tracking-id` annotation on its workload
  manifests plus the annotation allowlist — a separate repository, tracked
  separately from this change.

**Not affected**

- No API surface change, no new HTTP route or parameter, no auth change, no new
  Go dependency, no service-graph or route-resolution code path, no change to
  `data.owner` or the `controller` compound group.
