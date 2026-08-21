## Why

Every `/v1/graph` request loads **every cluster's every pod** from the
centralised VictoriaMetrics and only then narrows the result in memory. That
was a deliberate v1 rule ("no filters pushed to PromQL") chosen so one built
graph could later serve any filter from a cache — but the cache never came, the
deployment has grown to many clusters across several availability zones and
dev/test/prod environments, and the dominant use of the API is now a
**namespace-scoped storage question**: *which nodes, claims, and NetApp
aggregates does this namespace sit on, in this zone, in this environment?*
Answering it today costs a full multi-cluster fan-out whose result is thrown
away at projection time — and the default connectivity prune then hides the
idle, storage-bearing pods the question is about.

Every topology series the service reads — kube-state-metrics, kubelet, NetApp
Harvest — already carries scrape-time labels naming the availability zone and
environment of its source, alongside `cluster` and (where it applies)
`namespace`. Turning those four into **upstream selectors** lets
VictoriaMetrics do the narrowing before a sample crosses the wire. The
service-graph series are deliberately left **unfiltered**: their `cluster`
label is unreliable, and a complete traffic picture is what lets a narrowed
topology stay internally consistent. The same pass makes the prune an explicit
parameter, retires the name filter and traversal parameters that the prune's
escape hatches were built around, and removes `/v1/clusters`.

## What Changes

### Added

- **Two request parameters on `GET /v1/graph`: `az` and `env`.** Optional,
  repeatable, OR-combined (like `cluster` / `namespace`). Each renders an
  upstream label matcher — a single value as `<key>="<value>"`, several as one
  anchored alternation `<key>=~"<v1>|<v2>"` with every alternative
  regex-quoted. Values are validated before rendering (bounded length, no
  control characters) so a request can never inject selector syntax; an
  invalid value is a 400.
- **Configurable label keys.** The upstream label names `az` / `env` bind to
  default to `az` and `env`, overridable via `KSG_AZ_LABEL` / `KSG_ENV_LABEL`
  (+ `--az-label` / `--env-label`, normal flag/env precedence), validated as
  PromQL label names at startup (fail fast). The request parameter names are
  fixed; only the upstream binding moves. Threaded through `build.Options` and
  `kubegraph.Options`.
