# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project purpose

`kube-state-graph` is a Go HTTP API that returns a unified pod / node / PVC graph
for **one or more Kubernetes clusters** read from a single centralised
VictoriaMetrics. Edges between pods come from `traces_service_graph_*` metrics
and may cross cluster boundaries.

The repo ships **only the API server**. `kube-state-metrics`, the service-graph
producer (Beyla / Alloy / Tempo or any compatible exporter), and VictoriaMetrics
are external dependencies. Topology comes from **kube-state-metrics** `kube_*`
series; service-graph edges come from `traces_service_graph_request_total`
(carrying `client_k8s_pod_uid` + `server_k8s_pod_uid`) — both read from the
centralised VictoriaMetrics. Multi-cluster, cross-cluster, and service-graph
code paths are exercised by the integration tests in `internal/integration/`
via the testcontainers-go VictoriaMetrics container, which ingests hand-crafted
fixture series through `POST /api/v1/import/prometheus`.

## Common commands

```bash
# First-time dev env bootstrap (run once after clone). Downloads modules and
# installs host-level dev tools (golangci-lint, govulncheck). Mockery is
# tracked via go.mod `tool` directive (Go 1.24+) and invoked through
# `go tool mockery` — no separate install step.
make init                                   # one-shot: init-go + init-tools
make doctor                                 # report toolchain versions / missing pieces
make init-hooks                             # optional: pre-commit gofmt+lint+quick-test, pre-push CI mirror

# Build / test loop
make build                                  # ./bin/kube-state-graph
make test                                   # go test ./... -count=1 -race -shuffle=on
make vet                                    # go vet
make lint                                   # golangci-lint (installed by `make init-tools`)
make vuln                                   # govulncheck
make cover                                  # go test ./... -coverprofile=coverage.out

# Mocks (regenerate after editing an interface listed in .mockery.yaml).
# Mocks are committed under internal/<pkg>/mocks/ so CI does not need
# mockery installed; the `mocks-drift` CI job verifies freshness.
make mocks                                  # go tool mockery
make verify-mocks                           # CI-style freshness check (regen + git diff)

# OpenAPI docs. Regenerate after editing @-annotations (cmd/.../main.go general
# info; internal/api/*.go operations) or handler signatures, then commit docs/.
# swag writes docs/swagger.{json,yaml}; docs/embed.go compiles them into the
# binary (served at /openapi.{json,yaml}). The /docs Scalar UI is CDN-loaded.
make docs                                   # go tool swag init --outputTypes json,yaml -> docs/
make check-docs                             # CI docs-drift mirror (regen + git diff docs/)

# Single test
go test ./pkg/graph/ -run TestProject_ClusterFilter -v
go test ./internal/api/ -run TestGolden -v

# Update golden files (after changing serialiser shape on purpose)
go test ./internal/api/ -update -run Golden

# Run binary directly
./bin/kube-state-graph --prom-url=http://localhost:8428 --listen-addr=:8080
```

Module path: `github.com/akira-core/kube-state-graph`. Minimum Go 1.25 (`go.mod`); build toolchain pinned to `go1.26.5` via the `toolchain` directive.

## Architecture (the 90 % you need to know)

### Request lifecycle

```
HTTP /v1/graph?start=&end=&...
   │
   ▼
parseGraphRequest        ── validates start/end (RFC 3339 or Unix seconds); only `end > start` is enforced
   │
   ▼
context.WithTimeout(ctx, --build-timeout)   ── graph endpoints only; deadline exceeded → 504 timeout
   └─ Builder.Build(ctx, window, end)
         ├─ ReadTopology  (errgroup of 18 PromQL queries in parallel: KSM topology incl. node ready_status + 3 D29 service/endpointslice + 2 D34 owner + PVC-info + container-info + 2 NetApp Trident)
         ├─ ReadServiceGraph (1 PromQL, `user`/`unknown` peers excluded at selector — D30; joined with topology)
         └─ assemble + graph.NewGraph → *Graph (immutable, with adjacency)
   (no in-process concurrency cap; HPA + Pod resource limits handle load shedding)
   ▼
graph.Project(g, scope)            ── filters + traversal applied here, NOT during build
   ▼
serialiseCytoscape
```

v1 has **no in-process result cache** and **no singleflight**. Each request runs a fresh upstream fan-out and recomputes the body. A future iteration is expected to add a horizontally scalable cache mechanism for distributed deployment (Redis L2, background materialiser, or graph DB) — tracked as a separate change.

### Load-bearing design rules

These are non-obvious; read the archived design doc
`openspec/changes/archive/2026-06-06-add-k8s-pod-graph-api/design.md`
(D1–D34) before changing any of them. The capability specs it produced now
live under `openspec/specs/`.

