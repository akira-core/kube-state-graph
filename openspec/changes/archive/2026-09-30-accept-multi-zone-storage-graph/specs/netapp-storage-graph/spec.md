# Spec Delta

## MODIFIED Requirements

### Requirement: PVC-to-NetApp-aggregate edge join

For every PVC entity whose resolved PV name (`volumename`) is non-empty, the builder SHALL derive that PV name into a match token and match it against the `volume` label of the Harvest `volume_labels` series by the suffix comparison of "Suffix-only PV-name-to-FlexVol-name derivation". On a match with a **non-empty `aggr` label** it SHALL emit one directed `pvc-to-netapp-aggr` edge from the PVC node to the NetApp aggregate node `netapp/<ontap-cluster>/aggr/<aggr>` derived from the same matched series' `cluster` and `aggr` labels — no separate topology query is issued. The edge is a pure function of this one family: whether it is drawn SHALL NOT depend on the QoS families, which only decide what it carries. The join is rooted at the PV name alone (CSI-provisioned PV names are UUID-derived, so cross-cluster collisions are not a practical concern).

**Zone agreement.** A claim's **candidate set** — the matched `volume_labels` series every pick of this capability runs over (the aggregate, the SVM, the owning controller the edge's target names, the QoS scope and the ceiling key) — SHALL contain only series whose zone agrees with the claim's. A claim's zone is the `(az, env)` pair its `kube_persistentvolumeclaim_info` series carries under the configured label keys; a series' zone is the pair it carries likewise. A matched series SHALL be excluded from the candidate set only when BOTH sides carry a complete pair and the pairs differ — an unknown zone on either side never excludes, the rule the `alert-overlay` capability applies to alerts. The exclusion narrows a claim's candidates only: the excluded series still names its aggregate, controller and SVM for the owner vote and the storage-flow inventory. A token collision across zones — the same FlexVol name, or a clone, on filers of two zones — therefore never moves a claim onto another zone's filer, which is what makes a multi-zone `/v1/storage-graph` body the union of its single-zone bodies; on an unfiltered `/v1/graph` it changes the body only in an estate holding such a collision.

The edge SHALL carry empty `labels` (`{}`), a deterministic UUIDv5 `id` (canonical input `<type>|<source>|<target>`), and SHALL de-duplicate by `(pvc, netapp-aggr)`. When matched series disagree on the containing aggregate for one claim — including when several distinct FlexVol names end with the token (the same FlexVol name on a second filer, or a name carrying a different prefix) — the builder SHALL pick deterministically the lexically-smallest `(ontap-cluster, aggr)` pair, so the emitted edge set is byte-stable across rebuilds, independent of vector order. A PVC with no resolved `volumename` SHALL emit no `pvc-to-netapp-aggr` edge. A matched series whose `aggr` label is **empty** (the FlexGroup shape) SHALL emit no edge and SHALL be counted by the join-coverage signal.

#### Scenario: Joined claim emits the edge

- **WHEN** PVC `cluster-alpha/db/data-mongo-0` resolves `volumename="pvc-9f3a"` and a `volume_labels` series carries `volume="trident_pvc_9f3a"`, `cluster="ontap-prod"`, `node="ontap-prod-01"`, `aggr="aggr1"`
- **THEN** the graph contains a directed `pvc-to-netapp-aggr` edge from `cluster-alpha/db/data-mongo-0` to `netapp/ontap-prod/aggr/aggr1` with empty `labels`

#### Scenario: PVC without a PV name emits no edge

- **WHEN** a PVC entity has no resolved `volumename`
- **THEN** no `pvc-to-netapp-aggr` edge originates from it and the build does not fail

#### Scenario: Matched series with an empty aggr label emits no edge

- **WHEN** a claim's derived token matches a `volume_labels` series whose `aggr` label is empty (a FlexGroup volume spanning aggregates)
- **THEN** no `pvc-to-netapp-aggr` edge is emitted for that claim, the claim is counted by the join-coverage signal, and the build does not fail

#### Scenario: Deterministic pick on conflicting aggregates

- **WHEN** two series matched by one claim's token report `(ontap-prod, aggr-b)` and `(ontap-prod, aggr-a)`
- **THEN** the edge targets `netapp/ontap-prod/aggr/aggr-a` (the lexically-smallest pair) deterministically across rebuilds

#### Scenario: Another zone's colliding FlexVol is never a candidate

