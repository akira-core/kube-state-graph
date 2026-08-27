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
(carrying `client_k8s_pod_uid` + `server_k8s_pod_uid`) — both read from
VictoriaMetrics. That upstream is **one or more** installations: a routing table
dispatches each query to the store(s) holding it, selected by availability zone
and by metric family (see "Upstream backend routing" below). With no routing
table configured it is a single endpoint at `--prom-url`, byte-for-byte as
before. Multi-cluster, cross-cluster, and service-graph
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
parseGraphRequest        ── kubegraph.ParseValues → Request{Start, End, Scope, Selector}
   │                        validates start/end (RFC 3339 or Unix seconds; only `end > start`),
   │                        selector values (≤253 bytes, no control chars) and `prune`
   ▼
context.WithTimeout(ctx, --build-timeout)   ── graph endpoints only; deadline exceeded → 504 timeout
   └─ Builder.Build(ctx, window, end, sel)
         ├─ ReadTopology  (errgroup of 37 PromQL queries in parallel: KSM topology incl. node ready_status + 3 D29 service/endpointslice + 2 D34 owner + PVC-info + container-info + 6 controller-annotation families + kube_job_owner + 13 Harvest + 2 kubelet; 20 fetch + 17 fetchOptional — kube_replicaset_annotations and kube_job_annotations degrade with Harvest/kubelet)
         ├─ ReadServiceGraph (errgroup of 3 PromQL queries in parallel: the required request total + 2 OPTIONAL RED — failed total + server-seconds histogram; `user`/`unknown` peers excluded at selector — D30; joined with topology)
         └─ assemble + graph.NewGraph → *Graph (immutable, with adjacency)
   (no in-process concurrency cap; HPA + Pod resource limits handle load shedding)
   ▼
graph.Project(g, scope)            ── projection-level filters (cluster/namespace again, edge_type, prune)
   ▼
