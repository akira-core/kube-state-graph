# netapp-storage-graph Specification

## Purpose
Surfaces the physical NetApp ONTAP storage behind Kubernetes claims: one graph node per ONTAP aggregate and per ONTAP controller, a PVC-to-aggregate edge derived from a single Harvest label series, per-edge I/O measurements and the volume's declared throughput ceiling from the Harvest QoS families, per-aggregate health and usage, controller health, and coverage signals for claims that should have joined but did not.

## Requirements

### Requirement: Suffix-only PV-name-to-FlexVol-name derivation

The builder SHALL match a claim to a Harvest series iff the series' stock `volume` label ends with the **match token** derived from the claim's resolved PV name. The PV name and the `volume` value SHALL NOT be compared for equality, and no other comparison, and no setting selecting one, SHALL exist: the join SHALL be the same on every build of `GET /v1/graph` and `GET /v1/storage-graph`.

#### Scenario: Default derivation resolves a stock Trident FlexVol

- **WHEN** a PVC resolves `volumename="pvc-9f3a-11d0"`, the configuration is left at its defaults, and a `volume_labels` series carries `volume="trident_pvc_9f3a_11d0"`
- **THEN** the claim matches that series, its `pvc-to-netapp-aggr` edge is emitted, and no relabel rule was required of the deployment
- **AND** the FlexVol, named by prefixing the transformed PV name, resolves without the deployment declaring the storage prefix

#### Scenario: PV name and FlexVol name are never compared for equality

- **WHEN** a claim resolves a `pvc-<uuid>` PV name
- **THEN** no `volume` value is compared to it for equality, since ONTAP volume names admit only letters, digits and `_` and a `volume` value can therefore never equal a `pvc-<uuid>` PV name
- **AND** only the claim's derived token is compared, by suffix

#### Scenario: A clone whose name extends past the PV name never matches

- **WHEN** a PVC resolves `volumename="pvc-9f3a"` and `volume_labels` carries both `volume="trident_pvc_9f3a"` and `volume="trident_pvc_9f3a_clone"`
- **THEN** only `volume="trident_pvc_9f3a"` matches the claim, and the clone contributes neither an aggregate nor any I/O measurement
- **AND** the suffix comparison is what rejects a derived volume (a clone or snapshot) whose name extends past the PV name

#### Scenario: A FlexVol named exactly the token matches

- **WHEN** a backend provisions with an empty storage prefix, a PVC resolves `volumename="pvc-9f3a"`, and `volume_labels` carries `volume="pvc_9f3a"`
- **THEN** the claim matches that series
- **AND** it matches because a value ends with itself

#### Scenario: The match-mode setting does not exist

- **WHEN** the server is started with `--netapp-volume-match-mode=contains`
- **THEN** startup fails as it does for any unknown flag; and when `KSG_NETAPP_VOLUME_MATCH_MODE=contains` is set in the environment instead, it is not read and every claim is matched by suffix
- **AND** the server defines no match-mode flag: `--netapp-volume-match-mode` is an unknown flag and `KSG_NETAPP_VOLUME_MATCH_MODE` is never read

#### Scenario: The suffix comparison is invertible for the storage graph

- **WHEN** a `GET /v1/storage-graph` build restricts a Harvest read by derived tokens or inverts a FlexVol name back to candidate PersistentVolume names
- **THEN** each token renders as an anchored alternation branch (`.*<token>`) and a FlexVol name inverts to the PV names whose token it ends with
- **AND** suffix is the one comparison admitting both, which that endpoint's by-reference read requires (`storage-graph-api`, "Every root kind is tracked from its own tier to its claims")

### Requirement: Ordered PV-name rewrite rules

The builder SHALL derive a claim's match token by applying an ordered list of regular-expression rewrite rules, each a `(pattern, replacement)` pair, to the PV name in declaration order with every match replaced, once per claim and never per Harvest series. The default list SHALL be exactly one rule replacing `-` with `_` and SHALL NOT prepend a storage prefix.

#### Scenario: Default rule list

- **WHEN** the configuration is left at its defaults and a claim resolves `volumename="pvc-9f3a"`
- **THEN** the derived token is `pvc_9f3a`, produced by the single default rule `-` → `_`
- **AND** no storage prefix is prepended: the prefix is per-backend configurable in the provisioner and the suffix comparison does not need it

#### Scenario: Custom rewrite rules replace the default

- **WHEN** the deployment configures the ordered rules `-` → `_` followed by `^` → `vol_` and a claim resolves `volumename="pvc-9f3a"`
- **THEN** the derived token is `vol_pvc_9f3a` and it matches every `volume` value ending with it
- **AND** each rule is applied in declaration order with every match of its pattern replaced

#### Scenario: Derivation runs once per claim

- **WHEN** a build matches claims against many Harvest series
- **THEN** each claim's PV name is derived exactly once, yielding exactly one token, and no Harvest series value is rewritten
- **AND** no collision rule over rewritten values is required

#### Scenario: Invalid pattern is fatal at startup

- **WHEN** the deployment configures a rewrite pattern that is not a valid regular expression
- **THEN** startup fails with an error naming the offending pattern, and the process does not start with the default rules substituted
- **AND** every rewrite pattern is validated at startup, and an invalid one is a fatal configuration error, never a silent fallback to the default

#### Scenario: Empty PV name derives no token

- **WHEN** a claim's PV name is empty
- **THEN** it derives no token and is not matched against any series

#### Scenario: Derivation is deterministic

- **WHEN** two consecutive builds run over the same estate with the same configuration
- **THEN** every claim derives the same token and the emitted node, edge and label sets are byte-identical
- **AND** the derivation is a pure function of the PV name and the configuration

### Requirement: Harvest volume-label series as the storage topology source

The builder SHALL consume the NetApp Harvest volume-object info series `volume_labels`, from the same centralised VictoriaMetrics endpoint as every other series, as the SOLE source of the graph's storage topology. It SHALL read only the series' stock label set, ignore its sample value entirely, and SHALL NOT require a deployment-installed relabel rule.

#### Scenario: Volume-label contract

- **WHEN** the builder reads a `volume_labels` series
- **THEN** the series MUST carry, and the builder reads, the fixed, case-sensitive labels `cluster` (the ONTAP cluster name — NOT a Kubernetes cluster; the two namespaces never mix), `node` (the ONTAP controller currently owning the containing aggregate), `aggr` (the containing aggregate), `svm` (the serving Storage Virtual Machine) and `volume` (the ONTAP FlexVol name)
- **AND** every one of them is **stock Harvest output**

#### Scenario: Volume label series consumed for its labels only

- **WHEN** the builder issues the `volume_labels` query for a window
- **THEN** the query references the bare series evaluated at the window end, does not wrap it in `rate()`, carries no `volume` restriction on a `/v1/graph` build, and the resolver derives the aggregate, owning controller, and SVM from the matched series' labels while its sample value plays no part in any output
- **AND** the query has the same shape as every other Harvest leg

#### Scenario: Stock Harvest output joins without a relabel rule

- **WHEN** the upstream carries `volume_labels` exactly as stock Harvest emits it, with no deployment-installed relabel rule and no label naming the Kubernetes PersistentVolume
- **THEN** claims whose derived tokens match the `volume` label still resolve their aggregate, controller and `svm`, their PVCs carry the `aggr` label, and their `pvc-to-netapp-aggr` edges are emitted
- **AND** the builder reads no non-stock label naming the Kubernetes PersistentVolume

#### Scenario: Topology derives from the label series alone

- **WHEN** a build emits storage topology
- **THEN** the `pvc-to-netapp-aggr` edge, the `netapp-aggr` and `netapp-node` entities, and the PVC `svm` and `aggr` labels all derive from `volume_labels` and from nothing else
- **AND** the I/O measurements and the throughput ceiling ride on separate families ("Harvest QoS workload series as the I/O source", "QoS fixed-policy throughput ceilings") that contribute to no topological decision, so a claim NEVER loses its storage topology because an I/O family failed to match

#### Scenario: Harvest absent entirely

