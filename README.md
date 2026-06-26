# kube-state-graph

Traditional Chinese: [README.zh-tw.md](README.zh-tw.md).

A Go REST API server that returns a unified pod / node / PVC graph for one or
more Kubernetes clusters, including pod-UID-resolved RPC edges that may cross
cluster boundaries.

```
cluster A: kube-state-metrics ──┐
           service-graph source ┤
                                 │  (vmagent / Prometheus
cluster B: kube-state-metrics ──┤   with external_labels:
           service-graph source ┤   { cluster: "<name>" })
                                 │
       ...                       ├──► centralised VictoriaMetrics ◄── kube-state-graph
                                 │                                     (Prometheus HTTP API)
cluster N: kube-state-metrics ──┤
           service-graph source ─┘
```

## What it does

- Reads `kube_*` topology and `traces_service_graph_*` runtime metrics from a
  single centralised VictoriaMetrics, on demand for a caller-specified
  `[start, end]` time range.
- Joins them into a multi-cluster graph keyed by cluster-scoped pod UIDs and
  node names.
- Returns the graph as Cytoscape.js JSON (`/v1/graph`).
- Exposes cluster discovery (`/v1/clusters`) and a static edge-type catalogue
  (`/v1/edge-types`).
- Builds the graph on every request — v1 ships **no in-process result cache**,
  **no singleflight**, and **no HTTP cache validators** (`ETag` /
  `If-None-Match` / `304`). A horizontally scalable cache mechanism for
  distributed deployment is anticipated as a future change. Caller-supplied
  `start` / `end` accept RFC 3339 or Unix seconds; the server enforces only
  `end > start`, then passes the window through to upstream PromQL verbatim —
  no server-side bucketing, alignment, max-window cap, or future-time guard.
  Bounded query cost is delegated to VictoriaMetrics search limits
  (`-search.maxQueryDuration`, `-search.maxPointsPerTimeseries`,
  `-search.maxSamplesPerQuery`). The serialiser produces a deterministic body
  (`apiVersion`, `clusters`, `elements` only — no echoed time fields). Pod,
  node, and service IPs appear on the top-level `ipaddress` attribute, not in
  `labels`. Pods additionally carry typed `data` attributes — `owner`
  (`{kind, name}`), `application` (the ArgoCD Application), and `containers`
  (`[{name, image}]`) — all `omitempty` and never inside `labels`.

## Quick start

```bash
make build
./bin/kube-state-graph \
  --prom-url=http://victoria-metrics.example:8428 \
  --listen-addr=:8080
```

Then:

```bash
curl 'http://localhost:8080/v1/clusters'
curl 'http://localhost:8080/v1/graph?start=$(date -u -d "-5 min" +%s)&end=$(date -u +%s)' | jq '.elements'
```

When the server is started with API keys configured (`--api-keys-file` or
`--api-keys`), every `/v1/*` request must carry an `X-API-Key: <key>` header:

```bash
curl -H 'X-API-Key: my-secret-key' 'http://localhost:8080/v1/clusters'
```

Health probes (`/livez`, `/readyz`), `/metrics`, and the docs routes
(`/openapi.*`, `/docs`) are exempt and require no key. With no keys configured
the middleware is a no-op and every route is open.

## Upstream metrics consumed

The graph build issues these PromQL queries against centralised VictoriaMetrics
on every request (v1 has no result cache). Every series is expected to carry a
`cluster` external label (injected by `vmagent` / Prometheus `external_labels`
per source cluster).

### Topology metrics — produced by [`kube-state-metrics`](https://github.com/kubernetes/kube-state-metrics)

