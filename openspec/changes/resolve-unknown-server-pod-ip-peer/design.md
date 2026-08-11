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

The addressed pod is frequently not in the caller's own cluster. Where clusters share a
flat routable network — one VPC, VPC peering, a BGP or native-routing CNI, L3-routed
on-prem pod CIDRs — a pod reaches a remote pod's IP directly, with or without a service
mesh. Recovering those calls is what makes this rule worth having in a multi-cluster
deployment, and it is what D2 below is about.

## Goals / Non-Goals

**Goals:**

- Resolve an IP-literal peer value that matches a pod's own `pod_ip` to that topology
  pod, producing the ordinary `pod-calls-pod` edge — including when the pod lives in a
  same-family sibling cluster, since cross-cluster pod-to-pod dialling over a flat
  network is real traffic.
- Preserve every existing outcome: Service `ClusterIP` keeps priority, a miss keeps
  today's route-engine-then-external behaviour byte for byte, and the trigger condition
  (`server == "unknown"` plus a real topology client pod) is unchanged.
- Never guess across clusters: the anchor cluster is preferred, and a genuinely ambiguous
  family degrades to external rather than tie-breaking.
- Deterministic (D6): the new index is a pure function of `topology.Pods`, and both the
  intra-cluster and cross-cluster picks resolve by fixed rules rather than slice order.
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
- **No mesh gate.** Requiring evidence that a pod is enrolled in a service mesh before
  allowing a cross-cluster match was considered and dropped — see D2. The only such
  evidence the reader already holds is an `istio-proxy` container in
  `PodNode.Containers()`, and it is neither necessary nor sufficient. Everything
  label-based (`kube_pod_labels`' `security.istio.io/tlsMode`, `kube_namespace_labels`'
  `istio-injection` / `istio.io/dataplane-mode`) is absent from the codebase and would
  need a new query plus an operator-side kube-state-metrics allowlist; an archived design
  already rejected adding `kube_pod_labels`. Ambient-mode enrollment is undetectable
  either way.
- **No cross-cluster matching beyond the family, and none at all for Service
  `ClusterIP`.** Service CIDRs overlap just as readily (`10.96.0.0/12` nearly everywhere),
  and worse, under Istio multi-primary the same `(namespace, service)` exists in every
  cluster with a *different* `ClusterIP` — so a sibling's ClusterIP names a different
  Service instance than the caller addressed. The ClusterIP lookup stays anchor-only.
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

### D2: The index is family-scoped, with the anchor preferred and ambiguity degrading

New field on `sgResolver`, mirroring the shape of `svcCandidates`:

```go
type famIPKey struct{ family, ip string }
type podIPCandidate struct {
    cluster string
    pod     *graph.PodNode
}
podIPCandidates map[famIPKey][]podIPCandidate // family+IP -> holders, sorted by cluster
```

Built in `newSGResolver` from `topology.Pods` in **two stages**, so the intra-cluster and
cross-cluster rules stay separable:

1. `perCluster map[ipKey]*graph.PodNode` — cluster from `p.Labels()["cluster"]`, IP from
   `p.IPAddress()`, skipping a pod whose IP slice is empty or whose first element is
   empty; a duplicate within one cluster reduces to the lexically-smallest `pod.ID()`
   (D3).
2. Group the per-cluster winners by `famIPKey{ClusterFamilyKey(cluster), ip}`, each group
   sorted by cluster — the same `sort.Slice` the `svcCandidates` build uses, so the index
   is a pure function of the data rather than of map-iteration order (D6).

`lookupPeerPodIP(anchorCluster, host) (*graph.PodNode, bool)` then applies three rules:
the anchor cluster's own holder always wins; otherwise a **lone** family holder resolves;
otherwise (two or more) it returns no pod and reports `ambiguous`, and the caller falls to
an external node. A cluster outside the family is excluded by the key itself.

**Why:** cross-cluster pod-to-pod reachability is a **network-layer** property, not a mesh
property. Where two clusters share a flat routable plane — one VPC, VPC peering, a
BGP/native-routing CNI, L3-routed on-prem pod CIDRs — a pod dials a remote pod's IP
directly with no Istio involved, and that is exactly the traffic this rule must recover.

The safety condition is that the family's pod CIDRs do not overlap, and this design makes
that **observable rather than assumed**: overlapping CIDRs mean the same address appears
in more than one family cluster, which is precisely the `len(cands) > 1` case. *The
uniqueness test IS the CIDR-disjointness test*, evaluated per address, from direct
evidence. Preferring the anchor keeps the pre-widening path byte-for-byte identical and is
the right call even under real overlap — a caller most plausibly reached a pod in its own
cluster.

**Superseded rationale (recorded deliberately).** The first version of this change scoped
the lookup to the anchor cluster only, arguing that "pod CIDRs of sibling clusters overlap
by default, so a cross-cluster match would produce silently wrong data rather than a
missed resolution". Observation in a real deployment refuted it: the clusters' networks
were flat, cross-cluster Pod IP calls were ordinary traffic, and every one of them landed
on `external/<ip>`. The error was treating "the CIDRs might overlap" as an unobservable
premise when the loaded topology answers it directly. The anchor-first + lone-holder rule
keeps the original concern's protection without its false negatives.

**Alternatives considered:**

