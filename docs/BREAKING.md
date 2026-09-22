# BREAKING changes — harden the topology read against upstream series limits

A `/v1/storage-graph` build no longer reads what its body cannot carry, and it
reads pods by reference. `/v1/graph` bodies are unchanged; one of their legs
now degrades instead of failing the build.

## Non-breaking: `/v1/storage-graph` accepts an `application=` root

*storage-graph-api — Root selectors from either end of the flow; Roots are always materialised when the upstream knows them; Storage-reachability projection; Storage build reads only what it draws; Pod-only roots narrow the upstream read; Deterministic storage-graph body. cluster-topology-source — Application-rooted recovery reads of the owner and annotation families.*

Not a compatibility break on the wire. `application=<argo-app>` is an optional,
repeatable workload root. A value is the ArgoCD Application name exactly as
`data.application` carries it (the tracking-id segment before the first `:`).
A path is retained when its pod or its claim carries that Application. Every
loaded pod that resolves it is materialised, including a pod that mounts no
claim; a claim is never materialised on its own. A request that does not send
`application=` issues the same queries and returns a byte-identical body.

The build recovers those pods before it reads them: controller-annotation
families restricted by tracking-id prefix, then `kube_replicaset_owner` /
`kube_job_owner`, then `kube_pod_owner`. The recovery only supplies pod names.
Under an application root the claim-binding half of the pod scope narrows to
related claims, and `application=` suppresses the pod-only namespace derivation.

### In-process embedders

`graph.NewStorageScope` gains an `applications []string` parameter.
`graph.StorageRoots` gains `Applications map[string]struct{}`. Pass `nil` for
a request with no application root. `ParseStorageValues` fills the field from
`application=`.

## Non-breaking: a storage-rooted request restricts the `volume_labels` read

*storage-graph-api — Storage-side roots narrow the Harvest topology read (scope-volume-labels-by-storage-root)*

Not a compatibility break: every `/v1/storage-graph` body is byte-identical to
today's for an estate whose aggregates and controllers are each named by their
own Harvest gauge families (the stock `aggr_*` and `node_*` templates). What
changes is one upstream query.

Previously `volume_labels` — the largest leg in a NetApp estate, and the one leg
no request parameter narrowed — was read for the WHOLE filer on every request,
including a request that named a single aggregate. It is now read restricted to
the rooted components when the request carries `ontap_cluster=` and/or `aggr=`
and carries no `svm=` and no `node=`: phase 1 issues
`volume_labels{cluster=…,aggr=…}` (the two matchers AND-combined), and phase 2
re-reads it for the derived tokens of exactly the claims phase 1 matched, so each
claim's aggregate and SVM are still picked over its whole candidate set. Nothing
inverts a FlexVol name back to a PV name; phase 2 renders the derivation the join
already computes, forward.

What operators may notice:

- A rooted build issues one more sequential Harvest hop (phase 2), and — when a
  claim matched — more `volume_labels` queries under the one family name. The
  per-family series-count histogram and the `raw_series_counts` Debug log for
  `volume_labels` now track the rooted components rather than the filer.
- **`netapp_volume_join_miss` counts differently on a storage-rooted request.**
  There it counts only claims that matched at least one volume-label series, so
  a FlexGroup still reports while a claim off the rooted components does not. A
  derivation that fits no claim at all therefore reports nothing on a rooted
  request — alert on it from an unrooted one.
- **The restriction is capped and falls back.** `ontap_cluster=` and `aggr=` are
  repeatable and nothing bounds how many values a request may carry, so a
  restriction that would take more than sixteen queries is not applied: the leg
  reads unrestricted, logs `storage roots did not yield a bounded volume-label
  restriction`, and returns the same body.
- `--netapp-qos-scope-batch-bytes` now also bounds the rooted read's
  alternations, and phase 1 charges its repeated matcher at rendered length.
- An `svm=` or `node=` root, the `contains` / `regex` volume-match modes, and
  `/v1/graph` all read `volume_labels` exactly as before.

One body-changing corner is documented, not hidden: an aggregate or controller
named by `volume_labels` alone — no `aggr_*` / `node_*` series — outside the
rooted components is not materialised by a restricted read, which can change
whether an alert without a `cluster` label matches a unique entity. The stock
Harvest templates name every one.

## Non-breaking: storage build reads Kubernetes nodes and controllers by reference

*storage-graph-api — Storage build reads only what it draws (scope-controller-legs-by-reference)*

Not a compatibility break: every `/v1/storage-graph` body is byte-identical to
today's. What changes is which upstream queries the build issues and how they
are shaped.

