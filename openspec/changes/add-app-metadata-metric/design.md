# Design

## Context

See proposal.md — Why. Current state, as relevant here:

- `pkg/build/topology_owners.go`: a pod's Application is `resolveOwnerApplication`
  over the controller already resolved for `data.owner` (ReplicaSet collapsed to
  its Deployment) against six `kube_*_annotations` families, plus the one
  Job → CronJob hop. `ResolvePodApplication` (`podapplication.go`) applies the
  same chain through `Router.QueryLabels`, family `ksm`, one query per hop.
- `pkg/promql`: every server-issued query is a compile-time `Query` constant,
  classified by two exhaustive tables, `queryDims` (which request dimensions
  render as matchers / route) and `queryFamily` (which backends serve it).
  `Render(q, …)` uses `string(q)` as the metric name. There is no configurable
  metric name anywhere (`--metric-prefix` was removed). `alerts` is the one
  optional family: unserved ⇒ the leg returns an empty vector at Debug, while
  `Router.QueryLabels` errors on an unserved family.
- `pkg/cytoscape.Serialise` synthesises `namespace` / `application` groups from
  the real nodes' `labels.namespace` and `Application()`; every group carries
  `labels: {}`. `NodeData.Owner` is already `*graph.Owner` (`{kind, name}`).
- `/v1/storage-graph` resolves pod Applications too: `readScopedControllers`
  (`controllerscope.go`) waits on the pod wave and reads the controller
  families restricted to the loaded pods' owners. An `application=` root is
  recovered by `readScopedApplications` (`appscope.go`), three stages from
  tracking-id to pods; the recovery is a candidate generator and the projection
  judges on the resolved `Application()`. Every storage query fails closed
  except `ALERTS`.

## Goals / Non-Goals

**Goals:**

- With the metric unset, every query, body and golden is byte-identical.
- With it set, a pod's Application, a namespace's owner and an Application's
  deployer are the same value in `/v1/graph`, `/v1/storage-graph` and
  `ResolvePodApplication` for the same upstream data.
- The new query obeys every existing table-driven contract (`queryDims`,
  `queryFamily`, self-metric `query` label set, result cache); the table guard
  tests need only their ordinary new entry (D4).

**Non-Goals:**

- No change to Service Application resolution (annotation only).
- No metadata on any real node: pods, services and PVCs gain no new field.
- No configuration file, no hot reload of the label mapping.
- No configurable `cluster` label and no metric-specific `az` / `env` keys.
- The controller chain is not removed or narrowed; its queries are still
  issued when the metric is configured.

## Decisions

### D1 — Configuration is two env / flag settings, not a file

`--app-metadata-metric` / `KSG_APP_METADATA_METRIC` names the metric; empty
disables the feature. `--app-metadata-labels` / `KSG_APP_METADATA_LABELS` is a
comma-separated `field=label` list overriding any of five defaults:

| field | default label |
|---|---|
| `app` | `argocd_app` |
| `user` | `user` |
| `ns_owner` | `owner` |
| `namespace` | `namespace` |
| `pod` | `pod` |

Validation at startup (fatal): the metric name is a legal PromQL metric name;
every label is a legal label name; an unknown field or a repeated field is an
error; the five labels are pairwise distinct and none equals `cluster` or the
configured `az` / `env` label. The labels setting without the metric setting is
an error (a mapping with nothing to map is a typo).

`pkg/build` receives a typed value, `Options.AppMetadata *AppMetadataSource`
(`Metric` plus the five label names); nil disables. `kubegraph.Options` passes
it through. Parsing the `field=label` string lives in `internal/config`, since
an embedder constructs the struct directly.

The defaults and the value checks live on the type in `pkg/promql`, so an
embedder gets the same behaviour as the server: the five defaults are exported
constants, `AppMetadataSource.OrDefault()` fills every empty label field, and
`AppMetadataSource.Validate(keys)` runs the grammar, distinctness and collision
checks. `internal/config` seeds its table from the constants and delegates to
`Validate`. The build applies `OrDefault` when a read starts and never mutates
the caller's value. Neither `build.New` nor `kubegraph.New` returns an error,
so the build does not call `Validate`; like `LabelKeys`, a directly
constructed value is trusted code and the embedder calls `Validate` itself.
`ResolvePodApplication` does return an error and validates before any query.

Alternatives:

- **A YAML file beside the backends file.** Earns its keep only with list
  structure or hot reload. This is one metric with nine scalar facts, of which
  three are fixed by D2; hot reloading a label schema mid-flight would change
  rendered queries under a warm result cache. A file would also have to be
  parsed in `pkg/` (the `backendsfile` precedent) to stay embeddable. If a
  second enrichment metric ever appears, a file can subsume these two settings.
