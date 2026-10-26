# Spec Delta

## MODIFIED Requirements

### Requirement: Namespace and application group metadata

For each value the `app-metadata-source` capability resolves, a `namespace` group SHALL carry `data.namespace_owner`, `data.namespace_group`, `data.namespace_group_owner` and `data.namespace_alias` for its `(cluster, namespace)`, and an `application` group `data.deployed_by` and `data.deployed_at` for its `(cluster, namespace, application)`. All are `omitempty` strings outside `labels`, which stay `{}`; no other node type carries any.

#### Scenario: Namespace group carries its owner

- **WHEN** the metric resolves owner `team-a` for namespace `shop` in cluster `us-dev-c1` and the response contains that namespace group
- **THEN** the node `us-dev-c1/namespace/shop` carries `data.namespace_owner: "team-a"` and `data.labels` is `{}`

#### Scenario: Application group carries its deployer

- **WHEN** the metric resolves deployer `alice` for Application `checkout` in namespace `shop` of cluster `us-dev-c1`
- **THEN** the node `us-dev-c1/namespace/shop/application/checkout` carries `data.deployed_by: "alice"`

#### Scenario: Group exists only when the view needs it

- **WHEN** the metric resolves an owner for namespace `idle` but the projected view holds no pod, service or PVC in `idle`
- **THEN** the response contains no `idle` namespace group

#### Scenario: Unresolved values are omitted

- **WHEN** a namespace group or application group has no resolved value for one of its fields, or the metric is unset
- **THEN** its `data` object carries no key for that field, and with the metric unset the body is byte-identical to the pre-change shape

#### Scenario: Application group from the controller chain still gets its deployer

- **WHEN** every pod of Application `checkout` resolved it through the controller chain and a metric row carries `app="checkout"`, `user="alice"` in the same cluster and namespace
- **THEN** the `checkout` application group carries `data.deployed_by: "alice"`

#### Scenario: Real nodes never carry the group fields

- **WHEN** the response contains pod, service, PVC, K8s node, external or NetApp nodes
- **THEN** none of their `data` objects carries `namespace_owner`, `namespace_group`, `namespace_group_owner`, `namespace_alias`, `deployed_by` or `deployed_at`

#### Scenario: Namespace group carries its group, group owner and alias

- **WHEN** the metric resolves group `commerce` with group owner `bob` and alias `Shop` for namespace `shop` in cluster `us-dev-c1`
- **THEN** the node `us-dev-c1/namespace/shop` carries `data.namespace_group: "commerce"`, `data.namespace_group_owner: "bob"` and `data.namespace_alias: "Shop"`, and `data.labels` is `{}`

#### Scenario: Application group carries its deploy time

- **WHEN** the metric resolves deployer `alice` and deploy time `2025-10-08T13:14:15Z` for Application `checkout` in namespace `shop` of cluster `us-dev-c1`
- **THEN** the node `us-dev-c1/namespace/shop/application/checkout` carries `data.deployed_by: "alice"` and `data.deployed_at: "2025-10-08T13:14:15Z"`

#### Scenario: A metric without the new labels keeps the previous body

- **WHEN** the metric is configured and none of its rows carries a `deploy_time`, `namespace_group`, `namespace_group_owner` or `namespace_alias` label
- **THEN** no group carries `namespace_group`, `namespace_group_owner`, `namespace_alias` or `deployed_at`, and the body equals that of the same build before this change
