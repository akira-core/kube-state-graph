## MODIFIED Requirements

### Requirement: Unknown-server peer-label enrichment

The reader SHALL attempt to resolve the server side of a
`traces_service_graph_request_total` series from three additional client-recorded
peer-address labels — `client_server_address`, `client_network_peer_address`, and
`client_net_peer_name` — instead of unconditionally dropping the endpoint, when and only
when `client_k8s_pod_uid` resolves to a **real topology pod** (not a synthesised pod), the
server side has no resolvable pod (`server_k8s_pod_uid` empty, or non-empty but absent from
the global pod-UID index), AND the raw `server` label is exactly `"unknown"`. This is a
narrow, additive carve-out from the sentinel-exclusion outcome described in "Virtual
sentinel endpoint exclusion (user / unknown)"; it SHALL NOT apply when the client side is
unresolved or when the server UID itself resolves to a topology pod.

The three labels are distinct OpenTelemetry attributes, not three spellings of one:
`client_server_address` is the stable `server.address` (the logical destination as the
caller addressed it — a name, an IP, or a UDS name); `client_network_peer_address` is the
stable `network.peer.address` (the socket-level peer address, by convention an IP);
`client_net_peer_name` is the deprecated `net.peer.name`, superseded by `server.address`.

The reader SHALL NOT read `client_network_peer_port`. The stable OpenTelemetry network
conventions carry the peer port as its own attribute, but a port participates in neither
peer identification (Service resolution keys on DNS name or `ClusterIP`) nor node naming.
Its omission is deliberate, not an oversight.

Resolution order, evaluated only under the trigger condition above. The first non-empty
label wins outright; values are never merged across labels, and the reader SHALL NOT fall
through to a lower-precedence label when a higher-precedence one is non-empty but fails to
classify:

1. If `client_server_address` is non-empty, use its value as the peer address.
2. Otherwise, if `client_network_peer_address` is non-empty, use its value as the peer
   address.
3. Otherwise, if `client_net_peer_name` is non-empty, use its value as the peer address.
4. Otherwise (all three empty or absent), the reader SHALL drop the endpoint — no node, no
   edge — identical to the outcome when this requirement does not apply.

The order ranks the labels by what the classification stages below can resolve, not by
recency of spelling. Classification is strong on names — the Kubernetes Service-DNS grammar
and the bare-short-name form both yield a `(namespace, service)` pair that resolves to a
Service node with its family-wide fan-out — and weak on IP literals, whose `ClusterIP`
lookup is deliberately restricted to the anchor cluster, so a pod IP, a loopback address,
or any off-cluster IP resolves to nothing and falls to `external`. `server.address` is the
name-valued stable attribute and therefore leads; `network.peer.address` is the IP-valued
stable attribute and follows; the deprecated `net.peer.name`, superseded by
`server.address`, trails.

When a peer address is obtained (step 1, 2, or 3), the reader SHALL classify it through the
following stages, in order:

- **(a) Bracket truncation.** If the value contains a `[` at any index **greater than 0**,
  truncate the value at the **first** such `[`, discarding that character and everything
  after it. Some instrumentations append a bracketed connection or session identifier to
  the authority (e.g. `mongo.com:27017[-181]`), which is not part of the network address
  and prevents the host/port split in stage (b) from succeeding. A value with no `[`, or
  whose only `[` is at index 0, is used unchanged — a leading `[` is the IPv6 bracket form
  (`[2001:db8::1]:8080`), which the host/port split in stage (b) already handles correctly
  by removing the brackets, so truncating it would destroy a resolvable address. This
  truncation SHALL be applied uniformly to whichever of the three labels supplied the
  value — the reader does NOT track which label a value came from; the index-0 condition is
  a property of the value's shape, not of its source.
- **(b) Port strip.** Strip an optional trailing `:<port>` suffix (best-effort host/port
  split; a value with no splittable port is used unchanged). The split-out port is NOT
  discarded: it is carried to the Istio route-resolution step below as that step's
  highest-precedence listener-port hint. A bracketed IPv6 authority
  that reaches this stage intact SHALL yield the IPv6 address without its brackets.
- **(c) Service-DNS grammar.** Apply the same Kubernetes Service-DNS grammar used by
  connection-string resolution (2-label `<service>.<namespace>`, or 3-label headless
  `<pod>.<service>.<namespace>` with the leading pod-hostname dropped, `.svc[.<cluster-domain>]`
  suffix stripped) to the resulting host.
