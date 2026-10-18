# app-metadata-source Specification

## Purpose

Lets an operator feed one purpose-built, per-pod metric that names each pod's
ArgoCD Application, the last user who deployed it and the owner of its
namespace, and defines how the engine configures, reads and resolves that
metric.

## Requirements

### Requirement: The metric is opt-in by name

The server SHALL read the application-metadata metric only when its name is configured (`--app-metadata-metric` / `KSG_APP_METADATA_METRIC`). With no name configured the server SHALL issue no application-metadata query, and every rendered query and response body SHALL be byte-identical to a deployment without this capability.

#### Scenario: Unset metric issues nothing

- **WHEN** the server runs with no application-metadata metric configured and serves `/v1/graph`, `/v1/storage-graph` and a pod Application lookup
- **THEN** no query naming an application-metadata metric is issued and every body matches the pre-change golden files

#### Scenario: Configured metric is read

- **WHEN** the server runs with `KSG_APP_METADATA_METRIC=argocd_app_pod_info` and serves `/v1/graph`
- **THEN** one query reading `argocd_app_pod_info` is issued for the build

### Requirement: Logical fields map to configurable label names

The metric SHALL be read through five logical fields, each bound to a label name: `app` (default `argocd_app`), `user` (default `user`), `ns_owner` (default `owner`), `namespace` (default `namespace`) and `pod` (default `pod`). `--app-metadata-labels` / `KSG_APP_METADATA_LABELS`, a comma-separated `field=label` list, SHALL override any subset; fields it omits keep their defaults.

#### Scenario: Partial override keeps the other defaults

- **WHEN** the labels setting is `app=application,user=deployer`
- **THEN** the Application is read from `application`, the deployer from `deployer`, and the namespace owner, namespace and pod from `owner`, `namespace` and `pod`

#### Scenario: Configured namespace label carries the request filter

- **WHEN** the namespace field is bound to `k8s_namespace` and a `/v1/graph` request carries `namespace=shop`
- **THEN** the application-metadata query carries the matcher `k8s_namespace="shop"` and no `namespace` matcher

### Requirement: Cluster and zone labels are not configurable

The metric's cluster SHALL be read from the label `cluster`, and its availability zone and environment from the labels bound by `--az-label` / `--env-label`, exactly as for kube-state-metrics series, so a metric row composes the same cluster identity as the topology it decorates.

#### Scenario: Rows join under the composed identity

- **WHEN** the deployment binds `az` to `zone`, and a metric row carries `zone="us"`, `env="dev"`, `cluster="c1"`, namespace `shop`, pod `web-1`
- **THEN** the row's Application is attached to the pod whose id is `us-dev-c1/<uid>` for pod `web-1` in namespace `shop`

#### Scenario: A row without zone labels is ignored under a zone matcher

- **WHEN** a metric row for pod `web-1` carries `cluster="c1"` but no `az` or `env` label, the pod's kube-state-metrics series carry `az="zone-a"`, and a request carries `az=zone-a` (a `/v1/graph` filter, any `/v1/storage-graph` request, or a pod Application lookup pinned to `zone-a`)
- **THEN** the zone matcher excludes the row, and the pod's Application comes from its controller chain

### Requirement: Configuration is validated at startup

The server SHALL refuse to start, naming the offending value, when: the metric name is not a legal Prometheus metric name; a label is not a legal label name; the labels setting names an unknown field or repeats one; two fields share a label; a field's label equals `cluster` or the configured `az` / `env` label; or the labels setting is set while the metric name is not.

#### Scenario: Unknown field rejected

- **WHEN** the labels setting is `application=argocd_app`
- **THEN** the process exits non-zero before binding the listener, with an error naming the field `application`

#### Scenario: Label colliding with the zone key rejected

- **WHEN** the `az` label is `zone` and the labels setting is `ns_owner=zone`
- **THEN** startup fails with an error naming `ns_owner` and `zone`

#### Scenario: Mapping without a metric rejected

- **WHEN** the labels setting is set and the metric name is empty
- **THEN** startup fails with an error stating that the labels setting requires the metric setting

### Requirement: Pod Application from the metric

For each `(cluster, namespace, pod)`, the metric's Application SHALL be the lexically-smallest non-empty `app` value among the rows the window returns for that pod; rows with an empty `app` or empty `pod` SHALL NOT contribute. The value SHALL be used verbatim, with no tracking-id parsing.

