---
paths:
  - "pkg/graph/**"
  - "pkg/cytoscape/**"
  - "pkg/build/topology*.go"
  - "pkg/build/clusteridentity*.go"
  - "pkg/build/status*.go"
  - "pkg/build/alerts*.go"
  - "pkg/build/podapplication*.go"
  - "internal/api/golden_test.go"
  - "internal/api/testdata/golden/**"
---

# Graph model

Disclosed reference for `CLAUDE.md`. The text is the authoritative statement of each rule; `CLAUDE.md` carries only a one-line reminder.

## Default projection

- **Default projection is the connectivity-connected subgraph.** Every `/v1/graph` response carries only the workload that sits on a **connectivity edge** (`pod-calls-pod` / `pod-calls-service` / `service-selects-pod`) plus the infra that hangs off it. Concretely: a pod is kept iff it is an endpoint of a connectivity edge; an edgeless pod is dropped, and with it (via the generalised D6 reference rule) the node hosting only edgeless pods, the PVC mounted only by edgeless pods, and the NetApp aggregate serving only such PVCs (and then its controller). **An unmounted PVC (no `pod-mounts-pvc` binding at all) is therefore dropped too** — a PVC is kept iff a connectivity-connected pod mounts it. **Service nodes are unaffected** — they are only ever materialised by the D29 connection-string resolver, so they are connectivity-born by construction (topology `kube_service_info` is index-only, never emitted as a node). The decision set is `graph.connectivityExcluded(g)` — a **pure function of the built graph** (scope-independent: a PVC's keep/drop depends on whether its mounting pod is *connected*, not on the request's cluster/namespace filter, and the two co-move because `pod-mounts-pvc` is intra-cluster/same-namespace), computed once in `graph.Project` and consulted in **both** `filterNodes` (skip excluded ids) **and** `filterEdges`/`readdEdgePartners` (an excluded pod/PVC is never resurrected as an edge partner — e.g. the pruned pod of a `pod-to-node` edge whose host node survived via another pod). The prune is **suppressed by exactly one escape hatch**: `?prune=false` (`graph.Scope.Inventory`, stored INVERTED so the zero `Scope` keeps the prune on). `?cluster=` / `?namespace=` / `?az=` / `?env=` do **not** disable the prune. **Consequence:** the default view is the traffic graph, not the inventory — an edgeless pod, an unmounted PVC, or a podless node is fetched with `?prune=false` (optionally narrowed by `cluster` / `namespace` / `az` / `env`). The prune itself stays a **projection concern** and a pure function of the built graph.

## Cluster identity

- **`<cluster>` is the composed cluster IDENTITY `<az>-<env>-<cluster>`, not the
  raw label.** A raw name is reused across zones and environments, so keying on
  it merges two estates into one id space. `build.clusterResolver`
  (`pkg/build/clusteridentity.go`) composes the identity at the ONE point a
  series' `cluster` label is read — `bucket(query, metric)`, ~20 call sites —
  so ids, `labels.cluster`, every join key and index, `ClusterFamilyKey`,
  cross-cluster status, `clusters[]` and the self-metric `cluster` VALUES all
  inherit it with no downstream code taught about zones. `pkg/cytoscape` and
  `pkg/route` are unchanged. Every cluster name — topology, kubelet, the
  service-graph trace label, the route store — walks one ladder: **compose**
  (both configured labels non-empty), else **adopt** (the raw name maps to
  exactly one identity in this build), else **verbatim** + one aggregated
  `cluster_identity_unresolved` Warn per metric. The identity table is built by
  a FIRST PASS in `parseTopology` over the four entity families only
  (`kube_pod_info`, `kube_node_info`, `kube_service_info`, the PVC binding); a
  join input can never invent a cluster that holds no entity. `unknown`
  composes like any other name (`us-dev-unknown`), keeping its raw component so
  `?cluster=unknown` still addresses it. Adoption cannot rescue a family under
  `?az=`/`?env=` — the matcher excludes it upstream before the reader sees it.
