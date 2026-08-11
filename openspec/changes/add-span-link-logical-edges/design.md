## Context

The servicegraph exporter emits span-link-derived series: for a producer pod A
publishing through a broker (nats / mongo / …) consumed by consumer pod B, a
`traces_service_graph_request_total{edge_relation="link"}` series carries
`client_k8s_pod_uid` = A, `server_k8s_pod_uid` = B, and each side's OWN
client-recorded broker peer-address labels:

- client side: the existing `client_server_address` / `client_net_peer_name`
  / `client_dns_answers` / `client_server_port` (no
  `client_network_peer_address` on link series in v1);
- server side (new, mirrored): `server_server_address` /
  `server_net_peer_name` / `server_dns_answers` / `server_server_port` (no
  `server_network_peer_address` in v1, and no "via" string label).

The frontend renders `relation=link` edges solid (logical dependency) and
`relation=transport` edges dashed (the pod → broker network hop). Ordinary
edges carry no `relation` key. The user chose EDGE marking over node marking.
Performance and memory discipline are hard requirements: no per-resolver field
growth, no cross-build state, no repeated route-store reads for the same
broker.

## Goals / Non-Goals

**Goals:**

- `edge_relation="link"` series produce their pod→pod edge with
  `labels.relation="link"`; the pod→broker edges evidenced by each side's
  peer labels are marked `labels.relation="transport"` when the network edge
  exists.
- Broker (via) resolution reuses the unknown-server enrichment's
  classification chain byte-for-byte in ID space, in lookup-only form — the
  marker can never point at a node ID the materialising path would not have
  produced.
- Deterministic under sample order (D6); edge IDs unchanged; D30 selector
  unchanged; `service-selects-pod` never marked.
- One route-engine store read per distinct `(anchor cluster, broker FQDN,
  port, ips)` per build, shared between link-via keys and ordinary
  unknown-server keys.

**Non-Goals:**

- No numeric metrics, no typed edge field — `relation` is a plain string
  label under the strict `map[string]string` contract.
- No synthesis of missing network edges: a transport pair with no matching
  edge is a marker-only no-op (aggregated debug log), never a fabricated edge.
- No `server_network_peer_address` (the exporter does not emit it in v1) and
  no port participation in identity.
- No memoisation of the in-memory classification chain (the explicit
  `resolveConnString` precedent: µs-scale map hits, dwarfed by the fetch).

## Decisions

### D1: Marking is set-membership at edge-build time, function-local

`linkPairs` / `transportPairs` are two `map[pairKey]struct{}` local to
`parseWithResolver`, sitting next to the existing `pairs` aggregation map.
The edge-build loop consults them per pair: `linkPairs` first ⇒
`labels["relation"]="link"`, else `transportPairs` ⇒ `"transport"`, else no
key. No resolver field, no global, no cross-build state — the sets die with
the function (zero leak surface). Set accumulation is monotone (only inserts)
and the final marking is a pure function of the accumulated sets, so sample
arrival order cannot change the output (D6).

### D2: link wins over transport and over plain series

A pair present in both sets is `link` (checked first). A plain series and a
link series resolving to the same `(src, tgt)` produce ONE edge (the existing
`pairs` dedup) carrying `relation=link` — the logical claim is strictly more
informative. Both follow from the check order plus set monotonicity; no
ordering dependency.

### D3: via resolution is lookup-only — never materialises

`viaNodeID(ownPod, peer)` mirrors `resolveUnknownServerPeer`'s chain but only
DERIVES the node ID: `peer.value()` empty → no marker; bracket-truncate +
port-split; `classifyPeerHost` + `anchorHolds` → `graph.ServiceID(anchor, ns,
svc)`; IP literal → `lookupPeerPodIP` → pod ID; otherwise `routeNodeID(key,
raw)` (D5) → route-index service ID or `graph.ExternalID(raw)`. It calls only
the existing pure helpers — the same functions the materialising path calls —
so the two cannot drift (the `resolveRouteChain` orphan-protection precedent:
service nodes are never pruned by projection, so a marker lookup that
materialised would leak orphan broker nodes for graphs that carry no network
edge). When the network edge is absent the transport pair is a pure marker:
one aggregated debug log, zero synthesis.

### D4: route-engine cost is deduped through the existing prescan `seen` map

