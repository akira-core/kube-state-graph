## Context

`resolveUnknownServerPeer` (`pkg/build/servicegraph.go`) is the "Unknown-server peer-label enrichment"
carve-out: a `server="unknown"` series whose client resolved to a real topology pod is recovered from
`client_net_peer_name` / `client_server_address`. Its classification ladder is entirely *in-cluster*
(`.svc` DNS → bare short name → ClusterIP literal). A global / ingress FQDN — `api.example.com` — is
none of those, so it becomes `external/api.example.com`: a dead-end node hiding a real in-cluster
dependency reached through an Istio ingress Gateway.

`poc/route2a` (a separate Go module, deliberately isolated so `istio.io/istio` never touched this repo)
has a working, benchmarked engine for exactly this question. Its entry point already takes what we
have and returns what we need:

```go
// poc/route2a/internal/rangequery/rangequery.go
func (d Deps) Resolve(ctx context.Context, st store.Store, host, path, ip string, port int,
                      t0, t1 time.Time) ([]VersionResolution, error)
// VersionResolution{From, To, Gateway, Cluster}
// Cluster == "outbound|8080||reviews.prod.svc.cluster.local"
```

Constraints this design must respect, all from `CLAUDE.md`:

- `parseServiceGraph` is a **pure function** — no ctx, no window, no querier. Determinism (D6) is
  load-bearing for the golden tests.
- **No filters pushed to PromQL**; the service-graph selector is a fixed, request-invariant contract.
- `pkg/` MUST be externally importable; `graph-api-gateway` embeds `pkg/kubegraph` → `pkg/build`.
- "Don't import `k8s.io/client-go` into the API server"; "don't add dependencies casually".
- `labels` is strict `map[string]string`; typed facts live on typed attributes.

## Goals / Non-Goals

**Goals:**

- A `server="unknown"` endpoint whose peer FQDN is a global/ingress hostname resolves, **over the
  request's own time window**, to the Kubernetes Service the Istio Gateway + VirtualService config
  actually routed it to.
- The resolved node is an **ordinary `service` node** with an ordinary `pod-calls-service` edge —
  indistinguishable in the response from any other D29-resolved service node.
- The feature is **off by default** and, when off, produces byte-for-byte identical output.
- Every failure mode (engine disabled, store error, no gateway, no route, no listener on the port,
  timeout) degrades to **today's `external` node**. The build never fails because of route resolution.
- The heavy dependencies must never be linked into an embedder of `pkg/kubegraph`.

**Non-Goals:**

- Writing to the config store. kube-state-graph is strictly **read-only**; the metadata-exporter repo
  owns watch/ingest.
- DestinationRule **subsets** and EndpointSlice resolution (the POC does not do them either).
- Surfacing the destination **port** or **subset** on the graph — parsed, then discarded in v1.
- Extending D29 `resolveConnString` (the `"://"` path). Explicitly out of scope.
- Any performance work: caching, concurrency, or deduplication of resolutions. Correctness first.
- An HTTP `path` dimension. There is no path label on the metric; `path` is fixed to `"/"`.

## Decisions

### D0 — Linking `k8s.io/client-go` does NOT violate the "no Kubernetes API" rule; the rule's wording is amended

`CLAUDE.md` says "Don't import `k8s.io/client-go` or any Kubernetes API into the API server. All
cluster facts come from VictoriaMetrics. Informers were considered and rejected — see D1 / D16."
`istio.io/istio` transitively links `k8s.io/client-go`, so this needs settling head-on rather than
being waved through.

Read the rule's own justification (archived `add-k8s-pod-graph-api` design, D1 and D16):

> **client-go informer for topology:** informers expose only the *current* cluster state and cannot
> answer historical time-range queries — the API's contract. Multi-cluster makes this worse: would
> need N watch streams plus per-cluster RBAC.

The rule forbids a **data source** — a live connection to the Kubernetes API, and watches over it —
not a Go package. Its three reasons are: informers only know "now" and cannot answer
`?start=&end=`; multi-cluster would need one watch stream and one RBAC binding per cluster; all
cluster facts should come from a single historical upstream.

The route engine violates **none** of them, and in fact embodies the same philosophy:

- It reads **versioned historical config from ClickHouse over the request's own `[t0, t1)`** — exactly
  the time-range query an informer cannot answer.