- **(d) Bare short name.** When stage (c) does not match AND the host is non-empty,
  contains no `.`, and is not an IP literal, the reader SHALL treat it as a **bare short
  Service name resolved in the client pod's own namespace** — `(service=host,
  namespace=<client_k8s_namespace_name>)`. This is one grammar extension beyond
  connection-string resolution's own classification, and it applies ONLY within this
  requirement's trigger condition. The stage performs **no** DNS-1123 character or length
  validation: any dot-free non-IP-literal string is accepted as a candidate Service name,
  and an unmatchable one is filtered out by the Service lookup rather than by the grammar.
  A consequence: a value that bracket truncation or the port strip reduces to a dot-free
  label reaches this stage, so a peer such as `mongo:27017[-181]` resolves to a Service
  named `mongo` in the client pod's own namespace if one exists — the same trade this stage
  already makes for the un-bracketed `mongo:27017`.
- **(e) IP literal.** When stages (c) and (d) both fail to match AND the host is a valid IP
  literal (IPv4 or IPv6, per `net.ParseIP`), the reader SHALL look it up as a Kubernetes
  Service `ClusterIP` **within the already-resolved client pod's own (anchor) cluster
  only** — never any other cluster, including a family sibling. A match yields the
  `(namespace, service)` of the Service whose `ClusterIP` equals the host in that cluster.
  This is a second grammar extension beyond connection-string resolution's own
  classification, scoped ONLY to this requirement's trigger condition, and it is evaluated
  after, and independently of, stage (d) (an IP literal never satisfies stage (d), since
  stage (d) explicitly excludes IP literals).
- **(f) Unresolvable.** Any other shape (multi-label non-`.svc` FQDN, an IP literal absent
  from the anchor cluster's own Service `ClusterIP` set, any other value no earlier stage
  matched) is **unresolvable** at this step. Note that a bracketed IPv6 authority in its
  plain (non-IPv4-embedded) form that stages (a) and (b) cannot reduce to a parseable host
  does NOT reach this stage: being dot-free and not an IP literal, it is matched by stage
  (d) as a candidate Service name and is filtered out by the subsequent Service lookup
  instead. An IPv6 authority with an embedded IPv4 suffix (e.g. `[::ffff:10.0.0.5]:8080[-1]`)
  contains dots, so it fails stage (d)'s dot-free test and DOES reach this stage.

When stage (c), (d), or (e) yields a `(namespace, service)` pair, the reader SHALL
resolve it via the same same-cluster Service-node resolution used by connection-string
resolution ("Connection-string endpoint resolution", steps 3–4), anchored on the
**already-resolved client pod's own cluster** (no anchor-recovery ambiguity, since the
client side is guaranteed to be a real topology pod under this requirement's trigger
condition): AT MOST ONE service node in that cluster, materialised iff the anchor cluster
itself holds the addressed Service, with the same cross-cluster `service-selects-pod`
fan-out over every same-family cluster holding the Service. This applies identically
whether the `(namespace, service)` pair was obtained via DNS-grammar classification (stage
c), bare-short-name classification (stage d), or IP-literal `ClusterIP` matching (stage e):
once identified, an IP-literal match is resolved exactly like any other classified
Service address — including the family-wide `service-selects-pod` fan-out — only the
*identification* step (stage e itself) is restricted to the anchor cluster. Resolution is
likewise identical regardless of which of the three labels supplied the peer address.

If two or more Services within the SAME anchor cluster share the identical `ClusterIP`
value (a data anomaly Kubernetes itself prevents in a healthy cluster, but the reader
stays defensive), stage (e) SHALL deterministically select the Service with the
lexically-smaller `(namespace, service)` pair.

When classification is unresolvable (stage f), OR the anchor cluster does not hold the
addressed Service, the reader SHALL — **before** falling back to an external node —
consult the Istio route-resolution engine as specified in "Istio route resolution of global
FQDN peers" (subject to that requirement's own preconditions: in particular, an endpoint
carrying no destination IPs is NEVER consulted and falls external directly). This
route-resolution step SHALL be reached from EVERY path that would otherwise produce an
external node under this requirement, without exception. In particular, a global or ingress
FQDN such as `api.example.com` is a 3-label host and is therefore *successfully* classified
by stage (c) — as service `example` in namespace `com` — before failing the anchor-cluster
membership test, so it reaches route resolution via the "anchor cluster does not hold the
addressed Service" path, NOT via the unresolvable path. An implementation that consults the
engine only on stage-(f) unresolvability does NOT satisfy this requirement.

When route resolution is disabled, or does not yield a destination, the reader SHALL fall
back to an **external** node from the RAW peer-address
value — the label's value exactly as read from the series, with NEITHER the bracket
truncation of stage (a) NOR the port strip of stage (b) applied:

- `id`     = `external/<raw_peer_address_value>`
- `name`   = `<raw_peer_address_value>` (verbatim — no normalisation, no trimming)
- `type`   = `"external"`
- `labels` = `{}` (empty map — no `cluster` key)

This raw-value convention is identical for all three peer-address labels. A consequence,
accepted deliberately: one host dialed under several distinct bracketed identifiers
materialises one external node per identifier.

The resulting edge follows the existing generic rules unchanged: `type` is
`pod-calls-service` when the resolved target is a service node, otherwise `pod-calls-pod`;
`labels.cluster` is present (the client pod's cluster) because the client side is a
resolved pod. No new node type and no new edge type are introduced by this requirement.

#### Scenario: `client_network_peer_address` resolves to an in-cluster Service

- **WHEN** a series has `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`, namespace `shop`), `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_address="payments.payments-ns.svc.cluster.local"`, no `client_server_address` and no `client_net_peer_name`, and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with backing pods
- **THEN** the resulting edge has `type: "pod-calls-service"`, `target: "cluster-alpha/payments-ns/payments"`, `labels.cluster: "cluster-alpha"`; the target service node materialises with its usual `service-selects-pod` fan-out to its backing pods

#### Scenario: `client_server_address` outranks both other peer labels

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, and all three labels are non-empty — `client_server_address="payments.payments-ns.svc.cluster.local"`, `client_network_peer_address="10.4.4.4"`, `client_net_peer_name="legacy.other-ns.svc.cluster.local"` — with the `payments` and `legacy` Services present in `cluster-alpha` and a `cluster-alpha` Service holding `cluster_ip="10.4.4.4"`
- **THEN** the endpoint resolves from `client_server_address` alone, targeting `cluster-alpha/payments-ns/payments`; neither the `ClusterIP`-matching Service nor `legacy` contributes a node or an edge

#### Scenario: `client_network_peer_address` outranks `client_net_peer_name`

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, no `client_server_address`, `client_network_peer_address="payments.payments-ns.svc.cluster.local"`, and `client_net_peer_name="legacy.other-ns.svc.cluster.local"`, with both addressed Services present in `cluster-alpha`
- **THEN** the endpoint resolves from `client_network_peer_address`, targeting `cluster-alpha/payments-ns/payments`; `legacy` contributes no node and no edge

#### Scenario: A non-classifying higher-precedence label does not fall through

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="payments.partner.example"` (a multi-label host that is not a `.svc` name), and `client_network_peer_address="payments.payments-ns.svc.cluster.local"` (which WOULD resolve in `cluster-alpha`)
- **THEN** the endpoint resolves from `client_server_address` only and falls back to `external/payments.partner.example` — the first non-empty label wins outright, and a failure to classify does NOT defer to a lower-precedence label

