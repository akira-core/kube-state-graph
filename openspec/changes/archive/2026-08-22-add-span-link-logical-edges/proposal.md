## Why

Producer → consumer traffic that flows through a message broker (NATS, MongoDB
change streams, Kafka-style queues) never appears as a direct RPC edge: the
producer's trace ends at the broker and the consumer's trace starts there. The
servicegraph exporter now bridges the two via **span links** across trace IDs
and emits `traces_service_graph_request_total` series carrying a new
`edge_relation="link"` label: client = producer pod, server = consumer pod,
both with pod UIDs, plus **each side's own** broker peer-address labels
(client side reuses the existing `client_server_address` /
`client_net_peer_name` / `client_dns_answers` / `client_server_port`; the
server side adds the mirrored `server_server_address` / `server_net_peer_name`
/ `server_dns_answers` / `server_server_port` — no "via" string label, and no
`server_network_peer_address` in v1).

The frontend wants to draw the **logical dependency** (producer → consumer)
as a solid line and the **network dependency** (pod → broker connection) as a
dashed line. Today ksg cannot distinguish them: a link-derived edge and an
ordinary RPC edge are identical, and the pod → broker edges give no hint that
they are transport for a logical edge.

## What Changes

- Read the `edge_relation` label in the service-graph parse. A series whose
  value is exactly `"link"` produces its pod → pod (or degraded) edge with
  `labels.relation = "link"`.
- Resolve each side's broker peer-address labels through **the same
  classification chain as the unknown-server enrichment** (DNS grammar, bare
  short name, ClusterIP, Pod IP, route-engine index, external fallback) in
  **lookup-only** form — deriving the broker node ID **without materialising
  any node or edge** — and mark the matching pod → broker edge (produced by
  ordinary network series) with `labels.relation = "transport"`.
- Marking is a set-membership test at edge-build time: two function-local
  `map[pairKey]struct{}` sets (`linkPairs` / `transportPairs`) accumulated
  monotonically during the parse; `link` wins over `transport` on collision
  and over plain series (D6 order-free).
- A link series whose server side is `server="unknown"` with no resolvable
  pod recovered no consumer: it contributes NO markers at all (neither link
  nor transport, no via pairs) — the producer→broker edge it resolves to via
  the existing enrichment stays an ordinary unmarked network edge, so a
  dashed `transport` line can never appear without its solid `link`
  counterpart in the built graph.
- The prescan collects route keys for both sides' peer labels of link series
  (anchor = each side's own pod cluster), deduped through the existing `seen`
  map with the keys ordinary unknown-server series derive — one store read per
  distinct broker FQDN per anchor cluster.
- `service-selects-pod` fan-out edges and synthesized route-chain edges are
  never marked (shared edges).
- Registry: `pod-calls-pod` and `pod-calls-service` gain a `relation` entry in
  their `labels` array (`/v1/edge-types` + golden regen). Edge IDs (UUIDv5
  over type|source|target) are unchanged; the D30 selector is unchanged.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: new requirement "Span-link logical edge relation
  marking" — the `edge_relation="link"` series contract, per-side lookup-only
  broker resolution, transport marking, degrade paths, and determinism rules.
- `graph-api`: the edge-type discovery catalogue's `pod-calls-pod` and
  `pod-calls-service` entries enumerate the new optional `relation` label.

## Impact

- `pkg/graph/registry.go`: `relation` label entries on `pod-calls-pod` /
  `pod-calls-service`; `internal/api/testdata/golden/edge-types.json` regen.
- `pkg/build/routeprescan.go`: `serverPeerLabelsOf` (mirrored server-side
  labels into the existing `peerLabels` struct), `viaRouteKey` (the extracted
  skip chain, shared with the existing unknown-server branch), and the link
  branch in `collectRouteQueriesWith`.
- `pkg/build/servicegraph.go`: `relation*` constants, lookup-only `viaNodeID`
  / `routeNodeID`, link handling in `parseWithResolver`, relation marking in
  the edge-build loop, two aggregated debug logs.
- Tests: `pkg/build/servicegraph_test.go`, `pkg/build/routeprescan_test.go`,
  golden fixture `link-relation-cytoscape.json`, integration
  `TestSpanLinkRelationEdges`.
- No new node/edge type, no PromQL/selector change, no edge-ID change, no new
  config knob.
