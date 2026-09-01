# kube-state-graph

Traditional Chinese: [README.zh-tw.md](README.zh-tw.md).

A Go REST API server that returns a unified pod / node / PVC graph for one or
more Kubernetes clusters, including pod-UID-resolved RPC edges that may cross
cluster boundaries.

```
cluster A: kube-state-metrics ──┐
           service-graph source ┤
                                 │  (vmagent / Prometheus
cluster B: kube-state-metrics ──┤   with external_labels:
           service-graph source ┤   { cluster: "<name>" })
                                 │
       ...                       ├──► centralised VictoriaMetrics ◄── kube-state-graph
                                 │                                     (Prometheus HTTP API)
cluster N: kube-state-metrics ──┤
           service-graph source ─┘
```

## What it does

- Reads `kube_*` topology, Harvest / kubelet storage series, and
  `traces_service_graph_*` runtime metrics from a single centralised
  VictoriaMetrics, on demand for a caller-specified `[start, end]` time range.
  Every series the builder queries is listed in
  [`docs/upstream-metrics.md`](docs/upstream-metrics.md).
- Joins them into a multi-cluster graph keyed by cluster-scoped pod UIDs and
  node names.
- Returns the graph as Cytoscape.js JSON (`/v1/graph`).
- Exposes a static edge-type catalogue (`/v1/edge-types`). The set of clusters
  with data is the `clusters` field of any `/v1/graph` response.
- Builds the graph on every request — v1 ships **no in-process result cache**,
  **no singleflight**, and **no HTTP cache validators** (`ETag` /
  `If-None-Match` / `304`). A horizontally scalable cache mechanism for
  distributed deployment is anticipated as a future change. Caller-supplied
  `start` / `end` accept RFC 3339 or Unix seconds; the server enforces only
  `end > start`, then passes the window through to upstream PromQL verbatim —
  no server-side bucketing, alignment, max-window cap, or future-time guard.
  Bounded query cost is delegated to VictoriaMetrics search limits
  (`-search.maxQueryDuration`, `-search.maxPointsPerTimeseries`,
  `-search.maxSamplesPerQuery`). The serialiser produces a deterministic body
  (`apiVersion`, `clusters`, `elements` only — no echoed time fields). Pod,
  node, and service IPs appear on the top-level `ipaddress` attribute, not in
  `labels`. Pods additionally carry typed `data` attributes — `owner`
  (`{kind, name}`), `application` (the ArgoCD Application), and `containers`
  (`[{name, image}]`) — all `omitempty` and never inside `labels`.
