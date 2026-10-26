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

The metric SHALL be read through nine logical fields, each bound to a label, default in parentheses: `app` (`argocd_app`), `user` (`user`), `ns_owner` (`owner`), `namespace` (`namespace`), `pod` (`pod`), `deploy_time` (`deploy_time`), `ns_group` (`namespace_group`), `ns_group_owner` (`namespace_group_owner`) and `ns_alias` (`namespace_alias`). `--app-metadata-labels` / `KSG_APP_METADATA_LABELS`, a comma-separated `field=label` list, SHALL override any subset; omitted fields keep their defaults.

#### Scenario: Partial override keeps the other defaults

- **WHEN** the labels setting is `app=application,user=deployer`
- **THEN** the Application is read from `application`, the deployer from `deployer`, and the namespace owner, namespace, pod, deploy time, namespace group, namespace group owner and namespace alias from `owner`, `namespace`, `pod`, `deploy_time`, `namespace_group`, `namespace_group_owner` and `namespace_alias`

#### Scenario: Configured namespace label carries the request filter

- **WHEN** the namespace field is bound to `k8s_namespace` and a `/v1/graph` request carries `namespace=shop`
- **THEN** the application-metadata query carries the matcher `k8s_namespace="shop"` and no `namespace` matcher

#### Scenario: New fields are overridable

- **WHEN** the labels setting is `deploy_time=last_deployed,ns_alias=ns_display`
- **THEN** the deploy time is read from `last_deployed` and the namespace alias from `ns_display`, and every other field keeps its default label

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

#### Scenario: Existing override colliding with a new default rejected

- **WHEN** the labels setting is `user=namespace_alias` and does not rebind `ns_alias`
- **THEN** startup fails with an error naming the label `namespace_alias` and the fields `user` and `ns_alias`

#### Scenario: Rebinding the new field resolves the collision

- **WHEN** the labels setting is `user=namespace_alias,ns_alias=ns_display`
- **THEN** the server starts, reading the deployer from `namespace_alias` and the namespace alias from `ns_display`

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

### Requirement: Namespace group and group owner from the metric

For each `(cluster, namespace)`, the namespace group SHALL be the non-empty `ns_group` value of the row seen latest in the window, the lexically-smallest value breaking an exact tie. The group owner SHALL be the `ns_group_owner` of a row that selected that group and never come from any other row. A row with an empty `ns_group` SHALL contribute to neither; a row with an empty `pod` SHALL still contribute.

#### Scenario: Latest group wins with its own owner

- **WHEN** namespace `shop` has a row with `ns_group="retail"`, `ns_group_owner="alice"` last seen at t1 and a row with `ns_group="commerce"`, `ns_group_owner="bob"` last seen at t2 > t1
- **THEN** the namespace group of `shop` is `commerce` and its group owner is `bob`

#### Scenario: Owner is never borrowed from another row

- **WHEN** namespace `shop` has a row with `ns_group="commerce"` and no `ns_group_owner` last seen at t2, and a row with `ns_group="retail"`, `ns_group_owner="alice"` last seen at t1 < t2
- **THEN** the namespace group of `shop` is `commerce` and it has no group owner

#### Scenario: Owner without a group is ignored

- **WHEN** the only row of namespace `shop` carrying `ns_group_owner="alice"` has an empty `ns_group`
- **THEN** namespace `shop` has neither a namespace group nor a group owner from that row

#### Scenario: Tied rows pick the owner deterministically

- **WHEN** three rows of namespace `shop` were last seen at the same instant, all with `ns_group="commerce"`, and with `ns_group_owner` values `""`, `carol` and `bob`
- **THEN** the group owner of `shop` is `bob` regardless of the order the rows arrive in, since a non-empty owner wins over an empty one and the lexically-smallest non-empty owner breaks the rest

#### Scenario: A namespace-level row contributes

- **WHEN** a row carries `namespace="shop"`, `ns_group="commerce"` and an empty `pod`
- **THEN** it takes part in the namespace group pick of `shop`

### Requirement: Namespace alias from the metric

For each `(cluster, namespace)`, the namespace alias SHALL be the non-empty `ns_alias` value of the row seen latest in the window, the lexically-smallest value breaking an exact tie, independently of the namespace group and owner picks. Rows with an empty `ns_alias` SHALL NOT contribute; a row with an empty `pod` SHALL still contribute.

#### Scenario: Latest alias wins

- **WHEN** namespace `shop` has a row with `ns_alias="Shop (old)"` last seen at t1 and a row with `ns_alias="Shop"` last seen at t2 > t1
- **THEN** the namespace alias of `shop` is `Shop`