#### Scenario: Bracketed connection identifier is truncated before classification

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_address="payments.payments-ns.svc.cluster.local:27017[-181]"` (no `client_server_address`), and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha`
- **THEN** the value is truncated at the first `[` to `payments.payments-ns.svc.cluster.local:27017`, the `:27017` port suffix is then stripped, and the endpoint resolves to `cluster-alpha/payments-ns/payments` with `type: "pod-calls-service"`

#### Scenario: Bracketed value that does not resolve keeps its raw form as the external node

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, and `client_network_peer_address="mongo.com:27017[-181]"` (classification sees host `mongo.com`, which addresses no Service in `cluster-alpha`)
- **THEN** the endpoint falls back to an external node with `id: "external/mongo.com:27017[-181]"` and `name: "mongo.com:27017[-181]"` — the raw value verbatim, with neither the bracket suffix nor the port removed — and the edge has `type: "pod-calls-pod"`

#### Scenario: Two bracketed identifiers on one host produce two external nodes

- **WHEN** two series both resolve their client side to pods in `cluster-alpha` with `server="unknown"`, carrying `client_network_peer_address="mongo.com:27017[-181]"` and `client_network_peer_address="mongo.com:27017[-182]"` respectively, and `mongo.com` addresses no Service in `cluster-alpha`
- **THEN** two distinct external nodes are materialised — `external/mongo.com:27017[-181]` and `external/mongo.com:27017[-182]` — each the target of its own edge; the raw-value naming convention is not deduplicated by host

