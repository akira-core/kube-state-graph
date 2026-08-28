## Why

The estate reuses short cluster names across zones and environments —
`c1` and `c2` exist under `us`/`dev` and again under `eu`/`prod` — so the raw
`cluster` label is not a cluster identity: today `us-dev-c1` and `eu-prod-c1`
collapse into ONE graph cluster (`c1/worker-0` is one K8s node id, one
`cluster/c1` compound group, one `ServicesByNameNS` bucket), and the response
silently merges two unrelated clusters. The zone and environment that make the
name unique are already stamped on every series as the external labels the
`?az=` / `?env=` filters match on; the reader just never folds them into the
identity.

## What Changes

- **BREAKING (identity):** a Kubernetes cluster's identity becomes
  `<az>-<env>-<cluster>`, composed by the topology reader at the moment a
  series' `cluster` label is read, from that series' own zone and environment
  labels under the operator-configured `--az-label` / `--env-label` keys.
  Everything keyed by cluster follows without further change: pod / K8s node /
  PVC / service `id` prefixes, `labels.cluster` on nodes and edges, the
  `type="cluster"` compound group (`id` and `name`), `clusters[]`, every
  topology join key, the cluster-family key, cross-cluster detection, and the
  `cluster` label on the self-metric gauges. `us-dev-c1` and `eu-prod-c1` are
  two clusters.
- A series that carries the `cluster` label but not both zone and environment
  is resolved through one ladder: if its raw name maps to exactly one identity
  observed in the same build, it joins that cluster; otherwise it stays a
  cluster of its own under the raw name and the build logs
  `cluster_identity_unresolved` once per metric. A series with no `cluster`
  label composes like any other (`us-dev-unknown`). The same ladder resolves
  every cluster name arriving from outside the topology — the service-graph
  `cluster` label and the route store's cluster names.
- **`?cluster=` keeps its meaning: the raw Kubernetes cluster name**, matched
  upstream as `cluster="c1"` exactly as today and re-applied at projection
  against the identity's raw-name component, so `?cluster=c1` selects every
  `c1` and `?az=us&env=dev&cluster=c1` pins one. `clusters[]` now lists
  identities, which are therefore NOT valid `?cluster=` values (documented).
- **`pod-calls-*` edge `labels.cluster`** is the client pod's cluster identity
  when the client resolved to a topology pod, else the trace `cluster` label
  resolved through the ladder — revising the "raw trace label" rule so an edge
  never names a cluster its endpoints do not.
- The **cluster-family** rule (digit runs → `0`, byte-equal keys) is applied
  unchanged to the identity string, so a family is now scoped to one zone and
  one environment: `us-dev-c1` ~ `us-dev-c2`, `eu-prod-c1` ≁ `us-dev-c1`.
- No new node type, edge type, `labels` key, query, request parameter, or
  configuration knob. The identity format is fixed. A build whose series carry
  no zone/environment pair renders byte-for-byte as today.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `cluster-topology-source`: new requirement "Cluster identity composed from
  zone and environment labels" (the composition, the resolution ladder, the
  identity table exposed on the built graph, the family key over the
  identity); "Cluster-scoped IDs" (the `<cluster>` segment is the identity);
  "Series missing the cluster label" (`unknown` composes; `?cluster=unknown`
  still addresses it); "Configurable `az` / `env` label keys" (the keys also
  bind the identity read).
- `graph-api`: "Cytoscape.js response shape" (`id` prefixes, `labels.cluster`,
  `clusters[]` carry the identity); "Filter parameters" (`?cluster=` is the raw
  name at both layers; pinning one cluster needs `az` + `env`); "Cross-cluster
  edge representation" (same raw name in two zones is cross-cluster);
  "Availability-zone and environment selector filters" (the labels now also
  define identity; `clusters[]` lists identities).
- `pod-service-graph`: "Edge cluster label" (client-pod identity; trace label
  through the ladder).

## Impact

- `pkg/build`: `missingClusterCounts` grows into a `clusterResolver` that owns
  the label keys, a two-pass raw → identity table, and both aggregated
  warnings; the 20 `mc.bucket(metric, cluster)` call sites (`topology.go` ×17,
  `netapp.go` ×2, `servicegraph.go` ×1) become `bucket(metric, s.Metric)`;
  `parseTopology` receives the label keys; `Topology` carries the resolver for
  `ReadServiceGraph` and the route path; `Build` hands the identity table to
  the graph.
- `pkg/graph`: `ClusterIdentity{AZ, Env, Name}`, `Graph.ClusterIdentities`
  (nil-safe, set by the builder), `Graph.ClusterRawName`; the two
  `scope.Clusters[labels["cluster"]]` checks in `project.go` compare the raw
  component.
- `pkg/cytoscape`: no code change — the group id/name and `clusters[]` derive
  from `labels.cluster`.
- `pkg/route`: no code change; the store's cluster names pass through the
  resolver at the `pkg/build` boundary.
- Tests: unit (`pkg/build`, `pkg/graph`), one new golden, and the
  `internal/integration` filtered suite — whose fixtures already stamp
  `az="zone-a",env="prod"` on every series — becomes the end-to-end proof and
  has its id assertions moved to the composed form. Every fixture without the
  pair (all other goldens, `MultiBackendSuite`'s az-only `mb-alpha`) must stay
  byte-identical.
- Docs: `docs/BREAKING.md` (identity, `clusters[]` vs `?cluster=`, edge label,
  cross-cluster, route-store naming), `CLAUDE.md` load-bearing rules
  ("Cluster-scoped IDs everywhere", D9, family), and
  `docs/kube-state-metrics-preconditions.md` (both labels on every
  cluster-keyed family or the join misses).
- Demo (`kube-state-graph-demo`): `global.ksgExternalLabels` already stamps
  all three; the single demo cluster renders as
  `<az>-<env>-<cluster>` and the dashboard's `cluster` dropdown (sourced from
  `kube_pod_info`, i.e. the raw name) keeps working.