- **WHEN** the upstream contains topology series but no `volume_labels` series for the window
- **THEN** the build completes successfully with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` or `aggr` labels, while PVC `volumename` labels still resolve from `kube_persistentvolumeclaim_info`
- **AND** this is the normal case for a deployment without NetApp Harvest, since the family is OPTIONAL, and the build does not fail

#### Scenario: I/O families present without the label series

- **WHEN** the upstream carries QoS workload series whose `volume` matches a claim's derived token but no `volume_labels` series matches it
- **THEN** no `pvc-to-netapp-aggr` edge is emitted for that claim, its PVC carries no `aggr` label, no aggregate or controller is materialised from the QoS series, and the build does not fail

### Requirement: Volume-label read per endpoint

A `/v1/graph` build SHALL read `volume_labels` with a single read carrying no restriction on `volume`, while a `/v1/storage-graph` build SHALL NOT read the family across the zone and SHALL instead read it by reference, merging every read before the parse.

#### Scenario: Pod graph reads the family whole

- **WHEN** a `/v1/graph` build reads `volume_labels`
- **THEN** it issues a single read with no restriction on `volume`
- **AND** the build carries no root, and the set of interesting FlexVol names is not known until this family has been read

#### Scenario: Storage graph reads the family by reference

- **WHEN** a `/v1/storage-graph` build reads `volume_labels`
- **THEN** it reads the family restricted by the request's storage-side root (ONTAP cluster, aggregate, SVM or controller) where it has one, restricted on `volume` to the derived tokens of the claims the build tracked, and whole for every aggregate those reads touched, merging every read before the parse
- **AND** that shape and its byte-identical-body obligation are those of the `storage-graph-api` capability's "Every root kind is tracked from its own tier to its claims" and "The tracked claims expand to both ends of the chain"

### Requirement: Known storage-join blind spots

The builder SHALL draw no storage chain for a claim whose FlexVol the upstream does not expose as a per-claim, single-aggregate series whose name embeds the claim's PV name under the configured derivation, and SHALL draw no measurement for a volume ONTAP collects no QoS workload for.

#### Scenario: FlexVol name does not embed the PV name

- **WHEN** a FlexVol's name does not embed its claim's PV name under the configured derivation
- **THEN** it never joins: its claim's `svm` and `aggr` are absent and no edge is drawn

#### Scenario: Trident economy drivers

- **WHEN** a Trident "economy" driver packs many claims into one shared FlexVol
- **THEN** no per-claim series exists at all

#### Scenario: FlexGroup volume

- **WHEN** a claim's volume is a FlexGroup spanning aggregates
- **THEN** its series carries no single usable `aggr` label, no aggregate edge is drawn and its PVC carries no `aggr` label, and the claim is counted as described in "Join-coverage observability"

#### Scenario: Volume without a QoS workload

- **WHEN** ONTAP collects no QoS workload for a joined claim's volume
- **THEN** the claim keeps its storage topology but carries no measurement, the fourth known blind spot of this capability alongside the three derivation blind spots

### Requirement: Harvest QoS workload series as the I/O source

The builder SHALL read each claim's I/O from the Harvest QoS workload series `qos_read_ops`, `qos_write_ops`, `qos_read_latency`, `qos_write_latency`, `qos_read_data` and `qos_write_data` verbatim, summing only volume-level candidates (empty `lun`) and keeping one zone per workload as "One zone per QoS workload" defines. All six families SHALL be OPTIONAL and independent of the topology source.

#### Scenario: QoS workload label contract

- **WHEN** the builder reads a QoS workload series
- **THEN** each series MUST carry, and the builder reads, the fixed, case-sensitive labels `cluster` (the ONTAP cluster name), `svm` (the serving Storage Virtual Machine), `policy_group` (the QoS policy group governing the workload — empty when the volume is in none), `lun` (empty for a volume-level workload) and `volume` (the ONTAP FlexVol name, the same stock label the volume-object family carries)
- **AND** no non-stock label is read beyond the configured `az` / `env` pair, which is read, when present, only to choose one zone per workload ("One zone per QoS workload")

#### Scenario: QoS queries restricted to volume granularity

- **WHEN** the builder issues the six Harvest QoS queries for a window
- **THEN** volume granularity is enforced by the reader, not the selector: no query string carries a `lun` matcher, each references the bare series evaluated at the window end restricted only by the `volume` scope, and none wraps the series in `rate()`
- **AND** the `volume` scope restriction of "Scoped and batched QoS workload read" is the ONLY matcher these queries carry

#### Scenario: LUN rows are discarded by the reader, not the selector

- **WHEN** ONTAP collects a workload per LUN as well as per volume and a LUN workload carries the `volume` of its containing FlexVol
- **THEN** the two are not summed together: the builder sums only candidates whose `lun` label is empty, and reads every candidate, LUN rows included, when resolving the policy group ("Ceiling key assembled from both topology hops")
- **AND** this reader discard is strictly stronger than a `lun=""` selector, which a Harvest template omitting `lun` on a LUN workload would slip past, and it is what makes the SAN policy reachable at all

#### Scenario: LUN workloads never contribute

- **WHEN** a claim's matched FlexVol is carried both by a volume-level QoS series (no `lun` label, `qos_read_ops` = `150`) and by a LUN-level QoS series (`lun="/vol/trident_pvc_9f3a/lun0"`, `qos_read_ops` = `90`)
- **THEN** the edge reports `read_ops: 150` — the LUN series reaches the reader, which discards it from every I/O sum

#### Scenario: A LUN-only FlexVol measures nothing

- **WHEN** a claim's matched FlexVol is carried by LUN-level QoS series only, with no volume-level row in the window
- **THEN** the edge is still emitted with no `metrics` key at all, and the claim is counted by the I/O-coverage signal — a LUN row is not a volume measurement

#### Scenario: QoS values are already resolved

- **WHEN** the builder reads the QoS workload series
- **THEN** it reads their values **verbatim**, since Harvest already resolves ONTAP's base counters: the ops series are per-second rates, the latency series are averages in microseconds, and the data series are throughput in bytes per second
- **AND** no issued query wraps them in `rate()`, the opposite of the service-graph RED counters, whose upstream series are raw counters

#### Scenario: Topology resolves without QoS

- **WHEN** a claim resolves its aggregate from `volume_labels` but no QoS workload series carries its matched FlexVol name
- **THEN** the graph still contains the `pvc-to-netapp-aggr` edge and its `netapp-aggr` / `netapp-node` nodes, the edge has no `metrics` key, and the claim is counted by the I/O-coverage signal
- **AND** the edge carries no `metrics` key at all whenever none of the six families matches a claim that DID resolve its topology

### Requirement: QoS candidates belong to the claim's own volume

A matched QoS series SHALL contribute to a claim only when it belongs to the volume the edge was drawn for: its `cluster` MUST equal the ONTAP cluster of the picked aggregate, and its `svm` MUST equal the claim's resolved SVM whenever both are non-empty.

#### Scenario: A colliding PV name on another filer does not contribute

- **WHEN** a claim's derived token matches `volume_labels` series on both `ontap-a`/`aggr1` and `ontap-b`/`aggr9`, and QoS series for the same `volume` report `10` on `ontap-a` and `90` on `ontap-b`
- **THEN** the edge targets `netapp/ontap-a/aggr/aggr1` and reports `read_ops: 10` — the other filer's workload is not summed in
- **AND** without the `cluster` rule, a FlexVol name colliding across two filers sharing one VictoriaMetrics would sum a foreign volume's throughput onto this edge

#### Scenario: A candidate without an svm label is kept

- **WHEN** an otherwise in-scope QoS candidate carries no `svm` label
- **THEN** it still measures the volume and is kept, and can still contribute its `policy_group`
- **AND** the ceiling key's SVM component comes from hop A, not from this series

### Requirement: One zone per QoS workload

For each workload among a claim's in-scope QoS candidates, the builder SHALL keep the rows of EXACTLY ONE zone, and the kept rows alone SHALL supply both the I/O sums and the policy-group pick. Rows of two zones SHALL NOT be summed for one workload, and the choice SHALL be made per workload, never once per claim.

#### Scenario: Workload identity and zone order

- **WHEN** the Harvest queries, carrying no `az` / `env` matcher, return the same workload once per `(az, env)` pair it is labelled with
- **THEN** a **workload** is a candidate's `(cluster, svm, volume, lun)` together with its family, and the kept zone is: the claim's own zone when the workload carries it; otherwise the rows carrying no complete pair, which agree with every claim as in "Zone preference among a claim's matched series"; otherwise the lexically-smallest complete pair (compared by `az`, then `env`)
- **AND** a workload visible under two pairs is counted once and the result is independent of vector order
- **AND** another FlexVol the claim matched, a LUN workload beside the volume one, and another family are each kept on their own, so a zone choice never drops a different workload's I/O

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
- **AND** a deployment whose Harvest series carry no `az` / `env` labels keeps every in-scope row and observes no change

### Requirement: Scoped and batched QoS workload read

The builder SHALL issue the six QoS workload queries **only for FlexVol names the volume-label family has already matched**, restricting `volume` to the sorted, de-duplicated set of matched `volume` values with an anchored alternation as each query's ONLY matcher, and SHALL issue **no QoS workload query at all** when that scope is **empty**.

#### Scenario: QoS read is restricted to matched volumes

- **WHEN** a build loads 3 claims that match `volume` values `v_a`, `v_b` and `v_c` while the filer carries 40000 other QoS workloads
- **THEN** every issued QoS query restricts `volume` to exactly `{v_a, v_b, v_c}` and carries no other matcher, and no series for any other workload is fetched
- **AND** an unfiltered read is never issued: ONTAP collects a QoS workload for every volume, the overwhelming majority of which back no claim, and the builder consults the QoS families only for claims that already resolved an aggregate, so an unfiltered read fetches series that are provably discarded before they are read

#### Scenario: The query-layer restriction is exact

- **WHEN** the builder renders the scope's alternation
- **THEN** the restriction is **exact**, because the scope holds FlexVol names that already matched
- **AND** the suffix comparison of "Suffix-only PV-name-to-FlexVol-name derivation" is applied once, in the builder, and never reaches the query layer; volume granularity stays a reader rule ("Harvest QoS workload series as the I/O source")

#### Scenario: The QoS read waits only on the families it reads

- **WHEN** the builder computes the QoS scope
- **THEN** it computes it from the claim-info family and the volume-label family alone, and the QoS read waits on those two families only
- **AND** it does not wait on families it does not read

#### Scenario: No matched volumes issues no QoS query

- **WHEN** a build reads a `volume_labels` vector in which no series matches any loaded claim's derived token
- **THEN** no QoS workload query is issued at all, every claim's outcome is unchanged, and the build succeeds
- **AND** this mirrors the rule that a selector loading no pods or services issues no service-graph queries

#### Scenario: Volume-label family absent issues no QoS query

- **WHEN** the volume-label family is absent from the window or its read degraded
- **THEN** no QoS workload query is issued, no `pvc-to-netapp-aggr` edge is emitted, and the build succeeds

### Requirement: Deterministic QoS chunking with per-chunk degradation

The builder SHALL **chunk deterministically** the QoS scope and issue one query per chunk per family, merging chunk results in **chunk order**, not completion order. Each chunk SHALL degrade independently and SHALL NOT fail the build.

#### Scenario: Chunking bounds the query length

- **WHEN** the alternation grows with the number of matched volumes past what upstream installations accept as query length
- **THEN** the scope is split into chunks and one query is issued per chunk per family

#### Scenario: Chunking bounds the alternation's value count

- **WHEN** the matched scope holds more than 100 FlexVol names, even when they all fit the byte budget
- **THEN** it is split so that no chunk's `volume` alternation holds more than 100 values, so the upstream resolves each chunk by exact value lookup rather than by evaluating a regex against every `volume` value it indexes
- **AND** the merged result is identical to the one a byte-only split produces, and which claims a failed chunk costs on `/v1/graph` remains a pure function of the scope

#### Scenario: An oversized volume name is still issued

- **WHEN** a single volume name alone exceeds the chunk budget
- **THEN** it is still always issued, because dropping it would remove a claim's measurements silently

#### Scenario: Large scope is chunked and merged deterministically

- **WHEN** the matched scope is large enough to exceed the chunk budget and is split across several queries per family
- **THEN** the merged result is identical to a single unchunked read of the same scope, and two consecutive builds over the same estate produce byte-identical I/O values
- **AND** the summed I/O values are a pure function of the scope and independent of upstream timing

#### Scenario: One failed chunk costs only its own claims

- **WHEN** one chunk of one QoS family fails while every other chunk succeeds
- **THEN** the build succeeds, claims carried by the successful chunks keep their `metrics`, claims carried by the failed chunk emit their edge with no `metrics` key and are counted by the I/O-coverage signal, and no claim loses its aggregate, controller or `svm`
- **AND** which claims a failed chunk costs is a pure function of the scope, and the `pvc-to-netapp-aggr` edges, the aggregate and controller entities and the PVC `svm` labels are unaffected by any QoS chunk outcome

### Requirement: QoS fixed-policy throughput ceilings

The builder SHALL resolve each joined volume's declared throughput ceiling from the OPTIONAL fixed-policy series `qos_policy_fixed_max_throughput_iops` and `qos_policy_fixed_max_throughput_mbps`, keyed on the `(ontap-cluster, svm, policy-group)` triple, for every claim whose `volume_labels` match resolved an SVM, surfacing it as "Ceiling surfaces on the PVC node and the storage edge" defines. **An incomplete or unmatched key SHALL be ignored, never widened.**

#### Scenario: Fixed-policy label contract

- **WHEN** the builder reads a fixed-policy series
- **THEN** it reads the fixed, case-sensitive labels `cluster` (the ONTAP cluster), `svm`, and the policy's own identity label naming the policy group, read as `name` with a `policy_group` fallback
- **AND** Harvest spells that label differently across templates, so the contract pins the key's SHAPE, not the label's spelling

#### Scenario: Claims with an SVM are eligible with or without an aggregate

- **WHEN** a claim's `volume_labels` match resolved an SVM
- **THEN** a ceiling is resolved for it whether it also resolved an aggregate or resolved an SVM but no aggregate (the FlexGroup shape)

#### Scenario: Ceiling resolved from the volume's policy group

- **WHEN** a claim's picked aggregate is on `cluster="ontap-prod"`, its `volume_labels` match resolved `svm="svm-prod"`, its in-scope QoS workload series carry `policy_group="gold-tier"`, and the fixed-policy series `qos_policy_fixed_max_throughput_iops{cluster="ontap-prod", svm="svm-prod", name="gold-tier"} = 5000` and `qos_policy_fixed_max_throughput_mbps{...} = 250` exist
- **THEN** the edge's `data.metrics` contains `max_iops: 5000` and `max_bytes_per_sec: 262144000`, and the claim's PVC node carries `data.qos` with `policy_group: "gold-tier"`, `max_iops: 5000` and `max_bytes_per_sec: 262144000`
- **AND** `max_iops` is read verbatim, while `max_bytes_per_sec` is the **one** figure in this capability NOT read verbatim: it is `qos_policy_fixed_max_throughput_mbps × 1048576`, so the ceiling carries the same unit as the measured `read_bytes_per_sec` / `write_bytes_per_sec` and the two compare without client-side arithmetic

#### Scenario: Another policy group in the same SVM is never borrowed

- **WHEN** `svm-prod` also holds a `bronze-tier` policy with `max_throughput_iops = 100`, and the claim's workload series carry `policy_group="gold-tier"`
- **THEN** the edge and the PVC node both report `max_iops: 5000` — the ceiling is the volume's own policy group's, and the SVM's other policies are neither consulted nor minimised over
- **AND** the builder never falls back to an SVM-wide or cluster-wide ceiling: a figure resolved from another policy group would name a limit this volume does not have

#### Scenario: Volume in no policy group carries no ceiling

- **WHEN** a claim's in-scope QoS workload series all carry an empty `policy_group` label, while its SVM does hold fixed-policy series for other groups
- **THEN** the edge's `data.metrics` carries its measurement fields and has neither a `max_iops` nor a `max_bytes_per_sec` key, and the PVC node has no `qos` key — an empty match is ignored, never widened to the SVM
- **AND** a claim whose in-scope workload candidates carry no non-empty `policy_group` (the volume is in no policy group, or the Harvest template omits the label) resolves no ceiling

#### Scenario: Claim without an SVM carries no ceiling

- **WHEN** a claim's matched `volume_labels` series carries an empty `svm` label, while its workload series carry a `policy_group` and the ONTAP cluster holds fixed-policy series
- **THEN** the edge's `data.metrics` carries its measurement fields and has neither a `max_iops` nor a `max_bytes_per_sec` key, and the PVC node has no `qos` key — the triple is incomplete and is ignored
- **AND** likewise a claim whose triple matches no fixed-policy series resolves no ceiling

#### Scenario: Partial ceiling keeps only the resolved field

- **WHEN** the claim's triple matches `qos_policy_fixed_max_throughput_iops` but no `qos_policy_fixed_max_throughput_mbps` series
- **THEN** the edge's `data.metrics` contains `max_iops` and no `max_bytes_per_sec` key, and the PVC node's `data.qos` likewise carries `max_iops` and no `max_bytes_per_sec`
- **AND** each field is present iff its own family holds a series for the triple

#### Scenario: Duplicate fixed-policy series pick the smallest value

- **WHEN** a fixed-policy family holds more than one series for one triple
- **THEN** the builder picks deterministically the smallest numeric value

#### Scenario: Absent ceiling is never a number

- **WHEN** no ceiling resolves for a claim
- **THEN** the absence means *no declared ceiling* and is NEVER rendered as a number, whether `0` or an "unlimited" sentinel

#### Scenario: Fixed-policy query failure degrades

- **WHEN** either fixed-policy query fails
- **THEN** the build degrades gracefully and does not fail

### Requirement: Ceiling key assembled from both topology hops

The builder SHALL take the ceiling key's `ontap-cluster` and `svm` from **hop A**, the SVM the `volume_labels` match resolved and the ONTAP cluster it sits on, and its `policy-group` from **hop B**, the `policy_group` label of the claim's in-scope QoS workload candidates read at both granularities, LUN-level rows included.

#### Scenario: Hop A anchors the key on the claim's filer

- **WHEN** a claim's key is assembled
- **THEN** `svm` is the SVM picked as in "PVC svm label re-sourced from the Harvest join", and `ontap-cluster` is the ONTAP cluster of the claim's picked aggregate when an aggregate resolved (the SVM pick is scoped to that filer), otherwise the ONTAP cluster the SVM pick itself landed on
- **AND** the ceiling is therefore anchored on the same filer and SVM the claim's storage chain points at, and stays on the picked filer under a cross-filer FlexVol-name collision

#### Scenario: Hop B is the only statement of the policy group

- **WHEN** a claim's `policy-group` is resolved
- **THEN** it is read from the in-scope candidates — those on the key's ONTAP cluster whose `svm`, when present, equals the key's — since they are the ONLY upstream statement of which policy group governs THIS FlexVol and `volume_labels` carries no policy identity

#### Scenario: Workload without an svm label still resolves its ceiling

- **WHEN** a claim's hop-A match resolved `svm="svm-prod"` on `cluster="ontap-prod"` and its in-scope workload series carry `policy_group="gold-tier"` but no `svm` label of their own
- **THEN** the workload still measures the edge and the ceiling resolves from `(ontap-prod, svm-prod, gold-tier)` — the key's SVM component comes from hop A, not from the workload series

#### Scenario: SAN claim resolves its ceiling through the LUN workload

- **WHEN** a claim's FlexVol carries a volume-level workload in the built-in `User-Best_effort` class (`qos_read_ops` = `150`, no fixed-policy series for that class) and a LUN-level workload in `gold-tier` (`qos_read_ops` = `90`), and `gold-tier` holds fixed-policy series for the claim's `(ontap-cluster, svm)`
- **THEN** the edge reports `read_ops: 150` (the LUN row is not summed) AND resolves `gold-tier`'s ceiling — the policy group is recovered from the LUN row, the only series naming it — and the PVC node's `data.qos.policy_group` is `"gold-tier"`
- **AND** LUN rows are admitted because on a SAN backend the QoS policy is attached to the LUN while the FlexVol's own workload falls into the provisioner-independent built-in class the storage system assigns to unmanaged workloads (`User-Best_effort` on ONTAP), which by definition declares no ceiling and has no fixed-policy series
- **AND** admitting LUN rows cannot affect a measurement, since the I/O sum reads volume-level rows only

### Requirement: Policy-group pick prefers a policy the fixed-policy families hold

The pick of a claim's policy group SHALL **prefer a policy group the fixed-policy families actually hold** for the claim's `(ontap-cluster, svm)`, taking the lexically-smallest such value, and otherwise SHALL fall back to the lexically-smallest non-empty value overall. The preference SHALL be data-driven: the builder SHALL NOT carry a hardcoded list of built-in class names.

#### Scenario: A built-in class sorting first does not win the pick

- **WHEN** a claim's candidates offer both `User-Best_effort` (no fixed-policy series) and `gold-tier` (fixed-policy series present), and `User-Best_effort` sorts lexically before `gold-tier`
- **THEN** the ceiling resolves from `gold-tier` — the pick prefers a policy the fixed-policy families hold over a lexically-smaller one they do not
- **AND** no list of built-in class names is consulted, since their spelling varies by storage-system release

#### Scenario: No held policy falls back to the smallest non-empty value

- **WHEN** none of a claim's candidate policy groups resolves in the fixed-policy families
- **THEN** the pick falls back to the lexically-smallest non-empty value overall, which leaves the ceiling absent exactly as an unmatched key does
- **AND** both passes are order-free

### Requirement: Ceiling surfaces on the PVC node and the storage edge

The resolved ceiling SHALL surface on the claim's PVC node as `data.qos` whenever it resolved, and on the claim's `pvc-to-netapp-aggr` edge as the `max_iops` and `max_bytes_per_sec` fields of `data.metrics` only when that edge exists and carries at least one measurement field, both from the ONE resolution per claim so the two can never disagree.

#### Scenario: The node carries the ceiling independently of measurements

- **WHEN** a claim's ceiling resolves
- **THEN** its PVC node carries it as the `data.qos` attribute (the `graph-api` capability's "PVC `qos` attribute"), whether or not the claim carries any I/O measurement and whether or not an aggregate resolved

#### Scenario: A ceiling field never rides alone on the edge

- **WHEN** a claim's edge carries no measurement field
- **THEN** a ceiling field NEVER appears on it: on the edge the two ride together, or the ceiling is absent there

#### Scenario: No ceiling without a measurement

- **WHEN** a claim draws its edge from `volume_labels` but matches no QoS workload series
- **THEN** the edge has no `metrics` key at all and the PVC node has no `qos` key — with no workload series there is no policy group to key on
- **AND** because the policy group is recovered from a matched workload series, a claim with no in-scope workload series at all resolves no ceiling in either place

#### Scenario: FlexGroup claim resolves its ceiling on the node

- **WHEN** a claim's matched `volume_labels` series carries `svm="svm_big"` on `cluster="ontap-prod"` and an empty `aggr`, its in-scope workload series carry `policy_group="gold-tier"`, and `(ontap-prod, svm_big, gold-tier)` holds fixed-policy series with `max_throughput_iops = 8000`
- **THEN** the claim has no `pvc-to-netapp-aggr` edge, and its PVC node carries `data.qos` with `policy_group: "gold-tier"` and `max_iops: 8000`

#### Scenario: Ceiling on the node when the edge carries no measurement

- **WHEN** a claim resolved an aggregate and SVM, its only in-scope workload series are LUN-level rows carrying `policy_group="gold-tier"` (no volume-level row, so the edge carries no measurement), and `gold-tier` holds fixed-policy series for the claim's `(ontap-cluster, svm)`
- **THEN** the claim's `pvc-to-netapp-aggr` edge has no `metrics` key, and its PVC node carries `data.qos` with `policy_group: "gold-tier"` and the resolved ceiling fields

### Requirement: NetApp aggregate entity

The builder SHALL materialise each ONTAP aggregate referenced by at least one joined claim, ONLY via the PVC join, as one `type="netapp-aggr"` node with `id` `netapp/<ontap-cluster>/aggr/<aggr>`, `name` `<aggr>`, and `labels` exactly `{ontap_cluster: "<ontap-cluster>", node: "<node>"}`, carrying no `cluster` key.

#### Scenario: Aggregate identity and labels

- **WHEN** a claim joins a `volume_labels` series with `cluster="ontap-prod"`, `node="ontap-prod-01"`, `aggr="aggr1"`
- **THEN** the graph contains a node with `id="netapp/ontap-prod/aggr/aggr1"`, `name="aggr1"`, `type="netapp-aggr"`, and `labels={ontap_cluster:"ontap-prod", node:"ontap-prod-01"}` with no `cluster` key
- **AND** because the aggregate belongs to no Kubernetes cluster, its ONTAP cluster name NEVER appears in the response's top-level `clusters[]` array and a `?cluster=` filter never matches it directly

#### Scenario: HA takeover moves ownership, not identity

- **WHEN** the same aggregate `aggr1` is observed owned by `ontap-prod-02` in a later window after a takeover
- **THEN** the node keeps `id="netapp/ontap-prod/aggr/aggr1"` while its `labels.node` (and hence its compound parent) becomes `ontap-prod-02`
- **AND** aggregate names are cluster-wide unique in ONTAP, and the id deliberately **excludes the owning node**

#### Scenario: Owning controller drives compound nesting

- **WHEN** an aggregate is materialised
- **THEN** its `labels.node` is the controller **currently owning** the aggregate, from the matched `volume_labels` series' `node` label, and drives the compound nesting under the real `netapp-node` (graph-api "Cytoscape compound node grouping")

#### Scenario: Disagreeing owners pick deterministically

- **WHEN** matched `volume_labels` series disagree on the owning `node` for one aggregate (e.g. a takeover inside the window)
- **THEN** the builder picks the lexically-smallest non-empty `node` value, so the emitted labels are byte-stable across rebuilds

#### Scenario: Unreferenced aggregate not materialised

- **WHEN** Harvest reports `volume_labels` series for an aggregate whose `volume` values suffix-match no claim's PV-name token (no `volume_name` label is read)
- **THEN** no `netapp-aggr` node is materialised for that aggregate
- **AND** an aggregate is never materialised wholesale from Harvest series presence

#### Scenario: Shared filer is one node set across Kubernetes clusters

- **WHEN** a PVC in Kubernetes cluster `cluster-alpha` and a PVC in `cluster-beta` both join `volume_labels` series on aggregate `(ontap-prod, aggr1)`
- **THEN** the graph contains exactly one `netapp/ontap-prod/aggr/aggr1` node with one `pvc-to-netapp-aggr` edge from each PVC, and no ONTAP cluster name appears in `clusters[]`
- **AND** the edges arrive from every Kubernetes cluster that uses the filer, to the same aggregate id

#### Scenario: Aggregate carries no workload attributes

- **WHEN** an aggregate is materialised
- **THEN** it carries none of `ipaddress`, `owner`, `application`, `containers` or `ready_status`

### Requirement: NetApp node entity and health

The builder SHALL materialise each ONTAP controller named by an emitted `netapp-aggr` node's `labels.node`, or selected as a storage-flow root, as a `type="netapp-node"` node with `id` `netapp/<ontap-cluster>/<node>`, `name` `<node>` and `labels` exactly `{ontap_cluster: "<ontap-cluster>"}`. Its `health` SHALL derive from the OPTIONAL `node_new_status`; its `data.hardware` and `data.perf` SHALL follow "NetApp node hardware attribute" and "NetApp node performance attribute".

#### Scenario: Node identity, labels, and health

- **WHEN** an emitted aggregate carries `labels.node="ontap-prod-01"` in ONTAP cluster `ontap-prod` and `node_new_status{cluster="ontap-prod", node="ontap-prod-01"}` has value `1`
- **THEN** the graph contains a node with `id="netapp/ontap-prod/ontap-prod-01"`, `name="ontap-prod-01"`, `type="netapp-node"`, `labels={ontap_cluster:"ontap-prod"}`, and `data.health="online"`
- **AND** `node_new_status` carries the fixed, case-sensitive labels `cluster` (the ONTAP cluster) and `node`, is matched on `(ontap-cluster, node)`, and a sample of `1` means the controller is healthy and yields `data.health = "online"`
- **AND** the node carries no `cluster` key, with the same `clusters[]` / `?cluster=` exclusion as the aggregate

#### Scenario: Unhealthy controller

- **WHEN** `node_new_status` for a materialised controller has value `0`
- **THEN** that node carries `data.health="degraded"`
- **AND** any matched sample other than `1` yields `data.health = "degraded"`

#### Scenario: Absent node-status metric omits the attribute

- **WHEN** no `node_new_status` series matches a materialised NetApp node (or the metric is absent from the window)
- **THEN** that node's `data` has no `health` key — absence is never conflated with `"degraded"`
- **AND** the builder never defaults a missing metric to `"degraded"`, following the `ready_status` absent-vs-Unknown precedent

#### Scenario: Duplicate node-status series derive deterministically

- **WHEN** more than one `node_new_status` series matches one `(ontap-cluster, node)`
- **THEN** any non-`1` sample yields `"degraded"`, order-free

#### Scenario: Node-status query failure degrades

- **WHEN** the `node_new_status` query fails
- **THEN** the `health` attribute is omitted on every node and the build does not fail

#### Scenario: Controller not referenced by any aggregate is not materialised

- **WHEN** `node_new_status` reports a controller that owns no emitted aggregate and is not a storage-flow root
- **THEN** no `netapp-node` node is materialised for it
- **AND** a NetApp node is materialised ONLY via aggregate reference or as a storage-flow root, never wholesale from Harvest series presence

#### Scenario: Controller carries no workload attributes and parents its aggregates

- **WHEN** a controller is materialised
- **THEN** it carries none of `ipaddress`, `owner`, `application`, `containers` or `ready_status`, and acts as the **compound parent** of its aggregates (graph-api "Cytoscape compound node grouping")
- **AND** in `GET /v1/graph` it is the target of no edge, while in the storage-flow graph it is the source of `storage-flow` edges of tier `node-aggr`

#### Scenario: Attributes present on both endpoints

- **WHEN** a controller resolves `hardware` and `perf` and is retained by both `GET /v1/graph` and `GET /v1/storage-graph`
- **THEN** both bodies carry identical `data.hardware` and `data.perf` on that node
- **AND** both attributes are resolved at build time onto the graph, so every in-process engine call carries them identically too

### Requirement: NetApp node hardware attribute

A `netapp-node` SHALL carry a typed, nullable `data.hardware` object `{ model, serial, version, vendor, location }` resolved from the OPTIONAL Harvest info series `node_labels`, matched on `(ontap-cluster, node)` with its sample value ignored, each field taken verbatim from the like-named label; the attribute SHALL NEVER be placed inside `labels`.

#### Scenario: Hardware attribute resolved

- **WHEN** `node_labels{cluster="ontap-prod", node="ontap-prod-01", model="AFF-A400", serial="721234000123", version="9.14.1", vendor="NetApp", location=""}` is present
- **THEN** the node carries `data.hardware={model:"AFF-A400", serial:"721234000123", version:"9.14.1", vendor:"NetApp"}` with no `location` key, and `labels` stays exactly `{ontap_cluster:"ontap-prod"}`
- **AND** `node_labels` carries the labels `cluster`, `node`, and any of `model`, `serial`, `version`, `vendor`, `location`, each field taken from the like-named label
- **AND** a field is **omitted when the label is empty or absent**

#### Scenario: Hardware absent

- **WHEN** no `node_labels` series matches a materialised controller
- **THEN** that node's `data` has no `hardware` key
- **AND** the whole object is likewise omitted when a series matches but no field resolves

#### Scenario: Duplicate hardware series

- **WHEN** more than one `node_labels` series matches one controller
- **THEN** the lexically-smallest non-empty value per field wins

### Requirement: NetApp node performance attribute

A `netapp-node` SHALL carry a typed, nullable `data.perf` object resolved from four OPTIONAL Harvest `system_node` counters, each matched on `(ontap-cluster, node)` and read **verbatim** without `rate()`. The builder SHALL NOT derive `health`, nor the node's `status` verdict, from these counters.

#### Scenario: Performance counters resolved verbatim

- **WHEN** `node_cpu_busy=72.5`, `node_total_ops=18500`, `node_total_latency=830` and `node_total_data=1.2e9` match a materialised controller
- **THEN** the node carries `data.perf={cpu_busy_pct:72.5, total_ops:18500, total_latency_us:830, total_bytes_per_sec:1.2e9}` and its `data.health` is unchanged by those values
- **AND** the fields map `cpu_busy_pct` from `node_cpu_busy`, `total_ops` from `node_total_ops`, `total_latency_us` from `node_total_latency` and `total_bytes_per_sec` from `node_total_data`, the Harvest values being already per-second / percent figures
- **AND** values are JSON numbers rounded to 6 significant digits

#### Scenario: Partial performance counters

- **WHEN** only `node_cpu_busy` matches a controller
- **THEN** the node carries `data.perf={cpu_busy_pct:<value>}` and no other `perf` field
- **AND** each field is independently optional, omitted when its series is absent or its query failed, and the object is omitted when no field resolves

#### Scenario: High CPU does not degrade health

- **WHEN** `node_cpu_busy=99` and `node_new_status=1` match a controller and no alert names it
- **THEN** the node carries `data.health="online"` and `data.status="normal"`
- **AND** thresholds are model- and estate-specific and belong in the operator's alert rules, whose verdicts reach the node through the `alert-overlay` capability and, from there, fold into `status` (graph-api "Node `status` attribute")
- **AND** `health="degraded"` itself DOES fold into `status` as critical

#### Scenario: Node legs degrade independently

- **WHEN** any of the five node legs — `node_labels` plus the four counters — is absent or fails
- **THEN** only its own fields are omitted, the failure is logged and the build continues (log-and-continue)

### Requirement: NetApp aggregate health and usage

Each materialised `netapp-aggr` node SHALL be able to carry two typed, `omitempty` attributes outside `labels` — `health` from the OPTIONAL Harvest series `aggr_new_status` and `usage` from the OPTIONAL `aggr_space_used` / `aggr_space_total` — each matched 1:1 on `(ontap-cluster, aggr)` with NO cross-aggregate derivation. Their absence SHALL omit the attribute and SHALL NOT fail the build.

#### Scenario: Online aggregate with usage

- **WHEN** `aggr_new_status{cluster="ontap-prod", node="ontap-prod-01", aggr="aggr1"}` has value `1`, `aggr_space_used{...aggr="aggr1"}` is `700000000000`, and `aggr_space_total{...aggr="aggr1"}` is `1000000000000`
- **THEN** the `netapp/ontap-prod/aggr/aggr1` node carries `data.health: "online"` and `data.usage: {"used_bytes": 700000000000, "capacity_bytes": 1000000000000}`
- **AND** all three series carry the fixed, case-sensitive labels `cluster`, `node` and `aggr`, and an `aggr_new_status` sample of `1` means the aggregate is online and yields `"online"`
- **AND** `usage` is `{used_bytes, capacity_bytes}`, the **same shape** the PVC gains from kubelet volume stats, with values as JSON numbers (bytes), never strings

#### Scenario: Offline aggregate marks health degraded

- **WHEN** `aggr_new_status` for a materialised aggregate has value `0`
- **THEN** that aggregate node carries `data.health: "degraded"`
- **AND** any `aggr_new_status` sample other than `1` yields `"degraded"`

#### Scenario: Absent status metric omits health

- **WHEN** no `aggr_new_status` series matches a materialised aggregate
- **THEN** that node's `data` has no `health` key — absence is never conflated with `"degraded"`

#### Scenario: Duplicate aggregate series derive deterministically

- **WHEN** more than one series of one aggregate family matches one `(ontap-cluster, aggr)`
- **THEN** `health` is `"degraded"` if any `aggr_new_status` sample is not `1`, order-free, and each `usage` field takes the smallest numeric value

#### Scenario: Partial usage keeps only the resolved field

- **WHEN** an aggregate matches `aggr_space_total` but no `aggr_space_used` series
- **THEN** its `data.usage` equals `{"capacity_bytes": <value>}` with no `used_bytes` key
- **AND** each `usage` field is present iff its own series matched, and the object is present iff at least one field resolved

### Requirement: PVC-to-NetApp-aggregate edge join

For every PVC with a non-empty `volumename` whose derived token matches ("Suffix-only PV-name-to-FlexVol-name derivation") a candidate `volume_labels` series ("Zone preference among a claim's matched series") with a **non-empty `aggr` label**, the builder SHALL emit one directed `pvc-to-netapp-aggr` edge to `netapp/<ontap-cluster>/aggr/<aggr>` from that series' `cluster` and `aggr` labels. Whether the edge is drawn SHALL NOT depend on the QoS families.

#### Scenario: Joined claim emits the edge

- **WHEN** PVC `cluster-alpha/db/data-mongo-0` resolves `volumename="pvc-9f3a"` and a `volume_labels` series carries `volume="trident_pvc_9f3a"`, `cluster="ontap-prod"`, `node="ontap-prod-01"`, `aggr="aggr1"`
- **THEN** the graph contains a directed `pvc-to-netapp-aggr` edge from `cluster-alpha/db/data-mongo-0` to `netapp/ontap-prod/aggr/aggr1` with empty `labels`
- **AND** no separate topology query is issued, the join is rooted at the PV name alone (CSI-provisioned PV names are UUID-derived, so cross-cluster collisions are not a practical concern), and the QoS families decide only what the edge carries

#### Scenario: PVC without a PV name emits no edge

- **WHEN** a PVC entity has no resolved `volumename`
- **THEN** no `pvc-to-netapp-aggr` edge originates from it and the build does not fail

#### Scenario: Matched series with an empty aggr label emits no edge

- **WHEN** a claim's derived token matches a `volume_labels` series whose `aggr` label is empty (a FlexGroup volume spanning aggregates)
- **THEN** no `pvc-to-netapp-aggr` edge is emitted for that claim, the claim is counted by the join-coverage signal, and the build does not fail

#### Scenario: Deterministic pick on conflicting aggregates

- **WHEN** two series matched by one claim's token report `(ontap-prod, aggr-b)` and `(ontap-prod, aggr-a)`
- **THEN** the edge targets `netapp/ontap-prod/aggr/aggr-a` (the lexically-smallest pair) deterministically across rebuilds
- **AND** the same lexically-smallest `(ontap-cluster, aggr)` pick applies when several distinct FlexVol names end with the token (the same FlexVol name on a second filer, or a name carrying a different prefix), so the emitted edge set is byte-stable and independent of vector order

#### Scenario: Edge id stable across rebuilds

- **WHEN** the same `(pvc, netapp-aggr)` join is produced by two consecutive builds for the same window
- **THEN** the edge `id` (UUIDv5 over `<type>|<source>|<target>`) is byte-identical between the two builds
- **AND** the edge carries empty `labels` (`{}`) and a deterministic UUIDv5 `id` with canonical input `<type>|<source>|<target>`

#### Scenario: Edges de-duplicate per claim and aggregate

- **WHEN** several matched series of one claim name the same aggregate
- **THEN** exactly one `pvc-to-netapp-aggr` edge is emitted for that `(pvc, netapp-aggr)` pair

### Requirement: Zone preference among a claim's matched series

A claim's **candidate set** SHALL be the matched `volume_labels` series whose zone agrees with the claim's WHEN AT LEAST ONE agrees, and every matched series otherwise. Two zones SHALL disagree only when BOTH sides carry a complete `(az, env)` pair and the pairs differ; an unknown zone on either side agrees.

#### Scenario: Candidate set and zones

- **WHEN** the builder picks for a claim the aggregate, the SVM, the owning controller the edge's target names, the QoS scope or the ceiling key
- **THEN** every one of those picks runs over the claim's candidate set
- **AND** the claim's zone is the `(az, env)` pair its `kube_persistentvolumeclaim_info` series carries under the configured label keys, a series' zone is the pair it carries likewise, and an unknown zone agrees, the rule the `alert-overlay` capability applies to alerts

#### Scenario: Set-aside series still feed the inventory

- **WHEN** the preference sets a matched series aside for a claim
- **THEN** it narrows that claim's candidates only: the set-aside series still names its aggregate, controller and SVM for the owner vote and the storage-flow inventory

#### Scenario: Another zone's colliding FlexVol is never a candidate

- **WHEN** claim `zone-a-prod-c1/shop/orders-data` (its claim-info series carrying `az="zone-a",env="prod"`) resolves `volumename="pvc-9f3a"`, and two `volume_labels` series end with its token: `(ontap-a, aggr-z)` carrying `az="zone-a",env="prod"` and `(ontap-0, aggr-a)` carrying `az="zone-b",env="prod"`
- **THEN** the edge targets `netapp/ontap-a/aggr/aggr-z` although `(ontap-0, aggr-a)` sorts first, and the claim's `svm` label, QoS measurement and ceiling all come from the `ontap-a` series
- **AND** a token collision across zones (the same FlexVol name, or a clone, on filers of two zones) therefore does not move a claim onto another zone's filer while a series of its own zone exists, which keeps a multi-zone `/v1/storage-graph` body the union of its single-zone bodies for every claim that has such a series

#### Scenario: A claim with no agreeing series joins among every matched series

- **WHEN** the same claim's only matching series are `(ontap-b, aggr-q)` carrying `az="zone-b",env="prod"` and `(ontap-0, aggr-a)` carrying `az="zone-c",env="prod"`
- **THEN** neither agrees with the claim's `zone-a` / `prod`, the candidate set is both, and the edge targets `netapp/ontap-0/aggr/aggr-a` (the lexically-smallest pair) — the claim is not left without its storage topology
- **AND** such a claim — e.g. on a filer labelled with another `(az, env)` pair, as the Harvest side carries no request matcher — still joins

#### Scenario: An unknown zone never excludes

- **WHEN** the same claim's only other matching series carries no `az` label
- **THEN** that series stays in the candidate set and the lexically-smallest pair is picked across both, exactly as before this rule

#### Scenario: The pod graph changes only under a cross-zone collision

- **WHEN** an unfiltered `/v1/graph` build runs over an estate holding no cross-zone token collision
- **THEN** the zone preference leaves the body unchanged; it changes the body only in an estate holding such a collision

### Requirement: I/O measurements on the storage edge

Each `pvc-to-netapp-aggr` edge SHALL be able to carry up to six I/O measurements in its `data.metrics` object, each present iff its own Harvest QoS workload family matched at least one series for the join key and absent otherwise; an edge on which every I/O field is absent SHALL carry no `metrics` key at all.

#### Scenario: Measurement field mapping

- **WHEN** a claim's matched volume-level QoS rows (the reader keeps only rows with an empty `lun`; the query carries no `lun=""` matcher, see "Harvest QoS workload series as the I/O source") are mapped onto the edge
- **THEN** `read_ops` ← `qos_read_ops` (read requests per second), `write_ops` ← `qos_write_ops` (write requests per second), `read_latency_us` ← `qos_read_latency` (average read latency in microseconds), `write_latency_us` ← `qos_write_latency` (average write latency in microseconds), `read_bytes_per_sec` ← `qos_read_data` (read throughput in bytes per second) and `write_bytes_per_sec` ← `qos_write_data` (write throughput in bytes per second), each verbatim

#### Scenario: All six measurements present

- **WHEN** all six Harvest QoS families carry a volume-level series for the joined PV name
- **THEN** the edge's `data.metrics` contains numeric `read_ops`, `write_ops`, `read_latency_us`, `write_latency_us`, `read_bytes_per_sec`, and `write_bytes_per_sec`, and none of `rate` / `error_rate` / `p90_server_ms`
- **AND** the RED fields (`rate`, `error_rate`, `p90_server_ms`) NEVER appear on a `pvc-to-netapp-aggr` edge

#### Scenario: Missing family omits only its field

- **WHEN** the joined PV name matches `qos_read_ops`, `qos_write_ops`, `qos_read_data`, and `qos_write_data` series but no latency series
- **THEN** the edge's `data.metrics` contains `read_ops`, `write_ops`, `read_bytes_per_sec`, and `write_bytes_per_sec` and has no `read_latency_us` or `write_latency_us` key
- **AND** an absent field is distinct from `0`

#### Scenario: Verbatim values, no rate() derivation

- **WHEN** the matched `qos_read_ops` series' value is `150`, the matched `qos_read_latency` series' value is `830`, and the matched `qos_read_data` series' value is `5242880`
- **THEN** the edge reports `read_ops: 150`, `read_latency_us: 830`, and `read_bytes_per_sec: 5242880` — the upstream values verbatim, not re-derived

#### Scenario: Several matched series are summed order-independently

- **WHEN** a family matches more than one series for one join key
- **THEN** the value is summed over the matched series in ascending order, so the result is order-independent
- **AND** values are JSON numbers rounded to 6 significant digits at serialisation (graph-api "Edge `metrics` attribute") and MAY appear in exponent form

#### Scenario: Measurements and ceiling share one object

- **WHEN** a joined claim matches all six QoS families and its `(ontap-cluster, svm, policy-group)` triple resolves both fixed-policy series
- **THEN** the edge's single `data.metrics` object carries the six measurements together with `max_iops` and `max_bytes_per_sec`, and no RED field
- **AND** the ceiling fields are the declared ceiling of the policy group governing the volume, keyed by `(ontap_cluster, svm, policy_group)`, under the separate presence rules of "QoS fixed-policy throughput ceilings": measurements and ceiling are ONE family, and a single edge MAY carry any subset of the eight fields, subject to the rule that no ceiling field appears without at least one measurement

### Requirement: PVC svm label re-sourced from the Harvest join

When the PVC join resolves, the builder SHALL set the PVC's `svm` label from the `svm` label of the matched `volume_labels` series, its ONLY source, picking the lexically-smallest non-empty `svm` among matched series on the picked aggregate's ONTAP cluster, or among every matched series when no aggregate resolved. The key SHALL be set only when the resolved value is non-empty.

#### Scenario: A QoS series on another SVM neither overrides nor measures

- **WHEN** a claim's `volume_labels` series carries `svm="svm-a"` while the only QoS series for its PV name carries `svm="svm-b"`
- **THEN** the PVC entity's `svm` label is `svm-a`, the edge carries no `metrics` key, and the claim is counted by the I/O-coverage signal
- **AND** no Trident custom-resource chain is consulted, and the QoS series' own `svm` label is read solely to SCOPE a workload candidate to the claim's own volume ("Harvest QoS workload series as the I/O source") and NEVER serves as a fallback, so the emitted label cannot depend on which I/O families matched
- **AND** the label participates in NO join key: the fixed-policy ceiling takes its `svm` component from hop A, and reads hop B only for the `policy_group` label

#### Scenario: Cross-filer collision resolves svm on the picked filer

- **WHEN** a claim's derived token matches `volume_labels` series on two filers — `(cluster="ontap-a", aggr="aggr1", svm="zulu")` and `(cluster="ontap-b", aggr="aggr9", svm="alpha")` — and `ontap-a` also hosts an unrelated SVM named `alpha` carrying its own fixed-policy series
- **THEN** the picked aggregate is `netapp/ontap-a/aggr/aggr1`, the PVC entity's `svm` label is `zulu` (not the lexically-smaller `alpha` from the other filer), the `ontap-a`/`zulu` workload series stay in scope and measure the edge, and the ceiling resolves from `(ontap-a, zulu)` — never from the unrelated `alpha` tenant
- **AND** the pick is **scoped to the ONTAP cluster the aggregate pick landed on** because the resolved value is paired with that same ONTAP cluster twice over — it scopes every QoS workload candidate, and it is half of the hop-C ceiling key — so an unscoped pick could hand the OTHER filer's SVM to both, dropping the edge's I/O outright and attaching an unrelated tenant's ceiling whenever the picked filer hosts a same-named SVM

#### Scenario: FlexGroup claim keeps its svm with no filer to scope to

- **WHEN** a claim's only matched `volume_labels` series carries an empty `aggr` label
- **THEN** no aggregate resolves and no edge is emitted, and the `svm` label still resolves from that series — the cluster scope applies only when an aggregate pick supplied a filer
- **AND** the same unscoped pick over every matched series applies whenever no matched series carries both a `cluster` and an `aggr`, since there is then neither an edge nor a ceiling to scope to

#### Scenario: svm resolved from the joined series

- **WHEN** PVC `cluster-alpha/db/data-mongo-0` joins a `volume_labels` series carrying `svm="svm-prod"`
- **THEN** the PVC entity's `labels` contains `volumename` and `svm="svm-prod"`

#### Scenario: Join miss yields no svm

- **WHEN** a PVC resolves `volumename="pvc-9f3a"` but its derived token matches no `volume_labels` series
- **THEN** the PVC entity carries `volumename` but no `svm` key, and the build does not fail

#### Scenario: Empty svm label on the joined series

- **WHEN** the joined `volume_labels` series carries an empty `svm` label
- **THEN** the PVC entity carries no `svm` key — never an empty-string value
- **AND** the key is absent, never empty-string, whenever no non-empty value resolves

#### Scenario: svm never appears without volumename

- **WHEN** a PVC has no resolved `volumename`
- **THEN** it carries no `svm` key, because the join is rooted at the PV name

### Requirement: PVC aggr label from the Harvest join

When the PVC join resolves an aggregate, the builder SHALL set the PVC's `aggr` label to that aggregate's node id `netapp/<ontap-cluster>/aggr/<aggr>`, reusing the SAME pick as "PVC-to-NetApp-aggregate edge join" and never picking a second time, with the matched `volume_labels` series as the ONLY source. The key SHALL be absent, never an empty string, when no aggregate resolved.

#### Scenario: The label names the edge's target

- **WHEN** PVC `cluster-alpha/db/data-mongo-0` resolves `volumename="pvc-9f3a"` and a `volume_labels` series carries `volume="trident_pvc_9f3a"`, `cluster="ontap-prod"`, `node="ontap-prod-01"`, `aggr="aggr1"`, `svm="svm-prod"`
- **THEN** the PVC entity's `labels` contains `aggr="netapp/ontap-prod/aggr/aggr1"` and `svm="svm-prod"`, and its `pvc-to-netapp-aggr` edge targets `netapp/ontap-prod/aggr/aggr1`

#### Scenario: Conflicting aggregates follow the edge's pick

- **WHEN** two series matched by one claim's token report `(ontap-prod, aggr-b)` and `(ontap-prod, aggr-a)`
- **THEN** the PVC entity's `aggr` label is `netapp/ontap-prod/aggr/aggr-a`, the same node its edge targets, deterministically across rebuilds
- **AND** the label and the edge cannot disagree, even when the matched series disagree with each other, because the label reuses the edge's lexically-smallest `(ontap-cluster, aggr)` pick among the claim's matched series with a non-empty `aggr`

#### Scenario: No QoS family contributes to the aggr label

- **WHEN** a claim's QoS workload series carry labels naming an aggregate
- **THEN** the `aggr` label still comes from the matched `volume_labels` series alone, whatever labels the QoS series carry

#### Scenario: A FlexGroup claim resolves svm but no aggr

- **WHEN** a claim's only matched `volume_labels` series carries `svm="svm_big"` and an empty `aggr` label
- **THEN** the PVC entity carries `svm="svm_big"` and no `aggr` key, and no `pvc-to-netapp-aggr` edge is emitted

#### Scenario: A join miss yields no aggr

- **WHEN** a PVC resolves `volumename="pvc-9f3a"` but its derived token matches no `volume_labels` series
- **THEN** the PVC entity carries `volumename` but no `aggr` key, and the build does not fail

#### Scenario: No aggr without a PV name or a label series

- **WHEN** a PVC has no `volumename`, or the window holds no `volume_labels` series
- **THEN** the PVC carries no `aggr` key

#### Scenario: An empty svm label does not suppress aggr

- **WHEN** the joined `volume_labels` series carries `aggr="aggr1"` on `cluster="ontap-prod"` and an empty `svm` label
- **THEN** the PVC entity carries `aggr="netapp/ontap-prod/aggr/aggr1"` and no `svm` key
- **AND** the `aggr` label is independent of `svm`: a joined series with an empty `svm` label still resolves the aggregate, and a FlexGroup claim resolves `svm` with no aggregate

### Requirement: Join-coverage observability

The builder SHALL count, per build, claims missing their storage topology as `netapp_volume_join_miss` and claims missing every I/O measurement as `netapp_qos_join_miss`, surfacing each as ONE aggregated warning log carrying its count rather than one log per claim. Each signal SHALL be gated on its OWN family being read in the build, and neither SHALL ever fail the build.

#### Scenario: Unjoined claims counted and warned once

- **WHEN** a build reads a non-empty `volume_labels` vector and three PVCs with non-empty `volumename` derive tokens matching no series
- **THEN** the build succeeds and emits one `netapp_volume_join_miss` warning carrying the count `3`
- **AND** this follows the `failed_total_label_set_mismatch` precedent of one aggregated log per build

#### Scenario: A derivation that does not fit the estate is visible

- **WHEN** the estate's FlexVol names embed no PV name at all and every claim therefore fails to match
- **THEN** the build succeeds, no storage chain is emitted, and one `netapp_volume_join_miss` warning carries the full claim count — the misfit is reported rather than silent
- **AND** **topology coverage** (`netapp_volume_join_miss`) counts a PVC with a non-empty `volumename` whose derived token matched no `volume_labels` series, or matched only series whose `aggr` label is empty, **while at least one `volume_labels` series was read in the build**; it is the operator's primary signal that the configured derivation does not fit the estate's FlexVol naming

#### Scenario: Empty-aggr matches count toward the topology signal

- **WHEN** a build reads a non-empty `volume_labels` vector and one PVC's only matched series carries an empty `aggr` label
- **THEN** that claim is included in the `netapp_volume_join_miss` count even though its `svm` label may have resolved
- **AND** no aggregate edge can be drawn for such a claim

#### Scenario: Measurement-less edges counted separately

- **WHEN** a build issues its scoped QoS queries, reads a non-empty QoS workload vector, and two claims draw their aggregate edge but match no QoS workload series
- **THEN** the build succeeds, both edges are emitted without a `metrics` key, one `netapp_qos_join_miss` warning carries the count `2`, and the `netapp_volume_join_miss` count is unaffected
- **AND** **I/O coverage** (`netapp_qos_join_miss`) counts a claim that DID draw its `pvc-to-netapp-aggr` edge but matched no series in any of the six QoS workload families, **while at least one QoS workload series was read in the build**

#### Scenario: Volume template without the QoS template

- **WHEN** a build matches claims against `volume_labels`, issues its scoped QoS queries, and every one returns zero series
- **THEN** every joined claim's edge is emitted without a `metrics` key and no `netapp_qos_join_miss` warning is emitted
- **AND** the deployment still gets its storage topology: "at least one QoS workload series was read" means at least one issued chunk of at least one QoS family returned series

#### Scenario: Non-NetApp deployment stays silent

- **WHEN** a build reads zero `volume_labels` series while PVCs carry `volumename` labels, so no QoS query is issued at all
- **THEN** neither warning is emitted and the build succeeds
- **AND** absence of the upstream is not a coverage failure, and a build that issued no QoS query at all emits no I/O-coverage warning

#### Scenario: Full coverage emits no warning

- **WHEN** every PVC with a non-empty `volumename` matches a `volume_labels` series with a non-empty `aggr` label and matches at least one QoS workload family
- **THEN** neither warning is emitted

#### Scenario: An unresolved ceiling is not a coverage miss

- **WHEN** a joined claim resolves no throughput ceiling
- **THEN** no signal is emitted for it, since a volume in no QoS policy group is the normal case, not a defect

#### Scenario: StorageClass is left to the operator

- **WHEN** a claim with a non-empty `volumename` fails to join
- **THEN** the builder does not interpret StorageClass names or filter either count by them
- **AND** the PVC's retained `data.storageclass` value is the operator's discriminator between "this claim was never meant to have a NetApp backend" and "this claim should have joined and did not"

### Requirement: Harvest legs are zone-routed and carry no request matcher

Every NetApp Harvest query the builder issues SHALL carry NO request-scoped matcher: none of `az`, `env`, `cluster` or `namespace` SHALL reach a Harvest query as a label matcher. A Harvest series SHALL NOT be required to carry the configured `az` / `env` labels, and those labels SHALL never be used for exclusion at the query.

#### Scenario: The Harvest legs

- **WHEN** the builder issues `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency` or `node_total_data`
- **THEN** the query carries no `az`, `env`, `cluster` or `namespace` matcher
- **AND** Harvest's `cluster` label is the **ONTAP** cluster name and never a Kubernetes cluster, so a Kubernetes `cluster` value pushed into it would match nothing, and Harvest carries no `namespace` at all

#### Scenario: Zone labels are used only after the read

- **WHEN** a Harvest series carries the configured `az` / `env` labels
- **THEN** they are used for zone preference ("Zone preference among a claim's matched series", "One zone per QoS workload") and for the zone-agreeing alert match, never for exclusion at the query
- **AND** a series without them is read like any other

#### Scenario: A data- or root-derived scope is not a request matcher

- **WHEN** a Harvest query carries a scope derived from upstream data or from the request's ROOT
- **THEN** that is a different mechanism from a request-scoped matcher and is permitted: on `/v1/graph` one Harvest family carries one, the `qos_*` families' `volume` alternation, and on `/v1/storage-graph` EVERY Harvest family carries one, because that build reads each by reference (`storage-graph-api`, "Storage build reads every family by reference")
- **AND** no such scope appears in the request-scoped selector table

#### Scenario: Cluster and namespace filters never reach Harvest

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** no Harvest query carries a `cluster` or `namespace` matcher; the request carries neither `az` nor `env` and no storage-side root, so the `volume_labels`, aggregate, controller, hardware, performance and policy queries are issued exactly as in an unfiltered build; the QoS queries restrict `volume` to the FlexVol names matched by the loaded `cluster-alpha` / `shop` claims alone; and the aggregates in the response are exactly those those claims matched

#### Scenario: Environment filter does not reach Harvest

- **WHEN** a build runs with `env={prod}` against a single `harvest` backend holding series stamped `env="prod"` and `env="dev"`
- **THEN** no Harvest query carries an `<env-key>` matcher, series of both environments are returned, and a loaded `prod` claim whose derived token matches only a `volume_labels` series stamped `env="dev"` receives its `pvc-to-netapp-aggr` edge to that series' aggregate

#### Scenario: Harvest lacking the zone labels under a filter

- **WHEN** the kube-state-metrics series carry `az="zone-a"`, the Harvest series carry no `az` and no `env` label at all, and a build runs with `az={zone-a}` or `env={prod}`
- **THEN** every Harvest leg returns its rows, the `prod` / `zone-a` claims receive their `pvc-to-netapp-aggr` edges, `netapp-aggr` / `netapp-node` nodes and measurements exactly as an unfiltered build gives them, and no warning fires

### Requirement: Harvest family is zone-routed but unmatched

The Harvest queries SHALL constitute the `harvest` query family of the `upstream-backend-routing` capability: a request's `az` values SHALL select which `harvest` backends are asked, under the same rule as the `ksm` and `kubelet` families, without the zone also being rendered as a matcher. Routing SHALL change neither the query string, the three-hop join, nor its per-hop degradation.

#### Scenario: Harvest served by its own upstream

- **WHEN** the routing table declares one backend serving `harvest` at `http://vm-netapp.example:8428` and another serving every other family at `http://vm-k8s.example:8428`
- **THEN** every Harvest query — including each chunk of the scoped QoS read and the five node legs — is issued only to `http://vm-netapp.example:8428`, the kube-state-metrics and kubelet queries only to `http://vm-k8s.example:8428`, and the resulting `pvc-to-netapp-aggr` edges join claims read from one upstream to volumes read from the other
- **AND** the `harvest` family MAY therefore be served by a different upstream installation from the kube-state-metrics and kubelet legs

