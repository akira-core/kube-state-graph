# Deployment preconditions — kube-state-metrics

The graph reads **22** `kube_*` series. They come from **eleven**
kube-state-metrics collectors, which need `list` + `watch` on **eleven** resource
kinds and nothing else — no `get`, no `secrets`, no `configmaps`, no write verb.

Six collectors carry the graph itself. The other five
(`deployments`, `statefulsets`, `daemonsets`, `jobs`, `cronjobs`) exist **solely
to resolve pod ArgoCD Applications** and are individually optional — omit any of
them and the graph is unchanged except that pods of that controller kind carry
no `data.application`.

This document is the install-side companion to the *Topology metrics* table in
`README.md`, which specifies what each series is read for.

## Series → collector → RBAC

| KSM collector (`--resources`) | API group / resource | Series the graph reads |
|---|---|---|
| `pods` | `""` / `pods` | `kube_pod_info`, `kube_pod_owner`, `kube_pod_container_info`, `kube_pod_spec_volumes_persistentvolumeclaims_info` |
| `nodes` | `""` / `nodes` | `kube_node_info`, `kube_node_labels`, `kube_node_status_addresses`, `kube_node_status_condition` |
| `services` | `""` / `services` | `kube_service_info`, `kube_service_annotations` |
| `persistentvolumeclaims` | `""` / `persistentvolumeclaims` | `kube_persistentvolumeclaim_info`, `kube_persistentvolumeclaim_annotations` |
| `replicasets` | `apps` / `replicasets` | `kube_replicaset_owner`, `kube_replicaset_annotations` |
| `endpointslices` | `discovery.k8s.io` / `endpointslices` | `kube_endpointslice_endpoints`, `kube_endpointslice_labels` |
| `deployments` | `apps` / `deployments` | `kube_deployment_annotations` |
| `statefulsets` | `apps` / `statefulsets` | `kube_statefulset_annotations` |
| `daemonsets` | `apps` / `daemonsets` | `kube_daemonset_annotations` |
| `jobs` | `batch` / `jobs` | `kube_job_owner`, `kube_job_annotations` |
| `cronjobs` | `batch` / `cronjobs` | `kube_cronjob_annotations` |

Everything else KSM can collect — secrets, configmaps, ingresses, storageclasses,
horizontalpodautoscalers, resourcequotas, certificatesigningrequests, … — is
**unused**. The `storageclass`
node type was removed; a claim's StorageClass name is read from
`kube_persistentvolumeclaim_info`, not from the `storageclasses` collector.

Pod phase / restart counts / container resource requests are not read either: the
graph carries no pod health signal from KSM.

## Helm values

Chart: [`prometheus-community/kube-state-metrics`](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-state-metrics).
Value keys below are verified against **chart 8.4.0 / KSM appVersion 2.20.0**; the
`endpointslices` collector needs KSM ≥ v2.7. The chart really does spell its two
allowlists inconsistently — `metricLabelsAllowlist` (lowercase `l`) vs
`metricAnnotationsAllowList` (capital `L`) — that is not a typo here.

