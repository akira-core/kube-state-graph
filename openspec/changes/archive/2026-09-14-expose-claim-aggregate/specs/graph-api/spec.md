## ADDED Requirements

### Requirement: PVC `aggr` label

A `type="pvc"` node's `data.labels` SHALL additively carry an `aggr` entry whenever the claim's volume resolved a single containing aggregate. Its value SHALL be the **node id** of that `netapp-aggr` node (`netapp/<ontap_cluster>/aggr/<aggr>`), not the bare aggregate name: aggregate names are unique only within an ONTAP cluster, and the label must name exactly one node. It names the aggregate the claim's `pvc-to-netapp-aggr` edge is drawn to, so in `GET /v1/graph` a response carrying the label also carries that node and that edge, and the three can never disagree. `GET /v1/storage-graph`, which draws no such edge, carries the same label under the `storage-graph-api` capability.

The key follows the rules of "PVC `volumename` and `svm` labels": a plain `labels` entry with no typed `data.aggr` field, **absent** when unresolved, and never an empty string. It SHALL be absent when the claim resolved no aggregate — a FlexGroup volume spanning aggregates, a claim whose volume matched no `volume_labels` series, or an upstream without the Harvest series — and `aggr` SHALL never be present without `volumename` (the join is rooted at the PV name). It is independent of `svm`: a joined series with an empty `svm` label still resolves the aggregate, and a FlexGroup series resolves `svm` with no aggregate.

The addition is additive to the v1 wire contract and MUST NOT disturb the deterministic-body guarantee: for identical upstream data the label set is byte-identical across rebuilds.

#### Scenario: A resolved claim names its aggregate node

- **WHEN** the response contains a PVC node whose volume resolved to aggregate `aggr1` on ONTAP cluster `ontap-prod`, served by SVM `svm_shop`
- **THEN** its `data.labels.aggr` equals `"netapp/ontap-prod/aggr/aggr1"`, its `data.labels.svm` equals `"svm_shop"`, the response contains the `netapp/ontap-prod/aggr/aggr1` node, and the PVC's `pvc-to-netapp-aggr` edge targets that node

#### Scenario: The same aggregate name on two filers

- **WHEN** two PVC nodes resolve to aggregates both named `aggr1`, one on ONTAP cluster `ontap-a` and one on `ontap-b`
- **THEN** their `data.labels.aggr` values are `"netapp/ontap-a/aggr/aggr1"` and `"netapp/ontap-b/aggr/aggr1"` respectively

#### Scenario: A FlexGroup claim carries svm but no aggr

- **WHEN** the response contains a PVC node whose matched `volume_labels` series carries `svm="svm_big"` and an empty `aggr`
- **THEN** its `data.labels.svm` equals `"svm_big"` and its `data.labels` has no `aggr` key

#### Scenario: No Harvest series, no aggr

- **WHEN** the upstream carries no `volume_labels` series for the window
- **THEN** no PVC node's `data.labels` has an `aggr` key
