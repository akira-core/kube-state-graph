## Context

`resolveUnknownServerPeer` (`pkg/build/servicegraph.go`) is the narrow carve-out that
lets a `server="unknown"` endpoint resolve from the client-recorded peer address, when
and only when the client side resolved to a **real** topology pod. It picks a peer value
(`client_net_peer_name` first, `client_server_address` second), strips an optional
`:port` via `splitPeerAddressPort`, then classifies the host through
`classifyPeerHost`:

1. `classifyK8sDNS(host)` — the D29 2-label (`<service>.<namespace>`) / 3-label headless
   (`<pod>.<service>.<namespace>`) `.svc` grammar.
2. `classifyBareShortName(host)` — a dot-free, non-IP short name, resolved as a Service
   in the client pod's own namespace.
3. `ipIndex[ipKey{anchorCluster, host}]` — an IP literal matched against the anchor
   cluster's Service `ClusterIP` set (`resolve-unknown-server-ip-peer`).

All three produce a `(namespace, service)` pair, resolved by `resolveServiceLevel`.
Anything else — including an IP literal that matches no ClusterIP — reaches the external
fallback (`routeExternal`, which consults the route-resolution engine first when it is
enabled, then emits `external/<raw_value>`).

Step 3 covers callers that dialed a Service by its ClusterIP. It does not cover callers
that dialed a **Pod IP** directly, bypassing the Service — a StatefulSet peer addressed
by endpoint IP, a client caching a resolved endpoint address, a direct call from a
sidecar-less workload. Those endpoints match no ClusterIP and land on `external/<ip>`,
even though `topology.Pods` already carries the exact pod that owns the address:
`kube_pod_info`'s `pod_ip` label is read in `parseTopology` and baked onto
`PodNode.IPAddressValue`.

## Goals / Non-Goals

**Goals:**

- Resolve an IP-literal peer value that matches a pod's own `pod_ip` in the caller's
  anchor cluster to that topology pod, producing the ordinary `pod-calls-pod` edge.
- Preserve every existing outcome: Service `ClusterIP` keeps priority, a miss keeps
  today's route-engine-then-external behaviour byte for byte, and the trigger condition
  (`server == "unknown"` plus a real topology client pod) is unchanged.
- Deterministic (D6): the new reverse index is a pure function of `topology.Pods`, and a
  same-IP collision resolves by a fixed rule rather than slice order.
- No new upstream query: the data is already fetched.

**Non-Goals:**

- **No port-based disambiguation between pods sharing an IP.** Selecting among candidate
  pods by comparing `client_server_port` against each pod's listening ports was
  considered and dropped: no metric the reader currently queries carries a pod or
  container listening port. `kube_pod_container_info` is parsed for `container` / `image`
  only, `kube_endpointslice_ports` is not referenced anywhere in the repo, and stock
  kube-state-metrics exposes no container-port series at all. Adding one would mean a new
  PromQL query, a new metric-label contract, a new topology index and prefix handling —
  disproportionate to a collision case that a deterministic tiebreak already handles
  safely (D3). Deferred to a possible follow-up change.
- No cross-cluster / family-wide Pod IP matching. See D2.
- No service-level resolution on this path — a Pod IP hit is a pod, not a service. See
  D5.
- No change to `classifyK8sDNS`, `classifyBareShortName`, the ClusterIP step, or the D29
  connection-string path (`resolveConnString`). Scoped to the unknown-server peer-label
  trigger only, the same scoping discipline the two parent changes used.
- No new node type, edge type, typed attribute, or `labels` key; no PromQL/selector
  change; no serialiser change.

## Decisions

### D1: The Pod-IP step lives in `resolveUnknownServerPeer`, not in `classifyPeerHost`

`classifyPeerHost(host, clientNamespace, anchorCluster) (ns, svc string, classified bool)`
returns a **service identity**. A pod hit does not fit that signature, and widening it to
a sum type would break the anti-drift property that makes the same function serve both
the parse and the `collectRouteQueries` prescan (`translate-global-fqdn-to-k8s-service`
D2).

The new step therefore sits in `resolveUnknownServerPeer`, inside the `!classified`
branch's existing `net.ParseIP(host) != nil` sub-branch, immediately before
`routeExternal("unknown_server_peer_ip_literal_no_match", ...)`:

