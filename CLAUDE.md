# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project purpose

`kube-state-graph` is a Go HTTP API that returns a unified pod / node / PVC graph
for **one or more Kubernetes clusters** read from a single centralised
VictoriaMetrics. Edges between pods come from `traces_service_graph_*` metrics
and may cross cluster boundaries.

The repo ships **only the API server**. `kube-state-metrics`, the service-graph
producer (Beyla / Alloy / Tempo or any compatible exporter), and VictoriaMetrics
are external dependencies. Topology comes from **kube-state-metrics** `kube_*`
series; service-graph edges come from `traces_service_graph_request_total`
(carrying `client_k8s_pod_uid` + `server_k8s_pod_uid`) — both read from
VictoriaMetrics. That upstream is **one or more** installations: a routing table
dispatches each query to the store(s) holding it, selected by availability zone
and by metric family (see `.claude/rules/upstream-queries.md`). With no routing
table configured it is a single endpoint at `--prom-url`, byte-for-byte as
before. Multi-cluster, cross-cluster, and service-graph
code paths are exercised by the integration tests in `internal/integration/`
via the testcontainers-go VictoriaMetrics container, which ingests hand-crafted
fixture series through `POST /api/v1/import/prometheus`.

## Common commands

Targets are in the `Makefile` (`make doctor` reports missing tooling). The
non-obvious ones:

```bash
make init                 # one-shot bootstrap: modules + golangci-lint + govulncheck
make init-hooks           # optional: point core.hooksPath at .githooks/ (pre-commit gofmt+lint+quick-test, pre-push `make ci`)
make ci                   # full CI mirror: lint vuln test check-docs verify-mocks + containment checks
make mocks                # after editing an interface listed in .mockery.yaml; commit <pkg>/mocks/ (CI: mocks-drift)
make docs                 # after editing swag @-annotations or a handler signature; commit docs/ (CI: docs-drift)

go test ./pkg/graph/ -run TestProject_ClusterFilter -v    # single test
go test ./internal/api/ -update -run Golden               # refresh goldens after an INTENDED wire change
./bin/kube-state-graph --prom-url=http://localhost:8428 --listen-addr=:8080
```

`make test` is `go test ./... -count=1 -race -shuffle=on`. Mockery and swag run
through `go tool` (go.mod `tool` directive) — no separate install. Generated
files (`<pkg>/mocks/*.go`, `docs/swagger.{json,yaml}`) are never hand-edited.

Module path: `github.com/akira-core/kube-state-graph`. Minimum Go 1.26 (`go.mod`); build toolchain pinned to `go1.26.6` via the `toolchain` directive.

## Architecture

### Request lifecycle

```
HTTP /v1/graph?start=&end=&...
   │
   ▼
parseGraphRequest        ── kubegraph.ParseValues → Request{Start, End, Scope, Selector}
   │                        validates start/end (RFC 3339 or Unix seconds; only `end > start`),
   │                        selector values (≤253 bytes, no control chars) and `prune`
   ▼
kubegraph.AlignWindow(start, end, --end-align)  ── runBuild, AFTER validation: floor end to the grid (default 30s), shift start equally
   ▼
context.WithTimeout(ctx, --build-timeout)   ── graph endpoints only; deadline exceeded → 504 timeout
   └─ Builder.Build(ctx, window, end, sel)
         ├─ ReadTopology  (errgroup of 37 PromQL queries in parallel — KSM topology incl. node ready_status + 3 D29 service/endpointslice + 2 D34 owner + PVC-info + container-info + 6 controller-annotation families + kube_job_owner + 12 Harvest + 2 kubelet + ALERTS; 19 fetch + 18 fetchOptional — kube_replicaset_annotations, kube_job_annotations and kube_pod_container_info degrade with Harvest/kubelet/ALERTS; one table, `topologyLegs`, drives both the launch and the RawSeriesCount tally — plus a SECOND WAVE of the 6 Harvest QoS workload legs, gated on kube_persistentvolumeclaim_info + volume_labels and scoped to the FlexVol names the loaded claims matched, so a build where none matched issues 37 queries and one where some did issues 43)
         ├─ ReadServiceGraph (errgroup of 3 PromQL queries in parallel: the required request total + 2 OPTIONAL RED — failed total + server-seconds histogram; `user`/`unknown` peers excluded at selector — D30; joined with topology)
         └─ assemble → attachAlerts → attachStatus → graph.NewGraph (immutable)
   (every upstream query passes its store's guard: query-result cache → miss coalescing → per-store slot, see .claude/rules/upstream-queries.md)
   ▼
graph.Project(g, scope)            ── projection-level filters (cluster/namespace again, prune)
   ▼
serialiseCytoscape

HTTP /v1/storage-graph?start=&end=&az=&env=&<exactly one root kind>
   ▼
kubegraph.ParseStorageValues  ── az/env required and repeatable; one root kind (missing_root / invalid_scope)
   ▼
Builder.BuildStorage(…, roots) ── readTopology under storagePlan: per-kind seed → claims → one expansion;
                                  no ReadServiceGraph, no up{} probe; fails closed; never leaves the request's zones
   ▼
graph.ProjectStorage → cytoscape.Serialise
```

