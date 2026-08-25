## Context

See proposal.md — Why. The relevant current state:

- `cmd/kube-state-graph` builds exactly one `*promql.Client` from `cfg.PromURL` and hands the same value to `build.New` (as the `promql.Querier`), to `api.New` (for the `/readyz` probe), and — for an embedder — to `kubegraph.New`.
- Every upstream call in the codebase goes through one method,
  `Querier.Instant(ctx, name, query, ts)`, and at **every** call site `name` is
  `string(promql.QSomething)`. The query identity is therefore already present at
  the dispatch point; the request's `promql.Selector` is not.
- `pkg/promql` already owns a hardcoded "which request dimension reaches which
  series" table (`queryDims`) guarded by a test that parses the file's `Query`
  constants and fails on a missing entry. A family table is the same shape.
- `pkg/` must not import `internal/*`, must not construct a Kubernetes client,
  and `pkg/build` must not import `pkg/route` (`make check-route-containment`).
  The graph engine is embedded by `graph-api-gateway`, so every exported
  signature in `pkg/promql`, `pkg/build`, and `pkg/kubegraph` is a compatibility
  surface.
- `--api-keys-file` + `--api-keys-reload-interval` is the existing hot-reload
  precedent: a background goroutine re-reads a mounted file on a ticker and
  swaps a live value.

## Goals / Non-Goals

**Goals:**

- One process serves an estate whose series live in several VictoriaMetrics
  installations, split by availability zone and by metric family.
- Adding, moving, or removing a backend is a ConfigMap edit, not a restart.
- A deployment that configures nothing new is byte-identical to today, golden
  files included.
- No exported signature in `pkg/promql`, `pkg/build`, or `pkg/kubegraph` changes,
  so `graph-api-gateway` keeps compiling untouched.

**Non-Goals:**

- Cross-backend query federation or push-down of joins. Joins stay in Go over the
  merged result, exactly as they are today.
- Per-backend query rewriting. Every backend receives the identical query string.
- Result caching, deduplicated fan-out across concurrent requests, or a
  circuit breaker. v1 has no cache and this change does not add one.
- Routing on `env`, `cluster`, or `namespace`. Only `az` selects a backend.
- Reloading credential **values**. Only variable names are reloadable.

## Decisions

### D1 — Routing is an optional upgrade interface on `promql.Querier`, not a widened one

`Querier.Instant` carries the query name but not the `Selector`, and the selector
is what picks a backend. Three ways to bridge that were considered:

1. Widen `Querier` with a selector parameter — breaks every mock, every test
   helper, and the embedder.
2. Smuggle the `Selector` through `context.Value` — invisible coupling, and a
   build that forgets to attach it silently fans out to everything.
3. **Chosen:** add a second, optional interface that yields a `Querier` already
   bound to a selector:

   ```go
   // pkg/promql
   type QuerierSource interface{ QuerierFor(sel Selector) Querier }
   ```

`*promql.Router` implements **both** `Querier` and `QuerierSource`. `build.New`
type-asserts its `promql.Querier` argument for `QuerierSource` and uses the bound
querier when the assertion succeeds. This is precisely the pattern the route
change already established with `build.BuildScopedRouteResolver`
(`RouteResolver` + `BuildScoped()`), so it is a known shape in this codebase
rather than a new one.

Consequences: `build.New`, `api.New`, `kubegraph.New` keep their signatures; a
mock `Querier` does not satisfy `QuerierSource` and therefore behaves exactly as
today; `cmd/` passes the `*Router` where it passed the `*Client`.

### D2 — `Builder.Build` resolves the querier once, and uses it for every leg

`Build` calls `QuerierFor(sel)` exactly once, at the top, and threads the result
through `ReadTopology`, `ReadServiceGraph`, and the retention `up{}` probe. The
returned querier closes over **one immutable table snapshot**, which is what makes
the "a reload does not disturb an in-flight build" requirement structural rather
than best-effort.

Routing the retention probe through the same bound querier is deliberate: the
`probe` family ignores zones (D4), so the probe still reaches every
probe-serving backend, and the build cannot end up asking a different set of
stores than it read from.

### D3 — Parsing lives in `internal/config`; `pkg/promql` receives a validated table

`pkg/promql` stays free of file I/O and of any parser dependency. It exports an
immutable, already-validated `Table` value plus its constructor/validator;
`internal/config` reads the mounted file and produces one. `cmd/` owns the
ticker, the swap, and the client lifecycle.

The file is parsed with `sigs.k8s.io/yaml`, which converts YAML to JSON and then
uses `encoding/json` — so one struct with json tags accepts **both** forms, which
is what operators writing a ConfigMap actually want. It is already in the module
graph as an indirect dependency (via `istio.io/istio`), so promoting it to direct
adds **no new module** to the build. It is imported from `internal/config` only,
never from `pkg/`, so an embedder inherits nothing.