- It **never dials an apiserver, opens no watch, needs no kubeconfig and no RBAC.** istio is used
  purely as a **library**: Gateway/VirtualService protos in, an in-memory `RouteConfiguration` out.
  The POC's translator hand-builds a `model.Environment` over a memory config store and a
  `memregistry` service discovery precisely so that no apiserver client is ever constructed.
- `k8s.io/client-go` is linked only because istio's `model` package references Kubernetes **type
  definitions**. Not one line of it dials anything.

So the rule's *letter* is touched and its *reasons* are not. The rule is therefore **restated** (task
10.1) to say what it always meant:

> Don't talk to the Kubernetes API from the API server — no client-go clients, no informers, no
> watches, no kubeconfig, no per-cluster RBAC. All cluster facts come from VictoriaMetrics (topology,
> service graph) or the versioned config store (Istio routing). Linking a library that transitively
> vendors Kubernetes *types* is not a violation; **constructing a Kubernetes client is.**

*Alternative rejected:* keep the literal rule and shell out to `router_check_tool` **and** an external
route-query service, so no istio code is linked. That trades a compile-time dependency for a second
deployment and a second failure domain, to satisfy the letter of a rule whose purpose is already met.

### D1 — The interface lives in `pkg/build`; the engine lives in `pkg/route`; `pkg/build` MUST NOT import `pkg/route`

This is **dependency hygiene, not the D0 rule** — the two are independent, and conflating them
obscures both.

`pkg/` exists to be imported in-process by other modules; `graph-api-gateway` already embeds
`pkg/kubegraph` → `pkg/build`. If `pkg/build` imported `pkg/route`, every such embedder would inherit
`istio.io/istio`, `clickhouse-go/v2`, and `envoyproxy/go-control-plane` — a large binary, a
`k8s.io/client-go` it has no use for, and istio version coupling (D-risk below) — for a feature it
never calls. That is the cost this rule prevents, and it is worth preventing on its own merits.

```go
// pkg/build/routeresolve.go — the ONLY thing pkg/build knows about routing.
type RouteRequest struct {
    CallerCluster string   // the client pod's own cluster — family key + tie-break ONLY (D10/D11)
    Host          string   // peer FQDN, port split off
    Path          string   // "/" today
    Port          int      // ingress listener port; selects the RouteConfiguration
    IPs           []string // from client_dns_answers; REQUIRED — the prescan never emits an IP-less request (D6)
    Start, End    time.Time // the build's own window
}
type RouteDestination struct {
    Cluster   string // the engine-selected ingress cluster C (D10/D11) — the parse anchors on it
    Namespace string
    Service   string
    Port      uint32 // parsed, unused in v1
    Subset    string // parsed, unused in v1
}
// RouteOutcome: "hit" | "no_gateway" | "no_listener_on_port" | "no_route"
//             | "no_ingress" | "ambiguous_ingress_cluster".
// A typed outcome (not a bool) because D5 requires the caller to log a
// mis-derived port (no_listener_on_port) distinctly from an ordinary miss,
// and D10 adds two pre-window outcomes (no candidate ingress cluster /
// candidates that cannot be reduced to one).
type RouteResolver interface {
    ResolveRoute(ctx context.Context, req RouteRequest) (RouteDestination, RouteOutcome, error)
}
```

`build.Options.RouteResolver` (mirrored on `kubegraph.Options`) defaults to nil = feature off. Only
`cmd/kube-state-graph` imports `pkg/route` and constructs the concrete resolver. Go links only what is
imported, so `graph-api-gateway` — which imports `pkg/kubegraph` → `pkg/build` — never links istio or
ClickHouse. They appear in the module graph, not in its binary.

Note this is a *containment* rule, not a prohibition: `cmd/kube-state-graph` links the engine happily,
and any embedder that actually wants route resolution can import `pkg/route` itself and pass the
resolver in. The rule only ensures nobody pays for it by accident.

*Alternative rejected:* call an external route-query HTTP service. Keeps `go.mod` pristine but adds a
network hop, a second deployment, and a second failure domain for what is a read of a database we can
read directly. The user's requirement was explicit: query ClickHouse and build the RouteConfiguration
**in-process**.

*Alternative rejected:* put `pkg/route` in a separate Go module inside this repo. `cmd/` could then not
import it without a `require` anyway, so it buys nothing.

### D2 — Prefetch into an index; never thread I/O into `parseServiceGraph`

Route resolution needs a `context.Context` and the time window. `parseServiceGraph(vec, topology)` has
neither, deliberately: its purity is what makes the graph a deterministic function of the upstream data
(D6). Threading a resolver into it would put network I/O inside the parse and make edge materialisation
depend on I/O ordering.

