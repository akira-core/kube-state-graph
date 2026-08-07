# Tasks

Makes the ingress-gateway route path distinguishable from the direct edge via a two-valued
`labels.role` (`ingress-gateway` / `ingress-lb`) on ingress `service` nodes. The synthesized
`ingress gateway pod → backend service` hop stays typed `pod-calls-service` (no dedicated edge
type). No resolution behaviour changes.

## 1. Edge type (reverted — keep `pod-calls-service`)

- [x] 1.1 ~~Add `EdgeTypePodRoutesToService`~~ — **reverted**: no dedicated edge type; the
      synthesized hop remains `EdgeTypePodCallsService`.
- [x] 1.2 ~~Add registry catalogue entry~~ — **reverted**.
- [x] 1.3 ~~Extend `connectivityExcluded`~~ — **reverted** (hop already covered by
      `EdgeTypePodCallsService`).
- [x] 1.4 Registry / prune tests cover `pod-calls-service` only (no
      `pod-routes-to-service` assertions).

## 2. Ingress node marker

- [x] 2.1 `pkg/build/servicegraph.go`: add the role constants (`ingress-gateway` / `ingress-lb`)
      and `func (r *sgResolver) markIngressService(id, role string)` — set-only, with the monotone
      precedence: `ingress-gateway` always writes; `ingress-lb` writes only when no `role` key is
      present. Document why (design D3: one Service can be reached by both paths in one build).
- [x] 2.2 `pkg/build/servicegraph.go` `resolveRouteChain`: after the successful
      `resolveServiceLevelInCluster` materialisation, call `markIngressService(ids[0], roleIngressGateway)`.
      Every precondition degrade returns before this point (no node, no marker).
- [x] 2.3 `pkg/build/routeprescan.go` `routeIndexResolve`, `RouteIngressLBService` branch: after
      the successful `resolveServiceLevelInCluster`, call `markIngressService(ids[0], roleIngressLB)`.

## 3. Synthesized hop emission

- [x] 3.1 `pkg/build/servicegraph.go`: the `res.routeChainEdges` emission loop in
      `parseServiceGraphRoutes` emits `graph.EdgeTypePodCallsService`. The traced-edge-wins skip
      against `pairs` (same `(src, tgt)`) and the `{"cluster": ce.cluster}` label are unchanged.

## 4. `pkg/build` unit tests (`routeprescan_test.go`)

- [x] 4.1 Chain assertions: `pod-calls-service` holds {caller→igw, caller→payments,
      igw0→payments}. `cluster` labels unchanged.
- [x] 4.2 Chained hit ⇒ ingress node's `Labels()["role"] == "ingress-gateway"`; backend
      service node has NO `role` key.
- [x] 4.3 `RouteIngressLBService` fallback ⇒ node's `role == "ingress-lb"`.
- [x] 4.4 Determinism: a fixture where one endpoint chains through Service X and another
      LB-falls-back to the same X ⇒ `role == "ingress-gateway"` for BOTH input orderings.
- [x] 4.5 Degrade tests: no `role` key on any service node; node/edge set byte-identical to the
      pre-change direct shape.
- [x] 4.6 Engine-off / no-resolver parse tests: assert no `role` key (feature-off output
      unchanged).

## 5. API surface

- [x] 5.1 `internal/api/handlers.go`: `/v1/graph` and `/v1/edge-types` swagger `@Description`
      mention the ingress `labels.role` marker (no `pod-routes-to-service`).
- [x] 5.2 `make docs` — regenerate `docs/swagger.{json,yaml}`; `make check-docs` clean.
- [x] 5.3 Golden `edge-types.json` has no `pod-routes-to-service` entry.

## 6. Integration (`internal/integration/route_e2e_test.go`)

- [x] 6.1 Chain case: assert the ingress node's `labels.role == "ingress-gateway"` and that the
      gateway-pod → backend edge has `type == "pod-calls-service"`.
- [x] 6.2 nginx LB-fallback case: assert `labels.role == "ingress-lb"`.
- [x] 6.3 Chain-degrade case: assert no `role` key.

## 7. Documentation

- [x] 7.1 `CLAUDE.md`: chain description uses `pod-calls-service` for the synthesized hop +
      role marker; `/v1/edge-types` bullet lists no dedicated routes type.
- [x] 7.2 `docs/istio-virtualservice-routing-history-design.md`: §4.3 comparison table "圖上標記"
      row and consumer toggle rule use `labels.role` only.
- [x] 7.3 Note in the change: the `graph-api` delta also corrects pre-existing drift — the
      promoted spec's `pod-calls-service` scenario still says `may_cross_cluster: false`, while
      `translate-global-fqdn-to-k8s-service` made it `true` in `registry.go` without a `graph-api`
      delta. The MODIFIED requirement reproduces the accurate value.

## 8. Verification

- [x] 8.1 `make build && make vet && make lint`
- [x] 8.2 Targeted tests: `go test ./pkg/graph/ ./pkg/build/ ./internal/api/`
- [x] 8.3 `make check-route-containment` (`pkg/build` still must not import `pkg/route`)
- [ ] 8.4 Docker + `router_check_tool` (optional locally):
      `KSG_ROUTER_CHECK_BIN=$(which router_check_tool) go test ./internal/integration/ -run TestRouteSuite`