| Metric | Used for | Labels read | Required? |
|---|---|---|---|
| `kube_pod_info` | Pod nodes (`node` label drives the `pod-to-node` edge; pods nest under the `cluster > namespace > application > controller > pod` workload hierarchy) | `cluster`, `namespace`, `pod`, `uid`, `node`, `pod_ip` (→ `data.ipaddress`; `host_ip` not exported) | **Yes** |
| `kube_node_info` | K8sNode nodes | `cluster`, `node` | **Yes** |
| `kube_node_status_addresses{type="ExternalIP"}` | Node external IP (→ `data.ipaddress`) | `cluster`, `node`, `address` | Optional |
| `kube_node_status_condition{condition="Ready"}` | Node Ready status `data.ready_status` ∈ {`Ready`, `NotReady`, `Unknown`} from the active (`status` value 1) row; omitted when no Ready data — distinct from `Unknown` (kubelet lost contact) | `cluster`, `node`, `condition`, `status` | Optional (absent ⇒ no `data.ready_status`); a KSM default |
| `kube_node_labels` | Node label propagation (`kubernetes.io/*` etc.) | `cluster`, `node`, `label_*` | Optional |
| `kube_pod_spec_volumes_persistentvolumeclaims_info` | PVC nodes; pod-mounts-pvc edges | `cluster`, `namespace`, `pod`, `persistentvolumeclaim`, `volume` | Optional (no PVCs ⇒ no PVC nodes/edges) |
| `kube_persistentvolumeclaim_info` | PVC StorageClass → `pvc-to-storageclass` edge to the real `type="storageclass"` node (never a PVC `data` attribute or label) | `cluster`, `namespace`, `persistentvolumeclaim`, `storageclass` | Optional (absent ⇒ no StorageClass edge; PVC nests under its namespace group) |
| `kube_storageclass_info` | Real `type="storageclass"` nodes: `data.provisioner` (native label) + `data.parameters` object (`pool`←`storagePools`\|`pool`, `fs`←`fsType`\|`fsName`, `cluster_id`←`ClusterID`, `selector`) | `cluster`, `storageclass`, `provisioner`, `storagePools`, `pool`, `fsType`, `fsName`, `ClusterID`, `selector` | Optional (absent ⇒ PVC-referenced classes materialise bare). Parameter labels are operator-provided (`--metric-labels-allowlist`) |
| `kube_pod_owner` | Pod controller-owner attribute `data.owner` = `{kind, name}` (ReplicaSet skipped to its Deployment; omitted when no controller owner); also the pod ArgoCD Application `data.application` (segment before the first `:` of the `argocd_tracking_id` label). The owner and Application additionally drive the `application` / `controller` compound groups in the workload hierarchy | `cluster`, `namespace`, `pod`, `owner_kind`, `owner_name`, `owner_is_controller`, `argocd_tracking_id` | Optional (absent ⇒ no `data.owner`). `argocd_tracking_id` is **operator-provided** (e.g. `--metric-labels-allowlist` / relabel), NOT a KSM default; absent ⇒ no `data.application` |
| `kube_replicaset_owner` | Resolves a ReplicaSet pod-owner up to its owning Deployment | `cluster`, `namespace`, `replicaset`, `owner_kind`, `owner_name` | Optional (absent ⇒ ReplicaSet kept as owner) |
| `kube_pod_container_info` | Pod container list `data.containers` = `[{name, image}]`, sorted by `(name, image)`; on a mid-window image change the latest-seen image wins per container | `cluster`, `namespace`, `pod`, `container`, `image` | Optional (absent ⇒ no `data.containers`); a KSM default |
| `kube_service_info` | Service nodes for `://` connection-string resolution (D29); `cluster_ip` (headless `None` ⇒ no `data.ipaddress`) | `cluster`, `namespace`, `service`, `cluster_ip` | Optional (absent ⇒ `://` endpoints fall back to `external`) |
| `kube_endpointslice_endpoints` | Service → backing-pod fan-out (`service-selects-pod` edges) | `cluster`, `namespace`, `endpointslice`, `targetref_kind`, `targetref_namespace`, `targetref_name` | Optional |
| `kube_endpointslice_labels` | Joins an EndpointSlice to its owning Service | `cluster`, `namespace`, `endpointslice`, `label_kubernetes_io_service_name` | Optional — **requires** `--metric-labels-allowlist=endpointslices=[kubernetes.io/service-name]` (NOT a KSM default); absent ⇒ no `service-selects-pod` resolution |

Each is wrapped in `last_over_time(<metric>[<window>]) @ <end>` so the result
reflects the most recent value within the requested `[start, end]` window — except
`kube_pod_container_info`, which uses `tlast_over_time(...)` so each per-image
series carries its last-sample timestamp, letting the reader pick the latest image
per container (a recency pick that is accurate for near-now windows; see
`design.md` D-A4 for the far-past-window caveat).

### Service-graph metric — produced by [Tempo](https://grafana.com/docs/tempo/latest/metrics-generator/service_graphs/) or compatible generator

| Metric | Used for | Labels read | Required? |
|---|---|---|---|
| `traces_service_graph_request_total` | `pod-calls-pod` (intra/cross-cluster), `pod-calls-service` (intra-cluster), `service-selects-pod` (may cross-cluster) edges | `cluster`, `client`, `server`, `client_k8s_pod_uid`, `server_k8s_pod_uid` | Optional (no series ⇒ no call edges) |

