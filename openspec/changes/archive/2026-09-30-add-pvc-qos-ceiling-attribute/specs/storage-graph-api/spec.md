# Spec Delta

## MODIFIED Requirements

### Requirement: Attributes and compound groups carry over

Every retained real node SHALL carry the same `data` attributes it carries in `/v1/graph` — including `ipaddress`, `owner`, `application`, `ready_status`, `health`, `usage`, `storageclass`, `qos`, `hardware`, `perf`, `alerts` and `status` — and the body SHALL include the same synthesised compound groups with the same `data.parent` rules (`cluster > namespace > application > controller > pod`, `cluster > namespace > [application >] pvc`, `cluster > node`, `storage-cluster > netapp-node > netapp-aggr`, `storage-cluster > netapp-svm`). Namespace and ArgoCD Application SHALL NOT be tiers of the flow; a consumer derives a namespace- or Application-level Sankey by walking `data.parent` and summing the conserved weights.

A PVC's `qos` attribute is independent of its flow: it SHALL be carried whenever the claim's ceiling resolved, including when the claim's `svm-pvc` edge carries no `metrics` (no flow and no latency to report) and when the claim enters the chain at the SVM (the FlexGroup shape). Whenever the `svm-pvc` edge carries `max_iops` / `max_bytes_per_sec`, the PVC's `data.qos` carries the same values.

A storage-graph pod SHALL NOT carry `data.containers`: the storage build does not read the container family (see "Storage build reads every family by reference"), and a storage-flow consumer has no use for a container list. This is the ONE attribute on which the two bodies differ for the same pod.

#### Scenario: Pod keeps its attributes and groups

- **WHEN** a retained pod carries `data.application="checkout"` and `data.owner={kind:"StatefulSet", name:"orders"}` in `/v1/graph`
- **THEN** the storage-graph body carries the same attributes on that pod and its `data.parent` names the same controller group, whose ancestry reaches `cluster/<cluster>`

#### Scenario: No namespace or application tier

- **WHEN** any storage-graph body is inspected
- **THEN** no `storage-flow` edge has a `namespace` or `application` group node as source or target

#### Scenario: Pod carries no containers

- **WHEN** a retained pod carries `data.containers` in `/v1/graph`
- **THEN** the storage-graph body's node for that pod has no `containers` field, while its `owner`, `application`, `status` and `data.parent` are unchanged

#### Scenario: PVC keeps its ceiling when its claim-level edge is unweighted

- **WHEN** a retained claim's ceiling resolved to policy group `gold-tier` with `max_iops: 5000`, and its `svm-pvc` edge carries no `metrics` because the claim reported neither flow nor latency in the window
- **THEN** the PVC node carries `data.qos: {"policy_group": "gold-tier", "max_iops": 5000}` and the `svm-pvc` edge has no `metrics` key

#### Scenario: FlexGroup PVC carries its ceiling in the storage graph

- **WHEN** a retained claim enters the chain at `netapp/ontap-prod/svm/svm_big` (no aggregate) and its ceiling resolved
- **THEN** the PVC node carries `data.qos` with the resolved fields, exactly as it does in `/v1/graph`