#### Scenario: Zone filter routes Harvest without a matcher

- **WHEN** two backends serve `harvest`, declaring `zones: [zone-a]` and `zones: [zone-b]`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query — including `node_labels` and the four `system_node` counters — is issued only to the `zone-a` backend as the same query string an unfiltered build issues, carrying no `<az-key>` matcher; a `volume_labels` series held by the `zone-b` backend is not loaded even if a loaded claim's derived token would match it
- **AND** the backends asked are those whose `zones` intersect the request, plus any catch-all

#### Scenario: Catch-all Harvest backend under a zone filter

- **WHEN** one backend serves `harvest` with no `zones` declared, holds `volume_labels` series stamped `az="zone-a"` and `az="zone-b"`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued to that backend with no `az` matcher, series of both zones are returned, and the aggregates in the response are exactly those matched by the loaded `zone-a` claims — a `zone-a` claim resolves among the `zone-a` series when it has one
- **AND** the store boundary is the zone filter, so a catch-all backend or a store holding several zones returns every zone's series

#### Scenario: Unfiltered build merges Harvest across backends

- **WHEN** two backends serve `harvest` for different zones and a build runs with no `az` value
- **THEN** every Harvest query is issued to both and the results are merged, so aggregates from both zones can match their claims in one graph

#### Scenario: Environment has no routing counterpart