- **`?cluster=` is the RAW name at BOTH layers; `clusters[]` is the identity.**
  The upstream matcher is unchanged (`cluster="c1"`), and `pkg/graph/project.go`
  compares `Graph.ClusterRawName(labels["cluster"])`, so `?cluster=c1` admits
  every zone's `c1` and `?az=&env=&cluster=` pins one — the three request
  dimensions ARE the identity's components. A value read out of `clusters[]`
  and sent back as `?cluster=` returns an empty 200; that asymmetry is
  deliberate and documented in `docs/BREAKING.md`. The table reaches the graph
  as `Graph.ClusterIdentities`, assigned in `Builder.Build` right after
  `graph.NewGraph`; nil (an unstamped estate, a hand-built graph, an older
  embedder) degrades every comparison to the pre-identity behaviour, which is
  what keeps every existing golden byte-identical.
- **The cluster-family key runs over the identity string**, with the unchanged
  digit-run rule, so a family is scoped to one zone AND one environment
  (`us-dev-c1` ~ `us-dev-c2`, ≁ `eu-prod-c1`). Known widening: digits inside a
  zone or environment value normalise too (`us-east-1-prod-c1` ~
  `us-east-2-prod-c1`) — pinned by `TestClusterFamilyKey_OverClusterIdentities`
  so a future struct-aware key is a deliberate edit.

## Edge-type registry

- **`graph.EdgeTypes` is the builder's declaration table, served to nobody.**
  A single in-code registry: adding an edge type = update both the builder and
  the registry in the same change. Its `MayCrossCluster` bit derives
  `neverCrossCluster`, which buckets `kube_state_graph_graph_edge_count`, and
  the `pod-service-graph` spec pins the `may_cross_cluster: true` declaration
  on `pod-calls-service` — that is what keeps the registry load-bearing with
  no route serving it. Current edge types include `pod-calls-pod`,
  `pod-calls-service` (emitted when a `"://"` connection-string resolves to a
  service node in the caller's OWN cluster — that path stays intra-cluster —
  OR when the route engine resolves a global FQDN to a Service in the
  engine-selected ingress cluster, which may be a family sibling, so the type
  is `may_cross_cluster: true`; also used for the synthesized RouteHit
  ingress-chain hop from gateway pod → backend service), and
  `service-selects-pod` (directed service →
  pod, emitted on demand by the D29 connection-string resolution; the local
  service node fans out across same-family clusters holding the same-named
  Service, so it MAY be cross-cluster — `may_cross_cluster: true`), and
  `pvc-to-netapp-aggr` (PVC → ONTAP aggregate from the Harvest `volume_labels`
  join; `may_cross_cluster: false` — the target belongs to no Kubernetes
  cluster; I/O on `data.metrics`: `read_ops`, `write_ops`, `read_latency_us`,
  `write_latency_us`, `read_bytes_per_sec`, `write_bytes_per_sec`, plus the
  declared ceiling `max_iops`, `max_bytes_per_sec` — which the claim's PVC node
  also carries as `data.qos`), and
  `storage-flow` (the Sankey hop of `GET /v1/storage-graph`; `may_cross_cluster: false`; labels `tier` and `attribution`; `/v1/graph` never emits it).

## Node attributes