v1 caches **upstream query RESULTS** in process (never built graphs or bodies) and coalesces concurrent identical misses; each request still builds and serialises its own graph. A horizontally scalable cache for distributed deployment (Redis L2, background materialiser, or graph DB) remains a separate, future change.

### Design reference — read before editing

The rules below are one-line statements of load-bearing, non-obvious behaviour.
Each has a full statement — trigger conditions, rationale, the tests that pin
it — in a reference file. **Read the reference file for an area before changing
code in it**; the one-liner is a reminder, not the rule.

| Working on | Read first |
|---|---|
| `pkg/build/servicegraph*.go`, `redmetrics.go`, `histogram.go`; any `pod-calls-*` / `service-selects-pod` edge, `data.metrics`, `traces_service_graph_*` | `.claude/rules/service-graph-resolution.md` — endpoint resolution ladder (D27 / D29 / D30 / D33), unknown-server peer enrichment, span-link marking, RED scope, filtered-build admission |
| `pkg/route/**`, `pkg/build/route*.go`, `--route-store-dsn` | `.claude/rules/route-resolution.md` — Istio route engine, ingress-cluster pick, LB-Service fallback, ingress chain, containment |
| `pkg/build/{netapp,qosscope,volumekey,volumelabelscope,claimscope,zone}.go`, `pkg/promql/{qosscope,volumelabels,harvestpair}.go`; Harvest series, `pvc-to-netapp-aggr`, `data.qos` | `.claude/rules/netapp-storage-join.md` — hops A / B / C, zone agreement, rooted `volume_labels` read, scoped QoS wave, ceiling key |
| `/v1/storage-graph`: `pkg/build/{topologyplan,claimseed,*seed,*scope,expansion,storageflow}.go`, `pkg/graph/{storagescope,project_storage}.go`, `kubegraph.ParseStorageValues` | `.claude/rules/storage-graph.md` — build lifecycle, root kinds, qualified roots, claim seed, multi-zone union |
| `pkg/promql/**`, `pkg/kubegraph/align.go`; a new `Query` constant, a selector, the cache, the routing table | `.claude/rules/upstream-queries.md` — per-store guard, end alignment, `queryDims` / `queryFamily`, selector rendering, backend routing |
| `pkg/graph/**`, `pkg/cytoscape/**`, `pkg/build/{topology*,clusteridentity,status,alerts}.go`; a node attribute, a node / edge type, projection, goldens | `.claude/rules/graph-model.md` — connectivity prune, cluster identity, `EdgeTypes`, typed attributes, compound nodes, sealed node types |

The files are path-scoped rules: each one's `paths:` frontmatter loads it
automatically when a matching file is read or edited, and is the source of truth
for the mapping — this table is the manual route (searches, subagents, planning).

