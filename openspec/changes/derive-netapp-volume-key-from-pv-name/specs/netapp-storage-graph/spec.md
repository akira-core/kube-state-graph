## ADDED Requirements

### Requirement: PV-name-to-FlexVol-name derivation

The builder SHALL bridge the Kubernetes PersistentVolume name and the ONTAP FlexVol name by deriving a **match token** from each claim's resolved PV name and comparing that token against the stock `volume` label of the Harvest series. ONTAP volume names admit only letters, digits and `_`, so a `volume` value can never equal a `pvc-<uuid>` PV name and the two SHALL NOT be compared for equality directly.

Derivation is an **ordered list of regular-expression rewrite rules**, each a `(pattern, replacement)` pair, applied to the PV name in declaration order with every match replaced. The default list is exactly one rule replacing `-` with `_`. The default SHALL NOT prepend a storage prefix: the prefix is per-backend configurable in the provisioner and the default match mode below does not need it.

The derived token is compared against `volume` under one operator-selected **match mode**:

- `exact` — the token equals `volume`.
- `suffix` — `volume` ends with the token. **This is the default.**
- `contains` — `volume` contains the token.
- `regex` — the token is compiled as a regular expression and matched against `volume`.

`suffix` is the default because a FlexVol provisioned from a claim is named by prefixing the transformed PV name, so a suffix match resolves it without the deployment declaring the prefix, while still rejecting a derived volume whose name extends past the PV name (a clone or snapshot suffixed after it), which `contains` would wrongly accept.

Derivation SHALL run **once per claim**, never per Harvest series, so one PV name yields exactly one token and no collision rule over rewritten values is required.

Every rewrite pattern SHALL be validated at startup and an invalid pattern SHALL be a fatal configuration error, never a silent fallback to the default. An unrecognised match mode SHALL likewise be fatal. A claim whose PV name is empty SHALL derive no token and SHALL NOT be matched against any series.

The derivation SHALL be a pure function of the PV name and the configuration, so the same estate and the same configuration produce byte-identical output across rebuilds.

#### Scenario: Default derivation resolves a stock Trident FlexVol

- **WHEN** a PVC resolves `volumename="pvc-9f3a-11d0"`, the configuration is left at its defaults, and a `volume_labels` series carries `volume="trident_pvc_9f3a_11d0"`
- **THEN** the claim matches that series, its `pvc-to-netapp-aggr` edge is emitted, and no relabel rule was required of the deployment

#### Scenario: Suffix mode rejects a clone whose name extends past the PV name

- **WHEN** a PVC resolves `volumename="pvc-9f3a"`, the match mode is the default `suffix`, and `volume_labels` carries both `volume="trident_pvc_9f3a"` and `volume="trident_pvc_9f3a_clone"`
- **THEN** only `volume="trident_pvc_9f3a"` matches the claim, and the clone contributes neither an aggregate nor any I/O measurement

#### Scenario: Contains mode admits what suffix mode rejects

- **WHEN** the same estate is configured with match mode `contains`
- **THEN** both series match the claim and the aggregate pick collapses through the lexically-smallest `(ontap-cluster, aggr)` rule

#### Scenario: Custom rewrite rules replace the default

- **WHEN** the deployment configures the ordered rules `-` → `_` followed by `^` → `vol_` and a claim resolves `volumename="pvc-9f3a"`
- **THEN** the derived token is `vol_pvc_9f3a` and it is matched against `volume` under the configured mode

#### Scenario: Invalid pattern is fatal at startup

- **WHEN** the deployment configures a rewrite pattern that is not a valid regular expression
- **THEN** startup fails with an error naming the offending pattern, and the process does not start with the default rules substituted

#### Scenario: Derivation is deterministic

- **WHEN** two consecutive builds run over the same estate with the same configuration
- **THEN** every claim derives the same token and the emitted node, edge and label sets are byte-identical

### Requirement: Scoped and batched QoS workload read

The six QoS workload queries SHALL be issued **only for FlexVol names the volume-label family has already matched**, never as an unfiltered read of every workload on the filer. ONTAP collects a QoS workload for every volume, the overwhelming majority of which back no claim, and the builder consults the QoS families only for claims that already resolved an aggregate — so an unfiltered read fetches series that are provably discarded before they are read.

