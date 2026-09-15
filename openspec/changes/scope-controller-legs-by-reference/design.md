## Context

See proposal.md — Why. Six properties of the current code shape the design.

**The storage plan already has a by-reference wave, gated by channels inside one
errgroup.** `readTopology` (`pkg/build/topology.go`) launches every first-wave
leg from `topologyLegs`, closes a done-channel per prerequisite family
(`signalWhenDone`), and runs `readScopedQoS` (waits on `kube_persistentvolumeclaim_info`
+ `volume_labels`) and `readScopedPods` (waits on the claim-binding family) as
goroutines in the same group. `topologyPlan.issuesFirstWave` is what keeps a
by-reference family out of the first wave; `tallySeries` reports a second-wave
family only when its wave set an `Issued` flag.

**The pod wave signals nothing.** `readScopedPods` returns into the group and no
downstream wave exists, so there is no `podsDone` channel to gate a third wave
on. The QoS wave shows the shape: a channel closed on every return path.

**`RenderScoped` omits the fixed selector.** Its doc says "fixed selector, then
request matchers, then scope", but the body renders only the request matchers
plus the alternation — correct for the two pod families, which have no fixed
selector, wrong for `kube_job_owner`, `kube_node_status_addresses`,
`kube_node_status_condition` and the six annotation families, whose fixed
selectors live only inside `Render`'s `switch`.

**Every reader consults the by-reference families by a key it already holds.**
`resolvePodOwners` reads `kube_replicaset_owner` only at `(cluster, ns, <RS name
of a loaded pod>)`; `resolvePodApplications` reads the annotation index at the
loaded pod's resolved `(kind, name)`, then `kube_job_owner` at the Job name, then
the CronJob family at the recovered CronJob name; the node readers key on
`(cluster, node)`, and a pod's `labels.node` is set from `kube_pod_info`'s own
`node` label, not from `kube_node_info`. So a superset of those names is
output-preserving by construction.

**Two names are only known one hop later.** A Deployment is reached through the
ReplicaSet collapse (`kube_replicaset_owner`), a CronJob through the Job hop
(`kube_job_owner`). Pods are never owned directly by either in practice, but the
reader surfaces any `owner_kind` verbatim, so the direct case has to stay
covered.

**The upstream limit is per query, per day, before aggregation.**
VictoriaMetrics' `-search.maxUniqueTimeseries` counts the series a query's
matchers select from the per-day inverted index. A range read (`[w]`) over any
part of a day selects every series that had a sample that day, so shrinking the
window does not move the count, and only a narrower matcher does. The
`ChunkScope` budget (`--netapp-qos-scope-batch-bytes`, default 8192) is what
keeps each narrowed query under `-search.maxQueryLen`.

## Goals / Non-Goals

**Goals:**

- No storage build issues an unrestricted read of a family whose cardinality
  accumulates with history (`kube_replicaset_owner`, `kube_replicaset_annotations`,
  `kube_job_owner`, `kube_job_annotations`).
- Every family the storage body draws BY REFERENCE is read by reference:
  pods, Kubernetes nodes, controllers. What stays unrestricted is exactly what
  the body draws whole or what a scope is computed from.
- Every storage-graph body is byte-identical to today's.
- Each restricted family keeps its existing error class; the empty-scope,
  chunking, chunk-order and tally rules of the pod wave apply unchanged.
- `queryDims`, `render-baseline.txt`, the request surface, the flag set and
  the `/v1/graph` fan-out are unchanged.

**Non-Goals:**

- Scoping the `/v1/graph` read. Its pod set IS the estate, so a controller scope
  derived from it is the estate's controller set — for Job history that is the
  problem's own size split into a thousand chunks. That endpoint's answer is
  the error class of `kube_job_owner`, a separate change.
- Scoping the claim families. `kube_persistentvolumeclaim_info`,
  `kube_persistentvolumeclaim_annotations` and the kubelet pair are one series
  per claim the binding family already names; the binding family is the scope
  root and cannot be narrowed, so the claim families cannot be made smaller
  than it (D8).
- Scoping `volume_labels` or any Harvest family. Roots must be drawable with no
  claim, and SVMs are named by `volume_labels` alone (D8).
- Changing any leg's error class, adding a knob, a new metric, or a request
  parameter.

## Decisions

### D1: Twelve more families become scopeable, on their own identity labels

`promql.scopedLabel` (`pkg/promql/scope.go`) grows from the two pod entries to:

