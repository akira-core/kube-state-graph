## Why

Every one of the ~40 PromQL queries a build issues goes to the single `--prom-url`
endpoint. Two operational realities break that assumption: metrics are collected
into **per-availability-zone VictoriaMetrics installations** (there is no single
VM that holds every zone), and **NetApp Harvest series are ingested into a
different VM installation** from the kube-state-metrics / kubelet / service-graph
series. Today the only way to serve such an estate is one kube-state-graph
process per backend, which defeats the whole point of a cross-cluster graph.

Backend membership also changes at a different cadence from the binary — zones
are added, a filer's VM is moved — so the routing table must be reloadable
**without a restart**, the same way `--api-keys-file` already reloads a rotated
Secret.

## What Changes

- **New: a routing table between the builder and the upstream.** A mounted
  ConfigMap declares N named backends, each with a URL, the set of `az` values it
  holds, and the set of **query families** it serves. Every upstream call is
  dispatched through it instead of going to one fixed client.
- **New: query-family classification.** A hardcoded `Query → Family` table
  (`ksm`, `kubelet`, `harvest`, `servicegraph`, `probe`) in `pkg/promql`,
  alongside the existing `queryDims` table and guarded by the same
  "every Query constant must be listed" test. This is what lets NetApp Harvest
  series be served by a different installation from everything else.
- **New: fan-out and merge.** A request carrying no `?az=` (or `az` values
  spanning several backends) issues the query to **every** backend that serves
  the family and covers the requested zones, then merges the returned vectors.
  Merge order is a pure function of the sorted backend names, so the response
  stays byte-deterministic. `az` continues to be pushed down as a PromQL matcher
  as well — routing narrows *which store* is asked, the matcher narrows *what it
  returns*.
- **New: hot reload by polling.** A background goroutine re-reads the ConfigMap
  file on an interval, validates it, and atomically swaps the live table. A file
  that fails to parse or validate is **rejected wholesale** — the previous table
  keeps serving and the failure is logged and counted.
- **Harvest backends are `az`-routed too**, using the same zone table as the
  kube-state-metrics families; they simply resolve to different backend entries.
  **Routing is the ONLY effect `az` / `env` have on the Harvest legs**: the
  thirteen Harvest queries are issued as their bare, unfiltered strings (the
  `qos_*` families keeping `lun=""`) to the zone-selected backends. A per-zone
  Harvest store already holds only its zone, so the matcher was redundant there,
  and dropping it removes the requirement that the Harvest pipeline stamp the
  configured `az` / `env` labels at all. `env` has no routing dimension and so no
  longer touches Harvest. A `volume_name` shared across zones or environments
  resolves by reference through the loaded claims, exactly as an unfiltered build
  already does.
- **Per-backend credentials stay out of the ConfigMap.** A backend names the
  environment variables holding its basic-auth pair; the values never appear in
  the routing file. The existing global `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD`
  pair remains the fallback.
- **`/readyz` and the outside-retention `up{}` probe become multi-backend.**
  Both probe every configured backend; a single unreachable backend makes the
  server not-ready and suppresses the outside-retention classification.
- **Not breaking.** With no routing file configured, `--prom-url` synthesises a
  single implicit backend serving every family and every zone, and every rendered
  query, response body, and golden file is byte-identical to today.

## Capabilities

### New Capabilities
- `upstream-backend-routing`: the ConfigMap-declared multi-backend routing table —
  its schema and validation, the `Query → Family` classification, `az`-based
  backend selection, fan-out and deterministic merge, partial-failure semantics,
  hot reload with atomic swap and rejected-file fallback, per-backend credential
  sourcing, and the single-backend compatibility mode.

### Modified Capabilities
- `cluster-topology-source`: "Centralised VictoriaMetrics as the only topology
  source" no longer means a *single endpoint* — it becomes "one or more
  Prometheus-compatible endpoints selected by the routing table", with the
  no-Kubernetes-API rule untouched. "Optional basic-auth credentials for the
  upstream endpoint" gains per-backend credential resolution.
- `netapp-storage-graph`: the Harvest legs are declared as their own query family
  and MAY be served by a different backend set from the kube-state-metrics legs;
  the join, the hop split, and the per-hop degradation are unchanged.
- `graph-api`: `/readyz` probes all backends rather than one; new self-metrics for
  routing-table state and per-backend query outcomes.

## Impact

- **Code**: `pkg/promql` (new routing/family/merge code, `Querier` gains a
  selector-aware dispatch seam), `pkg/build` (Builder resolves a per-request
  querier from the routing table), `internal/config` (new file path + reload
  interval flags/env), `cmd/kube-state-graph` (construct the table, start the
  reload loop, close retired clients), `internal/api` (multi-backend `/readyz`),
  `internal/observability` (new metrics).
- **API surface**: no new request parameter, no response-body change, no new
  node or edge type. `?az=` keeps its matcher meaning for kube-state-metrics and
  kubelet and gains backend selection; for the Harvest legs it becomes
  backend selection **only** (the `az` matcher is withdrawn from them) and
  `?env=` stops reaching them. A `?az=` request against a catch-all Harvest
  backend, or any `?env=` request, therefore joins the loaded claims against the
  whole Harvest estate rather than one zone's or environment's slice.
- **Operators**: the Harvest series no longer need the configured `az` / `env`
  labels; a deployment that stamps them keeps working unchanged. The
  `kube-state-graph-demo` repository's three-stamper invariant reduces to two
  (vmagent, collector) — a separate repository, tracked separately.
- **Configuration**: new `--backends-file` / `KSG_BACKENDS_FILE` and
  `--backends-reload-interval`; `--prom-url` retained as the single-backend
  fallback.
- **Dependencies**: none added — the file is parsed with the standard library and
  polled on a ticker, deliberately avoiding an fsnotify dependency.
- **Operational**: an unreachable backend now fails a build that would previously
  have succeeded against a smaller estate; `/readyz` surfaces it. Query fan-out
  multiplies the per-build upstream call count by the number of matched backends.
