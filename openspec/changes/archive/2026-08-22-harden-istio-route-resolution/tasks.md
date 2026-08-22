# Tasks

Corrective follow-up to the route-resolution engine. CI first — the two headline defects
are e2e-level and nothing today would turn the pipeline red for them — then correctness,
then performance, then contract/docs. TDD within each group: tests first (RED), then
implementation (GREEN).

## 1. Supply chain + CI (make the gates real)

- [x] 1.1 `deploy/docker/server.Dockerfile`: declare `ARG ENVOY_TOOLS_IMAGE=<digest-pinned>`
      before the first `FROM` and use `FROM ${ENVOY_TOOLS_IMAGE} AS envoy-tools`. Resolve
      the digest with `docker buildx imagetools inspect envoyproxy/envoy:tools-v1.34-latest`
      and note the concrete version tag in a comment.
- [x] 1.2 `Makefile`: derive `ENVOY_TOOLS_IMAGE` from the Dockerfile declaration (single
      source, no drift check needed) and add a `router-check-tool` target that extracts
      `/usr/local/bin/router_check_tool` into `$(BIN_DIR)` via `docker create` + `docker cp`
      (no container run).
- [x] 1.3 `internal/integration/route_e2e_test.go` `RouteSuite.SetupSuite`: `Fatalf` when
      `os.Getenv("CI") != ""`, `Skipf` otherwise. Correct the file comment that currently
      claims CI runs the suite.
- [x] 1.4 `.github/workflows/ci.yml` `test` job: run `make router-check-tool`, export
      `KSG_ROUTER_CHECK_BIN` via `$GITHUB_ENV`, and change the test step to `make test`.
- [x] 1.5 `.github/workflows/ci.yml`: add an independent `route-containment` job running
      `make check-route-containment`.
- [x] 1.6 Push and verify on a real runner (see the verification checklist below) — this
      group is unverifiable locally.

## 2. Multi-IP snapshot integrity (RED → GREEN)

- [x] 2.1 `pkg/route/snapshot/snapshot_test.go`: duplicated VirtualService rows in the
      snapshot → `ScopedFor` returns one config per `(kind, ns, name)`; duplicated backend
      Service rows → one registry entry per hostname.
- [x] 2.2 `pkg/route/resolver_test.go`: two destination IPs whose `LoadTrafficAt` calls
      return the SAME non-empty gateway/VS rows → `RouteHit` (today: translate error →
      external). The existing `TestResolveRoute_LockedClusterScopesSnapshotLoads` uses an
      empty snapshot and does not cover this.
- [x] 2.3 `pkg/route/resolver.go` `loadSnapshot`: dedup the union by
      `(Cluster, Namespace, Name, ValidFrom)` per row kind.
- [x] 2.4 `pkg/route/snapshot/snapshot.go`: `ScopedFor` dedups `Configs` by
      `(GroupVersionKind, Namespace, Name)`; `backendServices` dedups by hostname.

## 3. Backend destination host resolution (RED → GREEN)

- [x] 3.1 `pkg/route/translate/translate_test.go`: a bare short `destination.host` →
      the emitted Envoy cluster names the full in-cluster FQDN (today: `<name>.<ns>`).
- [x] 3.2 `pkg/route/store/store_test.go`: `ParseBackendHost` accepts exactly
      `<name>.<ns>.svc.cluster.local`; rejects 3+ leading labels and every relative form.
- [x] 3.3 `pkg/route/store/store.go`: add the cluster-domain constant, derive
      `serviceDomain` from it, tighten `ParseBackendHost` to exactly two labels.
- [x] 3.4 `pkg/route/snapshot/snapshot.go` `ScopedFor`: stamp `Domain` on the Gateway and
      VirtualService `config.Meta`.
- [x] 3.5 Normalise destination hosts at collection time using the owning VirtualService's
      namespace, in BOTH copies of `vsDestHosts`
      (`pkg/route/snapshot/snapshot.go`, `pkg/route/store/clickhouse.go`) — the comments
      already require the two to stay identical.

## 4. Gateway identity + 3-hop determinism (RED → GREEN)

- [x] 4.1 `pkg/route/snapshot/snapshot_test.go`: same-named live Gateways in two
      namespaces → `ScopedFor` selects the requested namespace's row and only its bound
      VirtualServices.
- [x] 4.2 `pkg/route/snapshot/snapshot_test.go`: two live ingress Service identities on one
      IP → `ResolveIPToGateways` returns no candidate; two selector-matching Deployments →
      candidates derived from the pod-label union, both row orders.
- [x] 4.3 `pkg/route/snapshot/snapshot.go`: `ScopedFor(ns, name)`; Hop 1 collects and
      degrades on multiple identities; Hop 2 unions pod labels.