- **IP addresses live on the typed `ipaddress` attribute, never in `labels`.** `PodNode.IPAddress()` carries `[pod_ip]` from `kube_pod_info` (when present). `K8sNode.IPAddress()` carries `[external_ip]` from `kube_node_status_addresses{type="ExternalIP"}` when present, falling back to `[internal_ip]` from `kube_node_status_addresses{type="InternalIP"}` when the node has no ExternalIP row (ExternalIP always wins over InternalIP regardless of upstream sample order; within each type a duplicate `(cluster, node)` sample resolves to the lexically-smallest address; address types other than `ExternalIP`/`InternalIP` are ignored); omitted only when neither type is present. The selector is the anchored alternation `kube_node_status_addresses{type=~"ExternalIP|InternalIP"}` — a fixed, request-invariant metric-selection contract, not a caller filter. `ServiceNode.IPAddress()` carries `[cluster_ip]` from `kube_service_info` (when present, omitted for headless `cluster_ip="None"`). `PVCNode`, `ExternalNode`, `NetAppAggrNode`, and `NetAppNode` always return nil. `host_ip` from `kube_pod_info` is intentionally dropped — it is the node's IP, surfaced via the node entry instead. The serialiser emits `data.ipaddress` (with `omitempty`); `labels.pod_ip`, `labels.host_ip`, `labels.external_ip`, `labels.internal_ip`, and `labels.cluster_ip` MUST NOT appear.
- **Cytoscape compound nodes are presentation-only — workload hierarchy plus storage chain.** `pkg/cytoscape` synthesises `type="cluster"` / `type="storage-cluster"` / `type="namespace"` / `type="application"` / `type="controller"` groups (all `labels={}`, no `ipaddress`, emitted in that tier order each sorted by id, before real nodes) and sets `data.parent` (`omitempty`) for `cluster > namespace > application > controller > pod` with **skip-absent-levels**, plus `cluster > namespace > [application >] {service, pvc}`, `cluster > node`, and `storage-cluster > netapp-node > netapp-aggr` and `storage-cluster > netapp-svm`. The **real** `type="netapp-node"` is the compound parent of its aggregates (via `labels.node`) — the one scoped exception to "relationships are edges, groups are synthesised". An SVM nests under its storage-cluster, never under a controller. `external` nodes get no parent. Group ids are **path-encoded**. **NetApp types** (`NodeTypeNetAppAggr` id `netapp/<oc>/aggr/<aggr>`, `NodeTypeNetAppNode` id `netapp/<oc>/<node>`, `NodeTypeNetAppSVM` id `netapp/<oc>/svm/<svm>`) belong to no Kubernetes cluster (`labels` carry `ontap_cluster`, never `cluster`) so they stay out of `clusters[]` and `?cluster=`. The SVM is emitted only by `/v1/storage-graph`. The PVC→aggregate relationship is the `pvc-to-netapp-aggr` edge (Harvest `volume_labels`, matching a token derived from the PV name against the stock `volume` label); the pod→node relationship is `pod-to-node`. The `storageclass` node type and `pvc-to-storageclass` edge are **removed**; the claim's StorageClass name survives as `PVCNode.StorageClass()` / `data.storageclass`. Infra admission (D6, transitive for NetApp): a K8s `node` is retained iff a pod is scheduled on it; a `netapp-aggr` iff an admitted PVC has a `pvc-to-netapp-aggr` edge to it; a `netapp-node` iff an admitted aggregate names it. An admitted aggregate always pulls its owning controller. See `pkg/build/netapp.go` and `docs/netapp-harvest-preconditions.md`.
- **Pod controller-owner attribute (D34).** Each `type="pod"` node carries a typed, nullable `owner` attribute — `data.owner = {kind, name}`, serialised with `omitempty` and **omitted entirely** when the pod has no controller owner (never empty strings). It lives on the typed attribute, **never inside `labels`** (which stay strict typological metadata) — same precedent as `ipaddress`. Surfaced via `graph.GraphNode.Owner() *graph.Owner` (nil for non-pods and ownerless pods). Resolved from `kube_pod_owner` with the **ReplicaSet skipped to its owning Deployment** via `kube_replicaset_owner` (a bare ReplicaSet with no Deployment owner stays `kind="ReplicaSet"`; other owner kinds surface verbatim). Both series are KSM defaults (no `--metric-labels-allowlist`) and OPTIONAL (absence degrades gracefully, no build failure). Resolution lives in `pkg/build/topology_owners.go` (`resolvePodOwners`); the controller pick is deterministic (lexically-smallest `(kind, name)` on collision). No new node/edge type.
- **Pod `application` and `containers` attributes.** Each `type="pod"` node may carry two more typed, nullable attributes, both serialised with `omitempty` and **never inside `labels`** — same precedent as `owner` / `ipaddress`. (1) `data.application` (string) is the pod's ArgoCD Application, joined from the pod's **CONTROLLER** — ArgoCD stamps `argocd.argoproj.io/tracking-id` on the workload object it applies, never on the pods a controller spawns, and no `argocd_tracking_id` label is read off `kube_pod_owner` (see `docs/BREAKING.md`). The lookup key is the controller owner already resolved for `data.owner` (`(cluster, namespace, owner_kind, owner_name)`, ReplicaSet already collapsed to its Deployment) against one annotation family per kind — `kube_deployment_annotations` / `kube_statefulset_annotations` / `kube_daemonset_annotations` / `kube_replicaset_annotations` (bare RS only) / `kube_job_annotations` (identity label `job_name`, NOT `job`) / `kube_cronjob_annotations` — each carrying the same `annotation_argocd_argoproj_io_tracking_id` label as the service / PVC families and parsed as the segment **before the first `:`** of the tracking-id value (`<app>:<group>/<kind>:<ns>/<name>`; a value with no `:` is verbatim); per-controller collisions pick the lexically-smallest non-empty tracking-id. The ONE extra hop is **Job → CronJob** via `kube_job_owner` (`owner_kind="CronJob"` + `owner_is_controller="true"`), tried only when the Job carries no annotation of its own — the Kubernetes CronJob controller copies only `spec.jobTemplate.metadata` annotations onto its Jobs. That hop is **resolution-only**: `resolvePodOwners` never reads the index, so `data.owner` still reads `{kind:"Job", …}`. An owner kind with no KSM annotation family (`ReplicationController`, `Node`, any CRD controller) resolves no Application. Every family is OPTIONAL and degrades **per family** (`--metric-annotations-allowlist` is per-resource). All seven queries carry a **fixed selector** mirroring their reader's own discard — `kube_job_owner{owner_kind="CronJob",owner_is_controller="true"}` and `kube_*_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` — so an un-allowlisted family returns an EMPTY vector instead of one series per workload object, and `Topology.RawSeriesCount` for these seven counts matched (annotated / CronJob-controlled) objects, not all of them. On the **query-error** axis the seven split: `kube_replicaset_annotations` and `kube_job_annotations` are `fetchOptional` (log-and-continue — their cardinality accumulates with `revisionHistoryLimit` / Job history limits); the other four families and `kube_job_owner` stay abort-on-error `fetch`. Every degrade is **subtractive** — it removes an Application, never substitutes one — but a lost Application still reshapes the Cytoscape compound hierarchy (pods reparent, a sole-member `application` group node vanishes, an inheriting PVC re-inherits from a different mounter). Keeping it subtractive costs one gate: a degraded `kube_job_annotations` **suppresses the Job → CronJob hop** for that build (`topologyVectors.JobAnnotationsDegraded`, set only by `fetchOptionalTracking` on the swallowed-error path). The hop is gated on "this Job carries no annotation of its own", which an unread family cannot establish, so firing it would attribute a directly-managed Job's pod to its CronJob's Application — the one wrong-value degrade in the package. `kube_replicaset_annotations` needs no such flag: a bare ReplicaSet has no further ancestor to consult. Surfaced via `graph.GraphNode.Application() string` (`""` for non-pods and ArgoCD-less pods), resolved in `pkg/build/topology_owners.go` (`resolvePodApplications` + `resolveControllerApplications` + `resolveJobCronJobOwners`). **One pod on demand:** `build.ResolvePodApplication(ctx, LabelQuerier, PodApplicationRequest{AZ, Env, Cluster, Namespace, Pod, At, Window})` applies the same rules through `Router.QueryLabels` (family `ksm`, 2–5 sequential queries, no build); `Cluster` / `Namespace` / `Pod` are required, `AZ` / `Env` optional like on the graph request. It shares `controllerAnnotationFamilies` / `argoAppName` / `usableTrackingID` with the batch path, pins each unset `az` / `env` from the pod-owner rows so later legs route and match that zone (`ErrAmbiguousPod` on several combinations), and fails on any upstream error instead of degrading (add-pod-application-lookup). **Service and PVC nodes also carry `data.application`** (the `containers` attribute stays pod-only): resolved identically (segment before the first `:`, lexically-smallest on collision) from the `annotation_argocd_argoproj_io_tracking_id` label on `kube_service_annotations` / `kube_persistentvolumeclaim_annotations` (KSM's sanitised form of the `argocd.argoproj.io/tracking-id` annotation, gated on `--metric-annotations-allowlist`), via `resolveServiceApplications` / `resolvePVCApplications` (sharing the `argoAppName` + generic `resolveApplications` helper with the pod resolver). The PVC value is set at topology assembly; the **service** value is threaded into the connection-string resolver (`Topology.ServiceApplications` → `sgResolver.serviceApps`) since service nodes are materialised there. **PVC application inheritance (D13):** a PVC with **no** Application of its own additionally **inherits** the lexically-smallest Application among the pods that mount it (the `pod-mounts-pvc` bindings), via `pvcInheritedApps` in a post-PVC-loop pass at topology assembly (joins each binding's pod ID to the pod's already-resolved `Application()`). The PVC's **own** annotation always wins (the pass fills only app-less PVCs); the inherited value is baked onto `PVCNode.Application()` **before** `graph.NewGraph` freezes the nodes, so it is **indistinguishable** from an annotation-sourced value in `data.application` and drives the same `application` compound group. Because `pod-mounts-pvc` is intra-cluster and same-namespace, inheritance never crosses cluster/namespace; it is resolved over the full graph before projection (a `?cluster=`/`?namespace=` filter dropping the mounting pod does not change the PVC's app), and the min over the binding set is order-free (D6). No new query, metric, node, or edge type. So `Application()` returns non-empty on `PodNode` / `ServiceNode` / `PVCNode`, and these `application` values additionally drive the `application` compound group for all three (`controller` groups stay pod-only). (2) `data.containers` (`[{name, image}]`) is the pod's container list from `kube_pod_container_info` (one series per container/image; a topology leg rendered as `tlast_over_time(...)` so each series' value is its last-sample timestamp), ordered by `(name, image)` for determinism, empty-`image` series skipped, and — when a container reports more than one image in the window (each image a distinct series) — the **latest-seen image** kept (greatest last-sample timestamp; lexically-smallest image on an exact tie). The latest pick is reliable for near-now windows; for windows far from the real wall clock VM returns only one image-variant per container (see design.md D-A4), so a far-past window surfaces whatever single variant VM returns (never worse than a fixed pick). Surfaced via `graph.GraphNode.Containers() []graph.Container` (`nil` for non-pods and pods with no container info), resolved in `pkg/build/topology_owners.go` (`resolvePodContainers`). The container read is a KSM default (no `--metric-labels-allowlist`); the controller-annotation families need the operator's `--metric-annotations-allowlist`. All are OPTIONAL (absence degrades gracefully, no build failure). `kube_pod_container_info` also DEGRADES on a query error (`fetchOptional`, harden-topology-read-cardinality D1 — its cardinality multiplies with containers, image variants and pod churn, so it is the leg an upstream series limit rejects first), and `/v1/storage-graph` never reads it, so a storage-graph pod carries no `data.containers`. No new node/edge type.
- **Node `status` attribute.** `PodNode`, `K8sNode`, `PVCNode`,
  `NetAppNode`, and `NetAppAggrNode` always carry one of `"normal"`,
  `"warning"`, or `"critical"` in `data.status`; services, externals, SVMs,
  and synthesised compound groups omit it. `attachStatus` runs immediately
  after `attachAlerts` on both build paths and stores the pure
  `graph.FoldStatus` result before `graph.NewGraph`: alert severity is compared
  case-insensitively (`critical` → critical; `warning`, empty, or unrecognised
  → warning; `info` / `none` → no effect), `health="degraded"` and
  `ready_status="NotReady"` → critical, and `ready_status="Unknown"` →
  warning; worst wins, else normal. `data.perf` is deliberately not read —
  thresholds belong in alert rules. `"normal"` means no negative signal was
  observed, NOT that every optional signal source was present. Status never
  propagates to a parent or neighbour; consumer-side group roll-up remains a
  view concern.