`viaRouteKey(ownPod, peer)` extracts the existing prescan skip chain
(`peerRouteKey` → classified-and-anchor-holds skip → `lookupPeerPodIP`-hit
skip → empty-`ips` skip) into one shared method; the pre-existing
unknown-server branch calls it too (behaviour byte-for-byte). A link series
emits at most two via keys (client side, server side — anchor = each side's
own pod cluster) through the same `seen` map. Because the key derivation is
`peerRouteKey` in all cases, a link-via key for broker X and an ordinary
unknown-server key for broker X are structurally identical and collapse to
ONE store read. Same-FQDN different-cluster callers still make distinct keys
(`routeKey.callerCluster` is a deliberate dimension — family-first ingress
selection is per caller cluster). The in-memory classification chain is NOT
memoised (`resolveConnString`'s explicit precedent).

### D5: routeNodeID — the lookup-only twin of routeIndexResolve

`routeNodeID(key, raw)` consults the prefetched `routeIndex` exactly like
`routeIndexResolve` but without materialisation, logging, or tallies:
`RouteHit` / `RouteIngressLBService` whose `dest` cluster holds the service
(same membership test as `resolveServiceLevel` / `resolveServiceLevelInCluster`)
→ `graph.ServiceID(dest.Cluster, dest.Namespace, dest.Service)`; every other
entry, a missing entry, empty `ips`, or a nil index → `graph.ExternalID(raw)`.
Deliberate divergences from the materialising path (documented here, tested):
a chained RouteHit yields only the BACKEND id (the marker targets the routed
service, never the ingress hop), no `role` marking, no chain synthesis.
`maxRouteKeys` truncation stays consistent for free: a truncated key has no
index entry, so `routeNodeID` and the materialising path degrade to the same
`ExternalID(raw)` — markers never mis-align (the existing truncation Warn
still fires).

### D6: no-consumer guard — a server="unknown" link series contributes NO markers

Per link sample, after the cross product: if `serverLabel == "unknown"` AND
the server has no resolvable topology pod (`serverUID == ""` or absent from
`PodsByUID`), the series recovered no consumer — the server side resolved
through `resolveUnknownServerPeer` using the CLIENT-side peer labels, so the
produced edge is A→broker, a network shape with no renderable logical
counterpart. Such a series contributes NOTHING: its pairs enter neither
`linkPairs` (the target is the broker, not the consumer) nor
`transportPairs`, and no via marking runs (aggregated debug:
"link_series_without_resolvable_consumer"). The rendering contract demands
it: the frontend maps `transport` → dashed ONLY because a solid link edge
carries the dependency alongside; a transport marker with no accompanying
link edge would demote a real network dependency to a dashed line that backs
nothing. The A→broker edge therefore stays byte-identical to an ordinary
`server="unknown"` series' outcome.

Every other link sample's pairs — real pod, synth pod, D27 ghost external —
go to `linkPairs` (the logical claim stands even when one endpoint is
degraded), and only that branch runs the per-side via marking: client side
marks `(clientPod, viaNodeID(client peer))` iff the client resolved to a real
topology pod; server side marks `(serverPod, viaNodeID(server peer))` iff
`PodsByUID` resolves the server UID. An unresolved side simply contributes no
marker; the other side still does. Because via marking lives inside the
branch that always records the link pair, the invariant "a transport marker
only ever originates from a series that also emitted its link-marked logical
edge" holds by construction — the frontend can rely on transport edges
coexisting with their link edges in the built graph (projection filters may
still exclude either from a filtered view; marking never depends on the
projection). The prescan's link branch mirrors the same guard
(`serverLabel != "unknown" || serverKnown`) so the two condition sets cannot
drift; the collected key set is unchanged either way, since the
unknown-server branch derives the identical key for the enrichment path.

### D7: server-side mirror reuses peerLabels — one struct, two constructors

`serverPeerLabelsOf(m)` fills the same `peerLabels` struct from the four
`server_*` labels, leaving `networkPeerAddress` and `netPeerPort` empty
(labels the exporter does not emit server-side in v1). `value()` and
`derivePort` skip empty fields, so the D1 precedence and D5 port ladder apply
unchanged — zero forked logic, and if the exporter later adds
`server_network_peer_address`, filling one field lights it up.

### D8: registry gains a `relation` label entry; nothing else moves

`pod-calls-pod` and `pod-calls-service` `Labels` each gain
`{Name: "relation", ValueType: "string"}` (values `link` / `transport`,
absent on ordinary edges). `service-selects-pod` does not (a shared fan-out
edge serves many callers; a per-caller relation on it would be a lie).
`edge_relation` is read in the parse only — the D30 selector, the
`QServiceGraphTotal` constant, and the UUIDv5 edge-ID input (`type|source|
target` — labels never participate) are all untouched. Cytoscape serialises
edge labels verbatim; no serialiser change.

## Risks / Trade-offs

- **Cardinality**: `edge_relation` multiplies upstream series; the `pairs`
  aggregation dedupes per `(src, tgt)` so graph size is unchanged.
- **maxRouteKeys pressure**: each link series adds ≤ 2 keys. Truncation
  degrades markers and materialisation identically (D5) and is already
  logged.
- **Exporter drift**: if the exporter starts stamping `edge_relation` values
  other than `link`, they are ignored (exact-match), never mis-marked.

## Open Questions

(none)