- **No server-side result cache.** Each `/v1/graph` request runs a fresh upstream PromQL fan-out. Filters (`cluster`, `namespace`, `edge_type`, `name`, traversal) are applied at response time as a projection over the freshly built `*Graph`. A horizontally scalable cache mechanism for distributed deployment is anticipated but out of scope for v1.
- **Default projection is the connectivity-connected subgraph.** Every `/v1/graph` response carries only the workload that sits on a **connectivity edge** (`pod-calls-pod` / `pod-calls-service` / `service-selects-pod`) plus the infra that hangs off it. Concretely: a pod is kept iff it is an endpoint of a connectivity edge; an edgeless pod is dropped, and with it (via the generalised D6 reference rule) the node hosting only edgeless pods, the PVC mounted only by edgeless pods, and the StorageClass backing only such PVCs. **An unmounted PVC (no `pod-mounts-pvc` binding at all) is therefore dropped too** — a PVC is kept iff a connectivity-connected pod mounts it. **Service nodes are unaffected** — they are only ever materialised by the D29 connection-string resolver, so they are connectivity-born by construction (topology `kube_service_info` is index-only, never emitted as a node). The decision set is `graph.connectivityExcluded(g)` — a **pure function of the built graph** (scope-independent: a PVC's keep/drop depends on whether its mounting pod is *connected*, not on the request's cluster/namespace filter, and the two co-move because `pod-mounts-pvc` is intra-cluster/same-namespace), computed once in `graph.Project` and consulted in **both** `filterNodes` (skip excluded ids) **and** `filterEdges`/`readdEdgePartners` (an excluded pod/PVC is never resurrected as an edge partner — e.g. the pruned pod of a `pod-to-node` edge whose host node survived via another pod). The prune is **suppressed under two on-demand escape hatches** (symmetric with the D6 infra-node name/root exception): an explicit `?name=<pod|pvc>` surfaces a specific edgeless element, and a `?root=`-anchored traversal returns its reachable set verbatim (`excluded` is nil whenever `scope.Root != ""` or `NameFilterActive()`). `?cluster=` / `?namespace=` filters do **not** disable the prune. **Consequence:** the default/no-filter view is the traffic graph, not the full inventory — an edgeless pod, an unmounted PVC, or a podless node is fetched on demand with `?name=`. The **full-topology `*Graph` is still built** (the build still loads every cluster's every pod/node/pvc — see "No filters pushed to PromQL"); the prune is a **projection concern**, scope-independent, so a future cache serving any filter from one built graph is unaffected.
- **No time-window alignment, no window cap, no future-time guard.** `start` and `end` are passed through to upstream PromQL verbatim; only `end > start` is enforced. The previous 60 s `floor`/`ceil` grid was removed alongside the in-process cache it was bucketing for. Bounded query cost is delegated to upstream VictoriaMetrics search limits (`-search.maxQueryDuration`, `-search.maxPointsPerTimeseries`, `-search.maxSamplesPerQuery`). Response body is `{apiVersion, clusters, elements}` — no time fields are echoed.
- **`labels` is strict `map[string]string`** on both nodes and edges. No bools,
  no numbers, no string-encoded numbers. Numeric edge metrics (`rate`, `p99_ms`,
  `error_rate`) and boolean flags (`cross_cluster`, `ghost`) are **deferred to a
  future typed struct field**. `pod-calls-pod` and `pod-calls-service` edges
  carry a single `labels.cluster` (the trace source / client-side cluster,
  omitted when the client side is non-pod). Cross-cluster status is derived by
  comparing the resolved source-node and target-node `labels.cluster` — D9.
- **Edge IDs are UUIDv5** with a fixed compiled-in namespace (`graph.edgeNamespace`)
  and the canonical input `<type>|<source>|<target>`. Stable across rebuilds —
  required for golden tests. Bumping the namespace UUID is a v2 break.
- **Cluster-scoped IDs everywhere.** Pods: `<cluster>/<uid>`, K8s nodes:
  `<cluster>/<node>`, PVCs: `<cluster>/<namespace>/<claim>`, externals:
  `external/<value>`. Node names are not globally unique without the prefix.
- **Connection-string resolution rule** (D29, hardcoded — no knob): for any
  service-graph endpoint whose pod UID is empty, the verbatim `client`/`server`
  label is checked for a `"://"` connection string. Detection is hardcoded —
  there is no operator-tunable substring and no config knob. Per-endpoint
  independent (both sides of a single edge are evaluated separately); edge `type`
  is `pod-calls-service` when the target resolves to a service node, otherwise
  `pod-calls-pod`. When a `"://"` label is found, its URL host is parsed and
  the optional `.svc.<domain>` suffix stripped, then resolved by dotted-label
  count. **Both** in-cluster DNS forms resolve to the **service** — there is no
  per-pod resolution; a `"://"` endpoint is never a pod:
  - **2 labels** `<service>.<namespace>` and **3 labels**
    `<pod>.<service>.<namespace>` (headless per-pod) both → the addressed
    `(namespace, service)`, resolved to a **SINGLE `type="service"` node in the
    caller's own (anchor) cluster** (pod→svc is same-cluster only). The anchor
    is the **UID-recovered client-pod cluster** when the client side resolved to
    a topology pod (the trace `cluster` label is frequently missing or wrong),
    falling back to the raw trace label otherwise; edge `labels.cluster` always
    stays the raw trace label (D9). The endpoint resolves **iff the anchor
    cluster itself holds the `(namespace, service)`** in `ServicesByNameNS` — a
    same-named local Service is a service-mesh precondition (Istio multi-primary
    / Cilium Cluster Mesh keep the Service in *every* cluster; cross-cluster is
    endpoint aggregation), so a family sibling holding it is **not** enough.
    This single anchor-membership test uniformly covers an anchor whose own
    cluster lacks the Service, an `"unknown"`/empty/bogus anchor, **and** the
    fully-unlabelled single-cluster case — `ClusterFamilyKey("unknown") =
    "unknown"` is a family-of-one, so an `"unknown"`-bucketed Service makes
    `"unknown"` a legitimate holder. There is **NO unknown-family fallback and
    NO cross-family resolution**. The anchor materialises **one** node
    (`id="<anchor>/<namespace>/<service>"`, `labels={cluster,namespace}`,
    `ipaddress=[cluster_ip]` from the anchor's own `kube_service_info` unless
    headless `cluster_ip="None"`) and yields **one** `pod-calls-service` edge
    (this D29 path is always intra-cluster by construction; the TYPE is
    registered `may_cross_cluster: true` only because the route-engine path
    below can anchor on a sibling cluster). **Cross-cluster
    `service-selects-pod` fan-out**: from that single node, one edge is emitted
    per backing pod across the **UNION of `EndpointsByService` over every
    same-family cluster holding the same-named Service** — two clusters are in
    one family iff their names are equal after replacing every maximal digit run
    with a single `0` sentinel (`prod-03` ↔ `prod-12` match; `staging-1` ≠
    `prod-1`; digit-free names form exact-name singleton families; the sentinel
    being a digit makes the mapping collision-free without escaping). These
    `service-selects-pod` edges **MAY cross clusters** (**`may_cross_cluster:
    true`**) — a local service node selecting a backing pod in a family sibling,
    reflecting service-mesh endpoint aggregation (each cluster's KSM observes
    only its OWN EndpointSlices, so the cross-cluster endpoint set is rebuilt by
    unioning over the family). There is **no endpoint-backed pruning**: a
    sibling holding the Service with zero endpoints contributes no edge, and a
    service with zero endpoints anywhere still materialises its single (local)
    node — an operator signal. Candidates are iterated in sorted order, the
    anchor-membership test and the endpoint union are order-free, and
    `service-selects-pod` edges dedupe by `(service-node, pod)` (determinism).
    The family rule (`build.ClusterFamilyKey`, exported so pkg/route's
    ingress-cluster pick shares it) and the membership/union logic
    are hardcoded pure functions — no knob, no PromQL change (filtering is
    in-memory at resolution, preserving "no filters pushed to PromQL"). The
    3-label form drops the leading pod-hostname and resolves as its parent
    service. When BOTH sides of a series are `"://"` labels, each resolves to a
    single local node in the (shared) anchor cluster, so one intra-cluster edge
    is emitted between them.
  - **unresolvable** (host not a 2/3-label `.svc` name, or the anchor cluster
    does not itself hold the service in its own family) → an `external` node
    (`id="external/<label>"`, `labels={}`) with the verbatim label as `name`.
  - A series with a **wholly empty side** (no UID, no label) is dropped before
    any resolution — the other side's `"://"` label must not leak service /
    external nodes or fan-out edges as an orphan subgraph.
  A client-side `"://"` label resolves to `service` or `external` (never a pod),
  so the edge `labels.cluster` is always omitted for it.
- **Missing pod-UID human-label fallback** (D27, always on): when
  `client_k8s_pod_uid` or `server_k8s_pod_uid` is empty AND the corresponding
  `client`/`server` label is non-empty AND the label does NOT contain `"://"`,
  that endpoint is promoted to `external/<label>` (no cluster prefix; `labels={}`)
  instead of dropping the edge. Per-endpoint resolution order:
  (1) connection-string resolution (`"://"` in the label, empty UID) →
  a single `service` node in the caller's own (anchor) cluster (iff that
  cluster holds the service), with a cross-cluster `service-selects-pod`
  endpoint union over the same-family clusters holding it, or `external` when
  the anchor cluster lacks the service, per the D29 same-cluster rule above
  (never a pod; no unknown-family fallback, no endpoint-backed pruning);
  (2) UID-based pod resolution / synth-pod fallback (only when UID is non-empty);
  (3) missing-UID human-label fallback (this rule) → external with `labels={}`
  (**only for non-`"://"` labels**);
  (4) drop (both UID and label empty). A `"://"` label never reaches this fallback
  — it is resolved (or produces an `external` node) at step (1). Edge
  `labels.cluster` is omitted whenever the client side resolves to a non-pod node,
  whether via the connection-string rule (`service` / `external`) or this fallback
  (`external`).
- **Self-loop UID guard** (D33, always on, no knob): a pre-resolution
  normalisation in `parseServiceGraph`, applied **before** the resolution order
  above. Some `servicegraph` exporters stamp the **caller's own** pod UID onto
  **both** sides for a peer they could only identify as a `"://"` connection
  string, so `client_k8s_pod_uid == server_k8s_pod_uid` (non-empty, equal) while
  the real target lives only in the `"://"` label. A populated UID normally
  short-circuits Stage 0 (step 1 above), so the `"://"` side would collapse onto
  the caller's own pod — a self-loop `pod-calls-pod` edge, **no service node**.
  The guard: when the two UIDs are non-empty AND equal, clear the UID on **any
  side whose label contains `"://"`** (that side only), so it falls through to
  connection-string resolution; the non-`"://"` side keeps the shared UID and
  resolves to its real pod. Fires ONLY on the conjunction (UID collision AND a
  `"://"` label on the cleared side): differing UIDs are untouched (`"://"` with
  a populated UID still takes pod-UID resolution), and a UID collision with no
  `"://"` label stays a legitimate `pod-calls-pod` self-loop. Do NOT broaden this
  into a global "`"://"` always beats UID" reorder — that breaks the
  populated-UID-means-pod contract; the collision is the specific fingerprint of
  the exporter defect. Determinism unaffected (pure function of the two UID + two
  string labels); no new node/edge type. Tests:
  `pkg/build/servicegraph_test.go` (`TestParseServiceGraph_SelfLoopUID_*`) and
  `internal/integration` (`TestConnStringSelfLoopUIDResolvesToServiceNode`).