- **WHEN** a build runs with an `env` value and several `harvest` backends
- **THEN** the `env` dimension selects no backend and renders no matcher: it has neither routing nor a matcher counterpart on Harvest

### Requirement: Harvest reads are narrowed by reference

A request's `cluster`, `namespace`, `az` and `env` filters SHALL reach the NetApp graph solely through the claims they load (and, for `az`, the backends it selects): an aggregate and its owning controller SHALL materialise only when a **loaded** claim's derived token matches a `volume_labels` series, or, in the storage-flow graph, when selected as a root.

#### Scenario: The QoS alternation is derived from upstream data

- **WHEN** a filtered build issues its `qos_*` queries
- **THEN** their single selector, the `volume` alternation of "Scoped and batched QoS workload read", holds FlexVol names the volume-label family already returned, derived from upstream data and not from the request
- **AND** it is nonetheless influenced by the request, because the claims whose tokens produced those names are loaded under the request's selectors: this is the "narrowed by reference" principle realised at the query layer rather than only in the parse

#### Scenario: Pod-graph Harvest legs are issued as unfiltered

- **WHEN** a filtered `/v1/graph` build issues its Harvest queries
- **THEN** `volume_labels` and every other Harvest query except the `qos_*` alternation are issued exactly as in an unfiltered build

