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

The addressed pod is often in a *different* cluster. Where clusters share a flat routable
network — one VPC, VPC peering, a BGP or native-routing CNI, L3-routed on-prem pod CIDRs
— a pod reaches a remote pod's IP directly, with or without a service mesh, and every one
of those calls currently lands on `external/<ip>`.

Pod IPs are already loaded: `kube_pod_info`'s `pod_ip` label is read by
`parseTopology` and baked onto `PodNode.IPAddressValue`. No upstream data is missing;
only the lookup is.

## What Changes

- Add one classification step to `resolveUnknownServerPeer`, evaluated only after the
  Service `ClusterIP` lookup misses and before the external fallback (and therefore
  before the route-resolution engine, which is consulted at the external fallback
  points): if the port-stripped peer host parses as an IP literal, look it up against a
  new index — `(cluster family, Pod IP) → holding clusters` — built from
  `topology.Pods`, using the same `build.ClusterFamilyKey` rule as the
  `service-selects-pod` fan-out.
- Selection among the family's holders: the **anchor cluster's own** pod always wins;
  otherwise a **lone** family holder resolves across the cluster boundary; **two or
  more** holders degrade to the external node with no tie-break. Being the family's lone
  holder is itself the evidence that its pod CIDRs do not overlap at that address.
- A hit resolves the endpoint directly to that topology pod, producing the ordinary
  `pod-calls-pod` edge (which may now cross clusters). It does NOT go through
  `resolveServiceLevel`: no service node is materialised and no `service-selects-pod`
  fan-out is emitted.
- No service-mesh gate: cross-cluster pod-to-pod reachability is a network-layer
  property, and sidecar presence is neither necessary nor sufficient evidence of it (see
  design D2).
- Service `ClusterIP` keeps priority over Pod IP — the ClusterIP step runs first and a
  hit never reaches the new step. The ClusterIP lookup itself stays anchor-cluster-only.
- Pods with no `pod_ip` are not indexed. A miss keeps today's behaviour byte for byte
  (route engine, then `external/<raw_peer_address>`).
- Determinism: if two pods in ONE cluster report the same IP (`hostNetwork: true` pods
  all report the node IP; IP reuse within a window), the lexically-smallest pod ID wins
  as that cluster's holder, before the cross-cluster selection is applied.
- The `collectRouteQueries` prescan shares the same lookup, so the route-resolution
  engine is not asked about endpoints the in-cluster ladder now resolves — while an
  ambiguous family, which still falls external, is still offered to the engine.
- No PromQL/selector change, no new node type, no new edge type, no new attribute or
  `labels` key.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: extends the "Unknown-server peer-label enrichment" requirement
  with a Pod-IP classification step, tried after the Service `ClusterIP` step, scoped to
  the anchor cluster's family, and resolving to a topology pod rather than a service
  node.

## Impact

- `pkg/build/servicegraph.go`: new `famIPKey` / `podIPCandidate` types and
  `podIPCandidates` field on `sgResolver`, its two-stage build in `newSGResolver`, a
  `lookupPeerPodIP` helper carrying the anchor / lone-holder / ambiguous rules, and the
  new ladder step in `resolveUnknownServerPeer` (including the
  `unknown_server_peer_pod_ip_ambiguous` degrade reason).
- `pkg/build/routeprescan.go`: the matching skip in `collectRouteQueriesWith`.
- `pkg/build/servicegraph_test.go`, `pkg/build/routeprescan_test.go`: new unit cases
  (anchor-cluster hit, `:port` strip, ClusterIP-beats-Pod-IP precedence, lone family
  sibling resolves, anchor beats sibling, family ambiguity degrades, other family never
  matched, load-order independence, intra-cluster duplicate determinism, pod without
  `pod_ip`, miss → external, prescan skip vs. collect).
- `internal/integration`: two fixture-driven end-to-end cases (same-cluster and
  cross-cluster within one family).
- Out of scope: port-based disambiguation between pods sharing an IP — no upstream
  metric currently read carries a pod/container listening port (see design Non-Goals).
