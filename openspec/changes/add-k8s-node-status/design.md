# Design: add-k8s-node-status

## Context

K8s node entities are assembled in `parseTopology` (`pkg/build/topology.go`) from
`kube_node_info` (one entity per `(cluster, node)`), joined against `nodeIPs`
(`kube_node_status_addresses`) and `nodeLabels` (`kube_node_labels`), both keyed
`[2]string{cluster, node}`. `graph.K8sNode` carries `IDValue`, `NameValue`,
`LabelsValue`, `IPAddressValue` — no health signal. kube-state-metrics already
publishes node health as `kube_node_status_condition{condition, status}` (one
series per `(condition, status)`, value `1` for the active combination, `0`
otherwise); for `condition="Ready"` exactly one of `status="true"|"false"|"unknown"`
is active. The graph never reads it.

The repo has an established precedent for adding a typed, nullable node attribute
sourced from an optional KSM series: `owner` / `application` / `containers` on
pods (D34 + add-pod-application-containers) and `ipaddress` on K8s nodes. This
change follows that precedent for a K8s-node-only `ready_status`.

## Goals / Non-Goals

**Goals:**
- `type="node"` entries carry a typed `ready_status` ∈ {`"Ready"`, `"NotReady"`, `"Unknown"`}, omitted when no data.
- Source is OPTIONAL — absence degrades gracefully, never fails the build.
- Deterministic output: emitted value is a pure function of the sample set, not vector order.
- Existing deployments (no `kube_node_status_condition`) see byte-identical responses.

**Non-Goals:**
- No other node conditions (`MemoryPressure`, `DiskPressure`, `PIDPressure`, `NetworkUnavailable`) — only `Ready`. They can be added later as separate attributes if needed.
- No `labels.ready_status` / any status key in `labels` (typed attribute only).
- No new config knob — resolution is hardcoded, like every other resolver.
- No new node/edge type; no change to pod/service/PVC/external nodes.
- No numeric/boolean encoding — a string enum, consistent with `application`.

## Decisions

### D1. Parse-time status pick, not a PromQL `== 1` filter.

`QNodeStatusCondition` renders `last_over_time(<prefix>kube_node_status_condition{condition="Ready"}[w])`, returning all (up to three) status rows per node. `parseTopology` picks the row whose sample value is `1` and reads its `status` label. **Why:** every existing topology query is a bare `last_over_time(metric[w])` / `tlast_over_time(...)` with no comparison operator; the value-`1` pick at parse time mirrors the established address / owner / container picks. **Alternative — `kube_node_status_condition{condition="Ready"} == 1`** (returns only the active row) — rejected: it puts a value comparison into the render template (a first for this codebase) for a saving of two tiny rows per node; parse-time pick keeps the renderer uniform.

### D2. Absence is distinct from `"Unknown"` (central semantic decision).

`data.ready_status` is **omitted entirely** when the metric is absent, the node has no `condition="Ready"` series, or no status row is active. The literal `"Unknown"` is emitted **only** when the active Ready row has `status="unknown"` — the real Kubernetes state where the kubelet has stopped posting. **Why:** these are genuinely different facts — "kube-state-graph has no health data for this node" vs "Kubernetes reports the node's readiness as Unknown". Mapping both to `"Unknown"` would destroy information and break the omit-when-absent precedent that `owner` / `application` / `containers` all follow. **Alternative — default missing to `"Unknown"`** — rejected for exactly that conflation. Consumers distinguish the two as field-absent vs `ready_status:"Unknown"`.

### D3. K8s-node-only typed attribute on the sealed interface.

Add `ReadyStatusValue string` to `graph.K8sNode` only, and `ReadyStatus() string` to the sealed `graph.GraphNode` interface. `K8sNode.ReadyStatus()` returns the field; `PodNode`, `PVCNode`, `ServiceNode`, `ExternalNode` return `""` (the sealed interface forces all five to implement it — a compile-time guarantee). Export three value constants in `pkg/graph` (`ReadyStatusReady = "Ready"`, `ReadyStatusNotReady = "NotReady"`, `ReadyStatusUnknown = "Unknown"`) so build code and tests reference constants, not string literals. **Why a plain `string`, not a named type or struct:** mirrors `Application()` / `StorageClass()` (plain string accessors); the value carries no extra fields (a `bool` cannot represent the tri-state; a struct is over-built). Serialisation stays a plain JSON string.

