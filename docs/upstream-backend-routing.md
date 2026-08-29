# Upstream backend routing

`kube-state-graph` assembles one graph from a **set** of Prometheus-compatible
installations. Which installation answers a given query is decided by a routing
table declared in a mounted file, reloadable without a restart.

This exists because two splits are ordinary in production:

- **by availability zone** — metrics are collected per zone and no single
  VictoriaMetrics holds the whole estate;
- **by metric family** — NetApp Harvest series are ingested into a different
  installation from the kube-state-metrics / kubelet / service-graph series.

Without routing, such an estate needs one process per store, which defeats the
point of a cross-cluster graph.

> **Configuring nothing keeps today's behaviour.** With no routing file, an
> implicit backend named `default` at `--prom-url` serves every family, every
> rendered query string is unchanged, and every response body is byte-identical
> to a deployment from before routing existed.

## Configuration

| Setting | Flag | Environment | Default |
|---|---|---|---|
| Routing table path | `--backends-file` | `KSG_BACKENDS_FILE` | *(unset — implicit single backend)* |
| Reload interval | `--backends-reload-interval` | `KSG_BACKENDS_RELOAD_INTERVAL` | `30s` (`0` disables) |

When both `--backends-file` and `--prom-url` are set, the **file wins** and a
`WARN` says so.

