## ADDED Requirements

### Requirement: Span-link logical edge relation marking

The reader SHALL read the `edge_relation` label of every
`traces_service_graph_request_total` series. A series whose value is exactly
`"link"` (a span-link-derived logical edge: client = producer pod, server =
consumer pod, joined across trace IDs through a broker) SHALL resolve both
endpoints through the ordinary resolution ladder unchanged, and the resulting
edge SHALL carry `labels.relation = "link"`. Any other `edge_relation` value
(absent, empty, or an unrecognised string) SHALL be ignored — the series is
processed exactly as before this requirement, and its edge carries no
`relation` key.

For each side of a `"link"` series that resolved to a **real topology pod**,
the reader SHALL additionally derive that side's broker (via) node ID from the
side's own client-recorded peer-address labels — the client side from
`client_server_address` / `client_net_peer_name` (with `client_dns_answers` /
`client_server_port` as route-resolution dimensions), the server side from the
mirrored `server_server_address` / `server_net_peer_name` (with
`server_dns_answers` / `server_server_port`) — using the SAME classification
chain as the unknown-server peer-label enrichment (bracket truncation and
port split, Kubernetes `.svc` DNS grammar, bare short name in the pod's own
namespace, anchor-cluster ClusterIP, family Pod IP, prefetched route-engine
index, raw-value external fallback), anchored on that side's own pod cluster.
The pair `(that side's pod, derived broker node ID)` SHALL be recorded as a
**transport** pair. A side with no peer-address value, or a side that did not
resolve to a real topology pod, contributes no transport pair; the other side
is unaffected.

Via derivation SHALL be **lookup-only**: it MUST NOT materialise any service,
external, or synthesised node, MUST NOT emit any `service-selects-pod` or
route-chain edge, and MUST NOT log route-engine external-fallback reasons.
For a route-index entry whose outcome is a routed hit, the derived ID is the
BACKEND service ID only (never the ingress entry-point), with no `role`
marking and no chain synthesis; every other index outcome, a missing entry,
absent destination IPs, or an unconfigured engine derives the external node ID
of the raw peer-address value — identical to the ID the materialising path
would have produced for the same series.

At edge-build time, an emitted `pod-calls-pod` or `pod-calls-service` edge
whose `(source, target)` pair was recorded as a link pair SHALL carry
`labels.relation = "link"`; otherwise, if recorded as a transport pair, it
SHALL carry `labels.relation = "transport"`; otherwise no `relation` key.
`link` SHALL take precedence over `transport` for the same pair, and over any
plain series that aggregated into the same edge. `service-selects-pod` edges
and synthesized route-chain edges SHALL never carry a `relation` key. A
transport pair with no matching emitted edge is a no-op (the reader MAY log
one aggregated debug line); the reader MUST NOT synthesise an edge for it.
Marking SHALL be a pure function of the accumulated pair sets — independent of
sample arrival order — and MUST NOT alter edge identity (the UUIDv5 input
remains `type|source|target`).

A `"link"` series whose raw `server` label is exactly `"unknown"` AND whose
server side has no resolvable topology pod recovered no consumer: it SHALL
contribute **no relation markers at all** — its pair is recorded as neither
link (the resolved target is the broker, not the consumer) nor transport, and
no per-side via pair is recorded from it. Its server side still resolves
through the unknown-server peer-label enrichment exactly as an ordinary
`server="unknown"` series would, so the producer → broker edge it produces is
byte-identical to the enrichment's ordinary outcome (an unmarked network
edge). Rationale: the frontend contract is "transport = the network hop
backing a rendered logical edge"; a transport marker with no accompanying
link edge would demote a real network dependency to a dashed line that backs
nothing. Consequently, transport pairs SHALL be recorded ONLY by series that
also record their link pair — a transport-marked edge in the built graph
always coexists with at least one link-marked edge originating from the same
series set (projection filters MAY still exclude either edge from a filtered
view; marking itself never depends on the projection). A `"link"` series
whose server side degrades to a synthesised pod or to the missing-UID
external fallback keeps its **link** marking (the logical claim stands even
when an endpoint is degraded).

