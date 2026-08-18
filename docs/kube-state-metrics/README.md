# Setting up kube-state-metrics for kube-state-graph

kube-state-graph builds its topology (pods, nodes, PVCs, services,
StorageClasses, owners, containers, annotations) exclusively from
kube-state-metrics (KSM) `kube_*` series read out of one centralised
VictoriaMetrics. This guide configures a per-cluster KSM instance so that
**every** KSM-shaped series the graph consumes is present with the exact
label contract the reader expects.

Files in this directory:

| File | Purpose |
|---|---|
| `README.md` | This walkthrough |
| `values.yaml` | Helm values for `prometheus-community/kube-state-metrics` |
| `custom-resource-state.yaml` | Standalone custom-resource-state config (for non-Helm installs) |

The authoritative list of consumed metrics lives in
[`pkg/promql/queries.go`](../../pkg/promql/queries.go); the label contract is
implemented in [`pkg/build/topology.go`](../../pkg/build/topology.go). This
document mirrors both — if they ever disagree, the code wins.

## What the graph consumes, and what it takes to produce it

**Stock KSM defaults — no configuration needed:**

| Metric | Feeds |
|---|---|
| `kube_pod_info` | pod nodes, `pod_ip`, pod → node placement |
| `kube_node_info` | node nodes; also `/v1/clusters` discovery |
| `kube_node_status_addresses` | node `ipaddress` (ExternalIP, InternalIP fallback) |
| `kube_node_status_condition` | node `ready_status` (`condition="Ready"`) |
| `kube_pod_spec_volumes_persistentvolumeclaims_info` | `pod-mounts-pvc` edges |
| `kube_service_info` | service index + `cluster_ip` |
| `kube_pod_owner` | pod `owner` (controller) |
| `kube_replicaset_owner` | ReplicaSet → Deployment owner skip |
| `kube_persistentvolumeclaim_info` | PVC `storageclass` + `volumename` labels |
| `kube_pod_container_info` | pod `containers` (`{name, image}`) |

**Needs explicit KSM configuration (all covered by `values.yaml`):**

| Metric | Configuration |
|---|---|
| `kube_endpointslice_endpoints` | `endpointslices` is **not** in KSM's default collector list — add it |
| `kube_endpointslice_labels{label_kubernetes_io_service_name}` | collector **plus** `--metric-labels-allowlist=endpointslices=[kubernetes.io/service-name]` |
| `kube_node_labels{label_*}` | `--metric-labels-allowlist=nodes=[*]` (or a narrower list) |
| `kube_service_annotations{annotation_argocd_argoproj_io_tracking_id}` | `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id]` |
| `kube_persistentvolumeclaim_annotations{annotation_argocd_argoproj_io_tracking_id}` | `--metric-annotations-allowlist=persistentvolumeclaims=[argocd.argoproj.io/tracking-id]` |
| `kube_storageclass_info` + parameter labels | custom-resource-state config (stock KSM omits `.parameters`) |
| `kube_tridentvolume_info`, `kube_tridentbackend_info` | custom-resource-state config over the Trident CRDs (NetApp clusters only) |

**Out of KSM's reach — different producers, listed for completeness:**

| Metric | Producer |
|---|---|
| `traces_service_graph_request_total` (+ `_failed_total`, `_server_seconds_bucket`) | Tempo / Grafana Alloy servicegraph connector (trace pipeline) |
| `up` | Prometheus/VictoriaMetrics scrape machinery (readiness probe) |

Every optional series degrades gracefully — a missing metric never fails a
build, the corresponding attribute/label is simply absent.

## Prerequisites

- KSM **v2.10+** (endpointslice `targetref_*` labels, stable
  custom-resource-state support).
- A scrape agent per cluster (vmagent / Prometheus agent / Alloy) remote-writing
  into the centralised VictoriaMetrics.
- For `data.application` on services/PVCs: ArgoCD annotation-based tracking
  (`application.resourceTrackingMethod: annotation`), so the
  `argocd.argoproj.io/tracking-id` annotation exists on managed objects.

## Step 1 — Install KSM (Helm)

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm upgrade --install kube-state-metrics prometheus-community/kube-state-metrics \
  --namespace monitoring --create-namespace \
  -f docs/kube-state-metrics/values.yaml
