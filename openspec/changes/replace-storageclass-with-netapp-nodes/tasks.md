# Tasks — replace-storageclass-with-netapp-nodes

## 1. Upstream contract verification

- [x] 1.1 Verify the ten Harvest series names (`volume_read_ops`, `volume_write_ops`, `volume_read_latency`, `volume_write_latency`, `volume_read_data`, `volume_write_data`, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`) and the two kubelet series names against the production VictoriaMetrics (`/api/v1/label/__name__/values` + a sample `/api/v1/series` per name to confirm the `cluster`/`node`/`aggr`/`svm`/`volume_name` label contract, including that `volume_name` is populated by the relabel rule). On any drift: mechanically rename in `specs/` and `design.md` before starting task 2.

## 2. pkg/promql — queries and renderer

- [x] 2.1 Remove `QStorageClassInfo`, `QTridentVolumeInfo`, `QTridentBackendInfo` constants and their `Render` cases; delete their `queries_test.go` cases.
- [x] 2.2 Add twelve constants + `last_over_time` render cases (no `rate()`, no `sum by`): `QVolumeReadOps`, `QVolumeWriteOps`, `QVolumeReadLatency`, `QVolumeWriteLatency`, `QVolumeReadData`, `QVolumeWriteData`, `QAggrStatus`, `QAggrSpaceUsed`, `QAggrSpaceTotal`, `QNetAppNodeStatus`, `QKubeletVolumeUsedBytes`, `QKubeletVolumeCapacityBytes`; unit-test each rendered string.
- [x] 2.3 Delete `Renderer.Prefix` and the `Renderer` struct threading — `promql.Render(q, window)` becomes the single entry point; delete prefix validation/rendering tests; update every caller signature (`build`, `internal/api`).

## 3. pkg/graph — node types, edge value, registry

- [x] 3.1 Delete `StorageClassNode`, `NodeTypeStorageClass`, `StorageClassInfo`; add `NetAppAggrNode` (labels `{ontap_cluster, node}`, id `netapp/<oc>/aggr/<aggr>`) and `NetAppNode` (labels `{ontap_cluster}`, id `netapp/<oc>/<node>`) as sealed types.
- [x] 3.2 Extend `GraphNode` with `Health() string` and `Usage() *UsageBytes` (all types implement; zero values everywhere except NetApp types and `PVCNode.Usage()`); add `graph.UsageBytes{UsedBytes, CapacityBytes *float64}`; unit tests for accessor zero-values per type.
- [x] 3.3 Add `graph.IOMetrics{ReadOps, WriteOps, ReadLatencyUs, WriteLatencyUs, ReadBytesPerSec, WriteBytesPerSec *float64}` and `Edge.IO *IOMetrics` (with a `WithIO` copy helper mirroring `WithMetrics`); RED `EdgeMetrics` untouched.
- [x] 3.4 Registry: remove `pvc-to-storageclass`, add `pvc-to-netapp-aggr` (`[pvc]→[netapp-aggr]`, directed, `may_cross_cluster:false`, `labels:[]`); update `registry_test.go`.

## 4. pkg/build — readers and the storage resolver

- [x] 4.1 Remove the three dead fan-out legs and delete `resolveStorageClassInfo`, `resolveTridentVolumeBackends`, `resolveTridentBackendSVMs` (+ `trident_test.go`, `storageclass_test.go`); update `Topology` struct fields.
- [x] 4.2 Add the twelve new legs to `ReadTopology` with **log-and-continue** error semantics (empty vector on leg failure, existing KSM legs keep abort semantics); pin with a unit test that a failing Harvest leg does not fail the build.
- [x] 4.3 Implement `pkg/build/netapp.go` `resolveNetAppStorage` per design D3: volume_name index, aggregate pick (lexically-smallest `(oc, aggr)`, empty-`aggr` excluded), owner pick (lexically-smallest non-empty `node`; status series never votes), svm pick (includes empty-`aggr` matches), per-family I/O sum in ascending value order, per-aggregate health/usage, per-controller health, demand-driven node materialisation, sorted output slices.
- [x] 4.4 Implement the kubelet PVC usage resolver (join `(cluster, ns, claim)`, per-field, smallest-value on duplicates) and bake `PVCNode` usage + `svm` label + `data.storageclass` retention at assembly; wire aggr/node entities and `pvc-to-netapp-aggr` edges (UUIDv5, dedupe) into `graph.NewGraph` input.
- [x] 4.5 Coverage signal: count non-empty-`volumename` claims with no edge (no match or only empty-`aggr` matches) iff ≥1 volume series read; emit one aggregated `slog.Warn("netapp_volume_join_miss", "count", n)`; unit tests for all three trigger shapes (miss counted, Harvest absent silent, full coverage silent).
- [x] 4.6 `pkg/build/netapp_test.go`: join hit/miss, FlexGroup empty-`aggr`, duplicate-series determinism at every pick stage, takeover-in-window, health mapping incl. absence≠degraded, usage per-field; update `topology_test.go` for removed resolvers.

## 5. pkg/graph — projection

- [x] 5.1 Extend `infraNodePassesFilters`/`Project` with the transitive NetApp admission: wave 1 `netapp-aggr` iff referenced by an admitted PVC via `pvc-to-netapp-aggr`; wave 2 `netapp-node` iff named by an admitted aggregate's `labels.node`; cluster filter passes both types through (no `cluster` label).
- [x] 5.2 `?name=` escape hatch matches both NetApp types, and an admitted aggregate always pulls its owning controller (dangling-parent guarantee); update `project_storageclass_test.go` → `project_netapp_test.go` covering default-prune drop, namespace retention, shared-filer-two-clusters, name-surfacing of aggr and node.

## 6. pkg/cytoscape — serialiser

- [x] 6.1 Metrics union DTO: `Rate` → `*float64 omitempty`, add six `omitempty` I/O fields, `metricsDTO(m, io)` fills exactly one family (RED precedence on the impossible both-set case), `round6` on every field; unit tests RED-only / IO-only / neither.
- [x] 6.2 Emit `data.health` (NetApp types), `data.usage` (PVC + aggr via the shared accessor), `data.storageclass` (PVC); remove the storageclass node handling; unit tests for omitempty on each.
- [x] 6.3 `storage-cluster` group synthesis (from emitted NetApp nodes' `ontap_cluster`), tier order cluster → storage-cluster → namespace → application → controller, `compoundParent`: `netapp-node` → `storage-cluster/<oc>`, `netapp-aggr` → real node id `netapp/<oc>/<labels.node>`; pin `clusters[]` exclusion of ONTAP names; update `compound_test.go` + delete cytoscape `storageclass_test.go`.

## 7. Config, facade, API surface

- [x] 7.1 Remove `MetricPrefix` from `internal/config` (field, validation, flag `--metric-prefix`, env `KSG_METRIC_PREFIX`), `build.Options`, `kubegraph.Options`, and `api.Server` wiring; update config tests.
- [x] 7.2 Update swag annotations: node-type enums (`netapp-aggr`/`netapp-node`, storageclass gone), `EdgeMetricsDTO` union (rate optional + I/O fields), `data.usage`/`data.health`/`data.storageclass`; run `make docs` and commit `docs/`; `make check-docs` clean.

## 8. Goldens, property, integration

- [x] 8.1 Replace golden `with-netapp-trident-cytoscape.json` with `with-netapp-storage-cytoscape.json` (aggr + node + edge + I/O metrics + health + usage + svm + storage-cluster nesting); regenerate every golden touched by storageclass removal and the metrics DTO change (`go test ./internal/api -update -run Golden`).
- [x] 8.2 Extend `pkg/graph/property_test.go` generators with the two NetApp types; add invariant "emitted `netapp-aggr` ⇒ its owning `netapp-node` emitted".
- [x] 8.3 Integration: fixture Harvest volume/aggr/node + kubelet series into the VictoriaMetrics container; replace `TestPVCNetAppTridentLabels` with the Harvest-join equivalent (svm + edge + health + usage assertions); add a no-Harvest run asserting graceful degradation and no `netapp_volume_join_miss` warning.

## 9. Docs and final verification

- [x] 9.1 Update `CLAUDE.md` load-bearing bullets: replace the StorageClass/Trident bullets with the NetApp storage-graph rules, drop the `KSG_METRIC_PREFIX` bullet and prefix mentions in the series list, update the query fan-out count and edge-type list.
- [x] 9.2 Write the BREAKING release note (storageclass type + edge removed; metric prefix removed — prefixed deployments must republish at bare names; `rate` schema-optional) and the deployment-precondition doc (Harvest `volume_name` relabel rule with the three blind spots; Trident CRS config now removable); reconcile `docs/` KSM setup docs (PR #11 content) with the Trident removal.
- [x] 9.3 Full gate: `make build test vet lint vuln check-docs` clean (race + shuffle), `openspec validate --strict` then `openspec verify "replace-storageclass-with-netapp-nodes"`.
- [ ] 9.4 Coordinate `graph-api-gateway`: after tagging, open its PR bumping the dependency and deleting `Options.MetricPrefix` usage (design D10).

## 10. Volume throughput metrics (`volume_read_data` / `volume_write_data`)

> Delta on top of the shipped sections 1-8, which landed with four volume I/O
> families. Those task lines now read as the final contract (six families); the
> outstanding work is listed here.

- [x] 10.1 Verify `volume_read_data` / `volume_write_data` against the production VictoriaMetrics (`/api/v1/label/__name__/values` + a sample `/api/v1/series` per name) — same `cluster`/`node`/`aggr`/`svm`/`volume_name` label contract as the four shipped volume families, and values already resolved by Harvest to bytes per second. On drift: mechanically rename in `specs/` + `design.md` before task 10.2.
- [x] 10.2 `pkg/promql`: add `QVolumeReadData` / `QVolumeWriteData` constants + `last_over_time` render cases (no `rate()`, no `sum by`); unit-test both rendered strings.
- [x] 10.3 `pkg/graph`: extend `IOMetrics` with `ReadBytesPerSec` / `WriteBytesPerSec *float64`; extend the `WithIO` copy test.
- [x] 10.4 `pkg/build`: add the two OPTIONAL legs to the `ReadTopology` fan-out with the same log-and-continue semantics (27 legs total); index both vectors in `resolveNetAppStorage` and sum each family in ascending value order; both families vote in the aggregate/owner/svm picks exactly like the shipped four (they carry the same labels), and either family alone counts as "a volume series was read" for the D8 coverage signal.
- [x] 10.5 `pkg/cytoscape`: add `read_bytes_per_sec` / `write_bytes_per_sec` `omitempty` DTO fields with `round6`; extend the IO-only and RED-precedence DTO tests.
- [x] 10.6 `pkg/build/netapp_test.go`: per-family presence/absence, multi-series ascending sum, and a claim matching only the data families.
- [x] 10.7 Update swag annotations for the two new `EdgeMetricsDTO` fields, run `make docs`, regenerate goldens (`go test ./internal/api -update -run Golden`) so `with-netapp-storage-cytoscape.json` carries both fields, and extend the `internal/integration` Harvest fixture with both series.
- [x] 10.8 `CLAUDE.md`: Harvest leg count 8 → 10, topology fan-out 25 → 27, and the NetApp bullet's I/O field list.
- [ ] 10.9 Full gate: `make build test vet lint vuln check-docs` clean, `openspec validate --strict`, `openspec verify "replace-storageclass-with-netapp-nodes"`.
