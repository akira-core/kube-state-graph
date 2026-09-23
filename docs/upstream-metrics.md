# Upstream metrics used to build the graph

This is the operator catalog of every PromQL series `kube-state-graph` reads
from VictoriaMetrics when it builds a `/v1/graph` response.

The upstream is **one or more** installations. Each series below belongs to one
query family (`ksm`, `kubelet`, `harvest`, `servicegraph`, `probe`, `alerts`), and a
routing table decides which installation answers it — see
[`upstream-backend-routing.md`](upstream-backend-routing.md). With no routing
table configured every family is served by the single `--prom-url` endpoint and
the queries below are issued exactly as written.

The names are the `Query` constants in `pkg/promql/queries.go`. There is **no
configurable metric-name prefix**: every series is queried at its **bare**
name. A scrape path that publishes `kube_*` (or Harvest) under an organisational
prefix silently yields an empty graph — see [`BREAKING.md`](BREAKING.md).

Install-side companions:

- kube-state-metrics collectors, RBAC, Helm values, allowlists →
  [`kube-state-metrics-preconditions.md`](kube-state-metrics-preconditions.md)
- NetApp Harvest relabel, hops, templates →
  [`netapp-harvest-preconditions.md`](netapp-harvest-preconditions.md)

## Inventory (47 series)

| Family | Count | Producer |
|---|---|---|
| kube-state-metrics | 22 | KSM |
| NetApp Harvest | 18 | Harvest |
| kubelet | 2 | kubelet `/metrics` |
| traces service-graph | 3 | Tempo / Alloy `servicegraph` connector (or compatible) |
| alerts | 1 | Prometheus / vmalert `ALERTS` |
| probe | 1 | Prometheus `up` |

`up` is a diagnostic, not graph data. The other 46 series are the graph inputs.

## Fan-out per `/v1/graph` request

```
GET /v1/graph?start=&end=&…
        │
        ├─ ReadTopology — 37 queries in parallel, then up to 6 more
        │     19 kube-state-metrics   abort the build on query error
        │      3 kube-state-metrics legs log-and-continue (empty vector):
        │         kube_replicaset_annotations, kube_job_annotations
        │         (cardinality accumulates with history) and
        │         kube_pod_container_info (cardinality multiplies with
        │         containers, image variants and pod churn)
        │     12 Harvest + 2 kubelet + ALERTS  log-and-continue (empty vector)
        │     ── second wave, gated on kube_persistentvolumeclaim_info
        │        and volume_labels ────────────────────────────────────
        │      6 Harvest QoS workload legs, scoped to the FlexVol names
        │        the loaded claims matched; NOT issued when none did.
        │        Chunked by --netapp-qos-scope-batch-bytes; each chunk
        │        log-and-continue (empty vector)
        │
        ├─ up{}  — only when the request is unfiltered AND pods+nodes are empty
        │           (classifies outside_retention vs a genuine empty graph)
        │
        └─ ReadServiceGraph — 3 queries in parallel
              skipped entirely when the request is filtered AND the selector
              loaded neither pods nor services (those three series are never
              narrowed by ?cluster= / ?namespace= / ?az= / ?env=, so a
              mistyped filter would otherwise scan the whole estate)
```

v1 has no result cache: this fan-out runs on every request.

### Wrappers

| Family | PromQL wrapper |
|---|---|
| kube-state-metrics, Harvest, kubelet | `last_over_time(<metric>[<window>])` evaluated at `end` |
| `kube_pod_container_info` only | `tlast_over_time(...)` so each per-image series carries its last-sample timestamp |
| `traces_service_graph_*` | `rate(<metric>[<window>])` evaluated at `end` |
| `up` | bare `up` (no window) |

## Fan-out per `/v1/storage-graph` request

The storage build reads through the same topology fan-out under a narrower
plan: it issues only what a storage-flow body can carry, and it reads pods,
Kubernetes nodes and controllers BY REFERENCE — restricted to the names the
build's own earlier waves already named, never to the whole estate.

```
GET /v1/storage-graph?start=&end=&az=&env=&…
        │
        └─ ReadTopology (storage plan) — 18 queries in parallel (13 in hub
           mode), then up to 20 more across three by-reference waves (plus the
           hub's claim-keyed reads in hub mode)
              never issued: kube_pod_container_info, kube_service_info,
                kube_endpointslice_endpoints, kube_endpointslice_labels,
                kube_service_annotations (the body carries no containers
                and no service node)
              read UNRESTRICTED, exactly as for /v1/graph (18 legs): the
                claim-binding family (the scope root every wave below is
                computed from), kube_persistentvolumeclaim_info,
                kube_persistentvolumeclaim_annotations, the two kubelet
                families, every NetApp Harvest inventory leg (a storage root
                must be drawable with no claim), and ALERTS — except in HUB
                MODE (a request rooted at ontap_cluster=, aggr= or svm=; see
                "Storage-side roots read the claim chain through the volume
                hub" below), where volume_labels is read RESTRICTED and the
                five claim families leave the first wave: 13 legs
              ── recovery, only when the request carries application=;
                 starts with the first wave (request-derived) ─────────────
               three stages, each waiting on the previous. Stage 1: the six
               controller-annotation families restricted on
               annotation_argocd_argoproj_io_tracking_id to the root
               Applications (`(?:app)(?::.*)?`), composed with the fixed `!=""`
               and the same request matchers as the by-reference reads.
               Every family fails the build on a chunk error (the storage
               build fails closed).
               Capped at 16 chunks per family; past that the family is read
               once unrestricted and filtered in the reader
               (application_root_restriction_unbounded). Stage 2: kube_replicaset_owner
               for the recovered Deployment names and kube_job_owner for the
               recovered CronJob names (required; an empty name set issues
               nothing). Stage 3: kube_pod_owner once per non-empty kind
               (ReplicaSet = stage-1 ∪ stage-2, Job = stage-1 ∪ stage-2,
               StatefulSet, DaemonSet, and the direct Deployment / CronJob
               arms), required. Series are tallied under the family name and
               added to whatever the by-reference read of that family contributes.
               The wave yields pod names only.
              ── wave 1, gated on kube_pod_spec_volumes_persistentvolumeclaims_info
                 and, under application=, also on the recovery and on
                 kube_persistentvolumeclaim_annotations ───────────────────
               2 pod legs (kube_pod_info, kube_pod_owner), scoped to the pods
                 a claim binding names plus the request's pod=<ns>/<name>
                 roots plus the recovered names. Under application= the binding
                 half narrows to claims own-annotated with a root Application
                 or mounted by a recovered / pod= pod, every mounter of such a
                 claim included, so an unrelated binding pod is not read.
                 NOT issued when the scope is empty. Chunked by
                 --netapp-qos-scope-batch-bytes; a chunk error FAILS the build
              ── wave 2 (nodes), gated on wave 1 ─────────────────────────
               4 kube_node_* legs, scoped to the Kubernetes nodes the loaded
                 pods are scheduled on plus the request's node=<name> roots;
                 NOT issued when that scope is empty. Chunked the same way; a
                 chunk error FAILS the build
              ── wave 3 (controllers), gated on wave 1, itself two stages ──
               stage A: kube_replicaset_owner, kube_replicaset_annotations,
                 kube_job_owner, kube_job_annotations, kube_statefulset_annotations,
                 kube_daemonset_annotations — each scoped to the loaded pods'
                 owner names of its OWN kind; a kind no pod is owned by issues
                 nothing for it
               stage B, gated on stage A: kube_deployment_annotations (scoped
                 to direct Deployment owners plus every Deployment a landed
                 kube_replicaset_owner row resolved a ReplicaSet up to),
                 kube_cronjob_annotations (direct CronJob owners plus every
                 owner_name a landed kube_job_owner row carries)
               every family FAILS the build on a chunk error, including
                 kube_replicaset_annotations and kube_job_annotations (which
                 degrade on /v1/graph); the Job → CronJob hop suppression is
                 therefore unreachable on this endpoint
              ── wave 4 (QoS), gated on kube_persistentvolumeclaim_info
                 and volume_labels ────────────────────────────────────────
               6 Harvest QoS workload legs, scoped exactly as for /v1/graph;
                 a chunk error FAILS the build
```

