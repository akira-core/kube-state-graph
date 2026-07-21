# Design — route-hit-ingress-chain

Extends `translate-global-fqdn-to-k8s-service` (parent) and `ingress-lb-service-fallback`
(sibling; its D-numbers referenced as S1–S8). Settled decisions:

## D1 — Identity recovery reuses the sibling's window-wide dedup, tri-stated

The identity-dedup core of `resolveIngressLBService` is factored into
`ingressServiceIdentity(mw, ips) (ingressIdentity, ingressIdentityStatus)` with three states —
`identityUnique` / `identityAmbiguous` (same-IP collision OR disagreeing singletons) /
`identityIncomplete` (any IP with zero rows) — preserving the sibling's exact precedence (S7:
same-IP collision > any-empty > disagreement). The wrapper keeps `resolveIngressLBService`'s
outward contract byte-identical. On the hit path only `identityUnique` populates
`IngressNamespace`/`IngressService`; the other two states leave them empty — **a hit is never
demoted** (unlike the fallback, where ambiguity is a terminal outcome, here the chain is a bonus
on top of an already-successful resolution). No memwindow change; no new store read (parent D6 /
sibling S6).

## D2 — Ingress fan-out is locked-cluster-only; backend stays family-wide

An LB IP is a per-cluster address: the pods behind it are the locked cluster's own endpoints, so
the ingress Service resolves via a new `resolveServiceLevelInCluster(cluster, ns, svc)` — same
anchor-membership test and idempotent node materialisation as `resolveServiceLevel`, but the
`service-selects-pod` fan-out iterates `endpointsByService[{cluster, ns, svc}]` ONLY. A family
sibling's same-named `istio-ingressgateway` Service would otherwise pull ITS gateway pods behind
an IP they do not serve. The **backend** Service is a mesh-wide destination and keeps the
family-wide `resolveServiceLevel` union (parent D29 semantics untouched). The
`RouteIngressLBService` branch switches to the scoped variant too — same "pods behind this IP"
semantics — superseding the sibling's family-wide fan-out wording (its single-cluster tests are
unaffected: locked set == family set there).

## D3 — Chain preconditions are checked purely, before any materialisation

Service nodes are never pruned by projection ("Service nodes are unaffected", D-default-projection),
so a materialise-then-degrade would leak an orphan ingress node. `resolveRouteChain` therefore
gates on pure lookups first — identity present; identity != backend identity; `anchorHolds(...)`;
non-empty locked-cluster endpoint set — and only then materialises. Backend is resolved FIRST: a
backend topology miss takes the existing `route_engine_dest_cluster_lacks_service` external path
with the ingress never materialised. Every degrade path leaves zero stray nodes/edges and emits
today's shape byte-for-byte.

Degrade matrix (chain preconditions; all → today's direct-edge shape, debug-log only):

| Condition | Result |
|---|---|
| `IngressService == ""` (no unique identity in window) | direct edge + family-wide backend fan-out |
| Ingress identity == backend identity | same (a self-sandwich chain is meaningless) |
| Locked cluster's topology lacks the ingress Service | same |
| Ingress Service held but zero locked-cluster endpoints | same (chain would disconnect caller from backend) |

## D4 — Synthesized edges: traced-edge-wins, D9-consistent labels

The ingress-pod→backend edges are not trace-derived. They accumulate in a dedicated
`routeChainEdges` map (keyed `srcPodID|backendSvcID`, mirroring `svcEdges`) and are emitted after
the traced-pairs loop, **skipping any `(src, tgt)` already present in the traced `pairs` map** —
a real traced series for the same pair would otherwise produce a second edge with the SAME UUIDv5
edge ID (`pod-calls-service|src|tgt`), breaking ID uniqueness. Traced-wins keeps pre-existing
edges byte-identical. Labels: `{"cluster": dest.Cluster}` — the client side is a pod and every
locked-cluster endpoint pod lives in `dest.Cluster` by construction, so this is the parent-D9
client-side-cluster rule, not a new convention. Endpoint pods iterate sorted-unique
(determinism); all accumulators are idempotent, so repeated series sharing a route key re-enter
harmlessly.

## D5 — Direct edge removed by returning the ingress node as the resolution target

`routeIndexResolve` returns the ingress Service id (instead of the backend's) as the endpoint's
resolved ids, so the main loop's `srcIDs × tgtIDs` cross product emits `caller → ingress service`
and the direct `caller → backend` edge simply never exists. No post-hoc edge deletion. On
degrade, the backend id is returned as today.

## D6 — Observability: debug-only, no reason-counter changes

Chain degrades are `slog.Debug("route chain degraded to direct edge", "chain_degrade_reason", …)`
ONLY — never `noteExternal` (its `extReasons` invariant is "events that produced external
nodes"). The hit-success debug log gains `ingress_namespace` / `ingress_service` / `chain`
attributes (additive, log-only). No new engine outcome; `outcomeRank` untouched; no self-metric
label change (D26).

## D7 — Testability split

A real `RouteHit` needs the `router_check_tool` binary, so the `pkg/route` stamping line is
covered by the integration `RouteSuite` (whose VM fixtures gain the alpha ingress topology and
now assert the full chain); `ingressServiceIdentity` is unit-tested exhaustively in
`ingresslb_test.go`; the graph-side chain/degrade matrix is unit-tested in
`routeprescan_test.go` by injecting `routeEntry` values with populated `Ingress*` fields. The
cross-cluster integration test (beta ingress absent from VM topology) doubles as the e2e
topology-missing degrade proof.

## D8 — Non-goals

No per-instant ingress-identity evaluation (window-wide dedup, sibling S6, is kept); no chain for
`RouteIngressLBService` (there is no routed backend behind the LB entry point); no
DestinationRule-subset or port materialisation (parent: Port/Subset stay discarded); no Ingress
CR / nginx.conf backend expansion.