serialiseCytoscape
```

v1 has **no in-process result cache** and **no singleflight**. Each request runs a fresh upstream fan-out and recomputes the body. A future iteration is expected to add a horizontally scalable cache mechanism for distributed deployment (Redis L2, background materialiser, or graph DB) — tracked as a separate change.

### Load-bearing design rules

These are non-obvious; read the archived design doc
`openspec/changes/archive/2026-06-06-add-k8s-pod-graph-api/design.md`
(D1–D34) before changing any of them. The capability specs it produced now
live under `openspec/specs/`.

- **No server-side result cache.** Each `/v1/graph` request runs a fresh upstream PromQL fan-out. A horizontally scalable cache mechanism for distributed deployment is anticipated but out of scope for v1; it would be keyed by `(window, az, env, cluster-set, namespace-set)` — the selector-level dimensions vary the queries themselves, so one built graph can serve only the projection-level filters (`edge_type`, `prune`, and the re-applied `cluster` / `namespace`).
- **Default projection is the connectivity-connected subgraph.** Every `/v1/graph` response carries only the workload that sits on a **connectivity edge** (`pod-calls-pod` / `pod-calls-service` / `service-selects-pod`) plus the infra that hangs off it. Concretely: a pod is kept iff it is an endpoint of a connectivity edge; an edgeless pod is dropped, and with it (via the generalised D6 reference rule) the node hosting only edgeless pods, the PVC mounted only by edgeless pods, and the NetApp aggregate serving only such PVCs (and then its controller). **An unmounted PVC (no `pod-mounts-pvc` binding at all) is therefore dropped too** — a PVC is kept iff a connectivity-connected pod mounts it. **Service nodes are unaffected** — they are only ever materialised by the D29 connection-string resolver, so they are connectivity-born by construction (topology `kube_service_info` is index-only, never emitted as a node). The decision set is `graph.connectivityExcluded(g)` — a **pure function of the built graph** (scope-independent: a PVC's keep/drop depends on whether its mounting pod is *connected*, not on the request's cluster/namespace filter, and the two co-move because `pod-mounts-pvc` is intra-cluster/same-namespace), computed once in `graph.Project` and consulted in **both** `filterNodes` (skip excluded ids) **and** `filterEdges`/`readdEdgePartners` (an excluded pod/PVC is never resurrected as an edge partner — e.g. the pruned pod of a `pod-to-node` edge whose host node survived via another pod). The prune is **suppressed by exactly one escape hatch**: `?prune=false` (`graph.Scope.Inventory`, stored INVERTED so the zero `Scope` keeps the prune on). The former `?name=` / `?root=` hatches are withdrawn with those parameters. `?cluster=` / `?namespace=` / `?az=` / `?env=` do **not** disable the prune. **Consequence:** the default view is the traffic graph, not the inventory — an edgeless pod, an unmounted PVC, or a podless node is fetched with `?prune=false` (optionally narrowed by `cluster` / `namespace` / `az` / `env`). The prune itself stays a **projection concern** and a pure function of the built graph.
- **No time-window alignment, no window cap, no future-time guard.** `start` and `end` are passed through to upstream PromQL verbatim; only `end > start` is enforced. The previous 60 s `floor`/`ceil` grid was removed alongside the in-process cache it was bucketing for. Bounded query cost is delegated to upstream VictoriaMetrics search limits (`-search.maxQueryDuration`, `-search.maxPointsPerTimeseries`, `-search.maxSamplesPerQuery`). Response body is `{apiVersion, clusters, elements}` — no time fields are echoed.
- **`labels` is strict `map[string]string`** on both nodes and edges. No bools,
  no numbers, no string-encoded numbers. Boolean flags (`cross_cluster`, `ghost`)
  remain deferred to a future typed field. **RED edge metrics** live on the
  typed nullable `Edge.Metrics *EdgeMetrics` (`rate`, `error_rate`,
  `p90_server_ms`) serialised as `data.metrics` — never inside `labels`.
  Attachment rule (hardcoded): a **trace-derived** edge whose **both resolved
  endpoints** name a `type="pod"` node (real or synth) or a `type="service"`
  node — enforced by `sgResolver.isPodOrServiceID`, NOT by the raw UID labels
  and NOT by the edge type (D33 clears a `"://"` side's UID after the labels are
  read, and an `external` target leaves the type at `pod-calls-pod`). **How** an
  endpoint was identified is irrelevant: pod UID, `"://"` connection string,
  `server="unknown"` peer address → ClusterIP or Pod IP, and route-engine
  resolution all qualify, so `pod-calls-service` edges ARE measured. No metrics
  on: any edge with an `external` endpoint; synthesised edges
  (`service-selects-pod` fan-out, the ingress-chain gateway-pod → backend hop,
  topology edges); and the route-hit chain's **caller → ingress entry hop**
  (`chainEntryIndex` — that hop and the retained caller → backend edge are two
  projections of ONE call, so only the backend is measured and a sum over the
  chain never double-counts). A contributing series carrying
  `edge_relation="link"` is **out of scope** (span-link virtual edge — the call
  crosses a queue/DB and the two spans are different trace contexts): the edge
  is still emitted, but the series feeds no rate/error/bucket, so a mixed edge
  is measured over its non-link subset and an all-link edge gets no `metrics`
  object (empty in-scope set ⇒ rate 0 ⇒ ineligible; no special case). Three
  parallel queries: `traces_service_graph_request_total` (required for the
  edge; deliberately NOT link-filtered), plus OPTIONAL `..._failed_total` and
  `..._server_seconds_bucket` — both read at the total counter's **raw** label
  granularity (the histogram has NO upstream `sum by`) and joined by exact
  series identity (the histogram minus `le`) through one `seriesKey → pairKey`
  map, both carrying D30's sentinel plus `serviceGraphLinkExclusionSelector`
  (`edge_relation!="link"`). The queried population is a **superset** of the
  attached one (endpoint node type has no label-level form); what holds is the
  one-way property — every query-layer filter is mirrored in Go, so no eligible
  edge loses its companion series and reads `error_rate: 0`. Failure/duration
  errors degrade field-by-field (`error_rate` absent ≠ `0`; `p90_server_ms`
  omitted) and never fail the build; a non-empty companion vector that joined
  NOTHING is warned per vector (`failed_total_label_set_mismatch` /
  `server_seconds_bucket_label_set_mismatch`). Values are JSON numbers rounded
  to 6 significant digits at serialisation and MAY appear in exponent form.
  `pod-calls-pod` and
  `pod-calls-service` edges carry a single `labels.cluster` (the trace source /
  client-side cluster, omitted when the client side is non-pod). Cross-cluster
  status is derived by comparing the resolved source-node and target-node
  `labels.cluster` — D9.
- **Edge IDs are UUIDv5** with a fixed compiled-in namespace (`graph.edgeNamespace`)
  and the canonical input `<type>|<source>|<target>`. Stable across rebuilds —
  required for golden tests. Bumping the namespace UUID is a v2 break.
- **Cluster-scoped IDs everywhere.** Pods: `<cluster>/<uid>`, K8s nodes:
  `<cluster>/<node>`, PVCs: `<cluster>/<namespace>/<claim>`, externals:
  `external/<value>`. Node names are not globally unique without the prefix.
#### Service-graph glossary (load-bearing terms)

- **Trace-derived edge**: an edge produced from at least one
  `traces_service_graph_request_total` series (the `pairs` map in the parse).
- **Synthesised edge**: an edge with no originating series —
  `service-selects-pod` fan-out, topology edges (`pod-to-node`, `pod-mounts-pvc`,
  `pvc-to-netapp-aggr`), and the route-hit ingress-chain's gateway-pod →
  backend-service `pod-calls-service` hop. Spelling is British **synthesised**
  in prose; Go identifiers may use `synthesized` (e.g. `routeChainEdges`
  comments). NOT synthesised: the chain's **caller → ingress entry hop**, which
  IS trace-derived — it is excluded from RED for a different reason (it
  re-projects the caller → backend call).
- **UID-resolved endpoint**: resolved from a non-empty `client_k8s_pod_uid` /
  `server_k8s_pod_uid` to a `type="pod"` node (topology or synth).
- **Peer-resolved endpoint**: identified only via the unknown-server peer-address
  ladder (including Pod-IP) — the connector could not pair a server span. Since
  the RED revision this is a provenance label only: it does NOT affect metrics
  eligibility, which turns on the resolved node type.
- **Contributing series**: the set of total-series samples that collapsed onto
  one `(src, tgt)` pair during resolution.
- **In-scope series**: a contributing series that does NOT carry
  `edge_relation="link"` — i.e. one that measures the edge. Rate, error
  numerator and duration buckets are all summed over this subset and no other.
- **RED scope**: the edges that carry `data.metrics` — trace-derived, both
  endpoints pod-or-service, not the chain entry hop, at least one in-scope
  contributing series.

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
    in-memory at resolution — it adds no PromQL matcher of its own; the
    request-scoped selectors of push-request-filters-upstream are a separate,
    hardcoded per-series contract). The
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
  D1–D3, extended by resolve-unknown-server-ip-peer,
  resolve-unknown-server-network-peer-address, and
  resolve-unknown-server-pod-ip-peer, hardcoded — no knob): the one
  carve-out from the D30 outcome above. When `client_k8s_pod_uid` resolves to
  a **real topology pod** (never a synthesised one) AND the server side has no
  resolvable pod (UID empty, or present but absent from `Topology.PodsByUID`)
  AND the raw `server` label is exactly `"unknown"`, `resolveServer` dispatches
  to the new `resolveUnknownServerPeer` instead of the generic empty-UID
  (`resolveEmptyUID`, which owns the D27 fallback) or synth-pod path — never
  both, for this literal value. It reads **three** client-recorded peer-address
  labels, checked in this precedence order — `client_server_address` (checked
  first), then `client_network_peer_address` (checked second), then
  `client_net_peer_name` (checked third) — the first non-empty wins outright
  and is never merged with, nor falls back to, a lower-precedence label that
  fails to classify. The three are distinct OTel attributes, not three
  spellings of one: `client_server_address` is the stable `server.address`
  (logical destination as addressed — name, IP, or UDS name);
  `client_network_peer_address` is the stable `network.peer.address`
  (socket-level peer address, by convention an IP); `client_net_peer_name` is
  the deprecated `net.peer.name`, superseded by `server.address`. The order
  ranks them by what the classification chain below can resolve — strong on
  names (DNS grammar / bare short name, both reaching `resolveServiceLevel`
  with its family-wide fan-out), weak on IP literals (the `ClusterIP` lookup
  is anchor-cluster-only) — so the name-valued stable attribute leads, the
  IP-valued stable attribute follows, and the deprecated name-valued attribute
  trails. `client_network_peer_port` is deliberately **not read** — the
  stable conventions split the port into its own attribute, but a port
  participates in neither peer identification nor node naming.
  Whichever label wins is normalised in two steps before classification: (1)
  bracket-suffix truncation — cut at the **first `[` whose index is > 0**,
  discarding it and the remainder (some instrumentations append a bracketed
  connection/session id to the authority, e.g. `mongo.com:27017[-181]`, which
  `net.SplitHostPort` cannot handle and which `classifyK8sDNS`'s lack of
  DNS-1123 validation would otherwise garbage-classify); a leading `[` (index
  0) is left untouched because it is the IPv6 bracket form
  (`[2001:db8::1]:8080`), which step (2) already handles correctly — an
  unconditional cut would destroy a resolvable dual-stack `ClusterIP` peer;
  (2) an optional trailing `:<port>` is then best-effort stripped via
  `net.SplitHostPort`. Both steps apply uniformly regardless of which label
  supplied the value — the resolver stays provenance-free. The result is
  classified via the same `classifyK8sDNS` grammar D29 connection-string
  resolution uses (2-label `<service>.<namespace>`, 3-label headless
  `<pod>.<service>.<namespace>`, `.svc[.<domain>]` suffix stripped), **plus
  three grammar extensions scoped to this rule only**: (1) a single dot-free,
  non-IP-literal label is treated as a bare short Service name resolved in the
  **client pod's own namespace** — note this means bracket truncation can
  promote a value like `mongo:27017[-181]` into the bare short name `mongo`,
  resolved in the client's own namespace, exactly as the un-bracketed
  `mongo:27017` already does; (2) (resolve-unknown-server-ip-peer) when
  neither the DNS grammar nor the bare-short-name form matches AND the host is
  a valid IP literal (`net.ParseIP`), it is looked up as a Service `ClusterIP`
  **within the already-resolved client pod's own (anchor) cluster only** —
  never a family sibling, since a `ClusterIP` is a per-cluster address that
  can legitimately collide across unrelated clusters' Service CIDRs (unlike a
  Service DNS name, which is a mesh-wide convention the family union already
  handles); (3) (resolve-unknown-server-pod-ip-peer) when the IP literal
  matches **no** Service `ClusterIP`, it is looked up as a **Pod IP** against
  a second index (`famIPKey{family, pod_ip} → []podIPCandidate{cluster, pod}`,
  built once per parse from `topology.Pods` in **two stages**: stage 1 reduces
  to one holder per `(cluster, ip)` in the same loop as `podByID`, skipping
  pods with no `pod_ip`; stage 2 regroups by `ClusterFamilyKey(cluster)` and
  sorts each group by cluster, exactly like `svcCandidates`). This covers a
  caller that dialled another pod's address directly, bypassing any Service —
  **including across a cluster boundary**, which is ordinary traffic wherever
  clusters share a flat routable network. **Selection**: the **anchor
  cluster's own** holder always wins (byte-for-byte the anchor-only
  behaviour); otherwise a **lone family holder** resolves; **two or more**
  family holders yield no pod and degrade via `routeExternal` with the
  distinct reason `unknown_server_peer_pod_ip_ambiguous` — **no tie-break
  across clusters**. Being the family's only holder IS the evidence that its
  pod CIDRs do not overlap at that address, which is why **no service-mesh
  gate is applied**: cross-cluster pod-to-pod reachability is a network-layer
  property, and an `istio-proxy` sidecar is neither necessary (a flat network
  needs no Istio) nor sufficient (in a multi-network mesh the caller's sidecar
  is handed the east-west gateway address, never a remote Pod IP). A cluster
  outside the anchor's family is never a candidate. A hit resolves the
  endpoint **straight to that topology pod** — it does NOT go through
  `resolveServiceLevel`, materialises **no service node** and emits **no
  `service-selects-pod` edge**, so the generic target-driven rule makes the
  edge `pod-calls-pod` (which MAY therefore cross clusters). Ordering is
  structural, not conventional: the ClusterIP step lives inside
  `classifyPeerHost` and a hit there returns `classified=true`, so
  **`ClusterIP` always beats Pod IP**, and the Pod-IP step sits immediately
  before `routeExternal`, so it also beats the route engine and the external
  fallback. The **ClusterIP lookup itself stays anchor-only** — Service CIDRs
  overlap just as readily, and under multi-primary the same
  `(namespace, service)` carries a *different* ClusterIP in each cluster. On a
  **same-cluster** duplicate `pod_ip` — the normal case for `hostNetwork`
  pods, which all report their node's address, and transient on address reuse
  within the window — stage 1 keeps the **lexically-smallest pod ID**
  (order-free, D6), so an intra-cluster duplicate never makes the family look
  ambiguous. `lookupPeerPodIP` is pure and shared with the
  `collectRouteQueries` prescan, which skips resolvable endpoints so the route
  engine is never asked about traffic the in-cluster ladder now resolves —
  while an **ambiguous** family, which does fall external, is still offered to
  the engine. An IP-valued peer that matches neither an anchor-cluster
  `ClusterIP` nor a resolvable family Pod IP (a sidecar loopback, a
  NodePort/LB address, any off-cluster IP, or an ambiguous family) becomes an
  `external/<ip>` node, not a dropped endpoint. The reverse index
  (`(cluster, ClusterIP) → Service`) is built once per parse from
  `topology.ServicesByNameNS`, skipping empty/`"None"` ClusterIP; on a
  same-cluster duplicate `ClusterIP` (a data anomaly Kubernetes itself
  prevents), the lexically-smaller `(namespace, service)` wins. Once
  identified via IP, resolution proceeds through the SAME
  `resolveServiceLevel` call as every other classification path below —
  including its normal family-wide `service-selects-pod` fan-out — only the
  identification lookup itself is anchor-scoped. A successful classification
  resolves via the existing `resolveServiceLevel(anchorCluster, ns, svc)` —
  anchor = the already-resolved client pod's own cluster (no anchor-recovery
  fallback chain needed here, unlike D29) — with the same anchor-membership
  test and cross-cluster `service-selects-pod` fan-out. An unresolvable
  classification, or a `resolveServiceLevel` miss, falls back to
  `external/<raw_peer_address>` — the RAW, wholly unnormalised label value
  (neither bracket-truncated nor port-stripped) — same convention for all
  three labels; a host dialed under several distinct bracketed identifiers
  therefore materialises one external node per identifier. All three labels
  empty/absent, or the client did not resolve to a real pod, drops the
  endpoint (no node, no edge) — **identical outward behaviour to the
  pre-change blanket exclusion**. This is the invariant the loosened selector
  must never violate: it must never leak a `external/unknown` node via the
  generic D27 path for a case outside this rule's trigger. **Note:** this
  extension reorders the two pre-existing labels — a series carrying both
  `client_net_peer_name` and `client_server_address` with different values
  now resolves from `client_server_address` (previously the reverse), which
  can also change an unresolved endpoint's external node `id`/`name` from
  `external/<client_net_peer_name>` to `external/<client_server_address>`.
- **Span-link logical edge relation marking** (add-span-link-logical-edges,
  hardcoded — no knob): a series whose `edge_relation` label is exactly
  `"link"` (span-link-derived: client = producer pod, server = consumer pod,
  joined across trace IDs through a broker) resolves through the ordinary
  ladder unchanged and its emitted edge carries `labels.relation="link"`;
  each side whose own pod resolved to a REAL topology pod additionally
  derives its broker node ID from its OWN peer-address labels — client side
  the existing `client_server_address`/`client_net_peer_name` (+
  `client_dns_answers`/`client_server_port`), server side the mirrored
  `server_server_address`/`server_net_peer_name` (+ `server_dns_answers`/
  `server_server_port`, filled into the same `peerLabels` struct by
  `serverPeerLabelsOf`; no `server_network_peer_address` in v1) — via
  `sgResolver.viaNodeID`, a **lookup-only** mirror of the
  unknown-server-enrichment classification chain (shares every pure helper;
  route index consulted through `routeNodeID`, the lookup-only twin of
  `routeIndexResolve` that takes only the RouteHit BACKEND — never the
  ingress hop, no `role` marking, no chain — and degrades everything else to
  `ExternalID(raw)`); the `(pod, broker)` pair marks the matching
  `pod-calls-pod`/`pod-calls-service` edge `labels.relation="transport"`.
  Marking is set-membership at edge-build time over two
  **`parseWithResolver`-local** sets (`linkPairs`/`transportPairs` — no
  resolver field, no cross-build state); insert-only accumulation makes it
  order-free (D6), `link` wins over `transport` and over plain series for the
  same pair, `service-selects-pod` fan-out and synthesized route-chain edges
  are NEVER marked, and a transport pair with no matching edge is a pure
  marker (aggregated Debug, never synthesised — via lookup materialises
  NOTHING, the `resolveRouteChain` orphan-protection precedent). A link
  series with `server=="unknown"` and no resolvable server pod recovered no
  consumer and contributes **NO markers at all** (neither `link` nor
  `transport`, no via pairs — its producer→broker edge stays the ordinary
  unmarked enrichment outcome, byte-identical): the rendering contract is
  "transport = the network hop backing a rendered logical edge", so a
  `transport` edge always coexists with a `link` edge from the same series
  set in the built graph — do NOT re-add a demote-to-transport rule (other
  degrades — synth pod, D27 ghost external — keep `link`). Any other
  `edge_relation` value is ignored (exact match). Prescan: link series emit
  ≤2 via keys (per resolved side, anchor = that side's own pod cluster)
  through `viaRouteKey` — the extracted skip chain the unknown-server branch
  also uses — deduped by the prescan `seen` map with ordinary unknown-server
  keys (same `peerRouteKey` derivation ⇒ one store read per broker FQDN per
  anchor cluster; the in-memory chain stays un-memoised per the
  `resolveConnString` precedent). Edge IDs (UUIDv5 over `type|source|target`)
  and the D30 selector are untouched; `relation` is registered on the
  `pod-calls-pod`/`pod-calls-service` `graph.EdgeTypes` entries only. Tests:
  `pkg/build/servicegraph_link_test.go`, golden
  `link-relation-cytoscape.json`, `internal/integration`
  (`TestSpanLinkRelationEdges`).
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
  `lookupClientPod` / `anchorHolds` with the parse — and, in `ReadServiceGraph`,
  the very same `sgResolver` instance, so the two cannot drift and the topology
  indexes are built once per build), resolves the deduped keys under a bounded
  `errgroup` (`routeResolveConcurrency`; each call bounded by
  `--route-resolve-timeout`; the key set capped at `maxRouteKeys` with any
  truncation logged), and hands `parseServiceGraphRoutes` a prefetched index —
  nil index ⇒ byte-for-byte pre-change output. Concurrency cannot change the
  index's CONTENTS (entries are keyed by `routeKey` and independent); it changes
  only which keys are answered when the build deadline fires first, already
  wall-clock dependent when the loop was serial. **(3) Listener port
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
  single store read. The scope is one-build and mutex-guarded (keys resolve
  concurrently; the store read stays outside the lock, so a racing duplicate
  probe is possible and harmless); the shared `*Resolver` stays stateless (an
  instance cache would leak). Errors are not cached; no outcome/determinism
  change.
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
  the **multi-IP union is deduped by resource-version identity**
  `(cluster, namespace, name, valid_from)` — a dual-stack ingress Service makes
  each per-IP load return the same rows, and istiod's config store rejects a
  duplicate, so an undeduped union failed the whole resolution; `ScopedFor` /
  `backendServices` enforce the same one-entry-per-identity invariant on their
  own output. **Destination-host identity follows istiod exactly**: every
  config carries `Domain` (`store.ClusterDomain`), so a dot-free
  `destination.host` resolves to `<name>.<vs-namespace>.svc.cluster.local` (the
  common way operators write a destination) while anything containing a dot is
  left verbatim — istiod does not expand `checkout.shop` either, so that shape
  names no registry Service and correctly stays external. One
  `store.VSDestHosts` serves both the reader and the snapshot, and
  `ParseBackendHost` requires exactly two leading labels;
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
  (`r.Namespace == svcNS`) — so a single IP's candidate set can never hold two
  same-named Gateways (K8s per-ns name uniqueness). Istio's cross-namespace
  selector attachment is deliberately out of scope (degrades `no_gateway` → LB
  fallback/external). The **gateway identity carried downstream is
  `(namespace, name)`** (`ScopedFor(ns, name)`): the LOADED rows are a
  deliberate superset spanning namespaces (the gw_versions SQL binds the union
  of every ingress Service namespace carrying the IP), and a multi-IP request
  unions candidates across IPs, so a bare-name scan could select another
  namespace's same-named Gateway — and since the selected row's namespace also
  decides which VirtualServices bind to it, that was a WRONG destination, not a
  miss. `gwresolve` still matches on host patterns and returns a bare name;
  `pickCandidate` recovers the namespace, and same-named candidates from two
  namespaces (only reachable multi-IP) degrade rather than guess. **Hop 1
  degrades on ambiguity** — more than one live Service identity carrying the IP
  yields no candidates, matching `ingressServiceIdentity`'s rule for the same
  situation — and **hop 2 unions** the pod labels of every matching ingress
  Deployment (a revision-based canary gateway upgrade runs two; the SQL layer's
  `labelUnion` already did this), so neither hop depends on storage row order.
  Two
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
- **Two filter classes: selector-level and projection-level** (push-request-filters-upstream; supersedes the old "no filters pushed to PromQL" rule). **Selector-level** — `cluster`, `namespace`, `az`, `env` — are rendered into the upstream queries as label matchers by `promql.Render(q, window, keys, sel)`, so VictoriaMetrics narrows the build at the source. **Projection-level** — `edge_type`, `prune`, plus `cluster` / `namespace` re-applied as defence in depth — are applied over the built graph. Which dimension reaches which series is the hardcoded `promql.queryDims` table (a test parses `queries.go` and fails on a Query constant with no entry): pod/claim/Service/EndpointSlice KSM series + kubelet = all four; `kube_node_*` = az/env/cluster; **NetApp Harvest = NO request matcher** (its `cluster` label is the ONTAP cluster, not a Kubernetes one; it carries no namespace; and `az` reaches it only as backend ROUTING through the routing-only `dimAZRoute` bit — `dimsHarvest = dimAZRoute` — while `env` does not reach it at all; it is narrowed by reference through the loaded claims); **the three `traces_service_graph_*` queries and `up` take NO request matcher**. Rendering is a pure function of the sorted, de-duplicated value set (single value → `key="v"`, several → one anchored `key=~"a|b"` with `regexp.QuoteMeta` + string escaping), fixed dimension order `az, env, cluster, namespace`, and the `cluster` value `unknown` renders `cluster=~"unknown|"` — the literal PLUS the empty alternative, always the regex form — because `build.bucketCluster` puts an absent label AND a literal `unknown` in the SAME bucket, so the matcher must accept both (`promql.ClusterUnknownValue` is the one spelling shared by the query and parse layers). Each query's **fixed** selector (`type=~"ExternalIP|InternalIP"`, `condition="Ready"`, `lun=""`, `owner_kind="CronJob",owner_is_controller="true"` on `kube_job_owner`, `annotation_argocd_argoproj_io_tracking_id!=""` on the six controller-annotation families, the D30 sentinel, `edge_relation!="link"`) is a request-invariant metric-selection contract and is always rendered FIRST, composed with — never replaced by — the request matchers. Each mirrors a discard its Go reader already performs BEFORE keying or tallying the sample, so the pushdown is output-preserving down to the missing-cluster tally — pinned by `TestResolveJobCronJobOwners_QuerySelectorIsOutputPreserving` / `TestResolveApplications_TrackingIDPresenceIsOutputPreserving`; a matcher STRICTER than its reader would silently drop data. A zero `promql.Selector` adds no request matcher, so every query renders exactly its fixed form (`TestRender_EmptySelectorMatchesBaseline` diffs against `pkg/promql/testdata/render-baseline.txt`, which moves whenever a fixed selector does). The `az` / `env` label KEYS are operator-configurable (`--az-label` / `KSG_AZ_LABEL`, `--env-label` / `KSG_ENV_LABEL`, defaults `az` / `env`, validated as PromQL label names and required to differ); the request parameter names never change. **Operator precondition:** every kube-state-metrics and kubelet family must carry the configured labels — a family that does not vanishes under an `az` / `env` filter, and the connectivity prune can then empty the graph (a `selector_family_empty` Warn fires when KSM matched but a kubelet family that a LIVE dimension actually reaches returned nothing — `promql.Selector.Reaches(q)` reads `queryDims` backwards, and since Harvest renders no matcher it is never blamed). Harvest series need NO `az` / `env` label.
- **Filtered-build rules for the service graph** (design D5 / D6 of push-request-filters-upstream). Because the topology is narrowed while the service-graph series are read in full, a build with any selector-level dimension active applies two rules that are **inert when unfiltered** (`sgResolver.filtered`): (1) an endpoint whose non-empty pod UID names a pod the request did NOT load resolves exactly as if the UID were empty — the `"://"` ladder can still reach a LOADED Service, `server="unknown"` still goes through the peer ladder (which needs a real client pod, so an out-of-scope caller is dropped), any other non-empty label becomes `external/<label>` via the D27 fallback, and an empty label drops the side; **a filtered build NEVER synthesises a pod**; (2) a series is ADMITTED only when both sides resolved AND at least one resolved id names loaded topology (`podByID` or an already-materialised `services` entry) — otherwise a per-series **journal** rolls back every side effect (external / service / `service-selects-pod` / route-chain / ingress `role` / `extReasons`) and the series contributes nothing to `pairs`, the RED join or the link markers. This is what keeps the out-of-scope estate from rendering as an external-to-external web, and what makes an out-of-scope peer render as `external/<label>` instead of a ghost pod. **Consequence:** under `?cluster=` the cross-cluster partner is an `external` node, not a real pod — "Cross-cluster edge representation" now requires BOTH clusters loaded.
- **An empty filtered result is a 200, not `outside_retention`.** The zero-pods + zero-nodes + healthy-`up{}` classification runs only when `sel.Active()` is false; a filtered build issues no `up{}` probe and returns an empty `elements` array with an empty `clusters` list. It also issues **no `traces_service_graph_*` queries at all** when the selector loaded neither pods nor services: admission (D6) requires a resolved endpoint in loaded topology and a service node can only come from `ServicesByNameNS`, so every series would be rejected — and those three queries are the one leg `queryDims` never narrows, so a mistyped `?namespace=` would otherwise scan the whole estate per request.
- **Request surface is `start`, `end`, `cluster`, `namespace`, `az`, `env`, `edge_type`, `prune`** — everything but `start` / `end` optional. `name`, `root`, `depth` and `direction` are **withdrawn** (BREAKING) and, like any unknown parameter, ignored without error: an old client receives the unanchored view. `GET /v1/clusters` is **removed** (BREAKING) together with the `cluster_discovery` query — the cluster list is the `clusters` field of any `/v1/graph` response. `graph.Scope` is `{Clusters, Namespaces, EdgeTypes, Inventory}`; `traverse` / `MaxTraversalDepth` / `Direction` / `Names` are gone.
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
  Service, so it MAY be cross-cluster — `may_cross_cluster: true`), and
  `pvc-to-netapp-aggr` (PVC → ONTAP aggregate from the Harvest `volume_labels`
  join; `may_cross_cluster: false` — the target belongs to no Kubernetes
  cluster; I/O on `data.metrics`: `read_ops`, `write_ops`, `read_latency_us`,
  `write_latency_us`, `read_bytes_per_sec`, `write_bytes_per_sec`, plus the
  declared ceiling `max_iops`, `max_bytes_per_sec`).
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
- **Deterministic response body.** The serialiser produces byte-identical output for the same `(window, filters, upstream-data)`, and every rendered upstream selector is a pure function of the sorted, de-duplicated parameter values (so `?az=b&az=a` and `?az=a&az=b` issue identical queries): node/edge slices MUST go through `graph.SortNodes`/`SortEdges`, `Graph.ClusterNames()` MUST sort, and the response body MUST NOT carry time-of-build or echo-of-input fields. Body shape is fixed at `{apiVersion, clusters, elements}`. Optional edge `data.metrics` (when present) is part of that contract — contributions are summed in ascending order and rounded to 6 significant digits so the wire form is order-independent. Don't add timestamps, random IDs, or unsorted map iteration to the response — golden tests will break.
- **IP addresses live on the typed `ipaddress` attribute, never in `labels`.** `PodNode.IPAddress()` carries `[pod_ip]` from `kube_pod_info` (when present). `K8sNode.IPAddress()` carries `[external_ip]` from `kube_node_status_addresses{type="ExternalIP"}` when present, falling back to `[internal_ip]` from `kube_node_status_addresses{type="InternalIP"}` when the node has no ExternalIP row (ExternalIP always wins over InternalIP regardless of upstream sample order; within each type a duplicate `(cluster, node)` sample resolves to the lexically-smallest address; address types other than `ExternalIP`/`InternalIP` are ignored); omitted only when neither type is present. The selector is the anchored alternation `kube_node_status_addresses{type=~"ExternalIP|InternalIP"}` — a fixed, request-invariant metric-selection contract, not a caller filter. `ServiceNode.IPAddress()` carries `[cluster_ip]` from `kube_service_info` (when present, omitted for headless `cluster_ip="None"`). `PVCNode`, `ExternalNode`, `NetAppAggrNode`, and `NetAppNode` always return nil. `host_ip` from `kube_pod_info` is intentionally dropped — it is the node's IP, surfaced via the node entry instead. The serialiser emits `data.ipaddress` (with `omitempty`); `labels.pod_ip`, `labels.host_ip`, `labels.external_ip`, `labels.internal_ip`, and `labels.cluster_ip` MUST NOT appear.
- **Cytoscape compound nodes are presentation-only — workload hierarchy plus storage chain.** `pkg/cytoscape` synthesises `type="cluster"` / `type="storage-cluster"` / `type="namespace"` / `type="application"` / `type="controller"` groups (all `labels={}`, no `ipaddress`, emitted in that tier order each sorted by id, before real nodes) and sets `data.parent` (`omitempty`) for `cluster > namespace > application > controller > pod` with **skip-absent-levels**, plus `cluster > namespace > [application >] {service, pvc}`, `cluster > node`, and `storage-cluster > netapp-node > netapp-aggr`. The **real** `type="netapp-node"` is the compound parent of its aggregates (via `labels.node`) — the one scoped exception to "relationships are edges, groups are synthesised". `external` nodes get no parent. Group ids are **path-encoded**. **NetApp types** (`NodeTypeNetAppAggr` id `netapp/<oc>/aggr/<aggr>`, `NodeTypeNetAppNode` id `netapp/<oc>/<node>`) belong to no Kubernetes cluster (`labels` carry `ontap_cluster`, never `cluster`) so they stay out of `clusters[]` and `?cluster=`. The PVC→aggregate relationship is the `pvc-to-netapp-aggr` edge (Harvest `volume_labels` join on PV name = `volume_name`); the pod→node relationship is `pod-to-node`. The `storageclass` node type and `pvc-to-storageclass` edge are **removed**; the claim's StorageClass name survives as `PVCNode.StorageClass()` / `data.storageclass`. Infra admission (D6, now transitive for NetApp): a K8s `node` is retained iff a pod is scheduled on it; a `netapp-aggr` iff an admitted PVC has a `pvc-to-netapp-aggr` edge to it; a `netapp-node` iff an admitted aggregate names it. `?name=` surfaces either NetApp type directly and an admitted aggregate always pulls its owning controller. See `pkg/build/netapp.go` and `docs/netapp-harvest-preconditions.md`.
- **OTLP tracing/logging is config'd by OTel env vars only** (`OTEL_EXPORTER_OTLP_*`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER`). No bespoke `--otlp-*` flags. Telemetry defaults to no-op when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset (zero export overhead, no background goroutines). Tracing MUST NOT alter response bodies — resource attrs and span IDs live on spans, never in JSON. `otelgin` is mounted on `/v1/*` only; `/livez`, `/readyz`, `/metrics`, and `/docs/*` are deliberately untraced. The auth middleware MUST NEVER log or attribute the presented `X-API-Key` value via either the local handler or the OTLP slog bridge.
- **Pod controller-owner attribute (D34).** Each `type="pod"` node carries a typed, nullable `owner` attribute — `data.owner = {kind, name}`, serialised with `omitempty` and **omitted entirely** when the pod has no controller owner (never empty strings). It lives on the typed attribute, **never inside `labels`** (which stay strict typological metadata) — same precedent as `ipaddress`. Surfaced via `graph.GraphNode.Owner() *graph.Owner` (nil for non-pods and ownerless pods). Resolved from `kube_pod_owner` with the **ReplicaSet skipped to its owning Deployment** via `kube_replicaset_owner` (a bare ReplicaSet with no Deployment owner stays `kind="ReplicaSet"`; other owner kinds surface verbatim). Both series are KSM defaults (no `--metric-labels-allowlist`) and OPTIONAL (absence degrades gracefully, no build failure). Resolution lives in `pkg/build/topology.go` (`resolvePodOwners`); the controller pick is deterministic (lexically-smallest `(kind, name)` on collision). No new node/edge type.
- **Pod `application` and `containers` attributes.** Each `type="pod"` node may carry two more typed, nullable attributes, both serialised with `omitempty` and **never inside `labels`** — same precedent as `owner` / `ipaddress`. (1) `data.application` (string) is the pod's ArgoCD Application, joined from the pod's **CONTROLLER** — ArgoCD stamps `argocd.argoproj.io/tracking-id` on the workload object it applies, never on the pods a controller spawns, and no `argocd_tracking_id` label is read off `kube_pod_owner` any more (BREAKING — see `docs/BREAKING.md`). The lookup key is the controller owner already resolved for `data.owner` (`(cluster, namespace, owner_kind, owner_name)`, ReplicaSet already collapsed to its Deployment) against one annotation family per kind — `kube_deployment_annotations` / `kube_statefulset_annotations` / `kube_daemonset_annotations` / `kube_replicaset_annotations` (bare RS only) / `kube_job_annotations` (identity label `job_name`, NOT `job`) / `kube_cronjob_annotations` — each carrying the same `annotation_argocd_argoproj_io_tracking_id` label as the service / PVC families and parsed as the segment **before the first `:`** of the tracking-id value (`<app>:<group>/<kind>:<ns>/<name>`; a value with no `:` is verbatim); per-controller collisions pick the lexically-smallest non-empty tracking-id. The ONE extra hop is **Job → CronJob** via `kube_job_owner` (`owner_kind="CronJob"` + `owner_is_controller="true"`), tried only when the Job carries no annotation of its own — the Kubernetes CronJob controller copies only `spec.jobTemplate.metadata` annotations onto its Jobs. That hop is **resolution-only**: `resolvePodOwners` never reads the index, so `data.owner` still reads `{kind:"Job", …}`. An owner kind with no KSM annotation family (`ReplicationController`, `Node`, any CRD controller) resolves no Application. Every family is OPTIONAL and degrades **per family** (`--metric-annotations-allowlist` is per-resource). All seven queries carry a **fixed selector** mirroring their reader's own discard — `kube_job_owner{owner_kind="CronJob",owner_is_controller="true"}` and `kube_*_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` — so an un-allowlisted family returns an EMPTY vector instead of one series per workload object, and `Topology.RawSeriesCount` for these seven counts matched (annotated / CronJob-controlled) objects, not all of them. On the **query-error** axis the seven split: `kube_replicaset_annotations` and `kube_job_annotations` are `fetchOptional` (log-and-continue — their cardinality accumulates with `revisionHistoryLimit` / Job history limits); the other four families and `kube_job_owner` stay abort-on-error `fetch`. Every degrade is **subtractive** — it removes an Application, never substitutes one — but a lost Application still reshapes the Cytoscape compound hierarchy (pods reparent, a sole-member `application` group node vanishes, an inheriting PVC re-inherits from a different mounter). Keeping it subtractive costs one gate: a degraded `kube_job_annotations` **suppresses the Job → CronJob hop** for that build (`topologyVectors.JobAnnotationsDegraded`, set only by `fetchOptionalTracking` on the swallowed-error path). The hop is gated on "this Job carries no annotation of its own", which an unread family cannot establish, so firing it would attribute a directly-managed Job's pod to its CronJob's Application — the one wrong-value degrade in the package. `kube_replicaset_annotations` needs no such flag: a bare ReplicaSet has no further ancestor to consult. Surfaced via `graph.GraphNode.Application() string` (`""` for non-pods and ArgoCD-less pods), resolved in `pkg/build/topology.go` (`resolvePodApplications` + `resolveControllerApplications` + `resolveJobCronJobOwners`). **Service and PVC nodes also carry `data.application`** (the `containers` attribute stays pod-only): resolved identically (segment before the first `:`, lexically-smallest on collision) from the `annotation_argocd_argoproj_io_tracking_id` label on `kube_service_annotations` / `kube_persistentvolumeclaim_annotations` (KSM's sanitised form of the `argocd.argoproj.io/tracking-id` annotation, gated on `--metric-annotations-allowlist`), via `resolveServiceApplications` / `resolvePVCApplications` (sharing the `argoAppName` + generic `resolveApplications` helper with the pod resolver). The PVC value is set at topology assembly; the **service** value is threaded into the connection-string resolver (`Topology.ServiceApplications` → `sgResolver.serviceApps`) since service nodes are materialised there. **PVC application inheritance (D13):** a PVC with **no** Application of its own additionally **inherits** the lexically-smallest Application among the pods that mount it (the `pod-mounts-pvc` bindings), via `pvcInheritedApps` in a post-PVC-loop pass at topology assembly (joins each binding's pod ID to the pod's already-resolved `Application()`). The PVC's **own** annotation always wins (the pass fills only app-less PVCs); the inherited value is baked onto `PVCNode.Application()` **before** `graph.NewGraph` freezes the nodes, so it is **indistinguishable** from an annotation-sourced value in `data.application` and drives the same `application` compound group. Because `pod-mounts-pvc` is intra-cluster and same-namespace, inheritance never crosses cluster/namespace; it is resolved over the full graph before projection (a `?cluster=`/`?namespace=`/`?name=` filter dropping the mounting pod does not change the PVC's app), and the min over the binding set is order-free (D6). No new query, metric, node, or edge type. So `Application()` now returns non-empty on `PodNode` / `ServiceNode` / `PVCNode`, and these `application` values additionally drive the `application` compound group for all three (`controller` groups stay pod-only). (2) `data.containers` (`[{name, image}]`) is the pod's container list from `kube_pod_container_info` (one series per container/image; a new 11th topology query rendered as `tlast_over_time(...)` so each series' value is its last-sample timestamp), ordered by `(name, image)` for determinism, empty-`image` series skipped, and — when a container reports more than one image in the window (each image a distinct series) — the **latest-seen image** kept (greatest last-sample timestamp; lexically-smallest image on an exact tie). The latest pick is reliable for near-now windows; for windows far from the real wall clock VM returns only one image-variant per container (see design.md D-A4), so a far-past window surfaces whatever single variant VM returns (never worse than a fixed pick). Surfaced via `graph.GraphNode.Containers() []graph.Container` (`nil` for non-pods and pods with no container info), resolved in `pkg/build/topology.go` (`resolvePodContainers`). The container read is a KSM default (no `--metric-labels-allowlist`); the controller-annotation families need the operator's `--metric-annotations-allowlist`. All are OPTIONAL (absence degrades gracefully, no build failure). No new node/edge type.
- **NetApp storage join is three independently-degrading hops** (`pkg/build/netapp.go`
  `resolveNetAppStorage`), all rooted at the same key — the PVC's `volumename`
  (the bound PV name) matched against the Harvest `volume_name` relabel:
  - **hop A `volume_labels`** — the SOLE source of storage *topology*: the
    `pvc-to-netapp-aggr` edge, the `netapp-aggr` / `netapp-node` entities, and
    the PVC `svm` label. An **info series**: its sample value is discarded, only
    its labels (`cluster` = ONTAP cluster, `node`, `aggr`, `svm`) are read.
  - **hop B `qos_{read,write}_{ops,latency,data}`** — the six measured I/O
    figures plus the volume's `policy_group`. Queried at **`{lun=""}`**: ONTAP
    collects a workload per LUN as well as per volume, and a LUN workload
    carries its FlexVol's relabelled `volume_name`, so an unfiltered read would
    double-count the claim. That matcher is a fixed **request-invariant
    metric-selection contract** (same class as the D30 sentinel and
    `condition="Ready"`), NOT a caller filter. Candidates are further scoped in
    Go to the picked aggregate's ONTAP cluster (and its `svm` when both sides
    resolve one) so a colliding `volume_name` across two filers cannot merge.
  - **hop C `qos_policy_fixed_max_throughput_{iops,mbps}`** — the declared
    ceiling, joined on the `(ontap_cluster, svm, policy_group)` triple recovered
    from hop B. The policy's identity label is read as `name` with a
    `policy_group` fallback (Harvest spells it differently across templates).
    `max_bytes_per_sec` is the **one** value not read verbatim: `mbps × 1048576`
    (`bytesPerMB`), so the ceiling shares the unit of `read_bytes_per_sec`.
  The hop split is load-bearing: a hop-B miss leaves a valid **measurement-less
  edge**, it never costs the claim its topology. A ceiling can NEVER appear
  without a measurement — structurally, because the policy key rides on a
  matched workload series (the ceiling is attached inside the `io != nil`
  branch, and `metricsDTO` deliberately does not let a ceiling set `filled`).
  Both `volumename` and `svm` are **plain labels**, set only when non-empty;
  `svm` is impossible without `volumename` and comes from hop A ONLY (hop B's
  own `svm` is a join key, never a fallback). **`volumename` ≠ `volume`**.
  All 15 Harvest/kubelet legs are OPTIONAL (log-and-continue). **Two** coverage
  warnings, each gated on its OWN family having been read:
  `slog.Warn("netapp_volume_join_miss", "count", n)` (hop-A miss or
  empty-`aggr`) and `slog.Warn("netapp_qos_join_miss", "count", n)` (edge drawn,
  no QoS match). No signal for a missing ceiling — a volume in no policy group
  is normal. Tests: `pkg/build/netapp_test.go` (incl. the 37-leg fan-out pin),
  `pkg/promql/queries_test.go` (`TestRender_QoSVolumeGranularity` pins
  `{lun=""}`),
  `internal/api/testdata/golden/with-netapp-storage-cytoscape.json`,
  `internal/integration` (`TestPVCNetAppHarvestJoin`).
