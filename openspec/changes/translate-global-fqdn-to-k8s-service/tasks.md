# Tasks

Ports the `poc/route2a` Istio route-resolution engine into `pkg/route/`, injects it into
`pkg/build` behind a `RouteResolver` interface, and consults it at every external fallback in
`resolveUnknownServerPeer`. Feature is off by default (`--route-store-dsn` empty ⇒ nil resolver ⇒
byte-for-byte unchanged output), which is also the regression net.

**Blocker**: the route store's `cluster` column is a new requirement on the metadata-exporter repo
(design D7). Group 2 cannot be verified against a real store until it lands; the ClickHouse
testcontainer in group 8 seeds the schema itself, so implementation is not blocked.

## 1. Interface + injection (no behaviour change)

- [x] 1.1 Add `pkg/build/routeresolve.go`: `RouteRequest` (`Cluster`, `Host`, `Path`, `Port`, `IPs`,
      `Start`, `End`), `RouteDestination` (`Namespace`, `Service`, `Port`, `Subset`), and the
      `RouteResolver` interface. Document that nil = feature off.
- [x] 1.2 Add `RouteResolver` to `build.Options`; mirror it on `kubegraph.Options` and thread it
      through `kubegraph.New` → `build.New`.
- [x] 1.3 Register `build.RouteResolver` in `.mockery.yaml`, run `make mocks`, commit the generated
      mock. `make verify-mocks` clean.
- [x] 1.4 Confirm `go build ./...` and the full existing suite still pass — nothing is wired yet.

## 2. Route store (`pkg/route/store`) — read-only, cluster-scoped

- [x] 2.1 Port the row types and helpers from `poc/route2a/internal/store`: `ServiceRow`, `DeployRow`,
      `GatewayRow`, `VSRow`, `GatewayCand`, `TrafficWindow`, `SvcPort`, `BackendFQDN`,
      `ParseBackendHost`, `MarshalPorts`/`ParsePorts`. Add a `Cluster` field to every row type.
- [x] 2.2 Define the read-only `Store` interface: `LoadTrafficWindow(ctx, cluster, ip, t0, t1)`,
      `LoadConfigWindow(ctx, cluster, t0, t1)`, `Close()`. **Drop** `CreateSchema` and all four
      `*Batch` inserters — ksg never writes.
- [x] 2.3 Port the ClickHouse implementation from `poc/route2a/internal/chstore`, adding a
      `cluster = ?` predicate to every query. Interval semantics unchanged
      (`valid_from < t1 AND t0 < valid_to`).
- [x] 2.4 Implement `LoadConfigWindow` — the IP-less variant the POC lacks: gateways live in the
      window for the cluster, their bound VirtualServices, and the backend Services those VS route
      to. (`LoadTrafficWindow` is always rooted at an IP.)
- [x] 2.5 Add startup schema validation: assert the expected tables and columns exist; **fail fast**
      on drift rather than silently returning empty windows (design D7).

## 3. Route engine (`pkg/route/{gwresolve,translate,memwindow,matchcheck}`)

- [x] 3.1 Port `gwresolve` verbatim (`Resolve`, `ResolveAmong`, most-specific host disambiguation)
      plus its tests.
- [x] 3.2 Port `translate` verbatim: `ScopedInput` (incl. `Port`), `Translator.Translate`,
      `routeConfigNameFor` (`http.<port>` / `https.<port>.<portName>.<gw>.<ns>`; `ok=false` for a
      port with no server or a TLS-passthrough server).
- [x] 3.3 Port `memwindow` verbatim (interval slicing, in-memory 3-hop, `ConfigSigAt` content
      signature, `ScopedFor`, `GatewaysLiveAt`) plus its tests — including
      `TestConfigSigCoversScopedInputs`, which guards the load-bearing signature/scoped-input
      superset invariant.
- [x] 3.4 Port `matchcheck`, **dropping the docker fallback**: native binary only, path injected from
      config. Keep the `__routecheck_unmatched_sentinel__` trick, the `--details` output parse, and
      `--disable-deprecation-check`.
- [x] 3.5 Add `go.mod` deps: `istio.io/istio`, `istio.io/api`, `github.com/ClickHouse/clickhouse-go/v2`,
      `github.com/envoyproxy/go-control-plane/envoy`. Pin the istio version (design open question).

## 4. Resolver (`pkg/route/resolver.go`) — the `build.RouteResolver` impl

- [x] 4.1 Port the interval orchestration from `poc/route2a/internal/rangequery`: load window → slice
      → per-segment 3-hop + host disambiguation → per-distinct-config translate + `router_check_tool`.
- [x] 4.2 Branch on `RouteRequest.IPs`: non-empty ⇒ `LoadTrafficWindow` (traffic_simulation);
      empty ⇒ `LoadConfigWindow` + `GatewaysLiveAt` + `gwresolve.Resolve` (config_only).
