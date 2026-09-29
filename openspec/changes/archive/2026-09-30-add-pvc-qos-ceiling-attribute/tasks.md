# Tasks

## 1. Graph model

- [x] 1.1 Add `graph.QoSCeiling{PolicyGroup, MaxIOPS, MaxBytesPerSec}`, `PVCNode.QoSValue`, and `QoS() *QoSCeiling` on the sealed `GraphNode` interface (nil for every non-PVC node type); verify `go build ./...` and `go vet ./pkg/graph/...` pass and a unit test asserts `QoS()` is nil for each of the other seven node types

## 2. Builder: one ceiling resolution per claim

- [x] 2.1 Extract `resolveCeiling(qcands, policyIndex, oc, svm) *graph.QoSCeiling` from `pickPolicy` + `applyCeiling` (nil on an incomplete/unmatched key and when neither field resolved; floats copied, never aliased); verify the existing ceiling tests in `pkg/build/netapp_test.go` still pass unchanged
- [x] 2.2 In `resolveNetAppStorage`, resolve the ceiling for every claim before the aggregate gate using `keyOC = oc` or, when no aggregate resolved, the SVM pick's own ONTAP cluster; record it in a new `netappResult.qosByPVC`, and keep the edge's ceiling attached only inside the `io != nil` branch by copying from the resolved value; verify with new tests: FlexGroup claim gets a ceiling and no edge, LUN-only claim gets a node ceiling and an edge with no `IO`, measured claim's node and edge ceilings are equal, `netapp_qos_join_miss` count unchanged for a FlexGroup claim
- [x] 2.3 Stamp `qosByPVC` onto `PVCNode.QoSValue` in `pkg/build/topology.go` beside the `svm` / `aggr` labels, before `graph.NewGraph`; verify a topology test shows the attribute on the built PVC node and `make test` passes with `-race`
- [x] 2.4 Add a determinism test: shuffled QoS / fixed-policy vector order yields an identical `QoSValue` for every claim; verify it passes under `-shuffle=on -count=5`

## 3. Serialisation and API docs

- [x] 3.1 Add `QoSDTO` (`policy_group`, `max_iops,omitempty`, `max_bytes_per_sec,omitempty`) to `pkg/cytoscape`, placed after `storageclass` in `NodeData`, both numbers through `round6`; verify a `pkg/cytoscape` unit test pins the JSON for a full, a partial and an absent ceiling, and that a non-PVC node never emits `qos`
- [x] 3.2 Add the swag annotation for the `qos` object and run `make docs`; verify `make check-docs` is clean after committing `docs/`
- [x] 3.3 Regenerate goldens with `go test ./internal/api/ -update -run Golden` and review the diff: only PVC nodes gain `data.qos` (`with-netapp-storage-cytoscape.json` and the four `storage-graph-*-root-cytoscape.json` files), with values equal to their edge's `max_iops` / `max_bytes_per_sec`; verify `go test ./internal/api/ -run Golden` passes without `-update`
- [x] 3.4 Add a golden (or extend an existing fixture) with a FlexGroup claim whose ceiling resolved, showing `data.qos` on a PVC with no `pvc-to-netapp-aggr` edge; verify the golden test passes

## 4. Documentation

- [x] 4.1 Update CLAUDE.md's NetApp bullet: the "ceiling can NEVER appear without a measurement" invariant is scoped to the edge, the PVC node carries `data.qos` whenever the ceiling resolved, FlexGroup included; also list `qos` in the "Sealed graph types" method list; verify by re-reading the bullet against the specs
- [x] 4.2 Note the additive `data.qos` attribute in `docs/netapp-harvest-preconditions.md` (ceiling coverage now includes FlexGroup claims) and in `docs/upstream-metrics.md` if it lists attribute sources; verify links and field names match the DTO

## 5. Integration

- [x] 5.1 Extend `internal/integration` `TestPVCNetAppHarvestJoin` fixtures with fixed-policy series and assert `data.qos` on the PVC node equals the edge's ceiling, plus a FlexGroup claim carrying `data.qos` with no edge; verify `go test ./internal/integration/ -run TestPVCNetAppHarvestJoin` passes with Docker available
- [x] 5.2 Run `make lint`, `make test`, `make verify-mocks`, `make check-docs` and `openspec validate add-pvc-qos-ceiling-attribute --strict`; verify all are clean