- Narrows the build **at the source**: `cluster`, `namespace`, `az` and `env`
  are rendered into the upstream PromQL queries as label matchers, so
  VictoriaMetrics does the filtering before a sample crosses the wire. The
  service-graph series are deliberately read in full (see
  [Request filters](#request-filters)).

## Quick start

```bash
make build
./bin/kube-state-graph \
  --prom-url=http://victoria-metrics.example:8428 \
  --listen-addr=:8080
```

Then:

```bash
curl 'http://localhost:8080/v1/graph?start=$(date -u -d "-5 min" +%s)&end=$(date -u +%s)' | jq '.elements'

# One namespace's storage topology, including workload with no traffic.
curl 'http://localhost:8080/v1/graph?start=…&end=…&namespace=payments&prune=false' | jq '.elements'

# One zone / environment.
curl 'http://localhost:8080/v1/graph?start=…&end=…&az=eu-west-1a&env=prod' | jq '.clusters'
```

When the server is started with API keys configured (`--api-keys-file` or
`--api-keys`), every `/v1/*` request must carry an `X-API-Key: <key>` header:

```bash
curl -H 'X-API-Key: my-secret-key' 'http://localhost:8080/v1/edge-types'
```

Health probes (`/livez`, `/readyz`), `/metrics`, and the docs routes
(`/openapi.*`, `/docs`) are exempt and require no key. With no keys configured
the middleware is a no-op and every route is open.

## Request filters

`GET /v1/graph` takes `start`, `end` (both required) plus six optional
parameters. Values within one parameter are OR-combined, different parameters
are AND-combined.

| Parameter | Applied | Notes |
|---|---|---|
| `cluster` | upstream **and** projection | Repeatable. `unknown` addresses series carrying no `cluster` label. |
| `namespace` | upstream **and** projection | Repeatable. Narrows the pod-, claim-, Service- and EndpointSlice-scoped series; nodes and NetApp aggregates follow **by reference**. |
| `az` | upstream | Repeatable. Matched against `--az-label` (default `az`) on every topology query. |
| `env` | upstream | Repeatable. Matched against `--env-label` (default `env`). |
| `edge_type` | projection | Repeatable; validated against `/v1/edge-types`. |
| `prune` | projection | `true` (default) keeps only workload on a connectivity edge. `false` returns the inventory: every loaded pod with its node / PVC / NetApp chain, plus unreferenced infrastructure when no `cluster` or `namespace` filter narrows it. |

**Which matcher reaches which series** is a hardcoded contract:

| Series | `az` | `env` | `cluster` | `namespace` |
|---|---|---|---|---|
| pod / claim / Service / EndpointSlice KSM series, kubelet volume stats | ✅ | ✅ | ✅ | ✅ |
| `kube_node_*` | ✅ | ✅ | ✅ | — (no such label) |
| NetApp Harvest (`volume_labels`, `qos_*`, `aggr_*`, `node_new_status`) | ✅ | ✅ | — (its `cluster` is the **ONTAP** cluster) | — |
| `traces_service_graph_*`, `up` | — | — | — | — |

The service-graph family is read **in full for every request**: its `cluster`
label is the frequently-missing trace-source cluster and its namespace labels
describe only the caller's own view, so narrowing there would drop edges the
loaded topology still needs. Instead, a filtered build applies two rules:

- an endpoint whose pod is **not loaded** resolves as if its UID were empty —
  a `"://"` label can still reach a loaded Service, anything else becomes
  `external/<label>` (with empty `labels`), and **no synthesised pod is ever
  created**;
- a series is kept only if **at least one** endpoint reaches loaded topology,
  so the out-of-scope estate never renders as a web of external nodes.

The visible consequence: under `?cluster=` or `?namespace=`, a peer outside the
filter appears as an `external` node rather than a real pod — the request's
inbound and outbound dependencies stay visible without loading the rest of the
estate.

> **Operator precondition.** The kube-state-metrics and kubelet families must
> carry the configured `az` / `env` labels. A family that does not simply
> matches nothing under those filters, and because the default projection keeps
> only connectivity-connected workload, a missing label can turn a filtered
> request into an empty graph rather than a partial one. The NetApp Harvest
> family is exempt: it carries no request matcher — `?az=` selects which
> `harvest` backend of the routing table is asked, `?env=` does not reach it —
> so Harvest series need no `az` / `env` label.

## Upstream metrics consumed

The complete operator catalog — all **41** series, PromQL wrappers, fixed
selectors, query-error vs empty-vector semantics, and the per-request fan-out
— is [`docs/upstream-metrics.md`](docs/upstream-metrics.md).

Summary: one `/v1/graph` request fans out **37** topology queries in parallel
(20 kube-state-metrics abort-on-error + 2 accumulating-cardinality
annotation families log-and-continue + 13 Harvest + 2 kubelet
log-and-continue), then **3** service-graph queries (skipped when a filtered
build loaded neither pods nor services), plus `up{}` only for an unfiltered
empty topology. There is **no metric-name prefix**; every series is queried at
its bare name. v1 has no result cache.

Every Kubernetes-shaped series is expected to carry a `cluster` external label
(injected by `vmagent` / Prometheus `external_labels` per source cluster).
Harvest's `cluster` is the **ONTAP** cluster and is never used as
`?cluster=`.

The **"Required?"** column below is about an **empty vector** (the series is
absent from the store, or matched nothing in the window). A **query error**
(timeout / 5xx) on any of the 20 abort-on-error kube-state-metrics legs or on
`traces_service_graph_request_total` **fails the build**;
`kube_replicaset_annotations` and `kube_job_annotations` (cardinality
accumulates with history, not live object count), Harvest, kubelet, and
the two RED series log-and-continue. Details in the catalog.

### Topology metrics — produced by [`kube-state-metrics`](https://github.com/kubernetes/kube-state-metrics)

| Metric | Used for | Labels read | Required? |
|---|---|---|---|
| `kube_pod_info` | Pod nodes (`node` label drives the `pod-to-node` edge; pods nest under the `cluster > namespace > application > controller > pod` workload hierarchy) | `cluster`, `namespace`, `pod`, `uid`, `node`, `pod_ip` (→ `data.ipaddress`; `host_ip` not exported) | **Yes** |
| `kube_node_info` | K8sNode nodes | `cluster`, `node` | **Yes** |
| `kube_node_status_addresses{type=~"ExternalIP\|InternalIP"}` | Node `data.ipaddress` — ExternalIP preferred, InternalIP fallback when the node has no ExternalIP | `cluster`, `node`, `type`, `address` | Optional (absent ⇒ no `ipaddress`) |
| `kube_node_status_condition{condition="Ready"}` | Node Ready status `data.ready_status` ∈ {`Ready`, `NotReady`, `Unknown`} from the active (`status` value 1) row; omitted when no Ready data — distinct from `Unknown` (kubelet lost contact) | `cluster`, `node`, `condition`, `status` | Optional (absent ⇒ no `data.ready_status`); a KSM default |
| `kube_node_labels` | Node label propagation (`kubernetes.io/*` etc.) | `cluster`, `node`, `label_*` | Optional |
| `kube_pod_spec_volumes_persistentvolumeclaims_info` | PVC nodes; pod-mounts-pvc edges | `cluster`, `namespace`, `pod`, `persistentvolumeclaim`, `volume` | Optional (no PVCs ⇒ no PVC nodes/edges) |
| `kube_persistentvolumeclaim_info` | PVC `data.storageclass` (the policy name, never a node) + `labels.volumename` (bound PV name; roots the Harvest join) | `cluster`, `namespace`, `persistentvolumeclaim`, `storageclass`, `volumename` | Optional (absent ⇒ no `data.storageclass` / `volumename`; no Harvest join) |
| `kube_pod_owner` | Pod controller-owner attribute `data.owner` = `{kind, name}` (ReplicaSet skipped to its Deployment; omitted when no controller owner). The resolved owner is also the join key for the pod's ArgoCD Application, and both drive the `application` / `controller` compound groups in the workload hierarchy | `cluster`, `namespace`, `pod`, `owner_kind`, `owner_name`, `owner_is_controller` | Optional (absent ⇒ no `data.owner`, and no `data.application` — the Application is keyed on the controller) |
| `kube_replicaset_owner` | Resolves a ReplicaSet pod-owner up to its owning Deployment | `cluster`, `namespace`, `replicaset`, `owner_kind`, `owner_name` | Optional (absent ⇒ ReplicaSet kept as owner) |
| `kube_job_owner{owner_kind="CronJob",owner_is_controller="true"}` | Resolves a Job up to its owning CronJob, **for pod ArgoCD Application resolution only** — the Kubernetes CronJob controller copies only `spec.jobTemplate.metadata` annotations onto the Jobs it creates, so ArgoCD's tracking-id never reaches a Job. Never alters `data.owner` | `cluster`, `namespace`, `job_name`, `owner_kind`, `owner_name`, `owner_is_controller` | Optional (absent ⇒ CronJob-managed pods carry no `data.application`); a KSM default |
| `kube_{deployment,statefulset,daemonset,replicaset,job,cronjob}_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Pod ArgoCD Application `data.application` (segment before the first `:` of the tracking-id), joined on `(cluster, namespace, kind, name)` against the pod's resolved controller owner — ArgoCD stamps the annotation on the workload object it applies, never on the pods a controller spawns. Nests the pod under the `application` compound group | `cluster`, `namespace`, the family's identity label (`deployment` / `statefulset` / `daemonset` / `replicaset` / **`job_name`** / `cronjob`), `annotation_argocd_argoproj_io_tracking_id` | Optional, **per family** (absent ⇒ no `data.application` for pods of that controller kind). Each **requires** `--metric-annotations-allowlist=<plural-resource>=[argocd.argoproj.io/tracking-id]` (NOT a KSM default). On a **query error** `replicaset` / `job` log-and-continue (cardinality accumulates with history); the other four fail the build |
| `kube_pod_container_info` | Pod container list `data.containers` = `[{name, image}]`, sorted by `(name, image)`; on a mid-window image change the latest-seen image wins per container | `cluster`, `namespace`, `pod`, `container`, `image` | Optional (absent ⇒ no `data.containers`); a KSM default |
| `kube_service_info` | Service nodes for `://` connection-string resolution (D29); `cluster_ip` (headless `None` ⇒ no `data.ipaddress`) | `cluster`, `namespace`, `service`, `cluster_ip` | Optional (absent ⇒ `://` endpoints fall back to `external`) |
| `kube_service_annotations` | Service ArgoCD Application `data.application` (segment before the first `:` of the tracking-id), which nests the service under the `application` compound group | `cluster`, `namespace`, `service`, `annotation_argocd_argoproj_io_tracking_id` | Optional (absent ⇒ no `data.application`). **Requires** `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id]` (NOT a KSM default) |
| `kube_persistentvolumeclaim_annotations` | PVC ArgoCD Application `data.application` (same parse as the service), which nests the PVC under the `application` compound group. An app-less PVC additionally **inherits** the lexically-smallest Application among the pods that mount it | `cluster`, `namespace`, `persistentvolumeclaim`, `annotation_argocd_argoproj_io_tracking_id` | Optional (absent ⇒ no own annotation; inheritance may still fill `data.application`). **Requires** `--metric-annotations-allowlist=persistentvolumeclaims=[argocd.argoproj.io/tracking-id]` (NOT a KSM default) |
| `kube_endpointslice_endpoints` | Service → backing-pod fan-out (`service-selects-pod` edges) | `cluster`, `namespace`, `endpointslice`, `targetref_kind`, `targetref_namespace`, `targetref_name` | Optional |
| `kube_endpointslice_labels` | Joins an EndpointSlice to its owning Service | `cluster`, `namespace`, `endpointslice`, `label_kubernetes_io_service_name` | Optional — **requires** `--metric-labels-allowlist=endpointslices=[kubernetes.io/service-name]` (NOT a KSM default); absent ⇒ no `service-selects-pod` resolution |

These 22 series come from eleven kube-state-metrics collectors (`pods`, `nodes`,
`services`, `persistentvolumeclaims`, `replicasets`, `endpointslices`,
`deployments`, `statefulsets`, `daemonsets`, `jobs`, `cronjobs`), needing
`list` + `watch` on those resource kinds and nothing else. The last five exist
solely for pod ArgoCD Applications: skip them and the graph is unchanged except
that pods carry no `data.application`. A minimal Helm values
file, the exact ClusterRole it generates, and the `cluster` / `az` / `env`
external-label requirements are in `docs/kube-state-metrics-preconditions.md`.

### Harvest + kubelet storage metrics

| Metric | Used for | Labels read | Required? |
|---|---|---|---|
| `volume_labels` | **Hop A — the whole storage topology.** PVC→aggregate join (a token derived from the PV name matched against `volume`), the `netapp-aggr` / `netapp-node` entities, and the PVC `svm` label. An info series: its value is ignored, only its labels are read. Read unfiltered | `cluster` (ONTAP cluster), `node`, `aggr`, `svm`, `volume` | Optional (absent ⇒ no NetApp nodes / edges / `svm`, and no QoS query is issued) |
| `qos_read_ops` / `qos_write_ops` / `qos_read_latency` / `qos_write_latency` / `qos_read_data` / `qos_write_data` | **Hop B — I/O** on `pvc-to-netapp-aggr` (`read_ops`, `write_ops`, `read_latency_us`, `write_latency_us`, `read_bytes_per_sec`, `write_bytes_per_sec`). Read **verbatim** — Harvest already resolves ONTAP counters (ops/s, average µs, bytes/s); never wrapped in `rate()`. Queried at `{lun=""}` so a LUN workload, which carries its FlexVol's `volume`, is never summed on top, and **scoped** to the FlexVol names hop A matched | `cluster`, `svm`, `policy_group`, `lun`, `volume` | Optional (absent ⇒ edge with no `metrics`) |
| `qos_policy_fixed_max_throughput_iops` / `qos_policy_fixed_max_throughput_mbps` | **Hop C — the declared ceiling** `max_iops` / `max_bytes_per_sec` on the same edge, joined on `(cluster, svm, policy_group)`. The `mbps` figure is the one converted value (× 1048576 → bytes/s) so it shares the unit of `read_bytes_per_sec` | `cluster`, `svm`, `name` (or `policy_group`) | Optional (absent ⇒ no ceiling fields; never `0`) |
| `aggr_new_status` | Aggregate `data.health` (`online` if sample is `1`, else `degraded`; omitted if no series) | `cluster`, `node`, `aggr` | Optional |
| `aggr_space_used` / `aggr_space_total` | Aggregate `data.usage` `{used_bytes, capacity_bytes}` | `cluster`, `node`, `aggr` | Optional |
| `node_new_status` | Controller `data.health` (same mapping as aggregate) | `cluster`, `node` | Optional |
| `kubelet_volume_stats_used_bytes` / `kubelet_volume_stats_capacity_bytes` | PVC `data.usage` `{used_bytes, capacity_bytes}` | `cluster`, `namespace`, `persistentvolumeclaim` | Optional |

Every label above is **stock Harvest output** — no relabel rule is required, and none is read. ONTAP volume names admit no `-`, so the bridge to Kubernetes is built in the backend: the PVC's bound PV name is rewritten into a match token (default: replace `-` with `_`) and matched against `volume` (default: as a suffix, so the provisioner's `storagePrefix` need not be declared). Both are configurable. See `docs/netapp-harvest-preconditions.md`.

Each is wrapped in `last_over_time(<metric>[<window>]) @ <end>` so the result
reflects the most recent value within the requested `[start, end]` window — except
`kube_pod_container_info`, which uses `tlast_over_time(...)` so each per-image
series carries its last-sample timestamp, letting the reader pick the latest image
per container (a recency pick that is accurate for near-now windows; see
`design.md` D-A4 for the far-past-window caveat).

### Service-graph metric — produced by [Tempo](https://grafana.com/docs/tempo/latest/metrics-generator/service_graphs/) or compatible generator

| Metric | Used for | Labels read | Required? |
|---|---|---|---|
| `traces_service_graph_request_total` | `pod-calls-pod` (intra/cross-cluster), `pod-calls-service` (may cross-cluster — the route-engine path anchors on the selected ingress cluster), `service-selects-pod` (may cross-cluster) edges; denominator for `data.metrics.rate`. A query **error** fails the build; an empty vector does not | `cluster`, `client`, `server`, `client_k8s_pod_uid`, `server_k8s_pod_uid`, plus peer-address labels used only for `server="unknown"` enrichment (`client_server_address`, then `client_network_peer_address`, then `client_net_peer_name`) | Optional as data (no series ⇒ no call edges); query error is fatal |
| `traces_service_graph_request_failed_total` | `data.metrics.error_rate` on measured edges | Same identity labels as `_total` (joined by exact series identity minus `__name__`) | Optional — absence / query error omits `error_rate` (never reports `0`) |
| `traces_service_graph_request_server_seconds_bucket` | `data.metrics.p90_server_ms` on measured edges (server-observed classic histogram) | Same identity labels as `_total`, plus `le` — read **raw**, no upstream aggregation, joined by identity minus `le` | Optional — absence / non-classic buckets omit `p90_server_ms` |
| `edge_relation` (a **dimension**, not a metric) | Value `link` marks a connector-materialised **span-link** edge: the edge is still emitted, but it measures a queue/DB hop rather than a request, so it contributes nothing to `data.metrics` | Read on all three series; excluded from the two RED selectors via `edge_relation!="link"` | Optional — a producer that does not set it is unaffected (a negative matcher retains series where the label is absent) |

Wrapped in `rate(traces_service_graph_request_total[<window>]) @ <end>`. Each
series carries a single `cluster` external label representing the trace source
(typically the cluster running Tempo's metrics-generator); this is the
**client-side** cluster of the call. The **server-side** cluster is recovered
at build time by joining `server_k8s_pod_uid` against the global topology
pod-UID index — Kubernetes pod UIDs are unique across clusters in practice,
so the lookup is unambiguous. Edges are only emitted when both endpoints
resolve. When an endpoint's pod-UID label is empty, the human-readable
`client`/`server` label is resolved by built-in **connection-string detection**
(no knob): a label containing the literal `://` is parsed as a URL — an
in-cluster `<service>.<namespace>.svc` name becomes a **single** `type="service"`
node **in the caller's own cluster** (so this connection-string path is always
intra-cluster; the route-engine path below may anchor on a family sibling, which
is why the edge type is registered `may_cross_cluster: true`), provided that
cluster holds the same-named Service. That service
node then fans out on-demand `service-selects-pod` edges to its backing pods
across **every same-family cluster** holding the same-named Service — so
`service-selects-pod` **may cross clusters**, modelling multi-cluster
service-mesh endpoint aggregation (clusters are one family when their names
match after collapsing digit runs, e.g. `prod-1` ↔ `prod-2`). A headless
`<pod>.<service>.<namespace>.svc` name resolves to the **same** service node (the
leading pod-hostname is dropped) — a `://` endpoint is never a specific pod. An
unresolvable URL, or one whose caller cluster does not hold the Service, becomes
an `external` node. A non-URL label (no `://`) also becomes an `external` node
via the missing pod-UID human-label fallback.

#### RED metrics on edges (`data.metrics`)

An edge receives a typed `data.metrics` object **iff** it is trace-derived
(produced by at least one `traces_service_graph_request_total` series) **and**
both endpoints resolved to a `type="pod"` node (real or synthesised) or a
`type="service"` node. **How** the endpoint was identified does not matter — a
pod UID, a `://` connection string, a `server="unknown"` peer address matched to
a Service `ClusterIP` or straight to a Pod IP, and an Istio route-engine
resolution all qualify — so both `pod-calls-pod` and `pod-calls-service` edges
can be measured.

No `metrics` key at all on:

- any edge with an `external` endpoint (the external node collapses every
  destination sharing one label string onto one identity);
- synthesised edges: `service-selects-pod` fan-out, the ingress chain's
  gateway-pod → backend-service hop, and topology edges (`pod-to-node`,
  `pod-mounts-pvc`);
- the route-hit ingress chain's **caller → ingress-service entry hop**. One
  series produces both that hop and the retained caller → backend edge; they are
  two projections of the same call, so only the backend edge — the one naming
  the actual destination — is measured, and summing `rate` across the chain
  never double-counts;
- an edge whose contributing series **all** carry `edge_relation="link"`. Those
  are span-link virtual edges (the call crosses a queue or a database and the
  two spans belong to different trace contexts), so they measure nothing. A
  mixed edge is measured over its non-link series only.

When present:

| Field | Meaning |
|---|---|
| `rate` | RED family: requests per second over the window (always > 0). Schema-optional because `data.metrics` is a union |
| `error_rate` | RED: failed fraction in `[0, 1]`. **Absent** when the failure counter could not be read; **`0`** when it was read and reported no failures — do not conflate the two |
| `p90_server_ms` | RED: 90th percentile **server-observed** duration in milliseconds |
| `read_ops` / `write_ops` | I/O family (`pvc-to-netapp-aggr` only): Harvest ops/s, verbatim |
| `read_latency_us` / `write_latency_us` | I/O family: Harvest average latency in microseconds, verbatim |
| `read_bytes_per_sec` / `write_bytes_per_sec` | I/O family: Harvest throughput in bytes per second, verbatim |
| `max_iops` / `max_bytes_per_sec` | I/O family: declared QoS ceiling on the same edge. `max_bytes_per_sec` is `qos_policy_fixed_max_throughput_mbps × 1048576`. Neither field can appear without a measurement; absence means "no declared ceiling", never `0` |

Both new series are **optional** and degrade gracefully: a missing metric,
empty result, or query error omits only the affected field (or leaves
rate-only metrics) and never fails the build. All three values are JSON
numbers rounded to **6 significant digits** and **may appear in exponent
form** (e.g. `3.86e-7` for one request over a 30-day window). Consumers must
not assume fixed-decimal rendering (`toFixed`) and must treat `0` as
semantically distinct from a very small non-zero value.

**Producer prerequisites for RED coverage:** the collector's `dimensions` must
be **identical across all three series** — the failure counter and the histogram
join the request counter by exact label identity, so a relabel or an extra
dimension applied to only one family joins nothing (surfaced as its own warn:
`failed_total_label_set_mismatch` / `server_seconds_bucket_label_set_mismatch`).
`add_metric_suffixes` must be on so the histogram is named
`..._server_seconds_bucket` (not `..._server_bucket`). Pod UIDs
(`client_k8s_pod_uid` / `server_k8s_pod_uid`) are no longer required for
measurement, but they give the most precise endpoint identity; multi-replica
collectors need trace-ID-aware routing (`loadbalancing` exporter with
`routing_key: traceID`) or client/server spans never pair, and an unpaired edge
resolves through the peer-address ladder — still measured, but its
`_server_seconds` is what the **client** observed, so `p90_server_ms` then
includes network time the server never saw.

**Query cost.** The histogram is read raw (`rate(..._bucket{...}[w])`, no
`sum by`), so roughly `edge-cardinality × bucket-count` series cross the wire.
That is deliberate: no low-cardinality label subset identifies an edge once
endpoints may come from peer addresses or connection strings, and a group-by
would silently merge unrelated edges' latency distributions. The metric is
optional — a store that refuses the query degrades exactly like an absent one
(no `p90_server_ms`, no build failure).

The `servicegraph` connector's **virtual peers** — `client="user"` (an
uninstrumented caller) and `unknown` (an unresolved peer) — are dropped at the
query layer (`client!~"user|unknown",server!~"user"`) and normally never appear
as nodes or edges. The match is exact and case-sensitive, so a `://` host that
merely *contains* `user` is unaffected. The **server side is narrowed to
`server!~"user"`** so a `server="unknown"` series still reaches the reader: when
its client resolves to a **real** pod and the client-recorded peer address
(`client_server_address`, then `client_network_peer_address`, then
`client_net_peer_name` — first non-empty wins) names a Kubernetes Service,
that peer is recovered into a `pod-calls-service` edge (or an `external` node
for a non-cluster address) instead of being dropped; every other
`server="unknown"` case is still dropped, byte-for-byte as before. The same
sentinel fragment is applied to the two RED series selectors, which carry one
matcher of their own — `edge_relation!="link"` (see the table above).