The scope SHALL be the sorted, de-duplicated set of `volume` values matched by the loaded claims' derived tokens, and each issued query SHALL restrict `volume` to that set with an anchored alternation composed with — never replacing — the family's fixed `lun=""` selector. Because the scope holds FlexVol names that already matched, the query-layer restriction is **exact**: the match modes of the derivation requirement are applied once, in the builder, and SHALL NOT reach the query layer.

The scope SHALL be computed from the claim-info family and the volume-label family alone; the QoS read SHALL wait on those two families only, and SHALL NOT wait on families it does not read.

When the scope is **empty** — no claim matched any volume-label series, or the volume-label family was absent or degraded — the builder SHALL issue **no QoS workload query at all**, mirroring the rule that a selector loading no pods or services issues no service-graph queries.

Because the alternation grows with the number of matched volumes and upstream installations cap query length, the scope SHALL be **chunked deterministically** and one query issued per chunk per family. A single volume name SHALL always be issued even when it alone exceeds the chunk budget: dropping it would remove a claim's measurements silently. Chunk results SHALL be merged in **chunk order**, not completion order, so that the summed I/O values are a pure function of the scope and independent of upstream timing.

Each chunk SHALL degrade independently and SHALL NOT fail the build: a failed chunk costs I/O measurements only for the claims whose volumes it carried, and the deterministic chunking makes which claims those are a pure function of the scope. The `pvc-to-netapp-aggr` edges, the aggregate and controller entities, and the PVC `svm` labels are unaffected by any QoS chunk outcome.

#### Scenario: QoS read is restricted to matched volumes

- **WHEN** a build loads 3 claims that match `volume` values `v_a`, `v_b` and `v_c` while the filer carries 40000 other QoS workloads
- **THEN** every issued QoS query restricts `volume` to exactly `{v_a, v_b, v_c}` alongside its `lun=""` matcher, and no series for any other workload is fetched

#### Scenario: No matched volumes issues no QoS query

- **WHEN** a build reads a `volume_labels` vector in which no series matches any loaded claim's derived token
- **THEN** no QoS workload query is issued at all, every claim's outcome is unchanged, and the build succeeds

#### Scenario: Volume-label family absent issues no QoS query

- **WHEN** the volume-label family is absent from the window or its read degraded
- **THEN** no QoS workload query is issued, no `pvc-to-netapp-aggr` edge is emitted, and the build succeeds

#### Scenario: Large scope is chunked and merged deterministically

- **WHEN** the matched scope is large enough to exceed the chunk budget and is split across several queries per family
- **THEN** the merged result is identical to a single unchunked read of the same scope, and two consecutive builds over the same estate produce byte-identical I/O values

#### Scenario: One failed chunk costs only its own claims

- **WHEN** one chunk of one QoS family fails while every other chunk succeeds
- **THEN** the build succeeds, claims carried by the successful chunks keep their `metrics`, claims carried by the failed chunk emit their edge with no `metrics` key and are counted by the I/O-coverage signal, and no claim loses its aggregate, controller or `svm`

## MODIFIED Requirements

### Requirement: Harvest volume-label series as the storage topology source

The builder SHALL consume the NetApp Harvest volume-object label series `volume_labels` from the same centralised VictoriaMetrics endpoint as every other series. The fixed, case-sensitive label contract it MUST carry: `cluster` (the ONTAP cluster name — NOT a Kubernetes cluster; the two namespaces never mix), `node` (the ONTAP controller currently owning the containing aggregate), `aggr` (the containing aggregate), `svm` (the serving Storage Virtual Machine), and `volume` (the ONTAP FlexVol name). It is an **info series**: its sample value SHALL be ignored entirely and only its label set consumed.

Every label in that contract is **stock Harvest output**. The builder SHALL NOT require the deployment to install a Prometheus relabel rule, and SHALL NOT read any non-stock label naming the Kubernetes PersistentVolume.

This one series is the SOLE source of the graph's storage topology — the `pvc-to-netapp-aggr` edge, the `netapp-aggr` and `netapp-node` entities, and the PVC `svm` label all derive from it and from nothing else. The I/O measurements and the throughput ceiling ride on separate families (the two requirements below) and SHALL NOT contribute to any topological decision; conversely, a claim SHALL NEVER lose its storage topology because an I/O family failed to match.

