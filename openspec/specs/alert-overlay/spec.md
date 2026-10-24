# alert-overlay Specification

## Purpose

Overlays the alerting store's active `ALERTS` onto the graph nodes their labels identify — pods, Kubernetes nodes, claims, NetApp controllers and aggregates — so a consumer sees a node's alert state on the node itself instead of cross-referencing an alert list.

## Requirements

### Requirement: ALERTS series as the alert source

The build SHALL read the OPTIONAL `ALERTS` series over the request window from the backend(s) serving the `alerts` query family, on every build whichever endpoint or in-process call requested it. An alert SHALL be **active in the window** when at least one sample with `alertstate="firing"` falls inside `[start, end]`; `pending` alerts SHALL NOT be surfaced.

#### Scenario: Active-in-window read

- **WHEN** a build reads alerts
- **THEN** it issues a single `last_over_time` read of `ALERTS` over the window, with the fixed selector `alertstate="firing"` rendered first
- **AND** the leg runs on every build, whether it serves `GET /v1/graph`, `GET /v1/storage-graph`, or an in-process engine call

#### Scenario: Firing alert in window is read

- **WHEN** `ALERTS{alertname="KubePodCrashLooping",alertstate="firing",namespace="shop",pod="orders-0",cluster="c1"}` has a sample inside the window
- **THEN** the alert is a candidate for matching

#### Scenario: Pending alert ignored

- **WHEN** the only `ALERTS` sample for an alert in the window carries `alertstate="pending"`
- **THEN** no node carries that alert

### Requirement: ALERTS leg degrades without failing the build

The `ALERTS` read SHALL be a single leg that degrades log-and-continue: a query error, an empty vector, or an `alerts` family served by no backend SHALL each leave every node without an `alerts` attribute and SHALL NOT fail the build.

#### Scenario: Family unserved

- **WHEN** the routing table serves `alerts` on no backend
- **THEN** no `ALERTS` query is issued, no node carries `data.alerts`, and the build succeeds

#### Scenario: Query failure degrades

- **WHEN** the `ALERTS` query returns an error
- **THEN** the build succeeds with no `data.alerts` on any node and one Warn names the failed leg

#### Scenario: Empty vector degrades

- **WHEN** the `ALERTS` query returns an empty vector
- **THEN** the build succeeds and no node carries an `alerts` attribute

### Requirement: ALERTS under request-scoped selectors

The `ALERTS` query SHALL carry the request's `az`, `env` and `namespace` dimensions as label matchers, composed after its fixed `alertstate="firing"` selector in the fixed order `az`, `env`, `namespace`, and SHALL be zone-routed by `az` like the `ksm` family.

