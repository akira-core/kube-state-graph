## ADDED Requirements

### Requirement: PVC aggr label from the Harvest join

When the PVC join resolves an aggregate, the builder SHALL set the PVC entity's `aggr` label to that aggregate's node id, `netapp/<ontap-cluster>/aggr/<aggr>` — the target of the claim's `pvc-to-netapp-aggr` edge, taken from the SAME deterministic pick described in "PVC-to-NetApp-aggregate edge join" (the lexically-smallest `(ontap-cluster, aggr)` pair among the claim's matched series with a non-empty `aggr`). The aggregate SHALL NOT be picked a second time for the label, so the label and the edge cannot disagree, even when the matched series disagree with each other. The matched `volume_labels` series is the ONLY source: no QoS workload family contributes to the label, whatever labels its series carry.

The key SHALL be set only when an aggregate resolved, and SHALL be absent — never an empty string — otherwise: for a claim with no `volumename`, a join miss, a FlexGroup volume whose matched series carries an empty `aggr`, and a window without `volume_labels`. It is independent of `svm`: a joined series with an empty `svm` label still resolves the aggregate, and a FlexGroup claim resolves `svm` with no aggregate.

#### Scenario: The label names the edge's target

- **WHEN** PVC `cluster-alpha/db/data-mongo-0` resolves `volumename="pvc-9f3a"` and a `volume_labels` series carries `volume="trident_pvc_9f3a"`, `cluster="ontap-prod"`, `node="ontap-prod-01"`, `aggr="aggr1"`, `svm="svm-prod"`
- **THEN** the PVC entity's `labels` contains `aggr="netapp/ontap-prod/aggr/aggr1"` and `svm="svm-prod"`, and its `pvc-to-netapp-aggr` edge targets `netapp/ontap-prod/aggr/aggr1`

#### Scenario: Conflicting aggregates follow the edge's pick

- **WHEN** two series matched by one claim's token report `(ontap-prod, aggr-b)` and `(ontap-prod, aggr-a)`
- **THEN** the PVC entity's `aggr` label is `netapp/ontap-prod/aggr/aggr-a`, the same node its edge targets, deterministically across rebuilds

#### Scenario: A FlexGroup claim resolves svm but no aggr

- **WHEN** a claim's only matched `volume_labels` series carries `svm="svm_big"` and an empty `aggr` label
- **THEN** the PVC entity carries `svm="svm_big"` and no `aggr` key, and no `pvc-to-netapp-aggr` edge is emitted

#### Scenario: A join miss yields no aggr

- **WHEN** a PVC resolves `volumename="pvc-9f3a"` but its derived token matches no `volume_labels` series
- **THEN** the PVC entity carries `volumename` but no `aggr` key, and the build does not fail

#### Scenario: An empty svm label does not suppress aggr

- **WHEN** the joined `volume_labels` series carries `aggr="aggr1"` on `cluster="ontap-prod"` and an empty `svm` label
- **THEN** the PVC entity carries `aggr="netapp/ontap-prod/aggr/aggr1"` and no `svm` key

## MODIFIED Requirements

### Requirement: Harvest volume-label series as the storage topology source

The builder SHALL consume the NetApp Harvest volume-object label series `volume_labels` from the same centralised VictoriaMetrics endpoint as every other series. The fixed, case-sensitive label contract it MUST carry: `cluster` (the ONTAP cluster name — NOT a Kubernetes cluster; the two namespaces never mix), `node` (the ONTAP controller currently owning the containing aggregate), `aggr` (the containing aggregate), `svm` (the serving Storage Virtual Machine), and `volume` (the ONTAP FlexVol name). It is an **info series**: its sample value SHALL be ignored entirely and only its label set consumed.

Every label in that contract is **stock Harvest output**. The builder SHALL NOT require the deployment to install a Prometheus relabel rule, and SHALL NOT read any non-stock label naming the Kubernetes PersistentVolume.

This one series is the SOLE source of the graph's storage topology — the `pvc-to-netapp-aggr` edge, the `netapp-aggr` and `netapp-node` entities, and the PVC `svm` and `aggr` labels all derive from it and from nothing else. The I/O measurements and the throughput ceiling ride on separate families (the two requirements below) and SHALL NOT contribute to any topological decision; conversely, a claim SHALL NEVER lose its storage topology because an I/O family failed to match.

The issued query SHALL read the series at the window end without `rate()`, in the same shape as every other Harvest leg, and SHALL carry no restriction on `volume` — the set of interesting FlexVol names is not known until this family has been read.

The bridge from a claim's PV name to this family's `volume` label is the derivation described in "PV-name-to-FlexVol-name derivation". The graph inherits three blind spots from it: a FlexVol whose name does not embed the claim's PV name under the configured derivation never joins (its claim's `svm` and `aggr` are absent and no edge is drawn); the Trident "economy" drivers pack many claims into one shared FlexVol, so no per-claim series exists at all; and a FlexGroup volume spans aggregates, so its series carries no single usable `aggr` label (no aggregate edge can be drawn and its PVC carries no `aggr` label — see the join-coverage requirement).

The family is OPTIONAL. When it is absent from the window — the normal case for a deployment without NetApp Harvest — the builder SHALL produce a valid graph with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` or `aggr` labels; PVC `volumename` labels are unaffected and the build SHALL NOT fail.

#### Scenario: Volume label series consumed for its labels only

- **WHEN** the builder issues the `volume_labels` query for a window
- **THEN** the query references the bare series evaluated at the window end, does not wrap it in `rate()`, carries no `volume` restriction, and the resolver derives the aggregate, owning controller, and SVM from the matched series' labels while its sample value plays no part in any output

#### Scenario: Stock Harvest output joins without a relabel rule

- **WHEN** the upstream carries `volume_labels` exactly as stock Harvest emits it, with no deployment-installed relabel rule and no label naming the Kubernetes PersistentVolume
- **THEN** claims whose derived tokens match the `volume` label still resolve their aggregate, controller and `svm`, their PVCs carry the `aggr` label, and their `pvc-to-netapp-aggr` edges are emitted

#### Scenario: Harvest absent entirely

- **WHEN** the upstream contains topology series but no `volume_labels` series for the window
- **THEN** the build completes successfully with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` or `aggr` labels, while PVC `volumename` labels still resolve from `kube_persistentvolumeclaim_info`

#### Scenario: I/O families present without the label series

- **WHEN** the upstream carries QoS workload series whose `volume` matches a claim's derived token but no `volume_labels` series matches it
- **THEN** no `pvc-to-netapp-aggr` edge is emitted for that claim, its PVC carries no `aggr` label, no aggregate or controller is materialised from the QoS series, and the build does not fail
