## Why

`GET /v1/storage-graph` cannot tell a client which aggregate a claim sits on. The chain is `netapp-aggr → netapp-svm → pvc`, and an SVM spans aggregates: when one SVM holds claims on `aggr1` and on `aggr2`, the body carries `aggr1 → svm`, `aggr2 → svm` and one `svm → pvc` edge per claim, so the per-claim aggregate is summed away at the SVM. The builder already knows it — the assembler stamps `claim_aggr` on every `svm-pvc` edge so `ProjectStorage` can answer `?aggr=aggr1&pod=x` — and then strips it before serialising.

The frontend's Sankey needs it to link a PVC to its aggregate: a view that draws aggregate → PVC directly, and a hover that lights a claim's own aggregate instead of every aggregate feeding its SVM. The demo estate shows the gap today: `svm_demo` spans `aggr1` and `aggr2`, and none of its three PVCs says which one it is on.

## What Changes

- PVC nodes gain a plain `labels.aggr` holding the **node id** of the aggregate the claim's matched volume sits on (`netapp/<ontap_cluster>/aggr/<aggr>`), beside the existing `labels.svm`. It comes from the same pick that draws the claim's `pvc-to-netapp-aggr` edge, so the label, that edge and the storage chain cannot disagree.
- The value is an id, not a name: aggregate names are unique only within an ONTAP cluster, and a PVC's `labels.aggr` must name exactly one `netapp-aggr` node in the body. Pod `labels.node`, which carries a Kubernetes node's id, is the precedent for a label that references another node.
- It is set only when the claim resolved an aggregate. A FlexGroup claim (it spans aggregates and has no `pvc-to-netapp-aggr` edge), an unmatched claim and a deployment without Harvest omit the key, under the existing absent-not-empty rule for `volumename` / `svm`.
- Both endpoints carry it, because it is stamped where `svm` is, in the shared topology. On `/v1/graph` it restates the `pvc-to-netapp-aggr` edge; on `/v1/storage-graph` it is the only place the per-claim aggregate reaches the wire.
- Additive: no field, edge type, tier or id changes, so no `docs/BREAKING.md` entry. Golden bodies carrying NetApp-backed PVCs gain the key.
- `claim_aggr` stays an internal key of the storage projection. Whether `ProjectStorage` can read the PVC label instead is a design question, not a contract one.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `graph-api`: a new requirement "PVC `aggr` label" beside "PVC `volumename` and `svm` labels", whose behaviour is unchanged — the label's source, its id form, and when it is absent.
- `netapp-storage-graph`: a new requirement "PVC aggr label from the Harvest join", beside "PVC svm label re-sourced from the Harvest join", pinning that the label and the `pvc-to-netapp-aggr` edge come from the same pick; "Harvest volume-label series as the storage topology source" lists the label among what that series alone produces.
- `storage-graph-api`: a new requirement "Each claim names its aggregate": the body's PVC nodes carry `labels.aggr`, the named aggregate is always in the same body, and a scenario pins an SVM spanning two aggregates.

## Impact

- Code: `pkg/build/topology.go` (the stamp beside `svm`) and the NetApp resolution that already yields each claim's aggregate (`pkg/build/netapp.go`); `pkg/graph/project_storage.go` only if `claim_aggr` is re-derived from the label.
- Tests: unit tests on the stamp and on each absence case (FlexGroup, unmatched claim, no Harvest); a golden refresh of every body carrying NetApp-backed PVCs, including `internal/api/testdata/golden/with-netapp-storage-cytoscape.json`; an integration assertion on the storage-graph body.
- Docs: `CLAUDE.md`, whose PVC-label rule names only `volumename` and `svm`. `docs/swagger.yaml` does not enumerate PVC labels.
- Consumers: additive for every client. `kube-state-graph-frontend`'s change `sankey-svm-grouping` depends on it; the demo repository can assert the label in `verify.sh` once its submodule pointer moves.
