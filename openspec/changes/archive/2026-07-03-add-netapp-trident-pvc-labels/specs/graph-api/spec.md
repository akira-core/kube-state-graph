# graph-api Delta: add-netapp-trident-pvc-labels

## ADDED Requirements

### Requirement: PVC `volumename` and `svm` labels

A `type="pvc"` node's `data.labels` SHALL additively carry two further string entries whenever the `cluster-topology-source` capability resolves them ("PVC PersistentVolume name and NetApp SVM labels"):

- `volumename` — the name of the PersistentVolume bound to the claim (from the `volumename` label of `kube_persistentvolumeclaim_info`).
- `svm` — the NetApp ONTAP SVM serving the claim (from the `kube_persistentvolumeclaim_info` → `kube_tridentvolume_info` → `kube_tridentbackend_info` join chain).

Both are plain `labels` entries (strict `map[string]string`) — there SHALL be NO `data.volumename` or `data.svm` typed field on the PVC node. Each key SHALL be **absent** when its value is unresolved; an empty-string value SHALL never be emitted. `svm` SHALL never be present without `volumename`. The `volumename` key is distinct from the existing `volume` key (the pod-spec volume name); both MAY appear on the same node.

The additions are additive to the v1 wire contract and MUST NOT disturb the deterministic-body guarantee: for identical upstream data the label set is byte-identical across rebuilds, and responses built from upstreams without the Trident metrics are unchanged except for `volumename` appearing wherever `kube_persistentvolumeclaim_info` carries it.

#### Scenario: PVC node with a fully-resolved NetApp chain

- **WHEN** the response contains a PVC node whose PV name and SVM resolved (e.g. PV `pvc-9f3a` on SVM `svm-prod`)
- **THEN** its `data.labels.volumename` equals `"pvc-9f3a"`, its `data.labels.svm` equals `"svm-prod"`, and its `data` has no `volumename` or `svm` field outside `labels`

#### Scenario: PVC node with an unresolved chain omits the keys

- **WHEN** the response contains a PVC node whose claim reported no `volumename` (or whose Trident chain did not resolve)
- **THEN** its `data.labels` has no `volumename` key (or carries `volumename` but no `svm` key, respectively); neither key is ever present with an empty-string value

#### Scenario: volume and volumename are distinct keys

- **WHEN** the response contains a PVC node that derives a pod-spec volume name `data` and a bound PV name `pvc-9f3a`
- **THEN** `data.labels.volume` equals `"data"` and `data.labels.volumename` equals `"pvc-9f3a"` — the two keys coexist and carry different values