- **Nine individual env vars.** Same information, more surface, and the
  defaults stop being visible in one place.
- **Folding the mapping into the backends file.** That file declares where
  families live, not what their series look like; mixing them couples a
  routing reload to a schema change.

### D2 — `cluster` is fixed; `az` / `env` share the global binding

The metric is read through the same `clusterResolver.bucket` as every KSM
series, so its rows compose the same `<az>-<env>-<cluster>` identity and join
on the same keys. The resolver's only label inputs are `cluster` and the
global `LabelKeys`, so this needs no per-query key plumbing. A metric whose
zone labels differ from KSM's is an operator precondition violation, the same
one every KSM family already carries (`selector_family_empty` names it under
a filter).

### D3 — A new optional family `appmeta`, not `ksm`

`Families` gains `FamilyAppMeta = "appmeta"`; `requiredFamilies` is unchanged,
so `Family.Optional()` is true and the `alerts` behaviour applies verbatim: the
implicit single-backend table serves it; a backends file not listing it is
valid and the leg returns an empty vector at Debug. The family is zone-routed
and zone-matched (`dimAZ | dimEnv | dimCluster | dimNamespace`), so it is
homogeneous for `AcceptsAZ` / `RendersAZ`.

At startup, a configured metric whose family no backend serves logs one Warn
(not fatal: the routing table hot-reloads and may gain the backend). The
existing `Table.Unserved()` line already reports the family itself.

Alternative — **classify under `ksm`.** Simpler, but forces the custom exporter
into the KSM store; the operator chose a separate family.

### D4 — One logical query constant; the metric name is bound at render time

`QAppMetadata Query = "app_metadata"` is a logical name: it is the `queryDims`
and `queryFamily` key, the self-metric `query` label value, the span
`query_name` and the `issueScopedFamilies` tally key. It is never sent
upstream as a metric name. Rendering goes through dedicated renderers in the
`RenderQoSVolumeScoped` style:

- `RenderAppMetadata(src, window, keys, sel)` — the `/v1/graph` leg;
- `RenderAppMetadataByNamespace(src, window, keys, sel, cluster, namespaces)` — D9;
- `RenderAppMetadataByApp(src, window, keys, sel, apps)` — D10.

Each renders `tlast_over_time(<src.Metric>{…}[w])`, with the request's
`namespace` dimension (and any namespace restriction) on the **configured**
namespace label; `cluster`, `az` and `env` render on their fixed / global keys.
The result-cache key is the rendered string, so two configurations never share
an entry.

Two table-driven paths need care:

- `TestRender_EmptySelectorMatchesBaseline` renders EVERY `queryDims` key
  through the generic `Render`. `Render` returns `""` for `QAppMetadata` (an
  explicit empty arm keeps the switch exhaustive): an arm rendering the
  logical name would be reachable only from that test. The test excludes `QAppMetadata` by name and asserts the empty
  result, and `testdata/render-baseline.txt` carries no line for it. A build
  test pins that the issued `app_metadata` query names the configured metric,
  so a regression to the generic renderer is caught.
- The generic matcher path (`Selector.render`, `requestMatchers`,
  `RequestMatcherCost`) renders the `namespace` dimension on the literal key.
  The three renderers, and the chunk-budget cost D9 / D10 use, go through one
  helper that renders the same dimensions with the namespace key overridden by
  the configured label, so cost and issued query cannot disagree.

This is a deliberate, scoped exception to "no configurable metric name": that
rule protects the fixed name contract of standard exporters (KSM, kubelet,
Harvest, the service graph). The application-metadata metric has no standard
producer and therefore no name to fix.

Alternative — **make `Query` a struct or allow dynamic constants.** Widens
every table and guard test for a single query.

### D5 — Precedence is per pod; the controller chain stays as the fallback

For each loaded pod the metric's Application wins when the pod has a row whose
`app` is non-empty after D6; otherwise `resolveOwnerApplication` decides as
today. Both sources are computed independently and merged in one place
(`resolvePodApplications` grows a first-source map), so the controller chain's
degrade rules — including the `JobAnnotationsDegraded` gate — are untouched.

