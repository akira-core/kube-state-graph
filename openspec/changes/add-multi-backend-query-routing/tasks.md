## 1. Query family classification (`pkg/promql`)

- [x] 1.1 Add the `Family` type and its five constants (`ksm`, `kubelet`, `harvest`, `servicegraph`, `probe`) in `pkg/promql/queries.go`, beside `queryDims`, and verify `go build ./...` succeeds
- [x] 1.2 Add the exhaustive `queryFamily map[Query]Family` table covering every declared `Query` constant, and verify a new `TestQueryFamily_EveryQueryListed` (modelled on `TestQueryDims_EveryQueryListed`, parsing this file's `Query` constants) fails when an entry is removed and passes with the full table
- [x] 1.3 Add `func (f Family) AcceptsAZ() bool` derived from `queryDims` (true iff the family's queries carry `dimAZ`), and verify a unit test pins `ksm`/`kubelet`/`harvest` true and `servicegraph`/`probe` false

## 2. Routing table value type (`pkg/promql`)

- [x] 2.1 Add `pkg/promql/backend.go` declaring the immutable `Backend` (name, url, families, zones, resolved username/password) and `Table` types with unexported fields plus accessors, and verify `go vet ./pkg/promql/...` passes
- [x] 2.2 Implement `NewTable([]Backend) (*Table, error)` performing every validation in the `upstream-backend-routing` spec's "Declarative upstream backend table" requirement (non-empty table, unique names, parseable http/https URL, non-empty known families, every family served), and verify unit tests cover one rejection case per rule with the error naming the offending backend or family
- [x] 2.3 Implement `Table.Select(f Family, az []string) []Backend` returning candidates in ascending name order per the spec's "Backend selection by requested availability zone" rule (family match, then zone intersection only when `f.AcceptsAZ()`), and verify unit tests cover: single zone, absent zone, multi-zone subset, catch-all backend, servicegraph/probe ignoring zones, and an unmatched zone returning an empty slice
- [x] 2.4 Verify `Table` carries no credential value in its `String`/log representation by a test asserting a formatted table containing a configured password does not contain that password

## 3. Fan-out querier and merge (`pkg/promql`)

- [x] 3.1 Add `QuerierSource` (`QuerierFor(sel Selector) Querier`) to `pkg/promql/querier.go` with the D1 rationale comment, and verify `go build ./...` succeeds
- [x] 3.2 Implement `mergeVectors(parts []model.Vector) model.Vector` — concatenate in the caller's (backend-name-sorted) order, drop a sample whose label-set fingerprint was already kept — and verify unit tests cover disjoint concatenation, exact-duplicate collapse, differing-value duplicate keeping the first, and byte-identical output for reversed arrival order
- [x] 3.3 Implement the bound fan-out `Querier` returned by `QuerierFor`: resolve family from the query name, select backends, issue the identical query string to each under an `errgroup` with `SetLimit(len(selected))`, merge per 3.2, and verify a unit test with two fake backends asserts both receive byte-identical query strings and the merged result is de-duplicated
- [x] 3.4 Implement the D6 failure semantics — any backend error fails the call with an error naming that backend — and verify a unit test asserts the error text contains the failing backend name and that no partial vector is returned
- [x] 3.5 Implement the empty-candidate path — return an empty vector plus a Warn naming the family and unmatched zone values — and verify a unit test asserts no error and no upstream call is made
- [x] 3.6 Add the optional `RouterMetrics` upgrade interface (backend gauge, reload counter, per-backend failure counter) type-asserted from `promql.Metrics`, leaving `Metrics` itself unchanged, and verify a unit test with a plain `Metrics` implementation records no panic and no new series

## 4. Router with atomic swap (`pkg/promql`)

- [x] 4.1 Implement `Router` holding an `atomic.Pointer` to its live state, constructed from a `*Table` and a per-backend client factory, satisfying **both** `Querier` and `QuerierSource`, and verify a compile-time assertion (`var _ Querier`, `var _ QuerierSource`) plus a unit test that `QuerierFor(Selector{})` routes as an unfiltered request
- [x] 4.2 Implement `Router.Swap(*Table) error` validating before replacing and keeping the previous state on error, and verify a unit test asserts routing is unchanged after a rejected swap and changed after an accepted one
- [x] 4.3 Implement D12 client reuse — key clients by `(url, username, password)`, carry unchanged keys across a swap, call `CloseIdleConnections()` on retired ones — and verify a unit test asserts a same-URL backend keeps its client identity across a swap and a removed one is closed
- [x] 4.4 Implement snapshot consistency: `QuerierFor` reads the pointer once and the returned `Querier` closes over that state, and verify a unit test that swaps the table mid-flight and asserts the bound querier still dispatches by the original table
- [x] 4.5 Route the `probe` family through the same fan-out so `Router.Instant(ctx, "up", ...)` reaches every probe-serving backend and fails naming any that did not answer, and verify a unit test with one failing fake backend asserts the error names it

## 5. Builder integration (`pkg/build`)

- [x] 5.1 Add the `QuerierSource` upgrade assertion in `build.New` (store the source when the passed `Querier` satisfies it), leaving the signature unchanged, and verify a unit test asserts a plain mock `Querier` leaves the source nil
- [x] 5.2 Resolve the per-build querier once at the top of `Builder.Build` and thread it through `ReadTopology`, `ReadServiceGraph`, and `upProbe`, and verify the existing `pkg/build` suite still passes unchanged with a plain mock
- [x] 5.3 Verify with a new test that a routed build issues each topology leg to every selected backend and that the assembled graph joins across them — a claim read from one fake backend joining a `volume_labels` series read from another produces a `pvc-to-netapp-aggr` edge
- [x] 5.4 Verify the 37-leg fan-out pin in `pkg/build/netapp_test.go` still holds under routing (leg count is per-build, not per-backend)

## 6. Trace and metric attribution

- [x] 6.1 Add the backend name as an attribute on the existing `prometheus.query` client span, and verify a unit test with a recording span exporter asserts the attribute is present and names the backend
- [x] 6.2 Add `kube_state_graph_upstream_backends`, `kube_state_graph_backend_config_reload_total{result}`, and `kube_state_graph_backend_query_failures_total{backend}` to `internal/observability` and wire them through the `RouterMetrics` adapter, and verify a `/metrics` component test asserts all three names appear
- [x] 6.3 Verify by test that `kube_state_graph_upstream_query_duration_seconds` and `kube_state_graph_upstream_query_failures_total` carry exactly their pre-change label sets

## 7. File parsing and configuration (`internal/config`, `cmd/`)

- [x] 7.1 Promote `sigs.k8s.io/yaml` to a direct dependency and verify `go mod tidy` adds no new module to `go.sum` beyond the promotion
- [x] 7.2 Add the file schema struct and `ParseBackendsFile([]byte, LookupEnvFunc) (*promql.Table, error)` in `internal/config`, accepting YAML and JSON, and verify unit tests assert a YAML file and its JSON equivalent produce identical tables
- [x] 7.3 Implement per-backend credential resolution (D8): `usernameEnv`/`passwordEnv` resolved from the injected lookup, half-declared pair rejected, named-but-unset variable rejected, fallback to the global pair, and verify one unit test per rule asserting the error never echoes a value
- [x] 7.4 Reject a file carrying a literal `username`/`password` field, and verify a unit test asserts the error explains that credentials are environment-only and does not echo the value
- [x] 7.5 Add `BackendsFile` (`--backends-file` / `KSG_BACKENDS_FILE`) and `BackendsReloadInterval` (`--backends-reload-interval`, default matching the API-key reloader) to `config.Config` with `Defaults()` and `Validate()` coverage, and verify `internal/config` tests assert flag-over-env-over-default precedence
- [x] 7.6 Implement the D9 implicit table in `cmd/` when no file is configured (`default` backend, `--prom-url`, all five families, no zones) and the precedence Warn when both are set, and verify a unit test asserts the synthesised table has one backend serving five families

## 8. Hot reload loop (`cmd/`)

- [x] 8.1 Construct the `*promql.Router` in `main.go` and pass it where `promClient` is passed today (`build.New`, `api.New`), and verify `make build` succeeds and a manual single-backend run serves `/v1/graph`
- [x] 8.2 Implement `reloadBackends(ctx, router, path, interval, logger)` mirroring `reloadAPIKeys`: re-read on the ticker, skip when content is unchanged, parse + validate, swap on success, and verify a unit test with a temp file asserts a swap occurs after the file changes
- [x] 8.3 Implement wholesale rejection: on read/parse/validate failure keep the live table, log at Error, increment the reload counter with the failure result, and verify a unit test asserts routing is unchanged and the counter incremented after writing invalid content
- [x] 8.4 Verify a reload interval of zero starts no goroutine and leaves the startup table serving, by a unit test asserting no swap after a file change
- [x] 8.5 Verify startup fails non-zero with a validation error when the configured file is invalid at boot (fail fast, unlike the reload path)

## 9. Readiness and retention probes

- [x] 9.1 Implement the concurrent multi-backend `/readyz` probe within the existing 1-second budget, with the 503 body naming the backends that did not answer, and verify a component test with two fake backends (one failing) asserts 503 and the name in `reason`
- [x] 9.2 Verify a component test asserts 200 when every backend answers, and that a no-routing-table deployment produces the pre-change body byte for byte
- [x] 9.3 Verify the outside-retention classification is skipped when any probe-serving backend fails — an empty graph is returned as an empty graph, not `outside_retention`

## 10. Compatibility and end-to-end verification

- [x] 10.1 Verify every existing golden file under `internal/api/testdata/golden/` passes unchanged with the router in its implicit single-backend configuration (`go test ./internal/api/ -run Golden`)
- [x] 10.2 Verify `make test` (`-count=1 -race -shuffle=on`), `make vet`, and `make lint` all pass
- [x] 10.3 Add an `internal/integration` test starting **two** VictoriaMetrics containers, ingesting kube-state-metrics fixtures into one and Harvest `volume_labels` fixtures into the other, and verify the assembled graph carries `pvc-to-netapp-aggr` edges joining across them
- [x] 10.4 Add an `internal/integration` test asserting zone routing: two containers holding `zone-a` and `zone-b` series, `?az=zone-a` returning only the `zone-a` pods and an unfiltered request returning both
- [x] 10.5 Add an `internal/integration` test asserting a duplicate service-graph series present in both containers yields the single-backend `data.metrics.rate`, not twice it
- [x] 10.6 Verify `make check-route-containment` still passes and that `pkg/` imports no `internal/*` and no YAML parser

## 11. Documentation

- [x] 11.1 Document the routing file schema, the five families, the zone-selection rule, and a worked two-backend example under `docs/`, and verify the example file parses by pointing a unit test at it
- [x] 11.2 Add a `docs/BREAKING.md` entry recording that this change is **not** breaking, what the implicit single-backend fallback guarantees, and the one operational behaviour change (a single unreachable backend now fails builds)
- [x] 11.3 Update `CLAUDE.md`'s architecture notes with the routing seam (D1 upgrade interface, D4 zone-routable families, D5 merge rule, D6 fail-closed) and verify no stale "single configurable endpoint" claim remains
- [x] 11.4 Run `make docs` and verify `make check-docs` reports no drift