In hub mode the claim families become waves of their own, read FROM the rooted
volume-label rows, and the waves above hang off them (read-storage-roots-through-volume-hub
D9 — the Harvest tail is unchanged):

```
 L1  volume_labels phase 1 (aggr / svm groups)   aggr_* node_* qos_policy_* ALERTS   app recovery st.1
 L2  kube_persistentvolumeclaim_info{volumename}  owner completion (svm= only)        app recovery st.2
 L3  bindings / pvc_annotations / kubelet ×2 {persistentvolumeclaim}   phase 2 {volume=~tok}   st.3
 L4  pods                                          QoS ×6
 L5  kube_node_* ×4      controllers stage A
 L6                      controllers stage B
```

Critical path: 6 round-trips in hub mode (4 for an unrooted request). Every
kube-state-metrics, kubelet and `ALERTS` query of a hub build carries the
request's `cluster` / `namespace` matchers and NO `az` / `env` matcher, and is
dispatched to every backend serving its family, while the Harvest legs are
still routed by `az` — both decisions from one routing snapshot.

**The storage build fails closed.** On `/v1/storage-graph` a query error of any
family — every Harvest leg, both kubelet volume-stats legs, every scoped
chunk, every volume-label phase, every application-recovery stage — fails the
build with HTTP 502 `reason: "upstream"` and `message: "upstream query failed:
<family>"`. `ALERTS` is the one exception: it still logs, counts and continues.
An empty vector is never a failure. The degrade column of the table below
applies to `/v1/graph` only.

No `up{}` probe and no service-graph read. A pod / node / controller name is
unique per namespace (or per cluster, for nodes) only, so a by-reference scope
may admit a same-named object from another namespace or cluster; it is
consulted by no loaded pod and the body is unchanged.

**Fan-out per build, outside hub mode** (design.md D6 / D11): the 18 unrestricted legs, plus 2 when the
pod scope is non-empty, plus 4 when any loaded pod is scheduled or a `node=`
root exists, plus 2 (owner + annotations) for each of ReplicaSet / Job that
owns a loaded pod, plus 1 each for StatefulSet / DaemonSet that owns one, plus
1 for Deployment when any ReplicaSet resolved to one (or a pod is directly
Deployment-owned), plus 1 for CronJob when any Job resolved one (or a pod is
directly CronJob-owned), plus 6 when a claim matched a FlexVol. An empty scope
with no roots reads 18; every controller kind present with a matched volume
reads 38. An `application=` root adds the recovery on top of whatever the
bindings still require:

| Request | Recovery queries | Then |
|---|---|---|
| `application=x`, nothing matches | 6 (stage 1 only) | no pod query when the only bindings are unrelated |
| one Deployment-managed claimless pod | 6 + 1 (`kube_replicaset_owner`) + 2 (`kube_pod_owner` for ReplicaSet and the direct Deployment arm) | pods 2, nodes 4, controllers 3 |
| one CronJob-managed claimless pod | 6 + 1 (`kube_job_owner`) + 2 (`kube_pod_owner` for Job and the direct CronJob arm) | pods 2, nodes 4, controllers 3 |
| every kind present, matched volume | 6 + 2 + 6 | the 38-leg forward maximum |

**Fan-out per build, in hub mode**: 13 first-wave families (the 18 above minus
the five claim families), plus 1 (`kube_persistentvolumeclaim_info`) when the
rooted rows yield a PV candidate, plus 4 (bindings, claim annotations, kubelet
×2) when a candidate names a claim, plus the pod / node / controller / QoS
additions above for what those claims lead to. `volume_labels` is issued once
per phase-1 chunk, plus once per owner-completion chunk (only with `svm=`, and
only for aggregates no `aggr=` root read whole), plus once per phase-2 chunk
when a claim matched. `TestBuildStorage_FanOutLegCount_Hub` pins:

| Request | Families | `volume_labels` queries |
|---|---|---|
| `aggr=`, no rooted volume embeds `pvc_` | 13 | 1 |
| `aggr=`, a candidate naming no claim | 14 | 1 |
| `aggr=`, one StatefulSet-owned pod on a matched volume | 31 | 2 (phase 1, phase 2) |
| `svm=`, the same path | 31 | 3 (phase 1, owner completion, phase 2) |