The issued query SHALL read the series at the window end without `rate()`, in the same shape as every other Harvest leg, and SHALL carry no restriction on `volume` — the set of interesting FlexVol names is not known until this family has been read.

The bridge from a claim's PV name to this family's `volume` label is the derivation described in "PV-name-to-FlexVol-name derivation". The graph inherits three blind spots from it: a FlexVol whose name does not embed the claim's PV name under the configured derivation never joins (its claim's `svm` is absent and no edge is drawn); the Trident "economy" drivers pack many claims into one shared FlexVol, so no per-claim series exists at all; and a FlexGroup volume spans aggregates, so its series carries no single usable `aggr` label (no aggregate edge can be drawn — see the join-coverage requirement).

The family is OPTIONAL. When it is absent from the window — the normal case for a deployment without NetApp Harvest — the builder SHALL produce a valid graph with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` labels; PVC `volumename` labels are unaffected and the build SHALL NOT fail.

#### Scenario: Volume label series consumed for its labels only

- **WHEN** the builder issues the `volume_labels` query for a window
- **THEN** the query references the bare series evaluated at the window end, does not wrap it in `rate()`, carries no `volume` restriction, and the resolver derives the aggregate, owning controller, and SVM from the matched series' labels while its sample value plays no part in any output

#### Scenario: Stock Harvest output joins without a relabel rule

- **WHEN** the upstream carries `volume_labels` exactly as stock Harvest emits it, with no deployment-installed relabel rule and no label naming the Kubernetes PersistentVolume
- **THEN** claims whose derived tokens match the `volume` label still resolve their aggregate, controller and `svm`, and their `pvc-to-netapp-aggr` edges are emitted

#### Scenario: Harvest absent entirely

- **WHEN** the upstream contains topology series but no `volume_labels` series for the window
- **THEN** the build completes successfully with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` labels, while PVC `volumename` labels still resolve from `kube_persistentvolumeclaim_info`

#### Scenario: I/O families present without the label series

- **WHEN** the upstream carries QoS workload series whose `volume` matches a claim's derived token but no `volume_labels` series matches it
- **THEN** no `pvc-to-netapp-aggr` edge is emitted for that claim, no aggregate or controller is materialised from the QoS series, and the build does not fail

### Requirement: Harvest QoS workload series as the I/O source

The builder SHALL consume the NetApp Harvest QoS workload series `qos_read_ops`, `qos_write_ops`, `qos_read_latency`, `qos_write_latency`, `qos_read_data`, and `qos_write_data`. The fixed, case-sensitive label contract each series MUST carry: `cluster` (the ONTAP cluster name), `svm` (the serving Storage Virtual Machine), `policy_group` (the QoS policy group governing the workload — empty when the volume is in none), `lun` (empty for a volume-level workload), and `volume` (the ONTAP FlexVol name, the same stock label the volume-object family carries). No non-stock label is read from these families.

Every issued QoS query SHALL restrict the selector to **volume granularity** with the exact matcher `lun=""`. ONTAP collects a workload per LUN as well as per volume, and a LUN workload carries the `volume` of its containing FlexVol, so an unrestricted read would sum LUN traffic on top of volume traffic for the same claim. This matcher is a fixed, **request-invariant metric-selection contract** — not a caller filter — of the same class as the service-graph sentinel matcher and `kube_node_status_condition{condition="Ready"}`. Because a PromQL empty-string matcher also matches series carrying no such label at all, the contract stays correct against a Harvest template that omits `lun` entirely. It is composed with, never replaced by, the `volume` scope restriction of the scoped-read requirement.

Values SHALL be read **verbatim**: Harvest already resolves ONTAP's base counters, so the ops series are per-second rates, the latency series are averages in microseconds, and the data series are throughput in bytes per second. The issued queries SHALL NOT wrap these series in `rate()` — the opposite of the service-graph RED counters, where the upstream series are raw counters.

A matched QoS series SHALL contribute to a claim only when it belongs to the volume the edge was drawn for: its `cluster` MUST equal the ONTAP cluster of the picked aggregate, and its `svm` MUST equal the claim's resolved SVM whenever both are non-empty. A FlexVol name colliding across two filers sharing one VictoriaMetrics would otherwise sum a foreign volume's throughput onto this edge. A candidate carrying no `svm` label still measures the volume and is kept, but cannot contribute a policy group.