#### Scenario: Storage-graph Harvest legs carry by-reference scopes

- **WHEN** a `/v1/storage-graph` build issues its Harvest queries
- **THEN** `volume_labels` carries the root restriction (by ONTAP cluster, aggregate, SVM or controller), the token restriction of the tracked claims, or the whole-aggregate restriction of owner completion
- **AND** the aggregate gauge families carry the `(ONTAP cluster, aggregate)` pairs the build reached or the root names, the controller families carry the `(ONTAP cluster, controller)` pairs the build reached or the root names, and the two fixed-policy families carry the `(ONTAP cluster, SVM)` pairs of the tracked claims — the same principle, read from whichever end of the chain the root sits on

#### Scenario: Shared filer reached by reference from either filtered cluster

- **WHEN** claims in `cluster-alpha` and `cluster-beta` both match `netapp/ontap-prod/aggr/aggr1` and a build runs with `cluster={cluster-alpha}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` are materialised with a `pvc-to-netapp-aggr` edge from the `cluster-alpha` claim only; a `cluster={cluster-beta}` build materialises the same two nodes from the `cluster-beta` claim
- **AND** a filer shared across clusters, zones, or environments is one node set, reached from whichever loaded claims match it

#### Scenario: Hub mode loads claims from the rooted rows

- **WHEN** a `/v1/storage-graph` build rooted at `ontap_cluster=ontap-prod` runs with `az={zone-a}`, `env={prod}`, and the `zone-a` `harvest` backend returns two rooted FlexVols, one whose claim lives in a `zone-a` / `prod` cluster and one whose claim lives in an `env="dev"` cluster
- **THEN** the `prod` claim is loaded and receives its aggregate and SVM exactly as an unfiltered build would give them, and the `dev` claim is not loaded, because every claim query carries `<env-key>="prod"` — the Harvest read that returned both FlexVols carried no `az` / `env` matcher
- **AND** in a `/v1/storage-graph` build rooted at a storage component (`storage-graph-api`, "Every root kind is tracked from its own tier to its claims") the direction reverses: the claims are loaded FROM the rooted volume-label rows, under the request's `az`, `env`, `cluster` and `namespace` matchers on the Kubernetes side, so a rooted FlexVol whose claim lives in another zone or environment loads no claim