```

`values.yaml` does four things:

1. **Collectors** — chart default **plus `endpointslices`**, **minus
   `storageclasses`** (the CRS config re-emits `kube_storageclass_info` with
   parameter labels; running both would duplicate the metric family in one
   exposition).
2. **Label allowlist** — `endpointslices=[kubernetes.io/service-name]`
   (mandatory for the service → pod join) and `nodes=[*]`.
3. **Annotation allowlist** — the ArgoCD tracking-id on services and PVCs.
4. **Custom-resource-state + RBAC** — StorageClass parameters and the NetApp
   Trident chain (drop the Trident blocks on non-NetApp clusters).

### Non-Helm installs

Add to the KSM container args (mirror of the above):

```
--resources=certificatesigningrequests,configmaps,cronjobs,daemonsets,deployments,endpoints,endpointslices,horizontalpodautoscalers,ingresses,jobs,leases,limitranges,mutatingwebhookconfigurations,namespaces,networkpolicies,nodes,persistentvolumeclaims,persistentvolumes,poddisruptionbudgets,replicasets,replicationcontrollers,resourcequotas,secrets,services,statefulsets,validatingwebhookconfigurations,volumeattachments
--metric-labels-allowlist=endpointslices=[kubernetes.io/service-name],nodes=[*]
--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id],persistentvolumeclaims=[argocd.argoproj.io/tracking-id]
--custom-resource-state-config-file=/etc/ksm/custom-resource-state.yaml
```

Mount [`custom-resource-state.yaml`](./custom-resource-state.yaml) via a
ConfigMap at that path, and extend the KSM ClusterRole:

```yaml
- apiGroups: ["trident.netapp.io"]
  resources: ["tridentvolumes", "tridentbackends"]
  verbs: ["get", "list", "watch"]
```

## Step 2 — The `cluster` label (mandatory, not KSM's job)

Every series must carry a `cluster` label naming its origin cluster — it is
the multi-cluster join key for every topology index. KSM does not add it;
inject it on the scrape/remote-write path, **one unique value per cluster**:

vmagent:

```yaml
# -remoteWrite.label=cluster=prod-01     (flag form), or in scrape config:
global:
  external_labels:
    cluster: prod-01
```

Prometheus (agent mode):

```yaml
global:
  external_labels:
    cluster: prod-01
```

Grafana Alloy:

```alloy
prometheus.remote_write "central" {
  endpoint { url = "https://vm.example.com/api/v1/write" }
  external_labels = { cluster = "prod-01" }
}
```

Series arriving without `cluster` are bucketed as cluster `"unknown"` — the
graph still builds, but cross-metric joins for those series only work within
the `"unknown"` bucket. Cluster naming also drives the **family** rule for
cross-cluster service fan-out (`prod-03` / `prod-12` are one family: names
match after collapsing digit runs) — name clusters consistently.

## Step 3 — Label-name fixups (case-sensitive contract)

The reader consumes StorageClass parameter labels **verbatim, case-sensitive**:
`storagePools`|`pool`, `fsType`|`fsName`, `ClusterID`, `selector`. The CRS
wildcard (`"*": [parameters]`) emits parameter keys as-is:

- **NetApp Trident** SCs (`storagePools`, `fsType`, `selector`) match directly.
- **Ceph-CSI** SCs use `clusterID` / `fsName` / `pool` — `fsName` and `pool`
  match, but `clusterID` ≠ `ClusterID`. Rename at scrape time:

```yaml
metric_relabel_configs:
  - source_labels: [__name__, clusterID]
    regex: "kube_storageclass_info;(.+)"
    target_label: ClusterID
    replacement: "$1"
```

Same technique applies to any driver whose parameter keys differ from the
contract.

## Step 4 — Metric-name prefix (optional)

If your pipeline rewrites KSM metric names under a prefix (e.g.
`o11y_kube_pod_info`), start the API server with `KSG_METRIC_PREFIX=o11y_`.
The prefix applies to **KSM-shaped series only** — never to `traces_*` or
`up` — and is prepended verbatim (trailing underscore is on you).

## Step 5 — Verify

At the KSM pod (fast feedback, before remote-write):

```bash
kubectl -n monitoring port-forward svc/kube-state-metrics 8080:8080 &
curl -s localhost:8080/metrics | grep -E \
  '^kube_(endpointslice_labels|storageclass_info|tridentvolume_info|tridentbackend_info|service_annotations)' | head
