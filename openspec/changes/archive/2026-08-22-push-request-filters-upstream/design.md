# Design — push-request-filters-upstream

## Context

See `proposal.md` — Why. The relevant current state:

- `promql.Render(q, window)` is a pure switch over ~35 `Query` constants. A
  handful of queries carry a **fixed** selector (`type=~"ExternalIP|InternalIP"`,
  `condition="Ready"`, `lun=""`, the service-graph sentinel +
  `edge_relation!="link"`); every other query is a bare metric name wrapped in
  `last_over_time(...)` / `tlast_over_time(...)` / `rate(...)`. The
  `replace-storageclass-with-netapp-nodes` change has just reduced the renderer
  to this pure function by deleting `Renderer.Prefix`.
- `build.Builder.Build(ctx, window, end)` runs `ReadTopology` (one errgroup of
  KSM + kubelet + Harvest legs, each calling `Render`), the outside-retention
  probe, then `ReadServiceGraph` (three trace legs) whose `sgResolver` resolves
  every series against the topology indexes (`podByID`, `podByUID`,
  `svcCandidates`, `ipIndex`, `podIPCandidates`, `endpointsByService`) and
  **materialises as it resolves**: `synthPod`, `external`,
  `materializeServiceNode`, `addServiceEdge`, `addRouteChainEdge`,
  `markIngressService`, `noteExternal`.
- `kubegraph.ParseValues(url.Values)` returns `(start, end, graph.Scope, err)`;
  `graph.Scope` carries `Clusters`, `Namespaces`, `EdgeTypes`, `Names`, `Root`,
  `Depth`, `Direction`; `graph.Project` runs `traverse` (BFS) then
  `connectivityExcluded` (suppressed by `Names`/`Root`), then `filterNodes`
  with the reference-driven `infraNodePassesFilters` /
  `netappInfraPassesFilters` (both with `name` / `root` exceptions) and
  `filterEdges` / `readdEdgePartners`.
- `internal/api` owns `/v1/clusters` (its own `Render(QClusterDiscovery, 1h)`
  read) and the `/readyz` `up{}` probe; `internal/config` owns flags + `KSG_*`
  env with `Validate()`.
- Tests fake upstream with `MockQuerier` whose `fixtureSet` matches a
  **substring needle of the rendered query** — it cannot filter a vector by a
  matcher on its own.

Constraints carried over unchanged: no Kubernetes client; `pkg/` must not
import `internal/`; `pkg/build` must not import `pkg/route`; deterministic
body; strict `labels` map; edge IDs are UUIDv5 over `type|source|target`.

## Goals / Non-Goals

**Goals:**

- One rendering path for request-scoped matchers, composed with (never
  replacing) each query's fixed selector, driven by a static per-query
  dimension table so "which series gets which matcher" is a compile-time fact.
- A filtered build that is **self-consistent**: every node is loaded topology,
  a Service from the loaded index, or a label-derived `external`; no series
  materialises anything unless it is admitted; no synthesised pods.
- Byte-identical unfiltered output (queries and body), so the existing goldens
  stay green and the change is verifiable by diff.
- The request contract stays single-sourced in `kubegraph.ParseValues`.

**Non-Goals:**

- No caching, no query batching, no change to the trace-side resolution
  ladders beyond the "UID not loaded ⇒ treat as empty" rule.
- No new node/edge type, attribute, or `labels` key (`external` is reused
  as-is; no `ghost` flag).
- No per-family opt-out of `az` / `env` (the precondition is on the operator).
- No 400 for the withdrawn `name` / `root` / `depth` / `direction` parameters.

## Decisions

### D1 — A `promql.Selector` value plus a static dimension table drives rendering

`Render` becomes `Render(q Query, window time.Duration, keys LabelKeys, sel Selector) string` with

```go
type LabelKeys struct{ AZ, Env string }       // validated upstream; DefaultLabelKeys() = {"az","env"}
type Selector struct{ AZ, Env, Cluster, Namespace []string }
func (s Selector) Active() bool             // any dimension non-empty
```