Previously the four `kube_node_*` families and the eight controller-owner /
controller-annotation families (`kube_replicaset_owner`,
`kube_replicaset_annotations`, `kube_job_owner`, `kube_job_annotations`,
`kube_deployment_annotations`, `kube_statefulset_annotations`,
`kube_daemonset_annotations`, `kube_cronjob_annotations`) were read
UNRESTRICTED — across the whole estate — even though the storage body only
ever consults them for the Kubernetes nodes and controllers the pods it draws
actually name. Four of the eight accumulate one series per RETAINED object
rather than per LIVE one (`kube_replicaset_owner`, `kube_replicaset_annotations`
by ReplicaSet history, `kube_job_owner`, `kube_job_annotations` by Job
history), so a CronJob-heavy estate could exceed an upstream series limit on
`kube_job_owner` — a REQUIRED leg — regardless of the request window.

All twelve now read BY REFERENCE, in three waves gated on the pod wave: nodes
restricted to the Kubernetes nodes the loaded pods are scheduled on plus the
request's `node=` roots, and controllers restricted (in two stages, since a
Deployment or CronJob name is only known one hop after ReplicaSet / Job) to
the owner names those pods' resolved owners carry. `RawSeriesCount` for these
twelve families now counts only the series a build's OWN scope matched, and is
absent (never `0`) when that build's scope for the family was empty —
operators reading the per-family series-count histogram or the `raw_series_counts`
Debug log should expect these twelve to track the loaded pod count, not the
estate size. A `node=` root now reaches the build as an input to the node
wave, alongside the `pod=` root the pod wave already used.

## `kube_pod_container_info` no longer fails the build

*cluster-topology-source — Topology series consumed*

A query error on `kube_pod_container_info` — typically VictoriaMetrics'
`the number of matching timeseries exceeds …; either narrow down the search or
increase -search.maxUniqueTimeseries` — used to fail every build with a mapped
HTTP 5xx. It now logs `optional topology query failed` and returns **200** with
`data.containers` absent on every pod. Nothing else in the body moves. Caller
cancellation (build timeout, client disconnect) still fails the request.

The family's cardinality multiplies with the live object count — one series per
container per image variant, and, read over the whole window, one per pod that
existed at any instant of it — so it is the leg a memory-derived series limit
rejects first. If you alert on `/v1/graph` 5xx for it, alert instead on
`kube_state_graph_upstream_query_failures_total{query="kube_pod_container_info"}`.

## `/v1/storage-graph` pods carry no `data.containers`

*storage-graph-api — Attributes and compound groups carry over; Storage build
reads only what it draws*

The storage build never issues `kube_pod_container_info`, so a storage-graph pod
node has no `containers` field. Every other attribute and every compound group
is unchanged, and the Sankey reads none of it. Read containers from `/v1/graph`.

The storage build also no longer issues `kube_service_info`,
`kube_endpointslice_endpoints`, `kube_endpointslice_labels` or
`kube_service_annotations`, and reads `kube_pod_info` / `kube_pod_owner` only
for the pods a claim binding names plus the request's `pod=` roots — no query
at all when that set is empty. Those change the queries, not the body. A
failed pod chunk fails the build, as an unscoped `kube_pod_info` error does.

## Pod-only roots narrow the upstream read

*storage-graph-api — Pod-only roots narrow the upstream read*

A `/v1/storage-graph` request whose roots are all `pod=<ns>/<name>` and that
carries no `namespace` now renders the roots' namespaces as a `namespace`
matcher on every namespaced kube-state-metrics, kubelet and `ALERTS` query. The
body is identical; the queries — and anything inspecting them, such as upstream
query logs or recording tests — differ. An explicit `namespace` always wins, and
any storage-side or `node` root suppresses the derivation.

## In-process embedder (`pkg/`) signature changes

| Before | After |
|---|---|
| `build.Builder.BuildStorage(ctx, window, end, sel)` | `build.Builder.BuildStorage(ctx, window, end, sel, roots graph.StorageRoots)` |
| `kubegraph.Engine.BuildStorage(ctx, window, end, sel)` | `kubegraph.Engine.BuildStorage(ctx, window, end, sel, roots graph.StorageRoots)` |

A pod root that mounts no claim is drawable only if the build reads its pod, so
the roots must reach the build; a compile error is the honest failure, where a
roots-less sibling would silently drop every claimless root. Pass
`graph.StorageRoots{}` for a request with no roots, and pass the SAME roots you
hand `graph.ProjectStorage`. `kubegraph.Engine.BuildStorageFromValues`,
`kubegraph.ParseStorageValues` and every `/v1/graph` signature are unchanged.

## New self-metric (additive)

*graph-api — Self-metrics endpoint*

`kube_state_graph_upstream_query_result_series{query}` — a histogram of the
series each successful upstream query returned (buckets 1024 … 1048576; no
`backend` label). An embedder's `promql.Metrics` opts in through the optional
`promql.SeriesMetrics` upgrade; an implementation without it is unaffected.

## `RawSeriesCount` (debug log only)

`Topology.RawSeriesCount` now carries a key only for a family the build issued.
A family the storage plan skips, and a second-wave family whose scope came out
empty — the QoS workload legs with no matched FlexVol, the storage pod legs
with no binding and no root — are absent rather than `0`.

# BREAKING changes — remove the edge-type catalogue and the `edge_type` filter

A declared v1 break. No compatibility shim, no redirect, no deprecation window.

