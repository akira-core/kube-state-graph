# Ingress LB Service fallback

## Why

The Istio route-resolution engine (`translate-global-fqdn-to-k8s-service`) resolves a global FQDN
peer through the Gateway + VirtualService + `router_check_tool` pipeline. In an **nginx-ingress**
(or any non-Istio LB) scenario that pipeline necessarily misses: Hop 1 (destination IP → ingress LB
Service) and Hop 2 (→ Deployment) succeed, but Hop 3 finds no Istio Gateway CR — `candidatesAt`
returns nothing, the segment loop never hits, and the endpoint degrades to a dead-end
`external/<fqdn>` node even though the store already knows exactly which ingress LB Service owns
the destination IP (`docs/nginx-ingress-backend-resolution.md` §1, §7).

## What Changes

- After the Istio pipeline runs over the loaded window **without a hit**, the resolver consults a
  new fallback: does the destination IP set map to exactly **one** ingress LB Service inside the
  **already-selected ingress cluster's** window? A unique match returns that Service as the
  destination with a new `RouteIngressLBService` outcome; the graph side resolves it through the
  SAME `resolveServiceLevel` path as `RouteHit` — one `service` node in the selected cluster, a
  `pod-calls-service` edge, and the family-wide `service-selects-pod` fan-out (expanding to the
  ingress controller pods, e.g. nginx).
- Evaluation is a **window-wide identity dedup** — the in-memory analogue of the
  `ClustersWithIngressIP` SQL, scoped to the loaded cluster: every `ServiceRow` whose validity
  overlaps `[start, end]` and whose ingress IPs contain the destination IP is a candidate; per IP
  the distinct `(namespace, name)` set must be a singleton, and all IPs must agree on the same
  identity. No per-instant / per-segment evaluation.
- More than one identity for any IP (including a Service renamed within the window), or
  disagreeing single candidates across IPs, degrades with a new `RouteAmbiguousIngressService`
  outcome → the existing external node (no guessing, no lexicographic tie-break — same spirit as
  the ambiguous-ingress-cluster rule). Zero candidates keeps the Istio pipeline's deepest miss
  unchanged.
- Ingress-cluster selection (`ClustersWithIngressIP` + `pickIngressCluster`), the `RouteHit`
  priority (the fallback runs ONLY when the pipeline produced no hit), and every pre-window miss
  (`no_ingress`, `ambiguous_ingress_cluster`) are untouched.
- Semantics are deliberately coarse: the fallback answers "which ingress LB Service owns this IP",
  ignoring host/path/port. Ingress CR / nginx.conf backend resolution is a NON-goal (a future
  change).
- No new node type, edge type, attribute, or `labels` key. Feature-off (`RouteResolver == nil`)
  output stays byte-for-byte unchanged.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: adds an "Ingress LB Service fallback" requirement extending "Istio route
  resolution of global FQDN peers" — a post-miss degradation step with two new engine outcomes
  (`ingress_lb_service`, `ambiguous_ingress_service`), the window-wide uniqueness rule, and the
  hit-priority / no-candidate-keeps-miss rules.

## Impact

- **Modified**: `pkg/build/routeresolve.go` (two new `RouteOutcome` constants),
  `pkg/build/routeprescan.go` (`routeIndexResolve`: `RouteIngressLBService` joins the `RouteHit`
  service-level path; new `route_engine_ambiguous_ingress_service` reason),
  `pkg/route/resolver.go` (fallback call in the `!hit` branch), `pkg/route/memwindow`
  (new `ResolveIPToIngressServices` accessor), `CLAUDE.md`,
  `docs/nginx-ingress-backend-resolution.md` §7.
- **New**: `pkg/route/ingresslb.go` (+ tests).
- **Dependencies**: none added. `pkg/build` still does not import `pkg/route`
  (`make check-route-containment` unchanged).
- **Store**: no new reads — the fallback reuses the `ServiceRow`s already loaded by
  `LoadTrafficWindow`.
