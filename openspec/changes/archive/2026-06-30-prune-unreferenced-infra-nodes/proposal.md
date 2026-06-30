## Why

A default `GET /v1/graph` (and a `?cluster=` / `?edge_type=`-only) response currently
lists **every** K8s `node` and `storageclass` in upstream VictoriaMetrics — including
nodes that host no pod and StorageClasses that back no PVC. These orphan infrastructure
nodes clutter a graph whose purpose is the *workload* topology: an operator looking at
the pod/service graph does not want every empty / control-plane node and every unused
StorageClass dangling in the view.

The "keep an infra node only when an in-scope element references it" rule already exists
(`infraNodePassesFilters`, design.md D6) but is gated to `?namespace=` filtering only.
This change **generalises** it to every request shape, with one deliberate exception:
an explicit `?name=<node|storageclass>` still surfaces a specific empty node or unused
StorageClass on demand (so node health / IP and StorageClass attributes stay queryable).

## What Changes

> Generalises the existing **"Namespace-filter retention of cluster-scoped infra nodes"**
> requirement (introduced by `add-storageclass-and-argo-application-nodes`) from a
> namespace-only rule to an all-request rule. Archive that change first.

- **Projection (`pkg/graph/project.go`).** A `type="node"` (K8s node) or
  `type="storageclass"` node is admitted to a view **iff referenced by an in-scope
  element** — a pod scheduled on the node (`labels.node`) or a PVC backed by the
  StorageClass (`StorageClass()`) — on **every** request shape (default/no-filter,
  `?cluster=`, `?namespace=`). The default view now lists only the host nodes of pods
  that are in the graph, never an orphan/empty node or a PVC-less StorageClass.
- **`?name=` exception.** An explicit `?name=<node-or-sc-name>` admits the named infra
  node **even when referenced by nothing** (a podless / `NotReady` node, or an unused
  StorageClass, stays directly queryable). A name filter that does not name a given
  infra node drops it from `filterNodes`; if it is instead the host of a *named* pod (or
  backs a *named* PVC) it re-enters the view as that edge's re-added partner in
  `filterEdges`, exactly as before.
- The `hostNodes` / `referencedSC` reference sets are now built for every request (not
  only under a namespace filter).
- **No "unhealthy node always visible" exception.** A podless node with
  `ready_status` `NotReady` / `Unknown` is **also** hidden by default (queryable by
  name) — node-health of *empty* nodes is intentionally out of the default workload
  view (confirmed decision).

The full-topology `*Graph` is still built unchanged (the build loads every node and
StorageClass); the pruning is purely a **projection** concern, so the
"build-full-graph-then-project-per-request" model — and a future cache serving any
filter from one built graph — is unaffected.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `graph-api`: generalise the cluster-scoped infra-node retention requirement from
  namespace-only to all-request (default / cluster / namespace), add the explicit
  `?name=` admission exception, and record the consequence that a podless node's
  `ready_status` / `ipaddress` (and a PVC-less StorageClass's attributes) are absent
  from the default view.

## Impact

- **Code:** `pkg/graph/project.go` — `filterNodes` builds the reference sets
  unconditionally; `infraNodePassesFilters` admits an infra node iff referenced, with
  an explicit name-match exception (cluster filter applies first). No build / serialiser
  change.
- **Tests:** `pkg/graph/project_test.go` — generalise `TestProject_NamespaceFilter_DropsPodlessK8sNode`
  and add `TestProject_NoFilter_DropsUnreferencedInfraNodes` +
  `TestProject_NameFilter_MatchesUnreferencedInfraNode`. `internal/integration` —
  `TestNodeReadyStatusAttribute` fetches its podless probe nodes via `?name=` (they no
  longer appear in the default view).
- **Docs:** `CLAUDE.md` — the D6 infra-node rule is now all-request with the `?name=`
  exception, plus the explicit consequence note.
- **Contract / compatibility:** **behaviour change, not a wire-schema break.** The
  `{apiVersion, clusters, elements}` shape and determinism are unchanged; a default
  response simply carries fewer `node` / `storageclass` entries (only referenced ones).
  A consumer that relied on the default graph listing every node must switch to
  `?name=` for unreferenced ones.
- **Dependency:** logically supersedes the `add-storageclass-and-argo-application-nodes`
  "Namespace-filter retention" requirement — archive that change before archiving this
  one so the MODIFIED delta applies cleanly.
- **No new dependency, no new HTTP route, no PromQL change** (pruning is in-memory at
  projection — the "no filters pushed to PromQL" rule is preserved; the build still
  loads every node).