#### Scenario: A multi-zone read resolves each claim in its own zone

- **WHEN** an unfiltered build, or a `/v1/storage-graph` build selecting several zones or environments, reads the Harvest series of every backend selected, or a catch-all store returns several zones
- **THEN** a FlexVol name carried by volumes in two zones or environments resolves, for each claim, among its own zone's series ("Zone preference among a claim's matched series")
- **AND** it falls back to the lexically-smallest `(ontap_cluster, aggr)` over every candidate when no series agrees with the claim's zone or a side carries no zone

### Requirement: NetApp SVM entity

The builder SHALL resolve each joined claim's SVM exactly as the PVC `svm` label is picked, and SHALL be able to materialise it, only in the storage-flow graph and NEVER in `GET /v1/graph`, as a `type="netapp-svm"` node with `id` `netapp/<ontap-cluster>/svm/<svm>`, `name` `<svm>` and `labels` exactly `{ontap_cluster: "<ontap-cluster>"}`.

#### Scenario: SVM identity and labels

- **WHEN** a claim joins a `volume_labels` series with `cluster="ontap-prod"`, `svm="svm_shop"` and the storage-flow graph retains its path
- **THEN** the body contains `{id:"netapp/ontap-prod/svm/svm_shop", name:"svm_shop", type:"netapp-svm", labels:{ontap_cluster:"ontap-prod"}}`
- **AND** SVM names are unique within an ONTAP cluster, and the labels carry no `cluster` key, with the same `clusters[]` / `?cluster=` exclusion as the aggregate and controller