The decision ids (D1–D34) refer to the archived design doc
`openspec/changes/archive/2026-06-06-add-k8s-pod-graph-api/design.md`; the
capability specs live under `openspec/specs/`.

### Load-bearing design rules

**Wire contract**

- **Deterministic response body.** The serialiser produces byte-identical output for the same `(window, filters, upstream-data)`, and every rendered upstream selector is a pure function of the sorted, de-duplicated parameter values (so `?az=b&az=a` and `?az=a&az=b` issue identical queries): node/edge slices MUST go through `graph.SortNodes`/`SortEdges`, `Graph.ClusterNames()` MUST sort, and the response body MUST NOT carry time-of-build or echo-of-input fields. Body shape is fixed at `{apiVersion, clusters, elements}`. Optional edge `data.metrics` (when present) is part of that contract — contributions are summed in ascending order and rounded to 6 significant digits so the wire form is order-independent. Every golden carrying a pod / K8s node / PVC / NetApp controller / aggregate intentionally carries an explicit `data.status`; hand-built golden fixtures must stamp the same `FoldStatus` result the builder bakes before `graph.NewGraph`. Don't add timestamps, random IDs, or unsorted map iteration to the response — golden tests will break.
- **`labels` is strict `map[string]string`** on nodes and edges — no bools, no numbers, no string-encoded numbers. Everything else is a typed, `omitempty` attribute and never appears inside `labels`: `ipaddress`, `owner`, `application`, `containers`, `ready_status`, `status`, `health`, `usage`, `storageclass`, `qos`, and edge `metrics`.
- **Edge IDs are UUIDv5** with a fixed compiled-in namespace (`graph.edgeNamespace`)
  and the canonical input `<type>|<source>|<target>`. Stable across rebuilds —
  required for golden tests. Bumping the namespace UUID is a v2 break.
- **Cluster-scoped IDs everywhere.** Pods: `<cluster>/<uid>`, K8s nodes:
  `<cluster>/<node>`, PVCs: `<cluster>/<namespace>/<claim>`, externals:
  `external/<value>`. Node names are not globally unique without the prefix.
- **`<cluster>` is the composed identity `<az>-<env>-<cluster>`**, composed at the ONE point a series' `cluster` label is read (`build.clusterResolver`, `bucket(query, metric)`); nothing downstream knows about zones. **`?cluster=` is the RAW name at both layers; `clusters[]` is the identity** — a `clusters[]` value sent back as `?cluster=` is an empty 200, deliberately.
- **Default projection is the connectivity-connected subgraph**: a pod is kept iff it is an endpoint of a connectivity edge, and infra hangs off kept pods. `?prune=false` is the only escape hatch; `cluster` / `namespace` / `az` / `env` never disable the prune. The prune is a projection concern and a pure function of the built graph.
- **`graph.EdgeTypes` is the single edge-type registry**: adding an edge type means updating the builder AND the registry in the same change. `GraphNode` is sealed; serialisation goes through its methods, never a type switch.
- **Compound nodes (`cluster` / `namespace` / `application` / `controller` / `storage-cluster`) are presentation-only**, synthesised in `pkg/cytoscape`. NetApp nodes belong to no Kubernetes cluster and stay out of `clusters[]`.
- **`data.status` is always present** on pods, K8s nodes, PVCs, NetApp controllers and aggregates (`graph.FoldStatus`, baked before `graph.NewGraph`); hand-built golden fixtures must stamp it too. `"normal"` means no negative signal, not full coverage.

**Request and upstream**