```yaml
# ksm-values.yaml — minimal kube-state-metrics for kube-state-graph.

# The chart's ClusterRole template is generated FROM this list, so trimming the
# collectors is what trims the RBAC. Eleven collectors cover all 22 series;
# the last five serve pod ArgoCD Applications only and can be dropped
# individually (see "Pod ArgoCD Application" below).
collectors:
  - endpointslices
  - nodes
  - persistentvolumeclaims
  - pods
  - replicasets
  - services
  # Pod ArgoCD Application only — drop any of these and pods of that controller
  # kind carry no data.application. Nothing else in the graph changes.
  - cronjobs
  - daemonsets
  - deployments
  - jobs
  - statefulsets

# Optional cardinality guard: expose only the series the graph reads. Drop this
# block if other consumers scrape the same KSM.
metricAllowlist:
  - kube_pod_info
  - kube_pod_owner
  - kube_pod_container_info
  - kube_pod_spec_volumes_persistentvolumeclaims_info
  - kube_node_info
  - kube_node_labels
  - kube_node_status_addresses
  - kube_node_status_condition
  - kube_service_info
  - kube_service_annotations
  - kube_persistentvolumeclaim_info
  - kube_persistentvolumeclaim_annotations
  - kube_replicaset_owner
  - kube_endpointslice_endpoints
  - kube_endpointslice_labels
  # Pod ArgoCD Application.
  - kube_job_owner
  - kube_deployment_annotations
  - kube_statefulset_annotations
  - kube_daemonset_annotations
  - kube_replicaset_annotations
  - kube_job_annotations
  - kube_cronjob_annotations

# Labels and annotations that are NOT KSM defaults.
metricLabelsAllowlist:
  # REQUIRED for service-selects-pod edges: joins an EndpointSlice to its Service.
  # Without it kube_endpointslice_labels carries no label_kubernetes_io_service_name
  # and every "://" connection string degrades to an `external` node.
  - endpointslices=[kubernetes.io/service-name]
  # Optional: propagates node labels into a node entry's data.labels.
  # - nodes=[topology.kubernetes.io/zone,topology.kubernetes.io/region]

# Optional: ArgoCD Application (data.application) on service / PVC nodes and —
# through the pod's controller — on pod nodes. The flag is per-resource, so each
# line can be dropped on its own; see "Pod ArgoCD Application" below for which
# families are worth their cardinality.
metricAnnotationsAllowList:
  - services=[argocd.argoproj.io/tracking-id]
  - persistentvolumeclaims=[argocd.argoproj.io/tracking-id]
  # Pod ArgoCD Application — one entry per controller kind you run.
  - deployments=[argocd.argoproj.io/tracking-id]
  - statefulsets=[argocd.argoproj.io/tracking-id]
  - daemonsets=[argocd.argoproj.io/tracking-id]
  - replicasets=[argocd.argoproj.io/tracking-id]
  - jobs=[argocd.argoproj.io/tracking-id]
  - cronjobs=[argocd.argoproj.io/tracking-id]

# Generate RBAC from `collectors` above.
rbac:
  create: true

# Sharding adds a namespaced Role (`pods` get + name-scoped `statefulsets`
# get/list/watch) and the graph does not benefit from it. Leave off unless the
# cluster forces it.
autosharding:
  enabled: false
```

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm upgrade --install kube-state-metrics prometheus-community/kube-state-metrics \
  --namespace monitoring --create-namespace \
  -f ksm-values.yaml
```

## The ClusterRole this produces

Audit target for the values above — and the role to supply by hand when binding
an externally-managed one. Note the chart guard is
`rbac.create == true && !rbac.useExistingRole`: to reuse an existing role, keep
`rbac.create: true` and set `rbac.useExistingRole: <name>`. `rbac.create: false`
skips the ServiceAccount binding as well, leaving KSM unauthorised.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kube-state-metrics
rules:
  - apiGroups: [""]
    resources:
      - nodes
      - pods
      - services
      - persistentvolumeclaims
    verbs: ["list", "watch"]
  - apiGroups: ["apps"]
    resources:
      - replicasets
      # Pod ArgoCD Application only.
      - deployments
      - statefulsets
      - daemonsets
    verbs: ["list", "watch"]
  - apiGroups: ["batch"]
    # Pod ArgoCD Application only.
    resources: ["jobs", "cronjobs"]
    verbs: ["list", "watch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["list", "watch"]
```

Drop the `batch` rule and the three `apps` entries marked *Pod ArgoCD
Application only* — together with their collectors — if pod-level
`data.application` is not wanted; nothing else in the graph depends on them.

`list` + `watch` is the informer contract — KSM lists once and then watches, so
neither verb can be dropped. `get` is never used. The role must stay cluster-wide:
`nodes` is cluster-scoped, and the graph is an estate-wide view, so a
`--namespaces`-restricted KSM silently truncates it. Keep the chart default
`rbac.useClusterRole: true` — setting it to `false` emits one namespaced `Role`
per entry in `.Values.namespaces` instead, which cannot grant `nodes`.

The collector list can also be edited in place with `collectorsExclude` /
`collectorsExtra`, which layer onto `.Values.collectors` rather than onto KSM's
own defaults. Spelling out the eleven wanted collectors is the clearer form here:
the RBAC is a direct read of that list.

