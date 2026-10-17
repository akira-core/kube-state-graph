# Spec Delta

## ADDED Requirements

### Requirement: Harvest legs are zone-routed and carry no request matcher

Every NetApp Harvest query the builder issues — `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, and `node_total_data` — SHALL carry NO request-scoped matcher: none of `az`, `env`, `cluster` or `namespace` reaches a Harvest query as a label matcher. That does not forbid a scope derived from upstream data or from the request's ROOT, which is a different mechanism: on `/v1/graph` one Harvest family carries one — the `qos_*` families' `volume` alternation below — and on `/v1/storage-graph` EVERY Harvest family carries one, because that build reads each by reference (`storage-graph-api`, "Storage build reads every family by reference"). No such scope appears in the request-scoped selector table. Harvest's `cluster` label is the **ONTAP** cluster name and never a Kubernetes cluster, so a Kubernetes `cluster` value pushed into it would match nothing; Harvest carries no `namespace` at all. A Harvest series is NOT required to carry the configured `az` / `env` labels: a series without them is read like any other, and a series that carries them is used for zone preference ("PVC-to-NetApp-aggregate edge join", "Harvest QoS workload series as the I/O source") and for the zone-agreeing alert match, never for exclusion at the query.

The `qos_*` families carry one selector and no other: the `volume` alternation of the scoped-read requirement. That alternation is **derived from upstream data, not from the request**: its values are FlexVol names the volume-label family already returned. It is nonetheless *influenced* by the request, because the claims whose tokens produced those names are themselves loaded under the request's selectors. This is the "narrowed by reference" principle of this capability realised at the query layer rather than only in the parse: a `cluster`, `namespace`, `az` or `env` filter reaches the QoS read solely through the claims it loads, never as a matcher on a Harvest label. On `/v1/graph`, `volume_labels` and every other Harvest query are issued exactly as in an unfiltered build. On `/v1/storage-graph`, `volume_labels` carries the root restriction (by ONTAP cluster, aggregate, SVM or controller), the token restriction of the tracked claims, or the whole-aggregate restriction of owner completion; the aggregate gauge families carry the `(ONTAP cluster, aggregate)` pairs the build reached or the root names; the controller families carry the `(ONTAP cluster, controller)` pairs the build reached or the root names; and the two fixed-policy families carry the `(ONTAP cluster, SVM)` pairs of the tracked claims — the same "narrowed by reference" principle, read from whichever end of the chain the root sits on.

These queries constitute the `harvest` query family of the `upstream-backend-routing` capability, so they MAY be served by a different upstream installation from the kube-state-metrics and kubelet legs. The family is **zone-routed but unmatched**: a request's `az` values select which `harvest` backends are asked — those whose `zones` intersect the request, plus any catch-all — under the same rule as the `ksm` and `kubelet` families, but the zone is NOT also rendered as a matcher: the store boundary is the zone filter, and a catch-all backend or a store holding several zones returns every zone's series. The `env` dimension has neither routing nor a matcher counterpart on Harvest. Routing changes **which** installation answers a Harvest query; it changes neither the query string, the three-hop join, nor the per-hop degradation below.

Within a filtered build the storage chain is therefore narrowed **by reference**: an aggregate and its owning controller materialise only when a **loaded** claim's derived token matches a `volume_labels` series (or, in the storage-flow graph, when selected as a root), so `cluster`, `namespace`, `az` and `env` filters reach the NetApp graph solely through the claims they load (and, for `az`, the backends it selects). A filer shared across clusters, zones, or environments is one node set, reached from whichever loaded claims match it. In a `/v1/storage-graph` build rooted at a storage component (`storage-graph-api`, "Every root kind is tracked from its own tier to its claims") the direction reverses: the claims are loaded FROM the rooted volume-label rows, under the request's `az`, `env`, `cluster` and `namespace` matchers on the Kubernetes side, so a rooted FlexVol whose claim lives in another zone or environment loads no claim. An unfiltered build, and a `/v1/storage-graph` build selecting several zones or environments, reads the Harvest series of every backend selected; there, and in a catch-all store, a claim's candidates prefer its own zone ("PVC-to-NetApp-aggregate edge join"), so a FlexVol name carried by volumes in two zones or environments resolves, for each claim, among its own zone's series — falling back to the lexically-smallest `(ontap_cluster, aggr)` over every candidate when no series agrees with the claim's zone or a side carries no zone.

#### Scenario: Cluster and namespace filters never reach Harvest

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** no Harvest query carries a `cluster` or `namespace` matcher; the request carries neither `az` nor `env` and no storage-side root, so the `volume_labels`, aggregate, controller, hardware, performance and policy queries are issued exactly as in an unfiltered build; the QoS queries restrict `volume` to the FlexVol names matched by the loaded `cluster-alpha` / `shop` claims alone; and the aggregates in the response are exactly those those claims matched

#### Scenario: Zone filter routes Harvest without a matcher

- **WHEN** two backends serve `harvest`, declaring `zones: [zone-a]` and `zones: [zone-b]`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query — including `node_labels` and the four `system_node` counters — is issued only to the `zone-a` backend as the same query string an unfiltered build issues, carrying no `<az-key>` matcher; a `volume_labels` series held by the `zone-b` backend is not loaded even if a loaded claim's derived token would match it

#### Scenario: Catch-all Harvest backend under a zone filter

- **WHEN** one backend serves `harvest` with no `zones` declared, holds `volume_labels` series stamped `az="zone-a"` and `az="zone-b"`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued to that backend with no `az` matcher, series of both zones are returned, and the aggregates in the response are exactly those matched by the loaded `zone-a` claims — a `zone-a` claim resolves among the `zone-a` series when it has one

#### Scenario: Environment filter does not reach Harvest

- **WHEN** a build runs with `env={prod}` against a single `harvest` backend holding series stamped `env="prod"` and `env="dev"`
- **THEN** no Harvest query carries an `<env-key>` matcher, series of both environments are returned, and a loaded `prod` claim whose derived token matches only a `volume_labels` series stamped `env="dev"` receives its `pvc-to-netapp-aggr` edge to that series' aggregate

#### Scenario: Shared filer reached by reference from either filtered cluster

- **WHEN** claims in `cluster-alpha` and `cluster-beta` both match `netapp/ontap-prod/aggr/aggr1` and a build runs with `cluster={cluster-alpha}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` are materialised with a `pvc-to-netapp-aggr` edge from the `cluster-alpha` claim only; a `cluster={cluster-beta}` build materialises the same two nodes from the `cluster-beta` claim

#### Scenario: Harvest lacking the zone labels under a filter

- **WHEN** the kube-state-metrics series carry `az="zone-a"`, the Harvest series carry no `az` and no `env` label at all, and a build runs with `az={zone-a}` or `env={prod}`
- **THEN** every Harvest leg returns its rows, the `prod` / `zone-a` claims receive their `pvc-to-netapp-aggr` edges, `netapp-aggr` / `netapp-node` nodes and measurements exactly as an unfiltered build gives them, and no warning fires

#### Scenario: Harvest served by its own upstream

- **WHEN** the routing table declares one backend serving `harvest` at `http://vm-netapp.example:8428` and another serving every other family at `http://vm-k8s.example:8428`
- **THEN** every Harvest query — including each chunk of the scoped QoS read and the five node legs — is issued only to `http://vm-netapp.example:8428`, the kube-state-metrics and kubelet queries only to `http://vm-k8s.example:8428`, and the resulting `pvc-to-netapp-aggr` edges join claims read from one upstream to volumes read from the other