The prescan SHALL collect route keys for both sides' peer labels of `"link"`
series (each side only when its own pod resolved, anchored on that pod's
cluster, subject to the same skip chain as the unknown-server collection:
in-cluster-resolvable and IP-less endpoints are not collected), deduplicated
with the keys ordinary `server="unknown"` series derive — a broker FQDN shared
by link and non-link series in one anchor cluster SHALL cost at most ONE
route-engine store read per build.

#### Scenario: Link series between two resolved pods

- **WHEN** a series with `edge_relation="link"` carries client and server pod
  UIDs that both resolve to topology pods
- **THEN** one `pod-calls-pod` edge is emitted from producer to consumer with
  `labels.relation = "link"`

#### Scenario: Link wins over a plain series for the same pair

- **WHEN** a plain series and an `edge_relation="link"` series resolve to the
  same `(source, target)` pair, in either ingestion order
- **THEN** the single aggregated edge carries `labels.relation = "link"`

#### Scenario: Transport marking of the existing broker edge

- **WHEN** a `"link"` series' client side resolves to a real topology pod and
  its `client_server_address` classifies to a Service the pod's own cluster
  holds, AND a plain series already produces the pod → that-Service edge
- **THEN** that `pod-calls-service` edge carries `labels.relation =
  "transport"`, and the link edge itself stays `"link"`

#### Scenario: Link beats transport on pair collision

- **WHEN** the same `(source, target)` pair is recorded both as a link pair
  and as a transport pair within one build
- **THEN** the emitted edge carries `labels.relation = "link"`

#### Scenario: server="unknown" link series contributes no markers

- **WHEN** a `"link"` series has `server="unknown"`, no resolvable server pod,
  and a client that resolved to a real topology pod with a classifiable peer
  address
- **THEN** the emitted producer → broker edge carries NO `relation` key —
  byte-identical to the edge an ordinary `server="unknown"` series produces,
  including when a plain series merges into the same pair — and the series
  records neither a link pair nor any transport pair

#### Scenario: A transport-marked edge always accompanies a link-marked edge

- **WHEN** any build's emitted edge carries `labels.relation = "transport"`
- **THEN** the same built graph contains at least one edge carrying
  `labels.relation = "link"` recorded by the same series set (a transport
  marker can only originate from a series that also recorded its link pair)

#### Scenario: Degraded server endpoints keep the link marking

- **WHEN** a `"link"` series' server side degrades to a synthesised pod
  (unknown UID) or to the missing-UID external fallback (non-`"unknown"`
  label)
- **THEN** the emitted edge still carries `labels.relation = "link"`

#### Scenario: Per-side independence of via marking

- **WHEN** a `"link"` series' client side does not resolve to a real topology
  pod but its server side does, and the server side carries
  `server_server_address`
- **THEN** no client-side transport pair is recorded, and the server-side
  pair `(consumer pod, broker node)` is still recorded as transport

#### Scenario: Via lookup never materialises

- **WHEN** a build's ONLY series are `"link"` series (no plain network
  series), with peer-address labels that would classify to Services,
  externals, or route-engine destinations
- **THEN** the response contains no service node, no external node, no
  `service-selects-pod` edge, and no route-chain edge attributable to via
  derivation — the unmatched transport pairs are marker-only

#### Scenario: Fan-out edges are never marked

- **WHEN** a broker Service node materialises via a plain series and fans out
  `service-selects-pod` edges, while link/transport marking touches edges into
  that Service
- **THEN** no `service-selects-pod` edge carries a `relation` key

#### Scenario: Self-loop link series

- **WHEN** a `"link"` series carries the same resolvable pod UID on both
  sides with no `"://"` label (the D33 guard does not fire)
- **THEN** one self-loop `pod-calls-pod` edge is emitted with
  `labels.relation = "link"`

#### Scenario: Non-link edge_relation values are ignored

- **WHEN** a series carries `edge_relation="database"` (or any value other
  than `"link"`)
- **THEN** it is processed as an ordinary series and its edge carries no
  `relation` key

#### Scenario: Shared broker FQDN resolves through one route-engine read

- **WHEN** several series in one anchor cluster — link series (either side)
  and ordinary `server="unknown"` series — carry the same unclassifiable
  broker FQDN with the same destination IPs and port
- **THEN** the route-resolution engine is consulted exactly once for that key,
  and every dependent edge/marker derives from the same prefetched answer