### Probes — diagnostics, not graph data

| PromQL | Purpose |
|---|---|
| `up` | Backs `GET /readyz`, and distinguishes "no data in window" (`outside_retention`) from "upstream healthy but window empty". Issued only for an **unfiltered** build — under any request filter, zero rows means "nothing in scope" and returns an empty `200`. Not graph data. |

The three `traces_service_graph_*` queries are also **skipped** when a filtered
build loaded neither pods nor services — admission cannot keep any series, and
those three are the one family no request matcher narrows.

**Not VictoriaMetrics series, not graph-input metrics:** the optional ClickHouse
Istio route store (`--route-store-dsn`) resolves global FQDN peers to Services
(off by default; a miss degrades to `external`); `kube_state_graph_*` are the
API's own `/metrics` self-metrics.

### Edge → metric mapping

| Edge type | Source metric(s) |
|---|---|
| `pod-mounts-pvc` | `kube_pod_spec_volumes_persistentvolumeclaims_info` |
| `pod-to-node` | `kube_pod_info` (`node` label; one per scheduled pod, intra-cluster) |
| `pvc-to-netapp-aggr` | Harvest `volume_labels`, matching the stock `volume` label against a token derived from the PVC's `volumename` (PV name) |
| `pod-calls-pod` | `traces_service_graph_request_total` |
| `pod-calls-service` | `traces_service_graph_request_total` (when target resolves to a service node via connection-string resolution) |
| `service-selects-pod` | `traces_service_graph_request_total` (connection-string resolution + `kube_endpointslice_*` join) |