#### Scenario: Unfiltered build merges Harvest across backends

- **WHEN** two backends serve `harvest` for different zones and a build runs with no `az` value
- **THEN** every Harvest query is issued to both and the results are merged, so aggregates from both zones can match their claims in one graph

#### Scenario: Hub mode loads claims from the rooted rows

- **WHEN** a `/v1/storage-graph` build rooted at `ontap_cluster=ontap-prod` runs with `az={zone-a}`, `env={prod}`, and the `zone-a` `harvest` backend returns two rooted FlexVols, one whose claim lives in a `zone-a` / `prod` cluster and one whose claim lives in an `env="dev"` cluster
- **THEN** the `prod` claim is loaded and receives its aggregate and SVM exactly as an unfiltered build would give them, and the `dev` claim is not loaded, because every claim query carries `<env-key>="prod"` — the Harvest read that returned both FlexVols carried no `az` / `env` matcher

## MODIFIED Requirements

### Requirement: PVC-to-NetApp-aggregate edge join

For every PVC entity whose resolved PV name (`volumename`) is non-empty, the builder SHALL derive that PV name into a match token and match it against the `volume` label of the Harvest `volume_labels` series by the suffix comparison of "Suffix-only PV-name-to-FlexVol-name derivation". On a match with a **non-empty `aggr` label** it SHALL emit one directed `pvc-to-netapp-aggr` edge from the PVC node to the NetApp aggregate node `netapp/<ontap-cluster>/aggr/<aggr>` derived from the same matched series' `cluster` and `aggr` labels — no separate topology query is issued. The edge is a pure function of this one family: whether it is drawn SHALL NOT depend on the QoS families, which only decide what it carries. The join is rooted at the PV name alone (CSI-provisioned PV names are UUID-derived, so cross-cluster collisions are not a practical concern).