- [x] 4.3 Add the Envoy cluster-string parser: `outbound|<port>|<subset>|<svc>.<ns>.svc.cluster.local`
      → `RouteDestination`. Unit-test the malformed / empty / subset-bearing forms.
- [x] 4.4 Return typed outcomes distinguishing "no gateway", "no listener on port", "no route
      matched", and "store error", so the caller can log a distinct reason for each.
- [x] 4.5 Assert the containment rule (design D1 — dependency hygiene, **not** the client-go rule):
      `pkg/build` MUST NOT import `pkg/route`. Add a CI check that `go list -deps ./pkg/kubegraph`
      never reaches `k8s.io/client-go`, so an embedder never inherits istio by accident.
- [x] 4.6 Assert design D0 by review: the engine MUST NOT construct a Kubernetes client, informer, or
      watch, and MUST NOT read a kubeconfig. istio is used as a pure library — `model.Environment` is
      hand-built over a memory config store + `memregistry`, exactly as the POC's translator does.

## 5. Wire into `resolveUnknownServerPeer` (`pkg/build/servicegraph.go`)

- [x] 5.1 Change `stripPeerAddressPort` to **return** the port it currently discards. Existing callers
      keep using the host alone — behaviour unchanged.
- [x] 5.2 Read the new OPTIONAL dimensions in `parseServiceGraph`'s sample loop: `client_dns_answers`
      (destination IPs) and `client_server_port` / `client_net_peer_port`. Absence must degrade
      gracefully.
- [x] 5.3 Implement the listener-port derivation: peer-address `:port` → optional label → default
      **443** (design D5).
- [x] 5.4 Extract the classification ladder out of `resolveUnknownServerPeer` into one pure
      `classifyPeerHost` helper, so the prescan and the resolver cannot drift (design D2).
- [x] 5.5 Add `collectRouteQueries(vec, topology)` — a pure prescan collecting a `RouteRequest` for
      every endpoint that would reach `r.external(value)`. **All three** branches:
      `not_k8s_dns`, `ip_literal_no_match`, and — critically — `anchor_lacks_service`, which is the
      one a global FQDN actually takes (design D3).
- [x] 5.6 In `ReadServiceGraph`, run the prescan, resolve each query serially (bounded by
      `--route-resolve-timeout`; errors recorded, never fatal), and pass the resulting
      `map[routeKey]RouteDestination` into `parseServiceGraph`.
- [x] 5.7 Add the index param to `parseServiceGraph`, keeping a 2-arg wrapper (nil index) so the ~7
      existing direct call sites in `pkg/build/servicegraph*_test.go` keep compiling.
- [x] 5.8 In `resolveUnknownServerPeer`, consult the index at each of the three external branches; a
      hit resolves through the **existing** `resolveServiceLevel(anchorCluster, ns, svc)` unchanged.
      A miss falls to `external` exactly as today.
- [x] 5.9 Add the new `noteExternal` reasons: `route_engine_miss`,
      `route_engine_no_listener_on_port`, `route_engine_error`.

## 6. Config + runtime

- [x] 6.1 `internal/config`: add `--route-store-dsn` / `KSG_ROUTE_STORE_DSN` (empty ⇒ feature off),
      `--router-check-bin` / `KSG_ROUTER_CHECK_BIN` (default `/usr/local/bin/router_check_tool`),
      `--route-resolve-timeout` / `KSG_ROUTE_RESOLVE_TIMEOUT`. Extend `Validate`.
- [x] 6.2 `cmd/kube-state-graph/main.go`: construct the `pkg/route` resolver when the DSN is set
      (validating the schema and the `router_check_tool` binary at startup); pass nil otherwise.
- [x] 6.3 `Dockerfile`: multi-stage
      `COPY --from=envoyproxy/envoy:tools-v1.34-latest /usr/local/bin/router_check_tool /usr/local/bin/`.
      Verify the binary runs in the final image.

## 7. Unit tests (no I/O)

- [x] 7.1 `pkg/build/servicegraph_test.go`: global FQDN with a **prefilled** route index → service
      node + `pod-calls-service` edge (still `parseServiceGraph`, still pure).
- [x] 7.2 Index miss → `external/<raw value>`, unchanged.
- [x] 7.3 **Regression**: with a nil index, the `.svc` DNS / bare-short-name / ClusterIP ladders
      behave exactly as before.
- [x] 7.4 Port-derivation cases: `:port` in the peer address wins; the optional label is next;
      neither ⇒ 443.
- [x] 7.5 `collectRouteQueries` collects **exactly** the three external branches — in particular it
      MUST collect the `anchor_lacks_service` case (`api.example.com`), the regression that would make
      the whole feature a silent no-op.
- [x] 7.6 `ReadServiceGraph` with a mocked `RouteResolver`: a resolver error degrades to `external`
      and the build still succeeds.

## 8. Integration (Docker)

- [x] 8.1 Add a ClickHouse testcontainer to `internal/integration` alongside the existing
      VictoriaMetrics one; seed a Gateway / VirtualService / Service corpus with the `cluster` column.