**Pod-only roots narrow every namespaced leg.** When every root is a
`pod=<ns>/<name>` root and the request carries no `namespace`, the parser adds
the roots' namespaces as the `namespace` selector — output-preserving, since a
pod-rooted body draws nothing outside them. Any storage-side, `node` or
`application` root suppresses this, and an explicit `namespace` always wins.

**Storage-side roots narrow the volume-label read.** `volume_labels` is the
largest leg of a NetApp estate and no request dimension narrows it, so a request
rooted at `?ontap_cluster=`, `?aggr=` and/or `?svm=` restricts it to the rooted
components, in phases:

1. **Phase 1 — the roots.** First-wave queries in up to two groups, mirroring
   how the projection combines the roots (it UNIONS `aggr=` with `svm=` and
   narrows both by `ontap_cluster=`): an aggregate group
   `volume_labels{cluster=~"…",aggr=~"…"}` and an SVM group
   `volume_labels{cluster=~"…",svm=~"…"}`, the cluster matcher present only
   when the request carries `ontap_cluster=` (a request rooted at ONTAP
   clusters alone issues `volume_labels{cluster=~"…"}`). Each group is chunked
   by the same byte budget, charging the repeated matcher at its rendered
   length; the groups' results are merged de-duplicated by label set.
   **Capped:** these are repeatable parameters whose count nothing bounds, so a
   restriction that would take more than sixteen queries in total is not
   applied at all — the leg reads unrestricted, logs that it did, the build is
   not in hub mode, and the body is unchanged.
2. **Owner completion — SVM roots only.** An aggregate's owning controller is a
   vote over every one of its series, and an SVM group returns only the SVM's
   share of each aggregate it touches. After phase 1 the family is re-read
   whole for every `(ONTAP cluster, aggregate)` an SVM-group row names, minus
   the aggregates an `aggr=` root already read whole —
   `volume_labels{cluster="…",aggr=~"…"}`, one chunked query per ONTAP cluster.
   Its rows feed the owner vote and the inventory, never the hub's claim read.
3. **Phase 2 — candidate recovery.** A claim's aggregate and SVM are picked
   lexically-smallest over its *whole* candidate set, so a Trident clone or a
   same-named FlexVol on a second filer could otherwise move a claim onto or off
   the rooted aggregate. After phase 1, and once `kube_persistentvolumeclaim_info`
   has landed, the family is read again restricted on `volume` to the derived
   tokens of exactly the claims phase 1 matched (`volume=~".*<token>"` in the
   default `suffix` mode, `volume=~"<token>"` in `exact`). This is the
   *forward* derivation the join already computes. It is **not issued** when
   phase 1 matched no claim; a rooted component with no claim is still drawn
   from the unrestricted aggregate / controller / policy families. When the
   restriction names ONLY ONTAP clusters, phase 2 additionally carries
   `cluster!~"…"` for them: phase 1 read those filers whole.

The three are merged, de-duplicated by label set, before the QoS wave and the
parse. `pod=`, `application=` and `node=` compose freely — the projection ANDs
each with the storage-exclusive roots. A request whose only storage-side root is
`node=` reads unrestricted (a path through a Kubernetes node is found from its
pods, not from the filer). The `contains` and `regex` volume-match modes read
unrestricted. `/v1/graph` carries no roots and never restricts.

**Storage-side roots read the claim chain through the volume hub.** A
restricted read IS hub mode. Every suffix of a phase-1 `volume` value that
starts with `pvc_` at the start of the name or right after a `_` yields a
candidate PV name (`_` → `-`); `kube_persistentvolumeclaim_info` is read
restricted to `volumename=~"<candidates>"`, and the claim-binding family,
`kube_persistentvolumeclaim_annotations` and the two kubelet families
restricted to `persistentvolumeclaim=~"<claims>"`, their rows then kept only
when `(zone, environment, cluster, namespace, claim)` names a loaded claim.
Extraction is a candidate generator, never a judge: a candidate naming no PV
loads nothing, and the forward join still decides every pick. No candidate
issues no claim query; no claim issues no binding, pod, node or controller
query. A claim scope that would take more than sixteen chunks is read once
unrestricted and filtered in the reader, with the same body. Two aggregated
warnings report the whole-hub misses: `storage_root_claim_miss` with
`reason="no_pv_candidate"` (rooted volumes, no `pvc_` in any name) and
`reason="no_claim"` (candidates that named no claim the build can reach); a
Debug line `storage graph volume hub` logs volumes / candidates / claims /
bindings on every hub build. A statically provisioned PV is not reached from a
storage root — see [netapp-harvest-preconditions.md](netapp-harvest-preconditions.md).

## Query error vs empty vector

The README **"Required?"** column answers "does an **empty vector** drop this
feature?". Query **errors** (timeout, 5xx, PromQL parse) are a separate axis:

| Legs | Query error | Empty vector |
|---|---|---|
| 19 kube-state-metrics topology queries | **Fails the build** (HTTP 5xx / mapped `build.Reason`) | Feature omitted (no pods, no IPs, no `://` services, …) |
| 2 accumulating-cardinality annotation families (`kube_replicaset_annotations`, `kube_job_annotations`) | Log-and-continue; empty vector — **except** a failure caused by the CALLER's own context (build timeout / client disconnect), which still fails the request (`optionalQueryFatal`). Cardinality grows with history (`revisionHistoryLimit` / Job history limits), not live object count. The degrade is silent in the response — alert on the self-metric `kube_state_graph_upstream_query_failures_total{query="kube_replicaset_annotations"}` / `{query="kube_job_annotations"}`, which `pkg/promql.Client` increments for every failed query regardless of which fetch helper called it, or on the `optional topology query failed` Warn | No `data.application` for bare-ReplicaSet / Job-owned pods — which also **reshapes the Cytoscape hierarchy** (those pods reparent from `…/application/<app>/controller/…` to `…/controller/…`, an `application` group node with no other member disappears, and a PVC that inherited its Application from such a pod re-inherits from a different mounter). A degraded `kube_job_annotations` additionally **suppresses the Job → CronJob hop** for that build (`topologyVectors.JobAnnotationsDegraded`): the hop is gated on "this Job carries no annotation of its own", which an unread family cannot establish, so following it would attribute a directly-managed Job's pod to its CronJob's Application — a wrong value, not a missing one. A genuinely annotation-less Job under an annotated CronJob therefore also loses its Application while the leg is degraded. Every degrade in this table is subtractive |
| `kube_pod_container_info` | Log-and-continue; empty vector — **except** a failure caused by the CALLER's own context, which still fails the request (`optionalQueryFatal`). Cardinality **multiplies** with the live object count (containers × image variants, and every pod that existed at any instant of the window), so it is the largest kube-state-metrics family and the first to meet a memory-derived series limit (`the number of matching timeseries exceeds …`). Alert on `kube_state_graph_upstream_query_failures_total{query="kube_pod_container_info"}`, or watch it approach the cap (below) | No `data.containers` on any pod; nothing else moves |
| Scoped `kube_pod_info` / `kube_pod_owner` chunks (`/v1/storage-graph` only) | **Fails the build** — a pod is topology, and a missing chunk would be a smaller, plausible, wrong body | n/a — an empty scope issues no query |
| Scoped `kube_node_info` / `kube_node_status_addresses` / `kube_node_labels` / `kube_node_status_condition` chunks (`/v1/storage-graph` only) | **Fails the build** — a Kubernetes node is topology, same reasoning as a pod chunk | n/a — an empty scope (no scheduled pod, no `node=` root) issues no query |
| Scoped `kube_replicaset_owner` / `kube_job_owner` / `kube_deployment_annotations` / `kube_statefulset_annotations` / `kube_daemonset_annotations` / `kube_cronjob_annotations` chunks (`/v1/storage-graph` only) | **Fails the build** — required exactly as their unscoped read is | n/a — an owner kind no loaded pod is owned by (or, for the two Stage-B families, no first-stage series resolved) issues no query |
| Scoped `kube_replicaset_annotations` / `kube_job_annotations` chunks (`/v1/storage-graph` only) | **Fails the build** — the storage build fails closed on every family but `ALERTS` | No Application for the ReplicaSets / Jobs concerned |
| `traces_service_graph_request_total` | **Fails the build** | No call edges; topology still returned |
| 18 Harvest + 2 kubelet + `ALERTS` | On `/v1/graph`: log-and-continue; empty vector — **except** a failure caused by the CALLER's own context (build timeout / client disconnect), which still fails the request (`optionalQueryFatal`). On `/v1/storage-graph`: **fails the build** (502 naming the family), `ALERTS` excepted | No NetApp chain / no PVC `usage` / no `data.alerts` |
| `traces_service_graph_request_failed_total` | Log-and-continue | Measured edges omit `error_rate` (never reports `0`) |
| `traces_service_graph_request_server_seconds_bucket` | Log-and-continue | Measured edges omit `p90_server_ms` |
| `up` | Warn; skip `outside_retention` classification | n/a |

An **unfiltered** build with zero parsed pods **and** zero parsed nodes, plus a
healthy `up{}`, is classified `outside_retention` (HTTP 400). A **filtered**
build never probes `up{}`: zero rows means "nothing in scope" and returns HTTP
200 with empty `elements`.

### Watching a leg approach an upstream series limit

VictoriaMetrics rejects a query whose matchers SELECT more series than
`-search.maxUniqueTimeseries` (derived from vmselect memory when unset) — the
count is taken before any aggregation, so no `sum by` helps. Every successful
query records its result size in
`kube_state_graph_upstream_query_result_series{query}` (buckets 1024 … 1048576;
one observation per issued query, per chunk of a scoped read, and per backend a
routed query reaches), and both build log lines (`graph built`,
`storage graph built`) name the `largest_leg`. A leg whose high quantile sits
one bucket below the cap is the next rejection:

```promql
histogram_quantile(0.99, sum by (query, le) (rate(kube_state_graph_upstream_query_result_series_bucket[1h])))
```

## Which request filter reaches which series

Selector-level parameters (`cluster`, `namespace`, `az`, `env`) are rendered
into the upstream queries. Which dimension is allowed on which series is
hardcoded (`pkg/promql/queryDims`):

| Series | `az` | `env` | `cluster` | `namespace` |
|---|---|---|---|---|
| namespaced KSM (pod / owner / claim / Service / EndpointSlice / ReplicaSet — every `kube_*` series except the `kube_node_*` family) + kubelet volume stats | yes | yes | yes | yes |
| `kube_node_*` | yes | yes | yes | no (nodes have no namespace; they follow **by reference** from scheduled pods) |
| NetApp Harvest | **no** matcher — `?az=` only *routes* the leg to a zone's `harvest` backend | no | **no** (Harvest `cluster` is the **ONTAP** cluster name) | no |
| `ALERTS` | yes (also routes) | yes | **no** — an alert expression does not reliably keep `cluster`; matching uses the label through the identity ladder when present, else unique-in-estate | yes |
| `traces_service_graph_*`, `up` | no | no | no | no |

`az` / `env` match `--az-label` / `--env-label` (defaults `az` / `env`). Every
topology family that a live dimension actually reaches must carry those labels;
a family that does not matches nothing, and the default connectivity prune can
then empty the graph. A `selector_family_empty` warning fires when KSM matched
but a kubelet family that the selector *can* narrow returned nothing. Harvest
is never in that set: its Harvest legs carry no request matcher at all, so
Harvest series need no `az` / `env` label — `?az=` selects which `harvest`
backend is asked (see `upstream-backend-routing.md`) and `?env=` does not reach
the family.

The `cluster` value `unknown` is rendered `cluster=~"unknown|"` (literal plus
the empty alternative) because an absent `cluster` label and a literal
`unknown` land in the same bucket.

The application recovery carries the same request matchers as the by-reference
reads of those families (`az`, `env`, `cluster`, `namespace`), composed with
the fixed selector and the recovery key. It adds no new `queryDims` entry.

## Fixed selectors (request-invariant)

These matchers are metric-selection contracts, identical for every request.
They are **not** caller filters and are always rendered **before** any
`?az=` / `?env=` / `?cluster=` / `?namespace=` matchers.