### Multi-cluster and cross-cluster coverage

Cross-cluster paths and service-graph scenarios are covered by
`internal/integration/` tests against a `testcontainers-go` VictoriaMetrics
container. The suite spins up a real VictoriaMetrics, pushes hand-crafted
fixture series via `POST /api/v1/import/prometheus`, and drives the in-process
API — this is the sole verification path for multi-cluster, cross-cluster, and
service-graph behaviour.

## Configuration

| Flag                            | Env                              | Default              | Notes |
|---------------------------------|----------------------------------|----------------------|-------|
| `--prom-url`                    | `KSG_PROM_URL`                   | `http://localhost:8428` | VictoriaMetrics Prometheus-compatible endpoint. |
| `--listen-addr`                 | `KSG_LISTEN_ADDR`                | `:8080`              | HTTP listen address. |
| `--build-timeout`               | `KSG_BUILD_TIMEOUT`              | `15s`                | Per-build context timeout for `/v1/graph`. |
| `--api-timeout`                 | `KSG_API_TIMEOUT`                | `5s`                 | Per-request timeout for upstream calls outside a graph build (`/readyz` probe, outside-retention probe). |
| `--api-keys-file`               | `KSG_API_KEYS_FILE`              | (empty)              | Path to a file holding accepted API keys (one per line, `#` comments allowed). Designed for K8s `Secret` mounts. Reloaded periodically. |
| `--api-keys`                    | `KSG_API_KEYS`                   | (empty)              | Comma-separated literal keys. Dev only; ignored when `--api-keys-file` is set. |
| `--api-keys-reload-interval`    | `KSG_API_KEYS_RELOAD_INTERVAL`   | `30s`                | How often `--api-keys-file` is re-read. Set to `0` to disable hot reload. |
| `--log-level`                   | `KSG_LOG_LEVEL`                  | `info`               | `debug \| info \| warn \| error`. |
| `--az-label`                    | `KSG_AZ_LABEL`                   | `az`                 | Upstream label the `?az=` parameter is matched against. The request parameter name never changes — only the label binding. Must be a valid PromQL label name and differ from `--env-label`. |
| `--env-label`                   | `KSG_ENV_LABEL`                  | `env`                | Upstream label the `?env=` parameter is matched against. |
| —                               | `KSG_PROM_USERNAME`              | (empty)              | HTTP Basic Auth username for the upstream VictoriaMetrics endpoint. **Env-only — no flag exists**, because credential-carrying flags leak via `ps` and container specs. Must be set together with `KSG_PROM_PASSWORD`. |
| —                               | `KSG_PROM_PASSWORD`              | (empty)              | HTTP Basic Auth password for the upstream. Env-only, paired with `KSG_PROM_USERNAME` — setting exactly one of the two fails startup. Rotation requires a restart (no hot reload); changing a Secret-backed env var in a Deployment triggers a rollout anyway. |

