## MODIFIED Requirements

### Requirement: Virtual sentinel endpoint exclusion (user / unknown)

The reader SHALL exclude any `traces_service_graph_request_total` series whose `client`
label is exactly `"user"` or exactly `"unknown"`. These are **virtual peers** emitted by
the service-graph producer (the OpenTelemetry / Alloy / Tempo `servicegraph` connector) for
endpoints it cannot pair to an instrumented span — an uninstrumented caller surfaces as
`client="user"`, an unresolved peer as `"unknown"` — and they carry no pod UID and
represent no pod, service, or declared external dependency the API should surface.

The **client-side** exclusion SHALL be applied **at the PromQL query layer** via an
anchored negative label matcher on the series selector — `client!~"user|unknown"` — so a
series with either sentinel value on its `client` label is never returned by upstream
VictoriaMetrics and never reaches endpoint resolution.

The **server-side** exclusion is narrower: the query-layer matcher SHALL only exclude
`server` values exactly equal to `"user"` (`server!~"user"`). A series with `server`
exactly `"unknown"` SHALL reach Go-side resolution — it is no longer excluded at the query
layer — but the reader SHALL still treat it as unresolvable and drop it (no node, no edge)
UNLESS the "Unknown-server peer-label enrichment" requirement below applies. The narrower
server-side matcher exists solely to let that requirement's peer-label enrichment observe
the series' other labels (`client_server_address`, `client_network_peer_address`,
`client_net_peer_name`); it SHALL NOT be read as a general relaxation of the sentinel
rule — every `server="unknown"` case outside that requirement's narrow trigger condition
SHALL produce exactly the same outward result (dropped, no node, no edge) as the prior
query-layer exclusion.

Matching semantics:

- **Exact, fully anchored**: the PromQL `!~` regex is anchored to the entire label value,
  so only a label whose *whole* value is `user` or `unknown` is excluded (client side) or
  `user` (server side). A connection-string value such as `"http://user/path"` is NOT
  excluded (its value is not exactly `user`) and proceeds to connection-string resolution
  unchanged.
- **Case-sensitive**: `User`, `UNKNOWN`, and other case variants are NOT excluded.
- **Fixed set, no knob**: the sentinel set `{"user", "unknown"}` (client) / `{"user"}`
  (server) is compiled in. There is NO configuration surface (env var / flag / config
  field) to change either.

This exclusion is distinct from — and SHALL NOT affect — the `cluster="unknown"` bucketing
applied to series missing a `cluster` external label (a different label on a different
dimension): the sentinel matchers are evaluated ONLY against the `client` and `server`
endpoint labels.

Because the client-side-excluded series never arrive, no endpoint resolution runs for
them: no pod, synthesised-pod, `service`, or `external` node is materialised for a `user` /
`unknown` client sentinel, and no edge touching such a peer is emitted. A `server="unknown"`
series that reaches Go and does not satisfy the peer-label enrichment trigger is dropped in
Go with the identical observable effect (no node, no edge) — see the "Unknown-server
peer-label enrichment" requirement for the one case that resolves instead.

When the deferred numeric service-graph metrics (`traces_service_graph_request_failed_total`,
`traces_service_graph_request_server_seconds_bucket`) are queried in a future spec
revision, the same sentinel matchers (client: `user|unknown`; server: `user`) SHALL be
applied to their selectors so the edge set stays consistent across metric families.

#### Scenario: Series with client `user` is excluded at the query layer

- **WHEN** upstream holds a `traces_service_graph_request_total` series with `client="user"`, `server="checkout"`, `server_k8s_pod_uid="abc"`
- **THEN** the service-graph query selector includes `client!~"user|unknown"`, VictoriaMetrics does not return the series, and the graph contains no edge for it and no node named `user`

#### Scenario: Series with server `unknown` and no usable peer label is still dropped

- **WHEN** upstream holds a series with `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a real topology pod), `server="unknown"`, `server_k8s_pod_uid=""`, and the series carries none of `client_server_address`, `client_network_peer_address`, or `client_net_peer_name`
- **THEN** the series is no longer excluded at the query layer (`server!~"user"` only), but Go-side resolution drops it per the "Unknown-server peer-label enrichment" requirement's no-match case: the graph contains no edge for it and no node named `unknown`, `external/unknown`, or otherwise — identical to the outcome under the prior `server!~"user|unknown"` exclusion

#### Scenario: Series with server `unknown` and an unresolved client is still dropped

- **WHEN** upstream holds a series with `client="admin"`, `client_k8s_pod_uid=""` (the client side does NOT resolve to a real topology pod), `server="unknown"`, `server_k8s_pod_uid=""`, and the series carries a `client_network_peer_address` label
- **THEN** the peer-label enrichment trigger requires a resolved client-side pod, which this series lacks, so the series is dropped exactly as under the prior exclusion — the presence of a peer label does not by itself cause resolution

#### Scenario: Both endpoints are sentinels

- **WHEN** a series has `client="user"` and `server="unknown"`
- **THEN** the series is excluded at the query layer by the client-side matcher (`client!~"user|unknown"`) regardless of the server-side matcher or any peer label, and no edge is emitted

#### Scenario: Connection-string value containing `user` is not excluded

- **WHEN** a series has `server="http://user/api"`, `server_k8s_pod_uid=""` (the value contains, but is not equal to, `user`)
- **THEN** the series is NOT excluded (the matcher is fully anchored), and connection-string endpoint resolution proceeds normally for that endpoint

#### Scenario: `cluster="unknown"` bucketing is unaffected

- **WHEN** a series is missing its `cluster` external label and is bucketed to `cluster="unknown"`, while its `client` and `server` labels are real service names with resolvable pod UIDs
- **THEN** the series is NOT excluded by the sentinel matchers (they match only `client` / `server`, never `cluster`), and the edge is emitted under `cluster="unknown"` exactly as before

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
  split; a value with no splittable port is used unchanged). A bracketed IPv6 authority
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
addressed Service, the reader SHALL fall back to an **external** node from the RAW peer-address
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