#### Scenario: Smallest non-empty value wins

- **WHEN** pod `web-1` has rows with `app` values `""`, `shop-b` and `shop-a` in the window
- **THEN** the metric's Application for `web-1` is `shop-a`

#### Scenario: Value is not split on a colon

- **WHEN** a row carries `app="shop:v2"`
- **THEN** the metric's Application is `shop:v2`

### Requirement: Namespace owner from the metric

For each `(cluster, namespace)`, the namespace owner SHALL be the non-empty `ns_owner` value of the row seen latest in the window, the lexically-smallest value breaking an exact tie. Rows with an empty `ns_owner` SHALL NOT contribute; a row with an empty `pod` SHALL still contribute.

#### Scenario: Latest owner wins

- **WHEN** namespace `shop` has a row with `ns_owner="team-a"` last seen at t1 and a row with `ns_owner="team-b"` last seen at t2 > t1
- **THEN** the namespace owner of `shop` is `team-b`

#### Scenario: Tie broken lexically

- **WHEN** two rows of namespace `shop` were last seen at the same instant with owners `team-b` and `team-a`
- **THEN** the namespace owner of `shop` is `team-a`

### Requirement: Application deployer from the metric

For each `(cluster, namespace, app)` named by a row's own `app` label, the deployer SHALL be the non-empty `user` value of the row seen latest in the window, the lexically-smallest value breaking an exact tie. Rows with an empty `app` or empty `user` SHALL NOT contribute.

#### Scenario: Deployer keyed by the row's own Application

- **WHEN** rows in namespace `shop` carry `app="checkout"` with `user="alice"` last seen at t1 and `user="bob"` last seen at t2 > t1
- **THEN** the deployer of `checkout` in `shop` is `bob`, regardless of which source resolved any pod's Application

### Requirement: The metric belongs to the optional `appmeta` family

The application-metadata query SHALL be dispatched to the backends serving the `appmeta` family. When no backend serves it, the build SHALL issue no application-metadata query and behave as if the metric were unset. When the metric is configured and, at startup, no backend serves `appmeta`, the server SHALL log one Warn naming the family and SHALL start.

#### Scenario: Unserved family behaves as unset

- **WHEN** the metric is configured and the routing table serves `appmeta` on no backend
- **THEN** startup logs a Warn naming `appmeta`, the server starts, and `/v1/graph` bodies equal those of a deployment with the metric unset

#### Scenario: Dedicated store

- **WHEN** backend `meta-a` declares `families: [appmeta]`, `zones: [zone-a]` and a `/v1/graph` request carries `az=zone-a`
- **THEN** the application-metadata query is issued only to `meta-a`

### Requirement: Self-metrics name the query logically

On the graph builds, the query name that upstream self-metric `query` labels, span query-name attributes, tally keys and log lines carry SHALL be `app_metadata`, never the configured metric name. The rendered PromQL text is not a query name. No self-metric label value SHALL depend on the configured metric name.

#### Scenario: Configured name does not leak into self-metric labels

- **WHEN** the metric is configured as `argocd_app_pod_info` and a build issues its query
- **THEN** `kube_state_graph_upstream_query_duration_seconds` records it under `query="app_metadata"` and no series carries `query="argocd_app_pod_info"`

#### Scenario: The lookup keeps the label-query name

- **WHEN** the metric is configured as `argocd_app_pod_info` and a pod Application lookup issues its `appmeta` query
- **THEN** the query is recorded under the label-query path's fixed `query` value, and no series carries `query="argocd_app_pod_info"`

### Requirement: Graph-build degrade on the metric

On `/v1/graph`, an application-metadata query error SHALL NOT fail the build: every pod SHALL resolve its Application through the controller chain, no group SHALL carry a namespace owner or deployer, and the degrade SHALL be logged naming the query. The resulting body SHALL equal the body of the same build with the metric unset.

#### Scenario: Query error falls back to the controller chain

- **WHEN** the application-metadata query errors and pod `web-1`'s controller resolves Application `shop`
- **THEN** the build returns 200, `web-1` carries `data.application: "shop"`, and no group node carries `namespace_owner` or `deployed_by`