| Series | Fixed selector | Why |
|---|---|---|
| `kube_node_status_addresses` | `type=~"ExternalIP\|InternalIP"` | ExternalIP wins; InternalIP is the fallback when the node has no ExternalIP |
| `kube_node_status_condition` | `condition="Ready"` | Only the Ready condition is surfaced as `data.ready_status` |
| six `qos_{read,write}_{ops,latency,data}` | a data-derived `volume=~"…"` scope, nothing else | ONTAP also collects a per-LUN workload that carries its FlexVol's `volume`. It IS fetched — on a SAN backend it is the only series naming the QoS policy — and the reader discards non-empty-`lun` rows from every I/O sum, which is stricter than a `lun=""` matcher (that also admits rows omitting the label). The `volume` alternation names the FlexVols the loaded claims matched — derived from upstream data, not from the request |
| `traces_service_graph_request_total` | `client!~"user\|unknown",server!~"user"` | Drops the connector's virtual peers (`client="user"` / `"unknown"`, `server="user"`). Exact, case-sensitive. `server="unknown"` **is** admitted so the unknown-server peer-address ladder can run |
| two RED series | same sentinel **plus** `edge_relation!="link"` | Span-link series still produce an edge from `_total`, but they do not contribute rate / error / latency. An absent `edge_relation` label is retained (`!=` treats missing as `""`) |
| `kube_job_owner` | `owner_kind="CronJob",owner_is_controller="true"` | The reader (`resolveJobCronJobOwners`) keeps exactly those rows — Jobs owned by a CronJob controller. Every other owner kind, a non-controller row, or a missing `owner_is_controller` is discarded before it is keyed or counted |
| six controller-annotation families (`kube_{deployment,statefulset,daemonset,replicaset,job,cronjob}_annotations`) | `annotation_argocd_argoproj_io_tracking_id!=""` | The reader (`resolveApplications`) skips an empty or absent tracking-id before `keyOf`. PromQL treats a missing label as `""`, so an **allowlisted** family returns only its annotated objects instead of one series per workload object. It saves nothing on the **un-allowlisted** case — KSM short-circuits an un-allowlisted resource to an empty family (see [`kube-state-metrics-preconditions.md`](kube-state-metrics-preconditions.md)) |
| `ALERTS` | `alertstate="firing"` | Only firing alerts are active in the window. `pending` is a threshold crossed for less than its `for:` and is excluded upstream |

## kube-state-metrics (22)

20 abort-on-error; `kube_replicaset_annotations` and `kube_job_annotations`
log-and-continue (accumulating cardinality —
harden-controller-annotation-legs D3). Empty vectors
degrade as in the last column.

| Metric | Graph role | Labels read | Empty vector |
|---|---|---|---|
| `kube_pod_info` | `type="pod"` nodes; `pod-to-node` via `node`; `data.ipaddress` from `pod_ip` (`host_ip` is not exported) | `cluster`, `namespace`, `pod`, `uid`, `node`, `pod_ip` | No pods |
| `kube_node_info` | `type="node"` nodes | `cluster`, `node` | No K8s nodes |
| `kube_node_status_addresses{type=~"ExternalIP\|InternalIP"}` | Node `data.ipaddress` — ExternalIP preferred, InternalIP fallback; other address types ignored; duplicate `(cluster, node)` within a type keeps the lexically-smallest address | `cluster`, `node`, `type`, `address` | Node has no `ipaddress` |
| `kube_node_labels` | Node `labels` (KSM `label_*` keys) | `cluster`, `node`, `label_*` | Node `labels` empty of those keys |
| `kube_node_status_condition{condition="Ready"}` | Node `data.ready_status` ∈ {`Ready`, `NotReady`, `Unknown`} from the **active** (`value=1`) Ready row; `status` matched case-insensitively (`true`/`True`). Omitted when no Ready data — **not** the same as `"Unknown"` (kubelet lost contact) | `cluster`, `node`, `condition`, `status` | Attribute omitted |
| `kube_pod_spec_volumes_persistentvolumeclaims_info` | PVC nodes + `pod-mounts-pvc` | `cluster`, `namespace`, `pod`, `persistentvolumeclaim`, `volume` | No PVCs / mount edges |
| `kube_persistentvolumeclaim_info` | PVC `data.storageclass` (policy name, never a node) + `labels.volumename` (bound PV name; Harvest join key) | `cluster`, `namespace`, `persistentvolumeclaim`, `storageclass`, `volumename` | No StorageClass / no Harvest join |
| `kube_service_info` | Service **index** for `://` resolution (D29). A `type="service"` node is materialised only when a connection-string / route-engine / peer-address path needs it — this series is not emitted as nodes on its own. `cluster_ip` → `data.ipaddress`; headless `"None"` omits it | `cluster`, `namespace`, `service`, `cluster_ip` | `://` endpoints fall back to `external` |
| `kube_endpointslice_endpoints` | `service-selects-pod` fan-out | `cluster`, `namespace`, `endpointslice`, `targetref_kind`, `targetref_namespace`, `targetref_name` | No backing-pod edges |
| `kube_endpointslice_labels` | EndpointSlice → Service name. **Requires** `--metric-labels-allowlist=endpointslices=[kubernetes.io/service-name]` (not a KSM default) | `cluster`, `namespace`, `endpointslice`, `label_kubernetes_io_service_name` | Same as above |
| `kube_pod_owner` | Pod `data.owner` = `{kind, name}` (ReplicaSet skipped to its Deployment). The resolved owner is also the **join key** for the pod's `data.application` — the tracking-id lives on the controller, never on the pod | `cluster`, `namespace`, `pod`, `owner_kind`, `owner_name`, `owner_is_controller` | No `owner`, and no pod Application (it is keyed on the controller) |
| `kube_replicaset_owner` | ReplicaSet → Deployment | `cluster`, `namespace`, `replicaset`, `owner_kind`, `owner_name` | ReplicaSet kept as owner |
| `kube_job_owner{owner_kind="CronJob",owner_is_controller="true"}` | Job → CronJob, **for pod `data.application` only** — the CronJob controller copies only `spec.jobTemplate.metadata` annotations onto the Jobs it creates, so ArgoCD's tracking-id never reaches a Job. Never alters `data.owner` | `cluster`, `namespace`, `job_name`, `owner_kind`, `owner_name`, `owner_is_controller` | CronJob-managed pods carry no Application |
| `kube_deployment_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Pod `data.application` for Deployment-owned pods (segment before the first `:` of the tracking-id), joined on `(cluster, namespace, kind, name)` against the pod's resolved controller. **Requires** `--metric-annotations-allowlist=deployments=[argocd.argoproj.io/tracking-id]` | `cluster`, `namespace`, `deployment`, `annotation_argocd_argoproj_io_tracking_id` | No Application for pods of that kind |
| `kube_statefulset_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Same, for StatefulSet-owned pods. **Requires** `--metric-annotations-allowlist=statefulsets=[…]` | `cluster`, `namespace`, `statefulset`, `annotation_argocd_argoproj_io_tracking_id` | Same |
| `kube_daemonset_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Same, for DaemonSet-owned pods. **Requires** `--metric-annotations-allowlist=daemonsets=[…]` | `cluster`, `namespace`, `daemonset`, `annotation_argocd_argoproj_io_tracking_id` | Same |
| `kube_replicaset_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Same, for pods whose owner stayed a bare ReplicaSet. **Requires** `--metric-annotations-allowlist=replicasets=[…]`. Query error **degrades** (log-and-continue) — cardinality accumulates with `revisionHistoryLimit` | `cluster`, `namespace`, `replicaset`, `annotation_argocd_argoproj_io_tracking_id` | Same |
| `kube_job_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Same, for Job-owned pods. Identity label is **`job_name`**, not `job`. **Requires** `--metric-annotations-allowlist=jobs=[…]`. Query error **degrades** (log-and-continue) — cardinality accumulates with Job history limits | `cluster`, `namespace`, `job_name`, `annotation_argocd_argoproj_io_tracking_id` | Same |
| `kube_cronjob_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Same, for pods reached through `kube_job_owner`. **Requires** `--metric-annotations-allowlist=cronjobs=[…]` | `cluster`, `namespace`, `cronjob`, `annotation_argocd_argoproj_io_tracking_id` | Same |
| `kube_pod_container_info` | Pod `data.containers` = `[{name, image}]`, ordered by `(name, image)`; latest-seen image wins on a mid-window change | `cluster`, `namespace`, `pod`, `container`, `image` | Attribute omitted. A query error **degrades** too. Never read by `/v1/storage-graph` |
| `kube_service_annotations` | Service `data.application`. **Requires** `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id]` | `cluster`, `namespace`, `service`, `annotation_argocd_argoproj_io_tracking_id` | Attribute omitted |
| `kube_persistentvolumeclaim_annotations` | PVC's **own** `data.application` (same parse). **Requires** `--metric-annotations-allowlist=persistentvolumeclaims=[argocd.argoproj.io/tracking-id]`. An app-less PVC additionally **inherits** the lexically-smallest Application among pods that mount it | `cluster`, `namespace`, `persistentvolumeclaim`, `annotation_argocd_argoproj_io_tracking_id` | Own annotation omitted; inheritance may still fill it |