```go
ns, svc, classified := r.classifyPeerHost(host, clientNamespace, anchorCluster)
if !classified {
    if net.ParseIP(host) != nil {
        if pod := r.lookupPeerPodIP(anchorCluster, host); pod != nil {
            return []string{pod.ID()}
        }
        return routeExternal("unknown_server_peer_ip_literal_no_match", ...)
    }
    return routeExternal("unknown_server_peer_not_k8s_dns", ...)
}
```

This placement produces the required precedence without any extra ordering logic:

- **Service `ClusterIP` beats Pod IP.** A ClusterIP hit happens inside
  `classifyPeerHost` and returns `classified=true`, so it never reaches this branch.
- **Pod IP beats the route engine and beats `external`.** `routeExternal` — the only
  route-engine hook in the parse — is the very next statement.
- The third external exit, `unknown_server_peer_anchor_lacks_service`, is **unreachable
  for an IP host**: `ipIndex` and `svcCandidates` are both built from
  `topology.ServicesByNameNS`, so an `ipKey{anchorCluster, ip}` hit guarantees
  `anchorHolds(anchorCluster, ns, svc)` succeeds and `resolveServiceLevel` returns a
  node. No Pod-IP step is needed there.

**Why:** it satisfies the ordering requirement structurally rather than by convention,
and it keeps `classifyPeerHost` a pure service-identity classifier shared with the
prescan.

**Alternatives considered:**

- *Widen `classifyPeerHost` to return a target union.* Rejected: the prescan's
  `classified && anchorHolds(...)` skip test is written against a `(ns, svc)` pair; a
  union would force both call sites to branch on shape, which is exactly the drift the
  shared helper exists to prevent.
- *Try Pod IP before ClusterIP.* Rejected: contradicts the requirement, and a ClusterIP
  is the more specific signal — a caller that reached a Service address is describing a
  Service dependency, and the service node fans out to all backing pods anyway.
- *Also try Pod IP at the `anchor_lacks_service` exit.* Rejected as dead code, per the
  reachability argument above.

### D2: The reverse index is anchor-cluster-scoped, reusing `ipKey`

New field on `sgResolver`:

```go
podIPIndex map[ipKey]*graph.PodNode // (cluster, Pod IP) -> pod in that cluster
```

built in `newSGResolver` from `topology.Pods` — cluster from `p.Labels()["cluster"]`, IP
from `p.IPAddress()`, skipping a pod whose IP slice is empty or whose first element is
empty. It reuses the existing `ipKey{cluster, ip}` type: the rationale documented on
`ipKey` for ClusterIPs applies unchanged to Pod IPs, which are likewise per-cluster
addresses drawn from each cluster's own — frequently overlapping — pod CIDR. Matching an
IP across clusters would produce silently wrong data, not merely a missed resolution.

IPs are compared as raw strings, exactly as `ipIndex` does; no `net.IP` normalisation.
Both sides come from the same class of source (a KSM label vs. an exporter label), and
introducing normalisation on one index but not the other would be an inconsistency
without a demonstrated need.

`lookupPeerPodIP(anchorCluster, host string) *graph.PodNode` wraps the map read so the
prescan (D4) shares one lookup with the parse.

**Why:** correctness (no cross-cluster IP aliasing), consistency with the ClusterIP
index, and a single shared lookup for parse and prescan.

**Alternatives considered:**

- *Family-wide Pod IP matching.* Rejected for the same reason `resolve-unknown-server-ip-peer`
  D2 rejected it for ClusterIPs, and more strongly: pod CIDRs of sibling clusters overlap
  by default in most installations.
- *Index by `net.IP` bytes to fold `10.0.0.1` and IPv6 zero-compression variants.*
  Rejected as unmotivated: the ClusterIP index does not do it, and both values originate
  from the same exporter/KSM string forms.

### D3: Determinism on a duplicate `(cluster, Pod IP)`

Two pods in one cluster can legitimately report the same address: every
`hostNetwork: true` pod reports its node's IP, and an IP freed by a terminated pod can be
reassigned within the query window. On collision the index keeps the **lexically-smallest
`pod.ID()`**, following the exact pattern the `ipIndex` build already uses for duplicate
ClusterIPs. The result is independent of `topology.Pods` slice order and stable across
rebuilds of the same upstream data (D6).

**Why:** the archived D6 determinism rule; golden and property tests depend on rebuild
stability.

**Alternatives considered:**