#### Scenario: Zone, environment and namespace rendered

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={c1}`, `namespace={shop}`
- **THEN** the alerts query is issued as `last_over_time(ALERTS{alertstate="firing",<az-key>="zone-a",<env-key>="prod",namespace=~"shop|"}[<window>])` with no `cluster` matcher, and only to the `alerts` backends whose `zones` include `zone-a` or that are catch-alls

#### Scenario: Unfiltered build reads every alert

- **WHEN** a build runs with no selector dimension
- **THEN** the alerts query is issued as `last_over_time(ALERTS{alertstate="firing"}[<window>])`

### Requirement: ALERTS namespace matcher admits alerts without a namespace

The `namespace` matcher on the `ALERTS` query SHALL be rendered in its or-absent form, with the empty alternative last, so that an alert carrying NO `namespace` label is never excluded upstream and only namespaced alerts are narrowed.

#### Scenario: Or-absent namespace matcher

- **WHEN** a build selects namespaces `a` and `b`
- **THEN** the namespace matcher is rendered as `namespace=~"<a>|<b>|"`, the empty alternative last

#### Scenario: Un-namespaced alert survives a namespace filter

- **WHEN** a build selects a namespace and an active alert for a Kubernetes node, ONTAP controller or aggregate carries no `namespace` label
- **THEN** the alert is not excluded upstream, and it can still attach to its target node, which the request loads by reference

### Requirement: ALERTS never carries a cluster matcher

The `ALERTS` query SHALL NOT carry a `cluster` matcher under any request, because an alert expression does not reliably preserve the `cluster` label and a `?cluster=` request must not silently drop alerts. The `cluster` dimension SHALL reach alerts at projection only, through the node the alert attached to.

#### Scenario: Cluster filter applied at projection only

- **WHEN** a build runs with `cluster={c1}`
- **THEN** the alerts query carries no `cluster` matcher
- **AND** the `cluster` dimension reaches the alert only at projection, through the node the alert attached to

### Requirement: Alerting store stamps the zone labels

As an operator precondition — documented, not assumed — the alerting store MUST stamp the same `az` / `env` external labels as the kube-state-metrics store.

#### Scenario: Alerting store without zone labels

- **WHEN** the alerting store does not stamp the `az` / `env` external labels the kube-state-metrics store stamps, and a request carries an `az` / `env` filter
- **THEN** its alerts vanish from that request

### Requirement: Label-set matching to graph nodes

Each active alert SHALL be matched to at most one graph node by its label set, choosing the target kind by the most specific label present: pod, then claim, then NetApp aggregate, then node; an alert carrying none of these SHALL be unmatched. Only pods, Kubernetes nodes, claims, NetApp controllers and NetApp aggregates SHALL ever carry alerts.

#### Scenario: Target kind chosen by the most specific label

- **WHEN** an active alert is matched
- **THEN** a non-empty `namespace` and `pod` select the `type="pod"` node with that namespace and pod name
- **AND** else a non-empty `namespace` and `persistentvolumeclaim` select the `type="pvc"` node with that namespace and claim name
- **AND** else a non-empty `aggr` selects the `type="netapp-aggr"` node with that name, in the ONTAP cluster the `cluster` label names; it outranks `node` because the stock Harvest `aggr_*` series carry the owning controller's `node` beside `aggr`, so an aggregate alert names both
- **AND** else a non-empty `node` selects a `type="node"` (Kubernetes) or `type="netapp-node"` (ONTAP controller) node with that name, disambiguated by `cluster`
- **AND** else the alert is unmatched

#### Scenario: Pod alert attached

- **WHEN** an active alert carries `{cluster="c1", namespace="shop", pod="orders-0"}` and the build loaded that pod under identity `zone-a-prod-c1`
- **THEN** that pod node carries the alert and no other node does

#### Scenario: Claim alert attached

- **WHEN** an active alert carries `{cluster="c1", namespace="shop", persistentvolumeclaim="orders-data"}`
- **THEN** the PVC node `zone-a-prod-c1/shop/orders-data` carries the alert

#### Scenario: Alert for an unloaded pod

- **WHEN** an active alert names a pod the build did not load in the window
- **THEN** the alert is unmatched, because a pod is matched by name against the pods loaded in the window

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

### Requirement: Cluster-qualified alert matching

When an alert carries a non-empty `cluster` label, the match SHALL be restricted by it according to the target kind. A node-shaped alert whose `cluster` and `node` name both a Kubernetes node and an ONTAP controller SHALL be counted ambiguous and attached to neither.

#### Scenario: Cluster label per target kind

- **WHEN** an active alert carries a non-empty `cluster` label
- **THEN** for a pod or claim target the label is resolved through the same cluster-identity ladder as every Kubernetes series (compose, else adopt, else verbatim) and the match is restricted to that identity
- **AND** for an aggregate target the raw label must name the ONTAP cluster
- **AND** for a node target, the alert matches the Kubernetes node if the resolved identity holds a Kubernetes node of that name, and matches the controller if the raw label equals an ONTAP cluster known to the Harvest join that holds a controller of that name

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

### Requirement: Alerts without a cluster label match by uniqueness

When an alert carries NO `cluster` label, the match SHALL succeed only if exactly one node of the eligible kind(s) in the loaded estate carries the remaining labels; several candidates SHALL be counted ambiguous and attached to none.

#### Scenario: Missing cluster resolved by uniqueness

- **WHEN** an active alert carries `{namespace="shop", pod="orders-0"}` with no `cluster` label and exactly one loaded pod matches
- **THEN** that pod carries the alert

#### Scenario: Missing cluster with several candidates

- **WHEN** an active alert carries `{namespace="shop", pod="orders-0"}` with no `cluster` label and two clusters in the estate each hold such a pod
- **THEN** no node carries the alert and it is counted ambiguous

### Requirement: Zone agreement between an alert and its candidate

A candidate whose **zone** is KNOWN and does not agree with the alert's zone SHALL NOT match that alert. An alert with no zone, and a candidate whose zone is unknown, SHALL never be excluded by this rule; matching then reduces to the label comparison alone. The rule SHALL apply on every build path.

#### Scenario: Zone definitions

- **WHEN** an alert's zone is compared with a candidate's
- **THEN** an alert's zone is the pair of its configured `az` and `env` label values, and exists only when both are non-empty
- **AND** a pod's, claim's or Kubernetes node's zone is the `az` / `env` its cluster identity was composed from, and is unknown when the identity composed from no pair
- **AND** a NetApp controller's or aggregate's **zone set** is every `az` / `env` pair carried by a Harvest series this build read that names its ONTAP cluster — `volume_labels`, the `aggr_*` gauges and the controller families — counting only series that carry both labels, and is empty (unknown) when no such series carried the pair

#### Scenario: Zone rule on the cluster-qualified path

- **WHEN** an aggregate alert or the controller side of a node alert carries a `cluster` label
- **THEN** a candidate whose zone set does not contain the alert's zone is not a match
- **AND** the Kubernetes pod, claim and node candidates need no extra check there, being already zone-exact because the identity they are keyed on is composed from the alert's own `az` / `env`

#### Scenario: Zone rule on the no-cluster path

- **WHEN** an alert carries no `cluster` label
- **THEN** every candidate whose zone is known and different is removed BEFORE uniqueness is tested, so a same-named object in another zone neither absorbs the alert nor makes it ambiguous

#### Scenario: Zone rule on every build path

- **WHEN** one build holds same-named objects or alerts from several zones — an unfiltered `GET /v1/graph`, or a build whose alerting or Harvest backend is a catch-all
- **THEN** the zone agreement rule applies to every match

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

### Requirement: Node `alerts` attribute

Every node that matched at least one alert SHALL carry a typed `data.alerts` array of `{ name, state, severity }` objects, never inside `labels`, de-duplicated on `(name, severity)` and sorted by `name` then `severity`. The array SHALL be omitted entirely (never empty) from a node with no matched alert, and SHALL be resolved at build time onto the graph, so every endpoint and in-process engine call carries it alike.

#### Scenario: Entry fields

- **WHEN** a node carries an alert entry
- **THEN** `name` is the `alertname` label, `state` the `alertstate` label (`"firing"`), and `severity` the `severity` label, omitted when empty

#### Scenario: Attribute shape

- **WHEN** a pod matched alerts `KubePodCrashLooping` (severity `warning`) and `HighMemory` (severity `critical`)
- **THEN** the pod's `data.alerts` equals `[{"name":"HighMemory","state":"firing","severity":"critical"},{"name":"KubePodCrashLooping","state":"firing","severity":"warning"}]`

#### Scenario: Duplicate series collapse

- **WHEN** two `ALERTS` series for the same alert differ only in a label the matcher does not read
- **THEN** the node's `data.alerts` lists that `(name, severity)` once

#### Scenario: Absent on unalerted nodes

- **WHEN** a node matched no alert
- **THEN** its `data` has no `alerts` key

#### Scenario: Present on both endpoints

- **WHEN** a pod carries an alert and is retained by both `GET /v1/graph` and `GET /v1/storage-graph`
- **THEN** both bodies carry the identical `data.alerts` array on that pod
- **AND** an in-process engine call building the same graph carries it too

### Requirement: Alert severity is the only alert input to node status

An alert entry's `severity` SHALL be the ONLY alert field that feeds a node's `status`; `name` and `state` SHALL NOT.

#### Scenario: Status fold reads severity only

- **WHEN** a node's `data.status` (graph-api "Node `status` attribute") is folded
- **THEN** each matched alert contributes its `severity` and nothing else

#### Scenario: Unalerted node status

- **WHEN** a node matched no alert
- **THEN** its `data.status` is folded from its other signals alone

### Requirement: Alert-matching observability

The build SHALL emit at most ONE aggregated Warn per build for unmatched alerts (`alerts_unmatched`, carrying the count) and ONE for ambiguous alerts (`alerts_ambiguous`, carrying the count), each only when its count is non-zero and only when the `ALERTS` vector was non-empty. Matching SHALL never fail the build.

#### Scenario: Unmatched counted once

- **WHEN** three active alerts name pods the build did not load and one is ambiguous
- **THEN** one `alerts_unmatched` Warn carries `3`, one `alerts_ambiguous` Warn carries `1`, and the build succeeds

#### Scenario: Full match is silent

- **WHEN** every active alert attaches to a node
- **THEN** neither Warn is emitted
