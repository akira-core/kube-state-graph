---
paths:
  - "pkg/route/**"
  - "pkg/build/route*.go"
  - "internal/integration/route*.go"
---

# Istio route resolution

Disclosed reference for `CLAUDE.md`. The text is the authoritative statement of each rule; `CLAUDE.md` carries only a one-line reminder.

## Route engine

- **Istio route resolution of global FQDN peers**
  (translate-global-fqdn-to-k8s-service, OPT-IN — off by default): the ONE
  step added to the unknown-server peer-label enrichment
  (`service-graph-resolution.md`). When `--route-store-dsn` /
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
  nil index ⇒ output identical to a build with no route store configured.
  Concurrency cannot change the index's CONTENTS (entries are keyed by
  `routeKey` and independent); it changes only which keys are answered when
  the build deadline fires first, which is wall-clock dependent under any
  schedule. **(3) Listener port
  precedence** (D5): the `:<port>` on the peer-address value (returned by
  `splitPeerAddressPort`) → the optional
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
  BEFORE the instant is not a candidate
  (simplify-route-resolution-to-point-in-time D5).
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
  so a plain embedder never inherits istio/ClickHouse —
  `make check-route-containment` enforces this in CI. No new node type, edge
  type, attribute, or `labels` key; the destination's port/subset are parsed
  and discarded. Tests: `pkg/build/routeprescan_test.go`,
  `pkg/route/*_test.go`, `internal/integration/route_e2e_test.go`
  (`TestRouteSuite` needs `router_check_tool` — set `KSG_ROUTER_CHECK_BIN`;
  `TestRouteStoreSuite` needs only Docker), and the `-tags oracle` sweep.
