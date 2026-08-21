# Deployment preconditions — kube-state-metrics

The graph reads **15** `kube_*` series. They come from **six** kube-state-metrics
collectors, which need `list` + `watch` on **six** resource kinds and nothing
else — no `get`, no `secrets`, no `configmaps`, no write verb.

This document is the install-side companion to the *Topology metrics* table in
`README.md`, which specifies what each series is read for.

## Series → collector → RBAC

| KSM collector (`--resources`) | API group / resource | Series the graph reads |
|---|---|---|
| `pods` | `""` / `pods` | `kube_pod_info`, `kube_pod_owner`, `kube_pod_container_info`, `kube_pod_spec_volumes_persistentvolumeclaims_info` |
| `nodes` | `""` / `nodes` | `kube_node_info`, `kube_node_labels`, `kube_node_status_addresses`, `kube_node_status_condition` |
| `services` | `""` / `services` | `kube_service_info`, `kube_service_annotations` |
| `persistentvolumeclaims` | `""` / `persistentvolumeclaims` | `kube_persistentvolumeclaim_info`, `kube_persistentvolumeclaim_annotations` |
| `replicasets` | `apps` / `replicasets` | `kube_replicaset_owner` |
| `endpointslices` | `discovery.k8s.io` / `endpointslices` | `kube_endpointslice_endpoints`, `kube_endpointslice_labels` |

Everything else KSM can collect — deployments, statefulsets, daemonsets, jobs,
cronjobs, secrets, configmaps, ingresses, storageclasses, horizontalpodautoscalers,
resourcequotas, certificatesigningrequests, … — is **unused**. The `storageclass`
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
# collectors is what trims the RBAC. Six collectors cover all 15 series.
collectors:
  - endpointslices
  - nodes
  - persistentvolumeclaims
  - pods
  - replicasets
  - services

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

# Labels and annotations that are NOT KSM defaults.
metricLabelsAllowlist:
  # REQUIRED for service-selects-pod edges: joins an EndpointSlice to its Service.
  # Without it kube_endpointslice_labels carries no label_kubernetes_io_service_name
  # and every "://" connection string degrades to an `external` node.
  - endpointslices=[kubernetes.io/service-name]
  # Optional: propagates node labels into a node entry's data.labels.
  # - nodes=[topology.kubernetes.io/zone,topology.kubernetes.io/region]
  # Only for the recording-rule route to pod-level data.application (see below).
  # Feeds kube_pod_labels, which the graph does NOT read directly.
  # - pods=[app.kubernetes.io/instance]

# Optional: ArgoCD Application on service / PVC nodes (data.application).
metricAnnotationsAllowList:
  - services=[argocd.argoproj.io/tracking-id]
  - persistentvolumeclaims=[argocd.argoproj.io/tracking-id]

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
    resources: ["replicasets"]
    verbs: ["list", "watch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["list", "watch"]
```

`list` + `watch` is the informer contract — KSM lists once and then watches, so
neither verb can be dropped. `get` is never used. The role must stay cluster-wide:
`nodes` is cluster-scoped, and the graph is an estate-wide view, so a
`--namespaces`-restricted KSM silently truncates it. Keep the chart default
`rbac.useClusterRole: true` — setting it to `false` emits one namespaced `Role`
per entry in `.Values.namespaces` instead, which cannot grant `nodes`.

The collector list can also be edited in place with `collectorsExclude` /
`collectorsExtra`, which layer onto `.Values.collectors` rather than onto KSM's
own defaults. Spelling out the six wanted collectors is the clearer form here:
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

## Pod-level ArgoCD Application is not reachable by KSM config

`data.application` on **service** and **PVC** nodes comes from
`kube_service_annotations` / `kube_persistentvolumeclaim_annotations` and is fully
covered by `metricAnnotationsAllowList` above.

The **pod**-level value is read from an `argocd_tracking_id` label on
`kube_pod_owner` (`resolvePodApplications` in `pkg/build/topology.go`) — one
hardcoded label name on one metric, with no fallback source.

Stock KSM cannot produce it. Both allowlist flags write to their own metric
family and never enrich another one: `--metric-labels-allowlist` adds `label_*`
labels to `kube_<resource>_labels`, and `--metric-annotations-allowlist` adds
`annotation_*` labels to `kube_<resource>_annotations`. Neither touches
`kube_pod_owner`. Scrape-time `metric_relabel_configs` cannot help either — it
rewrites one series in isolation and cannot join a value in from another.

It is **not required**: absence degrades gracefully — pods carry no
`data.application` and no `application` compound group, while service and PVC
Applications keep working from their own annotations. The only loss beyond the
pod itself is the PVC *inheritance* path (an app-less PVC borrows the
lexically-smallest Application among its mounting pods), which has nothing to
borrow.

If pod-level Application is wanted, the two honest routes are:

1. **A recording rule** that re-publishes `kube_pod_owner` with the label joined
   in from a series that does carry it — e.g. `kube_pod_labels`'
   `label_app_kubernetes_io_instance` or `label_argocd_argoproj_io_instance`,
   which reach pods whenever the chart puts the tracking label in the pod
   template — via `group_left` + `label_replace`. Workable without touching the
   API server, at the cost of duplicating `kube_pod_owner`'s cardinality; a rule
   that records under a name it also reads is self-referential, so give it a
   guarded expression or accept the idempotent re-join.
2. **A code change** giving pods the same annotation path services and PVCs
   already use (`kube_pod_annotations` +
   `--metric-annotations-allowlist=pods=[argocd.argoproj.io/tracking-id]`).
   That metric is not currently queried, so it needs an OpenSpec change, not a
   config edit — and it only pays off where the annotation actually reaches the
   pod template, since ArgoCD stamps the tracking id on the managed Deployment,
   not on the pods it spawns.

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
# 1. All 15 series present, and the `cluster` external label is stamped.
count by (__name__, cluster) (
  {__name__=~"kube_(pod|node|service|endpointslice|persistentvolumeclaim|replicaset)_.+"}
)

# 2. The one REQUIRED non-default label — empty result means no
#    service-selects-pod edges and every "://" endpoint falls to `external`.
count(kube_endpointslice_labels{label_kubernetes_io_service_name!=""})

# 3. Optional ArgoCD annotations.
count(kube_service_annotations{annotation_argocd_argoproj_io_tracking_id!=""})
count(kube_persistentvolumeclaim_annotations{annotation_argocd_argoproj_io_tracking_id!=""})

# 4. az / env reach the pod family too, not just nodes.
count by (az, env) (kube_pod_info)
```
