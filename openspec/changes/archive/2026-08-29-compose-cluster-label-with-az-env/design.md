## Context

See proposal.md — Why. Four properties of the existing code shape everything below.

**Every cluster-keyed structure is minted from ONE call.** `missingClusterCounts.bucket(metric, c)` in `pkg/build/topology.go` is where a series' `cluster` label becomes the value that `graph.PodID` / `K8sNodeID` / `PVCID` / `ServiceID`, every `labels["cluster"]`, every `podKey` / `pvcKey` / `serviceKey` / `sliceKey`, `ServicesByNameNS`, `EndpointsByService`, the kubelet usage join, the trace-cluster anchor, and `clusters[]` are keyed on — twenty call sites, all of the shape `mc.bucket(promql.QX, string(s.Metric["cluster"]))`. Changing what that call returns changes the identity everywhere at once, with no consumer needing to know.

**`az` / `env` are per-series facts the reader already has in hand.** They are agent-stamped external labels present on every kube-state-metrics and kubelet series (the precondition `docs/kube-state-metrics-preconditions.md` states for the filters) and, in the demo, on the service-graph series too. The key names are configurable (`promql.LabelKeys`) and `ReadTopology` holds them; `parseTopology` currently does not receive them.

**`?cluster=` lives at two layers that must agree.** The selector renders `cluster="c1"` upstream against the RAW label; the projection (`nodePassesFilters`, `infraNodePassesFilters` in `pkg/graph/project.go`) re-applies the same value set against `labels["cluster"]` as defence in depth. Whatever the identity becomes, the projection must still accept the raw value the upstream matcher accepted.