Alternative considered: JSON-only via `encoding/json` for a zero-dependency
parse. Rejected — a JSON-only routing file inside a YAML ConfigMap is a papercut
operators would hit on day one, and the dependency is already resolved.

### D4 — Zone routing applies only to families whose queries accept `az`

`servicegraph` and `probe` are `dimsNone` in `queryDims`: they take no
request-scoped matcher at all, because the service-graph `cluster` label is the
unreliable trace-source cluster and its namespace labels describe only the
caller's own view. Narrowing them by zone at the *routing* layer would reintroduce
exactly the loss the matcher layer deliberately avoids — a `?az=zone-a` request
would drop every edge whose series happens to live in the `zone-b` store, and the
connectivity prune would then delete the pods on both ends.

So backend selection reads the same fact from the same place: a family is
zone-routable iff its queries accept `dimAZ`. `zones` on a backend serving only
non-`az` families is inert, and validation does not object to it — a backend
commonly serves both kinds.

### D5 — Merge de-duplicates by label-set identity, and the rule is mandatory

Concatenation alone is wrong. Several readers **sum** across contributing series —
the service-graph request/failure totals most visibly — so a series present in two
backends would multiply an edge's `rate` and `error_rate` by two. A shared
catch-all backend alongside a zone backend makes that overlap ordinary, not
exotic.

The rule: iterate backends in ascending `name` order, keep the first sample for
each distinct label set, drop later duplicates. It is order-free (a total order on
names, not on response arrival), it needs no cross-backend coordination, and it
preserves the existing determinism contract without touching the serialiser.

A duplicate carrying a *different* value is a genuine data ambiguity — two stores
disagreeing about the same series. Failing the build on it would make a benign
scrape overlap an outage; picking the larger or newer value would be an
undocumented merge policy. Logging at Debug and counting it, while keeping the
lexically-smallest backend's copy, keeps the outcome deterministic and the
disagreement visible.

### D6 — Required legs fail closed on a backend error

A partial fan-out result is indistinguishable from a smaller estate. Missing pods
remove their edges; the connectivity prune then removes their nodes, claims, and
aggregates; the response is a plausible, smaller, wrong graph — the exact failure
mode this repository's "invariants that fail silently" list exists to prevent.

So a backend error fails the query, and the error names the backend. Legs that are
already `fetchOptional` keep degrading, because their contract is already
"absence is a documented, subtractive degrade".

Alternative considered: a `partialFailure: degrade` knob. Rejected — it adds a
configuration surface whose wrong setting is invisible, and the operator-visible
signal (`/readyz` naming the backend) already exists.

### D7 — Reload is a ticker plus an atomic pointer swap

Matching `--api-keys-file`: a goroutine bounded by the app context re-reads the
file, and swaps only when the parsed content differs. The live table is held in an
`atomic.Pointer`, so a reader takes one snapshot with no lock and never observes a
half-applied table.

fsnotify was rejected for two reasons: a Kubernetes ConfigMap update replaces the
`..data` symlink rather than writing the file, so the watch has to be on the
directory and the event shape is subtle; and it is a new direct dependency in a
repository whose convention is that new dependencies need justification. Polling
is strictly simpler and the latency (one interval) is irrelevant for a topology
change.

A rejected file is rejected wholesale. Applying the valid subset of a broken file
would silently route a family to fewer stores — a partial graph with no error,
which is D6's failure mode arriving through the config path instead.

### D8 — Backends name credential variables; the loader resolves and validates them

The routing file is a ConfigMap and must never hold a secret. A backend therefore
carries `usernameEnv` / `passwordEnv` — **names**, resolved from the process
environment at load time — falling back to the global
`KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD` pair.

A named-but-unset variable is a load failure rather than a silent fallback to
unauthenticated. The alternative — fall back quietly — turns a typo'd variable
name into 401s from one store, which under D6 fails the build with an error that
points at the wrong thing.

Values are held only on the per-backend transport, which already scopes the
`Authorization` header to the backend's own host so a cross-host redirect carries
nothing (the existing `basicAuthTransport` behaviour, now instantiated per
backend).

### D9 — Compatibility is an implicit table, not a code path

With no `--backends-file`, `cmd/` synthesises a one-entry table: name `default`,
URL `--prom-url`, all five families, no zones. There is then exactly one candidate
for every query, the fan-out loop runs once, and the merge de-duplicates nothing.