- **Selector-level push-down of `az`, `env`, `cluster`, `namespace`.** Each
  topology query receives every matcher whose label it carries:
  - `az` + `env` + `cluster` + `namespace`: the pod/claim-scoped KSM series
    (`kube_pod_info`, `kube_pod_spec_volumes_persistentvolumeclaims_info`,
    `kube_persistentvolumeclaim_info`, `kube_pod_owner`,
    `kube_replicaset_owner`, `kube_pod_container_info`,
    `kube_persistentvolumeclaim_annotations`), the Service / EndpointSlice
    series (`kube_service_info`, `kube_service_annotations`,
    `kube_endpointslice_endpoints`, `kube_endpointslice_labels`), and the two
    kubelet volume-stats series.
  - `az` + `env` + `cluster`: the four `kube_node_*` series (no namespace).
  - `az` + `env` **only**: every Harvest series. Harvest's `cluster` label is
    the **ONTAP** cluster name, not a Kubernetes cluster — a pushed `cluster`
    matcher would return nothing. Harvest has no `namespace` either; it is
    narrowed by **reference** (an aggregate materialises only when a loaded
    claim's `volumename` joins to it — the existing rule).
  - **None**: the three `traces_service_graph_*` series and the `up{}` probe.
  A request value `cluster=unknown` renders `cluster=""` (PromQL's
  empty-string matcher matches an absent label), preserving the
  "series missing the cluster label are bucketed as `unknown`" contract.
- **`prune` parameter, default `true`.** `prune=false` turns the default
  connectivity prune off: every loaded pod is emitted with its `pod-to-node`,
  `pod-mounts-pvc`, `pvc-to-netapp-aggr` chain regardless of traffic. It also
  lifts the cluster-scoped infra "retained iff referenced" rule **where no
  filter could have excluded the node by its own labels** — a K8s node when no
  `namespace` filter is active, a NetApp aggregate/node when neither `cluster`
  nor `namespace` is — so `?prune=false` alone is the full inventory
  (podless nodes, PVC-less aggregates included) while
  `?namespace=x&prune=false` stays the namespace's storage topology. Values
  other than `true` / `false` are a 400. This replaces the `?name=` /
  `?root=` escape hatches (removed below) as the only way to surface
  disconnected elements.
- **Out-of-scope endpoints become `external` — one uniform rule for filtered
  builds.** With the service graph read in full and the topology narrowed,
  every resolution ladder meets peers whose pod was not loaded. In a build
  with **any** selector-level filter active: (1) a series is admitted only if
  **at least one** endpoint resolves to loaded topology — a real pod, or a
  Service present in the loaded index; external↔external series are dropped
  before anything is materialised (this bounds the output to the in-scope
  workload's direct neighbourhood and keeps the out-of-scope estate from
  rendering as an external-to-external web); (2) the other endpoint, when it
  does not map, is `external/<raw label>` with `labels={}` — a UID that is
  not loaded falls to its `client` / `server` human label exactly like the
  existing missing-UID fallback, a `"://"` string whose Service is not loaded
  keeps the verbatim label, a peer IP that matches nothing keeps the IP; (3)
  **no synthesised pod is ever created in a filtered build**; (4) Service
  nodes and `service-selects-pod` fan-out are materialised only for admitted
  series (resolve → admit → materialise). Same-named out-of-scope peers
  collapse into one external node (`external/cart`), and an out-of-scope
  caller of an in-scope pod or Service appears as an inbound external. Edges
  touching an external carry no `labels.cluster` and no `metrics`, as today.

### Changed

- **Filter taxonomy rewritten.** Selector-level (vary the upstream queries):
  `az`, `env`, `cluster`, `namespace`. Projection-level (over the built
  graph): `cluster` and `namespace` again as defence in depth, `edge_type`,
  `prune`. A future cache is keyed by `(window, az, env, cluster-set,
  namespace-set)`.
- **Cross-cluster edge representation.** A cross-cluster edge renders with
  both real endpoints only when **both** clusters are loaded (unfiltered, or
  both listed in `cluster`). Under a `cluster` filter the out-of-scope partner
  is the `external/<label>` node above, and the `service-selects-pod` family
  fan-out reaches only loaded clusters. The former "partner preserved
  regardless of the cluster filter" rule is withdrawn.
- **Cross-namespace calls under `?namespace=`** render the out-of-namespace
  Service or pod as `external/<label>` (the Service legs are
  namespace-filtered too), where today the edge was silently dropped — the
  namespace's outbound and inbound dependencies become visible as externals.
- **Empty filtered result is `200`, not `outside_retention`.** The
  zero-pods + zero-nodes + healthy-`up{}` classification runs only for an
  unfiltered build; with a selector-level filter active, zero rows is an empty
  `elements` array and an empty `clusters` list.
- **`clusters[]`** still derives from the built graph's node `cluster` labels
  and now reflects the filtered build.
- **Self-metric gauges** describe the last (possibly filtered) build. Metric
  names/labels unchanged; the `query` label stays the bare constant.
- **Unfiltered builds are byte-identical to today** except for the removed
  parameters: the admission/external rules, the retention carve-out and the
  prune lift are all gated on a filter or on `prune=false`.

### Removed — **BREAKING**

- **`GET /v1/clusters`** and its sole supports: handler + DTOs, the
  `cluster_discovery` query and its one-hour lookback, OpenAPI operation,
  README entries. `--api-timeout` / `KSG_API_TIMEOUT` survives (it bounds
  `/readyz` and the retention probe); the `clusters` field of the `/v1/graph`
  body is unaffected.
- **The `name` filter.** With it go the name-anchored partner re-add case and
  the `?name=<node|aggr>` infra exception; disconnected elements are reached
  with `prune=false` instead.
- **Traversal: `root`, `depth`, `direction`**, the BFS, `MaxTraversalDepth`,
  and the root-anchored prune exception. An unknown parameter is ignored as
  before (no 400), so an old client sending them gets the unanchored view.
- The remaining request surface is `start`, `end`, `cluster`, `namespace`,
  `az`, `env`, `edge_type`, `prune` — all but `start` / `end` optional.

### Retained

- `kubegraph.ParseValues` stays the single request parser shared by the HTTP
  handler and the facade.
- Determinism: every rendered selector is a pure function of the sorted,
  de-duplicated values; the admission rule is a pure function of the series
  set and the loaded topology.
- The fixed request-invariant selectors (`D30` sentinel, `edge_relation!=
  "link"`, `condition="Ready"`, the node-address `type` alternation, `lun=""`)
  are composed **with** the new matchers, never replaced.
- Synthesised pods keep their current role in **unfiltered** builds (a UID the
  topology has not caught up with).

## Capabilities

### New Capabilities

_None — every requirement lands in an existing capability._

### Modified Capabilities

- `graph-api`: the new parameters (`az`, `env`, `prune`), the selector /
  projection taxonomy, the empty-filtered-result status, cross-cluster and
  cross-namespace representation; removal of the cluster discovery endpoint,
  the `name` filter, and partial-graph traversal; the prune and infra-retention
  requirements rewritten around `prune`.
- `cluster-topology-source`: the upstream-selector contract (which matcher
  reaches which series, the Harvest and traces exemptions, the `unknown` →
  empty-matcher mapping, composition with fixed selectors), the configurable
  label keys, the retention carve-out; removal of the cluster discovery query.
- `pod-service-graph`: the service-graph selectors carry **no** request
  matcher; the filtered-build admission rule, the no-synth-pod rule, the
  out-of-scope-to-external rule, two-phase materialisation, and the rewritten
  cross-cluster partner requirement.
- `netapp-storage-graph` (introduced by `replace-storageclass-with-netapp-nodes`;
  this delta is written against its promoted spec, so this change lands
  **after** that one archives): Harvest legs carry `az` / `env` only and are
  narrowed by reference; kubelet legs carry all four; aggregate / node
  admission under `prune=false`.
- `container-integration`: fixtures stamp `az` / `env` / `cluster` on every
  topology family; `/v1/clusters`, name-filter and traversal scenarios are
  replaced by filtered-request scenarios (selector hit / miss, reference
  narrowing, out-of-scope externals, `prune=false`).

## Impact

**Code.** `pkg/promql` (`Render` takes a selector value; matcher builder with
validation / quoting; per-query matcher table; `cluster_discovery` removed);
`pkg/build` (`Build` / `ReadTopology` / `ReadServiceGraph` take the selector;
admission + two-phase materialisation in the service-graph resolver; retention
check gated on "unfiltered"; `Options` gains the label keys); `pkg/graph`
(`Scope` loses `Names` / `Root` / `Depth` / `Direction`, gains `Prune`;
`traverse` deleted; `Project` / `filterNodes` / `readdEdgePartners`
simplified; property tests' traversal invariants dropped); `pkg/kubegraph`
(`ParseValues`, `Engine.Build`, `Options`); `internal/config` + `cmd/`
(`KSG_AZ_LABEL` / `KSG_ENV_LABEL`, flags, swag annotations); `internal/api`
(request struct, 400 reasons, `/v1/clusters` deleted); `docs/swagger.*`
regenerated; golden `name-filter-cytoscape.json` removed, filtered-request
goldens added.

**API.** `/v1/graph` gains `az`, `env`, `prune`; loses `name`, `root`,
`depth`, `direction`. `/v1/clusters` is removed. No new node type, edge type,
attribute, or `labels` key. All three removals are v1-surface changes that
need a release note.

**Configuration.** Two new optional settings with safe defaults. Which series
receive which matcher is a hardcoded contract.

**Consumers.** No downstream module is coordinated with this change (the
former `graph-api-gateway` dependency is dropped). For any in-process embedder
of `pkg/kubegraph`, `ParseValues` and `Engine.Build` change signature while
`BuildFromValues` does not; `/v1/clusters`, `name`, and traversal are gone.

**Upstream dependencies.** A precondition, not a dependency: every
**topology** family (KSM, kubelet, Harvest) must stamp `az` and `env` under
the configured keys, and KSM / kubelet must carry `cluster`. A family lacking
a label vanishes from every filtered request. The service-graph family is
exempt by design. Cost floor per request is now the full service-graph read
(the raw `server_seconds_bucket` leg in particular); the saving is on the
topology side, which is where pod-count scales.

**Sequencing.** Depends on `replace-storageclass-with-netapp-nodes` (its
`netapp-storage-graph` spec, the `pvc-to-netapp-aggr` edge, and the Harvest /
kubelet legs are what the push-down narrows by reference, and its deltas touch
the same `graph-api` requirements). Write this change's deltas only after that
change is archived.