| Query | Scope label |
|---|---|
| `kube_pod_info`, `kube_pod_owner` | `pod` |
| `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels`, `kube_node_status_condition` | `node` |
| `kube_replicaset_owner`, `kube_replicaset_annotations` | `replicaset` |
| `kube_job_owner`, `kube_job_annotations` | `job_name` |
| `kube_deployment_annotations` | `deployment` |
| `kube_statefulset_annotations` | `statefulset` |
| `kube_daemonset_annotations` | `daemonset` |
| `kube_cronjob_annotations` | `cronjob` |

Three exported lists name the waves — `PodScopedQueries` (unchanged),
`NodeScopedQueries`, `ControllerScopedQueries` — and `ReferenceScopedQueries`
is their union, the set `issuesFirstWave` excludes under a by-reference plan. A
family can only be scoped on the label its reader joins on; the table stays the
gate (`RenderScoped` returns `ok=false` for anything else).

**Alternative rejected — scope the annotation families on `uid`.** The reader
keys every controller index by name, and `kube_pod_owner` carries `owner_name`,
not the owner's uid. Names are what the readers join on; scoping on anything
else would need a second index nobody consults.

### D2: `RenderScoped` renders the family's fixed selector from a shared table

The per-query fixed selectors move out of `Render`'s `switch` arms into one
`fixedSelector map[Query]string` (`type=~"ExternalIP|InternalIP"`,
`condition="Ready"`, `jobOwnerCronJobSelector`, `argoTrackingIDPresentSelector`
× 6, the service-graph sentinel / link matchers). `Render` reads the table
through its existing `braces(fixed)` helper; `RenderScoped` renders
`{<fixed>,<request matchers>,<scope>}` in that order, omitting an empty part.
`TestRender_EmptySelectorMatchesBaseline` pins that `render-baseline.txt` does
not move, and a new `TestRenderScoped_IsRenderPlusOneMatcher` pins, for EVERY
scopeable query, that the scoped string is `Render`'s output with exactly one
matcher appended inside the braces.

**Why a table rather than a second switch:** a fixed selector that exists in
one place cannot drift between the unrestricted and the restricted rendering
of the same family — the failure mode this design must not introduce is a
scoped `kube_job_owner` read that silently drops the CronJob selector and
fetches every Job of the named names.

### D3: One generic scoped-family reader, three waves

`readScopedPods`'s chunk → issue → merge loop is extracted into
`issueScoped(ctx, callerCtx, q, window, end, opts, sel, target scopedTarget,
scope []string, mode legMode) error` in `pkg/build/scopedread.go`, where
`legMode` is `{required | optional | optionalTracking(degraded *bool)}` and
mirrors `fetch` / `fetchOptional` / `fetchOptionalTracking`:

- `required`: the first chunk error is returned (fail closed — the pod wave's
  rule, now also the node wave's and the required controller legs').