- **K8s node `ready_status` attribute.** Each `type="node"` node may carry a typed, nullable `ready_status` attribute — `data.ready_status` (a string), serialised with `omitempty` and **never inside `labels`** — same precedent as `ipaddress` / `owner`. The value is one of `"Ready"`, `"NotReady"`, `"Unknown"`, derived from `kube_node_status_condition{condition="Ready"}` (a new topology query in the `ReadTopology` errgroup; the `condition="Ready"` selector is a fixed, **request-invariant metric-selection contract** — same class as the node-address `type` selector and the D30 sentinel — NOT a caller filter, and it is rendered ahead of any request-scoped matcher). The reader reads the `status` label of the **active** row (sample value `1`), matched **case-insensitively**: `true`→`Ready`, `false`→`NotReady`, `unknown`→`Unknown`. Status-label casing is NOT pinned by the KSM-shaped contract — stock kube-state-metrics lowercases it (`addConditionMetrics`→`strings.ToLower`), but an exporter that re-publishes the raw Kubernetes `v1.ConditionStatus` enum verbatim emits `True`/`False`/`Unknown`; both resolve (the reader canonicalises to lowercase at the read site). **Absence is distinct from `"Unknown"`**: `data.ready_status` is omitted entirely when the metric is absent, the node has no `condition="Ready"` series, or no row is active — `"Unknown"` is reserved for the genuine Kubernetes state where the kubelet has stopped reporting; the two MUST NOT be conflated (no defaulting missing data to `"Unknown"`). Surfaced via `graph.GraphNode.ReadyStatus() string` (`""` for non-nodes and nodes with no Ready data), resolved in `pkg/build/topology.go` (`resolveNodeReadyStatus`, keyed `(cluster, node)` like the IP/label joins); on the defensive multi-active tie the lexically-smallest `status` wins (determinism). The metric is a KSM default and OPTIONAL (absence degrades gracefully, no build failure). No new node/edge type.
- **Upstream backend routing (`add-multi-backend-query-routing`).** Every upstream call is dispatched through a `*promql.Router` over a validated, immutable `promql.Table` of named backends (URL + `families` + `zones` + resolved credentials). Full operator reference: `docs/upstream-backend-routing.md`.
  - **The seam is an OPTIONAL upgrade interface, not a widened one** (D1). `Querier.Instant` carries the query name but not the `Selector`, and the selector's `az` is what picks a backend — so `promql.QuerierSource` (`QuerierFor(sel) Querier`) was added alongside `Querier`, and `build.New` type-asserts its argument for it. `*Router` satisfies BOTH. Nothing in `pkg/promql`, `pkg/build`, or `pkg/kubegraph` changed signature, so a plain `Querier` (a `*Client`, a mock, `graph-api-gateway`) behaves exactly as before. Same shape as `build.BuildScopedRouteResolver`.
  - **`Builder.Build` resolves the querier ONCE** (D2) and threads it through `ReadTopology`, `ReadServiceGraph` and the retention `up{}` probe. That is what makes "a reload does not disturb a build in flight" structural: the bound querier closes over one table snapshot, and the build cannot probe a different set of stores than it read from.
  - **`queryFamily` is a second hardcoded table beside `queryDims`** — five families (`ksm`, `kubelet`, `harvest`, `servicegraph`, `probe`), exhaustive over the `Query` constants and guarded by `TestQueryFamily_EveryQueryListed`. `Family.AcceptsAZ()` is **derived** from `queryDims` (true iff every query in the family carries `dimAZ`), so routing and matcher rendering read the same fact from the same place.
  - **Zone-routability is declared per query beside the matcher table** (D4): `Family.AcceptsAZ()` is true iff every query in the family carries `dimAZ` (matcher AND route — `ksm`, `kubelet`) or the routing-only `dimAZRoute` (route, NO matcher — `harvest`). `servicegraph` and `probe` are `dimsNone`, so a `?az=`-scoped request still reaches EVERY backend serving them; narrowing them would drop edges whose series live in another zone's store, and the connectivity prune would then delete the pods on both ends. Routing composes **with** the PromQL matcher, never instead of it — the rendered query string is identical across every backend it is issued to, and `env` / `cluster` / `namespace` never route. **Harvest routes WITHOUT a matcher**: the thirteen legs go to the zone's `harvest` backend(s) as the bare unfiltered string, so a per-zone Harvest store is the zone boundary, Harvest series need no `az` / `env` label, `?env=` never touches Harvest, and a catch-all `harvest` backend under `?az=` (or any `?env=`) reads the whole estate narrowed by reference — a cross-zone `volume_name` collision then resolves to the lexically-smallest `(ontap_cluster, aggr)`, exactly as an unfiltered build already does.
  - **The merge de-duplicates by label-set fingerprint, and that is mandatory** (D5). Backends are concatenated in ascending `name` order and a series whose label set was already contributed is dropped. Several readers SUM across contributing series — the service-graph request/failure totals most visibly — so an undeduplicated merge multiplies an edge's `rate`/`error_rate` by the number of backends holding the series. Pinned end-to-end by `internal/integration` (`TestDuplicateServiceGraphSeriesDoesNotDoubleTheRate`). A duplicate with a DIFFERENT value keeps the first copy, is counted, and is logged at Debug — never escalated.
  - **Required legs fail closed** (D6). Any backend error fails the query, naming the backend; a partial fan-out is indistinguishable from a smaller estate and renders as a plausible, smaller, wrong graph. Already-optional legs (Harvest, kubelet, `kube_replicaset_annotations`, `kube_job_annotations`) keep degrading. A requested zone NO backend declares is the one non-error miss: empty vector + a Warn naming the family and the zone values.
  - **Parsing lives in `internal/config`, never in `pkg/`** (D3). `pkg/promql` receives an already-validated `Table` and stays free of file I/O and of any parser dependency. The file is read with `sigs.k8s.io/yaml` (already in the module graph via istio — promoting it to direct adds NO new module), so one struct with json tags accepts YAML and JSON alike. Parsing is STRICT: an unknown field is an error, because a misspelled `zone:` for `zones:` would silently turn a zone-scoped backend into a catch-all.
  - **Compatibility is an implicit table, not a branch** (D9). No `--backends-file` ⇒ one backend named `default` at `--prom-url` serving all five families with no zones. Every existing unit, component, golden and integration test therefore exercises the router in its degenerate configuration — the byte-identical claim is tested, not asserted.
  - **Reload is a ticker + atomic pointer swap** (D7), mirroring `--api-keys-file`. A file that fails to read/parse/validate is rejected WHOLESALE and re-reported every tick (the content digest is deliberately not advanced); an invalid file at STARTUP is fatal instead, since there is no previously-good table to fall back to. Clients are keyed by `(url, username, password)` and reused across a swap; retired ones get `CloseIdleConnections()`.
  - **Credentials never live in the routing file.** A backend names env vars (`usernameEnv` / `passwordEnv`); a literal `username`/`password` field is rejected, a half-declared pair is rejected, and a named-but-UNSET variable is a load failure rather than a quiet fallback — a typo would otherwise become 401s from one store, which under D6 fails the build pointing at the wrong thing. `Backend.String()` / `Table.String()` report `auth=true|false` and never a value.
  - **New self-metrics rather than new labels** (D10). `kube_state_graph_upstream_query_duration_seconds` / `..._failures_total` keep their `query`-only label sets; per-backend detail is `kube_state_graph_upstream_backends`, `..._backend_config_reload_total{result}`, `..._backend_query_failures_total{backend}`. `promql.Metrics` is likewise NOT widened — `promql.RouterMetrics` is a third optional upgrade interface, type-asserted. The backend name rides on the existing `prometheus.query` span as `kube_state_graph.backend`, omitted when unrouted.
  - **`/readyz` and the retention probe are multi-backend.** `promql.Prober` (a fourth optional upgrade) makes `Router.ProbeAll` ask every probe-serving backend concurrently under the one `--api-timeout` budget WITHOUT cancelling on first failure — a probe that stopped early could only ever name one backend. The 503 body carries `promql.ProbeError.Failed`: operator-chosen backend NAMES only, never a URL/host/IP, since `/readyz` is unauthenticated. The retention `up{}` probe rides the build's own bound querier, so a backend that did not answer suppresses the `outside_retention` classification (an empty graph stays an empty graph).