The eleven KSM collectors that produce these 22 series, the ClusterRole, and
Helm values are in
[`kube-state-metrics-preconditions.md`](kube-state-metrics-preconditions.md).

## NetApp Harvest (18)

All 18 are OPTIONAL (log-and-continue). Harvest `cluster` is the ONTAP cluster
and is **never** used as a Kubernetes `?cluster=` matcher. The family carries no
`?az=` / `?env=` matcher either: `?az=` routes the legs to a zone's `harvest`
backend and the query string stays unfiltered.

The storage join is three independently-degrading hops. Hops A and B are keyed
by the STOCK Harvest `volume` label (the ONTAP FlexVol name), matched against a
token derived from the PVC's `volumename` (bound PV name) — ONTAP volume names
admit no `-`, so the two are never compared for equality. The derivation is
operator-configurable and defaults to "replace `-` with `_`, match as a suffix",
which resolves a stock Trident estate without the deployment declaring its
`storagePrefix`. Hop C is keyed on the `(ontap_cluster, svm, policy_group)`
triple, assembled from both topology hops: hop A owns the ONTAP cluster of the
picked aggregate and the SVM the `volume_labels` match resolved, and hop B owns
the `policy_group` — the only upstream statement of which policy governs this
FlexVol. An incomplete or unmatched triple is ignored, never widened to an
SVM-wide figure. A ceiling therefore never appears without a measurement: its
policy group is recovered FROM a matched workload series. **No relabel rule is required
or read.** The six hop-B legs are issued in a second wave, scoped to the FlexVol
names hop A matched. See
[`netapp-harvest-preconditions.md`](netapp-harvest-preconditions.md).

| Metric | Hop | Graph role | Empty / miss |
|---|---|---|---|
| `volume_labels` | A — topology | Sole source of the storage *shape*: `pvc-to-netapp-aggr`, `netapp-aggr` / `netapp-node`, PVC `labels.svm`. Info series: sample **value discarded**, labels only (`cluster`, `node`, `aggr`, `svm`, `volume`). Read UNFILTERED — the interesting FlexVol names are not known until it has been read — except by a `/v1/storage-graph` request rooted at `ontap_cluster=` / `aggr=`, which reads it restricted to those components and then re-reads it for the matched claims' tokens | No NetApp nodes, edges, or `svm`, and no QoS query is issued at all |
| `qos_read_ops` | B — I/O | `data.metrics.read_ops` (ops/s, verbatim — never `rate()`) | Edge kept, no I/O fields |
| `qos_write_ops` | B | `write_ops` | same |
| `qos_read_latency` | B | `read_latency_us` (average µs, verbatim) | same |
| `qos_write_latency` | B | `write_latency_us` | same |
| `qos_read_data` | B | `read_bytes_per_sec` (bytes/s, verbatim) | same |
| `qos_write_data` | B | `write_bytes_per_sec` | same |
| `qos_policy_fixed_max_throughput_iops` | C — ceiling | `max_iops`, joined on the `(ontap_cluster, svm, policy_group)` triple — cluster and svm from hop A, policy group from hop B. Policy identity read as `name` with a `policy_group` fallback; smallest value on a duplicate triple | No ceiling (never `0`). A ceiling cannot appear without a measurement. A volume in no policy group gets none — another group's figure is never borrowed |
| `qos_policy_fixed_max_throughput_mbps` | C | `max_bytes_per_sec` = mbps × 1048576 (the one converted value, so it shares the unit of `read_bytes_per_sec`) | same |
| `aggr_new_status` | — | Aggregate `data.health` (`online` if sample is `1`, else `degraded`; omitted if no series) | Attribute omitted |
| `aggr_space_used` | — | Aggregate `data.usage.used_bytes` | `usage` incomplete / omitted |
| `aggr_space_total` | — | Aggregate `data.usage.capacity_bytes` | same |
| `node_new_status` | — | Controller `data.health` (same mapping as aggregate) | Attribute omitted |
| `node_labels` | — | Controller `data.hardware` `{model, serial, version, vendor, location}` from like-named labels. Info series: sample value discarded | Attribute omitted |
| `node_cpu_busy` | — | Controller `data.perf.cpu_busy_pct` (percent, verbatim) | Field omitted |
| `node_total_ops` | — | `data.perf.total_ops` | Field omitted |
| `node_total_latency` | — | `data.perf.total_latency_us` | Field omitted |
| `node_total_data` | — | `data.perf.total_bytes_per_sec` | Field omitted |