Keeping a single code path (rather than "if unrouted, use the old client")
guarantees the compatibility claim is actually exercised by the existing test
suite: every current unit, component, golden, and integration test runs through
the router in its degenerate configuration.

### D10 — New self-metrics; the existing two keep their labels

`kube_state_graph_upstream_query_duration_seconds` and
`kube_state_graph_upstream_query_failures_total` are stable contracts — adding a
`backend` label to either would break every dashboard and recording rule built on
them. Per-backend detail goes on new metrics instead
(`kube_state_graph_upstream_backends`, `kube_state_graph_backend_config_reload_total`,
`kube_state_graph_backend_query_failures_total`).

`promql.Metrics` is an exported interface an embedder may implement, so it is not
widened either: the router type-asserts for an optional `RouterMetrics` upgrade
interface and no-ops when absent — the same optional-upgrade shape as D1.

The backend name goes on the existing `prometheus.query` client span as an
attribute, which is additive and needs no new span.

### D11 — Fan-out concurrency is bounded per query, not globally

`ReadTopology` already runs ~37 legs in one errgroup with no global cap; each leg
now issues up to N calls. The fan-out uses its own `errgroup` with
`SetLimit(len(selected))`, which is naturally small — zone restriction usually
resolves to one or two backends per family — and the build deadline plus
`--api-timeout` remain the real ceiling. No new concurrency knob is introduced;
adding one would be a second load-shedding mechanism alongside the HPA and pod
resource limits the architecture already delegates to.

### D12 — Clients are keyed by identity and reused across reloads

A backend's HTTP client is keyed by `(url, resolved username, resolved password)`.
On reload, unchanged keys are carried over so connection pools survive a table
edit that only added a zone; clients whose key disappeared have
`CloseIdleConnections()` called and are dropped. Without this, every reload would
churn every pool.

### D13 — Readiness probes all backends concurrently within one budget

`/readyz` keeps its 1-second budget; the probes run in one errgroup under a single
`context.WithTimeout`, so readiness latency is that of the slowest backend rather
than the sum. The failure body names the backends that did not answer, because
"upstream unreachable" is not actionable when there are six of them.

## Risks / Trade-offs

- **A single unreachable backend now fails builds that a one-backend deployment
  would have served.** → Deliberate (D6): a partial graph is worse than an error.
  Mitigated by `/readyz` naming the backend so the failing store is identified in
  one look, and by the per-backend failure counter.
- **Upstream call volume multiplies by the number of matched backends.** → Zone
  restriction keeps the multiplier at one for the common `?az=`-scoped request;
  the unfiltered request is the expensive one, which was already true. D11 bounds
  each leg; the build timeout bounds the whole.
- **A zone typo (`az=zoen-a`) silently returns an empty graph.** → Specified as a
  Warn naming the family and the unmatched zone values, so the log distinguishes
  it from a genuinely empty estate. Making it an error was rejected: an empty
  filtered result is already a documented 200.
- **Two backends disagreeing about one series resolves silently to one of them.**
  → Deterministic (lexically-smallest backend), counted, and logged at Debug.
  Escalating it would turn a benign scrape overlap into an outage.
- **`servicegraph` fanning out to every backend is the most expensive leg
  multiplied.** → Inherent to D4's correctness argument. An operator who ingests
  service-graph series into only one installation declares only that backend as
  serving the family, which reduces the multiplier to one without any code
  support.
- **The reload goroutine reads a file on every tick for the process lifetime.** →
  Same cost profile as the existing API-key reloader; the swap only happens when
  content differs.
- **Widened blast radius of a bad ConfigMap.** → Wholesale rejection plus
  keep-the-previous-table means a broken edit degrades to "stale but correct
  routing" rather than to a partial estate.

## Migration Plan

1. Ship with no routing file configured. `--prom-url` synthesises the implicit
   `default` backend (D9); behaviour, metrics, and response bodies are unchanged.
   Verify `kube_state_graph_upstream_backends` reads 1.
2. Mount the ConfigMap and set `--backends-file` with a **single** backend
   entry that mirrors the current `--prom-url` (all five families, no zones).
   This exercises the file path, validation, and reload loop with no change in
   routing outcome; response bodies stay identical.
3. Split the `harvest` family onto its own backend. Verify the storage chain still
   draws (`pvc-to-netapp-aggr` edges present, aggregate I/O populated) — this is
   the first configuration where a single graph is assembled from two stores.
4. Add per-zone backends and their credential variables. Verify a `?az=`-scoped
   request and an unfiltered request return the same node set for the scoped
   zone.
5. Rollback at any step is removing `--backends-file` (falls back to
   `--prom-url`) or reverting the ConfigMap and waiting one reload interval —
   neither needs a redeploy of the binary.