**Degrade is not purely subtractive here, deliberately.** The package rule
(harden-controller-annotation-legs D3) is that a degraded leg removes an
Application and never substitutes one. When the `/v1/graph` leg degrades
(D8), a pod the metric would have named `X` instead takes the controller
chain's `Y`. This is accepted because the result is exactly the body a
deployment with the metric unset produces — a value from a supported source,
not an invented one — and the alternative gate (drop every pod Application
whenever the leg degrades) would empty the compound hierarchy on a transient
error. The cost is that a pod whose two sources disagree can change group
between builds while the leg flaps. `/v1/storage-graph` never takes this path
(D9 fails closed), and the degrade is logged with the leg name like every
`fetchOptional` leg.

Alternative — **skip the controller chain whenever the metric is configured.**
Saves seven queries but strands every pod the metric does not cover; rejected
by the user in favour of fallback.

### D6 — Resolving rows within a window

The leg is `tlast_over_time`, so a sample's value is the series' last-seen
timestamp — the `kube_pod_container_info` precedent. Three independent indexes
are built from one vector, each skipping empty values BEFORE the pick so an
empty row never masks a populated sibling:

| index | key | value | pick |
|---|---|---|---|
| pod Application | `(cluster, namespace, pod)` | `app` | lexically smallest |
| namespace owner | `(cluster, namespace)` | `ns_owner` | latest seen; ties → lexically smallest |
| Application deployer | `(cluster, namespace, app)` | `user` | latest seen; ties → lexically smallest |

The pod Application deliberately does NOT take the latest value: the
lexically-smallest pick is the rule every tracking-id collision already
follows, and it is the only pick `ResolvePodApplication` can reproduce from
label sets (D11), so build and lookup agree by construction. "Latest seen" is
kept for the two values the lookup never returns, where a changed owner or
deployer should show its current value.

The deployer is keyed on the row's own `app` label, not on the pod's resolved
Application, so it does not depend on D5's merge. The `app` value is used
verbatim (it is not a tracking-id; no `:` split). A row with an empty `pod` is
still read for the namespace and deployer indexes. The readers never mutate a
sample (shared cached vectors).

"Latest seen" is reliable for near-now windows; for far-past windows VM may
return one variant per series set (design D-A4 of the container attribute),
and the lexical tie-break still yields a deterministic body.

### D7 — Group metadata travels on `Graph`, not on real nodes

`graph.Graph` gains `NamespaceOwners map[NamespaceRef]string` and
`ApplicationDeployers map[ApplicationRef]string`, keyed by cluster identity,
assigned by the builder after `graph.NewGraph` (as `ClusterIdentities` is).
Nil — every hand-built graph and the metric unset — emits nothing.
`cytoscape.Serialise` looks a group's key up when synthesising it and sets
`NodeData.NamespaceOwner` (`namespace_owner`) or `NodeData.DeployedBy`
(`deployed_by`), both `omitempty`. A group is still synthesised only from the
projected view, so the prune and filters decide which groups exist; the maps
only decorate them.

The JSON keys avoid `owner`, which is already the `{kind, name}` object on
pods; the values are not put in `labels`, which stay `{}` on every group.

Alternative — **carry the values on each pod and let cytoscape collect them.**
Duplicates a namespace fact onto every pod, invites disagreement between pods,
and gives a PVC-only namespace no owner.

### D8 — `/v1/graph`: one optional first-wave leg

Added to `topologyLegs` only when `Options.AppMetadata` is non-nil, as a
`fetchOptional` leg: a query error, an unserved family or an empty result each
leave every pod on the controller chain and every group without the two
fields, and never fail the build. It carries the request's selector-level
matchers and nothing else (no data-derived scope), so it runs beside the
existing legs and adds no wave: one more first-wave leg with the metric set,
whatever the Harvest second wave adds.

### D9 — `/v1/storage-graph`: a namespace-restricted read in every build

`readScopedAppMetadata` waits on the pod wave and the claim-info read, groups
the loaded pods' and claims' namespaces by their bucketed `cluster` label, and
issues one `RenderAppMetadataByNamespace` restriction per cluster: that
cluster's matcher (`cluster=~"unknown|"` for the absent-or-unknown bucket) plus
the sorted, de-duplicated namespaces it loaded, chunked under
`qosScopeBatchBytes` with the cluster matcher in each chunk's reserve
(data-derived: chunked, never widened, never rejected). The per-cluster vectors
are concatenated in cluster order. Pairing the namespace with its cluster keeps
a same-named namespace of every other cluster in the zone out of the read; the
reader joins rows on the cluster identity whose raw component is that cluster
label, so the restriction drops nothing the join would keep. It runs in parallel
with `readScopedControllers`, which has two stages, so it does not lengthen the
critical path. Any chunk error fails the build (the storage fail-closed rule;
no new exception beside `ALERTS`). The rows feed the same D6 indexes.