### D4. `condition="Ready"` is a request-invariant metric-selection contract.

The `{condition="Ready"}` selector is pushed to the query layer for every build. This is consistent with the node-address `{type=~"ExternalIP|InternalIP"}` selector and the D30 service-graph sentinel selector — both fixed, request-invariant metric-selection contracts, NOT caller filters — so it does not break the "no caller filters pushed to PromQL" rule. It also drops the four other node conditions we never surface, keeping upstream series minimal. `QNodeStatusCondition` stays the bare metric name `kube_node_status_condition` so self-metric `query_name` / span dimensions are stable across prefixed deployments.

### D5. Determinism on the (impossible-in-practice) multi-active tie.

If more than one `condition="Ready"` row is active (value `1`) for a single `(cluster, node)` — which correct KSM never emits — the lexically-smallest `status` label wins. Pure function of the sample set, order-free. Documented as defensive; never fires against valid upstream data.

### D6. Resolver keyed `(cluster, node)`, own missing-cluster tally.

A new `resolveNodeReadyStatus(vec model.Vector, mc missingClusterCounts) map[[2]string]string` folds the status vector into a `[2]string{cluster, node}` → status-string map, then node assembly reads it by the same key it already uses for `nodeIPs` / `nodeLabels`. It uses `mc.bucket(promql.QNodeStatusCondition, cluster)` for its own missing-cluster tally (unlike `resolvePodApplications`, which reuses `kube_pod_owner`'s tally because it shares that vector — here the vector is exclusive to this resolver). A status series whose `cluster` label is empty buckets to `"unknown"` and will not join the real-cluster node, so its status is silently omitted — identical to the existing node-join behaviour for addresses/labels, and recorded in diagnostics by the tally.

### D7. Serialiser pulls via accessor, no type switch.

Add `ReadyStatus string \`json:"ready_status,omitempty"\`` to `cytoscape.NodeData`, populated from `n.ReadyStatus()` in the node loop — exactly like `Owner` / `Application` / `Containers` / `IPAddress`. `omitempty` on a `string` omits the empty string, giving the D2 omit-when-absent behaviour for free. No serialiser type switch is introduced (the sealed-interface accessor is the contract).

## Risks / Trade-offs

- **[Concurrent-change overlap with `node-internal-ip-fallback`]** — both active changes `## MODIFIED` the same two requirements (`Topology series consumed`, `Configurable upstream metric-name prefix`). This delta is written against the **current** promoted spec (which still has `kube_node_status_addresses{type="ExternalIP"}`); `node-internal-ip-fallback` widens that selector to `type=~"ExternalIP|InternalIP"`. OpenSpec does not auto-merge — on archive, whichever change lands **last** fully replaces the requirement text. **Mitigation:** archive coordination — whoever archives the second of the two must re-incorporate the first's edits into the MODIFIED requirement (or rebase this delta onto the post-`node-internal-ip-fallback` spec). `openspec verify` will surface the overlap. The `ADDED` requirements (`K8s node Ready-status attribute`, `Node ready_status attribute`) carry no overlap and are safe regardless of order.
- **[Selector pushes `condition="Ready"`]** — acceptable: request-invariant, same class as the node-address and D30 selectors (D4).
- **[Golden / integration fixtures]** — node fixtures that gain a `ready_status` shift golden JSON; refresh with `-update` only where a fixture deliberately adds the metric. Integration fixtures must use a unique node name (or the per-test discriminator) to avoid the shared-VM series-leak that bleeds series into alphabetically-later tests.
- **[Extra topology query]** — one more parallel PromQL in the `ReadTopology` errgroup; node-condition cardinality is tiny vs pods and bounded by upstream VM search limits like everything else.

## Migration Plan

Additive behaviour, no API shape break (`data.ready_status` is a new `omitempty` field). Deploy normally. Operators wanting the attribute need a kube-state-metrics that exports `kube_node_status_condition` (a KSM default). Rollback = previous binary; nodes simply lose `ready_status`.

## Open Questions

None. (Surfacing the other four node conditions is explicitly out of scope and would be a separate additive change reusing this resolver shape.)
