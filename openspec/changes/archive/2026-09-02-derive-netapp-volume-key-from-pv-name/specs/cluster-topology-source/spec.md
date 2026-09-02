## MODIFIED Requirements

### Requirement: PVC PersistentVolume name and NetApp SVM labels

The topology reader SHALL resolve each PVC's bound **PersistentVolume name** and surface it, together with the **ONTAP SVM** serving it when the NetApp Harvest join resolves, as additive entries in the PVC entity's `labels` map (strict `map[string]string`):

- `volumename` — the bound PV name, read from the `volumename` label of `kube_persistentvolumeclaim_info`, joined on `(cluster, namespace, persistentvolumeclaim)` to the PVC entity (the same join as PVC StorageClass resolution; the two label reads are per-field independent — a series may carry `volumename` without `storageclass` and vice versa). The key SHALL be set only when the resolved value is non-empty.
- `svm` — the NetApp SVM, resolved by the `netapp-storage-graph` capability's Harvest join rooted at the resolved PV name. That join derives a match token from the PV name and matches it against the **stock** `volume` label of the `volume_labels` series; the `svm` label rides on the same matched series that provides the serving aggregate and controller, and never on the QoS families that carry the edge's I/O. The key SHALL be set only when the join resolves a non-empty `svm` value. By construction `svm` SHALL never be present without `volumename`.

The `volumename` key is DISTINCT from the existing `volume` key (the pod-spec volume name from `kube_pod_spec_volumes_persistentvolumeclaims_info`); both MAY coexist on one PVC entity and neither replaces the other. Neither is the Harvest `volume` label, which names an ONTAP FlexVol and is never surfaced on a PVC entity.

Every link degrades gracefully: when `kube_persistentvolumeclaim_info` is absent, a join finds no match, or a required label is empty, the affected key(s) are simply omitted — the reader SHALL still build a valid topology, SHALL NOT fail the build, and SHALL NOT emit an empty-string label value. A PV name that the configured derivation cannot map onto any FlexVol name is one such no-match: the entity keeps `volumename` and loses only `svm`. The join ENRICHES PVC entities that exist via the pod→PVC binding metric; it SHALL NOT materialise a PVC on its own.

Resolution SHALL be deterministic: on duplicate series the reader SHALL pick the lexically-smallest non-empty value per stage (`volumename` per `(cluster, namespace, claim)`; `svm` per matched volume among the series on the picked aggregate's own ONTAP cluster, per the `netapp-storage-graph` capability), so the emitted labels are a pure function of the upstream data, independent of vector order. Labels are baked at build time before any projection.

#### Scenario: Full chain resolves volumename and svm

- **WHEN** the upstream provides a PVC entity `cluster-alpha/db/data-mongo-0` with `kube_persistentvolumeclaim_info{cluster="cluster-alpha", namespace="db", persistentvolumeclaim="data-mongo-0", volumename="pvc-9f3a"}` and a Harvest `volume_labels` series with `volume="trident_pvc_9f3a", svm="svm-prod"`
- **THEN** the emitted PVC entity's `labels` contains `volumename="pvc-9f3a"` and `svm="svm-prod"`

#### Scenario: PV without a TridentVolume row yields volumename only

- **WHEN** a PVC resolves `volumename="pvc-9f3a"` but no Harvest `volume_labels` series has a `volume` label its derived token matches
- **THEN** the emitted PVC entity's `labels` contains `volumename="pvc-9f3a"` and no `svm` key, and the build does not fail

#### Scenario: TridentVolume without a matching backend yields no svm

- **WHEN** the Harvest `volume_labels` series matched by a PVC's derived token carries an empty `svm` label
- **THEN** the emitted PVC entity carries `volumename` but no `svm` key — never an empty-string value — and the build does not fail

#### Scenario: Trident metrics absent entirely

- **WHEN** the upstream contains `kube_persistentvolumeclaim_info` (with `volumename` labels) and no Harvest `volume_labels` series for the window
- **THEN** the reader produces a valid topology in which PVC entities carry `volumename` but no `svm` key, and the build does not fail

#### Scenario: PVC info without volumename yields neither label

- **WHEN** a PVC's `kube_persistentvolumeclaim_info` series carries no (or an empty) `volumename` label, or no info series matches the PVC at all
- **THEN** the emitted PVC entity carries neither a `volumename` nor an `svm` key — no empty-string value is emitted — and the build does not fail

#### Scenario: volumename is independent of storageclass on the same series

- **WHEN** a `kube_persistentvolumeclaim_info` series carries `volumename="pvc-9f3a"` but an empty `storageclass` label
- **THEN** the emitted PVC entity carries `labels.volumename="pvc-9f3a"` while no `storageclass` attribute is emitted for it (and vice versa: a series with `storageclass` but no `volumename` drives the attribute without the label)

#### Scenario: volume and volumename coexist

- **WHEN** a PVC entity derives `volume="data"` from the pod→PVC binding metric and resolves `volumename="pvc-9f3a"` from `kube_persistentvolumeclaim_info`
- **THEN** the emitted PVC entity's `labels` contains both `volume="data"` (the pod-spec volume name) and `volumename="pvc-9f3a"` (the bound PV name) as distinct keys, and neither carries the ONTAP FlexVol name

#### Scenario: Deterministic pick on duplicate series at every stage

- **WHEN** the upstream reports two `kube_persistentvolumeclaim_info` series for `(cluster-alpha, db, data-mongo-0)` with `volumename="pvc-b"` and `volumename="pvc-a"`, and two Harvest `volume_labels` series matched by the `pvc-a` token with `svm="svm-b"` and `svm="svm-a"`
- **THEN** the reader resolves `volumename="pvc-a"` and `svm="svm-a"` (the lexically-smallest non-empty value at each stage) deterministically across rebuilds, independent of upstream vector order