#### Scenario: Alias does not follow the group's row

- **WHEN** the latest row of namespace `shop` carries `ns_group="commerce"` and no `ns_alias`, and an older row carries `ns_alias="Shop"`
- **THEN** the namespace group of `shop` is `commerce` and its alias is `Shop`

### Requirement: Application deploy time from the metric

For each `(cluster, namespace, app)`, the deploy time SHALL come from a row that the deployer pick selected (same latest last-seen time, same `user`) and never from any other row. Among those rows the latest parsable deploy time SHALL win; the deploy time is unresolved when none parses. A row with an empty `app` or empty `user` SHALL NOT contribute. The deployer pick itself is unchanged.

#### Scenario: Deploy time is paired with the deployer

- **WHEN** rows in namespace `shop` carry `app="checkout"` with `user="alice"`, `deploy_time="2025/10/01 08:00:00"` last seen at t1 and `user="bob"`, `deploy_time="2025/10/08 13:14:15"` last seen at t2 > t1
- **THEN** the deployer of `checkout` is `bob` and its deploy time is `2025-10-08T13:14:15Z`

#### Scenario: Deploy time is never borrowed from another row

- **WHEN** rows in namespace `shop` carry `app="checkout"` with `user="bob"` and no `deploy_time` last seen at t2, and `user="alice"`, `deploy_time="2025/10/01 08:00:00"` last seen at t1 < t2
- **THEN** the deployer of `checkout` is `bob` and it has no deploy time

#### Scenario: A row without a user contributes no deploy time

- **WHEN** the only row of `checkout` carrying a `deploy_time` has an empty `user`
- **THEN** `checkout` has no deploy time

#### Scenario: Tied rows pick the deploy time deterministically

- **WHEN** two rows of `checkout` were last seen at the same instant, both with `user="bob"`, and with `deploy_time` values `2025/10/01 08:00:00` and `2025/10/08 13:14:15`
- **THEN** the deploy time of `checkout` is `2025-10-08T13:14:15Z` regardless of the order the rows arrive in

#### Scenario: An unparsable tied row does not mask a parsable one

- **WHEN** two rows of `checkout` were last seen at the same instant, both with `user="bob"`, and with `deploy_time` values `yesterday` and `2025/10/08 13:14:15`
- **THEN** the deploy time of `checkout` is `2025-10-08T13:14:15Z`

### Requirement: Deploy time format

A deploy-time value SHALL be accepted only as `YYYY/MM/DD hh:mm:ss` with every component zero-padded to fixed width, read as UTC, or as RFC 3339 with its own offset. It SHALL be emitted as RFC 3339 in UTC with whole seconds and a `Z` suffix, fractional seconds truncated. Any other value leaves the deploy time unresolved and SHALL NOT fail the build.

#### Scenario: Slash form is read as UTC

- **WHEN** the selected row carries `deploy_time="2025/10/08 13:14:15"`
- **THEN** the deploy time is `2025-10-08T13:14:15Z`

#### Scenario: RFC 3339 is converted to UTC

- **WHEN** the selected row carries `deploy_time="2025-10-08T13:14:15.987+08:00"`
- **THEN** the deploy time is `2025-10-08T05:14:15Z`

#### Scenario: Values outside the two forms are unresolved

- **WHEN** the row that selected deployer `bob` for `checkout` carries a `deploy_time` of `2025/10/08 9:14:15`, `2025/10/08 13:14:15.5`, ` 2025/10/08 13:14:15`, `1759929255` or `2025/02/30 00:00:00`
- **THEN** `checkout` has no deploy time, its deployer is still `bob`, and the build succeeds

#### Scenario: Lenient RFC 3339 variants are unresolved

- **WHEN** the row that selected deployer `bob` for `checkout` carries a `deploy_time` of `2025-10-08T1:14:15Z`, `2025-10-08T13:14:15,5Z` or `2025-10-08T13:14:15+24:00`
- **THEN** `checkout` has no deploy time, its deployer is still `bob`, and the build succeeds

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

On `/v1/graph`, an application-metadata query error SHALL NOT fail the build: every pod SHALL resolve its Application through the controller chain, no group SHALL carry any group metadata field, and the degrade SHALL be logged naming the query. The resulting body SHALL equal the body of the same build with the metric unset.

#### Scenario: Query error falls back to the controller chain

- **WHEN** the application-metadata query errors and pod `web-1`'s controller resolves Application `shop`
- **THEN** the build returns 200, `web-1` carries `data.application: "shop"`, and no group node carries `namespace_owner`, `namespace_group`, `namespace_group_owner`, `namespace_alias`, `deployed_by` or `deployed_at`