Instead, use the pattern `Topology` itself is: **fetch outside the parse, pass an index in.**
`ReadServiceGraph` *does* have ctx / window / end:

1. `collectRouteQueries(vec, topology)` — a **pure** prescan returning the `RouteRequest`s for
   endpoints that would hit an external fallback in `resolveUnknownServerPeer`.
2. Resolve them (I/O; serial; each bounded by `--route-resolve-timeout`; errors and misses recorded,
   never fatal) into `map[routeKey]RouteDestination`.
3. `parseServiceGraph(vec, topology, routeIndex)` — still pure, still deterministic.

A two-arg `parseServiceGraph(vec, topology)` wrapper delegating with a nil index keeps the ~7 existing
direct call sites in `pkg/build/servicegraph*_test.go` compiling.

The prescan and the resolver must agree on *which* endpoints are candidates, or the index will be
queried for keys the parse never looks up (harmless) or, worse, miss keys it does (a silent
regression). To make drift impossible, the classification ladder inside `resolveUnknownServerPeer` is
extracted into one pure helper, `classifyPeerHost`, used by both. The prescan collects exactly the
branches that terminate in `r.external(value)`.

### D3 — The engine is consulted at ALL THREE external branches, not just "not k8s DNS"

This is the single most counter-intuitive point in the change, and getting it wrong makes the feature a
silent no-op for its own motivating case.

`classifyK8sDNS` splits the host on dots and treats a **3-label** name as the headless pod form
`<pod>.<service>.<namespace>`. So `api.example.com` is *successfully* classified — as service
`example` in namespace `com` — and only *then* fails, in `resolveServiceLevel`, because the anchor
cluster holds no such Service. A global FQDN therefore reaches the external fallback through
`unknown_server_peer_anchor_lacks_service`, **not** through `unknown_server_peer_not_k8s_dns`.

The rule is stated positively instead: the route engine is the **last step before `r.external(value)`**,
wherever that call is reached. Today that is three sites:

| Existing reason | Reached by |
|---|---|
| `unknown_server_peer_not_k8s_dns` | host matched no grammar (e.g. a 4+-label name) |
| `unknown_server_peer_ip_literal_no_match` | IP literal with no ClusterIP hit in the anchor cluster |
| `unknown_server_peer_anchor_lacks_service` | **the global-FQDN path** — classified, then unresolvable |

### D4 — The destination is resolved through the existing `resolveServiceLevel`, unchanged

The engine returns an Envoy cluster string, `outbound|<port>|<subset>|<svc>.<ns>.svc.cluster.local`.
Parse it to `(namespace, service, port, subset)` and hand `(anchorCluster, ns, svc)` to the **existing**
`resolveServiceLevel`. That inherits, for free and consistently with every other path: the
anchor-membership test, one `ServiceNode` via `materializeServiceNode`, the cross-cluster
`service-selects-pod` family fan-out, and — because edge type is target-driven — a `pod-calls-service`
edge. A `resolveServiceLevel` miss still falls to `external`.

Consequence: **no new node type, no new edge type, no new node attribute, no new `labels` key, no
PromQL/selector change.** The destination `port` and `subset` are parsed and discarded — `labels` is
strict `map[string]string`, and a typed attribute is a separate change.

### D5 — Listener port: peer-address `:port` → optional label → default 443

The updated POC is no longer hardwired to HTTP :80. `translate.ScopedInput` carries a `Port`, and
`routeConfigNameFor(gwCfg, port)` mirrors istio's unexported `gatewayRDSRouteName`: it finds the
Gateway **server listening on that exact port** and names the RouteConfiguration `http.<port>` for a
plain HTTP server, or `https.<port>.<portName>.<gwName>.<gwNamespace>` for a TLS-terminated HTTPS one.
The port is therefore a required input we must supply.

Precedence:

1. The `:port` in the peer-address value. `stripPeerAddressPort` currently **discards** it; it now
   returns it. (Every existing caller keeps using the host alone, so behaviour is unchanged.)
2. A new **optional** dimension, `client_server_port` / `client_net_peer_port` — both are OTel semconv
   names, so they ride along with `client_dns_answers` at no extra cost to the exporter config.
3. Default **443**.