- **Sentinel-endpoint exclusion at the query layer** (D30, hardcoded — no knob):
  the `servicegraph` connector emits virtual peers for endpoints it cannot pair
  to an instrumented span — an uninstrumented caller as `client="user"`, an
  unresolved peer as `"unknown"`. The service-graph selector drops these
  **upstream** via anchored negative matchers —
  `rate(traces_service_graph_request_total{client!~"user|unknown",server!~"user"}[w])`
  — so a `client="user"`/`"unknown"` series never reaches the resolver: no node
  (`pod` / synth / `service` / `external`) and no edge is produced for it. The
  **server-side matcher is narrower** (`server!~"user"` only —
  resolve-unknown-server-peer-labels D1): a `server="unknown"` series now
  reaches Go, but the reader still drops it (same outward result: no node, no
  edge) **UNLESS** the "Unknown-server peer-label enrichment" rule below
  applies — every `server="unknown"` case outside that rule's narrow trigger
  (client unresolved, or the server UID itself resolves) is **byte-for-byte
  unchanged** from the old blanket exclusion. PromQL `!~` is fully anchored, so
  the match is **exact** and **case-sensitive** (a `http://user/...` connection
  string is NOT excluded — it is not equal to `user`). This is a fixed
  selector contract on the `client` / `server` labels only — it does NOT touch
  the `cluster="unknown"` bucketing (a different label). The matcher fragment
  lives in `promql.serviceGraphSentinelSelector`; the `QServiceGraphTotal`
  constant stays the bare metric name so `query_name` self-metric / span
  dimensions are unchanged. Deferred numeric service-graph metrics MUST reuse
  the same fragment when added.
- **Unknown-server peer-label enrichment** (resolve-unknown-server-peer-labels
  D1–D3, hardcoded — no knob): the one carve-out from the D30 outcome above.
  When `client_k8s_pod_uid` resolves to a **real topology pod** (never a
  synthesised one) AND the server side has no resolvable pod (UID empty, or
  present but absent from `Topology.PodsByUID`) AND the raw `server` label is
  exactly `"unknown"`, `resolveServer` dispatches to the new
  `resolveUnknownServerPeer` instead of the generic empty-UID
  (`resolveEmptyUID`, which owns the D27 fallback) or synth-pod path — never
  both, for this literal value. It reads `client_net_peer_name` (checked
  first) then `client_server_address` (checked second; an optional trailing
  `:<port>` is best-effort stripped via `net.SplitHostPort`) and classifies
  whichever is non-empty first via the same `classifyK8sDNS` grammar D29
  connection-string resolution uses (2-label `<service>.<namespace>`, 3-label
  headless `<pod>.<service>.<namespace>`, `.svc[.<domain>]` suffix stripped),
  **plus two grammar extensions scoped to this rule only**: (1) a single
  dot-free, non-IP-literal label is treated as a bare short Service name
  resolved in the **client pod's own namespace**; (2) (resolve-unknown-server-ip-peer)
  when neither the DNS grammar nor the bare-short-name form matches AND the
  host is a valid IP literal (`net.ParseIP`), it is looked up as a Service
  `ClusterIP` **within the already-resolved client pod's own (anchor) cluster
  only** — never a family sibling, since a `ClusterIP` is a per-cluster
  address that can legitimately collide across unrelated clusters' Service
  CIDRs (unlike a Service DNS name, which is a mesh-wide convention the
  family union already handles). The reverse index (`(cluster, ClusterIP) →
  Service`) is built once per parse from `topology.ServicesByNameNS`,
  skipping empty/`"None"` ClusterIP; on a same-cluster duplicate `ClusterIP`
  (a data anomaly Kubernetes itself prevents), the lexically-smaller
  `(namespace, service)` wins. Once identified via IP, resolution proceeds
  through the SAME `resolveServiceLevel` call as every other classification
  path below — including its normal family-wide `service-selects-pod`
  fan-out — only the identification lookup itself is anchor-scoped. A
  successful classification resolves via the
  existing `resolveServiceLevel(anchorCluster, ns, svc)` — anchor = the
  already-resolved client pod's own cluster (no anchor-recovery fallback chain
  needed here, unlike D29) — with the same anchor-membership test and
  cross-cluster `service-selects-pod` fan-out. An unresolvable classification,
  or a `resolveServiceLevel` miss, falls back to `external/<raw_peer_address>`
  (the RAW, unstripped value — same convention as every other external
  fallback). Neither label present, or the client did not resolve to a real
  pod, drops the endpoint (no node, no edge) — **identical outward behaviour
  to the pre-change blanket exclusion**. This is the invariant the loosened
  selector must never violate: it must never leak a `external/unknown` node
  via the generic D27 path for a case outside this rule's trigger.