## What KSM cannot supply — `cluster`, `az`, `env`

Every ID in the graph is cluster-scoped (`<cluster>/<uid>`, `<cluster>/<node>`,
`<cluster>/<namespace>/<claim>`), and the `az` / `env` request filters are pushed
upstream as **raw** label matchers (`az="…"`, `env="…"`, configurable per key via
`--az-label` / `--env-label`). None of these three is a KSM label:

- KSM emits no `cluster` label. The scraping agent must stamp one per source
  cluster — `external_labels` on `vmagent` / Prometheus, or the remote-write
  path into the centralised VictoriaMetrics. A series without it lands in the
  `unknown` bucket, addressable as `?cluster=unknown`.
- `az` / `env` must likewise be agent-stamped **external** labels. A node label
  exposed through `metricLabelsAllowlist` arrives as `label_topology_kubernetes_io_zone`
  on `kube_node_labels` **only** — it reaches neither the pod/claim/service series
  nor the kubelet and Harvest families, so it cannot serve as the `--az-label` key.
- The precondition is per-family: kube-state-metrics, kubelet **and** Harvest must
  all carry the configured `az` / `env` labels. A family that does not matches
  nothing under those filters, and since the default projection keeps only
  connectivity-connected workload, one missing label can empty a filtered graph
  rather than thin it. The build logs `selector_family_empty` (Warn) when KSM
  matched but kubelet / Harvest returned nothing.

## Pod ArgoCD Application comes from the pod's controller

ArgoCD stamps `argocd.argoproj.io/tracking-id` on the resources it **applies** —
the Deployment, StatefulSet, DaemonSet, CronJob — and **not** on the pods a
controller spawns. Services and PVCs are therefore straightforward: they are
ArgoCD-managed objects themselves, so `kube_service_annotations` /
`kube_persistentvolumeclaim_annotations` carry the annotation directly.

For pods the value is joined from the pod's **controller**. The reader keys on
`(cluster, namespace, owner_kind, owner_name)` — the controller owner it has
already resolved for `data.owner`, with the ReplicaSet skipped to its
Deployment — against one annotation family per controller kind:

| Pod's resolved owner kind | Series | Identity label |
|---|---|---|
| `Deployment` | `kube_deployment_annotations` | `deployment` |
| `StatefulSet` | `kube_statefulset_annotations` | `statefulset` |
| `DaemonSet` | `kube_daemonset_annotations` | `daemonset` |
| `ReplicaSet` (bare, no owning Deployment) | `kube_replicaset_annotations` | `replicaset` |
| `Job` | `kube_job_annotations` | `job_name` |
| `CronJob` (via the hop below) | `kube_cronjob_annotations` | `cronjob` |

The Job family's identity label is **`job_name`**, not `job` —
kube-state-metrics avoids Prometheus' reserved `job` target label.

**The Job → CronJob hop.** The Kubernetes CronJob controller copies only
`spec.jobTemplate.metadata` annotations onto the Jobs it creates, never the
CronJob object's own annotations, so ArgoCD's tracking-id never reaches a Job.
When a Job carries no annotation of its own, `kube_job_owner` resolves it to its
owning CronJob and the CronJob's annotation is used. A Job ArgoCD manages
directly keeps its own Application — the hop runs only on a miss. The hop is
**resolution-only**: a CronJob-managed pod's `data.owner` still names the Job.

**Kinds that cannot be covered.** `ReplicationController` has no
`kube_replicationcontroller_annotations` family in kube-state-metrics; `Node`
(static / mirror pods) and third-party CRD controllers (argo-rollouts
`Rollout`, OpenKruise `CloneSet`, …) have none either. Pods owned by those kinds
keep their `owner` and carry no `application`.

**Nothing here is required.** Absence degrades gracefully and **per family**,
because `--metric-annotations-allowlist` is per-resource: enable `deployments`
alone and Deployment-managed pods get Applications while every other kind stays
absent. A pod with no Application carries no `data.application` and no
`application` compound group; the only knock-on is the PVC *inheritance* path
(an app-less PVC borrows the lexically-smallest Application among its mounting
pods), which then has nothing to borrow.

