# Tasks

Makes the ingress-gateway route path distinguishable from the direct edge: a new
`pod-routes-to-service` edge type for the synthesized `ingress gateway pod → backend service` hop,
and a two-valued `labels.role` (`ingress-gateway` / `ingress-lb`) on ingress `service` nodes. No
resolution behaviour changes. TDD throughout: tests first (RED), then implementation (GREEN).

## 1. Edge type registration

- [ ] 1.1 `pkg/graph/edge.go`: add `EdgeTypePodRoutesToService EdgeType = "pod-routes-to-service"`
      to the const block.
- [ ] 1.2 `pkg/graph/registry.go`: add the `EdgeTypeDefinition` — `SourceType: []NodeType{NodeTypePod}`
      (doc: always an ingress gateway pod), `TargetType: []NodeType{NodeTypeService}`,
      `Directed: true`, `MayCrossCluster: false`, `Labels: [{cluster, string}]`. The description
      MUST state the edge is **config-derived** (translated Gateway + VirtualService), not observed
      traffic, and that it is emitted only by the `RouteHit` ingress chain.
- [ ] 1.3 `pkg/graph/project.go`: add `EdgeTypePodRoutesToService` to the connectivity case of
      `connectivityExcluded`'s switch (the switch is documented as exhaustive over `EdgeType`).
- [ ] 1.4 `pkg/graph/service_test.go` (or a new `registry_test.go` case): assert the type is
      registered, `directed=true`, `may_cross_cluster=false`, source `[pod]`, target `[service]`,
      and that `ValidEdgeType` accepts it (so `?edge_type=pod-routes-to-service` parses).
- [ ] 1.5 `pkg/graph/project_prune_test.go`: a pod connected ONLY by a `pod-routes-to-service`
      edge survives the default connectivity prune.

## 2. Ingress node marker

- [ ] 2.1 `pkg/build/servicegraph.go`: add the role constants (`ingress-gateway` / `ingress-lb`)
      and `func (r *sgResolver) markIngressService(id, role string)` — set-only, with the monotone
      precedence: `ingress-gateway` always writes; `ingress-lb` writes only when no `role` key is
      present. Document why (design D3: one Service can be reached by both paths in one build).
- [ ] 2.2 `pkg/build/servicegraph.go` `resolveRouteChain`: after the successful
      `resolveServiceLevelInCluster` materialisation, call `markIngressService(ids[0], roleIngressGateway)`.
      Every precondition degrade returns before this point (no node, no marker).
- [ ] 2.3 `pkg/build/routeprescan.go` `routeIndexResolve`, `RouteIngressLBService` branch: after
      the successful `resolveServiceLevelInCluster`, call `markIngressService(ids[0], roleIngressLB)`.

## 3. Synthesized hop edge type

- [ ] 3.1 `pkg/build/servicegraph.go`: the `res.routeChainEdges` emission loop in
      `parseServiceGraphRoutes` emits `graph.EdgeTypePodRoutesToService` instead of
      `graph.EdgeTypePodCallsService`. The traced-edge-wins skip against `pairs` (same
      `(src, tgt)`) and the `{"cluster": ce.cluster}` label are unchanged.

## 4. `pkg/build` unit tests (`routeprescan_test.go`)

- [ ] 4.1 Update the existing chain assertions: the synthesized `igw0 → payments` edge moves out of
      `edgesByType(res, graph.EdgeTypePodCallsService)` into
      `edgesByType(res, graph.EdgeTypePodRoutesToService)`; `pod-calls-service` now holds exactly
      {caller→igw, caller→payments}. `cluster` labels unchanged.
- [ ] 4.2 New: chained hit ⇒ ingress node's `Labels()["role"] == "ingress-gateway"`; backend
      service node has NO `role` key.
- [ ] 4.3 New: `RouteIngressLBService` fallback ⇒ node's `role == "ingress-lb"` and zero
      `pod-routes-to-service` edges.
- [ ] 4.4 New (determinism): a fixture where one endpoint chains through Service X and another
      LB-falls-back to the same X ⇒ `role == "ingress-gateway"` for BOTH input orderings (assert
      by feeding the two samples in each order).
- [ ] 4.5 Existing degrade tests gain: no `role` key on any service node, zero
      `pod-routes-to-service` edges, node/edge set byte-identical to the pre-change direct shape.
- [ ] 4.6 Engine-off / no-resolver parse tests: assert no `role` key and no new-type edge anywhere
      (feature-off output unchanged).

## 5. API surface

- [ ] 5.1 `internal/api/handlers.go`: extend the `/v1/graph` and `/v1/edge-types` swagger
      `@Description` to mention `pod-routes-to-service` and the ingress `labels.role` marker.
- [ ] 5.2 `make docs` — regenerate `docs/swagger.{json,yaml}`; `make check-docs` clean.
- [ ] 5.3 `go test ./internal/api/ -run Golden -update` — regenerate
      `internal/api/testdata/golden/edge-types.json`; review the diff (only the new entry).

## 6. Integration (`internal/integration/route_e2e_test.go`)

- [ ] 6.1 Chain case: assert the ingress node's `labels.role == "ingress-gateway"` and that the
      gateway-pod → backend edge has `type == "pod-routes-to-service"`.
- [ ] 6.2 nginx LB-fallback case: assert `labels.role == "ingress-lb"` and no
      `pod-routes-to-service` edge in the body.
- [ ] 6.3 Chain-degrade case: assert no `role` key and no `pod-routes-to-service` edge.

## 7. Documentation

- [ ] 7.1 `CLAUDE.md`: update the route-resolution bullet's chain description (new edge type +
      role marker), the `/v1/edge-types` bullet's current-edge-type list, and the service-node
      `labels` note so `role` is documented alongside `{cluster, namespace}`.
- [ ] 7.2 `docs/istio-virtualservice-routing-history-design.md`: §4.1 / §4.2 diagrams and tables,
      §4.3 comparison table gains a "圖上標記" row, and a short consumer note on the toggle rule
      (hide `role="ingress-gateway"` nodes + `pod-routes-to-service` edges + those edges' source
      pods; `role="ingress-lb"` nodes and direct edges always shown).
- [ ] 7.3 Note in the change: the `graph-api` delta also corrects pre-existing drift — the
      promoted spec's `pod-calls-service` scenario still says `may_cross_cluster: false`, while
      `translate-global-fqdn-to-k8s-service` made it `true` in `registry.go` without a `graph-api`
      delta. The MODIFIED requirement reproduces the accurate value.

## 8. Verification

- [ ] 8.1 `make build && make vet && make lint`
- [ ] 8.2 `make test` (full, `-race -shuffle=on` — an order-dependent marker would fail here)
- [ ] 8.3 `make check-route-containment` (`pkg/build` still must not import `pkg/route`)
- [ ] 8.4 `make verify-mocks` (no interface changed — must be a no-op)
- [ ] 8.5 Docker + `router_check_tool`:
      `KSG_ROUTER_CHECK_BIN=$(which router_check_tool) go test ./internal/integration/ -run TestRouteSuite`
- [ ] 8.6 `openspec verify "mark-ingress-route-path"` before archiving.
