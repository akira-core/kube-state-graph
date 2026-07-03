## Why

The `resolve-unknown-server-peer-labels` enrichment (`resolveUnknownServerPeer` in
`pkg/build/servicegraph.go`) recovers a `server="unknown"` endpoint from
`client_net_peer_name` / `client_server_address` by classifying the value as Kubernetes
`.svc` DNS (`classifyK8sDNS`) or a bare short Service name (`classifyBareShortName`).
Neither classifier matches a bare IP literal (e.g. `172.20.10.5`), which is the most
common value some exporter configurations record in `client_server_address` when the
caller dialed a Service's ClusterIP directly rather than its DNS name. Today that value
falls through to an `external/172.20.10.5` node instead of resolving to the actual
Service — losing an otherwise-recoverable edge.

## What Changes

- Add a new classification step to `resolveUnknownServerPeer`, tried only after
  `classifyK8sDNS` and `classifyBareShortName` both miss: if the peer value (after the
  existing `:port` strip) parses as an IP literal (`net.ParseIP`), look it up against a
  new reverse index — `(anchor cluster, ClusterIP) → (namespace, service)` — built from
  `topology.ServicesByNameNS`, **scoped to the caller's own anchor cluster only** (not
  the cluster family). A hit falls through to the existing `resolveServiceLevel`
  unchanged (its normal family-union `service-selects-pod` fan-out still applies); a miss
  falls to `external/<raw_value>` exactly as today.
- No new label. `server_server_address` is explicitly out of scope — the peer value still
  comes only from the existing `client_net_peer_name` → `client_server_address`
  precedence; the new IP branch applies generically to whichever of the two supplied the
  value.
- No PromQL/selector change — the reverse index is built from data the topology reader
  already fetches (`kube_service_info` via `ServicesByNameNS`); no new upstream query.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: extends the "Unknown-server peer-label enrichment" requirement
  (added by the not-yet-archived `resolve-unknown-server-peer-labels` change) with a
  bare-IP-literal classification path in addition to the existing `.svc` DNS / bare
  short-name paths.

## Impact

- `pkg/build/servicegraph.go`: `resolveUnknownServerPeer`, new `ipKey`/reverse-index
  build in `parseServiceGraph`, new small IP-literal helper.
- `pkg/build/servicegraph_test.go`: new unit cases (IP hit in anchor cluster, IP miss →
  external, IP present in a family-sibling cluster but NOT the anchor → external, IP
  collision determinism).
- `internal/integration`: one fixture-driven case exercising a ClusterIP-literal peer
  value end-to-end.
- **Sequencing note**: this change's delta spec is written against the requirement text
  as it exists in `openspec/changes/resolve-unknown-server-peer-labels/specs/` (not yet
  promoted to `openspec/specs/`). That change's code is already merged to this branch: it
  should be archived before or together with this one so the requirement text lands in
  `openspec/specs/pod-service-graph/spec.md` in the correct order.
