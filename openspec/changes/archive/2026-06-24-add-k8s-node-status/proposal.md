# Proposal: add-k8s-node-status

## Why

K8s node entries surface `id`, `name`, `labels`, and `ipaddress`, but nothing
about whether the node is actually healthy. Operators looking at the graph
cannot tell a `Ready` node from one whose kubelet has stopped reporting — the
single most useful node-health signal is missing. kube-state-metrics already
publishes it as `kube_node_status_condition{condition="Ready"}`; we just don't
read it.

## What Changes

- A new optional topology query reads `kube_node_status_condition{condition="Ready"}` (anchored `condition="Ready"` is a fixed, request-invariant metric-selection contract — same shape as the existing node-address `type=~"ExternalIP|InternalIP"` selector, **not** a caller filter).
- `type="node"` entries gain a typed, nullable `ready_status` attribute serialised as `data.ready_status` (a `string`, `omitempty`) carrying one of `"Ready"`, `"NotReady"`, `"Unknown"`. It lives on a typed accessor, **never inside `labels`** — same precedent as `owner` / `ipaddress`.
- Resolution: among a node's `condition="Ready"` series, the active one (value `== 1`) decides the attribute — `status="true"` → `"Ready"`, `status="false"` → `"NotReady"`, `status="unknown"` → `"Unknown"`.
- **Absence is distinct from `"Unknown"`.** The field is omitted entirely when the metric is absent, when a node has no `condition="Ready"` series, or when no status row is active — graph-as-no-data. `"Unknown"` is reserved for the genuine Kubernetes state where the API itself reports `Ready=Unknown` (kubelet lost contact). The two are never conflated.
- Determinism preserved: among `condition="Ready"` rows that are active for one `(cluster, node)`, the lexically-smallest `status` label wins (a defensive tie-break that never fires in practice, since exactly one Ready status is active), so the emitted value is a pure function of the data — not upstream vector order.
- Graceful degradation: the metric is OPTIONAL. When kube-state-metrics does not export it (older KSM, condition reader disabled), the build still succeeds and every node simply omits `ready_status`. No build failure, no API contract break.
- The `KSG_METRIC_PREFIX` prefix applies to the new `kube_node_status_condition` query (it is a KSM-shaped series).
- No new node/edge types, no new dependency, no config knob, no `labels` change. `data.ready_status` is purely additive to the response shape.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `cluster-topology-source`: the "Topology series consumed" requirement gains `kube_node_status_condition{condition="Ready"}` as an OPTIONAL consumed series with graceful degradation; the "Canonical entity fields" K8s-node rule gains the `ready_status` derivation; the `KSG_METRIC_PREFIX` requirement lists the new series among the prefixed `kube_*` set.
- `graph-api`: a new "Node `ready_status` attribute" requirement specifies the `data.ready_status` field (`type="node"` only, string enum, `omitempty`, outside `labels`, omitted-vs-`"Unknown"` distinction).

## Impact

- `pkg/promql/queries.go` — new `QNodeStatusCondition` constant + prefix-aware render case; `pkg/promql/queries_test.go` selector + prefix expectations.
- `pkg/graph/node.go` — new `ReadyStatusValue` field on `K8sNode`, new `ReadyStatus() string` method on the sealed `GraphNode` interface (K8sNode returns the field; the other four node types return `""`), and exported value constants.
- `pkg/build/topology.go` — new `topologyVectors` field, an added `g.Go(fetch(...))` in the `ReadTopology` errgroup, a `RawSeriesCount` entry, a `resolveNodeReadyStatus` resolver keyed `(cluster, node)`, and assignment at K8s-node assembly.
- `pkg/cytoscape/cytoscape.go` — new `ReadyStatus` field on `NodeData` (`json:"ready_status,omitempty"`) pulled via the `n.ReadyStatus()` accessor.
- Tests: `pkg/graph/node_test.go`, `pkg/cytoscape/*`, `pkg/build/topology_test.go`, `internal/api` component/golden fixtures as needed, `internal/integration` fixture series.
- Docs: CLAUDE.md (typed-attribute and `KSG_METRIC_PREFIX` load-bearing paragraphs), `pkg/graph/node.go` doc comments, regenerated `docs/` via `make docs`, README topology-metrics table; promoted specs above (via this change's delta specs).
- No new dependencies, no self-metric change (`query_name` constants stay bare).