- **`/v1/graph` takes `start`, `end`, `cluster`, `namespace`, `az`, `env`, `prune`**; unknown parameters (including the withdrawn `name` / `root` / `depth` / `direction` / `edge_type`) are ignored without error. `GET /v1/clusters` is removed.
- **Only `end > start` is validated** — no window cap, no future-time guard. `kubegraph.AlignWindow` floors `end` to `--end-align` in exactly two places (`internal/api` `runBuild`, `Engine.BuildFromValues` / `BuildStorageFromValues`), never inside the parsers.
- **Two filter classes.** Selector-level (`cluster`, `namespace`, `az`, `env`) render as upstream label matchers per the hardcoded `promql.queryDims` table; projection-level (`prune`, plus `cluster` / `namespace` again) run over the built graph. The three `traces_service_graph_*` queries and `up` take NO request matcher. Each query's fixed selector must mirror a discard its Go reader already performs — never stricter.
- **A filtered build NEVER synthesises a pod**, admits a service-graph series only when a resolved endpoint names loaded topology, and returns an empty 200 (never `outside_retention`) when nothing matched.
- **Every upstream query passes a per-store guard** — result cache → miss coalescing → concurrency slot — ON by default for server and library. Cached vectors are shared: **readers in `pkg/build` MUST NOT mutate a sample or its `Metric` map**. `pkg/build.New` stays unguarded (the test seam).
- **Upstream is a routed table of backends** (`promql.Router`; no `--backends-file` ⇒ one implicit `default` backend). `queryFamily` is exhaustive beside `queryDims`; the cross-backend merge MUST de-duplicate by label-set fingerprint (readers sum); required legs fail closed; credentials are env-var names, never literals; file parsing lives in `pkg/promql/backendsfile`, never `pkg/promql`.
- **No configurable metric-name prefix.** `--metric-prefix` / `KSG_METRIC_PREFIX` / `Renderer.Prefix` are removed. Every series is queried at its bare name (`promql.Render(q, window)`). A deployment whose KSM series ARE prefixed silently returns an empty graph — see `docs/BREAKING.md`. The D29 endpointslice → service join still reads `kube_endpointslice_labels{label_kubernetes_io_service_name}`, which KSM only emits when `--metric-labels-allowlist=endpointslices=[kubernetes.io/service-name]` is set. The metric-name suffix and the label-name set per series are a fixed contract any compatible exporter MUST honour.

**Service graph**

- **Per-endpoint resolution order**: (1) empty UID + `"://"` label → a single `service` node in the caller's own cluster, else `external`; (2) non-empty UID → topology pod, else synth pod; (3) empty UID + other non-empty label → `external/<label>`; (4) drop. A `"://"` endpoint is never a pod.
- **Fixed selector contracts, no knobs**: the D30 sentinel matcher (`client!~"user|unknown",server!~"user"`), the D33 self-loop UID guard, and `edge_relation!="link"` on the two RED companion queries.
- **`server="unknown"` produces a node only through the peer-address ladder** (real client pod required): `client_server_address` → `client_network_peer_address` → `client_net_peer_name`, classified DNS → bare name → ClusterIP (anchor cluster only) → Pod IP (family, ambiguity degrades) → route engine → `external/<raw>`.
- **Route resolution is opt-in and can never fail a build.** I/O stays out of the parse (prescan → prefetched index). `pkg/build` declares only `RouteResolver` and MUST NOT import `pkg/route` (`make check-route-containment`).
- **RED metrics live on the typed `Edge.Metrics`** (`data.metrics`), only on trace-derived edges whose both endpoints are pod-or-service nodes; companion-query failures degrade field by field.

**Storage**

- **The NetApp join is three independently-degrading hops** keyed on the PVC's `volumename` rewritten to a token and suffix-matched against the stock Harvest `volume` label — never a label equality. `volume_labels` is the sole topology source; a QoS miss leaves a measurement-less edge; an incomplete ceiling key is ignored, never widened.
- **`/v1/storage-graph` requires `az` + `env` and exactly one root kind, fails closed on any query error (except `ALERTS`), and never reads a zone the request did not select.** A request-derived scope past the chunk cap is 400 `invalid_scope` before any query; a data-derived scope is chunked, never widened. A multi-zone body is the union of its zones.
- **Every Harvest series must carry the configured `az` / `env` labels**; ONTAP cluster and controller names are unique across the estate, aggregate and SVM names are not (hence the qualified `<ontap_cluster>/<name>` root).

