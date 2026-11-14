## MODIFIED Requirements

### Requirement: Request-scoped upstream selectors

The build SHALL accept four request-scoped selector dimensions — `az`, `env`, `cluster`, `namespace` — each a set of values, and SHALL render them into the upstream PromQL queries as label matchers composed **with** (never replacing) each query's fixed, request-invariant selectors. Which dimensions reach which series SHALL be a hardcoded contract with no configuration surface.

#### Scenario: Fixed selectors the request matchers compose with

- **WHEN** a query is rendered under a request-scoped selector
- **THEN** its fixed selectors are kept: `type=~"ExternalIP|InternalIP"`, `condition="Ready"`, `lun=""`, `owner_kind="CronJob",owner_is_controller="true"` on `kube_job_owner`, `annotation_argocd_argoproj_io_tracking_id!=""` on each of the six controller-annotation families, and the service-graph sentinel and `edge_relation!="link"` matchers

#### Scenario: Zone, environment and cluster reach

- **WHEN** `az`, `env` or `cluster` is non-empty
- **THEN** it renders on every kube-state-metrics and kubelet series and never on a service-graph series
- **AND** `az` and `env` **never** render on a NetApp Harvest series — the `harvest` store is the zone boundary for the `az` it is routed by (see "Backend routing composes with request-scoped selectors"), and a Harvest series is not required to carry either label
- **AND** `cluster` **never** renders on a Harvest series, because Harvest's `cluster` label names the ONTAP cluster, not a Kubernetes cluster

#### Scenario: Namespace reach

- **WHEN** `namespace` is non-empty
- **THEN** it renders on every kube-state-metrics and kubelet series that carries a `namespace` label — the pod-, claim-, Service-, EndpointSlice-, and **controller**-scoped series, the six controller-annotation families and `kube_job_owner` being namespaced like every other workload family
- **AND** it never renders on a node series, a Harvest series, or a service-graph series

#### Scenario: Series that take no request matcher

- **WHEN** any request runs
- **THEN** the three `traces_service_graph_*` series, the `up{}` probe and every NetApp Harvest series carry **no** request-scoped matcher, and the `qos_*` families keep their fixed `lun=""` selector

#### Scenario: Unmatched families are narrowed by reference

- **WHEN** a series family carries no request-scoped matcher for a dimension
- **THEN** it is narrowed by **reference** instead — a node is emitted only when a loaded pod is scheduled on it, an aggregate only when a loaded claim's `volumename` joins to it — as specified by the `graph-api` retention requirements
- **AND** zero rows under a non-empty dimension is a valid, empty topology — not a failure and not a retention miss

#### Scenario: Fixed selector composed with request matchers

- **WHEN** a build runs with `az={zone-a}` and `cluster={cluster-alpha}`
- **THEN** the node-address query is issued as `last_over_time(kube_node_status_addresses{type=~"ExternalIP|InternalIP",az="zone-a",cluster="cluster-alpha"}[<window>])` and the node-condition query keeps `condition="Ready"` ahead of the request matchers

#### Scenario: Controller-annotation families receive all four dimensions

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={cluster-alpha}` and `namespace={shop}`
- **THEN** each of the six controller-annotation queries and the `kube_job_owner` query is issued with exactly `az="zone-a",env="prod",cluster="cluster-alpha",namespace="shop"` added, in that fixed order, following that query's own fixed selector

#### Scenario: Controller-annotation fixed selectors precede the request matchers

- **WHEN** a build runs with `namespace={shop}`
- **THEN** the Deployment annotation query is issued as `last_over_time(kube_deployment_annotations{annotation_argocd_argoproj_io_tracking_id!="",namespace="shop"}[<window>])` and the Job-owner query as `last_over_time(kube_job_owner{owner_kind="CronJob",owner_is_controller="true",namespace="shop"}[<window>])` — the fixed selector first, the request matcher composed after it and never replacing it

#### Scenario: Harvest receives no request-scoped matcher

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** every Harvest query is issued exactly as in an unfiltered build, with no `cluster` or `namespace` matcher

#### Scenario: Harvest receives the zone and environment only

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={cluster-alpha}`, `namespace={shop}`
- **THEN** no Harvest query (`volume_labels`, the `qos_*` families, `qos_policy_fixed_max_throughput_*`, `aggr_*`, the `node_*` families) carries any of the four as a matcher — the `aggr_new_status` query is `last_over_time(aggr_new_status[<window>])`, exactly as in an unfiltered build — and of the four only `az` acts on Harvest directly, by selecting which `harvest` backends are asked; `env`, `cluster` and `namespace` reach it solely through the claims they load

#### Scenario: Service-graph series are never narrowed

- **WHEN** a build runs with any non-empty combination of the four dimensions
- **THEN** the three `traces_service_graph_*` queries are issued exactly as for an unfiltered build (sentinel and link matchers only), and the readiness / retention probe query is `count(up)`, with no request matcher

### Requirement: Request-scoped matcher rendering

Request-scoped matcher rendering SHALL be a pure function of the sorted, de-duplicated value set: one value renders `<key>="<value>"`, two or more render one fully-anchored alternation `<key>=~"<v1>|<v2>"`, and an empty set renders nothing. A build with every dimension empty SHALL add **no** request-scoped matcher to any query, so each query renders exactly its fixed form.

#### Scenario: Values are escaped for PromQL

- **WHEN** a matcher value is rendered
- **THEN** a single value has `"` and `\` escaped
- **AND** the alternatives of an alternation are regex-quoted and THEN string-escaped: a backslash introduced by regex-quoting is doubled, because a PromQL string literal rejects an unknown escape sequence
- **AND** the string escaping covers control characters itself rather than relying on request validation: `\n`, `\r` and `\t` render as those named escapes, and every other byte in 0x00–0x1F and 0x7F renders as `\xHH`, so no value can break out of, or terminate, the string literal

#### Scenario: Matchers appear in a fixed order

- **WHEN** a selector carries fixed and request-scoped matchers
- **THEN** the fixed selectors come first, then `az`, `env`, `cluster`, `namespace`

#### Scenario: The unknown cluster is the one non-literal value

- **WHEN** the `cluster` value is `unknown`
- **THEN** it renders `cluster=~"unknown|"` (see "Series missing the cluster label"), the one value that is not rendered as a plain literal

#### Scenario: Multi-value rendering is order-free

- **WHEN** one build runs with `namespace={b, a}` and another with `namespace={a, b, a}`
- **THEN** both issue `namespace=~"a|b"` on every namespace-scoped series

#### Scenario: Regex metacharacters in a value are quoted

- **WHEN** a build runs with `env={prod.eu, prod-us}`
- **THEN** the rendered alternation is `env=~"prod-us|prod\\.eu"` — the metacharacter is regex-quoted AND the resulting backslash is escaped for the PromQL string literal, which rejects an unknown escape sequence — so a series with `env="prodXeu"` does not match

#### Scenario: Empty dimensions render nothing

- **WHEN** a build runs with all four dimensions empty
- **THEN** every issued query string carries only its own fixed selector — byte-identical to the recorded unfiltered rendering, with no request-scoped matcher appended
