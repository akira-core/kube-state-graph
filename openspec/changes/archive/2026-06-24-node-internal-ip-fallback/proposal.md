# Proposal: node-internal-ip-fallback

## Why

K8s node entries only surface `ipaddress` when `kube_node_status_addresses` carries a `type="ExternalIP"` row. Private/on-prem clusters (and most managed node pools behind NAT) expose **no** ExternalIP — every node renders without any IP, even though `type="InternalIP"` is always present. Operators need a usable node IP in the graph for these deployments.

## What Changes

- Node-addresses topology query widens its selector from `type="ExternalIP"` to `type=~"ExternalIP|InternalIP"` (anchored regex; same metric, same labels).
- K8s node `ipaddress` resolution becomes: **ExternalIP when present, otherwise InternalIP, otherwise omitted**. ExternalIP always wins when both exist.
- Determinism preserved: lexically-smallest address wins on duplicate `(cluster, node)` samples **within each address type**, and the type preference is applied after — emitted IP stays a pure function of the data, not upstream vector order (D6).
- No new node/edge types, no `labels` change (`labels.external_ip` / `labels.internal_ip` stay forbidden), no serialiser shape change — `data.ipaddress` remains a single-element `omitempty` slice.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `cluster-topology-source`: the "Entity canonical fields" K8s-node rule and the node-addresses metric contract gain the InternalIP fallback; the `KSG_METRIC_PREFIX` query-shape scenario updates to the widened selector.
- `graph-api`: the "Typed ipaddress attribute" requirement for `type="node"` nodes gains the fallback rule (ExternalIP preferred, InternalIP fallback, omitted only when neither row exists).

## Impact

- `pkg/promql/queries.go` — `QNodeAddresses` rendered selector.
- `pkg/build/topology.go` — `parseTopology` external-IP map becomes a two-tier (per-type) pick.
- Tests: `pkg/promql/queries_test.go`, `pkg/build/topology_test.go` / `topology_fixes_test.go`, `internal/api` component/golden fixtures as needed, `internal/integration` fixture series.
- Docs: CLAUDE.md load-bearing rules paragraph for `ipaddress`; promoted specs above (via this change's delta specs).
- No new dependencies, no API surface change, no self-metric change (`query_name` constants stay bare).
