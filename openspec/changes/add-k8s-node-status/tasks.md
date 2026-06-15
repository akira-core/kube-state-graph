# Tasks: add-k8s-node-status

## 1. Query layer

- [x] 1.1 Add `QNodeStatusCondition Query = "kube_node_status_condition"` (bare metric name) in `pkg/promql/queries.go`
- [x] 1.2 Add a prefix-aware render case: `last_over_time(%skube_node_status_condition{condition="Ready"}[%s])` (prefix + window), alongside the other KSM cases
- [x] 1.3 Extend `pkg/promql/queries_test.go`: assert the bare and `KSG_METRIC_PREFIX`-prefixed render, and that the `condition="Ready"` selector is present; confirm the constant stays bare for `query_name` stability

## 2. Graph types

- [x] 2.1 Add `ReadyStatusValue string` to `K8sNode` in `pkg/graph/node.go`
- [x] 2.2 Add `ReadyStatus() string` to the sealed `GraphNode` interface; `K8sNode` returns the field, `PodNode` / `PVCNode` / `ServiceNode` / `ExternalNode` return `""`
- [x] 2.3 Export value constants `ReadyStatusReady = "Ready"`, `ReadyStatusNotReady = "NotReady"`, `ReadyStatusUnknown = "Unknown"` in `pkg/graph`
- [x] 2.4 Extend `pkg/graph/node_test.go`: only `K8sNode` carries a `ready_status`, the other four node types return `""`

## 3. Parse layer

- [x] 3.1 Add `NodeStatus model.Vector` to `topologyVectors` and a `g.Go(fetch(promql.QNodeStatusCondition, &v.NodeStatus))` call in `ReadTopology` (`pkg/build/topology.go`); add the `RawSeriesCount` entry
- [x] 3.2 Add `resolveNodeReadyStatus(vec model.Vector, mc missingClusterCounts) map[[2]string]string` keyed `(cluster, node)`: pick the active (`value == 1`) `condition="Ready"` row, map `true`→`Ready` / `false`→`NotReady` / `unknown`→`Unknown`; lexically-smallest `status` on the defensive multi-active tie; use `mc.bucket(promql.QNodeStatusCondition, ...)` for the missing-cluster tally
- [x] 3.3 Assign `ReadyStatusValue` at K8s-node assembly from the resolver map by the existing `[2]string{cluster, node}` key (alongside `nodeIPs` / `nodeLabels`)
- [x] 3.4 Unit tests in `pkg/build` (`topology_test.go`): Ready/NotReady/Unknown derivation, omit when metric absent / no Ready series / no active row, `Unknown` vs omitted distinction, order-free determinism, missing-cluster status row does not join a real-cluster node

## 4. Serialiser

- [x] 4.1 Add `ReadyStatus string \`json:"ready_status,omitempty"\`` to `cytoscape.NodeData` and populate it from `n.ReadyStatus()` in the node loop (`pkg/cytoscape/cytoscape.go`) — no type switch
- [x] 4.2 Serialiser test in `pkg/cytoscape`: `type="node"` emits `data.ready_status` when set, omits it when `""`, never emits a status key in `labels`, non-node types never carry it

## 5. API surface verification

- [x] 5.1 Component test in `internal/api`: a node carries `data.ready_status` end-to-end; `labels` carries no status key; refresh goldens with `-update` only where a fixture deliberately adds `kube_node_status_condition`
- [x] 5.2 Integration fixture in `internal/integration`: ingest `kube_node_status_condition{condition="Ready", status=...}` rows (a Ready node, a NotReady/Unknown node, a node with no Ready series), assert emitted `ready_status`; use a unique node name (or per-test discriminator) to avoid the shared-VM series leak

## 6. Docs / spec sync

- [x] 6.1 Update CLAUDE.md: the typed-attribute load-bearing paragraph (K8sNode `ready_status`, source, omit-vs-`Unknown`) and the `KSG_METRIC_PREFIX` series list (add `kube_node_status_condition`); update the `pkg/graph/node.go` `GraphNode` / `K8sNode` doc comments
- [x] 6.2 Regenerate docs (`make docs`) and update the README topology-metrics table (and `README.zh-tw.md`)
- [x] 6.3 Run `make build vet lint test`; `openspec validate --strict "add-k8s-node-status"` (this CLI has no `verify` subcommand) — all green (build, vet, `lint` 0 issues, full `-race -shuffle` suite incl. Docker integration, `check-docs` regen idempotent, strict validate). Reconcile the `Topology series consumed` / `Configurable upstream metric-name prefix` overlap with `node-internal-ip-fallback` at archive time (see design.md Risks) — deferred to archive, not an implementation blocker.