**Operations**

- **API-key auth is the only HTTP auth in v1.** Header is `X-API-Key`. Keys
  come from `--api-keys-file` (K8s `Secret` mount, hot-reloaded) or
  `--api-keys`. Empty keyset = auth disabled (dev default). Open paths
  (no key required): `/livez`, `/readyz`, `/metrics`, `/openapi.*`, `/docs`.
  The Scalar UI at `/docs` is a tiny HTML page that loads the Scalar bundle
  from the jsDelivr CDN and renders the same-origin `/openapi.json`; the spec
  itself is generated by `swag` into `docs/` and embedded via `docs/embed.go`.
  Validation is constant-time and iterates the whole set —
  do NOT add early-return optimisations to `auth.KeySet.Validate`. Logs must
  never include the presented key value.
- **OTLP tracing/logging is config'd by OTel env vars only** (`OTEL_EXPORTER_OTLP_*`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER`). No bespoke `--otlp-*` flags. Telemetry defaults to no-op when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset (zero export overhead, no background goroutines). Tracing MUST NOT alter response bodies — resource attrs and span IDs live on spans, never in JSON. `otelgin` is mounted on `/v1/*` only; `/livez`, `/readyz`, `/metrics`, and `/docs/*` are deliberately untraced. The auth middleware MUST NEVER log or attribute the presented `X-API-Key` value via either the local handler or the OTLP slog bridge.

### Reusable `pkg/` graph engine (D32)

The graph engine lives under `pkg/` so other Go modules can import it in-process
(no HTTP, no JSON round-trip); `internal/api` is a thin HTTP / auth shell over it:

- `pkg/graph` — `Graph`, the sealed `GraphNode` + eight node types, `Edge`,
  `Project` / `ProjectStorage`, `Scope` / `NewScope` / `StorageScope` / `NewStorageScope`, `View`, `SortNodes` / `SortEdges`, `EdgeTypes`.
  `Graph` carries **no adjacency index** — the `Forward` / `Reverse` maps existed
  only for the withdrawn `?root=&depth=` traversal, and every surviving consumer
  (projection, the connectivity prune, serialisation) scans `Edges` once.
- `pkg/build` — `Builder` + `Build`; topology / service-graph / NetApp readers. Takes a
  `build.Options{APITimeout, LabelKeys}` and a no-op-tolerant `build.Metrics`
  interface — **not** `internal/config` / `internal/observability`, whose
  couplings were broken so the package is externally importable.
- `pkg/promql` — `Querier`, `Render(q, window, keys, sel)`, `Selector`,
  `LabelKeys`, `Client`, `Router.QueryLabels`, and a no-op-tolerant `promql.Metrics` interface.
- `pkg/clock`; `pkg/cytoscape` — `Serialise(g, view) Body` plus the Cytoscape DTO.
- `pkg/kubegraph` — the convenience facade: `Engine.BuildFromValues(ctx,
  url.Values) (cytoscape.Body, error)` folds parse → build → project → serialise
  into one call. `kubegraph.ParseValues` / `ParseStorageValues` are the
  **single** request parsers, shared by the HTTP handlers and the facade, so
  the `/v1/graph` and `/v1/storage-graph` contracts cannot drift.
  `Engine.BuildStorageFromValues` is the storage-graph counterpart.

`pkg/` packages MUST NOT import `internal/*` — Go's internal rule would block any
external module from importing the engine. Metrics and OTLP tracing are injected
with no-op defaults, so an embedder does not inherit ksg's `kube_state_graph_*`
self-metrics; the concrete `*observability.Metrics` satisfies
`build.Metrics` / `promql.Metrics` structurally via wrappers in
`internal/observability/adapters.go`. The upstream load controls (limit, cache,
end alignment) are ON for an embedder by default — see `.claude/rules/upstream-queries.md`;
`kubegraph.Options` gains `EndAlign`, `MaxConcurrency`, `QueryCacheMaxSeries`,
`QueryCacheTTL` (zero ⇒ default, negative ⇒ off) and `promql` exports `Guard`,
`RouterOption`, `WithMaxConcurrency`, `WithQueryCache`, `WithGuardMetrics`.