## Removed endpoint

`GET /v1/edge-types` is gone. A client still calling the route receives `404`
with the standard error body (`{"apiVersion":"v1","error":{"reason":"not_found",…}}`)
and no `Cache-Control` header — the same shape the removed `GET /v1/clusters`
returns. The route no longer appears in the served OpenAPI document, and no
`GET /v1/edge-types` server span is emitted.

The catalogue existed to populate and validate the `edge_type` filter below.
With that filter withdrawn it described nothing a caller could act on: the set
of edge types a body can carry is fixed by the API contract —
`pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`,
`pod-to-node`, `pvc-to-netapp-aggr` on `/v1/graph`, and `storage-flow` on
`/v1/storage-graph` — and every edge already carries its own `data.type`.

## Withdrawn `/v1/graph` parameter

`edge_type` is withdrawn. Like `name`, `root`, `depth` and `direction` before
it, it is now an unknown parameter: **ignored without error**, and its VALUE is
never inspected. Two consequences are invisible in the response shape, so check
callers rather than status codes:

- `?edge_type=pod-calls-pod` returns the **full** projection. The parameter was
  a projection-level gate over the edge list only — node admission never
  consulted it — so a filtered request already returned every node of the
  default view with the edges that justified them stripped out. It now returns
  those edges too, and the body is byte-identical to the same request without
  the parameter.
- `?edge_type=pod-calls-pods` (any unregistered value) is now `200`, not the
  `400 invalid_scope` the registry-backed validation produced.

**Replacement:** filter client-side on each edge's `data.type`, which every
edge has always carried. A consumer doing so keeps the infrastructure nodes it
drops edges for, which is the thing the server-side filter could not express.

The `/v1/graph` request surface is now exactly `start`, `end`, `cluster`,
`namespace`, `az`, `env`, `prune`. `/v1/storage-graph` is unchanged: it already
ignored `edge_type`.

## In-process embedder (`pkg/`) signature changes

D32 embedders must update call sites — a silently-ignored argument would
reproduce in Go the inert control this removal exists to prevent:

| Before | After |
|---|---|
| `graph.NewScope(clusters, namespaces, edgeTypes, inventory) (Scope, error)` | `graph.NewScope(clusters, namespaces, inventory) Scope` |
| `graph.Scope{Clusters, Namespaces, EdgeTypes, Inventory}` | `graph.Scope{Clusters, Namespaces, Inventory}` |
| `graph.ValidEdgeType(t)` | removed |

`NewScope` no longer returns an error: edge-type validation was its only
failure mode, and every remaining dimension is an opaque string set the request
parser has already validated. `kubegraph.ParseValues`,
`kubegraph.Engine.BuildFromValues` and every other `pkg/` signature are
unchanged, so an embedder that goes through the facade needs no edit.

**Retained:** `graph.EdgeTypes` and `graph.EdgeTypeDefinition`. The registry is
the builder's single in-code declaration of each edge type — its
`may_cross_cluster` bit buckets the cross-cluster edge count, and the
`pod-service-graph` specification pins the `pod-calls-service` declaration. It
is no longer serialised to any HTTP response.

## Self-metrics

`kube_state_graph_http_requests_total{path="/v1/edge-types"}` and the matching
duration series stop being produced; a request to the old path counts under
`path="<unmatched>"` like any 404. This is a label VALUE disappearing, not a
metric or label contract change — but a dashboard or alert keyed on that path
goes stale.

## NOT changed

No node type, edge type, `labels` key, `data.*` attribute, edge id, or
`/v1/graph` body for any request that did not send `edge_type`. The
connectivity prune, `prune`, the edge retention and partner re-add rules, and
the cluster / namespace projection are untouched. Every golden fixture is
byte-identical.

# BREAKING changes — replace StorageClass nodes with NetApp nodes

This release is a declared v1 break. There is no compatibility shim.

## Removed from `/v1/graph` and `/v1/edge-types`

- Node type `storageclass` (ids `<cluster>/storageclass/<name>`).
- Edge type `pvc-to-storageclass`.
- Attributes `data.provisioner` / `data.parameters` that lived on StorageClass nodes.

The claim's StorageClass **name** survives as the PVC's own `data.storageclass`
(omitempty). Physical backend identity is the `pvc-to-netapp-aggr` edge to a
`netapp-aggr` node nested under a `netapp-node`.

## Removed configuration

`--metric-prefix` / `KSG_METRIC_PREFIX` / `kubegraph.Options.MetricPrefix` /
`build.Options.MetricPrefix` are gone. Every kube-state-metrics-shaped series
is queried at its bare name.

A deployment whose KSM series **are** published under an organisational prefix
will silently return an empty graph after upgrade. Re-publish those series at
their bare `kube_*` names (drop the prefixing relabel/fork) **before**
upgrading. Embedders (`graph-api-gateway`) must drop `Options.MetricPrefix` in
the same version bump.

## `data.metrics.rate` is schema-optional

