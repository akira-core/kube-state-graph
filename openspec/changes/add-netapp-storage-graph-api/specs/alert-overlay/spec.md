## Purpose

Overlays the alerting store's active `ALERTS` onto the graph nodes their labels identify — pods, Kubernetes nodes, claims, NetApp controllers and aggregates — so a consumer sees a node's alert state on the node itself instead of cross-referencing an alert list.

## ADDED Requirements

### Requirement: ALERTS series as the alert source

The build SHALL read the OPTIONAL `ALERTS` series over the request window from the backend(s) serving the `alerts` query family, and SHALL treat an alert as **active in the window** when at least one sample with `alertstate="firing"` falls inside `[start, end]` (a `last_over_time` read with the fixed selector `alertstate="firing"` rendered first). `pending` alerts SHALL NOT be surfaced. The read is a single leg that degrades log-and-continue: a query error, an empty vector, or an `alerts` family served by no backend SHALL each leave every node without an `alerts` attribute and SHALL NOT fail the build. The leg SHALL run on every build, whichever endpoint or in-process call requested it.

#### Scenario: Firing alert in window is read

- **WHEN** `ALERTS{alertname="KubePodCrashLooping",alertstate="firing",namespace="shop",pod="orders-0",cluster="c1"}` has a sample inside the window
- **THEN** the alert is a candidate for matching

#### Scenario: Pending alert ignored

- **WHEN** the only `ALERTS` sample for an alert in the window carries `alertstate="pending"`
- **THEN** no node carries that alert

#### Scenario: Family unserved

- **WHEN** the routing table serves `alerts` on no backend
- **THEN** no `ALERTS` query is issued, no node carries `data.alerts`, and the build succeeds

#### Scenario: Query failure degrades

- **WHEN** the `ALERTS` query returns an error
- **THEN** the build succeeds with no `data.alerts` on any node and one Warn names the failed leg

### Requirement: ALERTS under request-scoped selectors

The `ALERTS` query SHALL carry the request's `az`, `env` and `namespace` dimensions as label matchers, composed after its fixed `alertstate="firing"` selector in the fixed order `az`, `env`, `namespace`, and SHALL be zone-routed by `az` like the `ksm` family. The `namespace` matcher SHALL be rendered in its or-absent form (`namespace=~"<a>|<b>|"`, the empty alternative last) so that an alert carrying NO `namespace` label — a Kubernetes node, ONTAP controller or aggregate alert, whose target node the request still loads by reference — is never excluded upstream; only namespaced alerts are narrowed. It SHALL NOT carry a `cluster` matcher under any request: an alert expression does not reliably preserve the `cluster` label, and a `?cluster=` request must not silently drop alerts. The `cluster` dimension reaches alerts at projection only, through the node the alert attached to. Operator precondition (documented, not assumed): the alerting store MUST stamp the same `az` / `env` external labels as the kube-state-metrics store, or its alerts vanish under an `az` / `env` filter.

#### Scenario: Zone, environment and namespace rendered

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={c1}`, `namespace={shop}`
- **THEN** the alerts query is issued as `last_over_time(ALERTS{alertstate="firing",<az-key>="zone-a",<env-key>="prod",namespace=~"shop|"}[<window>])` with no `cluster` matcher, and only to the `alerts` backends whose `zones` include `zone-a` or that are catch-alls

#### Scenario: Unfiltered build reads every alert

- **WHEN** a build runs with no selector dimension
- **THEN** the alerts query is issued as `last_over_time(ALERTS{alertstate="firing"}[<window>])`

### Requirement: Label-set matching to graph nodes

Each active alert SHALL be matched to at most one graph node by its label set, choosing the target kind by the most specific label present, in this order:

1. non-empty `namespace` and `pod` → the `type="pod"` node with that namespace and pod name;
2. else non-empty `namespace` and `persistentvolumeclaim` → the `type="pvc"` node with that namespace and claim name;
3. else non-empty `aggr` → the `type="netapp-aggr"` node with that name, in the ONTAP cluster the `cluster` label names (it outranks `node` because the stock Harvest `aggr_*` series carry the owning controller's `node` beside `aggr`, so an aggregate alert names both);
4. else non-empty `node` → a `type="node"` (Kubernetes) or `type="netapp-node"` (ONTAP controller) node with that name, disambiguated by `cluster` as below;
5. else unmatched.

When the alert carries a non-empty `cluster` label: for kinds 1–2 it SHALL be resolved through the same cluster-identity ladder as every Kubernetes series (compose, else adopt, else verbatim) and the match restricted to that identity; for kind 3 the raw label must name the ONTAP cluster; for kind 4, if the resolved identity holds a Kubernetes node of that name the alert matches the Kubernetes node, if the raw label equals an ONTAP cluster known to the Harvest join that holds a controller of that name it matches the controller, and if BOTH hold the alert SHALL be counted ambiguous and attached to neither. When the alert carries NO `cluster` label, the match SHALL succeed only if exactly one node of the eligible kind(s) in the loaded estate carries the remaining labels; several candidates SHALL be counted ambiguous and attached to none. Only pods, Kubernetes nodes, claims, NetApp controllers and NetApp aggregates SHALL ever carry alerts; a pod is matched by name against the pods loaded in the window, so an alert for a pod the build did not load is unmatched.

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

### Requirement: Node `alerts` attribute

Every node that matched at least one alert SHALL carry a typed `data.alerts` array of `{ name, state, severity }` objects — `name` the `alertname` label, `state` the `alertstate` label (`"firing"`), `severity` the `severity` label, omitted when empty — never inside `labels`. The array SHALL be de-duplicated on `(name, severity)` and sorted by `name` then `severity`, and SHALL be omitted entirely (never empty) from nodes with no matched alert, so an unalerted estate serialises byte-identically to one built before this capability existed. The attribute SHALL be resolved at build time onto the graph, so it is present on `GET /v1/graph`, `GET /v1/storage-graph`, and every in-process engine call alike.

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

### Requirement: Alert-matching observability

The build SHALL emit at most ONE aggregated Warn per build for unmatched alerts (`alerts_unmatched`, carrying the count) and ONE for ambiguous alerts (`alerts_ambiguous`, carrying the count), each only when its count is non-zero and only when the `ALERTS` vector was non-empty. Matching SHALL never fail the build.

#### Scenario: Unmatched counted once

- **WHEN** three active alerts name pods the build did not load and one is ambiguous
- **THEN** one `alerts_unmatched` Warn carries `3`, one `alerts_ambiguous` Warn carries `1`, and the build succeeds

#### Scenario: Full match is silent

- **WHEN** every active alert attaches to a node
- **THEN** neither Warn is emitted