- **No configurable metric-name prefix.** `--metric-prefix` / `KSG_METRIC_PREFIX` / `Renderer.Prefix` are removed. Every series is queried at its bare name (`promql.Render(q, window)`). A deployment whose KSM series ARE prefixed silently returns an empty graph — see `docs/BREAKING.md`. The D29 endpointslice → service join still reads `kube_endpointslice_labels{label_kubernetes_io_service_name}`, which KSM only emits when `--metric-labels-allowlist=endpointslices=[kubernetes.io/service-name]` is set. The metric-name suffix and the label-name set per series are a fixed contract any compatible exporter MUST honour.

### Reusable `pkg/` graph engine (D32)

The graph engine lives under `pkg/` so other Go modules can import it in-process
(no HTTP, no JSON round-trip); `internal/api` is a thin HTTP / auth shell over it:

- `pkg/graph` — `Graph`, the sealed `GraphNode` + seven node types, `Edge`,
  `Project`, `Scope` / `NewScope`, `View`, `SortNodes` / `SortEdges`, `EdgeTypes`.
  `Graph` carries **no adjacency index** — the `Forward` / `Reverse` maps existed
  only for the withdrawn `?root=&depth=` traversal, and every surviving consumer
  (projection, the connectivity prune, serialisation) scans `Edges` once.