#### Scenario: Bracketed IP-literal peer address resolves to its ClusterIP-matching Service

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_address="172.20.10.5:27017[-9]"`, and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with `cluster_ip="172.20.10.5"`
- **THEN** the bracket suffix and the port are stripped in that order, the resulting host `172.20.10.5` matches by `ClusterIP`, and the endpoint resolves to `cluster-alpha/payments-ns/payments`

#### Scenario: Bracketed IPv6 authority is NOT truncated and still resolves by ClusterIP

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_address="[fd00:10:96::a]:8080"`, and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with `cluster_ip="fd00:10:96::a"`
- **THEN** the leading `[` is at index 0, so bracket truncation does NOT apply; the host/port split removes the brackets and the port, yielding `fd00:10:96::a`, which matches by `ClusterIP` and resolves to `cluster-alpha/payments-ns/payments` — identical to the behaviour before bracket truncation existed

#### Scenario: Bracketed IPv6 authority carrying a trailing identifier stays external

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, and `client_network_peer_address="[fd00:10:96::a]:8080[-181]"`
- **THEN** the first `[` is at index 0 so truncation does not apply, the host/port split then fails on the trailing `[`, the value reaches the bare-short-name stage (it is dot-free and not an IP literal) and is matched there as a candidate Service name, the Service lookup finds nothing, and the endpoint falls back to `external/[fd00:10:96::a]:8080[-181]` with the raw value verbatim

#### Scenario: Bracket truncation applies to the other peer labels too

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, no `client_server_address` and no `client_network_peer_address`, and `client_net_peer_name="payments.payments-ns.svc.cluster.local:27017[-181]"`, with a `payments` service in namespace `payments-ns` in `cluster-alpha`
- **THEN** the same truncation applies and the endpoint resolves to `cluster-alpha/payments-ns/payments` — the normalisation is a property of the peer-address value, not of which label carried it

