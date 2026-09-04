## MODIFIED Requirements

### Requirement: Declarative upstream backend table

The server SHALL accept a routing table declared in a file whose path is configured by `--backends-file` / `KSG_BACKENDS_FILE`. The file SHALL be accepted in either YAML or JSON form and SHALL declare a list of backends, each carrying:

- `name` — a non-empty identifier, unique across the table. It is the backend's identity in logs, metrics, and every ordering rule below.
- `url` — a Prometheus-compatible query endpoint, parseable as an absolute HTTP or HTTPS URL.
- `families` — a non-empty set of query families this backend serves, drawn from the fixed set defined by "Query family classification".
- `zones` — an optional set of `az` values whose series this backend holds. An omitted or empty set means **every zone** (a catch-all backend).
- `usernameEnv` / `passwordEnv` — optional names of environment variables holding this backend's basic-auth credentials, per "Per-backend credentials sourced from the environment".

The file SHALL NOT carry a credential value in any field. Validation SHALL reject a table that: is empty; declares a duplicate `name`; declares an unparseable or non-HTTP(S) `url`; declares an unknown family; declares an empty `families` set; or leaves any one of the **required** query families (`ksm`, `kubelet`, `harvest`, `servicegraph`, `probe`) served by **no** backend. The `alerts` family is **optional**: a table serving it on no backend SHALL be accepted, and the server SHALL then issue no `ALERTS` query and log one Info stating that the alert overlay is disabled. A rejected table SHALL NOT be applied.

#### Scenario: Valid table accepted

- **WHEN** the server starts with a table declaring backend `zone-a` (`families: [ksm, kubelet, servicegraph, probe]`, `zones: [zone-a]`) and backend `netapp-a` (`families: [harvest]`, `zones: [zone-a]`)
- **THEN** startup succeeds, an Info log names both backends and their families, and a second Info states that no backend serves `alerts`

#### Scenario: Alerts served by a dedicated backend

- **WHEN** the table additionally declares backend `vmalert-a` (`families: [alerts]`, `zones: [zone-a]`)
- **THEN** startup succeeds and every `ALERTS` query for `az=zone-a` is issued only to `vmalert-a`

#### Scenario: JSON and YAML forms are equivalent

- **WHEN** two servers start, one with the table written as YAML and one with the byte-equivalent JSON
- **THEN** both resolve identical backends for every query

#### Scenario: Duplicate backend name rejected

- **WHEN** the table declares two backends both named `zone-a`
- **THEN** validation fails with an error naming `zone-a`, and the process exits non-zero before binding the listener

#### Scenario: Family left unserved rejected

- **WHEN** the table declares backends covering `ksm`, `kubelet`, `servicegraph` and `probe` but no backend declaring `harvest`
- **THEN** validation fails with an error naming the `harvest` family

#### Scenario: Optional family left unserved accepted

- **WHEN** the table declares backends covering all five required families and none declaring `alerts`
- **THEN** validation succeeds

#### Scenario: Credential value in the file rejected

- **WHEN** a backend entry carries a literal `password` or `username` field
- **THEN** validation fails with an error stating that credentials are sourced from the environment only, and the error does not echo the value

### Requirement: Query family classification

Every upstream query the server issues SHALL belong to exactly one of six fixed families, and the mapping SHALL be a hardcoded contract with no configuration surface:

- `ksm` — every `kube_*` kube-state-metrics series (pod, node, PVC, Service, EndpointSlice, owner, and controller-annotation families).
- `kubelet` — `kubelet_volume_stats_used_bytes` and `kubelet_volume_stats_capacity_bytes`.
- `harvest` — every NetApp Harvest series: `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, `node_total_data`.
- `servicegraph` — the three `traces_service_graph_*` series.
- `probe` — the `up{}` probe.
- `alerts` — the `ALERTS` series (the `alert-overlay` capability). This family is zone-routable (`az` selects its backends, and `az` / `env` / `namespace` are rendered as matchers on it); it is the only family a valid table may leave unserved.

A query with no declared family SHALL be a build-time failure of the repository's own test suite, not a runtime default: the classification table SHALL be exhaustive over the query set by construction.

#### Scenario: Every query is classified

- **WHEN** the repository's test suite runs
- **THEN** a test enumerates every declared query and fails if any one of them has no family entry

#### Scenario: Harvest separable from kube-state-metrics

- **WHEN** the table declares one backend serving `ksm`, `kubelet`, `servicegraph` and `probe` at one URL and another serving `harvest` at a different URL
- **THEN** every `kube_*` and `kubelet_*` query is sent only to the first URL and every Harvest query only to the second

#### Scenario: Alerts separable from kube-state-metrics

- **WHEN** the table declares one backend serving `ksm` at one URL and another serving `alerts` at a different URL
- **THEN** the `ALERTS` query is sent only to the second URL and never to the first

### Requirement: Single-backend compatibility mode

When no routing-table file is configured, the server SHALL behave as a table declaring exactly one backend: named `default`, addressed at `--prom-url`, serving **all six** families (the five required plus `alerts`), with no `zones` (a catch-all). Every query SHALL then be issued to exactly one destination, and every rendered query string, merge result, and serialised response body SHALL be byte-identical to the same deployment before backend routing existed — the added `ALERTS` leg contributes nothing to the body when the store holds no `ALERTS` series.

When both a routing-table file and `--prom-url` are configured, the file SHALL take precedence and a Warn SHALL be logged stating that `--prom-url` is ignored.

#### Scenario: No table configured behaves as today

- **WHEN** the server starts with `--prom-url=http://vm.example:8428` and no `--backends-file`
- **THEN** every upstream query — including `ALERTS` — is sent to `http://vm.example:8428` and the served response bodies match the pre-change golden files byte for byte when that store holds no `ALERTS` series

#### Scenario: Table overrides prom-url

- **WHEN** the server starts with both `--prom-url` and `--backends-file` set
- **THEN** the file's backends serve every query, and a Warn log states that `--prom-url` is ignored