and a package-level table `queryDims map[Query]dims` (a bitmask of
`dimAZ|dimEnv|dimCluster|dimNamespace`): pod/claim/Service/EndpointSlice/kubelet
series → all four; `kube_node_*` → AZ|Env|Cluster; Harvest → AZ|Env; the
three trace queries and `up` → none. `Render` computes
`fixed + sel.render(queryDims[q], keys)` and wraps the result in `{...}` only
when non-empty, so a query with no fixed selector and no active dimension
renders exactly today's string. Rendering a dimension: values are
de-duplicated and sorted; one value → `key="v"` with `\` and `"` escaped; two
or more → `key=~"<QuoteMeta(v1)>|<QuoteMeta(v2)>"`; the `Cluster` value
`unknown` contributes TWO alternatives — the literal and the empty string —
and forces the regex form (`cluster=~"unknown|"` alone, `cluster=~"alpha|unknown|"`
mixed), because the bucket is reachable under both spellings and the parse
layer cannot tell them apart. Dimension order inside the selector is fixed
(`az`, `env`, `cluster`, `namespace`) after the fixed part.

*Why a table, not per-case code:* the contract is "series × dimension", and a
table is greppable, testable as a unit (`TestQueryDims_EveryQueryListed`
asserts every `Query` constant has an entry, so a new query cannot silently
default), and it keeps the `switch` in `Render` about query shape only.
*Alternative considered:* a `Renderer` struct holding keys and selector —
rejected because the previous change just removed renderer state for
testability, and a value parameter keeps `Render` pure.

### D2 — The selector travels as a value through `Build`; keys live in options

`build.Options` gains `LabelKeys promql.LabelKeys` (zero value ⇒ defaults);
`Builder.Build(ctx, window, end, sel promql.Selector)`; `ReadTopology` and
`ReadServiceGraph` take `sel` (the latter only to derive `filtered :=
sel.Active()` — it never renders it). `kubegraph.Options` mirrors `LabelKeys`;
`Engine.Build(ctx, window, end, sel)`; `kubegraph.ParseValues` returns a
`Request{Start, End time.Time; Scope graph.Scope; Selector promql.Selector}`
instead of four values (the signature changes anyway, and a struct stops the
next parameter from being a fifth return). `Engine.BuildFromValues` is
unchanged for callers.

*Why `promql.Selector` and not a `build.Selector`:* it is literally "what gets
rendered into PromQL"; `pkg/build` already imports `pkg/promql`, and a second
type would need a conversion that can drift.

### D3 — Value validation happens once, in `ParseValues`

`ParseValues` rejects (400 `invalid_scope`) any selector value longer than 253
bytes or containing a control character, and any `prune` value other than
`true` / `false`; empty values are skipped (as `stringSet` already does for
`cluster` / `namespace`). `Render` then only escapes. Validation is not
repeated in `Render` because an embedder constructing a `Selector` directly is
trusted code, and double validation would give two places to disagree. The
Kubernetes namespace and cluster charsets are deliberately *not* enforced —
`az` / `env` values are operator-defined strings, and a stricter rule would
have to be per-dimension for no gain (quoting already makes injection
impossible).

### D4 — `graph.Scope` loses traversal and names, gains an `Inventory` flag

`Scope` becomes `{Clusters, Namespaces, EdgeTypes, Inventory bool}`;
`NewScope(clusters, namespaces, edgeTypes []string, inventory bool)`. The flag
is the **inverse** of the `prune` parameter (`prune=false` ⇒ `Inventory=true`)
so that the zero-value `Scope{}` still means "default prune on" — property
tests and embedders that build a `Scope` literal keep today's semantics.
`traverse`, `MaxTraversalDepth`, `Direction`, `NameFilterActive`, and the
name/root branches of both infra predicates are deleted. `Project` computes
`excluded = connectivityExcluded(g)` unless `scope.Inventory`.

The infra-lift rule of the spec maps onto the two predicates directly:

```go
infraNodePassesFilters:   cluster check; if scope.Inventory && len(scope.Namespaces)==0 { return true }; return referenced[id]
netappInfraPassesFilters: if scope.Inventory && len(scope.Clusters)==0 && len(scope.Namespaces)==0 { return true }; return referenced[id]
```

`pullNetAppParents` stays (an inventory-admitted aggregate still needs its
controller). `readdEdgePartners` loses the "name-anchored view" comment but
not its logic — partner re-add is still how a `pod-to-node` edge pulls in its
node and how an unfiltered build keeps a cross-cluster partner.

### D5 — Filtered build: "UID not loaded ⇒ treat the side as UID-empty"