The wire `metrics` object is now a union of the RED family (`rate`,
`error_rate`, `p90_server_ms`) and the I/O family (`read_ops`, `write_ops`,
`read_latency_us`, `write_latency_us`, `read_bytes_per_sec`,
`write_bytes_per_sec`, plus the declared QoS ceiling `max_iops` and
`max_bytes_per_sec`). At the OpenAPI schema level every field
is optional. RED behaviour is unchanged: a RED-family object always carries a
positive `rate`.

An absent ceiling field means the volume has **no declared ceiling** — it is
never emitted as `0` or an "unlimited" sentinel — and neither ceiling field can
appear without at least one measurement field.

## Removed upstream queries

`kube_storageclass_info`, `kube_tridentvolume_info`, and
`kube_tridentbackend_info` are no longer read. The KSM custom-resource-state
config that exported the Trident CRs is now removable. PVC `labels.svm` is
re-sourced from the Harvest `volume_labels` series (see
`docs/netapp-harvest-preconditions.md`).

## New upstream requirements — NetApp Harvest

The storage join reads three Harvest families. It reads them at their **stock**
labels — see "The Harvest `volume_name` relabel is no longer read" below:

| Series | Hop | Effect if missing |
|---|---|---|
| `volume_labels` | A — topology | No `netapp-aggr` / `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, no PVC `svm` |
| `qos_{read,write}_{ops,latency,data}` | B — I/O | Edge is still emitted, carrying no `metrics` key |
| `qos_policy_fixed_max_throughput_{iops,mbps}` | C — ceiling | No `max_iops` / `max_bytes_per_sec` |

Required Harvest templates: volume instance labels, QoS workload, QoS fixed
policy. The QoS legs carry the `volume` scope and no `lun` matcher: a LUN
workload, which carries its FlexVol's `volume`, is fetched (on a SAN backend it
is the only series naming the QoS policy) and discarded by the reader before any
I/O sum, so it is never added to the volume's own traffic.

Two aggregated coverage warnings replace the single one:
`netapp_volume_join_miss` (hop A) and `netapp_qos_join_miss` (hop B), each gated
on its own family having been read.

---

# BREAKING — the Harvest `volume_name` relabel is no longer read

The PVC → ONTAP aggregate join no longer reads `volume_name`, a label stock
Harvest does not emit and that every deployment had to synthesise with a
Prometheus relabel rule. Both the volume-object family and the six QoS workload
families are now read at their **stock `volume` label** (the ONTAP FlexVol
name), and the bridge to Kubernetes is built in the backend: the PVC's bound PV
name is rewritten into a match token and matched against `volume`.

## What to do

1. Upgrade with **no configuration change**. The defaults — replace `-` with
   `_`, match as a suffix — resolve a stock Trident estate without the
   deployment declaring its `storagePrefix`.
2. Read `netapp_volume_join_miss` from the build logs. Zero means the derivation
   covers the estate.
3. If non-zero, compare `count by (volume) (volume_labels)` with a claim's
   `volumename` and set `--netapp-volume-key-rewrite` /
   `--netapp-volume-match-mode` accordingly.
   [`netapp-harvest-preconditions.md`](netapp-harvest-preconditions.md) has the
   full table.
4. Once step 2 reports zero, the `volume_name` relabel rule may be deleted.
   Leaving it installed is harmless — the label is simply not read.

## Who this breaks

A deployment whose relabel rule encoded an **arbitrary** mapping — a
hand-maintained table for pre-existing volumes, say — rather than a
transformation of the PV name. Regular expressions can express a
transformation, not a lookup table. Such a deployment loses its storage chain
and must either express the mapping through the rewrite rules or accept the
miss. The loss is reported by `netapp_volume_join_miss`, never silent.

## Rollback is asymmetric

The previous version reads `volume_name`. A deployment that already deleted its
relabel rule must **restore the rule before rolling back**, or the older binary
resolves no storage chain at all.

## Other changes in the same release

- **The six `qos_*` workload families are read scoped.** They are issued after
  the volume-object family, restricted to the FlexVol names the loaded claims
  actually matched, and split across several queries when that set exceeds
  `--netapp-qos-scope-batch-bytes` (default 8192). A build whose claims matched
  nothing issues **no QoS query at all**. Each chunk degrades on its own.
- **`build.ReadTopology`'s signature changed** (embedders only): its
  `keys promql.LabelKeys` parameter is now `opts build.Options`, which carries
  `LabelKeys` alongside the new `VolumeKey` and `QoSScopeBatchBytes` fields. A
  zero `Options` behaves exactly as `promql.LabelKeys{}` did.
- **New configuration**: `--netapp-volume-key-rewrite` /
  `KSG_NETAPP_VOLUME_KEY_REWRITE`, `--netapp-volume-match-mode` /
  `KSG_NETAPP_VOLUME_MATCH_MODE`, `--netapp-qos-scope-batch-bytes` /
  `KSG_NETAPP_QOS_SCOPE_BATCH_BYTES`. An uncompilable rewrite pattern or an
  unknown match mode is a startup failure, never a silent fallback.
- The `/v1/edge-types` **description** of `pvc-to-netapp-aggr` now says
  derive-then-match. The edge `id`, `type`, endpoints and `data.metrics` fields
  are unchanged.
- **The hop-C ceiling triple is anchored on hop A.** Its `ontap_cluster` and
  `svm` components now come from the topology hop — the picked aggregate's
  ONTAP cluster and the SVM the `volume_labels` match resolved — instead of
  from the workload series; only `policy_group` is still read from hop B. This
  is **additive** for the resolved value: a workload series carrying a
  `policy_group` but no `svm` label of its own now resolves a ceiling where it
  previously could not, and under a cross-filer FlexVol-name collision the key
  follows the filer the edge points at. A volume in no QoS policy group still
  carries no ceiling — an incomplete key is ignored, never widened to an
  SVM-wide figure.

---

# BREAKING changes — push request filters into upstream PromQL

A second declared v1 break, shipped in the same release train. No compatibility
shim.

## Removed endpoint

`GET /v1/clusters` is gone, together with its `cluster_discovery` query and the
fixed one-hour discovery lookback. The cluster list is the `clusters` field of
any `/v1/graph` response, which now reflects the clusters present in the
PROJECTED view. A client still calling the route receives `404`.

## Withdrawn `/v1/graph` parameters

`name`, `root`, `depth`, and `direction` are withdrawn along with the bounded
BFS behind them. Like any unknown parameter they are now **ignored without
error**, so an old client receives the unanchored view instead of a 400 — check
callers that relied on anchoring rather than on the error. The `invalid_depth`
and `depth_too_large` 400 reasons no longer occur.

Replacement for the "surface one specific element" use case: `?prune=false`
(new, boolean, default `true`) turns the connectivity prune off and returns the
inventory, optionally narrowed by `cluster` / `namespace` / `az` / `env`.

## New `/v1/graph` parameters

`az` and `env` (repeatable, OR-combined) join `cluster` and `namespace` as
**selector-level** filters: they are rendered into the upstream queries as label
matchers instead of being applied over the built graph. The upstream label each
one binds to is configurable with `--az-label` / `KSG_AZ_LABEL` and
`--env-label` / `KSG_ENV_LABEL` (defaults `az` / `env`, validated as PromQL
label names, required to differ); the request parameter names never change.

**Operator precondition:** every topology family the request narrows must carry
the configured labels. A family that does not carry them vanishes under an
`az` / `env` filter, and the connectivity prune can then empty the graph. A
`selector_family_empty` Warn fires when kube-state-metrics matched but a family
a live dimension reaches returned nothing.

## Behaviour changes under a filter

- A filtered build **never synthesises a pod**. An endpoint whose pod the
  request did not load renders as `external/<label>`, so under `?cluster=` the
  cross-cluster partner is an `external` node, not a real pod — cross-cluster
  edge representation now requires BOTH clusters loaded.
- A service-graph series that touches no loaded workload contributes nothing.
- `?cluster=unknown` addresses the missing-cluster-label bucket and now renders
  `cluster=~"unknown|"` rather than `cluster=""`, so a series whose `cluster`
  label is literally `unknown` — which the parse layer buckets identically and
  the projection filter already accepted — is loaded too.
- A filtered request that matches no topology at all issues no
  `traces_service_graph_*` queries: no series could survive admission, so the
  read is skipped rather than scanning the whole estate for an empty answer.
- An empty filtered result is a `200` with `elements: []` and `clusters: []`,
  never `outside_retention` (which stays an unfiltered-build classification).

## In-process embedder (`pkg/`) signature changes

D32 embedders must update call sites:

| Before | After |
|---|---|
| `kubegraph.ParseValues(v) (start, end, scope, err)` | `kubegraph.ParseValues(v) (Request, error)` |
| `Engine.Build(ctx, window, end)` | `Engine.Build(ctx, window, end, promql.Selector{})` |
| `build.Builder.Build(ctx, window, end)` | `Build(ctx, window, end, promql.Selector{})` |
| `build.ReadTopology(ctx, q, window, end)` | `ReadTopology(ctx, q, window, end, promql.LabelKeys{}, promql.Selector{})` |
| `promql.Render(q, window)` | `Render(q, window, promql.LabelKeys{}, promql.Selector{})` |

`graph.Graph` no longer carries the `Forward` / `Reverse` adjacency maps. They
existed only for the withdrawn `?root=&depth=` traversal; every surviving
consumer scans `Edges` once, so building them was two allocations and 2×|E|
appends per request that nothing read. An embedder that needs adjacency builds
it from `Edges` itself.

`graph.Scope` is now `{Clusters, Namespaces, EdgeTypes, Inventory}`;
`graph.NewScope(clusters, namespaces, edgeTypes, inventory)` takes four
arguments. `Names`, `Root`, `Depth`, `Direction`, `MaxTraversalDepth`,
`graph.Direction` and `Scope.NameFilterActive` are removed. The zero values of
`promql.Selector` / `promql.LabelKeys` reproduce every pre-change query string
byte-for-byte, so an embedder that wants the old behaviour passes them and
changes nothing else.

# BREAKING changes — resolve pod ArgoCD Application from the controller

## Withdrawn upstream source: `argocd_tracking_id` on `kube_pod_owner`

The pod `data.application` attribute is no longer read from an
`argocd_tracking_id` label on `kube_pod_owner`. That label has no
kube-state-metrics producer — ArgoCD stamps `argocd.argoproj.io/tracking-id` on
the workload objects it applies, never on the pods a controller spawns, and
neither `--metric-labels-allowlist` nor `--metric-annotations-allowlist` can
enrich `kube_pod_owner` from another resource's annotations. A deployment that
synthesised the label (a customised exporter, or a recording rule joining
`kube_pod_labels`' `label_app_kubernetes_io_instance`) loses pod Applications
until it configures the controller-annotation families below. The label is
ignored, not rejected: the build never fails and nothing else changes.

## New upstream requirements — controller annotations

Pod `data.application` is now joined on `(cluster, namespace, owner_kind,
owner_name)` — the controller owner already resolved for `data.owner`, with the
ReplicaSet skipped to its Deployment — against one annotation family per
controller kind:

| Pod's resolved owner kind | Series | Identity label |
|---|---|---|
| `Deployment` | `kube_deployment_annotations` | `deployment` |
| `StatefulSet` | `kube_statefulset_annotations` | `statefulset` |
| `DaemonSet` | `kube_daemonset_annotations` | `daemonset` |
| `ReplicaSet` (bare) | `kube_replicaset_annotations` | `replicaset` |
| `Job` | `kube_job_annotations` | `job_name` |
| `CronJob` (via `kube_job_owner`) | `kube_cronjob_annotations` | `cronjob` |

Each family needs
`--metric-annotations-allowlist=<plural-resource>=[argocd.argoproj.io/tracking-id]`
and its collector. The flag is per-resource, so the degradation is per-family:
enable `deployments` alone and only Deployment-managed pods gain Applications.
`kube_job_owner` is added for the Job → CronJob hop — the Kubernetes CronJob
controller copies only `spec.jobTemplate.metadata` annotations onto the Jobs it
creates, so ArgoCD's tracking-id never reaches a Job. Full install-side detail,
including the widened ClusterRole and the cardinality guidance for the
ReplicaSet / Job families, is in `docs/kube-state-metrics-preconditions.md`.

The topology fan-out grows from 30 to 37 legs. `ReplicationController`, `Node`
(static / mirror pods) and CRD controllers such as argo-rollouts `Rollout` have
no kube-state-metrics annotation family and resolve no Application.

## Controller-annotation legs: tighter upstream selector, two now degrade

Two changes to the seven controller-annotation / owner legs above.

**1. Fixed selectors are pushed upstream.** `kube_job_owner` is now read as
`kube_job_owner{owner_kind="CronJob",owner_is_controller="true"}` and the six
`kube_*_annotations` families as
`kube_*_annotations{annotation_argocd_argoproj_io_tracking_id!=""}`. Both mirror
a discard the Go reader already performed, so **no graph output changes**. What
changes is the upstream contract: a series that does not match is never
fetched. An exporter that spells `owner_is_controller` differently, or one whose
tracking-id label is not exactly `annotation_argocd_argoproj_io_tracking_id`,
now yields an empty family instead of rows the reader silently dropped.
`Topology.RawSeriesCount` for those seven legs likewise counts matched
(annotated / CronJob-controlled) objects, not every object of that kind — a
`0` there no longer means "the collector is off".

**2. `kube_replicaset_annotations` and `kube_job_annotations` no longer fail the
build.** Their cardinality accumulates with history (`revisionHistoryLimit`,
Job history limits) rather than live object count, so they moved from
abort-on-error `fetch` to log-and-continue `fetchOptional` — the same semantics
the Harvest and kubelet legs already had. The other four families and
`kube_job_owner` still abort.

**This is an operator-visible outcome change.** An upstream error on those two
legs (for example `search.maxUniqueTimeseries exceeded`) previously returned a
mapped HTTP 5xx; it now returns **200** with `data.application` silently absent
for bare-ReplicaSet-owned and Job-owned pods. The absence itself is
subtractive — never a substituted value — but it still moves the graph:
affected pods reparent in the Cytoscape compound hierarchy, a sole-member
`application` group node disappears, and a PVC that inherited its Application
from such a pod re-inherits from a different mounter.

A degraded `kube_job_annotations` additionally **suppresses the Job → CronJob
hop for that build**. The hop's precondition is "this Job carries no annotation
of its own", which a family that was never read cannot establish — so following
it would attribute a directly-managed Job's pod to its CronJob's Application, a
wrong value rather than a missing one. The cost is that a genuinely
annotation-less Job under an annotated CronJob also loses `data.application`
while the leg is degraded.

If you alert on `/v1/graph` 5xx for these families, move the alert to the
self-metric
`kube_state_graph_upstream_query_failures_total{query="kube_replicaset_annotations"}`
(and `{query="kube_job_annotations"}`), which is incremented for every failed
query regardless of which fetch helper issued it, or to the
`optional topology query failed; continuing with empty vector` Warn. Caller
cancellation (build timeout / client disconnect) still fails the request.

## NOT changed

`data.owner` and the `controller` compound group are untouched: the Job → CronJob
hop is resolution-only, so a CronJob-managed pod still reports
`owner={kind:"Job", …}`. `data.application`'s wire shape, `omitempty` semantics,
`<app>:<group>/<kind>:<ns>/<name>` parse and determinism rules are unchanged, as
are the service and PVC Application sources and the PVC inheritance rule. No
node type, edge type, `labels` key, request parameter, or HTTP route changes.

---

# NOT breaking — upstream backend routing

`add-multi-backend-query-routing` lets one process assemble a graph from several
Prometheus-compatible installations, selected by availability zone and by metric
family. It is recorded here because it changes how the upstream is *configured*
and adds one operational behaviour change — but **no client-visible contract
moves**. See [`upstream-backend-routing.md`](upstream-backend-routing.md) for
the full configuration reference.

## Unchanged

- **Request surface.** No new parameter, none withdrawn. `?az=` keeps its exact
  current meaning as a PromQL label matcher and *additionally* selects which
  stores are asked; the two compose, and the rendered query string for a given
  query is identical across every backend it is issued to.
- **Response body.** No node type, edge type, `labels` key, or `data.*`
  attribute changes. The determinism contract is unchanged: the fan-out merge
  is a pure function of the value sets returned, ordered by backend name, so it
  cannot depend on which store answered first.
- **Self-metric contracts.** `kube_state_graph_upstream_query_duration_seconds`
  and `kube_state_graph_upstream_query_failures_total` keep their `query`-only
  label sets. Per-backend detail lives on three NEW metrics —
  `kube_state_graph_upstream_backends`,
  `kube_state_graph_backend_config_reload_total{result}`, and
  `kube_state_graph_backend_query_failures_total{backend}`.
- **`--prom-url` and `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD`.** Retained. With
  no routing file configured, an implicit backend named `default` at
  `--prom-url` serves every family with no zones, carrying the global
  credentials.
- **Embedding API.** No exported signature in `pkg/promql`, `pkg/build`, or
  `pkg/kubegraph` changed. Routing is an OPTIONAL upgrade interface
  (`promql.QuerierSource`) that `build.New` type-asserts, so a consumer passing
  a plain `promql.Querier` — a `*promql.Client`, a mock, an embedder's own
  implementation — behaves exactly as before.

## What a deployment that configures nothing gets

Byte-identical output. Every existing unit, component, golden, and integration
test runs through the router in its degenerate single-backend configuration —
the compatibility mode is a one-entry routing table, not a separate code path,
precisely so that the claim is exercised rather than asserted.

## The one operational behaviour change

**A single unreachable backend now fails builds that a one-backend deployment
would have served.** When a query is fanned out and any backend errors, the
query fails and the error names that backend; a partial result is never
returned in its place.

This is deliberate. A partial fan-out is indistinguishable from a smaller
estate: missing pods lose their edges, the connectivity prune then removes their
nodes, claims and aggregates, and the response is a plausible, smaller, wrong
graph — the failure mode this repository's "invariants that fail silently" list
exists to prevent.

Blast radius is unchanged for a single-backend deployment (one store down was
already a 502). It grows with the number of backends, which is why:

- `/readyz` probes **every** backend within the one `--api-timeout` budget and
  names the ones that did not answer, and
- `kube_state_graph_backend_query_failures_total{backend}` is materialised at
  zero for every configured backend, so a healthy store is a visible zero series
  rather than an absent one.

Legs the builder already treats as optional (Harvest, kubelet,
`kube_replicaset_annotations`, `kube_job_annotations`) keep degrading exactly as
they do for any other upstream error.

## Two configuration rules that fail loudly on purpose

- **A family served by no backend is a validation error**, not a degrade. Its
  queries would have nowhere to go, and the empty vector that produced would be
  indistinguishable from an estate that genuinely holds nothing.
- **A backend naming a credential environment variable that is unset or empty is
  a load failure**, not a quiet fallback to the global pair or to no
  credentials. A typo'd variable name would otherwise become 401s from one
  store, which — since a backend failure fails the whole query — surfaces as an
  error pointing at the wrong thing.

An invalid routing file at **startup** is fatal. An invalid file at **reload**
is rejected wholesale: the previous table keeps serving and the failure is
counted every tick until it is fixed.

## New configuration

- `--backends-file` / `KSG_BACKENDS_FILE` — path to the routing table (YAML or
  JSON). Unset keeps the implicit single backend.
- `--backends-reload-interval` / `KSG_BACKENDS_RELOAD_INTERVAL` — default `30s`,
  matching the API-key reloader. `0` disables reloading.

## New dependency

`sigs.k8s.io/yaml` is promoted from an indirect to a direct dependency. It was
already in the module graph (via `istio.io/istio`), so **no new module enters
the build**; it is imported from `internal/config` only, never from `pkg/`, so
an embedder inherits nothing.

# BREAKING changes — cluster identity is `<az>-<env>-<cluster>`

A Kubernetes cluster is now identified by the composite `<az>-<env>-<cluster>`,
composed from the zone and environment external labels the `?az=` / `?env=`
filters already match on (under the `--az-label` / `--env-label` keys). The raw
`cluster` label alone was never unique: `c1` in `us`/`dev` and `c1` in
`eu`/`prod` collapsed into ONE graph cluster — one `c1/worker-0` node, one
`cluster/c1` compound group, one service index — silently merging two unrelated
estates.

**A deployment whose series carry no `az`/`env` pair is completely unaffected:
every id, label, group and `clusters[]` entry is byte-identical to before.**

## What carries the identity now

- Node ids: pods `<az>-<env>-<cluster>/<uid>`, K8s nodes
  `<az>-<env>-<cluster>/<node>`, PVCs and services
  `<az>-<env>-<cluster>/<namespace>/<name>`.
- `labels.cluster` on pod / node / PVC / service nodes, and the pod's
  `labels.node` reference.
- The `type="cluster"` compound group (`id` AND `name`) and every group id
  nested under it (`<identity>/namespace/<ns>`, …).
- The top-level `clusters[]` array.
- The `cluster` label VALUE of `kube_state_graph_graph_node_count` and
  `..._graph_edge_count` (the label SETS are unchanged).

NetApp `netapp-aggr` / `netapp-node` nodes are untouched: their `ontap_cluster`
is a filer, not a Kubernetes cluster.

## `?cluster=` still takes the RAW name — `clusters[]` no longer round-trips

`?cluster=` is matched upstream against the raw `cluster` label and is compared
at projection against the identity's raw component, so it keeps its old
meaning:

- `?cluster=c1` selects **every** zone's and environment's `c1`.
- `?az=us&env=dev&cluster=c1` pins exactly one — the three request dimensions
  ARE the identity's three components.
- `?cluster=unknown` still addresses the missing-label bucket, whose identities
  spell `<az>-<env>-unknown`.

**Migration:** a client that fed a value from `clusters[]` back into
`?cluster=` now gets an empty 200. Send the three components instead, or read
the raw name from a node id prefix's last segment.

## Edge `labels.cluster` is the client pod's identity

A `pod-calls-pod` / `pod-calls-service` edge whose client side resolves to a
topology pod now carries that pod's identity rather than the verbatim
service-graph `cluster` label. The label was frequently missing or disagreed
with topology, so an edge could name a cluster that appeared on no node of the
response. A synthesised or non-pod client still falls back to the trace label,
resolved through the ladder below.

## Same name in two zones is now a cross-cluster edge

Cross-cluster status compares the endpoints' `labels.cluster`, so a call
between `us-dev-c1` and `eu-prod-c1` is cross-cluster where it previously
looked intra-cluster. The cluster-family rule (digit runs → `0`) now runs over
the identity, so a family is scoped to one zone and one environment:
`us-dev-c1` ~ `us-dev-c2`, but `us-dev-c1` ≁ `eu-prod-c1`. Service-mesh
`service-selects-pod` fan-out narrows accordingly.

Caveat: the rule normalises digit runs anywhere in the string, so a zone value
containing digits widens the family (`us-east-1-prod-c1` and
`us-east-2-prod-c1` share a family key).

## Upstream requirement: stamp both labels on every cluster-keyed family

Every cluster name meets one ladder:

1. **compose** — the series carries both labels → `<az>-<env>-<cluster>`;
2. **adopt** — it does not, but the raw name maps to exactly ONE identity in
   the build → that identity;
3. **verbatim** — otherwise the raw name stands as its own cluster and the
   build logs one aggregated `cluster_identity_unresolved` per metric.

Step 2 keeps a partially-stamped estate whole. Step 3 is the visible failure: a
kubelet, owner or annotation family with no pair whose raw name is ambiguous
becomes an orphan cluster and joins nothing. Stamp `az` and `env` on every
kube-state-metrics and kubelet family — the same precondition the filters
already required.

## Route store: write the identity in the `cluster` column

`RouteRequest.CallerCluster` is now the caller pod's identity, and a
destination's `cluster` is resolved through steps 2–3 before the topology
lookup. A store writing raw names still resolves while a name is unambiguous in
the build and otherwise degrades through the existing
`route_engine_dest_cluster_lacks_service` path — no new outcome. The metadata
exporter should write the identity string.

## In-process embedder (`pkg/`)

No signature moved. `graph.Graph` gains `ClusterIdentities
map[string]ClusterIdentity` (nil-safe) and `Graph.ClusterRawName(id)`; a graph
built without the table compares raw labels exactly as before.
