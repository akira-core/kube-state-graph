# Harden Istio route resolution

## Why

A multi-agent code review of the route-resolution branch surfaced two defects that make the
engine **silently useless in ordinary production shapes**, plus a set of narrower
correctness, CI, performance, and contract gaps. Both headline defects were reproduced
against the real code, not inferred from the diff:

1. **Multi-IP snapshot union has no dedup.** `Resolver.loadSnapshot` issues one
   `store.LoadTrafficAt` per destination IP and appends the rows. `dedupLiveAtCounted`
   dedups only *within* one call, so two IPs served by the same ingress Service return
   overlapping rows and `snapshot.ScopedFor` appends the same VirtualService twice.
   istiod's in-memory config store rejects the duplicate (`item already exists`),
   `Translate` errors, and the endpoint degrades to `external`. A dual-stack ingress
   Service — one row carrying both an IPv4 and an IPv6 address — is enough.

2. **A bare short `destination.host` never resolves.** `ScopedFor` builds `config.Meta`
   without `Domain`, and istiod's `ResolveShortnameToFQDN` only appends `.svc.<domain>`
   when the meta carries one. `destination: {host: checkout}` in a VirtualService in
   namespace `shop` therefore translates to the Envoy cluster
   `outbound|8080||checkout.shop` instead of `outbound|8080||checkout.shop.svc.cluster.local`,
   and `ParseBackendHost` — which requires the FQDN suffix — rejects it. Result:
   `RouteNoRoute` → external, for the most common way Istio users write a destination.

Neither would turn CI red today: `TestRouteSuite`, the only route-engine e2e, calls
`t.Skipf` when `router_check_tool` is absent, and no workflow installs it — a skip prints
as `ok`. Two more gates named in the repo's own docs are equally inert: CI runs `go test`
directly so the Makefile's `-timeout 30m` never applies, and no workflow calls
`make check-route-containment` even though `CLAUDE.md` states it is enforced there.

## What Changes

**Route-resolution correctness**

- **Snapshot integrity across a multi-IP load**: `loadSnapshot` dedups the union by
  resource identity, and `ScopedFor` / `backendServices` refuse to emit a duplicate
  config or registry entry. A multi-IP request now translates exactly the configuration a
  single-IP request would.
- **Backend host identity matches istiod's own resolution**: the Gateway and
  VirtualService `config.Meta` carry `Domain: "cluster.local"`, and destination hosts are
  normalised to their FQDN at collection time using the VirtualService's namespace, so a
  bare short name resolves and its backend Service is present in the translation
  registry. `ParseBackendHost` is tightened to accept only a well-formed
  `<name>.<namespace>.svc.cluster.local`. Dotted relative forms (`checkout.shop`,
  `checkout.shop.svc`) are deliberately **not** accepted — istiod does not expand them
  either, so the external fallback faithfully reflects a configuration that would fail in
  a real mesh.
- **Namespace-qualified gateway identity**: `snapshot.ScopedFor` selects the Gateway by
  `(namespace, name)`. Scoping Hop 3 to the ingress namespace made the candidate set
  unambiguous, but `ScopedFor` still scanned every loaded row by bare name, and the loaded
  set spans namespaces (the `gw_versions` SQL binds the union of every ingress Service
  namespace carrying the IP, and a multi-IP request unions candidates across IPs). The
  selected row's namespace also feeds the VirtualService binding test, so a wrong pick
  produced a wrong destination rather than a miss.
- **Deterministic 3-hop selection**: Hop 1 no longer takes whichever ingress Service row
  ClickHouse returned first — more than one live identity for an IP degrades, matching
  the ambiguity rule the ingress LB path already applies. Hop 2 takes the **union** of
  every matching ingress Deployment's pod labels, mirroring the SQL layer's own
  `labelUnion`, so a canary gateway Deployment cannot change the candidate set by row
  order.
- **Infrastructure errors stop carrying a load-bearing outcome**: the three error returns
  in `Resolver.resolve` return an empty outcome instead of `RouteNoGateway`, which is the
  ingress-LB-fallback gate.