- **K8s node `ready_status` attribute.** Each `type="node"` node may carry a typed, nullable `ready_status` attribute — `data.ready_status` (a string), serialised with `omitempty` and **never inside `labels`** — same precedent as `ipaddress` / `owner`. The value is one of `"Ready"`, `"NotReady"`, `"Unknown"`, derived from `kube_node_status_condition{condition="Ready"}` (a new topology query in the `ReadTopology` errgroup; the `condition="Ready"` selector is a fixed, **request-invariant metric-selection contract** — same class as the node-address `type` selector and the D30 sentinel — NOT a caller filter, and it is rendered ahead of any request-scoped matcher). The reader reads the `status` label of the **active** row (sample value `1`), matched **case-insensitively**: `true`→`Ready`, `false`→`NotReady`, `unknown`→`Unknown`. Status-label casing is NOT pinned by the KSM-shaped contract — stock kube-state-metrics lowercases it (`addConditionMetrics`→`strings.ToLower`), but an exporter that re-publishes the raw Kubernetes `v1.ConditionStatus` enum verbatim emits `True`/`False`/`Unknown`; both resolve (the reader canonicalises to lowercase at the read site). **Absence is distinct from `"Unknown"`**: `data.ready_status` is omitted entirely when the metric is absent, the node has no `condition="Ready"` series, or no row is active — `"Unknown"` is reserved for the genuine Kubernetes state where the kubelet has stopped reporting; the two MUST NOT be conflated (no defaulting missing data to `"Unknown"`). Surfaced via `graph.GraphNode.ReadyStatus() string` (`""` for non-nodes and nodes with no Ready data), resolved in `pkg/build/topology_attrs.go` (`resolveNodeReadyStatus`, keyed `(cluster, node)` like the IP/label joins); on the defensive multi-active tie the lexically-smallest `status` wins (determinism). The metric is a KSM default and OPTIONAL (absence degrades gracefully, no build failure). No new node/edge type.

