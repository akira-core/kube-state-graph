## Why

The unknown-server peer-label enrichment (`resolveUnknownServerPeer` in
`pkg/build/servicegraph.go`) classifies the client-recorded peer address of a
`server="unknown"` endpoint as Kubernetes `.svc` DNS, a bare short Service name, or —
since `resolve-unknown-server-ip-peer` — a Service `ClusterIP` literal looked up in the
caller's own (anchor) cluster.

That ClusterIP lookup only covers callers that dialed a **Service**. In practice some
callers dial another pod's **Pod IP** directly, bypassing the Service entirely (a
StatefulSet peer addressed by its endpoint IP, a client that resolved and cached an
endpoint address, a sidecar-less direct call). Such an endpoint matches no Service
ClusterIP, so it falls through to an `external/<ip>` node — losing a real, recoverable
pod-to-pod dependency that the topology already knows about.

Pod IPs are already loaded: `kube_pod_info`'s `pod_ip` label is read by
`parseTopology` and baked onto `PodNode.IPAddressValue`. No upstream data is missing;
only the lookup is.

## What Changes

- Add one classification step to `resolveUnknownServerPeer`, evaluated only after the
  Service `ClusterIP` lookup misses and before the external fallback (and therefore
  before the route-resolution engine, which is consulted at the external fallback
  points): if the port-stripped peer host parses as an IP literal, look it up against a
  new reverse index — `(anchor cluster, Pod IP) → pod` — built from `topology.Pods`,
  **scoped to the caller's own anchor cluster only**, exactly as the ClusterIP index is.
- A hit resolves the endpoint directly to that topology pod, producing the ordinary
  `pod-calls-pod` edge. It does NOT go through `resolveServiceLevel`: no service node is
  materialised and no `service-selects-pod` fan-out is emitted.
- Service `ClusterIP` keeps priority over Pod IP — the ClusterIP step runs first and a
  hit never reaches the new step.
- Pods with no `pod_ip` are not indexed. A miss keeps today's behaviour byte for byte
  (route engine, then `external/<raw_peer_address>`).
- Determinism: if two pods in the anchor cluster report the same IP (`hostNetwork: true`
  pods all report the node IP; IP reuse within a window), the lexically-smallest pod ID
  wins.
- The `collectRouteQueries` prescan mirrors the new step, so the route-resolution engine
  is not asked about endpoints the in-cluster ladder now resolves.
- No PromQL/selector change, no new node type, no new edge type, no new attribute or
  `labels` key.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: extends the "Unknown-server peer-label enrichment" requirement
  with a Pod-IP classification step, tried after the Service `ClusterIP` step and
  resolving to a topology pod rather than a service node.

## Impact

- `pkg/build/servicegraph.go`: new `podIPIndex` field on `sgResolver`, its build in
  `newSGResolver`, a `lookupPeerPodIP` helper, and the new ladder step in
  `resolveUnknownServerPeer`.
- `pkg/build/routeprescan.go`: the matching skip in `collectRouteQueriesWith`.
- `pkg/build/servicegraph_test.go`, `pkg/build/routeprescan_test.go`: new unit cases
  (anchor-cluster hit, `:port` strip, ClusterIP-beats-Pod-IP precedence, family-sibling
  pod NOT matched, duplicate-IP determinism, pod without `pod_ip`, miss → external,
  prescan skip).
- `internal/integration`: one fixture-driven end-to-end case.
- Out of scope: port-based disambiguation between pods sharing an IP — no upstream
  metric currently read carries a pod/container listening port (see design Non-Goals).