### Sealed graph types

`graph.GraphNode` is a sealed interface (`isGraphNode()` unexported). Concrete
types: `PodNode`, `K8sNode`, `PVCNode`, `ServiceNode`, `ExternalNode`,
`NetAppAggrNode`, `NetAppNode`, `NetAppSVMNode`. Every attribute is a method on
the interface returning its zero value for the kinds it does not apply to;
serialisation goes through these methods — never through type switches in the
serialiser. Per-method semantics: `.claude/rules/graph-model.md`.

### Test stack layers

Boundary rule: **unit tests must not contact a real upstream service**. Anything
that needs a TCP socket fronting upstream is integration. Unit tests substitute
upstream behind small interfaces (`promql.Querier`, `auth.Validator`,
`clock.Clock`, …) using the mockery-generated mocks in each interface's
`<pkg>/mocks/` — `.mockery.yaml` is the list.

| Layer | Where | Real I/O? |
|---|---|---|
| Unit | `pkg/{graph,build,promql,clock,cytoscape,kubegraph}/*_test.go` + `pkg/route/...` (except `oracle_test.go`, `-tags oracle`) + `internal/{config,auth,telemetry}/*_test.go` | None — pure functions: parsers, joins, projection, edge IDs, request parsing, serialiser, KeySet, Clock. |
| Component | `internal/api/*_test.go` | None — gin handlers driven via a `MockQuerier` injected through `promql.Querier`; `httptest.NewServer` only wraps the server-under-test, never fakes upstream. Test helpers in `internal/api/helpers_test.go` (`newServerWithMocks`, `newMockQuerier`, `newErrQuerier`, `vec`). |
| Golden | `internal/api/golden_test.go` + `testdata/golden/*.json` | None. Wire-format snapshots; run with `-update` to refresh. |
| Property | `pkg/graph/property_test.go` | None. Random multi-cluster graphs → invariants (orphan edges, pruned ⊆ inventory, ID uniqueness). |
| Integration | `internal/integration/*` | **Docker required.** testcontainers-go VictoriaMetrics suite; gated `SkipIfDockerUnavailable` — skips locally without Docker, runs full on CI (ubuntu-latest). Inject hooks into the in-process API via `StartAPIServer(cfg, WithClock(...))`. |

When **adding a unit test that needs to fake upstream PromQL**, use
`newMockQuerier(t, fixtureSet{...})` — never spin up an `httptest.NewServer`
to impersonate the Prometheus HTTP API.

When **changing an interface** registered in `.mockery.yaml` (the file is
the list), run `make mocks` and
commit the regenerated files. CI's `mocks-drift` job will fail otherwise.

## OpenSpec workflow

Spec-driven changes live under `openspec/changes/<name>/` with four artifacts
in dependency order: **proposal → design + specs → tasks**. The
`/opsx:*` commands and the `openspec` CLI manage the lifecycle.

Common openspec commands:

```bash
openspec list                                       # all active changes
openspec status --change "<name>"                   # artifact progress + tasks
openspec validate "<name>"                          # checks structure
openspec instructions <artifact> --change "<name>" --json   # what to write
openspec archive "<name>"                           # promote to openspec/specs/
```

Before archiving, check the implementation against the change with the
`/opsx:verify` skill; the CLI has no `verify` subcommand.

The v1 implementation change **`add-k8s-pod-graph-api`** is archived under
`openspec/changes/archive/2026-06-06-add-k8s-pod-graph-api/`; its capability
specs were promoted to `openspec/specs/`. When making non-trivial behaviour
changes, start a new change and write its delta specs under
`openspec/changes/<name>/specs/<capability>/spec.md` before touching code;
`openspec archive` (or `/opsx:sync`) promotes them into
`openspec/specs/<capability>/spec.md`.

