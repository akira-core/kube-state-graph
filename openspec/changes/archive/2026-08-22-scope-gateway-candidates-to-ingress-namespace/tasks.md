# Tasks

Scopes Hop 3 gateway candidates to the ingress Service's own namespace (closing the
same-named cross-namespace Gateway hazard without signature changes) and makes the
identical-pattern host match deterministic. Alongside, adds the point-in-time test
coverage the simplify change left open (SQL inclusive bound, uniqueRows probe). TDD:
tests first (RED), then implementation (GREEN).

## 1. Unit tests (RED)

- [x] 1.1 `pkg/route/snapshot/snapshot_test.go`: add
      `TestResolveIPToGatewaysScopedToIngressNamespace` — ingress Service/Deploy in
      `istio-system`, two selector-matching live gateways `istio-system/gw-a` and
      `team-b/gw-a`; candidates contain ONLY `istio-system/gw-a`.
- [x] 1.2 `pkg/route/gwresolve/gwresolve_test.go`: add
      `TestResolveIdenticalPatternLexicalTieBreak` — `gw-b`/`gw-a` both declaring
      `*.example.com` (and an exact-host variant), both declaration orders →
      `Resolve` always returns `gw-a`.
- [x] 1.3 `pkg/route/resolver_test.go`: add
      `TestResolveRoute_CrossNamespaceGatewayNotACandidate` — mock store, zero
      matchcheck.Runner tripwire; hop 1+2 in `istio-system`, the only
      selector-matching gateway (serving the host) in `team-b`, plus the LB-shaped
      ingress ServiceRow → outcome `RouteIngressLBService` (pipeline missed
      `no_gateway`, fallback fired), never touching translate.

## 2. Implementation (GREEN)

- [x] 2.1 `pkg/route/snapshot/snapshot.go` `ResolveIPToGateways` Hop 3: add
      `r.Namespace == svcNS`; doc comment states the same-namespace rule and the
      deferred cross-namespace attachment.
- [x] 2.2 `pkg/route/store/clickhouse.go` `LoadTrafficAt` gw_versions query: add
      `AND has(?, namespace)` bound to `nsList` (same predicate shape as the deploy
      hop); update the doc comment.
- [x] 2.3 `pkg/route/gwresolve/gwresolve.go` `sortPats`: insert `pat.gw` ascending
      between the pattern and idx comparators; comment the lexical tie-break and why
      PickHosts is unaffected.

## 3. Integration (`internal/integration/route_e2e_test.go`)

- [x] 3.1 Seed a cross-namespace gateway row
      `("cluster-alpha","team-b","public-gw", selector istio=ingressgateway, hosts
      ["team-b.example.com"], seq 43)`; `TestTrafficSnapshotNoFinalDedupAndBareRef`'s
      `s.Len(w.Gateways, 2)` still holds — the SQL namespace filter excludes it
      (assert message updated to state this).
- [x] 3.2 Point-in-time gap T7: const `ingressLBIPBoundary`; seed an isolated 3-hop
      chain (`boundary` namespace, `app=boundary-igw` selector; service seq40, deploy
      seq41, gateway seq42 minimal HTTP-80 spec) all opening at `dt64s(fixedNow)`;
      add `TestAsOfBoundaryValidFromEqualsInstant` (probe + LoadTrafficAt at
      `fixedNow` inclusive; at `fixedNow-1ms` empty).
- [x] 3.3 Point-in-time gap T8: add
      `TestUniqueRowsProbeAgainstRewriteWriterResurrectsDeadCluster` — WithUniqueRows
      probe on `ingressLBIPGone` returns `["cluster-gone"]` and `CollapsedRows() == 0`
      (documents the pruned-mode hazard on the probe path).

## 4. Docs

- [x] 4.1 `CLAUDE.md` route bullet (5): Hop 3 is namespace-scoped to the ingress
      Service's namespace (cross-namespace attachment deferred, degrades no_gateway);
      identical equal-specificity patterns resolve to the lexically-smallest gateway
      name.

## 5. Verification

- [x] 5.1 `go build ./... && go vet ./...`, `go vet -tags oracle ./pkg/route/`,
      `go test ./pkg/route/... ./pkg/build/... -count=1`, `make test` (golden files
      unchanged), `make lint`, `make verify-mocks`, `make check-route-containment`.
- [x] 5.2 Docker integration: `go test ./internal/integration/ -run
      'TestRouteStoreSuite|TestRouteSuite' -count=1` (full chain needs
      `KSG_ROUTER_CHECK_BIN`). `openspec validate
      scope-gateway-candidates-to-ingress-namespace`. Both suites PASS
      (RouteStoreSuite 4.76s, RouteSuite 9.54s); validate clean. On macOS the
      extracted `router_check_tool` is a Linux ELF, so `KSG_ROUTER_CHECK_BIN`
      pointed at a shim that runs the SAME digest-pinned Envoy tools image via
      `docker run` with `/var/folders` mounted (the tool takes absolute -c / -t
      paths in the caller's temp dir).
