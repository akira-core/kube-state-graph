## MODIFIED Requirements

### Requirement: Centralised VictoriaMetrics as the only topology source

The topology reader SHALL fetch all pod, node, and PVC topology by issuing PromQL queries against one or more configured Prometheus-compatible endpoints, pointing at VictoriaMetrics. Which endpoint a given query is issued to is decided by the routing table defined by the `upstream-backend-routing` capability; when no routing table is configured, the single `--prom-url` endpoint serves every query. The reader SHALL NOT call the Kubernetes API server, SHALL NOT scrape `kube-state-metrics` directly, and SHALL NOT use Kubernetes informers.

Regardless of how many endpoints are configured, the endpoints are the reader's **only** source of cluster facts: no Kubernetes client, no informer, no kubeconfig, and no per-cluster RBAC is involved at any point.

#### Scenario: Single configured upstream

- **WHEN** the server starts with `--prom-url=http://vm.example:8428` and no routing table
- **THEN** every topology query is sent to `http://vm.example:8428` and no other HTTP destinations

#### Scenario: Routed upstreams

- **WHEN** the server starts with a routing table declaring `http://vm-a.example:8428` and `http://vm-b.example:8428`
- **THEN** every topology query is sent to one or both of those two endpoints as the routing table dictates, and to no other HTTP destinations

#### Scenario: No Kubernetes API access

- **WHEN** the server runs in any environment
- **THEN** the binary makes no requests to any `/api/*` Kubernetes API path and requires no Kubernetes ServiceAccount or kubeconfig

### Requirement: Optional basic-auth credentials for the upstream endpoint

The server SHALL support optional HTTP Basic Auth credentials for each upstream Prometheus-compatible endpoint, sourced **exclusively** from environment variables. No CLI flag SHALL exist for any credential value — credential-carrying flags leak through process listings and container specs; this is a deliberate exception to the env+flag dual-track configuration convention.

`KSG_PROM_USERNAME` and `KSG_PROM_PASSWORD` are the **global** pair. When both are set (non-empty), every outbound HTTP request to an upstream that declares no credentials of its own — topology queries, the service-graph queries, the Harvest queries, and the `/readyz` `up` probe — SHALL carry an `Authorization: Basic` header for those credentials. When both are unset, such requests SHALL carry no `Authorization` header and behaviour is unchanged from an unauthenticated deployment.

A routed deployment MAY additionally give an individual backend its own pair by naming the environment variables holding it, as specified by the `upstream-backend-routing` capability's per-backend credential requirement; that pair takes precedence over the global one for requests to that backend.

Setting exactly one of the two global variables (non-empty) SHALL fail server startup with a validation error that names both environment variables but does NOT echo either value.

Credential values SHALL NOT appear in any log line, trace span attribute, metric label, error message, or HTTP response body. Rotation of a credential **value** requires a process restart — there is no hot reload for upstream credential values.

#### Scenario: Credentials applied to all upstream queries

- **WHEN** the server starts with `KSG_PROM_USERNAME=ksg` and `KSG_PROM_PASSWORD=s3cret` and serves a `/v1/graph` request
- **THEN** every upstream HTTP request issued for the build (topology fan-out, service-graph, Harvest, and any readiness query) carries `Authorization: Basic` for `ksg:s3cret`

#### Scenario: Per-backend pair overrides the global pair

- **WHEN** the global pair is set and one backend declares its own credential variables
- **THEN** requests to that backend carry the backend's credentials and requests to every other backend carry the global pair

#### Scenario: No credentials configured

- **WHEN** the server starts with neither `KSG_PROM_USERNAME` nor `KSG_PROM_PASSWORD` set and no backend declares its own
- **THEN** upstream requests carry no `Authorization` header and startup validation passes

#### Scenario: Half-configured credentials rejected at startup

- **WHEN** the server starts with `KSG_PROM_USERNAME=ksg` and no `KSG_PROM_PASSWORD` (or vice versa)
- **THEN** `config.Validate` returns an error naming `KSG_PROM_USERNAME` and `KSG_PROM_PASSWORD`, the error does not contain the configured value, and the process exits non-zero before binding the listener

#### Scenario: No CLI flag exists for credentials

- **WHEN** the server is started with `--prom-username=x` or `--prom-password=x`
- **THEN** flag parsing fails with an unknown-flag error, because credentials are env-only

#### Scenario: Credentials never logged

- **WHEN** the server runs with credentials configured at any log level, including `debug`, and upstream queries succeed or fail
- **THEN** no emitted log line, span attribute, or error string contains any configured username or password

### Requirement: Request-scoped upstream selectors

The build SHALL accept four request-scoped selector dimensions — `az`, `env`, `cluster`, `namespace` — each a set of values, and SHALL render them into the upstream PromQL queries as label matchers composed **with** (never replacing) each query's fixed, request-invariant selectors (`type=~"ExternalIP|InternalIP"`, `condition="Ready"`, `lun=""`, the service-graph sentinel and `edge_relation!="link"` matchers). Which dimensions reach which series is a hardcoded contract with no configuration surface:

- `az` and `env`: every kube-state-metrics and kubelet series. **Never** a NetApp Harvest series — the `az` dimension reaches Harvest through backend selection only (the `upstream-backend-routing` capability's zone rule), and `env` does not reach Harvest at all.
- `cluster`: every kube-state-metrics and kubelet series. **Never** a Harvest series — Harvest's `cluster` label names the ONTAP cluster, not a Kubernetes cluster — and never a service-graph series.
- `namespace`: every kube-state-metrics and kubelet series that carries a `namespace` label — the pod-, claim-, Service-, EndpointSlice-, and **controller**-scoped series (the six controller-annotation families and `kube_job_owner` are namespaced like every other workload family). Never a node series, never a Harvest series, never a service-graph series.
- Every NetApp Harvest series, the three `traces_service_graph_*` series, and the `up{}` probe SHALL carry **no** request-scoped matcher under any request. The `qos_*` families keep their fixed `lun=""` selector.

Rendering SHALL be a pure function of the sorted, de-duplicated value set: one value renders `<key>="<value>"` (with `"` and `\` escaped); two or more render one fully-anchored alternation `<key>=~"<v1>|<v2>"` whose alternatives are regex-quoted and THEN string-escaped (a backslash introduced by regex-quoting is doubled, because a PromQL string literal rejects an unknown escape sequence); an empty set renders nothing. Matchers inside a selector SHALL appear in a fixed order (fixed selectors first, then `az`, `env`, `cluster`, `namespace`). The `cluster` value `unknown` renders `cluster=~"unknown|"` (see "Series missing the cluster label"), the one value that is not rendered as a plain literal. Series families that carry no request-scoped matcher for a dimension are narrowed by **reference** instead — a node is emitted only when a loaded pod is scheduled on it, an aggregate only when a loaded claim's `volumename` joins to it — as specified by the `graph-api` retention requirements.

A build with every dimension empty SHALL issue each query exactly as it is issued today. Zero rows under a non-empty dimension is a valid, empty topology — not a failure and not a retention miss.

#### Scenario: Fixed selector composed with request matchers

- **WHEN** a build runs with `az={zone-a}` and `cluster={cluster-alpha}`
- **THEN** the node-address query is issued as `last_over_time(kube_node_status_addresses{type=~"ExternalIP|InternalIP",az="zone-a",cluster="cluster-alpha"}[<window>])` and the node-condition query keeps `condition="Ready"` ahead of the request matchers

#### Scenario: Controller-annotation families receive all four dimensions

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={cluster-alpha}` and `namespace={shop}`
- **THEN** each of the six controller-annotation queries and the `kube_job_owner` query is issued with exactly `az="zone-a",env="prod",cluster="cluster-alpha",namespace="shop"` added, in that fixed order

#### Scenario: Harvest receives zone and environment but not cluster or namespace

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={cluster-alpha}`, `namespace={shop}`
- **THEN** every Harvest query (`volume_labels`, the `qos_*` families, `qos_policy_fixed_max_throughput_*`, `aggr_*`, `node_new_status`) is issued exactly as in an unfiltered build — the `qos_*` families with `lun=""` only — with no `az`, `env`, `cluster`, or `namespace` matcher; the `az` value reaches Harvest only as backend selection (which `harvest` backends the query is issued to) and the `env` value does not reach it at all

#### Scenario: Service-graph series are never narrowed

- **WHEN** a build runs with any non-empty combination of the four dimensions
- **THEN** the three `traces_service_graph_*` queries are issued exactly as for an unfiltered build (sentinel and link matchers only), and the `up{}` probe query is the bare `up`

#### Scenario: Multi-value rendering is order-free

- **WHEN** one build runs with `namespace={b, a}` and another with `namespace={a, b, a}`
- **THEN** both issue `namespace=~"a|b"` on every namespace-scoped series

#### Scenario: Regex metacharacters in a value are quoted

- **WHEN** a build runs with `env={prod.eu, prod-us}`
- **THEN** the rendered alternation is `env=~"prod-us|prod\\.eu"` — the metacharacter is regex-quoted AND the resulting backslash is escaped for the PromQL string literal, which rejects an unknown escape sequence — so a series with `env="prodXeu"` does not match

#### Scenario: Empty dimensions render nothing

- **WHEN** a build runs with all four dimensions empty
- **THEN** every issued query string is byte-identical to the query issued before request-scoped selectors existed

## ADDED Requirements

### Requirement: Backend routing composes with request-scoped selectors

Backend selection and PromQL matcher rendering SHALL be independent, composed mechanisms. The `az` dimension SHALL continue to be rendered as a label matcher on every query that accepts it — exactly as specified by "Request-scoped upstream selectors" — **in addition to** selecting which backends the query is issued to. Neither mechanism SHALL substitute for the other: routing narrows which store is asked, the matcher narrows what that store returns. The `harvest` family is the one family where the two diverge: it is zone-routed yet accepts no `az` matcher, so backend selection is the only effect `az` has on it (see the `netapp-storage-graph` capability).

The rendered query string for a given query SHALL be identical across every backend the query is fanned out to. A per-backend query variant SHALL NOT exist.

The `env`, `cluster`, and `namespace` dimensions SHALL play no part in backend selection.

#### Scenario: Zone matcher still rendered under routing

- **WHEN** a request carries `az=zone-a` and the routing table sends `ksm` queries for that zone only to backend `zone-a`
- **THEN** the query issued to `zone-a` still carries the `az="zone-a"` matcher

#### Scenario: Identical query string across backends

- **WHEN** a request with no `az` fans `kube_pod_info` out to three backends
- **THEN** all three receive byte-identical query strings

#### Scenario: Namespace filter does not route

- **WHEN** a request carries `namespace=shop` and no `az`
- **THEN** every backend serving the family is selected, exactly as for a request carrying neither parameter