Coverage warnings (each gated on its **own** family having been read):
`netapp_volume_join_miss` (hop A miss or empty `aggr`; under a **storage-rooted**
request it counts only claims that matched at least one series, so a FlexGroup
still reports while a claim off the rooted components — which the request never
asked about — does not), `netapp_qos_join_miss` (edge drawn, no QoS match). No
warning for a missing ceiling — a volume in no policy group is normal.

## kubelet (2)

OPTIONAL (log-and-continue). Namespaced — `?cluster=` / `?namespace=` /
`?az=` / `?env=` all apply.

| Metric | Graph role | Labels read | Empty vector |
|---|---|---|---|
| `kubelet_volume_stats_used_bytes` | PVC `data.usage.used_bytes` | `cluster`, `namespace`, `persistentvolumeclaim` | `usage` incomplete / omitted |
| `kubelet_volume_stats_capacity_bytes` | PVC `data.usage.capacity_bytes` | same | same |

Per-field independent: a used-only or capacity-only result still populates
what it can.

## traces service-graph (3)

Never narrowed by request matchers. Skipped as a group when a **filtered**
build loaded neither pods nor services.

| Metric | Graph role | Empty / error |
|---|---|---|
| `traces_service_graph_request_total` | Trace-derived edges: `pod-calls-pod`, `pod-calls-service`. Also the source of on-demand `service-selects-pod` fan-out (with the EndpointSlice join). Denominator for `data.metrics.rate`. **Does not** exclude `edge_relation="link"` — those series still emit an edge | Query error fails the build. Empty vector ⇒ no call edges |
| `traces_service_graph_request_failed_total` | `data.metrics.error_rate` on measured edges. Joined to `_total` by **exact series identity** (minus `__name__`) | Omits `error_rate` (never `0`) |
| `traces_service_graph_request_server_seconds_bucket` | `data.metrics.p90_server_ms` (server-observed classic histogram). Read **raw** (no upstream `sum by`); joined by identity minus `le` | Omits `p90_server_ms` |

Labels read on the total series (the two RED series must carry the **same**
identity labels or they join nothing — logged as
`failed_total_label_set_mismatch` / `server_seconds_bucket_label_set_mismatch`):

- `cluster` — trace-source / **client-side** cluster. The server-side cluster
  is recovered by looking up `server_k8s_pod_uid` in the topology pod-UID index
- `client`, `server`
- `client_k8s_pod_uid`, `server_k8s_pod_uid`
- `edge_relation` — value `link` marks a span-link logical edge
  (`labels.relation="link"`); those series are excluded from the two RED
  selectors
- unknown-server peer-address ladder (checked in this order; first non-empty
  wins): `client_server_address`, `client_network_peer_address`,
  `client_net_peer_name`
- span-link broker derivation (always on, no knob): the mirrored SERVER-side
  peer labels `server_server_address`, `server_net_peer_name`,
  `server_dns_answers`, `server_server_port` — read on an `edge_relation="link"`
  series to derive the consumer side's broker node and mark the backing hop
  `labels.relation="transport"`. A collector that drops them still emits the
  `link` edge, but the `transport` marker is lost
- route-engine extras when `--route-store-dsn` is set: `client_dns_answers`
  (required to consult the engine), `client_server_port` / `client_net_peer_port`

`client_network_peer_port` is deliberately **not** read: the stable conventions
split the port into its own attribute, and a port takes part in neither peer
identification nor node naming.

`add_metric_suffixes` must stay on so the histogram is named
`..._server_seconds_bucket` (not `..._server_bucket`).

`data.metrics` (RED) is attached only when the edge is trace-derived, both
resolved endpoints are `type="pod"` or `type="service"`, the hop is not the
ingress-chain **entry** hop, and at least one contributing series is not
`edge_relation="link"`. No RED on: any `external` endpoint; synthesised edges
(`service-selects-pod`, ingress gateway-pod → backend, topology edges); an
all-link edge.

## ALERTS (1)

OPTIONAL (log-and-continue). The `alerts` family is the first **optional**
routing family: a table serving it on no backend is valid and the overlay is
off. `pending` alerts are excluded by the fixed `alertstate="firing"` selector.
The series takes `az`, `env` and `namespace` matchers but **never** `cluster`;
`namespace` renders in its or-absent form (`namespace=~"shop|"`) so the
namespace-less node / controller / aggregate alerts are not excluded upstream.