- [x] 8.2 End-to-end: a `server="unknown"` series with `client_net_peer_name="api.example.com"` and
      `client_dns_answers=<ingress LB IP>` → `/v1/graph` contains the `pod-calls-service` edge to the
      routed Service and **no** `external/api.example.com`. Seed a TLS-terminated `:443` server so the
      default-443 path and the `https.<port>.<portName>.<gw>.<ns>` route-config name are both
      exercised.
- [x] 8.3 Second case: an explicit `:8080` in `client_server_address` against an HTTP listener.
- [x] 8.4 `t.Skip` when `router_check_tool` is absent, mirroring the existing
      `SkipIfDockerUnavailable` gate.

## 9. Oracle cross-check

- [x] 9.1 Port the POC's `scalegen` by-construction oracle as a build-tagged long test
      (`-tags oracle`), demonstrating the ported engine reproduces the POC's 0-mismatch result over
      600 gateways × 100 VirtualServices.

## 10. Docs + verify

- [x] 10.1 **Reword** the `CLAUDE.md` rule "Don't import `k8s.io/client-go` or any Kubernetes API into
      the API server" to state its actual intent (design D0): don't *talk to* the Kubernetes API — no
      clients, informers, watches, kubeconfig, or per-cluster RBAC. Linking a library that transitively
      vendors Kubernetes types is not a violation; constructing a Kubernetes client is. Cite the route
      engine as the motivating case.
- [x] 10.2 Add a `CLAUDE.md` bullet for the route-resolution step: the three-branch trigger, the
      port-derivation rule, the optional dimensions, off-by-default, and the `pkg/build` ↛ `pkg/route`
      containment rule (dependency hygiene — distinct from 10.1).
- [x] 10.3 **Feature-off regression**: with `KSG_ROUTE_STORE_DSN` unset, `make test` (race + shuffle,
      golden files included) passes **unchanged**.
- [x] 10.4 `go vet ./...` clean; `make lint` 0 new issues; `make docs` / `make check-docs` clean.
- [x] 10.5 `openspec validate translate-global-fqdn-to-k8s-service` passes.

## 11. Production reader compatibility (post-implementation POC sync)

- [x] 11.1 Run every version-table read with `FINAL`: the exporter closes versions by rewriting the
      open row (same key, higher ingest_seq), so pre-merge duplicates must collapse at query time.
      GROUP BY+argMax fallback (valid_to in HAVING) documented in the CH doc comment.
- [x] 11.2 Drop `rev` from the reader entirely (expectedSchema, SELECTs, row types): a POC-only
      oracle column absent from production tables — selecting it fails with Unknown column, and
      validateSchema would have failed fast at startup.
- [x] 11.3 Parse `spec_json` with `protojson DiscardUnknown` (shared `pjUnmarshal` in store +
      memwindow): production spec_json follows the cluster's CRD version, not the reader's
      istio.io/api pin.
- [x] 11.4 Match bare gateway refs: `memwindow.boundTo` (qualified `<ns>/<name>`, or bare `<name>`
      iff VS ns == gateway ns) used by ScopedFor AND ConfigSigAt; store loads the `hasAny` superset
      over both forms. Unit tests (`TestBoundTo`, `TestScopedForBareGatewayRef`,
      `TestScopedForToleratesUnknownSpecFields`) + integration seed reshaped to production form
      (rev-free DDL, verbatim bare binding, stale-open/closing row pair proving FINAL) — both
      route suites re-verified green end to end.

## 12. Read-mode: no-FINAL client dedup + pruned opt-in + dt64 literals (POC sync round 2)

- [x] 12.1 Replace FINAL with the no-FINAL pattern: SQL keeps only immutable predicates
      (`valid_to` NEVER filtered in SQL), client-side `dedupLatest` per version slot (max
      ingest_seq), overlap check post-dedup in Go. ~10x cheaper than FINAL on the POC bench and
      independent of server PREWHERE settings.
- [x] 12.2 `WithUniqueRows()` store option + `--route-store-unique-rows` / 
      `KSG_ROUTE_STORE_UNIQUE_ROWS` (default false): restores the SQL valid_to prune for
      update-close writers only; hazard against rewrite-close writers documented in the CH doc
      and proven by `TestUniqueRowsAgainstRewriteWriterResurrectsStaleRow`.
- [x] 12.3 `CollapsedRows()` counter: dedup demoted to counted safety net; production
      metric/alert wiring tracked as follow-up.
- [x] 12.4 `dt64Lit` literals for every time operand in reader queries (clickhouse-go `?` binds
      interpolate time.Time at second precision — ms truncation + 2200-sentinel saturation);
      integration seed writes times as string literals for the same reason. Unit tests:
      `pkg/route/store/clickhouse_test.go` (rewrite-pair collapse both arrival orders, distinct
      slots survive, half-open overlap boundaries, dt64Lit format incl. sentinel + UTC
      normalisation, prune fragment on/off). Both route suites re-verified green end to end.
