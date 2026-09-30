---
paths:
  - "pkg/build/servicegraph*.go"
  - "pkg/build/redmetrics*.go"
  - "pkg/build/histogram*.go"
  - "pkg/build/clusterfamily*.go"
  - "pkg/build/route*.go"
---

# Service-graph resolution

Disclosed reference for `CLAUDE.md`. The text is the authoritative statement of each rule; `CLAUDE.md` carries only a one-line reminder.

## Edge labels and RED metrics

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
  `pod-calls-service` edges carry a single `labels.cluster` — the CLIENT POD's
  cluster identity when the client side resolved to a topology pod, else the
  trace `cluster` label put through the identity ladder; omitted when the client
  side is non-pod. (Not the raw trace label: that label is not an identity and
  could name a cluster present on no node of the response.) Cross-cluster
  status is derived by comparing the resolved source-node and target-node
  `labels.cluster` — D9.

## Resolution rules

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
    (`route-resolution.md`) can anchor on a sibling cluster). **Cross-cluster
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
  resolve-unknown-server-peer-labels D1): a `server="unknown"` series
  reaches Go, but the reader drops it (no node, no edge) **UNLESS** the
  "Unknown-server peer-label enrichment" rule below applies — every
  `server="unknown"` case outside that rule's narrow trigger (client
  unresolved, or the server UID itself resolves) produces no node and no
  edge. PromQL `!~` is fully anchored, so
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
  engine is never asked about traffic the in-cluster ladder resolves —
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
  endpoint (no node, no edge). This is the invariant the narrower
  server-side selector must never violate: it must never leak a
  `external/unknown` node via the generic D27 path for a case outside this
  rule's trigger.
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

## Server-side pod resolution

- **Server-side pod resolution** uses `Topology.PodsByUID` — a global pod-UID
  index built from all loaded clusters. Service-graph metrics carry only the
  trace-source `cluster` (client side); the server side's cluster is recovered
  by looking up `server_k8s_pod_uid` against this index, since K8s pod UIDs
  are unique cross-cluster in practice. Missing UIDs (with non-empty server
  label) follow the missing-UID fallback above; UIDs present but unknown
  to topology become synth pods with `cluster=""` (server-side cluster
  unknown).

## Filtered builds

- **Filtered-build rules for the service graph** (design D5 / D6 of push-request-filters-upstream). Because the topology is narrowed while the service-graph series are read in full, a build with any selector-level dimension active applies two rules that are **inert when unfiltered** (`sgResolver.filtered`): (1) an endpoint whose non-empty pod UID names a pod the request did NOT load resolves exactly as if the UID were empty — the `"://"` ladder can still reach a LOADED Service, `server="unknown"` still goes through the peer ladder (which needs a real client pod, so an out-of-scope caller is dropped), any other non-empty label becomes `external/<label>` via the D27 fallback, and an empty label drops the side; **a filtered build NEVER synthesises a pod**; (2) a series is ADMITTED only when both sides resolved AND at least one resolved id names loaded topology (`podByID` or an already-materialised `services` entry) — otherwise a per-series **journal** rolls back every side effect (external / service / `service-selects-pod` / route-chain / ingress `role` / `extReasons`) and the series contributes nothing to `pairs`, the RED join or the link markers. This is what keeps the out-of-scope estate from rendering as an external-to-external web, and what makes an out-of-scope peer render as `external/<label>` instead of a ghost pod. **Consequence:** under `?cluster=` the cross-cluster partner is an `external` node, not a real pod — "Cross-cluster edge representation" requires BOTH clusters loaded.