#### Scenario: Truncation reduces a value to a bare short name and it resolves in the client's namespace

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, namespace `shop`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_address="payments:8080[-181]"`, and `cluster-alpha` holds a Service named `payments` in namespace `shop`
- **THEN** the value truncates to `payments:8080`, the port strip yields `payments`, the bare-short-name stage resolves it to `(namespace="shop", service="payments")`, and the endpoint targets `cluster-alpha/shop/payments` — the same trade the bare-short-name stage already makes for the un-bracketed value `payments:8080`, now reachable by one more value shape. Absent such a Service the endpoint falls back to `external/payments:8080[-181]` with the raw value verbatim

#### Scenario: `client_server_address` resolves to an in-cluster Service

- **WHEN** a series has `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`, namespace `shop`), `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="payments.payments-ns.svc.cluster.local:8080"`, and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with backing pods
- **THEN** the port suffix `:8080` is stripped before classification, the resulting edge has `type: "pod-calls-service"`, `target: "cluster-alpha/payments-ns/payments"`, `labels.cluster: "cluster-alpha"`, and the target service node materialises with its usual `service-selects-pod` fan-out to its backing pods

#### Scenario: `client_net_peer_name` used only when both higher-precedence labels are absent

- **WHEN** a series has `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`), `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address=""` and `client_network_peer_address=""` (both absent), and `client_net_peer_name="payments.payments-ns.svc.cluster.local"`
- **THEN** the endpoint resolves from `client_net_peer_name`, targeting `cluster-alpha/payments-ns/payments` exactly as if a higher-precedence label had carried the same host

#### Scenario: Bare short Service name resolves in the client pod's own namespace

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, namespace `shop`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="payments"` (a bare single-label name, no `.svc` suffix), and topology has a `payments` service in namespace `shop` (the client's own namespace) in `cluster-alpha`
- **THEN** the reader treats `payments` as addressing `(namespace="shop", service="payments")` and resolves it to `cluster-alpha/shop/payments`, exactly as the 2-label `.svc` form would

#### Scenario: Bare IP-literal peer address resolves to its ClusterIP-matching Service

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="172.20.10.5"` (no `:port`, no DNS name), and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with `cluster_ip="172.20.10.5"`
- **THEN** the resulting edge has `type: "pod-calls-service"`, `target: "cluster-alpha/payments-ns/payments"`, exactly as if the peer address had been the Service's `.svc` DNS name

#### Scenario: IP-literal peer address with a port suffix is stripped before matching

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="172.20.10.5:8080"`, and topology has a service in `cluster-alpha` with `cluster_ip="172.20.10.5"`
- **THEN** the `:8080` suffix is stripped before IP-literal classification, and the endpoint resolves to that service node

#### Scenario: IP-literal peer address present only in a family-sibling cluster — external, not cross-resolved

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="172.20.10.5"`, `cluster-alpha` holds NO service with `cluster_ip="172.20.10.5"`, but a same-family sibling cluster `cluster-alpha-2` DOES hold a service with that `ClusterIP`
- **THEN** the endpoint falls back to `external/172.20.10.5` — the IP-literal lookup (stage e) is scoped to the anchor cluster only and does NOT consult family siblings, unlike the subsequent `service-selects-pod` fan-out that would apply once a Service is identified

#### Scenario: IP-literal peer address absent from the anchor cluster's Service set — external

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="192.0.2.55"`, and no Service in `cluster-alpha` has that `ClusterIP`
- **THEN** the endpoint falls back to `external/192.0.2.55`

#### Scenario: External peer address becomes an external node

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, and `client_net_peer_name="payments.partner.example"` (a multi-label host that is not a `.svc` name and not a bare short name)
- **THEN** the resulting edge has `type: "pod-calls-pod"`, `target: "external/payments.partner.example"`; the target node has `type: "external"`, `name: "payments.partner.example"`, `labels={}`

#### Scenario: Anchor cluster lacks the addressed Service — external, not dropped

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="web.shop.svc.cluster.local"`, and `cluster-alpha` does NOT hold a `web` service in namespace `shop` (a family sibling holding it does not count, per the existing same-cluster rule)
- **THEN** the endpoint falls back to `external/web.shop.svc.cluster.local` rather than resolving to a service node in a different cluster, and rather than being dropped

#### Scenario: No peer label present — dropped

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a real topology pod, `server="unknown"`, `server_k8s_pod_uid=""`, and all of `client_server_address`, `client_network_peer_address`, and `client_net_peer_name` are empty or absent
- **THEN** the endpoint is dropped: no node and no edge are produced for it

#### Scenario: `client_network_peer_port` alone does not trigger enrichment

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a real topology pod, `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_port="27017"`, and all three peer-address labels are empty or absent
- **THEN** the endpoint is dropped: the port label is not read, is never treated as a peer address, and produces no node and no edge

#### Scenario: Client side not a resolved real pod — enrichment does not apply

- **WHEN** a series has `client="admin"`, `client_k8s_pod_uid=""` (client does not resolve to a topology pod), `server="unknown"`, `server_k8s_pod_uid=""`, and `client_network_peer_address="payments.payments-ns.svc.cluster.local"` is present
- **THEN** the trigger condition (client resolved to a real pod) is not met, so this requirement does not apply and the endpoint is dropped per the sentinel-exclusion requirement, even though a peer label is present

#### Scenario: Server UID present but unresolved — enrichment still applies

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a real topology pod, `server="unknown"`, `server_k8s_pod_uid="stale-uid"` (non-empty but absent from the global pod-UID index), and `client_network_peer_address="payments.payments-ns.svc.cluster.local"` resolves to an in-cluster Service
- **THEN** the reader resolves via peer-label enrichment (target: the service node) rather than synthesising a pod node for `stale-uid` — a non-empty but unresolvable server UID does not take priority over this requirement when `server` is literally `"unknown"`

#### Scenario: Duplicate ClusterIP within the anchor cluster resolves deterministically

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="172.20.10.5"`, and (as a data anomaly) `cluster-alpha` holds two Services with `cluster_ip="172.20.10.5"` — `(namespace="ops", service="zeta")` and `(namespace="ops", service="alpha")`
- **THEN** the endpoint resolves to `(namespace="ops", service="alpha")` — the lexically-smaller `(namespace, service)` pair — deterministically and identically across rebuilds of the same upstream data

#### Scenario: Global FQDN reaches route resolution via the anchor-lacks-service path

- **WHEN** a `server="unknown"` series has a client resolving to a real topology pod and
  `client_net_peer_name="api.example.com"`
- **THEN** DNS-grammar classification (step 2) succeeds, yielding `(namespace="com",
  service="example")`
- **AND** the anchor cluster does not hold that Service, so same-cluster Service-node
  resolution misses
- **AND** the reader SHALL consult the route-resolution engine before emitting any external
  node

#### Scenario: Route resolution disabled falls back to external unchanged

- **WHEN** route resolution is not configured
- **AND** a `server="unknown"` endpoint's peer address is unresolvable by steps 2–4, or the
  anchor cluster does not hold the classified Service
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>` with `labels = {}`,
  byte-for-byte identical to the behaviour before route resolution existed

#### Scenario: In-cluster classification is unaffected by route resolution

- **WHEN** a `server="unknown"` endpoint's peer address classifies to a `(namespace,
  service)` pair the anchor cluster DOES hold — via `.svc` DNS, bare short name, or
  ClusterIP literal
- **THEN** the reader SHALL resolve it to that service node exactly as before
- **AND** the route-resolution engine SHALL NOT be consulted

## ADDED Requirements

### Requirement: Istio route resolution of global FQDN peers

The reader SHALL support an OPTIONAL Istio route-resolution engine that resolves a
global / ingress FQDN peer to the Kubernetes Service that the Istio Gateway and
VirtualService configuration routed it to during the request's own time window. The engine
is consulted ONLY from "Unknown-server peer-label enrichment", at every point that would
otherwise emit an external node.

The engine SHALL be configured by a route-store connection string. When that setting is
empty the engine is **disabled**, and the reader's behaviour SHALL be byte-for-byte
identical to its behaviour before this requirement existed. Disabled is the default.

**Inputs.** For one candidate endpoint the reader SHALL supply:

- the **caller cluster** — the already-resolved client pod's own cluster, used ONLY to
  derive the cluster-family key and to break candidate ties in ingress-cluster selection;
  it SHALL NOT by itself scope any route-store window query;
- the **host** — the port-stripped peer address;
- the **path** — fixed to `"/"`. The service-graph metric carries no HTTP path or route
  dimension, so no per-request path is available;
- the **listener port** — derived as specified below;
- the **destination IPs** — from the `client_dns_answers` dimension. These are a
  **precondition**: when the endpoint carries no parseable destination IP, the engine
  SHALL NOT be consulted at all — no store read occurs, the endpoint falls back to the
  external node directly, and the reader SHALL record a distinct diagnostic reason for
  the skip (separate from every engine outcome);
- the **time window** — the build's own `[start, end]`.

**Listener-port derivation.** The port SHALL be derived by this precedence:

1. the `:<port>` suffix split from the peer-address value, when present;
2. otherwise the OPTIONAL `client_server_port` or `client_net_peer_port` dimension, when
   present;
3. otherwise the default **443**.

**Ingress-cluster selection.** The destination IPs select the ingress cluster; the caller
cluster contributes only its family key and a tie-break. For each destination IP the
engine SHALL probe the store for the candidate set `G` — the clusters that had an ingress
Service carrying that IP overlapping the window — and derive `F`, the subset of `G` in the
caller's cluster family (the same digit-run-collapsing family rule used by
`service-selects-pod` fan-out). Selection per IP:

1. exactly one family candidate → that cluster;
2. several family candidates → the caller's own cluster if it is among them, otherwise
   **ambiguous**;
3. no family candidate and exactly one global candidate → that cluster;
4. no family candidate and several global candidates → the caller's own cluster if it is
   among them, otherwise **ambiguous**;
5. no candidate at all → **no ingress**.

With several destination IPs, each IP SHALL be selected independently and all selections
MUST agree on one cluster; every IP yielding "no ingress" degrades as **no ingress**, and
any other combination — an ambiguous IP, disagreeing selections, or a mix of "no ingress"
and a selected cluster — degrades as **ambiguous**. Candidate sets and store windows SHALL
NEVER be unioned across clusters. Once a cluster is selected, every subsequent resolution
step — window load, gateway narrowing, host disambiguation, translation, route matching —
SHALL operate on that single cluster only, and the engine SHALL narrow the candidate
Gateways to those reachable from the destination IPs within it.

**Store scoping.** Every route-store *window* query SHALL be scoped to the selected
ingress cluster. The ONLY cross-cluster store read SHALL be the ingress probe that answers
"which clusters had an ingress Service with this IP in the window", and its result SHALL
be deterministic (deduplicated and ordered). The reader SHALL be strictly read-only
against the store: it SHALL NOT create schema and SHALL NOT write. The reader SHALL
validate the expected schema at startup and fail fast on drift, rather than silently
returning empty results.

**Store-shape tolerance.** The store is written by an exporter whose version-close
operation REWRITES the previous open row; until background merges collapse them, a stale
open row and its closing rewrite coexist. The reader SHALL NOT treat such a pair as two
live versions (deduplicating at query time), so results do not depend on merge timing.
Stored resource specs are the API server's JSON verbatim, whose field set follows the
cluster's CRD version: the reader SHALL tolerate unknown spec fields rather than failing
the query. A VirtualService binding its gateway by the bare `<name>` form (Istio shorthand
for a gateway in the VirtualService's own namespace) SHALL bind exactly as the qualified
`<namespace>/<name>` form does.

**Resolution.** A hit yields a destination `(cluster, namespace, service)`, where the
cluster is the engine-selected ingress cluster. The reader SHALL resolve that triple
through the SAME same-cluster Service-node resolution every other path uses — anchored on
the **selected ingress cluster**, materialising AT MOST ONE service node iff that cluster
holds the Service in topology, with the same cross-cluster `service-selects-pod` fan-out
over its family. Because the selected cluster may differ from the caller's, the resulting
`pod-calls-service` edge MAY cross clusters, and the edge-type registry SHALL declare
`may_cross_cluster: true` for `pod-calls-service` (connection-string-resolved edges remain
intra-cluster by construction; per-edge cross-cluster status is still derived by comparing
the resolved endpoints' clusters). The destination's port and DestinationRule subset SHALL
be discarded. **No new node type, no new edge type, no new node attribute, and no new
`labels` key** are introduced.

**Degradation.** Every failure — engine disabled, endpoint carrying no destination IPs,
store error, no candidate ingress cluster for the IPs, an ambiguous candidate set, no
Gateway serving the host, no route matched, no listener on the derived port, or a
resolution timeout — SHALL fall back to the external node specified in "Unknown-server
peer-label enrichment". Route resolution SHALL NEVER fail a build. The reader SHALL record
distinct diagnostic reasons for a "no listener on the derived port" outcome (separate from
"no route matched", so a mis-derived port is diagnosable), for a "no candidate ingress
cluster" outcome, and for an "ambiguous ingress cluster" outcome.

**Determinism.** Route resolution SHALL be performed outside the pure service-graph parse
and its results supplied to the parse as a prefetched index, so the emitted graph remains a
deterministic function of the upstream data and the resolved destinations.

#### Scenario: Global FQDN resolves to its routed Service

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint's peer address is
  `api.example.com` with a destination IP selecting the caller's own cluster as ingress,
  whose Istio Gateway and VirtualService route the host to Service `checkout` in namespace
  `shop` during the request window
- **THEN** the reader SHALL emit a `service` node for `(selected cluster, shop, checkout)`
- **AND** a `pod-calls-service` edge from the client pod to it
- **AND** SHALL NOT emit `external/api.example.com`

#### Scenario: Listener port taken from the peer address

- **WHEN** the peer address is `api.example.com:8080`
- **THEN** the host SHALL be `api.example.com` and the derived listener port SHALL be `8080`

#### Scenario: Listener port defaults to 443

- **WHEN** the peer address carries no port and neither `client_server_port` nor
  `client_net_peer_port` is present
- **THEN** the derived listener port SHALL be `443`

#### Scenario: No listener on the derived port degrades to external

- **WHEN** the selected ingress cluster's Gateway serves the host but declares no routable
  HTTP listener on the derived port
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>`
- **AND** SHALL record a diagnostic reason distinct from the "no route matched" reason

#### Scenario: The server owning the host on the port is selected

- **WHEN** the selected ingress cluster's Gateway declares two TLS-terminated HTTPS servers
  on the derived port — one whose hosts match the peer FQDN and one whose hosts do not —
  in either declaration order
- **THEN** the reader SHALL resolve the request through the RouteConfiguration of the
  server whose hosts most-specifically match the peer FQDN (Istio exact/wildcard
  semantics, any `<ns>/` binding prefix stripped before matching)

#### Scenario: No server on the derived port serves the host

- **WHEN** the selected ingress cluster's Gateway declares servers on the derived port but
  none of their hosts match the peer FQDN
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>`
- **AND** SHALL record a diagnostic reason distinct from both the "no listener on the
  derived port" reason and the "no route matched" reason

#### Scenario: Store failure never fails the build

- **WHEN** the route store is unreachable or returns an error while resolving an endpoint
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>` for that endpoint
- **AND** the build SHALL complete successfully

#### Scenario: Destination IP narrows the candidate Gateways within the selected cluster

- **WHEN** `client_dns_answers` supplies a destination IP that, within the selected ingress
  cluster, reaches exactly one of two Gateways whose server hosts both match the peer FQDN
- **THEN** the reader SHALL resolve the host against that Gateway only

#### Scenario: No destination IPs means the engine is not consulted

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint's peer address
  would fall external, but the series carries no parseable `client_dns_answers` IP
- **THEN** the engine SHALL NOT be consulted (no store read for this endpoint)
- **AND** the reader SHALL emit `external/<raw_peer_address_value>` exactly as when route
  resolution is disabled
- **AND** SHALL record a diagnostic reason distinct from every engine outcome

#### Scenario: Same-family ingress candidate wins over a cross-family one

- **WHEN** a destination IP's candidate ingress clusters are one cluster in the caller's
  family and one cluster outside it
- **THEN** the engine SHALL select the same-family cluster

#### Scenario: Caller breaks a same-family ingress-IP collision

- **WHEN** a destination IP's candidate ingress clusters include the caller's own cluster
  and a family sibling
- **THEN** the engine SHALL select the caller's own cluster

#### Scenario: Unresolvable ingress-cluster tie degrades to external

- **WHEN** a destination IP's candidate ingress clusters are two family siblings, neither
  of which is the caller's own cluster
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>`
- **AND** the reader SHALL record the "ambiguous ingress cluster" diagnostic reason

#### Scenario: No cluster serves the destination IP

- **WHEN** no cluster had an ingress Service carrying any of the destination IPs during
  the window
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>`
- **AND** the reader SHALL record the "no candidate ingress cluster" diagnostic reason

#### Scenario: Disagreeing multi-IP selections degrade to external

- **WHEN** `client_dns_answers` supplies two IPs whose independent selections pick two
  different ingress clusters
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>` with the
  "ambiguous ingress cluster" diagnostic reason

#### Scenario: Cross-cluster ingress resolves to a Service in the sibling cluster

- **WHEN** the caller pod lives in cluster A and the destination IP selects ingress
  cluster B, whose Gateway and VirtualService route the host to Service `payments` in
  namespace `shop`, and B holds that Service in topology
- **THEN** the reader SHALL emit the `service` node `B/shop/payments`
- **AND** a `pod-calls-service` edge from the cluster-A client pod to it (a cross-cluster
  edge)
- **AND** SHALL NOT emit an external node for the peer

#### Scenario: A rewritten (closed) version does not double-count

- **WHEN** the store carries both a stale open row (far-future `valid_to`) and its closing
  rewrite (same version key, higher ingest sequence) for one Gateway version
- **THEN** the reader SHALL see exactly one live version per instant, regardless of
  whether the store's background merge has run

#### Scenario: Unknown spec fields do not fail the query

- **WHEN** a stored Gateway or VirtualService spec carries a field unknown to the reader's
  compiled Istio API version
- **THEN** the reader SHALL parse the known fields and resolve routing from them

#### Scenario: Bare gateway reference binds

- **WHEN** a VirtualService in the gateway's own namespace lists the gateway by bare name
  in `spec.gateways`
- **THEN** its routes SHALL bind to that gateway exactly as a qualified
  `<namespace>/<name>` reference would