Matching is label-set, most-specific kind first: `{namespace, pod}` → pod;
else `{namespace, persistentvolumeclaim}` → PVC; else `{aggr}` → aggregate
(it outranks `node` because the stock Harvest `aggr_*` series carry the
owning controller's `node` beside `aggr`); else `{node}` → Kubernetes node
**or** ONTAP controller. When `cluster` is
present it walks the Kubernetes identity ladder for kinds 1–2, and for `{cluster, node}` a Kubernetes identity hit matches the K8s node, a raw label
equal to a known ONTAP cluster matches the controller, and **both** is counted
ambiguous and attached to neither — a Kubernetes cluster and an ONTAP cluster
that share a raw name cannot be disambiguated from labels alone. Missing
`cluster` succeeds only when exactly one node of the eligible kind(s) matches.

Operator precondition: the alerting store MUST stamp the same `az` / `env`
external labels as kube-state-metrics, or its alerts vanish under those
filters. `FamilyAlerts` is excluded from the `selector_family_empty` Warn — an
empty alert vector is the healthy estate.

| Metric | Graph role | Empty / error |
|---|---|---|
| `ALERTS` | `data.alerts` `[{name, state, severity}]` on pod / K8s node / PVC / netapp-node / netapp-aggr. Sorted by `(name, severity)`, omitted when empty | Attribute omitted; build succeeds |

`severity` is compared case-insensitively when the builder folds
`data.status`: `critical` → `critical`; `warning`, an empty value, or any
unrecognised value → `warning`; `info` and `none` have no effect. The same fold
treats NetApp `health="degraded"` and Kubernetes `ready_status="NotReady"` as
critical, and `ready_status="Unknown"` as warning. Worst wins; raw `data.perf`
never participates because performance thresholds belong in alert rules.

Eligible node kinds (`pod`, `node`, `pvc`, `netapp-node`, `netapp-aggr`) always
carry one of `normal`, `warning`, or `critical`. `normal` means only **no
negative signal was observed**; it is not proof that the `alerts` family or
every optional health series was available. Services, externals, SVMs, and
synthesised compound groups carry no `status` key.

## Probe (1)

| Metric | When issued | Role |
|---|---|---|
| `up` | `/readyz`; and during an **unfiltered** build whose topology parsed to zero pods and zero nodes | Distinguishes `outside_retention` from "upstream healthy, window empty". Never materialises nodes or edges |

## Not PromQL — also not graph-input metrics

These are sometimes confused with the catalog above:

| Input | Role |
|---|---|
| ClickHouse Istio route store (`--route-store-dsn` / `KSG_ROUTE_STORE_DSN`) | **Opt-in.** Resolves a global FQDN peer to a Kubernetes Service. Off by default; setting the DSN also **requires** `--router-check-bin` (the native Envoy `router_check_tool`) or the server refuses to start. A miss degrades to the existing `external` node and **never** fails a build. Not a VictoriaMetrics series |
| `kube_state_graph_*` | The API's **own** Prometheus self-metrics (`/metrics`). Not read as graph input |
| `kube_storageclass_info`, `kube_tridentvolume_info`, `kube_tridentbackend_info` | **No longer queried.** The StorageClass node type is gone; Trident CRS metrics are unused. See [`BREAKING.md`](BREAKING.md) |

## Edge type → source metric

| Edge type | Source |
|---|---|
| `pod-to-node` | `kube_pod_info` (`node` label) |
| `pod-mounts-pvc` | `kube_pod_spec_volumes_persistentvolumeclaims_info` |
| `pvc-to-netapp-aggr` | Harvest `volume_labels`, matching the stock `volume` label against a token derived from the PVC's `volumename` |
| `pod-calls-pod` | `traces_service_graph_request_total` |
| `pod-calls-service` | `traces_service_graph_request_total` (target resolved to a service node: `://` connection string, unknown-server peer, or route engine) |
| `service-selects-pod` | same total series + `kube_service_info` / `kube_endpointslice_*` (on-demand fan-out; not a raw series of its own) |
| `storage-flow` | Harvest hop A (claim → SVM / aggregate / controller) + `kube_pod_spec_volumes_persistentvolumeclaims_info` + `kube_pod_info`. Emitted only by `GET /v1/storage-graph` |

## Verifying the store

Against centralised VictoriaMetrics:

```promql
# Every name the builder can query (47, `up` included). The alternation is
# explicit on purpose: the useful answer is which name is MISSING from the
# output, and a `kube_.+` / `qos_.+` wildcard buries that under the hundreds
# of other series kube-state-metrics and Harvest publish.
count by (__name__) ({__name__=~"ALERTS|aggr_new_status|aggr_space_total|aggr_space_used|kube_cronjob_annotations|kube_daemonset_annotations|kube_deployment_annotations|kube_endpointslice_endpoints|kube_endpointslice_labels|kube_job_annotations|kube_job_owner|kube_node_info|kube_node_labels|kube_node_status_addresses|kube_node_status_condition|kube_persistentvolumeclaim_annotations|kube_persistentvolumeclaim_info|kube_pod_container_info|kube_pod_info|kube_pod_owner|kube_pod_spec_volumes_persistentvolumeclaims_info|kube_replicaset_annotations|kube_replicaset_owner|kube_service_annotations|kube_service_info|kube_statefulset_annotations|kubelet_volume_stats_capacity_bytes|kubelet_volume_stats_used_bytes|node_cpu_busy|node_labels|node_new_status|node_total_data|node_total_latency|node_total_ops|qos_policy_fixed_max_throughput_iops|qos_policy_fixed_max_throughput_mbps|qos_read_data|qos_read_latency|qos_read_ops|qos_write_data|qos_write_latency|qos_write_ops|traces_service_graph_request_failed_total|traces_service_graph_request_server_seconds_bucket|traces_service_graph_request_total|up|volume_labels"})

# The one REQUIRED non-default KSM label.
count(kube_endpointslice_labels{label_kubernetes_io_service_name!=""})

# Harvest join key present on both hops. Compare a `volume` value here with a
# claim's `volumename` to check the configured derivation fits the estate.
count by (volume) (volume_labels)
count(qos_read_ops)
```

The code-side pins: `TestQueryDims_EveryQueryListed` fails if a `Query`
constant is missing from the dimension table; `TestReadTopology_FanOutLegCount`
fails if topology issues anything other than 37 queries when no claim matches a
Harvest volume, or 43 when one does. `TestBuildStorage_FanOutLegCount` pins the
storage plan's by-reference fan-out the same way: 18 legs when nothing is
named, growing by exactly what the loaded pods, nodes and resolved owners name
(design.md D6) — 22 with a `node=` root alone, 25 / 27 / 27 with one
StatefulSet-, Deployment- or CronJob-owned pod, and 32 with every controller
kind present, 38 with one matched FlexVol added.
`TestBuildStorage_FanOutLegCount_Hub` pins hub mode: 13 families with no
candidate, 14 with a candidate naming no claim, 31 for one claim's full path.