Restricting by namespace rather than by loaded pod is what makes D6's picks
agree with `/v1/graph`: for a given `(cluster, namespace)` both endpoints read
the identical row set, so the namespace owner and every deployer in it come out
the same, and a namespace present only through a claim still gets its owner.
The cost is reading the unloaded pods of touched namespaces, about one series
per pod.

Alternative — **restrict to the loaded pods.** Fewer rows, but the picks run
over a subset and can disagree with `/v1/graph` when a value changed in the
window, and a PVC-only namespace gets no owner.

### D10 — `application=` roots also seed from the metric

`readScopedApplications` issues `RenderAppMetadataByApp` over the root values
in parallel with its stage 1, and unions the returned `(cluster, namespace,
pod)` identities with stage 3's. The read is request-derived: its chunk count
joins the existing `maxApplicationRootChunks` check, so an oversized root set
is 400 `invalid_scope` before any query. Errors fail closed. The seed stays a
candidate generator: whether a pod is a root is decided by the projection over
the pod's D5-resolved Application, so a pod the metric names `X` but whose
row is beaten by a later `Y` is loaded and dropped, exactly like the annotation
recovery's mismatches.

### D11 — `ResolvePodApplication`

The pod-owner query still runs first. When it returns rows, those rows pin
`az` / `env` (unchanged). When `AppMetadata` is set and `appmeta` is served,
the lookup then issues one label query of family `appmeta` on `(cluster,
namespace, pod)` with that pinned zone, returns the D6 pick when it is
non-empty, and does not read the controller chain. An empty pick continues
down the chain unchanged.

When the pod-owner query returns no rows and the metric step is active, the
lookup still issues that query, unpinned: a dimension the request omitted
(`az` or `env`) carries no matcher, and a zone or environment the request did
set is still applied. The same single-combination rule then runs on the metric
rows. More than one `(az, env)` combination fails with `ErrAmbiguousPod` and
issues no further query. One combination with a non-empty `app` is the
result. An empty pick returns "" — there is no controller left to consult.

Because `Router.QueryLabels` errors on an unserved family, the lookup first
asks an optional upgrade of its `LabelQuerier`,
`FamilyServer{ Serves(promql.Family) bool }` (satisfied by `*promql.Router`).
A querier without it, or a family it does not serve, skips the metric step.
That default is the opposite of `appMetadataServed` on the graph build, which
treats a querier without `FamilyServer` as served. Any metric query error
fails the call (the capability's existing rule). The request gains
`AppMetadata *AppMetadataSource`; nil skips the step.

The query filters `cluster`, the configured `namespace` label and the
configured `pod` label through `LabelQuery.Filters` (arbitrary label names
are already accepted there). It takes the lexically smallest non-empty `app`
among the returned label sets — the same pick D6 makes in the build, verbatim,
with no `:` split — so the two agree without exception.

## Risks / Trade-offs

- [A pod's Application can move when the metric is turned on — it reparents
  in the compound tree, and a PVC inheriting from it may change] → Only with
  the metric configured; the metric is the operator's declared source of
  truth. Documented in `docs/upstream-metrics.md`.
- [`user` changes create new series; long windows read more rows] → The
  `/v1/graph` leg is `fetchOptional`, so an upstream series-limit rejection
  degrades to the controller chain. The storage read is namespace-bounded.
- [Metric labelled with different zone keys than KSM] → Precondition
  documented; the rows bucket under the wrong identity and simply do not join
  (subtractive), and `selector_family_empty` does not fire for it (optional
  family) — the startup docs call this out.
- [A pod whose `app` changed in the window keeps the lexically smaller value,
  not the newer one (D6)] → Rare (the metric's Application of a running pod
  rarely changes); the price of build / lookup agreement.
- [Degraded `/v1/graph` leg substitutes the chain's Application (D5)] →
  Equals the metric-unset body; logged; never on the storage endpoint.
- [Exception to "no configurable metric name" is read as precedent] → D4
  states the boundary: only a metric with no standard producer, behind a
  logical `Query` constant.

## Migration Plan

Additive and off by default. Rollout: deploy, then set
`KSG_APP_METADATA_METRIC` (and `KSG_APP_METADATA_LABELS` if the label names
differ from the defaults); with a backends file, add `appmeta` to the families
of the backend(s) holding the metric. Rollback: unset the variable — no state
to migrate.

## Open Questions

- Default label names in D1 (`argocd_app`, `user`, `owner`) are placeholders
  until the exporter's actual names are confirmed; changing them alters only
  defaults, not the design.