Wrapped in `rate(traces_service_graph_request_total[<window>]) @ <end>`. Each
series carries a single `cluster` external label representing the trace source
(typically the cluster running Tempo's metrics-generator); this is the
**client-side** cluster of the call. The **server-side** cluster is recovered
at build time by joining `server_k8s_pod_uid` against the global topology
pod-UID index — Kubernetes pod UIDs are unique across clusters in practice,
so the lookup is unambiguous. Edges are only emitted when both endpoints
resolve. When an endpoint's pod-UID label is empty, the human-readable
`client`/`server` label is resolved by built-in **connection-string detection**
(no knob): a label containing the literal `://` is parsed as a URL — an
in-cluster `<service>.<namespace>.svc` name becomes a **single** `type="service"`
node **in the caller's own cluster** (so `pod-calls-service` is always
intra-cluster), provided that cluster holds the same-named Service. That service
node then fans out on-demand `service-selects-pod` edges to its backing pods
across **every same-family cluster** holding the same-named Service — so
`service-selects-pod` **may cross clusters**, modelling multi-cluster
service-mesh endpoint aggregation (clusters are one family when their names
match after collapsing digit runs, e.g. `prod-1` ↔ `prod-2`). A headless
`<pod>.<service>.<namespace>.svc` name resolves to the **same** service node (the
leading pod-hostname is dropped) — a `://` endpoint is never a specific pod. An
unresolvable URL, or one whose caller cluster does not hold the Service, becomes
an `external` node. A non-URL label (no `://`) also becomes an `external` node
via the missing pod-UID human-label fallback.

The `servicegraph` connector's **virtual peers** — `client="user"` (an
uninstrumented caller) and `unknown` (an unresolved peer) — are dropped at the
query layer (`client!~"user|unknown",server!~"user|unknown"`) and never appear
as nodes or edges. The match is exact and case-sensitive, so a `://` host that
merely *contains* `user` is unaffected.

### Probes — diagnostics, not graph data

| PromQL | Purpose |
|---|---|
| `group by (cluster) (last_over_time(kube_node_info[1h]))` | Powers `GET /v1/clusters` discovery |
| `up` | Distinguishes "no data in window" (`outside_retention`) from "upstream healthy but window empty" |

### Edge → metric mapping

| Edge type | Source metric(s) |
|---|---|
| `pod-mounts-pvc` | `kube_pod_spec_volumes_persistentvolumeclaims_info` |
| `pod-to-node` | `kube_pod_info` (`node` label; one per scheduled pod, intra-cluster) |
| `pvc-to-storageclass` | `kube_persistentvolumeclaim_info` (`storageclass` label → `kube_storageclass_info` node; intra-cluster) |
| `pod-calls-pod` | `traces_service_graph_request_total` |
| `pod-calls-service` | `traces_service_graph_request_total` (when target resolves to a service node via connection-string resolution) |
| `service-selects-pod` | `traces_service_graph_request_total` (connection-string resolution + `kube_endpointslice_*` join) |

### Multi-cluster and cross-cluster coverage

Cross-cluster paths and service-graph scenarios are covered by
`internal/integration/` tests against a `testcontainers-go` VictoriaMetrics
container. The suite spins up a real VictoriaMetrics, pushes hand-crafted
fixture series via `POST /api/v1/import/prometheus`, and drives the in-process
API — this is the sole verification path for multi-cluster, cross-cluster, and
service-graph behaviour.

## Configuration

| Flag                            | Env                              | Default              | Notes |
|---------------------------------|----------------------------------|----------------------|-------|
| `--prom-url`                    | `KSG_PROM_URL`                   | `http://localhost:8428` | VictoriaMetrics Prometheus-compatible endpoint. |
| `--listen-addr`                 | `KSG_LISTEN_ADDR`                | `:8080`              | HTTP listen address. |
| `--build-timeout`               | `KSG_BUILD_TIMEOUT`              | `15s`                | Per-build context timeout for `/v1/graph`. |
| `--api-timeout`                 | `KSG_API_TIMEOUT`                | `5s`                 | Per-request timeout for non-graph endpoints with upstream calls (`/v1/clusters`, `/readyz`). |
| `--api-keys-file`               | `KSG_API_KEYS_FILE`              | (empty)              | Path to a file holding accepted API keys (one per line, `#` comments allowed). Designed for K8s `Secret` mounts. Reloaded periodically. |
| `--api-keys`                    | `KSG_API_KEYS`                   | (empty)              | Comma-separated literal keys. Dev only; ignored when `--api-keys-file` is set. |
| `--api-keys-reload-interval`    | `KSG_API_KEYS_RELOAD_INTERVAL`   | `30s`                | How often `--api-keys-file` is re-read. Set to `0` to disable hot reload. |
| `--log-level`                   | `KSG_LOG_LEVEL`                  | `info`               | `debug | info | warn | error`. |
| `--metric-prefix`               | `KSG_METRIC_PREFIX`              | (empty)              | Additive prefix prepended to every kube-state-metrics-shaped series the topology reader queries (e.g. `o11y_` → `o11y_kube_pod_info`). Does **not** affect `traces_service_graph_request_total` or `up{}`. The metric-name suffix and per-series label set are a fixed contract any compatible exporter must honour. |
| —                               | `KSG_PROM_USERNAME`              | (empty)              | HTTP Basic Auth username for the upstream VictoriaMetrics endpoint. **Env-only — no flag exists**, because credential-carrying flags leak via `ps` and container specs. Must be set together with `KSG_PROM_PASSWORD`. |
| —                               | `KSG_PROM_PASSWORD`              | (empty)              | HTTP Basic Auth password for the upstream. Env-only, paired with `KSG_PROM_USERNAME` — setting exactly one of the two fails startup. Rotation requires a restart (no hot reload); changing a Secret-backed env var in a Deployment triggers a rollout anyway. |

