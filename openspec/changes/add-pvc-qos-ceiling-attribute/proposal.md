# Proposal

## Why

A claim's declared QoS throughput ceiling (`max_iops` / `max_bytes_per_sec`) is
visible today ONLY on an edge — the `pvc-to-netapp-aggr` edge of `GET /v1/graph`
and the claim-level `svm-pvc` `storage-flow` edge of `GET /v1/storage-graph` —
and only when that edge also carries a measurement. A consumer inspecting a PVC
node has to find the right edge to learn the limit the claim runs under, and
two classes of claim never show a ceiling at all even though the upstream data
to resolve one is already fetched:

- **FlexGroup claims** (matched `volume_labels` resolved an `svm` but no
  `aggr`): the join `continue`s before hop B, so no QoS candidate is consulted
  and no ceiling is resolved — although `qosVolumeScope` already puts every
  matched FlexVol name, FlexGroup included, into the scoped QoS read.
- **Storage-graph claims whose `svm-pvc` edge carries no flow or latency**:
  `sumUnitShares` drops the claim's I/O object, and the ceiling with it.

The ceiling is a property of the claim's volume (which policy group governs it),
not of one hop, so it belongs on the PVC node as well.

## What Changes

- Add a typed, nullable PVC node attribute **`data.qos`** =
  `{policy_group, max_iops?, max_bytes_per_sec?}`, `omitempty`, never inside
  `labels` (same precedent as `usage` / `owner`). It is present iff at least
  one ceiling field resolved; `policy_group` names the policy group the ceiling
  was keyed on. Absence keeps its meaning: no declared ceiling, never `0`.
- **Copy, not move**: the edge fields `data.metrics.max_iops` /
  `max_bytes_per_sec` on `pvc-to-netapp-aggr` and on the `svm-pvc`
  `storage-flow` tier are unchanged. The node value and the edge value for one
  claim SHALL be equal whenever both are present.
- Resolve the ceiling for **FlexGroup claims** too: the hop-C key's ONTAP
  cluster comes from the SVM pick's own cluster (`pickSVM` already returns it)
  when no aggregate resolved, and hop B's policy pick runs over the claim's
  in-scope candidates. No new upstream query — the candidates are already read.
- The node attribute is decoupled from the edge's "no ceiling without a
  measurement" rule, but NOT from hop B: the policy group is still recovered
  only from a matched QoS workload series, so a claim with no workload series
  still has no `qos`. The "incomplete or unmatched key is ignored, never
  widened" rule is unchanged.
- `GET /v1/storage-graph` carries the same attribute on every retained PVC (the
  "Attributes and compound groups carry over" list gains `qos`).
- `graph.GraphNode` gains `QoS() *graph.QoSCeiling` (nil for every non-PVC node
  and for a PVC with no resolved ceiling); the Cytoscape DTO and the OpenAPI
  document gain the `qos` object.

Not in scope: adaptive QoS policies (`qos_policy_adaptive_*`) — a claim under
an adaptive policy still carries no ceiling.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `netapp-storage-graph`: "QoS fixed-policy throughput ceilings" — the resolved
  ceiling also surfaces on the PVC node, and FlexGroup claims resolve one
  (key's ONTAP cluster from the SVM pick when no aggregate resolved).
- `graph-api`: new "PVC `qos` attribute" requirement in the Cytoscape shape
  (field names, presence rule, equality with the edge fields).
- `storage-graph-api`: "Attributes and compound groups carry over" lists `qos`.

## Impact

- `pkg/build/netapp.go` — `resolveNetAppStorage` (FlexGroup branch before the
  `continue`; the ceiling recorded per claim, not only inside `io != nil`),
  `netappResult` gains a per-PVC ceiling map; `pkg/build/topology.go` stamps it
  on `PVCNode` before `graph.NewGraph`.
- `pkg/graph/node.go` — `PVCNode.QoSValue`, `QoSCeiling` type, `QoS()` on the
  sealed interface (every node type implements it).
- `pkg/cytoscape` — `qos` DTO, round6 on both figures, OpenAPI annotation;
  `make docs` regenerates `docs/`.
- Goldens carrying a NetApp-joined PVC with a ceiling gain `data.qos`
  (additive; `with-netapp-storage-cytoscape.json` and the storage-graph
  goldens).
- Additive to the v1 wire contract; no new query, metric, node or edge type.