- [x] 4.4 `pkg/route/resolver.go`: carry the candidate's namespace through
      `candsToGateways` / `candNames` / `ResolveAmong` to `ScopedFor` — prefer recovering
      the winning `store.GatewayCand` over changing `gwresolve`'s matcher semantics.
      `PickHosts` numeric-index and `sortPats` lexical tie-break behaviour must not change.

## 5. Small hardening (RED → GREEN)

- [x] 5.1 `pkg/route/resolver.go`: the three error returns in `resolve` return the empty
      outcome; document that the outcome is meaningless when the error is non-nil.
- [x] 5.2 `pkg/route/matchcheck/matchcheck.go`: `parseActuals` errors unless it recovered
      one result per query; test with a stray numeric line in a multi-query parse.
- [x] 5.3 `pkg/route/store/clickhouse.go`: warn once (via `sync.Once`) the first time a
      collapse occurs under `uniqueRows`, naming the writer-uniqueness violation and the
      flag.

## 6. Performance (RED → GREEN)

- [x] 6.1 `pkg/build/routeprescan_test.go`: the index from concurrent resolution equals the
      serial index; exceeding the key cap truncates and logs the dropped count.
- [x] 6.2 `pkg/build/routeprescan.go` `resolveRouteQueries`: bounded `errgroup`, mutex
      around the index write, key cap with logged truncation.
- [x] 6.3 `pkg/route/scoped.go`: mutex-guard the probe memo; update the doc comment that
      currently states the scope is serial and needs no locking.
- [x] 6.4 `pkg/build/servicegraph.go` `ReadServiceGraph`: build the `sgResolver` once and
      share it with `collectRouteQueries` and `parseServiceGraphRoutes`; adjust the tests
      that call those functions directly.

## 7. Contract + documentation

- [x] 7.1 `pkg/graph/registry.go`: correct the `service-selects-pod` and `pod-calls-pod`
      descriptions to cover peer-label enrichment, the route engine's ingress-cluster
      anchor, and the locked-cluster fan-outs. Refresh
      `internal/api/testdata/golden/edge-types.json`.
- [x] 7.2 `internal/api/handlers.go`: correct the `/v1/graph` `@Description` (node types,
      compound hierarchy, all six edge types, the stale endpoint-resolution paragraphs) and
      add `pod-to-node` + `pvc-to-storageclass` to the `edge_type` `Enums(...)`. Run
      `make docs` and commit `docs/`.
- [x] 7.3 Comment drift sweep over `pkg/route` and `pkg/build/route*.go` for
      `window|range|segment` residue and invalidated invariant claims — at minimum
      `pkg/route/ingresspick.go`, `pkg/build/routeprescan.go`, `pkg/build/servicegraph.go`
      ("every other path also yields exactly one ID"), `pkg/build/routeresolve.go`
      (`resolveServiceLevel` claim), and the `window ...` error strings in
      `pkg/route/store/clickhouse.go`.
- [x] 7.4 Re-read `CLAUDE.md`'s route-resolution section against the behaviour changed
      here (Hop 2 union, Hop 1 degrade, bounded concurrency, bare-short-name resolution)
      and update only what is now wrong.

## 8. Integration coverage

- [x] 8.1 `internal/integration/route_e2e_test.go`: dual-stack `client_dns_answers` (two
      IPs on one ingress Service) resolves to the routed backend Service.
- [x] 8.2 `internal/integration/route_e2e_test.go`: a VirtualService using a bare short
      `destination.host` resolves to the backend Service.

## 9. Verification

- [x] 9.1 `go test ./pkg/route/... ./pkg/build/ -count=1 -race`
- [x] 9.2 `KSG_ROUTER_CHECK_BIN=... CI=1 go test ./internal/integration/ -run 'TestRouteSuite|TestRouteStoreSuite' -v`
- [x] 9.3 `make ci` green (lint + vuln + test + docs + mocks + containment)
- [x] 9.4 Real GitHub Actions run: the matcher install step succeeds; the route e2e
      executes (not `SKIP`); `-timeout 30m` appears in the test command; the containment
      job is present and green. Prove the guard by temporarily disabling the install step
      and confirming the job goes RED, then restore it.
- [x] 9.5 `openspec verify harden-istio-route-resolution`
- [x] 9.6 Reply on PR #7 covering EVERY review finding — including the three where this
      change's conclusion differs from the review's (dotted relative destination hosts are
      not a defect; ingress identity from backend rows is intended; the matcher parse is
      currently fail-closed) and the one the reviewer self-corrected (`recover()`).
