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

The storage join reads three Harvest families, and the deployment's
`volume_name` relabel rule must now stamp **both** the volume-object and the
QoS workload series:

| Series | Hop | Effect if missing |
|---|---|---|
| `volume_labels` | A — topology | No `netapp-aggr` / `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, no PVC `svm` |
| `qos_{read,write}_{ops,latency,data}` | B — I/O | Edge is still emitted, carrying no `metrics` key |
| `qos_policy_fixed_max_throughput_{iops,mbps}` | C — ceiling | No `max_iops` / `max_bytes_per_sec` |

Required Harvest templates: volume instance labels, QoS workload, QoS fixed
policy. The QoS legs are issued at `{lun=""}` — without that matcher a LUN
workload, which carries its FlexVol's relabelled `volume_name`, would be summed
on top of the volume's own traffic.

Two aggregated coverage warnings replace the single one:
`netapp_volume_join_miss` (hop A) and `netapp_qos_join_miss` (hop B), each gated
on its own family having been read.

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