```

At the centralised VictoriaMetrics (end-to-end, per cluster):

```bash
VM=https://vm.example.com
q() { curl -sG "$VM/api/v1/query" --data-urlencode "query=$1"; }

q 'count by (cluster) (kube_pod_info)'                    # every cluster present?
q 'count(kube_endpointslice_labels{label_kubernetes_io_service_name!=""})'
q 'count by (cluster) (kube_endpointslice_endpoints{targetref_kind="Pod"})'
q 'count(kube_storageclass_info{provisioner!=""})'
q 'count(kube_service_annotations{annotation_argocd_argoproj_io_tracking_id!=""})'
q 'count(kube_tridentvolume_info{backendUUID!=""})'       # NetApp clusters
q 'count(kube_tridentbackend_info{svm!=""})'              # NetApp clusters
q 'count by (cluster) (kube_node_status_condition{condition="Ready"})'
```

Then hit the graph API: `GET /v1/graph` should show `service-selects-pod`
edges (endpointslice join alive), `data.application` on annotated
services/PVCs, `provisioner`/`parameters` on StorageClass nodes, and
`volumename`/`svm` labels on Trident-backed PVCs.

## Known gaps

- **`argocd_tracking_id` on `kube_pod_owner`** (pod-level `data.application`):
  **not producible by stock KSM.** The label allowlist only decorates
  `kube_pod_labels` (as `label_*`), never `kube_pod_owner`, and ArgoCD tracks
  top-level resources — pods don't carry the tracking-id at all. This label
  requires a patched/custom exporter or a pipeline capable of cross-series
  joins. Without it the pod `application` attribute is simply absent;
  service/PVC `application` (annotation-based, Step 1) still works, and the
  `application` compound group is derived from whichever of the three carries
  a value.
- **TridentBackend `svm` path**: `[config, ontap_config, svm]` matches the
  ONTAP driver family. Verify against your Trident version
  (`kubectl get tridentbackends -n trident -o yaml`) and adjust
  `custom-resource-state.yaml` if the nesting differs.

## Appendix — full label contract per metric

Labels the reader actually consumes (extra labels are ignored). `cluster` is
consumed on every row (Step 2).

| Metric | Consumed labels |
|---|---|
| `kube_pod_info` | `namespace`, `pod`, `uid`, `node`, `pod_ip` |
| `kube_node_info` | `node` |
| `kube_node_status_addresses` | `node`, `type` (`ExternalIP`/`InternalIP`), `address` |
| `kube_node_status_condition` | `node`, `condition` (=`Ready`), `status` |
| `kube_node_labels` | `node`, every `label_*` |
| `kube_pod_spec_volumes_persistentvolumeclaims_info` | `namespace`, `pod`, `persistentvolumeclaim` (fallback `claim_name`), `volume` |
| `kube_service_info` | `namespace`, `service`, `cluster_ip` |
| `kube_endpointslice_labels` | `namespace`, `endpointslice`, `label_kubernetes_io_service_name` |
| `kube_endpointslice_endpoints` | `namespace`, `endpointslice`, `targetref_kind`, `targetref_namespace`, `targetref_name` |
| `kube_pod_owner` | `namespace`, `pod`, `owner_kind`, `owner_name`, `owner_is_controller`; optional `argocd_tracking_id` (see Known gaps) |
| `kube_replicaset_owner` | `namespace`, `replicaset`, `owner_kind` (=`Deployment`), `owner_name` |
| `kube_persistentvolumeclaim_info` | `namespace`, `persistentvolumeclaim`, `storageclass`, `volumename` |
| `kube_storageclass_info` | `storageclass`, `provisioner`, `storagePools`\|`pool`, `fsType`\|`fsName`, `ClusterID`, `selector` |
| `kube_pod_container_info` | `namespace`, `pod`, `container`, `image` |
| `kube_service_annotations` | `namespace`, `service`, `annotation_argocd_argoproj_io_tracking_id` |
| `kube_persistentvolumeclaim_annotations` | `namespace`, `persistentvolumeclaim`, `annotation_argocd_argoproj_io_tracking_id` |
| `kube_tridentvolume_info` | `name` (= PV name), `backendUUID` |
| `kube_tridentbackend_info` | `backendUUID`, `svm` |