`newSGResolver(topology, filtered bool)` stores the flag. In `resolveClient`,
when `lookupClientPod` misses and `r.filtered`, return
`r.resolveEmptyUID(label, traceCluster, t), false` instead of synthesising; in
`resolveServer`, when `podByUID` misses and `r.filtered`, fall through to the
same branches the empty-UID case takes (`resolveUnknownServerPeer` for
`server="unknown"`, else `resolveEmptyUID`). That single rule yields every
outcome the spec lists without new code paths: a `"://"` label goes through
connection-string resolution (and may land on a **loaded** Service — an
anchor); `"unknown"` goes through the peer ladder (which still requires a real
client pod, so an out-of-scope caller's unknown server is dropped exactly as
today); any other non-empty label becomes `external/<label>` via the D27
fallback; an empty label makes the side wholly empty, and the existing
wholly-empty-side rule drops the series. `synthPod` is therefore unreachable
when `filtered` — enforced by a test, not by deleting the function (unfiltered
builds still need it).

*Alternative considered:* a dedicated "out-of-scope" branch that directly
emits `external/<label>`. Rejected: it would bypass connection-string and
ClusterIP resolution for a side whose label *can* reach loaded topology, and
it duplicates the D27 fallback's precedence rules.

### D6 — Admission by journal and rollback, after resolution, before the pair is recorded

Resolution side effects stay where they are; the resolver gains a per-series
**journal** that is active only when `filtered`:

- `external`, `materializeServiceNode`, `addServiceEdge`, `addRouteChainEdge`
  append the key they **newly created** (idempotent re-hits append nothing);
  `markIngressService` appends `(id, previousRole)`; `noteExternal` appends
  the reason instead of incrementing `extReasons` (the slog line still fires —
  it is diagnostic, not output).
- After `resolveServer` returns, `parseWithResolver` evaluates
  `admitted := anyLoaded(srcIDs) || anyLoaded(tgtIDs)` where `anyLoaded(id)`
  is `podByID[id] != nil || services[id] != nil` (a Service in `services` was
  materialised from the loaded index, so membership is the "loaded topology"
  test; externals never count).
- `admitted` ⇒ `commit()` (apply buffered `noteExternal` counts, clear the
  journal) and continue into the existing `pairs` / RED / link-marker logic.
  Not admitted ⇒ `rollback()` (delete journalled keys, restore roles, drop
  buffered counts) and `continue` — the series touches neither `pairs`, the
  RED join, nor `linkPairs` / `transportPairs`.

When `!filtered` the journal is never consulted and `admitted` is not
evaluated, so the unfiltered parse is byte-identical. The journal is a pure
function of one series plus the committed state, and commit/rollback happen
before the next series is read, so D6 determinism (order-free output) holds:
a dropped series can never leave a node that a later series would have found
already present.

*Alternative considered:* pre-resolution admission (inspect UIDs and labels
first). Rejected: deciding whether a `"://"` or peer-address side reaches a
loaded Service requires the full classification chain, so the pre-check would
be a second copy of the ladders.

### D7 — Outside-retention check runs only for an unfiltered build

In `Builder.Build`, the `len(Pods)==0 && len(Nodes)==0` branch is guarded by
`!sel.Active()`. A filtered build with no rows returns an empty graph; the
existing `graph built` log line gains `selector_active` so an operator can
tell the two apart. No `up{}` probe is issued for a filtered empty result.

### D8 — `/v1/clusters` removal is a deletion, not a deprecation

Route, handler, DTOs, swag block, `QClusterDiscovery`, `ClusterDiscoveryLookback`
and its `Render` case go; `--api-timeout`'s description and both READMEs are
edited. `observability` keeps its `query`-labelled histograms (the
`cluster_discovery` value simply stops appearing). No redirect or 410: a
removed v1 route returns the router's 404 like any unknown path.

### D9 — Configuration surface

`Config` gains `AZLabel`, `EnvLabel` (defaults `az`, `env`; env
`KSG_AZ_LABEL` / `KSG_ENV_LABEL`; flags `--az-label` / `--env-label`).
`Validate()` checks each against `^[a-zA-Z_][a-zA-Z0-9_]*$` and that they
differ; `cmd/` passes `promql.LabelKeys{AZ: cfg.AZLabel, Env: cfg.EnvLabel}`
into `build.Options`. The server also exposes the keys to the swag `@Param`
descriptions only as prose (the OpenAPI parameter names are fixed).

### D10 — Test strategy

- **`pkg/promql`**: table tests over every `Query` × selector shape (empty,
  single, multi, `unknown` cluster, regex metacharacters, custom keys), plus
  the completeness test that every constant has a `queryDims` entry and that
  the three trace queries and `up` map to none. A golden-style test asserts
  the empty selector reproduces today's strings verbatim for all queries.