**Zone preference.** A claim's **candidate set** — the matched `volume_labels` series every pick of this capability runs over (the aggregate, the SVM, the owning controller the edge's target names, the QoS scope and the ceiling key) — SHALL be the matched series whose zone agrees with the claim's WHEN AT LEAST ONE agrees, and every matched series otherwise. A claim's zone is the `(az, env)` pair its `kube_persistentvolumeclaim_info` series carries under the configured label keys; a series' zone is the pair it carries likewise. Two zones disagree only when BOTH sides carry a complete pair and the pairs differ — an unknown zone on either side agrees, the rule the `alert-overlay` capability applies to alerts. The preference narrows a claim's candidates only: the set-aside series still names its aggregate, controller and SVM for the owner vote and the storage-flow inventory. A token collision across zones — the same FlexVol name, or a clone, on filers of two zones — therefore does not move a claim onto another zone's filer while a series of its own zone exists, which keeps a multi-zone `/v1/storage-graph` body the union of its single-zone bodies for every claim that has such a series; a claim none of whose matched series agrees with its zone (a filer labelled with another `(az, env)` pair, as the Harvest side carries no request matcher) still joins, among every matched series. On an unfiltered `/v1/graph` the preference changes the body only in an estate holding such a collision.

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

#### Scenario: A claim with no agreeing series joins among every matched series

- **WHEN** the same claim's only matching series are `(ontap-b, aggr-q)` carrying `az="zone-b",env="prod"` and `(ontap-0, aggr-a)` carrying `az="zone-c",env="prod"`
- **THEN** neither agrees with the claim's `zone-a` / `prod`, the candidate set is both, and the edge targets `netapp/ontap-0/aggr/aggr-a` (the lexically-smallest pair) — the claim is not left without its storage topology

#### Scenario: An unknown zone never excludes

- **WHEN** the same claim's only other matching series carries no `az` label
- **THEN** that series stays in the candidate set and the lexically-smallest pair is picked across both, exactly as before this rule

#### Scenario: Edge id stable across rebuilds

- **WHEN** the same `(pvc, netapp-aggr)` join is produced by two consecutive builds for the same window
- **THEN** the edge `id` (UUIDv5 over `<type>|<source>|<target>`) is byte-identical between the two builds

### Requirement: Harvest QoS workload series as the I/O source

The builder SHALL consume the NetApp Harvest QoS workload series `qos_read_ops`, `qos_write_ops`, `qos_read_latency`, `qos_write_latency`, `qos_read_data`, and `qos_write_data`. The fixed, case-sensitive label contract each series MUST carry: `cluster` (the ONTAP cluster name), `svm` (the serving Storage Virtual Machine), `policy_group` (the QoS policy group governing the workload — empty when the volume is in none), `lun` (empty for a volume-level workload), and `volume` (the ONTAP FlexVol name, the same stock label the volume-object family carries). No non-stock label is read from these families beyond the configured `az` / `env` pair, which is read, when present, only to choose one zone per workload below.

