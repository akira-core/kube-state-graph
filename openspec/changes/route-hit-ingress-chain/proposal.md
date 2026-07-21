# Full ingress chain for routed global-FQDN hits

## Why

When the Istio route-resolution engine (`translate-global-fqdn-to-k8s-service`) resolves a global
FQDN peer (`RouteHit`), the graph today shows `caller pod → backend service → backend pods` — the
ingress hop the traffic actually traversed (the LB Service in front of the istio ingressgateway,
and the gateway pods themselves) is invisible. The identity of that ingress LB Service is already
known: Hop 1 of the IP→Gateway 3-hop matches the destination IP against ingress Service rows, but
discards the Service's name after reading its selector. The nginx fallback
(`ingress-lb-service-fallback`) proved the identity is recoverable from the already-loaded window
via `memwindow.ResolveIPToIngressServices` — zero new store reads.

## What Changes

- `RouteDestination` gains two fields — `IngressNamespace` / `IngressService` — the ingress LB
  Service the destination IPs **uniquely** mapped to in the locked cluster's window. Populated on
  `RouteHit` (by reusing the fallback's window-wide identity-dedup, factored into a shared
  helper) and, for uniformity, on `RouteIngressLBService` (where they equal the destination
  itself). Empty when the window pins no unique identity — an ambiguous or absent identity NEVER
  demotes a hit.
- On a `RouteHit` whose destination carries an ingress identity, the reader emits the **full
  chain** instead of the direct edge:
  `caller pod -[pod-calls-service]-> ingress LB service -[service-selects-pod]-> ingress gateway
  pod(s) -[pod-calls-service]-> backend service -[service-selects-pod]-> backend pods`.
  The direct `caller → backend service` edge is REMOVED whenever the chain is emitted (no
  duplicate semantics). The ingress-pod→backend edges are synthesized (not trace-derived); a real
  traced edge for the same `(pod, service)` pair wins (identical UUIDv5 edge ID otherwise).
- The **ingress** Service's `service-selects-pod` fan-out is **locked-cluster-only** — the
  endpoints of `(dest.Cluster, ns, name)` alone, NOT the family-wide union — because an LB IP
  belongs to exactly one cluster's Service (a family sibling's same-named `istio-ingressgateway`
  pods are not behind this IP). The synthesized ingress-pod→backend edges are emitted from exactly
  that endpoint set. The **backend** Service keeps the existing family-wide `resolveServiceLevel`
  semantics unchanged.
- The existing nginx `RouteIngressLBService` path switches to the same locked-cluster-only
  fan-out ("the pods behind this IP"), superseding the family-wide fan-out wording of the
  still-active `ingress-lb-service-fallback` change.
- **Degrade, never fail**: every chain precondition failure (no unique identity in the window,
  ingress identity == backend identity, locked cluster's topology lacks the ingress Service, or
  the ingress Service has zero locked-cluster endpoints) falls back to today's RouteHit shape —
  direct edge + family-wide backend fan-out — with a debug-only degrade log, no external node,
  and no stray/orphan ingress node (all preconditions are checked purely before any
  materialisation). A topology miss on the **backend** keeps the existing
  `route_engine_dest_cluster_lacks_service` external path unchanged.
- No new node type, edge type, attribute, `labels` key, engine outcome, PromQL change, or store
  read. Feature-off (`RouteResolver == nil`) output stays byte-for-byte unchanged.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: adds a "Full ingress chain for routed global-FQDN hits" requirement
  extending "Istio route resolution of global FQDN peers" — the chain shape, the window-wide
  ingress-identity recovery, the locked-cluster fan-out rule (also applied to the
  `ingress-lb-service-fallback` requirement's resolution, superseding its family-wide fan-out),
  the direct-edge removal, the traced-edge-wins dedup, and the degrade matrix.

## Impact

- **Modified**: `pkg/build/routeresolve.go` (two new `RouteDestination` fields),
  `pkg/build/servicegraph.go` (cluster-scoped `resolveServiceLevelInCluster`, synthesized-edge
  accumulator + emission, `resolveRouteChain`), `pkg/build/routeprescan.go` (`routeIndexResolve`:
  hit/LB branch split), `pkg/route/ingresslb.go` (identity core factored into
  `ingressServiceIdentity`; wrapper populates the new fields), `pkg/route/resolver.go` (hit-path
  identity stamping), `CLAUDE.md`.
- **Dependencies**: none added. `pkg/build` still does not import `pkg/route`
  (`make check-route-containment` unchanged). `RouteResolver` interface unchanged — no mock
  regeneration.
- **Store / memwindow**: no changes, no new reads — identity comes from the rows already loaded
  by `LoadTrafficWindow`.