All six families are OPTIONAL and independent of the topology source. When none matches a claim that DID resolve its topology, the builder SHALL still emit that claim's `pvc-to-netapp-aggr` edge with no `metrics` key at all, and SHALL count the claim toward the I/O-coverage signal. A volume for which ONTAP collects no QoS workload is the fourth known blind spot of this capability, alongside the three derivation blind spots above.

#### Scenario: QoS queries restricted to volume granularity

- **WHEN** the builder issues the six Harvest QoS queries for a window
- **THEN** every query string carries the exact `lun=""` matcher, references the bare series evaluated at the window end, and none wraps the series in `rate()`

#### Scenario: LUN workloads never contribute

- **WHEN** a claim's matched FlexVol is carried both by a volume-level QoS series (`lun=""`, `qos_read_ops` = `150`) and by a LUN-level QoS series (`lun="/vol/trident_pvc_9f3a/lun0"`, `qos_read_ops` = `90`)
- **THEN** the edge reports `read_ops: 150` — the LUN series is excluded at the query layer and never summed in

#### Scenario: A colliding PV name on another filer does not contribute

- **WHEN** a claim's derived token matches `volume_labels` series on both `ontap-a`/`aggr1` and `ontap-b`/`aggr9`, and QoS series for the same `volume` report `10` on `ontap-a` and `90` on `ontap-b`
- **THEN** the edge targets `netapp/ontap-a/aggr/aggr1` and reports `read_ops: 10` — the other filer's workload is not summed in

#### Scenario: Topology resolves without QoS

- **WHEN** a claim resolves its aggregate from `volume_labels` but no QoS workload series carries its matched FlexVol name
- **THEN** the graph still contains the `pvc-to-netapp-aggr` edge and its `netapp-aggr` / `netapp-node` nodes, the edge has no `metrics` key, and the claim is counted by the I/O-coverage signal

### Requirement: PVC-to-NetApp-aggregate edge join

For every PVC entity whose resolved PV name (`volumename`) is non-empty, the builder SHALL derive that PV name into a match token and match it against the `volume` label of the Harvest `volume_labels` series under the configured match mode. On a match with a **non-empty `aggr` label** it SHALL emit one directed `pvc-to-netapp-aggr` edge from the PVC node to the NetApp aggregate node `netapp/<ontap-cluster>/aggr/<aggr>` derived from the same matched series' `cluster` and `aggr` labels — no separate topology query is issued. The edge is a pure function of this one family: whether it is drawn SHALL NOT depend on the QoS families, which only decide what it carries. The join is rooted at the PV name alone (CSI-provisioned PV names are UUID-derived, so cross-cluster collisions are not a practical concern).

The edge SHALL carry empty `labels` (`{}`), a deterministic UUIDv5 `id` (canonical input `<type>|<source>|<target>`), and SHALL de-duplicate by `(pvc, netapp-aggr)`. When matched series disagree on the containing aggregate for one claim — including when the match mode admits several distinct FlexVol names — the builder SHALL pick deterministically the lexically-smallest `(ontap-cluster, aggr)` pair, so the emitted edge set is byte-stable across rebuilds, independent of vector order. A PVC with no resolved `volumename` SHALL emit no `pvc-to-netapp-aggr` edge. A matched series whose `aggr` label is **empty** (the FlexGroup shape) SHALL emit no edge and SHALL be counted by the join-coverage signal.

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

#### Scenario: Edge id stable across rebuilds

- **WHEN** the same `(pvc, netapp-aggr)` join is produced by two consecutive builds for the same window
- **THEN** the edge `id` (UUIDv5 over `<type>|<source>|<target>`) is byte-identical between the two builds

### Requirement: Join-coverage observability

The join has two independently failing halves, and the builder SHALL count and surface each on its own. Both counts are per build, each is surfaced as ONE aggregated warning log carrying its count (the `failed_total_label_set_mismatch` precedent) rather than one log per claim, and neither SHALL ever fail the build:

- **Topology coverage** (`netapp_volume_join_miss`) — a PVC with a non-empty `volumename` whose derived token either matched no `volume_labels` series, or matched only series whose `aggr` label is empty (the FlexGroup shape — the claim's `svm` may still resolve, but no aggregate edge can be drawn), **while at least one `volume_labels` series was read in the build**. This count is the operator's primary signal that the configured derivation does not fit the estate's FlexVol naming.
- **I/O coverage** (`netapp_qos_join_miss`) — a claim that DID draw its `pvc-to-netapp-aggr` edge but matched no series in any of the six QoS workload families, leaving that edge with no measurements at all, **while at least one QoS workload series was read in the build**.

Each signal is gated on its OWN family being present in the window. A deployment running Harvest's volume template without the QoS template therefore gets its storage topology and no spurious I/O warning; a non-NetApp deployment (neither family present) stays silent on both — absence of the upstream is not a coverage failure. Because the QoS read is scoped to already-matched volumes, "at least one QoS workload series was read" SHALL mean at least one issued chunk of at least one QoS family returned series; a build that issued no QoS query at all SHALL emit no I/O-coverage warning. No signal is emitted for an unresolved throughput ceiling: a volume in no QoS policy group is the normal case, not a defect.

The PVC's retained `data.storageclass` value is the operator's discriminator between "this claim was never meant to have a NetApp backend" and "this claim should have joined and did not"; the builder itself SHALL NOT interpret StorageClass names or filter either count by them.

#### Scenario: Unjoined claims counted and warned once

- **WHEN** a build reads a non-empty `volume_labels` vector and three PVCs with non-empty `volumename` derive tokens matching no series
- **THEN** the build succeeds and emits one `netapp_volume_join_miss` warning carrying the count `3`

#### Scenario: A derivation that does not fit the estate is visible

- **WHEN** the estate's FlexVol names embed no PV name at all and every claim therefore fails to match
- **THEN** the build succeeds, no storage chain is emitted, and one `netapp_volume_join_miss` warning carries the full claim count — the misfit is reported rather than silent

#### Scenario: Empty-aggr matches count toward the topology signal

- **WHEN** a build reads a non-empty `volume_labels` vector and one PVC's only matched series carries an empty `aggr` label
- **THEN** that claim is included in the `netapp_volume_join_miss` count even though its `svm` label may have resolved

#### Scenario: Measurement-less edges counted separately

- **WHEN** a build issues its scoped QoS queries, reads a non-empty QoS workload vector, and two claims draw their aggregate edge but match no QoS workload series
- **THEN** the build succeeds, both edges are emitted without a `metrics` key, one `netapp_qos_join_miss` warning carries the count `2`, and the `netapp_volume_join_miss` count is unaffected

#### Scenario: Volume template without the QoS template

- **WHEN** a build matches claims against `volume_labels`, issues its scoped QoS queries, and every one returns zero series
- **THEN** every joined claim's edge is emitted without a `metrics` key and no `netapp_qos_join_miss` warning is emitted

#### Scenario: Non-NetApp deployment stays silent

- **WHEN** a build reads zero `volume_labels` series while PVCs carry `volumename` labels, so no QoS query is issued at all
- **THEN** neither warning is emitted and the build succeeds

#### Scenario: Full coverage emits no warning

- **WHEN** every PVC with a non-empty `volumename` matches a `volume_labels` series with a non-empty `aggr` label and matches at least one QoS workload family
- **THEN** neither warning is emitted

### Requirement: Harvest legs under request-scoped selectors

Every NetApp Harvest query the builder issues — `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, and `node_new_status` — SHALL carry **no request-scoped matcher of any kind**: not `az`, not `env`, and never `cluster` or `namespace`. Harvest's `cluster` label is the **ONTAP** cluster name and never a Kubernetes cluster, so a Kubernetes `cluster` value pushed into it would match nothing; Harvest carries no `namespace` at all; and the `az` dimension reaches Harvest through backend selection alone (below), so a Harvest series need not carry the configured `az` / `env` labels.

The `qos_*` families carry two selectors and no others: the fixed `lun=""` contract, and the `volume` alternation of the scoped-read requirement. That alternation is **derived from upstream data, not from the request**: its values are FlexVol names the volume-label family already returned. It is nonetheless *influenced* by the request, because the claims whose tokens produced those names are themselves loaded under the request's selectors. This is the "narrowed by reference" principle of this capability realised at the query layer rather than only in the parse: a `cluster`, `namespace`, `az` or `env` filter reaches the QoS read solely through the claims it loads, never as a matcher on a Harvest label. Every other Harvest query carries no selector at all.

These queries constitute the `harvest` query family of the `upstream-backend-routing` capability, so they MAY be served by a different upstream installation from the kube-state-metrics and kubelet legs. The family is **zone-routed**: a request's `az` values select which `harvest` backends are asked — those whose `zones` intersect the request, plus any catch-all — under the same rule as the `ksm` and `kubelet` families. Unlike those families, the selected zone is NOT additionally rendered as a matcher: for Harvest the zone boundary is the store, not a label on the series. The `env` dimension has no routing counterpart and SHALL have no effect on the Harvest legs whatsoever. Routing changes **which** installation answers a Harvest query; it changes neither the query string, the three-hop join, nor the per-hop degradation below.

Within a filtered build the storage chain is therefore narrowed **by reference**: an aggregate and its owning controller materialise only when a **loaded** claim's derived token matches a `volume_labels` series, so a `cluster`, `namespace`, or `env` filter reaches the NetApp graph solely through the claims it loads, and an `az` filter reaches it through the claims it loads plus the backends it selects. A filer shared across clusters, zones, or environments is one node set, reached from whichever loaded claims match it. Under a catch-all `harvest` backend, or under any `env` value, the volume-label read is the whole estate and the narrowing is by reference alone; a FlexVol name carried by volumes in two zones or environments resolves to the lexically-smallest `(ontap_cluster, aggr)` — the same collision rule an unfiltered build already applies, since an unfiltered build reads every zone.

#### Scenario: Cluster and namespace filters never reach Harvest

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** no Harvest query carries a `cluster` or `namespace` matcher; the `volume_labels`, aggregate, controller and policy queries are issued exactly as in an unfiltered build; the QoS queries restrict `volume` to the FlexVol names matched by the loaded `cluster-alpha` / `shop` claims alone; and the aggregates in the response are exactly those those claims matched

#### Scenario: Zone filter reaches Harvest

- **WHEN** two backends serve `harvest`, declaring `zones: [zone-a]` and `zones: [zone-b]`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued only to the `zone-a` backend, carrying no `az` matcher; a `volume_labels` series held by the `zone-b` backend is not loaded even if a loaded claim's derived token would match it

#### Scenario: Catch-all Harvest backend under a zone filter

- **WHEN** one backend serves `harvest` with no `zones` declared and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued to that backend carrying no `az` matcher, every zone's `volume_labels` series is loaded, and the aggregates in the response are exactly those matched by the loaded `zone-a` claims

#### Scenario: Environment filter does not reach Harvest

- **WHEN** a build runs with `env={prod}` against a single `harvest` backend holding series stamped `env="prod"` and `env="dev"`
- **THEN** no Harvest query carries an `env` matcher, both environments' series are loaded, and a loaded `prod` claim whose derived token matches a `volume_labels` series stamped `env="dev"` still receives its `pvc-to-netapp-aggr` edge

#### Scenario: Harvest lacking the environment label under an env filter

- **WHEN** the kube-state-metrics series carry `az="zone-a"`, the Harvest series carry no `az` and no `env` label at all, and a build runs with `az={zone-a}` or `env={prod}`
- **THEN** every Harvest leg returns its rows, the `pvc-to-netapp-aggr` edges are drawn for the loaded claims that match, and the selector-coverage Warn never names a Harvest family

#### Scenario: Harvest served by its own upstream

- **WHEN** the routing table declares one backend serving `harvest` at `http://vm-netapp.example:8428` and another serving every other family at `http://vm-k8s.example:8428`
- **THEN** every Harvest query — including each chunk of the scoped QoS read — is issued only to `http://vm-netapp.example:8428`, the kube-state-metrics and kubelet queries only to `http://vm-k8s.example:8428`, and the resulting `pvc-to-netapp-aggr` edges join claims read from one upstream to volumes read from the other

#### Scenario: Unfiltered build merges Harvest across backends

- **WHEN** two backends serve `harvest` for different zones and a build runs with no `az` value
- **THEN** every Harvest query is issued to both and the results are merged, so aggregates from both zones can match their claims in one graph

#### Scenario: Shared filer reached by reference from either filtered cluster

- **WHEN** claims in `cluster-alpha` and `cluster-beta` both match `netapp/ontap-prod/aggr/aggr1` and a build runs with `cluster={cluster-alpha}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` are materialised with a `pvc-to-netapp-aggr` edge from the `cluster-alpha` claim only; a `cluster={cluster-beta}` build materialises the same two nodes from the `cluster-beta` claim