**Two cluster names arrive from outside the topology.** The service-graph `cluster` label (trace source, "frequently missing or wrong" — `ReadServiceGraph` already prefers the UID-recovered client pod's cluster as the anchor) and the route store's cluster names (`ClustersWithIngressIP`, `RouteDestination.Cluster`, written by a separate exporter). Both are raw names today and are looked up against topology keyed by cluster.

## Goals / Non-Goals

**Goals:**

- One mechanism, one place: the identity is composed where the raw label is read, and nothing downstream is taught about zones.
- `us-dev-c1` and `eu-prod-c1` are two clusters in every structure — ids, groups, joins, families, cross-cluster status, `clusters[]`, self-metrics.
- A build whose series carry no zone/environment pair is byte-identical to today (tested by the existing goldens, not asserted).
- The request surface does not move; `?cluster=` keeps its upstream meaning.

**Non-Goals:**

- Accepting the composed identity as a `?cluster=` value (hyphen-ambiguous; the user chose raw).
- A struct-aware family key that ignores digit runs inside zone/environment values (D4 explains the caveat; a follow-up if it bites).
- Teaching `pkg/route` about identities — its names pass through the resolver at the `pkg/build` boundary (D9).
- Any new wire field, label key, or configuration knob.

## Decisions

### D1 — Compose at the bucket step, in `pkg/build`, and nowhere else

`missingClusterCounts` becomes a `clusterResolver` whose `bucket(metric, m model.Metric) string` reads `m["cluster"]`, `m[keys.AZ]`, `m[keys.Env]` and returns the identity. All twenty call sites migrate from passing the cluster string to passing the metric. Every ID constructor, label map, join key, index and the `clusters` set inherit the identity because they are built from that return value.

*Alternative — rewrite at serialisation (the previous revision of this change).* Rejected: a caption cannot split two clusters that the reader has already merged into one id space.

*Alternative — compose in `graph.PodID` & co.* Rejected: the ID constructors take only the cluster string, and the join keys (`podKey`, `pvcKey`, …) are struct literals built beside them; the label-read site is the single chokepoint.

### D2 — One resolution ladder for every cluster name, topology or foreign

For a name read from a series (or handed in from the trace label / route store) the resolver applies, in order:

1. **Compose** — the series carries both zone and environment (non-empty under the configured keys): identity = `az + "-" + env + "-" + raw`.
2. **Adopt** — otherwise, if the raw name maps to **exactly one** identity in this build's identity table, return that identity.
3. **Verbatim** — otherwise return the raw name (the `unknown` bucket when the `cluster` label is absent), count it against the metric, and let `warn()` emit one aggregated `cluster_identity_unresolved` per metric beside the existing missing-cluster warning.

The identity table is built by a **first pass** in `parseTopology` over the four families that mint cluster-labelled entities — `kube_pod_info`, `kube_node_info`, `kube_service_info`, `kube_pod_spec_volumes_persistentvolumeclaims_info` — collecting every step-1 composition into `raw → set(identity)`. The second pass (the existing parse) then buckets every series through the ladder. Kubelet, endpointslice, owner, annotation and NetApp-kubelet families are join inputs, not entity sources: they never add to the table, they only resolve through it. `Topology` carries the resolver so `ReadServiceGraph` (trace `cluster` label, route-store names) resolves through the SAME table — it cannot drift, and the table is built once per build.

Step 2 is what keeps a partially-stamped estate whole: a kubelet or owner family that lacks the pair still joins its cluster whenever the raw name is unambiguous in the build — always the case under `?az=&env=`, and the common case elsewhere. Step 3 is the honest failure: two identities under one raw name and a series that says only `c1` cannot be assigned, so it becomes its own `c1` cluster (joins miss, entities render under `c1`) and the Warn names the metric. Nothing is guessed.

*Alternative — step 2 picks the lexically-smallest identity on ambiguity.* Rejected: it would silently attach `c1`'s kubelet usage to `eu-prod-c1` when it belonged to `us-dev-c1`; a visible `c1` orphan cluster plus a Warn is the failure an operator can act on.

### D3 — `?cluster=` is the raw name at both layers; `clusters[]` is the identity

Upstream: unchanged, `cluster="c1"` (`cluster=~"unknown|"` for the bucket). Projection: `nodePassesFilters` / `infraNodePassesFilters` compare `scope.Clusters` against `g.ClusterRawName(labels["cluster"])` — the identity's `Name` component from `Graph.ClusterIdentities`, falling back to the label itself when the table has no entry (an unresolved raw name, or a graph built without the table). So `?cluster=c1` admits `us-dev-c1` and `eu-prod-c1` at projection exactly as the matcher did at source, and `?az=us&env=dev&cluster=c1` pins one — the three orthogonal selector dimensions the API already has ARE the identity's three components.

`clusters[]` derives from `labels.cluster` and therefore lists identities. They are not valid `?cluster=` values; `docs/BREAKING.md` says so and names the pinning form. The response is otherwise self-describing: an identity string tells the reader which `az` / `env` / `cluster` triple to send.

*Alternative — accept the composed form in `?cluster=` and split it into three matchers.* Rejected: `us-east-1-prod-c1` cannot be split without knowing the zone set, and the user chose the raw form.

### D4 — The family key is the unchanged string rule over the identity

`build.ClusterFamilyKey` (every maximal digit run → `0`) is applied to the identity string: `us-dev-c1` → `us-dev-c0`, `us-dev-c2` → `us-dev-c0` (family), `eu-prod-c1` → `eu-prod-c0` (not). This gives the user's choice — same zone, same environment, digit-normalised name — with zero code change, and keeps `pkg/route`'s ingress-cluster pick (which calls the same exported function) in agreement with the fan-out.

**Caveat, documented:** digit runs inside the zone or environment value normalise too, so `us-east-1-prod-c1` and `us-east-2-prod-c1` share family `us-east-0-prod-c0`. A struct-aware key would need the identity table inside `pkg/route` (a `BuildScoped` signature change) and is deferred; for zone names like `us` / `eu` / `zone-a` the two rules coincide.

### D5 — Edge `labels.cluster` names the client pod's identity (revises archived D9)

When the client side resolved to a topology pod, `srcCluster` is that pod's `labels.cluster` (the identity); otherwise it is the trace `cluster` label resolved through D2. The archived D9 kept the raw trace label because there was no better source; now the label is not an identity at all, and the anchor-cluster logic already trusts the UID-recovered pod over the label. Cross-cluster detection is unaffected (it compares node labels), but the edge no longer names a cluster that appears nowhere else in the body.

### D6 — `unknown` composes like any other name

A series with no `cluster` label but both zone/environment labels becomes `us-dev-unknown`; the raw component stays `unknown`, so `?cluster=unknown` still renders `cluster=~"unknown|"` upstream and still matches at projection through D3. A per-`(az, env)` unknown bucket is strictly more useful than one global bucket, and exempting the sentinel would be the only special case in the ladder.

### D7 — The identity table rides on `graph.Graph`, nil-safe

`graph.ClusterIdentity{AZ, Env, Name string}` and `Graph.ClusterIdentities map[string]ClusterIdentity` (identity → components), assigned by `Builder.Build` right after `graph.NewGraph` the way `BuiltAt` is a plain field; `NewGraph` and `Serialise` signatures do not move (D32). `Graph.ClusterRawName(id string) string` is the one pure accessor the projection uses. `pkg/graph` gains no import and no knowledge of the composition rule — it stores what the reader decided.

### D8 — Compatibility is the existing goldens; the filtered suite is the new proof

No fixture under `internal/api/testdata/golden`, `pkg/cytoscape`, `pkg/graph` or `pkg/build` (bar the selector-rendering tests, which never build a graph) carries the zone/environment pair, so every existing golden must pass unchanged. `internal/integration/filtered_e2e_test.go` stamps `az="zone-a",env="prod"` on EVERY series through `ExtraLabels` — its id assertions move from `cluster-alpha/…` to `zone-a-prod-cluster-alpha/…` and it becomes the end-to-end demonstration; `MultiBackendSuite`'s `mb-alpha` carries `az` only and must stay raw.

### D9 — The route store passes through the ladder at the `pkg/build` boundary

`RouteRequest.CallerCluster` is the caller pod's identity; `RouteDestination.Cluster` and the ingress identity fields are resolved through D2 before the topology lookup (`resolveServiceLevelInCluster`). A store that writes identity names hits step 1's table directly (the recommended contract, stated in the route documentation); a store that writes raw names resolves through step 2 wherever the name is unambiguous in the build and degrades through the EXISTING `route_engine_dest_cluster_lacks_service` reason otherwise — no new engine outcome, no `pkg/route` change, `make check-route-containment` unaffected.

### D10 — Self-metric label VALUES move; label sets do not

`kube_state_graph_graph_node_count{cluster}` / `..._edge_count` and `clusters_observed` see identity values and identity counts. Archived D26 governs label *sets*; values reflecting a corrected identity are the metric doing its job.

## Risks / Trade-offs

- [A cluster-keyed family without the pair, in a build where its raw name is ambiguous, becomes a `c1` orphan cluster and its joins miss] → deliberate (D2 step 3): visible in the body and in `cluster_identity_unresolved`; `docs/kube-state-metrics-preconditions.md` upgrades the precondition from "for the filters" to "for identity" and gives the `count by (cluster, az, env)` sanity query.
- [Clients that parsed `id` prefixes or matched `labels.cluster` against `?cluster=` break on stamped estates] → declared BREAKING with the migration (send the triple; read `clusters[]` as identities).
- [Digit runs inside zone/environment values widen the family] → D4 caveat, documented; zone names in the user's estate carry none.
- [A route store writing raw names in a multi-zone build degrades to external where it previously resolved] → D9: the store should write identities; the degrade reason is the existing one and names the cluster.
- [Two passes over the four identity families] → linear, in-memory, over vectors already loaded; no new query.

## Migration Plan

No configuration. Deploy; every cluster whose series carry both labels acquires its identity on the next build. Rollback is a binary downgrade. Clients: replace any `?cluster=<clusters[] value>` round-trip with `?az=&env=&cluster=` using the components. Embedders on `pkg/` recompile unchanged; one that inspects ids should read `Graph.ClusterIdentities`.