- **WHEN** claim `zone-a-prod-c1/shop/orders-data` (its claim-info series carrying `az="zone-a",env="prod"`) resolves `volumename="pvc-9f3a"`, and two `volume_labels` series end with its token: `(ontap-a, aggr-z)` carrying `az="zone-a",env="prod"` and `(ontap-0, aggr-a)` carrying `az="zone-b",env="prod"`
- **THEN** the edge targets `netapp/ontap-a/aggr/aggr-z` although `(ontap-0, aggr-a)` sorts first, and the claim's `svm` label, QoS measurement and ceiling all come from the `ontap-a` series

#### Scenario: An unknown zone never excludes

- **WHEN** the same claim's only other matching series carries no `az` label
- **THEN** that series stays in the candidate set and the lexically-smallest pair is picked across both, exactly as before this rule

#### Scenario: Edge id stable across rebuilds

- **WHEN** the same `(pvc, netapp-aggr)` join is produced by two consecutive builds for the same window
- **THEN** the edge `id` (UUIDv5 over `<type>|<source>|<target>`) is byte-identical between the two builds

### Requirement: Harvest legs carry the request zone and environment

Every NetApp Harvest query the builder issues — `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, and `node_total_data` — SHALL carry the request's `az` and `env` matchers, in that order, ahead of any restriction of its own, and SHALL NEVER carry a `cluster` or `namespace` matcher. Those two are the only selector-level dimensions that reach Harvest. That does not forbid a scope derived from upstream data or from the request's ROOT, which is a different mechanism: on `/v1/graph` one Harvest family carries one — the `qos_*` families' `volume` alternation below — and on `/v1/storage-graph` EVERY Harvest family carries one, because that build reads each by reference (`storage-graph-api`, "Storage build reads every family by reference"). No such scope appears in the request-scoped selector table, and each is composed AFTER the `az` / `env` matchers. Harvest's `cluster` label is the **ONTAP** cluster name and never a Kubernetes cluster, so a Kubernetes `cluster` value pushed into it would match nothing; Harvest carries no `namespace` at all. **Operator precondition:** every Harvest series MUST carry the configured `az` / `env` labels (the ones kube-state-metrics carries); a series without them matches nothing under any `az` / `env` value — and `/v1/storage-graph` always carries both — so its filer silently drops out of every filtered body.

The `qos_*` families carry one selector and no other: the `volume` alternation of the scoped-read requirement. That alternation is **derived from upstream data, not from the request**: its values are FlexVol names the volume-label family already returned. It is nonetheless *influenced* by the request, because the claims whose tokens produced those names are themselves loaded under the request's selectors. This is the "narrowed by reference" principle of this capability realised at the query layer rather than only in the parse: a `cluster`, `namespace`, `az` or `env` filter reaches the QoS read solely through the claims it loads, never as a matcher on a Harvest label. On `/v1/graph`, `volume_labels` and every other Harvest query carry nothing beyond `az` / `env`. On `/v1/storage-graph`, `volume_labels` carries the root restriction (by ONTAP cluster, aggregate, SVM or controller), the token restriction of the tracked claims, or the whole-aggregate restriction of owner completion; the aggregate gauge families carry the `(ONTAP cluster, aggregate)` pairs the build reached or the root names; the controller families carry the `(ONTAP cluster, controller)` pairs the build reached or the root names; and the two fixed-policy families carry the `(ONTAP cluster, SVM)` pairs of the tracked claims — the same "narrowed by reference" principle, read from whichever end of the chain the root sits on.

These queries constitute the `harvest` query family of the `upstream-backend-routing` capability, so they MAY be served by a different upstream installation from the kube-state-metrics and kubelet legs. The family is **zone-routed**: a request's `az` values select which `harvest` backends are asked — those whose `zones` intersect the request, plus any catch-all — under the same rule as the `ksm` and `kubelet` families, and, like them, the zone is ALSO rendered as a matcher, so a catch-all backend or a store holding several zones returns the requested zone's series only. The `env` dimension has no routing counterpart and reaches Harvest as a matcher alone. Routing changes **which** installation answers a Harvest query; it changes neither the query string, the three-hop join, nor the per-hop degradation below.

Within a filtered build the storage chain is therefore narrowed **by reference**: an aggregate and its owning controller materialise only when a **loaded** claim's derived token matches a `volume_labels` series (or, in the storage-flow graph, when selected as a root), so a `cluster` or `namespace` filter reaches the NetApp graph solely through the claims it loads, and an `az` or `env` filter reaches it through the claims it loads plus its own matcher on every Harvest query (and, for `az`, the backends it selects). A filer shared across clusters, zones, or environments is one node set, reached from whichever loaded claims match it. In a `/v1/storage-graph` build rooted at a storage component (`storage-graph-api`, "Every root kind is tracked from its own tier to its claims") the direction reverses: the claims are loaded FROM the rooted volume-label rows, under the request's `az`, `env`, `cluster` and `namespace` matchers and from the backends the request's `az` selects, so a rooted FlexVol whose claim lives in another zone or environment loads no claim. An unfiltered build, and a `/v1/storage-graph` build selecting several zones or environments, reads the Harvest series of every selected zone; there, a claim's candidates are restricted to its own zone ("PVC-to-NetApp-aggregate edge join"), so a FlexVol name carried by volumes in two zones or environments resolves, for each claim, among its own zone's series — falling back to the lexically-smallest `(ontap_cluster, aggr)` over every candidate only when a side carries no zone.

#### Scenario: Cluster and namespace filters never reach Harvest

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** no Harvest query carries a `cluster` or `namespace` matcher; the request carries neither `az` nor `env` and no storage-side root, so the `volume_labels`, aggregate, controller, hardware, performance and policy queries are issued exactly as in an unfiltered build; the QoS queries restrict `volume` to the FlexVol names matched by the loaded `cluster-alpha` / `shop` claims alone; and the aggregates in the response are exactly those those claims matched

#### Scenario: Zone filter reaches Harvest

- **WHEN** two backends serve `harvest`, declaring `zones: [zone-a]` and `zones: [zone-b]`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query — including `node_labels` and the four `system_node` counters — is issued only to the `zone-a` backend, carrying `<az-key>="zone-a"`; a `volume_labels` series held by the `zone-b` backend is not loaded even if a loaded claim's derived token would match it

#### Scenario: Catch-all Harvest backend under a zone filter

- **WHEN** one backend serves `harvest` with no `zones` declared and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued to that backend carrying `<az-key>="zone-a"`, only `zone-a`'s `volume_labels` series are loaded, and the aggregates in the response are exactly those matched by the loaded `zone-a` claims

#### Scenario: Environment filter reaches Harvest

- **WHEN** a build runs with `env={prod}` against a single `harvest` backend holding series stamped `env="prod"` and `env="dev"`
- **THEN** every Harvest query carries `<env-key>="prod"`, only the `prod` series are loaded, and a loaded `prod` claim whose derived token matches only a `volume_labels` series stamped `env="dev"` receives no `pvc-to-netapp-aggr` edge

#### Scenario: Shared filer reached by reference from either filtered cluster

- **WHEN** claims in `cluster-alpha` and `cluster-beta` both match `netapp/ontap-prod/aggr/aggr1` and a build runs with `cluster={cluster-alpha}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` are materialised with a `pvc-to-netapp-aggr` edge from the `cluster-alpha` claim only; a `cluster={cluster-beta}` build materialises the same two nodes from the `cluster-beta` claim

#### Scenario: Harvest lacking the zone labels under a filter

- **WHEN** the kube-state-metrics series carry `az="zone-a"`, the Harvest series carry no `az` and no `env` label at all, and a build runs with `az={zone-a}` or `env={prod}`
- **THEN** every Harvest leg returns no rows, no `pvc-to-netapp-aggr` edge is drawn, and the build succeeds — the operator precondition above is what prevents this; the selector-coverage Warn does not name a Harvest family, because an empty Harvest read is also the normal state of a deployment without NetApp storage

#### Scenario: Harvest served by its own upstream

- **WHEN** the routing table declares one backend serving `harvest` at `http://vm-netapp.example:8428` and another serving every other family at `http://vm-k8s.example:8428`
- **THEN** every Harvest query — including each chunk of the scoped QoS read and the five node legs — is issued only to `http://vm-netapp.example:8428`, the kube-state-metrics and kubelet queries only to `http://vm-k8s.example:8428`, and the resulting `pvc-to-netapp-aggr` edges join claims read from one upstream to volumes read from the other

#### Scenario: Unfiltered build merges Harvest across backends

- **WHEN** two backends serve `harvest` for different zones and a build runs with no `az` value
- **THEN** every Harvest query is issued to both and the results are merged, so aggregates from both zones can match their claims in one graph

#### Scenario: Hub mode loads claims from the rooted rows

- **WHEN** a `/v1/storage-graph` build rooted at `ontap_cluster=ontap-prod` runs with `az={zone-a}`, `env={prod}`, and the `zone-a` `harvest` backend returns two rooted FlexVols, one whose claim lives in a `zone-a` / `prod` cluster and one whose claim lives in an `env="dev"` cluster
- **THEN** the `prod` claim is loaded and receives its aggregate and SVM exactly as an unfiltered build would give them, and the `dev` claim is not loaded, because every claim query carries `<env-key>="prod"`