### Upstream basic auth

When VictoriaMetrics is protected by basic auth (`-httpAuth.*`, vmauth, or an
authenticating reverse proxy), set both env vars — in Kubernetes, source them
from a `Secret`:

```yaml
env:
  - name: KSG_PROM_USERNAME
    valueFrom:
      secretKeyRef: { name: ksg-upstream-auth, key: username }
  - name: KSG_PROM_PASSWORD
    valueFrom:
      secretKeyRef: { name: ksg-upstream-auth, key: password }
```

Every upstream request (topology, service-graph, cluster discovery, the
`/readyz` probe) then carries `Authorization: Basic …`. The credential values
never appear in logs, traces, metrics, or error responses.

## Documentation

- **Upstream metrics catalog (all 41 series):** [`docs/upstream-metrics.md`](docs/upstream-metrics.md)
- **kube-state-metrics install / RBAC / allowlists:** [`docs/kube-state-metrics-preconditions.md`](docs/kube-state-metrics-preconditions.md)
- **NetApp Harvest relabel and hops:** [`docs/netapp-harvest-preconditions.md`](docs/netapp-harvest-preconditions.md)

The full API reference is served by the running server:

- **Interactive API reference (Scalar UI):** [`/docs`](http://localhost:8080/docs)
- **OpenAPI 3.1 spec:** [`/openapi.yaml`](http://localhost:8080/openapi.yaml) · [`/openapi.json`](http://localhost:8080/openapi.json)

The spec is generated from in-source annotations (`make docs`) and embedded into
the binary, so it always matches the running build. The Scalar UI loads its
front-end bundle from the jsDelivr CDN.

## Development

### First-time setup

Run **once** after cloning. Bootstraps the dev environment, downloads modules,
and installs host-level tools (`golangci-lint`, `govulncheck`). Mockery is
tracked via go.mod's `tool` directive (Go 1.24+) and invoked through
`go tool mockery` — no separate install step is required.

```bash
make init           # go mod download + dev tools
make doctor         # verify toolchain (go, golangci-lint, govulncheck, mockery, docker)
make init-hooks     # (optional) install pre-commit hook (gofmt + go vet)
```

Required: Go 1.25+. The toolchain pinned in `go.mod` (currently `go1.26.5`)
will be auto-fetched by Go on first build.

### Day-to-day commands

```bash
make build          # compile binary
make test           # unit + component + golden + property + integration (Docker required)
make lint           # golangci-lint
make vuln           # govulncheck
make cover          # coverage profile
```

### Mocks (mockery)

Production-side dependencies are exposed as small interfaces (`promql.Querier`,
`auth.Validator`, `clock.Clock`) so unit tests can substitute mockery-generated
mocks instead of fronting real services with `httptest.NewServer`. Mocks live
under `internal/<pkg>/mocks/` and are committed to git so CI does not need
mockery installed.

```bash
make mocks          # regenerate mocks after editing an interface
make verify-mocks   # CI-style freshness check (regen + git diff)
```

`.mockery.yaml` lists the configured interfaces. After **adding or editing any
interface** registered there, run `make mocks` and commit the regenerated
files — the `mocks-drift` CI job blocks merges otherwise.

### Test layout

| Suite | Where | Real I/O? |
|---|---|---|
| Unit | `pkg/{graph,build,promql,clock,cytoscape,kubegraph}/*_test.go` + `internal/{config,auth,telemetry}/*_test.go` | None — pure Go. |
| Component | `internal/api/*_test.go` | None — `MockQuerier` injected via interface; `httptest.NewServer` only wraps the server-under-test, never fakes upstream. |
| Golden | `internal/api/golden_test.go` + `testdata/golden/*.json` | None. Run with `-update` to refresh snapshots. |
| Integration | `internal/integration/*` | **Docker required.** testcontainers-go spins a real VictoriaMetrics container; `SkipIfDockerUnavailable` skips locally without Docker. CI runs the full suite. |

The boundary between unit and integration is strict: anything that touches a
TCP socket fronting an upstream service is integration. Unit tests must run
with no external dependencies.

## License

Apache-2.0