## Repository conventions

- **Conversational output is written in Traditional Chinese (繁體中文).** This
  covers the two artifacts of a Claude Code session: the plan file
  (`~/.claude/plans/*.md`) and the explanatory prose in chat replies. Code,
  identifiers, API names, CLI commands, commit-type keywords (feat/fix/…) and
  error strings stay verbatim in English. This rule does **NOT** apply to
  anything persisted in the repo — OpenSpec artifacts (`proposal.md`,
  `design.md`, `tasks.md`, `spec.md`), code comments, commit messages,
  `CLAUDE.md` itself, and all other docs stay in English, consistent with the
  existing codebase.
- All HTTP routes live under `/v1/`. Adding a route means committing to keeping
  it for v1's lifetime. Schema changes that aren't additive are v2 — see D14.
- Self-metric names are stable contracts: `kube_state_graph_*`. Adding a label
  to an existing metric is a contract change — see design.md D26.
- Errors returned to HTTP carry a typed `build.Reason` mapped to a fixed
  status + `reason` string in `internal/api/errors.go`. Adding new failure
  modes means adding both a `Reason` constant and an entry in `mapBuildError`.
- Don't **talk to the Kubernetes API** from the API server — no client-go
  clients, no informers, no watches, no kubeconfig, no per-cluster RBAC. All
  cluster facts come from VictoriaMetrics (topology, service graph) or the
  versioned Istio-config store (route resolution). The rule's reasons
  (archived design D1 / D16): informers only know the *current* state and
  cannot answer this API's historical `?start=&end=` contract, and
  multi-cluster would need N watch streams + per-cluster RBAC. **Linking a
  library that transitively vendors Kubernetes types is NOT a violation;
  constructing a Kubernetes client is** (translate-global-fqdn-to-k8s-service
  D0) — `pkg/route` links `istio.io/istio` (→ `k8s.io/client-go` types) purely
  as an in-memory translation library and never dials an apiserver. Tests and
  harness tooling are exempt.
- Don't add dependencies casually. Current direct deps: Gin, Prometheus
  client_golang + common (`model`), google/uuid, golang.org/x/sync,
  `sigs.k8s.io/yaml` (routing-file parser, `pkg/promql/backendsfile`), testify v1.12.x (test-only,
  also drives mockery-generated mocks), testcontainers-go (integration
  test-only), swaggo/swag/v2 (codegen tool, not imported at runtime),
  vektra/mockery v2.x (codegen tool tracked via go.mod `tool` directive,
  not imported at runtime, not linked into the production binary), the
  OpenTelemetry Go SDK family (`go.opentelemetry.io/otel`, `sdk`, `sdk/log`,
  OTLP gRPC + HTTP exporters for `otlptrace` and `otlplog`, `semconv/v1.27.0`,
  `contrib/...otelgin`, `contrib/...otelhttp`, `contrib/bridges/otelslog`),
  and — **contained to `pkg/route` + `cmd/` only** (see
  `.claude/rules/route-resolution.md`) — `istio.io/istio` + `istio.io/api` (pinned; in-process istiod
  translation), `ClickHouse/clickhouse-go/v2` (route store),
  `envoyproxy/go-control-plane/envoy` (RouteConfiguration protos), and
  `google.golang.org/protobuf`.
  Adding more requires a design-doc note.
- Production code MUST NOT carry test-only fields, methods, or constructors.
  Inject substitutable behaviour via the small interfaces registered in
  `.mockery.yaml` (`promql.Querier`, `auth.Validator`, `clock.Clock`, …);
  tests consume the mockery-generated mocks under `<pkg>/mocks/`. If a new
  hard-to-test dependency appears, add an interface + regenerate mocks rather
  than a `SetXxxFunc` setter.