## Sealed graph types

`graph.GraphNode` is a sealed interface (`isGraphNode()` unexported). Concrete
types: `PodNode`, `K8sNode`, `PVCNode`, `ServiceNode`, `ExternalNode`,
`NetAppAggrNode`, `NetAppNode`, `NetAppSVMNode`. All
expose `ID()`, `Name()`, `Type()`, `Labels()`, `IPAddress()`, `Owner()`,
`Application()`, `Containers()`, `ReadyStatus()`, `Health()`, `Usage()`,
`StorageClass()`, `QoS()`, `Hardware()`, `Perf()`, `Alerts()`, `Status()`. Serialisation
goes through these methods — never through type switches in the serialiser.
`IPAddress()` returns nil for `PVCNode` / `ExternalNode`; `PodNode` returns
`[pod_ip]` when known;
`K8sNode` returns `[external_ip]` when known; `ServiceNode` returns
`[cluster_ip]` when known (nil when headless `cluster_ip="None"`).
`Owner() *graph.Owner` returns the controller owner (`{Kind, Name}`) for
`PodNode` when known and nil for every other node kind and for ownerless pods
(D34) — serialised as the `omitempty` `data.owner` object.
`Application() string` returns the ArgoCD Application of a `PodNode`,
`ServiceNode`, or `PVCNode` (`""` for `K8sNode` / `ExternalNode` /
`NetAppAggrNode` / `NetAppNode` and for ArgoCD-less pods/services/pvcs) and
`Containers() []graph.Container` returns a `PodNode`'s ordered `{name, image}`
list (`nil` for every other node kind) — serialised as the `omitempty`
`data.application` / `data.containers` attributes.
`ReadyStatus() string` returns a `K8sNode`'s Kubernetes Ready-condition status
(`"Ready"` / `"NotReady"` / `"Unknown"`; `""` for every other node kind and for
nodes with no Ready data) — serialised as the `omitempty` `data.ready_status`
attribute. `""` (omitted) is distinct from `"Unknown"` (kubelet lost contact).
`Health() string` returns `"online"` / `"degraded"` for NetApp types (`""` otherwise;
absence ≠ degraded). `Usage() *UsageBytes` returns kubelet/Harvest used+capacity
bytes for PVC and aggregate nodes. `StorageClass() string` is the PVC's own
policy name (`data.storageclass`). `QoS() *graph.QoSCeiling` returns a PVC's
declared throughput ceiling `{PolicyGroup, MaxIOPS, MaxBytesPerSec}` (`nil` for
every other node kind and for a PVC whose ceiling did not resolve; non-nil
implies at least one figure) — serialised as the `omitempty` `data.qos` object,
both figures rounded like the edge's. `Status() string` returns the baked
`"normal"` / `"warning"` / `"critical"` verdict for pods, K8s nodes, PVCs,
NetApp controllers, and aggregates, and `""` for services, externals, and SVMs.