- *Gate cross-cluster matching on service-mesh evidence (an `istio-proxy` container in
  `PodNode.Containers()`).* Rejected: the signal is **neither necessary nor sufficient**.
  Not necessary — a flat network needs no Istio at all, so the gate would suppress the
  very case that motivated the widening. Not sufficient — in a multi-network Istio mesh
  the caller's sidecar is handed the east-west gateway address, never a remote Pod IP, so
  a sidecar's presence does not establish that the observed IP is a remote pod's. It also
  misses ambient-mode enrollment entirely, and every richer signal
  (`security.istio.io/tlsMode`, `istio-injection`, `istio.io/dataplane-mode`) needs a new
  query plus a kube-state-metrics allowlist.
- *Lexicographic tie-break among family holders instead of degrading.* Rejected: the
  intra-cluster tie-break (D3) picks among pods that are all in the cluster the caller
  demonstrably reached, whereas a cross-cluster tie-break would fabricate a dependency on
  a cluster the caller may never have contacted. The repo already degrades rather than
  guessing in the same situation — `pickIngressCluster` and
  `RouteAmbiguousIngressService`.
- *Union all family holders into several edges.* Rejected: the caller reached exactly one
  peer; emitting N edges misreports fan-out that did not happen.
- *Index by `net.IP` bytes to fold `10.0.0.1` and IPv6 zero-compression variants.*
  Rejected as unmotivated: the ClusterIP index does not do it, and both values originate
  from the same exporter/KSM string forms. IPs are compared as raw strings throughout.

### D3: Determinism on a duplicate Pod IP **within one cluster**

Two pods in one cluster can legitimately report the same address: every
`hostNetwork: true` pod reports its node's IP, and an IP freed by a terminated pod can be
reassigned within the query window. Stage 1 of the index build keeps the
**lexically-smallest `pod.ID()`**, following the exact pattern the `ipIndex` build already
uses for duplicate ClusterIPs. The result is independent of `topology.Pods` slice order
and stable across rebuilds of the same upstream data (D6).

Note the deliberate asymmetry with D2's cross-cluster rule: an intra-cluster duplicate
tie-breaks, a cross-cluster duplicate degrades. The reason is what the caller
demonstrably reached. Within one cluster every candidate is in the cluster the traffic
went to, so picking one is a choice among equals; across clusters the candidates are in
*different* clusters and picking one asserts a network path that may not exist. The
reduction happens first, so an intra-cluster duplicate never inflates a cluster's
candidate count and never makes the family look ambiguous.

**Why:** the archived D6 determinism rule; golden and property tests depend on rebuild
stability.

**Alternatives considered:**

- *Emit edges to every candidate pod.* Rejected: fabricates dependencies that do not
  exist — the caller reached exactly one peer.
- *Drop the endpoint on ambiguity (fall to external), as D2 does across clusters.*
  Rejected here: for `hostNetwork` pods the collision is the normal case, not an anomaly,
  so dropping would make the feature a no-op precisely where direct-IP calls are most
  common. An external node is strictly less informative than a same-cluster pod that is
  provably one of the candidates.
- *Disambiguate by port.* See Non-Goals — no port data is available.

### D4: The prescan mirrors the new step

`collectRouteQueriesWith` skips endpoints the in-cluster ladder already resolves, so the
route engine is consulted only where the parse would fall external. Add the matching skip
directly after the existing `classifyPeerHost` / `anchorHolds` skip:

```go
if pod, _ := r.lookupPeerPodIP(anchorCluster, key.host); pod != nil {
    continue
}
```

Sharing the one `lookupPeerPodIP` makes the prescan track the parse for free, including
the D2 selection rules: an ambiguous family yields a nil pod, so the key IS collected —
which is correct, because the parse degrades to external there and the route engine
should get its chance at it.

Output correctness does not depend on the skip — the parse returns before `routeExternal`,
so an entry collected for a resolvable key would simply go unread — but without it every
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
  `parseTopology` keeps one canonical pod per `(cluster, namespace, pod)` and stage 1 of
  the index keeps one pod per `(cluster, IP)`, so a long window can attribute the call to
  the wrong occupant. Same class of risk the existing ClusterIP path carries; unchanged by
  this design.
- **A one-sided CIDR overlap can still mislead across clusters.** The family uniqueness
  test sees only *occupied* addresses. If two family clusters have overlapping pod CIDRs
  but only the sibling currently has a pod at the address — the anchor's slot happens to
  be free, or its pod is not in the window — the sibling looks like a lone holder and the
  call is attributed across the cluster boundary. The residual case needs three
  coincidences at once, and the alternative (a mesh gate) was rejected as neither
  necessary nor sufficient (D2). An operator seeing this should look at the
  `pod_cluster` field on the resolution's debug line, which is exactly why it is logged.
- **Index build cost.** Two extra maps of at most `len(topology.Pods)` entries per build,
  the first populated in the loop that already walks `topology.Pods` for `podByID` and
  the second a regroup of the first. Negligible next to the 18-query upstream fan-out.
- **Fewer external nodes in existing deployments.** Operators who were reading
  `external/<ip>` nodes as "unresolved direct-IP traffic" will see those become
  `pod-calls-pod` edges, some of them crossing cluster boundaries. This is the intent of
  the change, but it does alter the graph for existing data without any configuration
  change.

## Migration Plan

Purely additive and unconditional: no flag, no config, no schema or API change. Existing
outcomes are preserved except for peer IPs that match a pod in the anchor cluster's
family, which previously produced an `external` node and now produce a `pod-calls-pod`
edge to a pod that is already in the graph. Golden files are unaffected (no fixture
exercises a direct-Pod-IP peer). Clusters whose pods report no `pod_ip` are unaffected —
the index is empty and every lookup misses.

**Rollback:** revert the commit. The index build, the `lookupPeerPodIP` helper, the
ladder step and the prescan skip are independent additions with no persisted state and no
other call sites; removing them restores the previous behaviour exactly.