- *Emit edges to every candidate pod.* Rejected: fabricates dependencies that do not
  exist — the caller reached exactly one peer.
- *Drop the endpoint on ambiguity (fall to external).* Rejected: for `hostNetwork` pods
  the collision is the normal case, not an anomaly, so dropping would make the feature a
  no-op precisely where direct-IP calls are most common. An external node is strictly
  less informative than a same-cluster pod that is provably one of the candidates.
- *Disambiguate by port.* See Non-Goals — no port data is available.

### D4: The prescan mirrors the new step

`collectRouteQueriesWith` skips endpoints the in-cluster ladder already resolves, so the
route engine is consulted only where the parse would fall external. Add the matching skip
directly after the existing `classifyPeerHost` / `anchorHolds` skip:

```go
if net.ParseIP(key.host) != nil && r.lookupPeerPodIP(anchorCluster, key.host) != nil {
    continue
}
```

Output correctness does not depend on it — the parse returns before `routeExternal`, so
an entry collected for such a key would simply go unread — but without it every
direct-Pod-IP endpoint costs a `ClustersWithIngressIP` probe, ClickHouse round-trips and
an istiod translation per build, and consumes `maxRouteKeys` budget that real external
peers need.

**Why:** the prescan's stated contract is "resolve only what the parse cannot", and the
per-key cost is on the request path with no cache to amortise it.

**Alternatives considered:**

- *Leave the prescan untouched.* Rejected on cost and on the misleading `maxRouteKeys`
  truncation log it would produce.

### D5: A Pod-IP hit resolves to a pod — no service node, no fan-out

The hit returns `[]string{pod.ID()}` directly. It must NOT call `resolveServiceLevel` or
`resolveServiceLevelInCluster`: the caller addressed a pod, not a Service, so
materialising a service node would invent a Service relationship the trace does not
evidence and would emit `service-selects-pod` edges to sibling pods that were never
contacted.

The edge type follows automatically. `parseWithResolver` decides it post-hoc by testing
membership of the target id in `res.services`; a real topology pod is not in that map, so
the edge is `pod-calls-pod`, and `labels.cluster` is present (the client side is a
resolved pod, per D9). No code change is required for either.

**Why:** faithful to the observed traffic, and it keeps the change to a lookup — no new
materialisation path.

**Alternatives considered:**

- *Resolve the pod's owning Service and emit `pod-calls-service`.* Rejected: the pod may
  back several Services or none, the choice would be arbitrary, and it would misreport a
  direct call as a Service call.

## Risks / Trade-offs

- **`hostNetwork` collisions pick one of several pods.** On a node running multiple
  `hostNetwork` pods, an IP-literal peer resolves to the lexically-smallest of them,
  which may not be the pod actually contacted. Mitigation: the result is deterministic,
  same-cluster, and strictly more informative than the `external/<ip>` node it replaces;
  port-based disambiguation is scoped out (Non-Goals) and can be added later without
  changing the index or the ladder position.
- **Stale `pod_ip` within a long window.** If an IP was reassigned during the window,
  `parseTopology` keeps one canonical pod per `(cluster, namespace, pod)` and the index
  keeps one pod per `(cluster, IP)`, so a long window can attribute the call to the
  wrong occupant. Same class of risk the existing ClusterIP path carries; unchanged by
  this design.
- **Index build cost.** One extra map of at most `len(topology.Pods)` entries per build,
  populated in the loop that already walks `topology.Pods` for `podByID`. Negligible
  next to the 18-query upstream fan-out.
- **Fewer external nodes in existing deployments.** Operators who were reading
  `external/<ip>` nodes as "unresolved direct-IP traffic" will see those become
  `pod-calls-pod` edges. This is the intent of the change, but it does alter the graph
  for existing data without any configuration change.

## Migration Plan

Purely additive and unconditional: no flag, no config, no schema or API change. Existing
outcomes are preserved except for peer IPs that match a pod in the anchor cluster, which
previously produced an `external` node and now produce a `pod-calls-pod` edge to a pod
that is already in the graph. Golden files are unaffected (no fixture exercises a
direct-Pod-IP peer). Clusters whose pods report no `pod_ip` are unaffected — the index is
empty and every lookup misses.

**Rollback:** revert the commit. The index build, the `lookupPeerPodIP` helper, the
ladder step and the prescan skip are independent additions with no persisted state and no
other call sites; removing them restores the previous behaviour exactly.