Issued QoS queries SHALL carry **no `lun` matcher**. ONTAP collects a workload per LUN as well as per volume and a LUN workload carries the `volume` of its containing FlexVol, so the two SHALL NOT be summed together — but that discard is a **reader** rule, not a selector one: the builder SHALL sum only candidates whose `lun` label is empty, and SHALL read every candidate, LUN rows included, when resolving the policy group (see "QoS fixed-policy throughput ceilings"). Keeping the discard in the reader is strictly stronger than a `lun=""` selector, which a Harvest template omitting `lun` on a LUN workload would slip past, and it is what makes the SAN policy reachable at all. The `volume` scope restriction of the scoped-read requirement is therefore the ONLY matcher these queries carry.

Values SHALL be read **verbatim**: Harvest already resolves ONTAP's base counters, so the ops series are per-second rates, the latency series are averages in microseconds, and the data series are throughput in bytes per second. The issued queries SHALL NOT wrap these series in `rate()` — the opposite of the service-graph RED counters, where the upstream series are raw counters.

A matched QoS series SHALL contribute to a claim only when it belongs to the volume the edge was drawn for: its `cluster` MUST equal the ONTAP cluster of the picked aggregate, and its `svm` MUST equal the claim's resolved SVM whenever both are non-empty. A FlexVol name colliding across two filers sharing one VictoriaMetrics would otherwise sum a foreign volume's throughput onto this edge. A candidate carrying no `svm` label still measures the volume and is kept, and can still contribute its `policy_group` — the ceiling key's SVM component comes from hop A, not from this series.

**One zone per workload.** The Harvest queries carry no `az` / `env` matcher, so the same workload can be returned once per `(az, env)` pair it is labelled with. A **workload** is a candidate's `(cluster, svm, volume, lun)` together with its family; the builder SHALL keep, for each workload among a claim's in-scope candidates, the rows of EXACTLY ONE zone: the claim's own zone when the workload carries it; otherwise the rows carrying no complete pair, which agree with every claim as in the join above; otherwise the lexically-smallest complete pair (compared by `az`, then `env`). The kept rows alone SHALL supply both the I/O sums and the policy-group pick. The choice SHALL be made per workload, never once per claim: another FlexVol the claim matched, a LUN workload beside the volume one, and another family are each kept on their own, so a zone choice never drops a different workload's I/O. Rows of two zones SHALL NOT be summed for one workload, so a workload visible under two pairs is counted once, and the result is independent of vector order. A deployment whose Harvest series carry no `az` / `env` labels keeps every in-scope row and observes no change.

All six families are OPTIONAL and independent of the topology source. When none matches a claim that DID resolve its topology, the builder SHALL still emit that claim's `pvc-to-netapp-aggr` edge with no `metrics` key at all, and SHALL count the claim toward the I/O-coverage signal. A volume for which ONTAP collects no QoS workload is the fourth known blind spot of this capability, alongside the three derivation blind spots above.

#### Scenario: QoS queries restricted to volume granularity

- **WHEN** the builder issues the six Harvest QoS queries for a window
- **THEN** volume granularity is enforced by the reader, not the selector: no query string carries a `lun` matcher, each references the bare series evaluated at the window end restricted only by the `volume` scope, and none wraps the series in `rate()`

#### Scenario: LUN workloads never contribute

- **WHEN** a claim's matched FlexVol is carried both by a volume-level QoS series (no `lun` label, `qos_read_ops` = `150`) and by a LUN-level QoS series (`lun="/vol/trident_pvc_9f3a/lun0"`, `qos_read_ops` = `90`)
- **THEN** the edge reports `read_ops: 150` — the LUN series reaches the reader, which discards it from every I/O sum