**Cardinality.** `kube_replicaset_annotations` and `kube_job_annotations` are the
two expensive families — old ReplicaSets are retained under a Deployment's
`revisionHistoryLimit`, and Jobs accumulate under a CronJob's history limits —
and the ReplicaSet family is only ever consulted for a **bare** ReplicaSet, since
the normal case is already collapsed to its Deployment. If you run no
ArgoCD-managed bare ReplicaSets or Jobs, omit those two entries from
`metricAnnotationsAllowList` (and, if you also run `metricAllowlist`, drop
`kube_replicaset_annotations` / `kube_job_annotations` from it) and pay nothing —
kube-state-metrics emits an empty `_annotations` family when nothing is
allowlisted for that resource. Do **not** drop the `replicasets` or `jobs`
*collectors* to achieve this: `replicasets` also carries `kube_replicaset_owner`,
without which the ReplicaSet → Deployment owner skip stops and every
Deployment-managed pod loses both its `data.owner` collapse and its Application,
and `jobs` also carries `kube_job_owner`, without which the Job → CronJob hop
stops and CronJob-managed pods lose their Application. The per-resource
allowlist is the lever; the API server has no knob, because which families are
worth their cardinality is a deployment-shaped question.

**A note for deployments migrating off a custom exporter.** An earlier
arrangement had a customised exporter copy each pod's tracking-id annotation onto
`kube_pod_owner` as an `argocd_tracking_id` label. That label is **no longer
read**. It never had a stock producer — both allowlist flags write only to their
own metric family (`--metric-labels-allowlist` adds `label_*` to
`kube_<resource>_labels`, `--metric-annotations-allowlist` adds `annotation_*` to
`kube_<resource>_annotations`) and neither can enrich `kube_pod_owner`, while
scrape-time `metric_relabel_configs` rewrites one series in isolation and cannot
join a value in from another. Configure the controller-annotation families above
instead; the values are real ArgoCD tracking-ids, so a pod, its Service and its
PVC finally agree on one Application.

## HA and duplicate series

Running two KSM replicas (or a rollout landing inside the query window) yields
duplicate series that differ only in target labels. The builder is
duplicate-tolerant by construction: nodes dedupe by `(cluster, node)`, pod→PVC
bindings by `(PodID, PVCID)`, and every multi-sample join resolves ties by a
lexicographic pick, so the response stays byte-identical. No `honor_labels` or
deduplication rule is required for correctness.

## Verifying an install

Against the centralised VictoriaMetrics, per source cluster:

```promql
# 1. All 22 series present, and the `cluster` external label is stamped.
count by (__name__, cluster) (
  {__name__=~"kube_(pod|node|service|endpointslice|persistentvolumeclaim|replicaset|deployment|statefulset|daemonset|job|cronjob)_.+"}
)

# 2. The one REQUIRED non-default label — empty result means no
#    service-selects-pod edges and every "://" endpoint falls to `external`.
count(kube_endpointslice_labels{label_kubernetes_io_service_name!=""})

# 3. Optional ArgoCD annotations — service / PVC nodes.
count(kube_service_annotations{annotation_argocd_argoproj_io_tracking_id!=""})
count(kube_persistentvolumeclaim_annotations{annotation_argocd_argoproj_io_tracking_id!=""})

# 4. Optional ArgoCD annotations — pod nodes, one probe per controller kind.
#    An empty result for a family means pods of that controller kind carry no
#    data.application; it is not an error.
count(kube_deployment_annotations{annotation_argocd_argoproj_io_tracking_id!=""})
count(kube_statefulset_annotations{annotation_argocd_argoproj_io_tracking_id!=""})
count(kube_daemonset_annotations{annotation_argocd_argoproj_io_tracking_id!=""})
count(kube_replicaset_annotations{annotation_argocd_argoproj_io_tracking_id!=""})
count(kube_job_annotations{annotation_argocd_argoproj_io_tracking_id!=""})
count(kube_cronjob_annotations{annotation_argocd_argoproj_io_tracking_id!=""})

# 5. The Job → CronJob hop that CronJob-managed pods depend on.
count(kube_job_owner{owner_kind="CronJob",owner_is_controller="true"})

# 6. az / env reach the pod family too, not just nodes.
count by (az, env) (kube_pod_info)
```