- `optional`: a failed chunk logs `optional topology query failed`, contributes
  nothing, and the merge continues (the QoS wave's rule, now
  `kube_replicaset_annotations`').
- `optionalTracking`: as `optional`, and sets the flag
  (`kube_job_annotations` → `JobAnnotationsDegraded`, suppressing the Job →
  CronJob hop build-wide, exactly as the unrestricted degrade does).

Caller cancellation fails every mode (`optionalQueryFatal`). Chunks are issued
under `scopeConcurrency`; results are written into pre-sized slots and merged
in chunk-index order; an empty scope returns without issuing and without
setting the family's issued flag. `readScopedPods` becomes a caller of
`issueScoped`; `readScopedQoS` stays as it is (its renderer is the Harvest one).

The three storage waves:

1. **Pods** — `readScopedPods`, unchanged, but wrapped by `signalWhenDone` so it
   closes `podsDone` on every return path.
2. **Nodes** — `readScopedNodes` (`pkg/build/nodescope.go`): waits on
   `podsDone`; scope = sorted union of `v.Pod`'s `node` labels (empty skipped)
   and `plan.nodeRoots`; issues the four node families as `required`.
3. **Controllers** — `readScopedControllers` (`pkg/build/controllerscope.go`):
   waits on `podsDone`; **stage A** computes `controllerScope(v.PodOwner)` — a
   pure `map[kind][]string` from rows with `owner_is_controller="true"` and a
   non-empty `owner_name`, each list sorted and de-duplicated — and issues
   `kube_replicaset_owner` (required) + `kube_replicaset_annotations`
   (optional) on the ReplicaSet names, `kube_job_owner` (required) +
   `kube_job_annotations` (optionalTracking) on the Job names,
   `kube_statefulset_annotations` and `kube_daemonset_annotations` (required)
   on their names, all concurrently under one bounded sub-group; **stage B**
   runs after stage A's sub-group returns, in the same goroutine, and issues
   `kube_deployment_annotations` on `deploymentScope(direct, v.ReplicaSetOwner)`
   (direct `Deployment` owner names ∪ `owner_name` of every ReplicaSet row with
   `owner_kind="Deployment"` — the same filter `resolvePodOwners` applies) and
   `kube_cronjob_annotations` on `cronJobScope(direct, v.JobOwner)` (direct
   `CronJob` names ∪ every `kube_job_owner` row's `owner_name`; the fixed
   selector already restricts the rows to CronJob controllers, and the scope is
   a superset either way).

Waves 2 and 3 run concurrently; nothing waits on either (the parse runs after
`g.Wait()`). Under `fullPlan` none of the three is launched and the twelve
families stay in the first wave — `TestReadTopology_FanOutLegCount` keeps
37 / 43.

**Alternative rejected — derive the Deployment name by stripping the
ReplicaSet's pod-template-hash suffix, to fold stage B into stage A.** The
suffix rule is a controller implementation detail, not an API contract, and a
mis-derived name is an UNDER-fetch: a pod silently loses its Application with
no signal. The authoritative hop costs one round-trip of a tiny query.

**Alternative rejected — one wave that issues every controller family on the
full owner-name set regardless of kind.** `kube_job_owner{job_name=~"<sts
names>"}` matches nothing but still scans the label's values; and it would
issue queries for kinds no pod is owned by, defeating the empty-scope rule that
makes the common StatefulSet-only estate issue exactly one controller query.

### D4: The plan carries node roots beside pod roots

`topologyPlan` gains `nodeRoots []string` (the sorted `StorageRoots.Nodes` set)
next to `podRoots`; `storagePlan(roots)` fills both, `fullPlan` neither. The
`scopePods` bit is renamed `byReference`: one bit, because pods, nodes and
controllers are by-reference together or not at all — a plan that scoped
controllers over an unrestricted pod read would derive a scope from the whole
estate (see Non-Goals). `issuesFirstWave` returns false for every
`ReferenceScopedQueries` member under a by-reference plan.

A `node=` root reaches the build for the same reason a `pod=` root does: a root
must be drawable when nothing flows through it, and the node families are now
issued only for names in the scope. `StorageRoots.Nodes` is one set serving
both tiers, so ONTAP controller names are in the node scope too; a name
`kube_node_info` does not know matches nothing, which is the existing "a root
NO series names is not drawn" rule.

### D5: Per-family issued flags replace the wave-level bools

`topologyVectors.QoSScopeIssued` / `PodScopeIssued` become one
`ScopeIssued map[promql.Query]bool`, written by the wave goroutine that owns a
family and read only after `g.Wait()`. Each wave writes disjoint keys (the QoS
six, the pod two, the node four, the controller eight), but disjoint keys do
NOT make concurrent map writes safe: unlike the vector slots, which are
distinct struct fields, the waves share one map. Every write therefore goes
through `markScopeIssued` under one `*sync.Mutex` that `readTopology` creates
and threads into each wave. The mutex is a local, not a `topologyVectors`
field: that struct is passed by value into the resolvers, and an embedded lock
would make each call a lock copy (`go vet` copylocks). Reads after `g.Wait()`
need no lock.
`tallySeries` reports a by-reference family iff its key is set; a stage-A kind
with no owners, or a stage-B set nothing populated, never sets it. The
`RawSeriesCount` doc comment gains the rule.

### D6: The storage fan-out is 18 first-wave legs plus a per-kind formula

First wave under `storagePlan`: 30 − 4 node − 8 controller = **18** (the claim
three, `volume_labels`, the two QoS ceilings, three `aggr_*`, six controller
performance / identity legs, `ALERTS`, the two kubelet families). Then, per
build:

- + 2 × pod chunks, when the pod scope is non-empty;
- + 4 × node chunks, when any loaded pod is scheduled or a `node=` root exists;
- + 2 × ReplicaSet chunks, + 2 × Job chunks, + 1 × StatefulSet chunks,
  + 1 × DaemonSet chunks — each only when that kind owns a loaded pod;
- + 1 × Deployment chunks when any ReplicaSet resolved to a Deployment or a pod
  is directly Deployment-owned; + 1 × CronJob chunks when any `kube_job_owner`
  row landed or a pod is directly CronJob-owned;
- + 6 × QoS chunks when any claim matched a FlexVol.

`TestBuildStorage_FanOutLegCount` pins, with one chunk each and no matched
volume: empty scope, no roots → **18**; `node=n9` root alone → **22**; one
StatefulSet-owned scheduled pod → **25**; one Deployment-owned pod (via
ReplicaSet) → **27**; one CronJob-owned pod (via Job) → **27**; a fixture with
every kind present (StatefulSet, DaemonSet, Deployment via ReplicaSet, bare
ReplicaSet, CronJob via Job, directly annotated Job) → **32**, and **38** with
one matched volume. The all-kinds numbers coincide with today's 32 / 38 by
arithmetic, not by design; the pin is the whole table.

### D7: Determinism and output preservation are pinned, not argued

Every scope is a sorted, de-duplicated slice; `ChunkScope` is a pure function
of it; every merge is in chunk order; stage B's scopes are computed from stage
A's merged vectors. So the issued queries and the parsed vectors are a pure
function of `(window, end, selector, roots, upstream data)`, and
`TestBuildStorage_PlanIsOutputPreserving` extends its fixture to every
controller kind (a bare ReplicaSet, a Job with its own annotation, a Job under
an annotated CronJob, a DaemonSet, a StatefulSet, a Deployment), an unscheduled
pod, a `node=` root on a pod-less node, and a same-named StatefulSet in another
namespace, asserting byte-equal bodies between the full read and the storage
plan (minus `containers`, as today).

### D8: What stays unrestricted, and why each does

| Family | Why unrestricted |
|---|---|
| `kube_pod_spec_volumes_persistentvolumeclaims_info` | The scope root: every other scope is computed from it; nothing precedes it |
| `kube_persistentvolumeclaim_info`, `kube_persistentvolumeclaim_annotations`, `kubelet_volume_stats_*` | One series per claim the binding family already names; a restriction to bound claims cannot select fewer than the root read did, and turns 4 queries into 4 × chunks for no cardinality gain |
| `volume_labels` | The only source of SVM names and of the storage-side inventory; a storage root must be drawable with no claim, and the Harvest join is a derived-token suffix match the query layer cannot express without the provisioner's prefix |
| `aggr_*`, `node_*`, `qos_policy_fixed_*` | Filer inventory: bounded by the number of controllers / aggregates / policy groups, and roots must be drawable |
| `ALERTS` | Already restricted to `alertstate="firing"` |

The criterion is stated so the next family added to the storage read is
classified rather than defaulted: **restrict when the body draws the family by
reference from an earlier wave; leave unrestricted when the family is a scope
root, must be drawn whole for roots, or cannot be selected below the root's
own cardinality.**

## Risks / Trade-offs

- [The storage critical path grows from two sequential hops to four
  (bindings → pods → stage A → stage B)] → each added hop is a handful of tiny
  queries; the QoS wave and the node wave overlap them; the alternative was an
  unrestricted read that the upstream rejects. Wall-clock is bounded by
  `--build-timeout` as before.
- [Name-scoped reads admit same-named objects from other namespaces / clusters
  inside the selector] → keyed separately, consulted by nobody; parity test
  pins body equality; the missing-`cluster` diagnostic tally may count the
  extra rows (logging only, the pod wave already accepted this).
- [A `kube_job_annotations` chunk failure suppresses the hop for the whole
  build, not just the chunk's Jobs] → conservative and spec-consistent; a
  per-chunk suppression would need the chunk membership to reach the parse,
  for an Application string on a degraded build.
- [Owner-heavy estates produce many controller chunks (5 000 mounting pods
  under distinct ReplicaSets ≈ 35 chunks × 2 families)] → each chunk is bounded
  and concurrent under `scopeConcurrency`; still a strict subset of today's
  unrestricted read; the budget is operator-tunable.
- [`fixedSelector` refactor touches every `Render` arm] → `render-baseline.txt`
  is the byte-level pin; `TestRenderScoped_IsRenderPlusOneMatcher` is the
  cross-check.
- [Component tests with strict mock expectations for the storage suite expect
  the twelve families in the first wave] → fixtures updated; the storage
  goldens are hand-built and do not move.
- [This change's storage-graph-api delta MODIFIES a requirement that exists
  only in `harden-topology-read-cardinality`'s delta] → sequence: sync or
  archive that change first; `openspec validate` reports the missing header
  as an archive-time refusal until then.

## Migration Plan

- Deploy with no configuration change; no persisted state, no schema, no
  request-surface change. Rollback is a revert.
- Operators watching `kube_state_graph_upstream_query_result_series{query}`
  see the twelve by-reference families drop to their scope size on storage
  builds; the `largest_leg` of a storage build becomes a claim or Harvest
  family.

## Open Questions

- Whether `/v1/graph` should adopt the controller wave gated on its
  unrestricted `kube_pod_owner` read for the live-count kinds only
  (StatefulSet, DaemonSet, Deployment), leaving the history-accumulating
  Job / ReplicaSet families to the error-class change. Deferrable: it changes
  neither this change's specs nor its tasks.