An invalid file **at startup** is fatal: there is no previously-good table to
fall back to, and starting on a guess would route queries somewhere the operator
did not ask for. An invalid file **at reload** is rejected wholesale — see
[Hot reload](#hot-reload).

## File format

YAML or JSON; the same fields either way.

```yaml
backends:
  - name: zone-a
    url: http://vmselect-zone-a.monitoring.svc:8481/select/0/prometheus
    families: [ksm, kubelet, servicegraph, probe]
    zones: [zone-a]
    usernameEnv: KSG_PROM_USERNAME_ZONE_A
    passwordEnv: KSG_PROM_PASSWORD_ZONE_A

  - name: zone-b
    url: http://vmselect-zone-b.monitoring.svc:8481/select/0/prometheus
    families: [ksm, kubelet, servicegraph, probe]
    zones: [zone-b]

  - name: netapp
    url: http://vmselect-netapp.monitoring.svc:8481/select/0/prometheus
    families: [harvest]
```

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Unique identity. Appears in logs, in `kube_state_graph_backend_query_failures_total{backend=…}`, on query spans, and in the `/readyz` failure body. It is also the **merge order key** — see [Fan-out and merge](#fan-out-and-merge). |
| `url` | yes | Query endpoint. Must parse as an absolute `http` or `https` URL. |
| `families` | yes | Non-empty subset of the five families below. |
| `zones` | no | The `az` values this backend holds. **Omitted or empty means every zone** — a catch-all. |
| `usernameEnv` / `passwordEnv` | no | **Names** of environment variables holding this backend's basic-auth pair. |

A table is rejected when it is empty, declares a duplicate `name`, declares an
unparseable or non-HTTP(S) `url`, declares an unknown or empty `families` set,
declares an empty zone string, **or leaves any family served by no backend**. An
unknown field is also rejected: a misspelled `zone:` for `zones:` would silently
turn a zone-scoped backend into a catch-all.

## Query families

Every query belongs to exactly one family. The mapping is hardcoded
(`queryFamily` in `pkg/promql/queries.go`) and has no configuration surface.

| Family | Series | Routed by zone? |
|---|---|---|
| `ksm` | every `kube_*` kube-state-metrics series — pod, node, claim, Service, EndpointSlice, owner, controller-annotation | yes |
| `kubelet` | `kubelet_volume_stats_used_bytes`, `kubelet_volume_stats_capacity_bytes` | yes |
| `harvest` | every NetApp Harvest series: `volume_labels`, the six `qos_*` families, the two `qos_policy_fixed_max_throughput_*`, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status` | yes — **without a matcher** |
| `servicegraph` | the three `traces_service_graph_*` series | **no** |
| `probe` | the `up{}` store-health probe | **no** |

`servicegraph` and `probe` carry no request-scoped matcher of any kind, so they
are never narrowed by zone: a `?az=`-scoped request still reaches **every**
backend serving them. Narrowing them would drop edges whose series happen to
live in another zone's store, and the connectivity prune would then delete the
pods on both ends.

`harvest` is the opposite special case: it **is** routed by zone, but the query
string sent to the selected backend is the unfiltered one — no `az` and no
`env` matcher (the `qos_*` families keep their fixed `lun=""`). A per-zone
Harvest store already holds only its own zone's series, so the store boundary is
the zone filter, and the Harvest series therefore need **not** carry the
configured `az` / `env` labels at all. Two consequences: `?env=` has no effect
on the Harvest legs, and a catch-all `harvest` backend (no `zones`) under
`?az=` returns every zone's series, narrowed only by reference through the
loaded claims — both exactly what an unfiltered build already does.

## Backend selection

For a query of family `F` under a request whose `az` parameter carries the value
set `A`:

1. Candidates are the backends whose `families` contains `F`.
2. If `F` is zone-routed, keep the candidates whose `zones` is empty (catch-all)
   **or** intersects `A`. An empty `A` keeps every candidate.
3. If `F` is not zone-routed, `zones` is ignored entirely.

Routing composes **with** the PromQL matchers, never instead of them: an `az`
value that selects a backend is still rendered as a label matcher on every query
that accepts it. Routing narrows *which store is asked*; the matcher narrows
*what that store returns*. `env`, `cluster` and `namespace` play no part in
backend selection. The one family that routes without a matcher is `harvest`
(above): for it, backend selection is the *only* effect `az` has.

A requested zone that no backend declares yields an **empty result, not an
error** — an empty filtered result is a legitimate empty graph. A `WARN` names
the family and the unmatched zone values, so a typo is distinguishable from an
estate that genuinely holds nothing.

## Fan-out and merge

A query issued to several backends is merged by concatenating each backend's
series in **ascending `name` order** and dropping any series whose label set was
already contributed. The surviving copy is the lexically-smallest backend's.

De-duplication is a correctness requirement, not tidiness: several readers *sum*
across contributing series — the service-graph request and failure totals most
visibly — so a series held by two backends would multiply an edge's `rate` and
`error_rate` by the number of backends holding it. A catch-all backend sitting
alongside a per-zone one makes that overlap ordinary.

A duplicate carrying a *different* value is logged at `DEBUG` and counted; the
first contributor still wins, so the response stays deterministic.

## Failure handling

**A backend error fails the query it was issued for**, and the error names the
backend. A partial fan-out result is indistinguishable from a smaller estate:
missing pods lose their edges, the connectivity prune then removes their nodes,
claims and aggregates, and the response is a plausible, smaller, **wrong** graph.

Legs the builder already treats as optional (the Harvest and kubelet families,
`kube_replicaset_annotations`, `kube_job_annotations`) keep degrading exactly as
they do for any upstream error: that leg is dropped, the build continues.

`/readyz` probes **every** backend concurrently within the one `--api-timeout`
budget and returns 503 unless all of them answer. Its body names the backends
that did not — names only, never a URL, host, or IP.

## Credentials

The routing file is a ConfigMap and **must never hold a secret**. A backend
names the environment variables holding its pair; a file carrying a literal
`username` or `password` field is rejected.

- Both `usernameEnv` and `passwordEnv` must be set together, or both omitted.
- A named variable that is unset or empty is a **load failure**, not a quiet
  fallback: a typo'd variable name would otherwise become 401s from one store,
  which — since a backend failure fails the whole query — points at the wrong
  thing.
- A backend naming neither variable falls back to the global
  `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD` pair.
- Credentials are attached only to requests addressed to that backend's own
  host, so a cross-host redirect carries no `Authorization` header.

Rotating a credential **value** requires a restart; only the variable **names**
are reloadable.

## Hot reload

The file is re-read on `--backends-reload-interval`. When its content changed it
is parsed, validated, and the live table is replaced **atomically**. A build in
flight keeps the table it started with, so a reload never changes which backends
a single request reaches part-way through.

A file that fails to read, parse, or validate is rejected **wholesale**: the
previous table keeps serving, an `ERROR` names the reason, and
`kube_state_graph_backend_config_reload_total{result="error"}` increments on
every tick until the file is fixed. Applying the valid subset of a broken file
would silently route a family to fewer stores — a partial graph with no error.

Polling rather than `fsnotify`: a Kubernetes ConfigMap update replaces the
`..data` symlink rather than writing the file, so a watch has to be on the
directory with a subtle event shape — and one interval of latency is irrelevant
for a topology change.

Clients are keyed by `(url, username, password)` and reused across a reload, so
a table edit that only added a zone does not churn every connection pool. A
backend the new table no longer declares has its idle connections released.

## Observability

| Metric | Type | Meaning |
|---|---|---|
| `kube_state_graph_upstream_backends` | gauge | Backends in the live routing table |
| `kube_state_graph_backend_config_reload_total{result}` | counter | Reload attempts by `ok` / `error` / `unchanged` |
| `kube_state_graph_backend_query_failures_total{backend}` | counter | Upstream query failures per backend |

`kube_state_graph_upstream_query_duration_seconds` and
`kube_state_graph_upstream_query_failures_total` keep their existing
`query`-only label sets — adding a `backend` label to an established
self-metric would break every dashboard and recording rule built on it.

Every upstream query span (`prometheus.query`) carries
`kube_state_graph.backend` when routing is configured; the attribute is omitted
entirely in a single-upstream deployment.

## Embedding the engine

The graph engine is importable (`pkg/kubegraph`, `pkg/build`, `pkg/promql`), and
routing is configured through the **same code this binary runs** — an external
module cannot import `internal/`, so nothing here asks you to re-derive the
schema, the validation, or the credential rules.

```go
import (
    "github.com/akira-core/kube-state-graph/pkg/kubegraph"
    "github.com/akira-core/kube-state-graph/pkg/promql"
    "github.com/akira-core/kube-state-graph/pkg/promql/backendsfile"
)

// The operator's file — identical schema, identical errors. A nil lookup reads
// the process environment for the credential variables the file names.
table, err := backendsfile.Read("/etc/ksg/backends.yaml", nil)
if err != nil {
    return err // an invalid file at startup is fatal: there is no table to fall back to
}

router, err := promql.NewRouter(table, nil, nil) // nil metrics, default client factory
if err != nil {
    return err
}

engine := kubegraph.NewRouted(router, kubegraph.Options{APITimeout: 30 * time.Second})

// `az` selects the backend per request, exactly as ?az= does over HTTP.
g, err := engine.Build(ctx, window, end, promql.Selector{AZ: []string{"zone-a"}})
```

Three ways to obtain the table, all producing the same validated value:

| Source | Call |
|---|---|
| The operator's mounted file | `backendsfile.Read(path, lookup)` |
| Bytes you already hold (a ConfigMap read through your own client) | `backendsfile.Parse(data, lookup)` |
| A table assembled in code | `promql.NewBackend(…)` + `promql.NewTable(…)` |
| A single upstream endpoint | `promql.SingleBackendTable(url, user, pass)` |

Hot reload is the same loop the binary arms, so the digest short-circuit, the
wholesale rejection and the atomic swap behave identically:

```go
backendsfile.Start(ctx, router, backendsfile.ReloaderOptions{
    Path:    "/etc/ksg/backends.yaml",
    Lookup:  nil, // process environment
    Logger:  nil, // nil = silent
    Metrics: nil, // nil = records nothing
}, 30*time.Second)
```

`Logger` and `Metrics` are optional: leaving them nil means an embedder inherits
neither kube-state-graph's log format nor its `kube_state_graph_*` series. Pass
`backendsfile.NewReloader(...)` instead of `Start` when you want to drive
`Once()` yourself.

Two containment rules hold, and CI enforces the second
(`make check-parser-containment`):

- `pkg/` imports no `internal/*`, so the engine is importable at all.
- `pkg/promql` itself reaches **no YAML parser and no file I/O** — the parser
  lives one package down, in `pkg/promql/backendsfile`. A module that builds its
  table in code imports `pkg/promql` alone and inherits neither.

Credential **values** never come from the file: a backend names the environment
variables holding its pair, and the same rules apply here as to the binary — a
half-declared pair is rejected, and a named-but-unset variable is a load failure
rather than a quiet fallback.

## Rollout

1. Ship with no routing file. Verify `kube_state_graph_upstream_backends` is 1.
2. Mount a **single**-entry file mirroring `--prom-url` (all five families, no
   zones). This exercises the file path, validation and reload loop with no
   change in routing outcome.
3. Split `harvest` onto its own backend. Verify the storage chain still draws:
   `pvc-to-netapp-aggr` edges present, aggregate I/O populated.
4. Add per-zone backends and their credential variables. Verify a `?az=`-scoped
   request and an unfiltered request agree on the scoped zone's node set.

Rollback at any step is removing `--backends-file` or reverting the ConfigMap
and waiting one interval — neither needs a redeploy.
