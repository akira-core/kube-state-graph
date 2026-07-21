# Tasks

Emits the full ingress chain for routed global-FQDN hits — `caller pod → ingress LB service →
ingress gateway pod(s) → backend service → backend pods` — recovering the ingress Service
identity from the already-loaded window, with a pure-precondition degrade matrix back to today's
direct-edge shape, and locked-cluster `service-selects-pod` fan-out for ingress Services (chain +
nginx LB fallback). TDD throughout: tests first (RED), then implementation (GREEN).

## 1. Contract

- [x] 1.1 `pkg/build/routeresolve.go`: add `IngressNamespace` / `IngressService` fields to
      `RouteDestination` (doc: unique window identity of the destination IPs' ingress LB
      Service; empty ⇒ chain degrades; never demotes a hit). Interface unchanged — verify with
      `make verify-mocks`.

## 2. `pkg/route` — identity recovery

- [x] 2.1 `pkg/route/ingresslb.go`: factor the identity-dedup core of `resolveIngressLBService`
      into `ingressServiceIdentity(mw, ips) (ingressIdentity, ingressIdentityStatus)`
      (tri-state: unique / ambiguous / incomplete; precedence preserved: same-IP collision >
      any-empty > disagreement). Rewrite `resolveIngressLBService` as a thin wrapper that also
      populates `IngressNamespace`/`IngressService` (= destination) on the unique state.
- [x] 2.2 `pkg/route/ingresslb_test.go`: `TestIngressServiceIdentity_*` table — unique /
      same-IP collision / disagreeing singletons / any-empty precedence over disagreement /
      multi-version same identity / identity change within window / no rows at all. Existing
      `resolveIngressLBService` tests stay green; full-struct literals gain the `Ingress*`
      fields.
- [x] 2.3 `pkg/route/resolver.go`: in the hit branch, after `hitDest.Cluster = cluster`, stamp
      `Ingress*` iff `ingressServiceIdentity` is unique. `RouteNoGateway` fallback gate
      untouched.
- [x] 2.4 `pkg/route/resolver_test.go`: `TestResolveRoute_NginxIngressFallsBackToLBService`
      expected destination gains the `Ingress*` fields; all other full-struct assertions stay
      zero-value.

## 3. `pkg/build` — scoped resolver + chain

- [x] 3.1 `servicegraph.go`: `resolveServiceLevelInCluster(cluster, ns, svc) []string` — same
      anchor-membership + `materializeServiceNode`, fan-out over
      `endpointsByService[{cluster, ns, svc}]` only.
- [x] 3.2 `servicegraph.go`: `routeChainEdges` accumulator on `sgResolver` (keyed
      `srcPodID|backendSvcID`) + `addRouteChainEdge`; emission in `parseServiceGraphRoutes`
      after the traced-pairs loop, skipping pairs already in `pairs` (traced wins), labels
      `{"cluster": <ingress cluster>}`.
- [x] 3.3 `servicegraph.go`: `resolveRouteChain(dest, backendSvcID, t) ([]string, bool)` — pure
      precondition gates (identity present; identity != backend; `anchorHolds`; non-empty
      locked-cluster endpoints) each logging `route_chain_degraded` at Debug on failure, then
      scoped materialisation + sorted-unique synthesized edges; returns the ingress service id.
- [x] 3.4 `routeprescan.go` `routeIndexResolve`: split the shared branch —
      `RouteIngressLBService` → `resolveServiceLevelInCluster`;
      `RouteHit` → backend via family-wide `resolveServiceLevel` first (nil keeps the existing
      `route_engine_dest_cluster_lacks_service` external path), then `resolveRouteChain`
      swaps the returned ids to the ingress node on success. Success debug log gains
      `ingress_namespace` / `ingress_service` / `chain` attrs.

## 4. `pkg/build` unit tests (`routeprescan_test.go`)

- [x] 4.1 Fixture `sampleTopologyWithIngress()` — `sampleTopologyWithServices()` +
      `cluster-alpha/istio-system/igw` Service + endpoint pod `cluster-alpha/igw0`.
- [x] 4.2 `_RouteHitWithIngressIdentityEmitsChain`: two service nodes; `pod-calls-service`
      exactly {caller→igw, igw0→payments} (both `labels.cluster=cluster-alpha`); NO direct
      caller→payments edge; `service-selects-pod` = igw→igw0 + payments→pay0,pay1; no external.
- [x] 4.3 Degrade tests: `_ChainDegradesWhenIngressMissingFromTopology`,
      `_ChainDegradesWhenIngressHasNoEndpoints` (also: no igw node),
      `_ChainDegradesWhenIngressEqualsBackend` — all assert today's direct shape.
- [x] 4.4 `_ChainSynthEdgeDedupsAgainstTracedEdge`: traced igw0→payments series coexists ⇒
      exactly one edge for the pair.
- [x] 4.5 Locked-vs-family pins over `familyTopology()`: (a) `RouteIngressLBService` fan-out =
      selected cluster's endpoints only; (b) chain with same-named ingress in a sibling —
      ingress fan-out + synthesized edges from the selected cluster only, backend fan-out still
      family-wide.
- [x] 4.6 `_IndexMissFallsExternal`: add `hit_with_ingress_identity_but_backend_missing` case →
      external, no ingress node. Existing hit tests stay green (empty `Ingress*` ⇒ degrade
      branch); re-comment as the empty-identity degrade pins.

## 5. Integration (`internal/integration/route_e2e_test.go`)

- [x] 5.1 `SetupTest`: add alpha ingress topology to the VM fixtures (`kube_pod_info` igw pod,
      `kube_service_info` istio-system/igw, endpointslice series) matching the ClickHouse
      `istio-system/igw` ingress seed.
- [x] 5.2 `TestGlobalFQDNRoutesToService` (+ `TestExplicitPortRoutesViaHTTPListener`): assert
      the chain — ingress node id, caller→igw edge, igw fan-out to the igw pod, synthesized
      igw-pod→payments edge, no direct caller→payments edge, no external.
- [x] 5.3 `TestCrossClusterIngressResolves`: keep as the e2e topology-missing degrade proof
      (beta ingress absent from VM); assert no beta ingress node.
- [x] 5.4 `TestNginxIngressFallsBackToLBService`: expectations unchanged (single-cluster);
      comment updated to locked-cluster fan-out.
- [x] 5.5 `make test`, `make check-route-containment`, `make lint`, `make verify-mocks` green;
      goldens untouched (no golden covers route hits).

## 6. Docs

- [x] 6.1 CLAUDE.md: add the (5c) chain sub-point to the route-resolution bullet (identity
      recovery, degrade matrix, direct-edge removal, traced-edge-wins, locked-cluster ingress
      fan-out) and amend (5b) to locked-cluster fan-out.
