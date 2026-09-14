## ADDED Requirements

### Requirement: Each claim names its aggregate

A PVC node in the body SHALL carry the same `labels.aggr` it carries in `/v1/graph` (see "PVC `aggr` label" in `graph-api`): the node id of the aggregate its volume sits on, absent for a FlexGroup claim. It is the one place the body states a claim's aggregate. The `aggr-svm` tier is shared by every claim an SVM holds on that aggregate, so once an SVM spans more than one aggregate, walking up from a PVC through its SVM cannot tell which aggregate the claim sits on.

Whenever a PVC carrying `labels.aggr` is in the body, the node it names SHALL be in the same body: it lies on the claim's path (`netapp-node → netapp-aggr → netapp-svm → pvc`), which the storage-reachability projection retains whole, and it has an `aggr-svm` edge to the PVC's SVM. No `storage-flow` edge SHALL carry a label naming a claim's aggregate: the tier chain and the edge labels (`tier`, `attribution`) are unchanged.

#### Scenario: An SVM spanning two aggregates

- **WHEN** SVM `svm_shop` on ONTAP cluster `ontap-prod` holds claim `shop/orders-data` on `aggr1` and claim `shop/catalog-data` on `aggr2`, both mounted, and a client sends `?az=zone-a&env=prod&svm=svm_shop`
- **THEN** the body carries an `aggr-svm` edge from each aggregate to `svm_shop` and one `svm-pvc` edge to each PVC; the `shop/orders-data` PVC carries `labels.aggr="netapp/ontap-prod/aggr/aggr1"`, the `shop/catalog-data` PVC carries `labels.aggr="netapp/ontap-prod/aggr/aggr2"`, and both aggregate nodes are in the body

#### Scenario: A storage root keeps each claim's own aggregate

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1` and `svm_shop` also holds mounted claims on `aggr2`
- **THEN** every PVC in the body carries `labels.aggr="netapp/ontap-prod/aggr/aggr1"`, and none of `svm_shop`'s `aggr2` claims is retained

#### Scenario: A FlexGroup claim names no aggregate

- **WHEN** a retained claim's volume is a FlexGroup, so its path starts at its `svm-pvc` edge
- **THEN** its PVC carries `labels.svm` and no `labels.aggr`, and no `node-aggr` or `aggr-svm` edge is emitted for it

#### Scenario: No storage-flow edge names a claim's aggregate

- **WHEN** any storage-graph body is inspected
- **THEN** every `storage-flow` edge's `labels` holds only `tier` and, on a split `pvc-pod` edge, `attribution`
