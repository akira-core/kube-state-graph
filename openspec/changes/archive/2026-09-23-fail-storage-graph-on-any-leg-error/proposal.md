## Why

`GET /v1/storage-graph` inherits `/v1/graph`'s error classes: required
kube-state-metrics legs fail the build, but every Harvest family, both kubelet
volume-stats families and the two accumulating annotation families degrade — the
query error is logged, counted on `kube_state_graph_upstream_query_failures_total`,
and the build continues with an empty vector. The client receives a 200 whose
body cannot be told apart from a smaller estate.

That trade-off is right for `/v1/graph`, where NetApp and kubelet data is optional
decoration on a traffic graph. It is wrong for the storage graph, whose whole
subject IS the Harvest chain: a failed `volume_labels` read draws the rooted
aggregates and SVMs with no path through them, a failed QoS chunk draws paths with
no I/O, and operators read either as "the filer carries no traffic" instead of
"the upstream failed". The frontend needs to be told.

## What Changes

- **BREAKING (storage-graph only):** a `/v1/storage-graph` build fails on a query
  error of ANY family, chunk, phase, stage or backend — `volume_labels`, the QoS
  workload and policy families, the aggregate and controller families, the
  kubelet volume-stats families, `kube_replicaset_annotations`,
  `kube_job_annotations` and the application recovery included — except
  `ALERTS`, which stays optional.
- The failure maps to the existing 502 `reason: "upstream"`; the `message` now
  names the failed family (`upstream query failed: <family>`) and never an
  upstream URL, host or backend address.
- Not failures: empty vectors (absent or un-allowlisted families), a requested
  zone no backend declares, a restriction that falls back to an unrestricted read.
- `/v1/graph` keeps every existing error class.
- Three storage-graph-api requirements whose scenarios pinned degrading behaviour
  are replaced under new names (OpenSpec cannot drop a scenario through MODIFIED):
  "Storage build reads only what it draws" → "Storage build reads only what the
  body draws"; "Application roots recover their pods upstream" → "Application roots
  recover their pods before the pod read"; "Storage-side roots narrow the Harvest
  topology read" → "Storage-side roots restrict the Harvest topology read". Every
  requirement that cites them by name is updated.

**Sequencing.** This change goes BEFORE `read-storage-roots-through-volume-hub`,
which modifies several of the same requirements. After this change is archived,
that change's deltas are rebased onto the renamed requirements (its task 0.1).

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `storage-graph-api`: new requirement **Storage build fails closed on upstream
  query errors**; the three requirements above replaced under new names without
  their degrade scenarios; **Roots are always materialised when the upstream knows
  them**, **Attributes and compound groups carry over** and **Application roots
  compose with the Harvest restriction** updated to cite the new names.
- `netapp-storage-graph`: **Harvest legs under request-scoped selectors** and
  **Harvest volume-label series as the storage topology source** cite the renamed
  requirement.
- `cluster-topology-source`: **Topology series consumed** states that a storage
  build fails on any query error except `ALERTS` (absence still never fails);
  **Application-rooted recovery reads of the owner and annotation families** cites
  the renamed requirement and the fail-closed rule.

## Impact

- `pkg/build/topologyplan.go` — plan flag `failClosed` (true on `storagePlan`).
- `pkg/build/topology.go`, `scopedread.go`, `qosscope.go`, `volumelabelscope.go`,
  `appscope.go` — optional / degrading legs become required under the flag,
  `ALERTS` excepted.
- `pkg/build/errors.go` — `build.Error` carries the failed query name.
- `internal/api/errors.go` — `upstream` message names the family.
- Tests: component tests for the 502 mapping per family class, the `ALERTS`
  exception, empty-vector and no-backend-zone non-failures, `/v1/graph`
  unchanged; storage plan tests updated where they pinned degradation.
- Docs: `docs/BREAKING.md`, `docs/upstream-metrics.md`, `docs/netapp-harvest-preconditions.md`,
  `README.md`, `README.zh-tw.md`, `CLAUDE.md`, OpenAPI error description for
  `/v1/storage-graph`.