- **Istio route resolution of global FQDN peers**
  (translate-global-fqdn-to-k8s-service, OPT-IN — off by default): the ONE
  step added to the enrichment above. When `--route-store-dsn` /
  `KSG_ROUTE_STORE_DSN` is set, every point where `resolveUnknownServerPeer`
  would emit an external node first consults an Istio route-resolution engine:
  which Kubernetes Service did the **engine-selected ingress cluster's** Gateway
  + VirtualService config route `(host, "/", port)` to **at the END of the
  request's own window** (simplify-route-resolution-to-point-in-time D1 — a
  single as-of instant, never a per-version range; `RouteRequest.At`, the same
  instant the service-graph samples are evaluated at, so exactly ONE
  configuration state is consulted and ONE outcome produced)? A hit resolves
  through the SAME `resolveServiceLevel` as every
  other path — anchored on the **selected ingress cluster** (`dest.Cluster`),
  not the caller's (membership test, one service node, `pod-calls-service`
  edge — which therefore MAY cross clusters, family-wide `service-selects-pod`
  fan-out); any miss/error degrades to the existing external node — route
  resolution can NEVER fail a build. Key facts:
  **(1) The trigger is ALL THREE external branches**, not just "not k8s DNS" —
  `classifyK8sDNS` splits on dots, so a global FQDN like `api.example.com`
  (3 labels) is *successfully* classified (service `example`, namespace `com`)
  and reaches external via the anchor-lacks-service branch; wiring only the
  unclassifiable branch makes the feature a silent no-op for its motivating
  case. **(2) I/O stays out of the parse** (D6): `ReadServiceGraph` runs a pure
  prescan (`collectRouteQueries`, sharing `classifyPeerHost` /
  `lookupClientPod` / `anchorHolds` with the parse so the two cannot drift),
  resolves the deduped keys serially (each bounded by
  `--route-resolve-timeout`), and hands `parseServiceGraphRoutes` a prefetched
  index — nil index ⇒ byte-for-byte pre-change output. **(3) Listener port
  precedence** (D5): the `:<port>` on the peer-address value (now returned by
  `splitPeerAddressPort`, no longer discarded) → the optional
  `client_server_port` / `client_net_peer_port` dimension → default **443**
  (a :443-only Gateway or an httpsRedirect :80 stub is the common ingress
  shape; a wrong port fails as "no listener" — logged distinctly as
  `route_engine_no_listener_on_port` — never as a wrong destination). The
  RouteConfiguration is then selected **host-aware** within the port
  (`translate.ListenerFor`, the single tri-state decision point shared by
  `Translate` and the resolver's listener gate): among the servers on the
  port, the one whose `hosts` most-specifically match the request FQDN
  (`gwresolve.PickHosts` — Istio exact/wildcard semantics, declaration-order
  independent, `<ns>/` binding prefixes stripped) owns the RC, with
  `server.bind` reflected in the name (`http.<port>[.<bind>]` shared by HTTP
  servers; `https.<port>.<portName>.<gw>.<ns>[.<bind>]` per TLS-terminated
  HTTPS server). Servers on the port that serve only OTHER hosts short-circuit
  as `route_engine_no_server_for_host` (`RouteNoServerForHost`, ranked between
  `no_listener_on_port` and `no_route`) without a translate round-trip —
  istiod builds vhosts from the server-hosts ∩ VS-hosts intersection, so such
  a request could only ever reach an empty `RouteNoRoute`.
  **(4) The `client_dns_answers` dimension is REQUIRED** (D6 rev): its IPs
  select the ingress cluster and feed the ClickHouse IP 3-hop; no parseable IP
  ⇒ the engine is NEVER consulted (prescan skip, no store read, distinct
  `route_engine_no_ip` reason) — config_only mode and `LoadConfigWindow` were
  removed. **(4b) Ingress-cluster selection** (D10, `pickIngressCluster` — a
  pure function in `pkg/route`): per IP, the store probe
  `ClustersWithIngressIP` (the store's ONLY cross-cluster read) yields the
  candidate clusters G; F = G ∩ caller's family (`build.ClusterFamilyKey`,
  exported). |F|==1 → it; |F|>1 → caller if caller∈F else ambiguous; F empty
  and |G|==1 → it; |G|>1 → caller if caller∈G else ambiguous; G empty →
  no-ingress. Multi-IP selections must all agree or degrade ambiguous;
  candidate sets / snapshots are NEVER unioned across clusters. Misses surface as
  `route_engine_no_ingress` / `route_engine_ambiguous_ingress_cluster`;
  `RouteRequest.CallerCluster` feeds ONLY the family key + tie-break, and
  `RouteDestination.Cluster` carries the locked cluster the parse anchors on.
  The `ClustersWithIngressIP` probe is a pure function of `(ip, at)` (both
  constant across a build's keys), so it is **memoised per build** (D13): when
  the resolver implements the optional `build.BuildScopedRouteResolver` upgrade
  (`RouteResolver` + `BuildScoped() RouteResolver`), `resolveRouteQueries`
  drives the whole build through one `scopedResolver` scope that caches the
  probe by `(ip, at)`, collapsing keys that share a destination IP to a
  single store read. The scope is one-build/serial (no mutex); the shared
  `*Resolver` stays stateless (an instance cache would leak). Errors are not
  cached; no outcome/determinism change.
  **(5) The engine** (`pkg/route`) loads an **ingress-cluster-scoped,
  read-only, as-of** ClickHouse snapshot (`store.LoadTrafficAt` →
  `store.TrafficSnapshot`, resolved in memory by `pkg/route/snapshot`; the
  tables stay interval-versioned and are written by the metadata-exporter repo;
  schema drift fails fast at startup; reads use the no-FINAL pattern —
  `valid_to` NEVER filtered in SQL, SQL carries only `valid_from <= at` plus the
  join keys, client-side dedup per version slot by max ingest_seq, and the
  liveness test `valid_from <= at < valid_to` applied post-dedup — because the
  exporter closes a version by REWRITING the open row; `--route-store-unique-rows`
  opts into SQL-side pruning for update-close writers ONLY; time operands are
  `dt64Lit` literals, never `?` binds; `spec_json` parses with `DiscardUnknown`;
  bare `spec.gateways` names bind same-namespace gateways — see design
  "production reader compatibility"),
  translates that one gateway's scoped config via
  in-process istiod (`ConfigGenerator`, no istiod pod, no Kubernetes client —
  see the client-go rule) and matches with the native `router_check_tool`
  binary (`--router-check-bin`; copied into the image from the Envoy tools
  image; ~50–60 ms per config — one translate + one check per resolution, so
  there is no segment loop and no config-signature cache). **Hop 3 is
  namespace-scoped** (scope-gateway-candidates-to-ingress-namespace): a
  candidate Gateway must live in the ingress Service's OWN namespace —
  enforced in both the gw_versions SQL (`has(?, namespace)` on the hop-1
  nsList, like the deploy hop) and the in-memory hop
  (`r.Namespace == svcNS`) — so within a resolution the candidate set can
  never hold two same-named Gateways (K8s per-ns name uniqueness) and the
  bare-name gateway identity through `gwresolve`/`ScopedFor` is unambiguous
  by construction. Istio's cross-namespace selector attachment is
  deliberately out of scope (degrades `no_gateway` → LB fallback/external;
  extension path: thread `(namespace, name)` end-to-end). Two
  different-named candidates declaring an identical equal-specificity host
  pattern resolve to the **lexically-smallest gateway name**
  (`gwresolve.sortPats` tie-break; `PickHosts`' numeric-index semantics
  unchanged), never to storage row order.
  **(5b) Ingress LB Service fallback** (ingress-lb-service-fallback change):
  when the pipeline produces no hit AND its miss is exactly
  `RouteNoGateway` (resolution never got past gateway selection — the nginx
  signature: Hop 3 finds no Istio Gateway CR; a DEEPER miss keeps its
  diagnostic reason unmasked), the resolver falls back to an **as-of identity
  dedup** over the already-loaded rows
  (`snapshot.ResolveIPToIngressServices` — the in-memory, single-cluster
  analogue of the `ClustersWithIngressIP` SQL; no new store read): per
  destination IP the distinct `(namespace, name)` of every ingress-IP-carrying
  Service row LIVE AT the instant, merged order-free — any IP with >1
  simultaneous identity → `RouteAmbiguousIngressService`
  (`route_engine_ambiguous_ingress_service` → external, no lexicographic
  tie-break); any IP with 0 → keep the pipeline miss byte-for-byte; else all
  singletons must agree → `RouteIngressLBService`, resolved by
  `routeIndexResolve` via `resolveServiceLevelInCluster` — the same node
  materialisation as `resolveServiceLevel` but with a **locked-cluster
  `service-selects-pod` fan-out** (the selected cluster's own endpoints ONLY,
  no family union — an LB IP is a per-cluster address, so a family sibling's
  same-named Service is not behind it; route-hit-ingress-chain D2)
  (dest.Cluster = the locked ingress cluster, topology miss →
  `route_engine_dest_cluster_lacks_service`), with the outcome dimension in
  the success debug log distinguishing the coarser "LB entry point" semantics
  (host/path/port play no part — the fan-out reaches the ingress controller
  pods, e.g. nginx, never a routed backend). An identity that was superseded
  BEFORE the instant is simply not a candidate (no longer ambiguous —
  simplify-route-resolution-to-point-in-time D5).
  **(5c) RouteHit ingress chain** (route-hit-ingress-chain): on every routed
  hit the resolver ALSO recovers the ingress LB Service identity of the
  destination IPs via the same as-of dedup (shared core
  `ingressServiceIdentity` in `pkg/route/ingresslb.go`; zero new store
  reads) into two new `RouteDestination` fields `IngressNamespace` /
  `IngressService` — empty on ambiguous/incomplete identity, which NEVER
  demotes the hit (the LB fallback mirrors its own identity into them for
  uniformity). When populated AND every chain precondition holds, the parse
  emits the **full chain in addition to the direct edge**: caller pod
  -[pod-calls-service]→ ingress service (locked-cluster
  `service-selects-pod` fan-out to the gateway pods) plus ONE synthesized
  **`pod-calls-service`** edge per locked-cluster ingress pod → the
  backend service (which keeps its family-wide fan-out); the direct
  caller→backend edge is KEPT (`routeIndexResolve` returns `[ingress,
  backend]` as the endpoint's resolution targets — the chain alone would
  funnel every caller through the shared ingress node and erase the
  per-caller → backend dependency), and it collapses with any
  trace-derived edge for the same `(caller, backend)` pair via the traced
  pairs map (identical UUIDv5 edge ID — no duplicate possible). **Ingress
  role marker** (mark-ingress-route-path): the ingress entry-point node
  stays `type="service"` (no new node type — `materializeServiceNode` is
  idempotent by id, so a path-dependent type would be arrival-order
  dependent) but its `labels` carry `role` — `ingress-gateway` for the
  RouteHit chain's entry hop, `ingress-lb` for the
  `RouteIngressLBService` (nginx) fallback destination (no routed backend
  behind it). Assignment (`sgResolver.markIngressService`) is set-only and
  MONOTONE: `ingress-gateway` always overwrites, `ingress-lb` writes only
  into an unset value — one Service can be reached by both paths in one
  build, and the marker must not depend on series arrival order (D6). The
  key is absent (never empty-string) on every non-ingress service node; a
  degrade materialises no ingress node and therefore no marker. Marking
  happens strictly AFTER successful materialisation at the two call sites
  owning the outcomes (`resolveRouteChain`, `routeIndexResolve`'s LB
  branch). Preconditions — identity present,
  identity ≠ backend identity, locked cluster holds the ingress Service in
  topology, non-empty locked-cluster endpoint set — are checked **purely
  before any materialisation** (`resolveRouteChain` in
  `pkg/build/servicegraph.go`), so every degrade falls back to today's
  direct-edge shape with zero stray nodes/edges, logged at Debug only
  (`route_chain_degraded`, never counted in the external-fallback reasons —
  no external node is produced). A backend topology miss stays the existing
  `route_engine_dest_cluster_lacks_service` external path with the ingress
  never materialised (backend resolves first). Synthesized edges carry
  `labels={"cluster": <ingress cluster>}` (the client side is a pod in that
  cluster — D9), accumulate in `sgResolver.routeChainEdges`, and dedupe
  **traced-edge-wins** against the parse's `(src, tgt)` pairs (a
  trace-derived `pod-calls-service` edge for the pair is emitted and the
  synthesized hop skipped). No new engine outcome or PromQL change.
  **(6) Containment** (D1, dependency hygiene, distinct from the client-go
  rule): `pkg/build` declares only the `RouteResolver` interface and MUST NOT
  import `pkg/route`; only `cmd/` (or an opting-in embedder) links the engine,
  so `graph-api-gateway` never inherits istio/ClickHouse —
  `make check-route-containment` enforces this in CI. No new node type, edge
  type, attribute, or `labels` key; the destination's port/subset are parsed
  and discarded. Tests: `pkg/build/routeprescan_test.go`,
  `pkg/route/*_test.go`, `internal/integration/route_e2e_test.go`
  (`TestRouteSuite` needs `router_check_tool` — set `KSG_ROUTER_CHECK_BIN`;
  `TestRouteStoreSuite` needs only Docker), and the `-tags oracle` sweep.
- **Server-side pod resolution** uses `Topology.PodsByUID` — a global pod-UID
  index built from all loaded clusters. Service-graph metrics carry only the
  trace-source `cluster` (client side); the server side's cluster is recovered
  by looking up `server_k8s_pod_uid` against this index, since K8s pod UIDs
  are unique cross-cluster in practice. Missing UIDs (with non-empty server
  label) follow the missing-UID fallback above; UIDs present but unknown
  to topology become synth pods with `cluster=""` (server-side cluster
  unknown).
- **No filters pushed to PromQL.** Each build loads every cluster present in upstream VictoriaMetrics. Caller-supplied filters (`cluster`, `namespace`, `edge_type`, `name`, traversal) are applied at projection time over the freshly built `*Graph`. Bounded query cost is delegated to upstream VictoriaMetrics search limits. The one fixed exception is the D30 sentinel matcher (`client!~"user|unknown",server!~"user"` — the server side narrowed by resolve-unknown-server-peer-labels D1) on the service-graph selector — it is a **request-invariant metric-selection contract**, not a caller filter, so it never varies per request and does not break the projection-over-graph contract a future cache relies on.
- **`/v1/edge-types` reads from `graph.EdgeTypes` only** — a single in-code
  registry shared with the builder. Adding an edge type = update both the
  builder and the registry in the same change; the API can never list a type
  the builder cannot produce. Current edge types include `pod-calls-pod`,
  `pod-calls-service` (emitted when a `"://"` connection-string resolves to a
  service node in the caller's OWN cluster — that path stays intra-cluster —
  OR when the route engine resolves a global FQDN to a Service in the
  engine-selected ingress cluster, which may be a family sibling, so the type
  is `may_cross_cluster: true`; also used for the synthesized RouteHit
  ingress-chain hop from gateway pod → backend service), and
  `service-selects-pod` (directed service →
  pod, emitted on demand by the D29 connection-string resolution; the local
  service node fans out across same-family clusters holding the same-named
  Service, so it MAY be cross-cluster — `may_cross_cluster: true`).
- **API-key auth is the only HTTP auth in v1.** Header is `X-API-Key`. Keys
  come from `--api-keys-file` (K8s `Secret` mount, hot-reloaded) or
  `--api-keys`. Empty keyset = auth disabled (dev default). Open paths
  (no key required): `/livez`, `/readyz`, `/metrics`, `/openapi.*`, `/docs`.
  The Scalar UI at `/docs` is a tiny HTML page that loads the Scalar bundle
  from the jsDelivr CDN and renders the same-origin `/openapi.json`; the spec
  itself is generated by `swag` into `docs/` and embedded via `docs/embed.go`.
  Validation is constant-time and iterates the whole set —
  do NOT add early-return optimisations to `auth.KeySet.Validate`. Logs must
  never include the presented key value.
- **Deterministic response body.** The serialiser produces byte-identical output for the same `(window, filters, upstream-data)`: node/edge slices MUST go through `graph.SortNodes`/`SortEdges`, `Graph.ClusterNames()` MUST sort, and the response body MUST NOT carry time-of-build or echo-of-input fields. Body shape is fixed at `{apiVersion, clusters, elements}`. Don't add timestamps, random IDs, or unsorted map iteration to the response — golden tests will break.
- **IP addresses live on the typed `ipaddress` attribute, never in `labels`.** `PodNode.IPAddress()` carries `[pod_ip]` from `kube_pod_info` (when present). `K8sNode.IPAddress()` carries `[external_ip]` from `kube_node_status_addresses{type="ExternalIP"}` when present, falling back to `[internal_ip]` from `kube_node_status_addresses{type="InternalIP"}` when the node has no ExternalIP row (ExternalIP always wins over InternalIP regardless of upstream sample order; within each type a duplicate `(cluster, node)` sample resolves to the lexically-smallest address; address types other than `ExternalIP`/`InternalIP` are ignored); omitted only when neither type is present. The selector is the anchored alternation `kube_node_status_addresses{type=~"ExternalIP|InternalIP"}` — a fixed, request-invariant metric-selection contract, not a caller filter. `ServiceNode.IPAddress()` carries `[cluster_ip]` from `kube_service_info` (when present, omitted for headless `cluster_ip="None"`). `PVCNode` and `ExternalNode` always return nil. `host_ip` from `kube_pod_info` is intentionally dropped — it is the node's IP, surfaced via the node entry instead. The serialiser emits `data.ipaddress` (with `omitempty`); `labels.pod_ip`, `labels.host_ip`, `labels.external_ip`, `labels.internal_ip`, and `labels.cluster_ip` MUST NOT appear.
- **Cytoscape compound nodes are presentation-only — workload hierarchy (supersedes D31).** The change `add-storageclass-and-argo-application-nodes` replaced the old `cluster > node > pod` / `cluster > storageclass > pvc` nesting. `pkg/cytoscape` now synthesises a `type="cluster"` group per cluster plus `type="namespace"` / `type="application"` / `type="controller"` groups (all `labels={}`, no `ipaddress`, emitted in tier order each sorted by id, before real nodes) and sets `data.parent` (`omitempty`) for the hierarchy `cluster > namespace > application > controller > pod` with **skip-absent-levels** (pod → its controller group when it has an `Owner()`, else its application group when it has an `Application()`, else its namespace group), plus `cluster > namespace > [application >] {service, pvc}` (a service/pvc nests under its `application` group when it resolves an `Application()`, else its namespace group — skip-absent) and `cluster > {node, storageclass}`. `external` nodes get no parent. Group ids are **path-encoded** (`<cluster>/namespace/<ns>/application/<app>/controller/<kind>/<name>`) so each pod independently derives its full parent chain — the tree is dangling-free by construction. The synthesised `namespace`/`application`/`controller`/`cluster` groups are serialiser DTOs, not `GraphNode`s, derived from emitted pods' `labels.namespace` / `Application()` / `Owner()` and from service/pvc `labels.namespace` / `Application()` (the `application` group is derived from any pod/service/pvc with a resolved `Application()`; `controller` groups stay pod-only). **`StorageClass` is now a real `GraphNode`** (`NodeTypeStorageClass`, `id="<cluster>/storageclass/<name>"`), sourced from `kube_storageclass_info` (`resolveStorageClassInfo` in `pkg/build/topology.go`): it carries `data.provisioner` (native `provisioner` label) + `data.parameters` (`pool`←`storagePools`\|`pool`, `fs`←`fsType`\|`fsName`, `cluster_id`←`ClusterID`, `selector`) as typed attributes via the new sealed `GraphNode.StorageClassInfo() *StorageClassInfo` method (`labels` stay `{cluster}`); a class referenced by a PVC but absent from the info metric is materialised **bare** (nil info). The pod→node and pvc→storageclass relationships are now **edges** (`pod-to-node`, `pvc-to-storageclass`, intra-cluster, built in `TopologyEdges`), so K8s `node` nodes carry edges again and a `?name=<node>` match pulls its scheduled pods via the `pod-to-node` edge re-add. `PVCNode.StorageClass()` stays a PVC-only typed value (never a label) and drives the `pvc-to-storageclass` edge (lexically-smallest on collision). On **every** request shape (default/no-filter, `?cluster=`, `?namespace=`), the cluster-scoped infra nodes `node` and `storageclass` (neither carries a namespace) are **retained iff referenced by an in-scope element** — a pod scheduled on the node (`labels.node`) or a PVC backed by the StorageClass — so the default view never carries an orphan/empty node or a PVC-less StorageClass (it only lists host nodes of pods that are in the graph). The **one exception** is an explicit `?name=<node|storageclass>`, which surfaces the named infra node even when referenced by nothing (an empty / NotReady node, or an unused StorageClass, stays queryable on demand). This is the generalised `infraNodePassesFilters` deferred-admission rule in `graph.Project`/`filterNodes` (the D6 rule). **Consequence:** a podless node's `ready_status` / `ipaddress` (and a PVC-less StorageClass's attributes) are no longer in the default graph — fetch them with `?name=`. The full-topology `*Graph` is still built (the build loads every node); the pruning is a projection concern, so a future cache serving any filter from one built graph is unaffected.
- **OTLP tracing/logging is config'd by OTel env vars only** (`OTEL_EXPORTER_OTLP_*`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER`). No bespoke `--otlp-*` flags. Telemetry defaults to no-op when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset (zero export overhead, no background goroutines). Tracing MUST NOT alter response bodies — resource attrs and span IDs live on spans, never in JSON. `otelgin` is mounted on `/v1/*` only; `/livez`, `/readyz`, `/metrics`, and `/docs/*` are deliberately untraced. The auth middleware MUST NEVER log or attribute the presented `X-API-Key` value via either the local handler or the OTLP slog bridge.
- **Pod controller-owner attribute (D34).** Each `type="pod"` node carries a typed, nullable `owner` attribute — `data.owner = {kind, name}`, serialised with `omitempty` and **omitted entirely** when the pod has no controller owner (never empty strings). It lives on the typed attribute, **never inside `labels`** (which stay strict typological metadata) — same precedent as `ipaddress`. Surfaced via `graph.GraphNode.Owner() *graph.Owner` (nil for non-pods and ownerless pods). Resolved from `kube_pod_owner` with the **ReplicaSet skipped to its owning Deployment** via `kube_replicaset_owner` (a bare ReplicaSet with no Deployment owner stays `kind="ReplicaSet"`; other owner kinds surface verbatim). Both series are KSM defaults (no `--metric-labels-allowlist`) and OPTIONAL (absence degrades gracefully, no build failure). Resolution lives in `pkg/build/topology.go` (`resolvePodOwners`); the controller pick is deterministic (lexically-smallest `(kind, name)` on collision). No new node/edge type.
- **Pod `application` and `containers` attributes.** Each `type="pod"` node may carry two more typed, nullable attributes, both serialised with `omitempty` and **never inside `labels`** — same precedent as `owner` / `ipaddress`. (1) `data.application` (string) is the pod's ArgoCD Application, read from the `argocd_tracking_id` label on `kube_pod_owner` (the **same query** as the controller owner, read independently of the controller-row pick) and parsed as the segment **before the first `:`** of the tracking-id value (`<app>:<group>/<kind>:<ns>/<name>`; a value with no `:` is verbatim); per-pod collisions pick the lexically-smallest non-empty tracking-id. Surfaced via `graph.GraphNode.Application() string` (`""` for non-pods and ArgoCD-less pods), resolved in `pkg/build/topology.go` (`resolvePodApplications`, which uses the pure `bucketCluster` helper — `resolvePodOwners` already owns the `kube_pod_owner` missing-cluster tally). **Service and PVC nodes also carry `data.application`** (the `containers` attribute stays pod-only): resolved identically (segment before the first `:`, lexically-smallest on collision) from the `annotation_argocd_argoproj_io_tracking_id` label on `kube_service_annotations` / `kube_persistentvolumeclaim_annotations` (KSM's sanitised form of the `argocd.argoproj.io/tracking-id` annotation, gated on `--metric-annotations-allowlist`), via `resolveServiceApplications` / `resolvePVCApplications` (sharing the `argoAppName` + generic `resolveApplications` helper with the pod resolver). The PVC value is set at topology assembly; the **service** value is threaded into the connection-string resolver (`Topology.ServiceApplications` → `sgResolver.serviceApps`) since service nodes are materialised there. **PVC application inheritance (D13):** a PVC with **no** Application of its own additionally **inherits** the lexically-smallest Application among the pods that mount it (the `pod-mounts-pvc` bindings), via `pvcInheritedApps` in a post-PVC-loop pass at topology assembly (joins each binding's pod ID to the pod's already-resolved `Application()`). The PVC's **own** annotation always wins (the pass fills only app-less PVCs); the inherited value is baked onto `PVCNode.Application()` **before** `graph.NewGraph` freezes the nodes, so it is **indistinguishable** from an annotation-sourced value in `data.application` and drives the same `application` compound group. Because `pod-mounts-pvc` is intra-cluster and same-namespace, inheritance never crosses cluster/namespace; it is resolved over the full graph before projection (a `?cluster=`/`?namespace=`/`?name=` filter dropping the mounting pod does not change the PVC's app), and the min over the binding set is order-free (D6). No new query, metric, node, or edge type. So `Application()` now returns non-empty on `PodNode` / `ServiceNode` / `PVCNode`, and these `application` values additionally drive the `application` compound group for all three (`controller` groups stay pod-only). (2) `data.containers` (`[{name, image}]`) is the pod's container list from `kube_pod_container_info` (one series per container/image; a new 11th topology query rendered as `tlast_over_time(...)` so each series' value is its last-sample timestamp), ordered by `(name, image)` for determinism, empty-`image` series skipped, and — when a container reports more than one image in the window (each image a distinct series) — the **latest-seen image** kept (greatest last-sample timestamp; lexically-smallest image on an exact tie). The latest pick is reliable for near-now windows; for windows far from the real wall clock VM returns only one image-variant per container (see design.md D-A4), so a far-past window surfaces whatever single variant VM returns (never worse than a fixed pick). Surfaced via `graph.GraphNode.Containers() []graph.Container` (`nil` for non-pods and pods with no container info), resolved in `pkg/build/topology.go` (`resolvePodContainers`). Both source reads are KSM defaults (no `--metric-labels-allowlist` for the container metric; the `argocd_tracking_id` label is the operator's allowlist responsibility) and OPTIONAL (absence degrades gracefully, no build failure). No new node/edge type.
- **PVC `volumename` + `svm` labels (NetApp Trident chain).** Each `type="pvc"`
  node's `labels` may additively carry `volumename` (the bound PersistentVolume
  name, from the `volumename` label of `kube_persistentvolumeclaim_info` — read
  per-field independently of `storageclass` in the same one-pass resolver,
  `resolvePVCInfo` in `pkg/build/topology.go`) and `svm` (the NetApp ONTAP SVM
  serving the claim, resolved by chaining two OPTIONAL Trident custom-resource
  metrics within the PVC's own cluster: `kube_tridentvolume_info` — series
  `name` label == PV name → `backendUUID` label — then `kube_tridentbackend_info`
  — matching `backendUUID` → `svm` label; `resolveTridentVolumeBackends` /
  `resolveTridentBackendSVMs`). Both are **plain labels** (strict
  `map[string]string`; NO `data.volumename`/`data.svm` typed field). A key is
  set only when its value resolves non-empty — absent, never empty-string —
  and `svm` is impossible without `volumename` (the chain is rooted at the PV
  name). **`volumename` ≠ `volume`**: the pre-existing `volume` key is the
  pod-spec volume name from the binding metric; both may coexist on one PVC.
  The Trident metrics are NOT stock KSM — they come from a KSM
  custom-resource-state config over the `tridentvolumes`/`tridentbackends`
  CRDs (or compatible exporter); their label names (`name`, `backendUUID`,
  `svm`) are a fixed, case-sensitive contract (D26 style), and both series are
  prefix-aware via `KSG_METRIC_PREFIX`. Absence (non-NetApp clusters) degrades
  gracefully: no `svm`, `volumename` unaffected, never a build failure.
  Duplicate-series collisions resolve to the lexically-smallest non-empty value
  per field / per stage (D6 determinism). Labels are baked at PVC assembly
  before `graph.NewGraph`, so projection/prune/cache semantics are untouched.
  No new node/edge type. Tests: `pkg/build/trident_test.go`,
  `internal/api/testdata/golden/with-netapp-trident-cytoscape.json`,
  `internal/integration` (`TestPVCNetAppTridentLabels`).