443 rather than 80 because of how a wrong port fails. When no server listens on the requested port (or
it is TLS **passthrough** / TCP), `routeConfigNameFor` returns `ok=false`, the translator returns an
**empty** RouteConfiguration, `router_check_tool` matches nothing, and the endpoint falls back to
`external`. The failure mode is "no cluster at all", never "the wrong cluster". Now:

- A Gateway declaring **both** a real `:80` HTTP server and a `:443` HTTPS server over the same hosts
  binds the same VirtualServices to both listeners and generates the same route table — either port
  resolves to the same destination, so the choice is harmless.
- But a Gateway declaring only `:443`, or whose `:80` server is an `httpsRedirect` stub (a very common
  pattern), has **no routable HTTP listener on :80** — an 80 guess misses.

So 443 is the safer default, and the resolver records `route_engine_no_listener_on_port` as a
**distinct** external-fallback reason from `route_engine_miss` (no route matched), so a mis-guessed
port is diagnosable in the logs rather than blending into ordinary external traffic.

Note `gwresolve` is port-agnostic (it matches `server_hosts` regardless of port), so the Gateway is
still found — only the RDS route-name lookup fails. And `matchcheck.Query` stays `{Host, Path}`: the
port selects *which* RouteConfiguration is built; it is not part of the match input.

### D6 — `client_dns_answers` is REQUIRED; no IPs ⇒ the engine is never consulted (config_only removed)

*(This decision supersedes its original form, which made the label optional and fell back to a
`config_only` mode — resolving the host over ALL the caller cluster's Gateways via a
`LoadConfigWindow(ctx, cluster, t0, t1)` store read and `memwindow.GatewaysLiveAt`. That mode is
removed entirely. Two reasons. First, a host-only match over one guessed cluster's gateways answers
"which Gateway is configured to accept this host", not "where did this traffic land" — a config that
exists in several clusters made the answer arbitrary. Second, and decisive under D10: without an IP
there is no signal at all for ingress-cluster selection — the caller's own cluster was only ever a
guess, and the motivating case is precisely the one where that guess is wrong.)*