#### Scenario: SVM materialised as a tier or a root

- **WHEN** the storage-flow graph (`storage-graph-api`) retains a claim's path, or selects an SVM as a root
- **THEN** the SVM is materialised as a tier of that path, or as the root
- **AND** its pick is the PVC `svm` pick: the `svm` label of the matched `volume_labels` series, scoped to the picked aggregate's ONTAP cluster, lexically-smallest on conflict

#### Scenario: SVM spans aggregates

- **WHEN** two claims in `svm_shop` join aggregates `aggr1` and `aggr2` on the same ONTAP cluster
- **THEN** exactly one `netapp/ontap-prod/svm/svm_shop` node is materialised, with an `aggr-svm` edge arriving from each aggregate

#### Scenario: Not emitted by the pod graph

- **WHEN** the same estate is served by `GET /v1/graph`
- **THEN** the body contains no `type="netapp-svm"` node and no `storage-flow` edge
- **AND** the `GET /v1/graph` body stays free of SVM nodes

#### Scenario: SVM carries no other attribute

- **WHEN** an SVM node is materialised
- **THEN** it carries none of `ipaddress`, `owner`, `application`, `containers`, `ready_status`, `health`, `usage`, `hardware`, `perf`, `alerts` or `status`

#### Scenario: A claim with an empty svm draws no storage-flow path

- **WHEN** a claim's matched series carries an empty `svm`
- **THEN** it produces no SVM node and contributes no storage-flow path at all, since the tier chain is fixed and an `aggr → pvc` shortcut is not permitted
- **AND** such a claim is counted like a topology miss for that graph
