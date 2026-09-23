## MODIFIED Requirements

### Requirement: Label-set matching to graph nodes

Each active alert SHALL be matched to at most one graph node by its label set, choosing the target kind by the most specific label present, in this order:

1. non-empty `namespace` and `pod` → the `type="pod"` node with that namespace and pod name;
2. else non-empty `namespace` and `persistentvolumeclaim` → the `type="pvc"` node with that namespace and claim name;
3. else non-empty `aggr` → the `type="netapp-aggr"` node with that name, in the ONTAP cluster the `cluster` label names (it outranks `node` because the stock Harvest `aggr_*` series carry the owning controller's `node` beside `aggr`, so an aggregate alert names both);
4. else non-empty `node` → a `type="node"` (Kubernetes) or `type="netapp-node"` (ONTAP controller) node with that name, disambiguated by `cluster` as below;
5. else unmatched.

When the alert carries a non-empty `cluster` label: for kinds 1–2 it SHALL be resolved through the same cluster-identity ladder as every Kubernetes series (compose, else adopt, else verbatim) and the match restricted to that identity; for kind 3 the raw label must name the ONTAP cluster; for kind 4, if the resolved identity holds a Kubernetes node of that name the alert matches the Kubernetes node, if the raw label equals an ONTAP cluster known to the Harvest join that holds a controller of that name it matches the controller, and if BOTH hold the alert SHALL be counted ambiguous and attached to neither. When the alert carries NO `cluster` label, the match SHALL succeed only if exactly one node of the eligible kind(s) in the loaded estate carries the remaining labels; several candidates SHALL be counted ambiguous and attached to none. Only pods, Kubernetes nodes, claims, NetApp controllers and NetApp aggregates SHALL ever carry alerts; a pod is matched by name against the pods loaded in the window, so an alert for a pod the build did not load is unmatched.

**Zone agreement.** An alert's **zone** is the pair of its configured `az` and `env` label values, and exists only when both are non-empty. A pod's, claim's or Kubernetes node's zone is the `az` / `env` its cluster identity was composed from, and is unknown when the identity composed from no pair. A NetApp controller's or aggregate's **zone set** is every `az` / `env` pair carried by a Harvest series this build read that names its ONTAP cluster — `volume_labels`, the `aggr_*` gauges and the controller families — counting only series that carry both labels; it is empty (unknown) when no such series carried the pair. A candidate whose zone is KNOWN and does not agree with the alert's zone SHALL NOT match that alert:

- on the cluster-qualified path of kind 3 and of kind 4's controller side, a candidate whose zone set does not contain the alert's zone is not a match (the Kubernetes candidates of kinds 1, 2 and 4 are already zone-exact there, because the identity they are keyed on is composed from the alert's own `az` / `env`);
- on the no-`cluster` path, every candidate whose zone is known and different is removed BEFORE uniqueness is tested, so a same-named object in another zone neither absorbs the alert nor makes it ambiguous.

An alert with no zone, and a candidate whose zone is unknown, SHALL never be excluded by this rule — matching then reduces to the label comparison above, which is what an estate whose Harvest series carry no `az` / `env` pair observes. The rule applies on every build path; it is what keeps a zone's alerts off another zone's same-named NetApp entity when a build reads alerts from zones whose Harvest stores it did not read, as a hub-mode storage build does.

#### Scenario: Pod alert attached

- **WHEN** an active alert carries `{cluster="c1", namespace="shop", pod="orders-0"}` and the build loaded that pod under identity `zone-a-prod-c1`
- **THEN** that pod node carries the alert and no other node does

#### Scenario: Claim alert attached

- **WHEN** an active alert carries `{cluster="c1", namespace="shop", persistentvolumeclaim="orders-data"}`
- **THEN** the PVC node `zone-a-prod-c1/shop/orders-data` carries the alert

#### Scenario: Kubernetes node alert attached

- **WHEN** an active alert carries `{cluster="c1", node="worker-1"}` and `c1` resolves to an identity holding node `worker-1` and names no ONTAP cluster
- **THEN** the `type="node"` node `zone-a-prod-c1/worker-1` carries the alert

#### Scenario: ONTAP controller alert attached

- **WHEN** an active alert carries `{cluster="ontap-prod", node="ontap-prod-01"}`, `ontap-prod` is an ONTAP cluster the Harvest join materialised, and no Kubernetes identity has raw name `ontap-prod`
- **THEN** the `type="netapp-node"` node `netapp/ontap-prod/ontap-prod-01` carries the alert

#### Scenario: Aggregate alert attached

- **WHEN** an active alert carries `{cluster="ontap-prod", aggr="aggr1"}` and `netapp/ontap-prod/aggr/aggr1` is materialised
- **THEN** that aggregate node carries the alert

#### Scenario: Node-shaped alert ambiguous across kinds

- **WHEN** an active alert carries `{cluster="x", node="n1"}`, a Kubernetes identity with raw name `x` holds node `n1`, AND an ONTAP cluster `x` holds controller `n1`
- **THEN** no node carries the alert and it is counted ambiguous

#### Scenario: Missing cluster resolved by uniqueness

- **WHEN** an active alert carries `{namespace="shop", pod="orders-0"}` with no `cluster` label and exactly one loaded pod matches
- **THEN** that pod carries the alert

#### Scenario: Missing cluster with several candidates

- **WHEN** an active alert carries `{namespace="shop", pod="orders-0"}` with no `cluster` label and two clusters in the estate each hold such a pod
- **THEN** no node carries the alert and it is counted ambiguous

#### Scenario: Aggregate alert outranks node label

- **WHEN** an active alert carries `{cluster="ontap-prod", node="ontap-prod-01", aggr="aggr1"}` (the label set a rule over the `aggr_*` series inherits)
- **THEN** only `netapp/ontap-prod/aggr/aggr1` carries the alert; the controller does not

#### Scenario: Pod alert outranks node label

- **WHEN** an active alert carries `{cluster="c1", namespace="shop", pod="orders-0", node="worker-1"}`
- **THEN** only the pod carries the alert; the node does not

#### Scenario: Non-firing series discarded by the reader

- **WHEN** a hand-built vector reaching the reader carries `{alertstate="pending", namespace="shop", pod="orders-0"}`
- **THEN** no node carries it and it is counted neither unmatched nor ambiguous (the reader mirrors the query's fixed `alertstate="firing"` selector)

#### Scenario: Unmatchable label set

- **WHEN** an active alert carries only `{cluster="c1", alertname="TargetDown"}`
- **THEN** no node carries it and it is counted unmatched

#### Scenario: Aggregate alert from its own zone attached

- **WHEN** the Harvest series naming ONTAP cluster `ontap-prod` carry `<az-key>="zone-a"`, `<env-key>="prod"` and an active alert carries `{<az-key>="zone-a", <env-key>="prod", cluster="ontap-prod", aggr="aggr1"}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` carries the alert

#### Scenario: Aggregate alert from another zone not attached

- **WHEN** the Harvest series naming ONTAP cluster `ontap-prod` carry `<az-key>="zone-a"`, `<env-key>="prod"` and an active alert carries `{<az-key>="zone-b", <env-key>="prod", cluster="ontap-prod", aggr="aggr1"}`
- **THEN** no node carries the alert and it is counted unmatched

#### Scenario: Controller alert from another zone not attached

- **WHEN** the Harvest series naming ONTAP cluster `ontap-prod` carry `<az-key>="zone-a"`, `<env-key>="prod"`, no Kubernetes identity has raw name `ontap-prod`, and an active alert carries `{<az-key>="zone-b", <env-key>="prod", cluster="ontap-prod", node="ontap-prod-01"}`
- **THEN** no node carries the alert and it is counted unmatched

#### Scenario: Harvest without a zone falls back to the label comparison

- **WHEN** no Harvest series naming ONTAP cluster `ontap-prod` carries both `<az-key>` and `<env-key>`, and an active alert carries `{<az-key>="zone-b", <env-key>="prod", cluster="ontap-prod", aggr="aggr1"}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` carries the alert

#### Scenario: Missing cluster disambiguated by zone

- **WHEN** an active alert carries `{<az-key>="zone-a", <env-key>="prod", namespace="shop", pod="orders-0"}` with no `cluster` label, and the build loaded pod `shop/orders-0` under identity `zone-a-prod-c1` and another under `zone-b-prod-c1`
- **THEN** only the `zone-a-prod-c1` pod carries the alert and it is counted neither unmatched nor ambiguous

#### Scenario: Missing cluster, only another zone holds the object

- **WHEN** an active alert carries `{<az-key>="zone-b", <env-key>="prod", aggr="aggr1"}` with no `cluster` label, and the only loaded `aggr1` is on an ONTAP cluster whose Harvest series carry only `<az-key>="zone-a"`, `<env-key>="prod"`
- **THEN** no node carries the alert and it is counted unmatched