The destination IPs from `client_dns_answers` are now a **hard precondition** for consulting the
engine. The prescan (`collectRouteQueries`) skips any endpoint whose parsed answer list is empty —
the engine is never invoked, no store read happens, and the endpoint falls to today's `external`
node via the pre-existing fallback. The parse records the distinct diagnostic reason
`route_engine_no_ip` (only when a resolver is configured, so the log tells "engine on but this
endpoint carried no IPs" apart from "engine off"). Graph output is byte-for-byte identical to the
engine-off case for such endpoints.

With IPs present, the engine first selects the ingress cluster (D10), then runs the ClickHouse
**3-hop** (`has(ingress_ips, ip)` → ingress Service selector → `hasAll(pod_labels_kv, sel)` →
ingress Deployment pod labels L → `hasAll(L, selector_kv)` → candidate Gateways) within that one
cluster and disambiguates the host **among those candidates** (`gwresolve.ResolveAmong`).

`LoadConfigWindow` is deleted from `store.Store` and its ClickHouse implementation — the repo rule
forbids production code kept only for tests, and no production path reads it any more.
`memwindow.GatewaysLiveAt` stays: it is a pure in-memory helper with its own unit coverage.

### D7 — The route store is read-only, `cluster`-scoped, and a fixed schema contract

The metadata-exporter repo owns watch/ingest and writes the versioned config. kube-state-graph **only
reads**: the port drops `CreateSchema` and all four `*Batch` inserters from the POC's `store.Store`.
The updated POC deliberately made `BackendFQDN` / `ParseBackendHost` / `ParsePorts` decode "a real
metadata-exporter `spec` blob", which is exactly this contract — they port verbatim.

The POC schema assumes a single Kubernetes cluster. kube-state-graph is multi-cluster by construction
(every node id is `<cluster>/…`), so every table gains a **`cluster` column** and every *window*
query a `cluster = ?` predicate — bound to the **engine-selected ingress cluster** (D10), not to the
caller's cluster. The **one deliberate cross-cluster read** in the whole store is the selection
probe, `ClustersWithIngressIP(ctx, ip, t0, t1) ([]string, error)`: the distinct clusters that had an
ingress LB Service version carrying `ip` overlapping `[t0, t1)`. It follows the same no-FINAL
semantics as the window loads (select the dedup envelope `cluster, namespace, name, valid_from,
valid_to, ingest_seq`; `valid_to` never filtered in SQL except under pruned mode; client-side
`dedupLatest` then the `t0 < valid_to` overlap check; then distinct clusters, **sorted** for
determinism). It scans `service_versions` without a cluster predicate — bounded by the small number
of ingress LB Service versions and the `valid_from < t1` predicate:

```
service_versions(cluster, namespace, name, valid_from, valid_to,
                 ingress_ips Array(String), selector_kv Array(String), spec_json, ingest_seq)
deploy_versions (cluster, namespace, name, valid_from, valid_to,
                 pod_labels_kv Array(String), ingest_seq)
gw_versions     (cluster, namespace, name, valid_from, valid_to,
                 selector_kv Array(String), server_hosts Array(String), spec_json, ingest_seq)
vs_versions     (cluster, namespace, name, valid_from, valid_to,
                 bound_gateways Array(String), spec_json, ingest_seq)
```

Note there is **no `rev` column**: that was the POC corpus's synthetic oracle field. The production
tables identify versions by `resource_version` + `ingest_seq` envelope columns, and the reader never
needed a rev — selecting one would fail with Unknown column against production.

Four production-shape behaviours distinguish the reader from the POC's original loader assumptions
(ported from the POC's own production-compat pass):

1. **No `FINAL`; the no-FINAL read pattern instead.** The exporter "closes" a version by REWRITING
   the previous open row (same ORDER BY key, higher `ingest_seq`, `valid_to` pulled in from the
   far-future sentinel); until merges collapse the pair, both rows are visible, so the
   ReplacingMergeTree rule must apply at read time. FINAL does that server-side but costs ~10x
   lookup latency on the POC bench and silently depends on the server not moving WHERE into
   PREWHERE ahead of the merge. The reader instead: (a) keeps ONLY immutable predicates in SQL
   (cluster, join keys, `valid_from`) — `valid_to` must NEVER be filtered in SQL, or the closing
   rewrite is dropped pre-dedup and its stale sentinel twin wins unopposed; (b) dedups client-side
   per version slot `(cluster, namespace, name, valid_from)` keeping max `ingest_seq`
   (`dedupLatest`); (c) applies the `t0 < valid_to` overlap check AFTER dedup, in Go. Collapses
   are counted (`CollapsedRows`) — a rewrite-compatible-mode diagnostic and, under pruned mode, a
   writer-uniqueness alarm.
1b. **Pruned mode (`--route-store-unique-rows`, default OFF).** When the exporter guarantees one
   physical row per version (closeMode=update, after historical convergence), the opt-in restores
   the `valid_to` predicate in SQL so closed versions are pruned server-side instead of fetched
   and discarded. NEVER enable against the default rewrite-close exporter: the prune drops the
   closing rewrite before dedup and the stale sentinel row resurrects — the integration suite
   carries a negative test (`TestUniqueRowsAgainstRewriteWriterResurrectsStaleRow`) demonstrating
   exactly this, including that the `CollapsedRows` alarm stays silent (which is why the mode is
   an explicit operator assertion, not autodetected). In pruned mode `CollapsedRows > 0` means
   the uniqueness guarantee broke — wire it to an alert (follow-up).
1c. **Time operands are `toDateTime64` literals (`dt64Lit`), never `?` binds.** clickhouse-go
   interpolates `time.Time` binds at second precision (`toDateTime`): milliseconds are truncated
   (a `valid_from < t1` boundary error for sub-second windows) and the 2200 far-future sentinel
   SATURATES past 32-bit DateTime's 2106 ceiling. The integration seed writes times as string
   literals for the same reason.
2. **`spec_json` parses with `protojson.UnmarshalOptions{DiscardUnknown: true}`.** Production
   spec_json is the API server's CR JSON verbatim; its field set follows the CLUSTER's Istio CRD
   version, not the reader's compiled istio.io/api. A cluster upgrade adding a spec field must not
   fail the whole historical query (istiod itself parses CRs allow-unknown). The trade-off — a
   discarded field that genuinely affects routing silently diverges — is the already-accepted §9.4
   version-coupling risk.
3. **Bare gateway refs bind.** `bound_gateways` carries `spec.gateways[*]` VERBATIM, and Istio
   accepts both `<ns>/<name>` and the bare `<name>` (shorthand for a same-namespace gateway, common
   in practice). The store loads a superset (`hasAny` over both forms); `memwindow.boundTo` applies
   the exact predicate (bare matches only when VS ns == gateway ns). Matching only the qualified
   form silently drops every bare-bound VS's routes — the most covert of the four gaps.
4. **The reader never selects `rev`** (above).

Interval semantics are the POC's: `valid_from < t1 AND t0 < valid_to` for the window load, and
`valid_from <= t <= valid_to` for a point. This is a **fixed contract** in the same sense as the
kube-state-metrics metric/label contract (D26): ksg validates the expected columns exist at startup
and **fails fast** on drift, rather than silently returning empty windows.

### D8 — `router_check_tool` as a native binary, no docker, no Envoy

The POC's `matchcheck` prefers a native binary and falls back to running the Envoy tools image under
docker. The port **drops the docker fallback**: the binary path comes from `--router-check-bin`
(default `/usr/local/bin/router_check_tool`), and the image gets it via a multi-stage
`COPY --from=envoyproxy/envoy:tools-v1.34-latest`. No Envoy process, no istiod process, no docker at
runtime.

The `__routecheck_unmatched_sentinel__` trick ports verbatim: setting an expected cluster no real route
can equal forces the validator to report every case as a mismatch and print the *actual* matched cluster
in its `--details` output, turning a validator into a resolver. `--disable-deprecation-check` is
required because istiod-translated RouteConfigurations carry deprecated fields.

*Alternative rejected:* reimplement Envoy's route matching in pure Go. Removes the exec entirely and
would be far faster, but re-derives RDS semantics (prefix/exact/regex, header and query matchers,
weighted clusters, virtual-host domain precedence) with no oracle in production. The POC's whole
correctness argument rests on `router_check_tool` being Envoy's own matcher.

### D9 — Off by default; every failure degrades to today's `external` node

`--route-store-dsn` empty ⇒ `RouteResolver` is nil ⇒ the prescan collects nothing and
`resolveUnknownServerPeer` behaves exactly as today. This is both the default and the regression net:
the full existing suite, golden files included, must pass unchanged with the feature off.

With the feature on, every failure — store error, no candidate ingress cluster, an ambiguous
candidate set, no gateway for the host, no route matched, no listener on the port, per-endpoint
timeout — resolves to the **existing** `external/<raw peer value>` node, with a distinct
`noteExternal` reason (`route_engine_no_ip`, `route_engine_no_ingress`,
`route_engine_ambiguous_ingress_cluster`, `route_engine_miss`, `route_engine_no_listener_on_port`,
`route_engine_error`). Route resolution can never fail a build.

### D10 — IP + family-first ingress-cluster selection (`pickIngressCluster`, a pure function)

The caller's cluster stops being the store scope. Instead, the destination IPs pick the ingress
cluster **C**, and the caller cluster contributes only its **family key** and a collision
tie-break. Two clusters are in one family iff `build.ClusterFamilyKey` (the existing digit-run→`0`
sentinel rule, now exported) maps their names to the same key — the same family rule the D29
`service-selects-pod` fan-out already uses.

Per destination IP, the store probe (D7) yields the candidate set
`G = {clusters that had an ingress Service with that IP overlapping the window}`, and
`F = {g ∈ G : ClusterFamilyKey(g) == ClusterFamilyKey(caller)}`. Selection:

1. `|F| == 1` → C = that cluster.
2. `|F| > 1` → C = caller if caller ∈ F, else **ambiguous** (collision-safe default).
3. `|F| == 0 && |G| == 1` → C = that cluster (a shared ingress under a cross-family name).
4. `|F| == 0 && |G| > 1` → C = caller if caller ∈ G, else **ambiguous**.
5. `|G| == 0` → **no ingress**.

Multi-IP (multi-A-record answers): each IP independently yields its own C_i, and **all must agree**.
All IPs no-ingress → `no_ingress`; any IP ambiguous, any disagreement between picked clusters, or a
mix of no-ingress and picked → `ambiguous_ingress_cluster`. Candidate sets and windows are **never
unioned across clusters** — a window mixing two clusters' configs would let one cluster's Gateway
match another cluster's VirtualService, which is exactly the wrong-answer class this feature must
never produce. (The known cost: DNS answer sets that routinely include off-mesh IPs over-degrade to
external — a watched follow-up, and always "no answer", never a wrong one.)

`pickIngressCluster(caller string, perIP [][]string)` is a **pure function** in `pkg/route`
(table-testable; ClickHouse's only job is answering "ip → []cluster"). Once C is locked, the entire
pipeline is single-cluster: `LoadTrafficWindow(ctx, C, ip, t0, t1)` per IP (same-C union is fine) →
memwindow → gwresolve → translate → matchcheck. No later hop may change cluster.

The two selection misses surface as new `RouteOutcome`s — `no_ingress` and
`ambiguous_ingress_cluster`. Both occur **before** any window is loaded, so the resolver's
`outcomeRank` miss-folding (a per-segment concern) is untouched.

### D11 — `RouteRequest.CallerCluster`; `RouteDestination.Cluster`; the parse anchors on C

`RouteRequest.Cluster` is renamed `CallerCluster` to say what it now is: an input to the family key
and the tie-break, never a store scope by itself. The prescan's dedupe key (`routeKey`) keeps the
same five dimensions — caller cluster, host, path, port, joined IPs — since two callers in different
clusters may legitimately resolve the same host to different ingress clusters.

`RouteDestination` gains `Cluster` — the locked C. On a hit the parse resolves through the same
`resolveServiceLevel(dest.Cluster, ns, svc)` as every other path (anchor-membership test in C, one
service node in C, family-wide `service-selects-pod` fan-out anchored at C — all existing
machinery, unchanged). A hit whose destination Service the cluster C does not hold in topology falls
external with reason `route_engine_dest_cluster_lacks_service` (renamed from
`…_anchor_lacks_service`; log-only contract).

### D12 — `pod-calls-service` becomes `may_cross_cluster: true`

With the service node anchored at C, a route-engine hit where C ≠ caller cluster produces a
`pod-calls-service` edge whose endpoints live in different clusters. The edge-type registry entry
(`pkg/graph/registry.go`) therefore flips to `MayCrossCluster: true`, with the description
distinguishing the two sources: the D29 connection-string path still always materialises the service
node in the caller's own cluster (intra-cluster by construction), while a route-engine-resolved
endpoint anchors on the engine-selected ingress cluster, which may be a family sibling. Per-edge
cross-cluster status is still derived by the D9 rule (comparing resolved source/target
`labels.cluster`), so intra-cluster edges keep counting as `cross_cluster="false"` — the flag is a
MAY, not a state. `edge-types.json` (golden) and the registry unit tests update with it; no other
golden may change.

### D13 — the ingress-IP probe is memoised per build (optional upgrade interface)

The D10 ingress-cluster probe (`store.ClustersWithIngressIP`) is a pure function of `(ip, start,
end)`, and `start`/`end` are constant across one build's keys. But `ResolveRoute` runs it once per
`(key, ip)`: keys sharing a destination IP — the same host called from several caller clusters, or
several hosts behind one LB IP — each repeat the identical cross-cluster ClickHouse read (`N_keys ×
N_ips` reads where a handful of distinct IPs would do). That probe is the ONE query with no
`cluster` predicate (its whole job is cross-cluster), so the redundancy is the most expensive to
repeat.

The fix memoises the probe **per build**, keyed `(ip, start.UnixMilli, end.UnixMilli)`. The scope is
one build, not the resolver instance: `*route.Resolver` is shared across concurrent builds and must
stay stateless — a cache on it would grow unbounded (a leak) and need locking. The prescan loop
(`resolveRouteQueries`) is where all keys of one build share `(start, end)` and run serially, so the
scope needs no mutex.

Containment (D1) forbids `pkg/build` importing `pkg/route`, so `pkg/build` cannot construct the
scope. It declares an **optional upgrade interface** instead — `BuildScopedRouteResolver` (a
`RouteResolver` plus `BuildScoped() RouteResolver`, idiomatic Go à la `http.Flusher`).
`resolveRouteQueries` type-asserts and, when present, drives the whole build through the returned
scope. The base `RouteResolver` contract is unchanged: a resolver that does not implement it
(mocks, any embedder's resolver) runs exactly as before. `pkg/route`'s `*Resolver` implements it;
`ResolveRoute`'s body is factored into `resolve(ctx, req, probe)` with the probe injected, and the
scope (`scopedResolver`) passes a memoising wrapper. Errors are NOT cached — a later key sharing the
IP retries — so error behaviour is byte-identical to per-call. No outcome, determinism, or edge/node
change: purely fewer identical store reads.

## Risks / Trade-offs

- **`router_check_tool` dominates latency** (~50–60ms per invocation native; istiod translate is only
  ~3ms and the ClickHouse window load 30–40ms). With v1's serial, uncached resolution, a build with
  many distinct unknown FQDN peers is slow, and the POC measured ~500MB peak RSS.
  → Bounded by `--route-resolve-timeout` per endpoint and by the existing `--build-timeout` overall;
  anything that doesn't finish degrades to `external`. The named follow-up is to dedupe by
  `(cluster, host, path, port, ip)`, resolve concurrently, and add a resolver-side TTL cache — noting
  that such a cache is an **upstream-lookup** cache, not the server-side *result* cache `CLAUDE.md`
  forbids in v1.

- **`istio.io/istio` version coupling.** The RouteConfiguration is generated by linked-in istiod code,
  so a cluster's Istio upgrade means rebuilding kube-state-graph.
  → Same caveat the design doc already carries (§9.4). Pin the version; the oracle test detects drift.

- **Dependency surface.** `istio.io/istio` transitively links `k8s.io/client-go`. Per D0 this is not a
  violation of the "no Kubernetes API" rule — no client is ever constructed — but it is still a large
  dependency, and an embedder of `pkg/kubegraph` should not inherit it.
  → Contained by D1: `pkg/build` ↛ `pkg/route`, so no embedder links it. A CI check (`go list -deps`)
  asserts `pkg/kubegraph` never reaches `k8s.io/client-go`. **That check guards D1 (hygiene), not D0**
  — D0 is guarded by the design rule that no Kubernetes client is ever constructed, which is a code
  review concern, not something `go list` can see.

- **Cross-repo schema dependency.** The `cluster` column is a **new requirement on the
  metadata-exporter repo**. Until it lands, cluster-scoped queries cannot be written.
  → This is the one hard external blocker. Track it explicitly; ksg fails fast on schema drift rather
  than silently degrading.

- **Port guessing.** With no `:port` in the peer address and no `client_server_port` label, we default
  to 443. A Gateway serving the host only on a non-standard port misses.
  → Logged distinctly as `route_engine_no_listener_on_port`. Cheap follow-up, deliberately not in v1:
  on that specific miss, retry against the Gateway's other HTTP-capable listener (a second translate +
  `router_check_tool` run).

- **No `client_dns_answers` ⇒ no route resolution at all** (D6). Deployments whose exporter does not
  emit the label lose the feature entirely — the endpoint stays `external` exactly as pre-change.
  → Accepted deliberately: without an IP the ingress cluster cannot be selected (D10), and a guessed
  cluster is the failure mode this revision removes. Adding the label enables the feature with no
  code change; the `route_engine_no_ip` reason makes the gap visible in logs.

- **Ambiguous ingress-cluster selection over-degrades to external** (D10). An IP served in several
  clusters with no caller membership, or multi-A answers disagreeing (including off-mesh IPs mixed
  into the answer set), yields `ambiguous_ingress_cluster` / `no_ingress` → external.
  → By design: never a wrong cluster, and the two reasons are distinct in logs. Watched follow-up if
  real answer sets prove noisy.

- **The `ClustersWithIngressIP` probe has no cluster predicate** — it scans every cluster's
  `service_versions` rows for the IP.
  → Bounded by the small ingress-LB Service version count and the `valid_from < t1` predicate; one
  indexed scan per distinct IP per build. Acceptable for v1's serial resolution.

- **Inherited POC limits**: no DestinationRule subset resolution; TLS passthrough / TCP servers have no
  HTTP RDS route and therefore always miss.
  → Both degrade to `external`, never to a wrong answer.

## Migration Plan

The feature ships **off**. Rollout is: (1) metadata-exporter adds the `cluster` column and ingests the
Istio GVRs; (2) the ksg image gains `router_check_tool`; (3) set `KSG_ROUTE_STORE_DSN` on one replica
and compare its graph against an unset replica. Rollback is unsetting the DSN — no data migration, no
schema ownership, nothing to undo, and the graph reverts to today's `external` nodes.

## Open Questions

- Which Istio version does the deployed mesh run, and therefore which `istio.io/istio` version do we
  pin? (Determines the `router_check_tool` / Envoy tools image tag too.)
  **Resolved at implementation time**: pinned to the POC-validated pair — `istio.io/istio
  v0.0.0-20250506181944-c2e9871f340c` + `istio.io/api v1.26.0-beta.0` + Envoy tools image
  `tools-v1.34-latest` — the exact versions the POC's oracle proved 0 mismatches against. Bumping
  them means re-running the `-tags oracle` sweep.
- Should a Gateway that resolves the host but yields no route on the chosen port trigger the
  other-listener retry (D5's follow-up) in v1 after all, if real traffic shows it is common?
