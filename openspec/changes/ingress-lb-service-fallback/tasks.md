# Tasks

Adds a post-Istio-miss fallback to the route engine: a destination IP that uniquely maps to an
ingress LB Service inside the already-selected ingress cluster resolves to that Service
(`RouteIngressLBService`); same-IP identity collisions degrade external
(`RouteAmbiguousIngressService`). TDD throughout: tests first (RED), then implementation (GREEN).

## 1. Contract + pure functions

- [x] 1.1 `pkg/build/routeresolve.go`: add `RouteIngressLBService = "ingress_lb_service"` and
      `RouteAmbiguousIngressService = "ambiguous_ingress_service"` outcome constants.
- [x] 1.2 `pkg/route/memwindow`: add `ResolveIPToIngressServices(ip string) []store.ServiceRow` —
      all rows overlapping the window with `HasIngressIP(ip)` (defensive overlap check against
      the window bounds). Tests: match / IP mismatch / multiple rows all returned /
      non-overlapping row excluded.
- [x] 1.3 `pkg/route/ingresslb.go`: `resolveIngressLBService(mw, ips) (dest, outcome, ok)` —
      per-IP distinct-identity sets, order-free precedence (any >1 → ambiguous; any 0 → no
      fallback; singletons must agree).
- [x] 1.4 `pkg/route/ingresslb_test.go`: single IP single Service → hit; single IP two Services →
      ambiguous; two IPs different identities → ambiguous; two IPs same identity → hit; an IP
      with no match → ok=false; one IP two candidates + one IP zero → ambiguous; same-identity
      multi-version rows → hit; identity change within window → ambiguous.

## 2. Resolver integration

- [x] 2.1 `pkg/route/resolver.go`: in the `!hit` branch, gated on
      `miss == RouteNoGateway` (deep Istio misses keep their diagnostic reason), call the
      fallback; on `RouteIngressLBService` stamp `dest.Cluster` with the locked cluster (D11)
      and return; on `RouteAmbiguousIngressService` return it; otherwise return the pipeline
      miss unchanged.
- [x] 2.2 `pkg/route/resolver_test.go`: nginx fixture (ingress LB ServiceRow + DeployRow, no
      GatewayRow) → `RouteIngressLBService` with correct dest; existing Istio-hit and deep-miss
      tests stay green (hit priority + no-gateway gate — `HostNotServedByAnyServerOnPort` keeps
      `RouteNoServerForHost` even with an LB ServiceRow in the window); dual LB Services on one
      IP → `RouteAmbiguousIngressService`; no LB row → original miss (`RouteNoGateway`);
      BuildScoped path unaffected.

## 3. Graph-side wiring (`pkg/build`)

- [x] 3.1 `routeprescan.go` `routeIndexResolve`: `RouteIngressLBService` joins the `RouteHit`
      `resolveServiceLevel` case (same `route_engine_dest_cluster_lacks_service` on topology
      miss); success debug log carries the outcome so fallback hits are distinguishable.
- [x] 3.2 `routeprescan.go`: new `RouteAmbiguousIngressService` case →
      `noteExternal("route_engine_ambiguous_ingress_service", ...)`.
- [x] 3.3 `routeprescan_test.go`: fallback-hit test (service node + `pod-calls-service` +
      `service-selects-pod`, no external); miss-table rows for `ambiguous_ingress_service` and
      `ingress_lb_service`-but-topology-lacks-service → external.

## 4. Integration + full checks

- [x] 4.1 `internal/integration/route_e2e_test.go`: nginx fixture — ClickHouse seed
      `service_versions` LB Service (new IP) + `deploy_versions` nginx Deployment, NO Gateway
      CR reachable from that IP; full-graph e2e in **RouteSuite** (VM seed `kube_service_info`
      + endpointslice → nginx pod + unknown-server series with `client_dns_answers` = new IP;
      assert service node, `pod-calls-service`, `service-selects-pod`, no external) plus a
      tool-free resolver-level test in **RouteStoreSuite** (real ClickHouse + zero
      `matchcheck.Runner` tripwire → `RouteIngressLBService` / ambiguous outcomes, Docker-only).
- [x] 4.2 ambiguous integration case: two LB Service rows on one IP → external
      (`TestAmbiguousIngressLBServiceStaysExternal`).
- [x] 4.3 `make test`, `make check-route-containment`, `make lint` all green.

## 5. Docs + observability

- [x] 5.1 CLAUDE.md: extend the Istio route-resolution bullet with the fallback (outcomes,
      ambiguous rule, window-wide dedup evaluation).
- [x] 5.2 `docs/nginx-ingress-backend-resolution.md` §7: note that the LB-layer fallback is
      implemented by this change.
- [x] 5.3 No new self-metric labels; existing `route_engine_*` reason counting covers the new
      reasons.