#### Scenario: A LUN-only FlexVol measures nothing

- **WHEN** a claim's matched FlexVol is carried by LUN-level QoS series only, with no volume-level row in the window
- **THEN** the edge is still emitted with no `metrics` key at all, and the claim is counted by the I/O-coverage signal — a LUN row is not a volume measurement

#### Scenario: A colliding PV name on another filer does not contribute

- **WHEN** a claim's derived token matches `volume_labels` series on both `ontap-a`/`aggr1` and `ontap-b`/`aggr9`, and QoS series for the same `volume` report `10` on `ontap-a` and `90` on `ontap-b`
- **THEN** the edge targets `netapp/ontap-a/aggr/aggr1` and reports `read_ops: 10` — the other filer's workload is not summed in

#### Scenario: Topology resolves without QoS

- **WHEN** a claim resolves its aggregate from `volume_labels` but no QoS workload series carries its matched FlexVol name
- **THEN** the graph still contains the `pvc-to-netapp-aggr` edge and its `netapp-aggr` / `netapp-node` nodes, the edge has no `metrics` key, and the claim is counted by the I/O-coverage signal

#### Scenario: A workload labelled under two zones is counted once

- **WHEN** a `zone-a` / `prod` claim's FlexVol is carried by volume-level `qos_read_ops` series on `ontap-a` reporting `100` stamped `az="zone-a",env="prod"` and `100` stamped `az="zone-b",env="prod"`
- **THEN** the edge reports `read_ops: 100`, not `200` — the `zone-a` / `prod` row alone is kept

#### Scenario: No row of the workload carries the claim's zone

- **WHEN** the same claim's workload series are stamped `az="zone-c",env="prod"` (`80`) and `az="zone-b",env="prod"` (`100`) only
- **THEN** the edge reports `read_ops: 100` — the lexically-smallest complete pair, `zone-b` / `prod`, is kept, and the policy-group pick reads that row alone

#### Scenario: A row without a zone is preferred to another zone's

- **WHEN** the same claim's workload series are one stamped `az="zone-b",env="prod"` (`100`) and one carrying no `az` / `env` label (`60`)
- **THEN** the edge reports `read_ops: 60` — the row with no pair agrees with the claim and the `zone-b` row does not; had a row stamped `zone-a` / `prod` been present, it would have been kept instead

#### Scenario: A zone choice never drops another workload

- **WHEN** the claim matches two FlexVols on `ontap-a`, one whose `qos_read_ops` row is stamped `zone-a` / `prod` (`100`) and one whose row carries no pair (`5`)
- **THEN** the edge reports `read_ops: 105` — the two FlexVols are different workloads, each keeping its own row
- **WHEN** instead the claim's only `zone-a` / `prod` row is a LUN-level workload and its volume-level row is stamped `zone-b` / `prod` (`150`)
- **THEN** the edge reports `read_ops: 150`, and the LUN row still takes part in the policy-group pick

#### Scenario: Estate without Harvest zone labels is unchanged

- **WHEN** no QoS series carries an `az` or `env` label
- **THEN** every in-scope candidate is kept and the sums are exactly those of an estate that never had the labels

## REMOVED Requirements

### Requirement: Harvest legs carry the request zone and environment

**Reason**: Superseded by "Harvest legs are zone-routed and carry no request matcher". The `az` / `env` matchers made every Harvest series depend on carrying the same zone labelling as kube-state-metrics: a series without the labels, or with another pair than the claims it serves, matched nothing under any filter, and `/v1/storage-graph` is always filtered. The `harvest` backend a request's `az` routes to is the zone boundary; duplicates a shared store returns under several pairs are resolved by zone preference in the reader instead.

**Migration**: None required for an estate whose Harvest series carry the configured `az` / `env` labels consistently with kube-state-metrics — its bodies are unchanged. The operator precondition that every Harvest series MUST carry the labels is withdrawn. An operator who relied on the matcher to keep a catch-all or multi-environment `harvest` store from answering with another environment's filers SHALL split that store into zone-declared `harvest` backends.