- **`router_check_tool` output parsing fails loudly**: `parseActuals` errors when it did
  not recover one result per query, instead of silently returning a misaligned answer.

**CI**

- The `test` job installs `router_check_tool` and exports `KSG_ROUTER_CHECK_BIN`;
  `TestRouteSuite` **fails** rather than skips when `CI` is set.
- The `test` job invokes `make test`, so the 30-minute timeout the Docker-backed suites
  need actually applies.
- A `route-containment` job runs `make check-route-containment`, making the documented
  guarantee real.
- The route store's `CollapsedRows` writer-uniqueness alarm logs a warning the first time
  it fires under `--route-store-unique-rows`, instead of being observable only from tests.

**Performance**

- Route resolution within one build runs with bounded concurrency instead of strictly
  serially, the per-build ingress-IP probe memo becomes concurrency-safe, and the key set
  is capped with the truncation logged (never silently).
- The service-graph resolver's topology indexes are built once per build and shared
  between the route prescan and the parse, instead of being rebuilt for each.

**Contract and documentation**

- `graph.EdgeTypes`' `service-selects-pod` and `pod-calls-pod` prose is corrected to
  match the producers that exist today (peer-label enrichment, the route engine's
  ingress-cluster anchor, and the locked-cluster fan-outs).
- The `/v1/graph` OpenAPI `@Description` is corrected: `storageclass` is a real node type,
  the compound hierarchy is `cluster > namespace > application > controller > pod`, and
  the `edge_type` enum lists all six edge types (`pod-to-node` and `pvc-to-storageclass`
  were missing, so a client generated from the spec could not express them).
- Comments left describing a time *range* by the point-in-time simplification, and
  invariant claims contradicted by the ingress-chain work, are corrected.

**Supply chain**

- The `envoyproxy/envoy:tools-*` image is pinned by digest, and the Dockerfile becomes the
  single source of that value for both the image build and the CI install step.

**Deliberately not changed** (reviewed and rejected): treating a backend Service row that
carries an ingress IP as a non-candidate for ingress identity. The schema offers no
discriminator beyond `external_ips` / `loadbalancer_ips`, and a Service that carries an
externally reachable IP **is** an entry point — equivalent to a LoadBalancer and displayed
through the same path. Several Services sharing one IP is genuinely indistinguishable, so
degrading to ambiguous is the correct outcome, not a defect.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: "Istio route resolution of global FQDN peers" — snapshot integrity
  under a multi-IP load, backend host resolution matching istiod, namespace-qualified
  gateway identity, deterministic 3-hop selection, and bounded per-build resolution work.
- `static-analysis-suite`: the route-engine e2e and the dependency-containment guard are
  enforced by CI, the test job runs through the Makefile target, and the Envoy tools image
  is digest-pinned.

## Impact

- **Modified**: `pkg/route/{resolver,scoped,ingresspick}.go`,
  `pkg/route/snapshot/snapshot.go`, `pkg/route/store/{store,clickhouse}.go`,
  `pkg/route/matchcheck/matchcheck.go`, `pkg/route/gwresolve/gwresolve.go`,
  `pkg/build/{routeprescan,routeresolve,servicegraph}.go`, `pkg/graph/registry.go`,
  `internal/api/handlers.go` + regenerated `docs/`, `.github/workflows/ci.yml`,
  `Makefile`, `deploy/docker/server.Dockerfile`, and the corresponding tests including
  `internal/integration/route_e2e_test.go`.
- **Behaviour**: single-IP requests whose VirtualServices use FQDN destinations — every
  existing fixture — are byte-for-byte unchanged. Multi-IP requests that previously
  degraded to `external` now resolve. Bare short destination hosts that previously missed
  now resolve. An ingress IP owned by more than one live Service identity now degrades at
  Hop 1 instead of silently picking a row. The engine remains disabled by default, and a
  resolution failure still can never fail a build.
- **Dependencies**: none added or removed. Store schema unchanged; no new query, node
  type, edge type, attribute, or `labels` key. `make check-route-containment` continues
  to hold and is now actually run by CI.