- **`pkg/build`**: resolver tests for D5 (no synth when filtered; external by
  label; `"://"` to loaded Service anchors) and D6 (external↔external dropped
  with no residue in `externals` / `services` / `svcEdges` /
  `routeChainEdges` / `extReasons`; role rollback; same output under both
  series orders; unfiltered path unchanged). Retention gating (D7).
- **`pkg/graph`**: `Project` tests for `Inventory` (podless node admitted
  without namespace filter, not with; NetApp lift requires no cluster and no
  namespace); property tests keep "every edge endpoint resolves" and
  "filtered ⊆ unfiltered", drop the traversal and name properties, add
  "pruned view ⊆ inventory view".
- **`internal/api` goldens**: the mock cannot filter, so filtered goldens key
  their `fixtureSet` needles on the **rendered matcher fragment**
  (`kube_pod_info{namespace="shop"`) and supply the pre-narrowed vector; a
  companion component test captures every `Instant` call and asserts the
  exact query strings for a filtered request (that is what proves the
  push-down, the golden proves the body). `name-filter-cytoscape.json` is
  deleted; new goldens: `namespace-pushdown`, `az-env`, `prune-false`,
  `cluster-filter-external-partner`.
- **Integration**: the real VictoriaMetrics does the filtering. Fixtures gain
  `az` / `env` on every topology family and `cluster` on kubelet series; tests
  per the `container-integration` delta, including the `cluster=~"alpha|"`
  absent-label match and the Harvest-without-`env` degrade.

### D11 — Landing order

1. `replace-storageclass-with-netapp-nodes` archives (its `netapp-storage-graph`
   spec and Harvest legs are this change's base).
2. This change lands; `docs/swagger.*`, READMEs, and `CLAUDE.md` rules
   ("no filters pushed to PromQL", parameter list, `/v1/clusters`) are updated
   in the same PR.
3. No downstream coordination: the `graph-api-gateway` dependency is dropped.
   The `pkg/kubegraph` signature changes are recorded in the release note for
   any future in-process embedder.

## Risks / Trade-offs

- [A topology family lacks the configured `az` / `env` label] → It vanishes
  from filtered requests; the connectivity prune can then empty the graph.
  Mitigation: the `graph built` log reports per-family raw series counts; add a
  single Warn when a selector is active, KSM returned rows, and a kubelet or
  Harvest family returned zero (`selector_family_empty`). Documented as an
  operator precondition in three specs.
- [Full service-graph read is the per-request cost floor] → Unchanged from
  today; the raw histogram leg dominates. Accepted; the saving is on the
  topology side where pod count scales.
- [Mock-based tests can pass vacuously because the mock ignores matchers] →
  D10 pairs every filtered golden with a captured-query assertion, and the
  integration suite exercises the real filter.
- [Journal/rollback bugs leave residue or drop admitted output] → The D6 tests
  assert the resolver's maps are identical before and after a rejected
  series and that output is order-free; the journal is only consulted when
  `filtered`, so an unfiltered regression is impossible by construction.
- [Same-named out-of-scope peers merge into one `external/<label>`] →
  Accepted; `external` has no cluster/namespace dimension by design, and the
  merge is what keeps a namespace view readable.
- [`cluster=~"alpha|unknown|"` relies on regex-empty matching an absent label]
  → Prometheus and VictoriaMetrics both treat an absent label as `""` for
  matchers; verified by an integration test. If a store disagreed, the
  fallback is to render the empty alternative as its own `cluster=""` matcher
  in a separate query and union the results — no approach change.
- [Withdrawn parameters silently ignored] → A client sending `?name=` gets a
  bigger response than before rather than an error. Accepted per proposal;
  the release note calls it out.
- [`Scope` zero value] → `Inventory=false` keeps prune-on as the default for
  literals; documented on the field.

## Migration Plan

1. Deploy the new binary with no configuration change: defaults `az` / `env`
   apply; unfiltered requests are byte-identical; `/v1/clusters` returns 404.
2. Operators set `KSG_AZ_LABEL` / `KSG_ENV_LABEL` if their scrape labels
   differ, and confirm every topology family carries them (one `count by
   (<key>) (volume_labels)` / `... (kube_pod_info)` query each).
3. Clients migrate: `/v1/clusters` → the `clusters` field of `/v1/graph`;
   `name` / `root` → selector-level filters (+ `prune=false`).
4. Rollback: redeploy the previous image — no stored state, no schema.

## Open Questions

- The exact `selector_family_empty` heuristic thresholds (log only; tunable
  without spec impact).