- `pkg/build` — `Builder` + `Build`; topology / service-graph / NetApp readers. Takes a
  `build.Options{APITimeout, LabelKeys}` and a no-op-tolerant `build.Metrics`
  interface — **not** `internal/config` / `internal/observability`, whose
  couplings were broken so the package is externally importable.
- `pkg/promql` — `Querier`, `Render(q, window, keys, sel)`, `Selector`,
  `LabelKeys`, `Client`, and a no-op-tolerant `promql.Metrics` interface.
- `pkg/clock`; `pkg/cytoscape` — `Serialise(g, view) Body` plus the Cytoscape DTO.
- `pkg/kubegraph` — the convenience facade: `Engine.BuildFromValues(ctx,
  url.Values) (cytoscape.Body, error)` folds parse → build → project → serialise
  into one call. `kubegraph.ParseValues(url.Values) (Request, error)` is the
  **single** request parser — `Request{Start, End, Scope, Selector}`, shared by
  `internal/api`'s handler and the facade, so the `/v1/graph` request contract
  cannot drift between the server and an embedded consumer. `Engine.Build` takes
  the `promql.Selector`; `Options` carries `LabelKeys`.

`pkg/` packages MUST NOT import `internal/*` — Go's internal rule would block any
external module from importing the engine. Metrics and OTLP tracing are injected
with no-op defaults, so an embedder does not inherit ksg's `kube_state_graph_*`
self-metrics; the concrete `*observability.Metrics` satisfies
`build.Metrics` / `promql.Metrics` structurally via wrappers in
`internal/observability/adapters.go`. The first external consumer is
`graph-api-gateway` (its `embed-ksg-graph-engine` change).