- **K8s node `ready_status` attribute.** Each `type="node"` node may carry a typed, nullable `ready_status` attribute — `data.ready_status` (a string), serialised with `omitempty` and **never inside `labels`** — same precedent as `ipaddress` / `owner`. The value is one of `"Ready"`, `"NotReady"`, `"Unknown"`, derived from `kube_node_status_condition{condition="Ready"}` (a new topology query in the `ReadTopology` errgroup; the `condition="Ready"` selector is a fixed, **request-invariant metric-selection contract** — same class as the node-address `type` selector and the D30 sentinel — NOT a caller filter, so the "no filters pushed to PromQL" rule is preserved). The reader reads the `status` label of the **active** row (sample value `1`), matched **case-insensitively**: `true`→`Ready`, `false`→`NotReady`, `unknown`→`Unknown`. Status-label casing is NOT pinned by the KSM-shaped contract — stock kube-state-metrics lowercases it (`addConditionMetrics`→`strings.ToLower`), but an exporter that re-publishes the raw Kubernetes `v1.ConditionStatus` enum verbatim emits `True`/`False`/`Unknown`; both resolve (the reader canonicalises to lowercase at the read site). **Absence is distinct from `"Unknown"`**: `data.ready_status` is omitted entirely when the metric is absent, the node has no `condition="Ready"` series, or no row is active — `"Unknown"` is reserved for the genuine Kubernetes state where the kubelet has stopped reporting; the two MUST NOT be conflated (no defaulting missing data to `"Unknown"`). Surfaced via `graph.GraphNode.ReadyStatus() string` (`""` for non-nodes and nodes with no Ready data), resolved in `pkg/build/topology.go` (`resolveNodeReadyStatus`, keyed `(cluster, node)` like the IP/label joins); on the defensive multi-active tie the lexically-smallest `status` wins (determinism). The metric is a KSM default and OPTIONAL (absence degrades gracefully, no build failure). No new node/edge type.
- **Upstream metric-name prefix is an additive `KSG_METRIC_PREFIX` knob** applied to KSM-shaped series only (`kube_pod_info`, `kube_node_info`, `kube_node_status_addresses`, `kube_pod_spec_volumes_persistentvolumeclaims_info`, `kube_node_labels`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`, `kube_pod_owner`, `kube_replicaset_owner`, `kube_persistentvolumeclaim_info`, `kube_storageclass_info`, `kube_pod_container_info`, `kube_node_status_condition`, `kube_service_annotations`, `kube_persistentvolumeclaim_annotations`, `kube_tridentvolume_info`, `kube_tridentbackend_info`, and the `kube_node_info`-backed cluster-discovery query). The prefix is prepended verbatim — trailing underscore is the operator's responsibility. NOT applied to `traces_service_graph_request_total` (different exporter family — Alloy/Tempo) or `up{}` (Prometheus-native). The D29 endpointslice → service join reads `kube_endpointslice_labels{label_kubernetes_io_service_name}`, which KSM only emits when `--metric-labels-allowlist=endpointslices=[kubernetes.io/service-name]` is set (NOT exposed by default). The metric-name suffix and the label-name set per series are a fixed contract any compatible exporter MUST honour; see design.md D26. Threaded via `promql.Renderer{Prefix}` held on `build.Builder` and `api.Server`; the `Query` string constants remain bare so `query=` / `query_name=` dimensions on self-metrics and spans stay stable across deployments that differ only by prefix.

### Reusable `pkg/` graph engine (D32)

The graph engine lives under `pkg/` so other Go modules can import it in-process
(no HTTP, no JSON round-trip); `internal/api` is a thin HTTP / auth shell over it:

- `pkg/graph` — `Graph`, the sealed `GraphNode` + five node types, `Edge`,
  `Project`, `Scope` / `NewScope`, `View`, `SortNodes` / `SortEdges`, `EdgeTypes`.
- `pkg/build` — `Builder` + `Build`; topology / service-graph readers. Takes a
  `build.Options{MetricPrefix, APITimeout}` and a no-op-tolerant `build.Metrics`
  interface — **not** `internal/config` / `internal/observability`, whose
  couplings were broken so the package is externally importable.
- `pkg/promql` — `Querier`, `Renderer`, `Client`, and a no-op-tolerant
  `promql.Metrics` interface.
- `pkg/clock`; `pkg/cytoscape` — `Serialise(g, view) Body` plus the Cytoscape DTO.
- `pkg/kubegraph` — the convenience facade: `Engine.BuildFromValues(ctx,
  url.Values) (cytoscape.Body, error)` folds parse → build → project → serialise
  into one call. `kubegraph.ParseValues` is the **single** request parser, shared
  by `internal/api`'s handler and the facade, so the `/v1/graph` request contract
  cannot drift between the server and an embedded consumer.

`pkg/` packages MUST NOT import `internal/*` — Go's internal rule would block any
external module from importing the engine. Metrics and OTLP tracing are injected
with no-op defaults, so an embedder does not inherit ksg's `kube_state_graph_*`
self-metrics; the concrete `*observability.Metrics` satisfies
`build.Metrics` / `promql.Metrics` structurally via wrappers in
`internal/observability/adapters.go`. The first external consumer is
`graph-api-gateway` (its `embed-ksg-graph-engine` change).

### Sealed graph types

`graph.GraphNode` is a sealed interface (`isGraphNode()` unexported). Concrete
types: `PodNode`, `K8sNode`, `PVCNode`, `ServiceNode`, `ExternalNode`. All
expose `ID()`, `Name()`, `Type()`, `Labels()`, `IPAddress()`, `Owner()`,
`Application()`, `Containers()`, `ReadyStatus()`. Serialisation
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
`StorageClassNode` and for ArgoCD-less pods/services/pvcs) and
`Containers() []graph.Container` returns a `PodNode`'s ordered `{name, image}`
list (`nil` for every other node kind) — serialised as the `omitempty`
`data.application` / `data.containers` attributes.
`ReadyStatus() string` returns a `K8sNode`'s Kubernetes Ready-condition status
(`"Ready"` / `"NotReady"` / `"Unknown"`; `""` for every other node kind and for
nodes with no Ready data) — serialised as the `omitempty` `data.ready_status`
attribute. `""` (omitted) is distinct from `"Unknown"` (kubelet lost contact).

### Test stack layers

Boundary rule: **unit tests must not contact a real upstream service**. Anything
that needs a TCP socket fronting upstream is integration. Unit tests substitute
upstream behind small interfaces (`promql.Querier`, `auth.Validator`,
`clock.Clock`) using mockery-generated mocks under `pkg/{clock,promql}/mocks/`
and `internal/auth/mocks/`.

| Layer | Where | Real I/O? |
|---|---|---|
| Unit | `pkg/{graph,build,promql,clock,cytoscape,kubegraph}/*_test.go` + `internal/{config,auth,telemetry}/*_test.go` | None — pure functions: parsers, joins, projection, edge IDs, request parsing, serialiser, KeySet, Clock. |
| Component | `internal/api/*_test.go` | None — gin handlers driven via a `MockQuerier` injected through `promql.Querier`; `httptest.NewServer` only wraps the server-under-test, never fakes upstream. Test helpers in `internal/api/helpers_test.go` (`newServerWithMocks`, `newMockQuerier`, `newErrQuerier`, `vec`). |
| Golden | `internal/api/golden_test.go` + `testdata/golden/*.json` | None. Wire-format snapshots; run with `-update` to refresh. |
| Property | `pkg/graph/property_test.go` | None. Random multi-cluster graphs → invariants (orphan edges, traversal depth, ID uniqueness). |
| Integration | `internal/integration/*` | **Docker required.** testcontainers-go VictoriaMetrics suite; gated `SkipIfDockerUnavailable` — skips locally without Docker, runs full on CI (ubuntu-latest). Inject hooks into the in-process API via `StartAPIServer(cfg, WithClock(...))`. |

When **adding a unit test that needs to fake upstream PromQL**, use
`newMockQuerier(t, fixtureSet{...})` — never spin up an `httptest.NewServer`
to impersonate the Prometheus HTTP API.

When **changing an interface** registered in `.mockery.yaml`
(`promql.Querier`, `auth.Validator`, `clock.Clock`), run `make mocks` and
commit the regenerated files. CI's `mocks-drift` job will fail otherwise.

## OpenSpec workflow

Spec-driven changes live under `openspec/changes/<name>/` with four artifacts
in dependency order: **proposal → design + specs → tasks**. The
`/opsx:*` commands and the `openspec` CLI manage the lifecycle.

Common openspec commands:

```bash
openspec list                                       # all active changes
openspec status --change "<name>"                   # artifact progress + tasks
openspec validate "<name>"                          # checks structure
openspec instructions <artifact> --change "<name>" --json   # what to write
openspec verify "<name>"                            # before archive
openspec archive "<name>"                           # promote to openspec/specs/
```

The v1 implementation change **`add-k8s-pod-graph-api`** is archived under
`openspec/changes/archive/2026-06-06-add-k8s-pod-graph-api/`; its capability
specs were promoted to `openspec/specs/`. When making non-trivial behaviour
changes, start a new change and update the relevant promoted spec
(`openspec/specs/<capability>/spec.md`) before touching code.

## Repository conventions

- All HTTP routes live under `/v1/`. Adding a route means committing to keeping
  it for v1's lifetime. Schema changes that aren't additive are v2 — see D14.
- Self-metric names are stable contracts: `kube_state_graph_*`. Adding a label
  to an existing metric is a contract change — see design.md D26.
- Errors returned to HTTP carry a typed `build.Reason` mapped to a fixed
  status + `reason` string in `internal/api/errors.go`. Adding new failure
  modes means adding both a `Reason` constant and an entry in `mapBuildError`.
- Don't **talk to the Kubernetes API** from the API server — no client-go
  clients, no informers, no watches, no kubeconfig, no per-cluster RBAC. All
  cluster facts come from VictoriaMetrics (topology, service graph) or the
  versioned Istio-config store (route resolution). The rule's reasons
  (archived design D1 / D16): informers only know the *current* state and
  cannot answer this API's historical `?start=&end=` contract, and
  multi-cluster would need N watch streams + per-cluster RBAC. **Linking a
  library that transitively vendors Kubernetes types is NOT a violation;
  constructing a Kubernetes client is** (translate-global-fqdn-to-k8s-service
  D0) — `pkg/route` links `istio.io/istio` (→ `k8s.io/client-go` types) purely
  as an in-memory translation library and never dials an apiserver. Tests and
  harness tooling are exempt.
- Don't add dependencies casually. Current direct deps: Gin, Prometheus
  client_golang, google/uuid, golang.org/x/sync, testify v1.11.x (test-only,
  also drives mockery-generated mocks), testcontainers-go (integration
  test-only), swaggo/swag/v2 (codegen tool, not imported at runtime),
  vektra/mockery v2.x (codegen tool tracked via go.mod `tool` directive,
  not imported at runtime, not linked into the production binary), the
  OpenTelemetry Go SDK family (`go.opentelemetry.io/otel`, `sdk`, `sdk/log`,
  OTLP gRPC + HTTP exporters for `otlptrace` and `otlplog`, `semconv/v1.27.0`,
  `contrib/...otelgin`, `contrib/...otelhttp`, `contrib/bridges/otelslog`),
  and — **contained to `pkg/route` + `cmd/` only** (see the route-resolution
  bullet) — `istio.io/istio` + `istio.io/api` (pinned; in-process istiod
  translation), `ClickHouse/clickhouse-go/v2` (route store), and
  `envoyproxy/go-control-plane/envoy` (RouteConfiguration protos).
  Adding more requires a design-doc note.
- Production code MUST NOT carry test-only fields, methods, or constructors.
  Inject substitutable behaviour via the small interfaces in
  `internal/{promql,auth,clock}` (`Querier`, `Validator`, `Clock`); tests
  consume mockery-generated mocks under `internal/<pkg>/mocks/`. If a new
  hard-to-test dependency appears, add an interface + regenerate mocks rather
  than a `SetXxxFunc` setter.