### Upstream basic auth

When VictoriaMetrics is protected by basic auth (`-httpAuth.*`, vmauth, or an
authenticating reverse proxy), set both env vars — in Kubernetes, source them
from a `Secret`:

```yaml
env:
  - name: KSG_PROM_USERNAME
    valueFrom:
      secretKeyRef: { name: ksg-upstream-auth, key: username }
  - name: KSG_PROM_PASSWORD
    valueFrom:
      secretKeyRef: { name: ksg-upstream-auth, key: password }
```

Every upstream request (topology, service-graph, cluster discovery, the
`/readyz` probe) then carries `Authorization: Basic …`. The credential values
never appear in logs, traces, metrics, or error responses.

## Documentation

The full API reference is served by the running server:

- **Interactive API reference (Scalar UI):** [`/docs`](http://localhost:8080/docs)
- **OpenAPI 3.1 spec:** [`/openapi.yaml`](http://localhost:8080/openapi.yaml) · [`/openapi.json`](http://localhost:8080/openapi.json)

The spec is generated from in-source annotations (`make docs`) and embedded into
the binary, so it always matches the running build. The Scalar UI loads its
front-end bundle from the jsDelivr CDN.

## Development

### First-time setup

Run **once** after cloning. Bootstraps the dev environment, downloads modules,
and installs host-level tools (`golangci-lint`, `govulncheck`). Mockery is
tracked via go.mod's `tool` directive (Go 1.24+) and invoked through
`go tool mockery` — no separate install step is required.

```bash
make init           # go mod download + dev tools
make doctor         # verify toolchain (go, golangci-lint, govulncheck, mockery, docker)
make init-hooks     # (optional) install pre-commit hook (gofmt + go vet)
```

Required: Go 1.25+. The toolchain pinned in `go.mod` (currently `go1.26.4`)
will be auto-fetched by Go on first build.

### Day-to-day commands

```bash
make build          # compile binary
make test           # unit + component + golden + property + integration (Docker required)
make lint           # golangci-lint
make vuln           # govulncheck
make cover          # coverage profile
```

### Mocks (mockery)

Production-side dependencies are exposed as small interfaces (`promql.Querier`,
`auth.Validator`, `clock.Clock`) so unit tests can substitute mockery-generated
mocks instead of fronting real services with `httptest.NewServer`. Mocks live
under `internal/<pkg>/mocks/` and are committed to git so CI does not need
mockery installed.

```bash
make mocks          # regenerate mocks after editing an interface
make verify-mocks   # CI-style freshness check (regen + git diff)
```

`.mockery.yaml` lists the configured interfaces. After **adding or editing any
interface** registered there, run `make mocks` and commit the regenerated
files — the `mocks-drift` CI job blocks merges otherwise.

### Test layout

| Suite | Where | Real I/O? |
|---|---|---|
| Unit | `pkg/{graph,build,promql,clock,cytoscape,kubegraph}/*_test.go` + `internal/{config,auth,telemetry}/*_test.go` | None — pure Go. |
| Component | `internal/api/*_test.go` | None — `MockQuerier` injected via interface; `httptest.NewServer` only wraps the server-under-test, never fakes upstream. |
| Golden | `internal/api/golden_test.go` + `testdata/golden/*.json` | None. Run with `-update` to refresh snapshots. |
| Integration | `internal/integration/*` | **Docker required.** testcontainers-go spins a real VictoriaMetrics container; `SkipIfDockerUnavailable` skips locally without Docker. CI runs the full suite. |

The boundary between unit and integration is strict: anything that touches a
TCP socket fronting an upstream service is integration. Unit tests must run
with no external dependencies.

## License

Apache-2.0