### Sealed graph types

`graph.GraphNode` is a sealed interface (`isGraphNode()` unexported). Concrete
types: `PodNode`, `K8sNode`, `PVCNode`, `ServiceNode`, `ExternalNode`,
`NetAppAggrNode`, `NetAppNode`. All
expose `ID()`, `Name()`, `Type()`, `Labels()`, `IPAddress()`, `Owner()`,
`Application()`, `Containers()`, `ReadyStatus()`, `Health()`, `Usage()`,
`StorageClass()`. Serialisation
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
policy name (`data.storageclass`).

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
| Property | `pkg/graph/property_test.go` | None. Random multi-cluster graphs → invariants (orphan edges, pruned ⊆ inventory, ID uniqueness). |
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

- **Conversational output is written in Traditional Chinese (繁體中文).** This
  covers the two artifacts of a Claude Code session: the plan file
  (`~/.claude/plans/*.md`) and the explanatory prose in chat replies. Code,
  identifiers, API names, CLI commands, commit-type keywords (feat/fix/…) and
  error strings stay verbatim in English. This rule does **NOT** apply to
  anything persisted in the repo — OpenSpec artifacts (`proposal.md`,
  `design.md`, `tasks.md`, `spec.md`), code comments, commit messages,
  `CLAUDE.md` itself, and all other docs stay in English, consistent with the
  existing codebase.
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
