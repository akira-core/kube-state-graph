## Context

See proposal.md — Why. Six properties of the current code shape the design.

**Application is a forward-resolved, controller-borne attribute.** No pod series
carries it. `resolvePodApplications` (`pkg/build/topology.go`) joins a pod's
already-resolved controller owner (`kube_pod_owner`, ReplicaSet collapsed to
its Deployment through `kube_replicaset_owner`) to one annotation family per
kind (`controllerAnnotationFamilies`), takes the segment of the tracking-id
before the first `:` (`argoAppName`), picks the lexically-smallest raw
tracking-id on collision, and — for a Job with no annotation of its own —
hops to the owning CronJob through `kube_job_owner`. PVCs carry their own
(`kube_persistentvolumeclaim_annotations`) or inherit the lexically-smallest
Application of their mounting pods (`pvcInheritedApps`). `PodNode` /
`PVCNode.Application()` is what the storage projection can read.

**The storage build never loads a pod nobody named.** Under `storagePlan`
(`pkg/build/topologyplan.go`) `kube_pod_info` / `kube_pod_owner` are read BY
REFERENCE (`readScopedPods`, `pkg/build/podscope.go`): the scope is the
claim-binding pods plus `plan.podRoots`, the wave waits on `bindingsDone`
alone, and nodes / controllers are scoped off what that wave loaded. A pod
that mounts nothing and is not a `pod=` root is never read, so it cannot be a
root of anything — which is why an Application root cannot be a
projection-only filter (the user's answer: "the root is the starting point;
there are no loaded pods yet").

**The by-reference machinery is generic and keyed per family.**
`issueScopedFamilies` (`pkg/build/scopedread.go`) issues N families, each on
its own scope, chunked under `opts.qosScopeBatchBytes()`, bounded by
`scopeConcurrency`, merged in chunk order, with a per-family `legMode` (required
/ optional / optionalTracking) and `markScopeIssued` for the tally. It renders
through `promql.RenderScoped`, which knows ONE label per family
(`scopedLabel`: `pod`, `node`, `replicaset`, `job_name`, …) and composes
`{<fixed>,<request>,<scope>}`. The recovery restricts the same families on
DIFFERENT labels — the tracking-id, `owner_name` — so it needs renderers of its
own but can reuse the chunker, the concurrency bound, the merge and the error
classes.

**Two request-derived scopes already exist, and both are bounded.** The pod
scope adds `pod=` names (widening only); the volume-label restriction
(`pkg/build/volumelabelscope.go`) is the one request-derived NARROWING, capped at
`maxRootedVolumeLabelChunks = scopeConcurrency` and falling back to the
unrestricted read past it, because a repeatable parameter's count is never
bounded by the parser. The application recovery's first stage is the same
shape: a scope the client can inflate.

**The projection resolves roots by name against the built graph.**
`resolveStorageRoots` (`pkg/graph/project_storage.go`) walks `g.NodesByID` once,
building a storage-side id set, a workload-side id set and the `node=` hits;
`ProjectStorage` keeps a flow unit iff it intersects every requested side and
passes the cluster / namespace filters, then materialises every resolved root
id through `admitRoot`. Workload ids are BOTH the retention key and the
materialisation set — one set, two uses — which a PVC-matching root must not
join, since a PVC is never materialised on its own.

**The namespace derivation is a parser rule keyed on root kinds.**
`deriveStorageNamespaces` (`pkg/kubegraph/parse.go`) derives `namespace` from
pod roots only when `!RequestedStorage() && len(Pods) > 0`; a root kind that
spans namespaces has to suppress it.

## Goals / Non-Goals

**Goals:**

- `application=` behaves as a first-class workload root: pods carrying it are
  loaded whether or not they mount a claim, materialised like `pod=` roots, and
  paths are retained by pod-or-claim Application.
- The recovery is a superset generator only: no code path lets a recovered
  name decide a root, resolve an attribute or bypass the forward resolver.
- Requests without the root are byte-identical in queries and body; the
  `/v1/graph` fan-out and `render-baseline.txt` are untouched.
- Every recovery family keeps its existing error class, chunking, chunk-order
  merge, bare-name issue and tally semantics.
- The request-derived first stage is bounded exactly as the volume-label
  restriction is, with the same fallback direction.

**Non-Goals:**

- Materialising PVCs (or their mounting pods) that carry a root Application
  but lie on no complete path. A PVC is never a root today; an Application
  root keeps that rule and uses the claim's Application for retention only.
- Recovering pods of controllers kube-state-metrics has no annotation family
  for (Rollout, CloneSet, ReplicationController, static pods). They resolve no
  Application in the forward direction either, so they could never be roots.
- Enumerating Application names (a label-values concern of the front end and
  `Router.QueryLabels`, not of this endpoint).
- Adding `application=` to `/v1/graph`, or any new node / edge type, `labels`
  key, flag, self-metric or label.
- Avoiding the second read of the Application's own annotation rows (D4).

## Decisions

### D1: The recovery generates candidates; the projection judges

The reverse walk (tracking-id → controllers → ReplicaSets / Jobs → pods) can
over-admit: a controller carrying two tracking-ids in the window matches stage
1 on either, while the forward resolver picks the lexically-smallest; an
annotated Job under a differently-annotated CronJob matches on the CronJob's
name, while the forward resolver keeps the Job's own. Rather than re-implement
the forward rules in reverse (and keep two implementations agreeing), the
recovery produces pod NAMES only. Those names join the pod scope like `pod=`
roots, the pods are read by the existing by-reference waves, `Application()` is
resolved by the existing resolver, and `resolveStorageRoots` matches the root
value against `Application()`. An over-admitted pod is loaded, resolves a
different Application, is neither a root nor on a retained unit, and is
dropped — the same fate as a cross-namespace name-collision pod today.

This is what makes the feature output-preserving in the only sense that
matters: the body is a pure function of the forward resolution. The recovery
can only lose a root by under-admitting, and it cannot under-admit: every
forward path (Deployment ← RS ← pod, StatefulSet ← pod, DaemonSet ← pod, bare
RS ← pod, Job ← pod, CronJob ← Job ← pod) is walked in reverse over the same
families with the same fixed selectors, and stage 3 filters
`owner_is_controller="true"` exactly as `resolvePodOwners` does.

*Alternative rejected:* resolving the Application in the recovery and marking
the pods as roots directly — two resolvers to keep in sync, and a wrong root
on the first divergence.

### D2: A fourth by-reference wave, three stages, launched with the first wave

`readScopedApplications(ctx, callerCtx, q, window, end, opts, sel, roots, v,
scopeMu) ([]string, error)` in a new `pkg/build/appscope.go`:

1. **Stage 1** — `issueScopedFamilies`-style fan-out over the six annotation
   families, each with `scope = roots` and a new `render` hook selecting
   `promql.RenderTrackingIDScoped` (D3). Modes mirror the forward wave:
   Deployment / StatefulSet / DaemonSet / CronJob `legRequired`;
   ReplicaSet / Job `legOptional` (NOT `legOptionalTracking` — D5).
2. **Stage 2** — waits on stage 1's return: `kube_replicaset_owner` with
   `promql.RenderOwnerScoped(q, …, "Deployment", deployments)`,
   `kube_job_owner` with `RenderOwnerScoped(q, …, "CronJob", cronjobs)`; both
   `legRequired`; an empty name set issues nothing.
3. **Stage 3** — waits on stage 2: one `kube_pod_owner` family entry per kind
   with a non-empty set (`ReplicaSet`, `Job`, `StatefulSet`, `DaemonSet`,
   `Deployment`, `CronJob`), each `RenderOwnerScoped(QPodOwner, …, kind,
   names)`, `legRequired`.

The wave's vectors are LOCAL (D4); the function returns the sorted,
de-duplicated pod names. `readTopology` launches it under
`signalWhenDone(…, appDone)` when `len(plan.applicationRoots) > 0`, otherwise
closes `appDone` immediately (the "prerequisite this plan never issues" rule
already in `readTopology`). `readScopedPods` gains `appDone` and
`pvcAnnotationsDone` parameters plus a slot for the recovered names, waits on
`bindingsDone` AND `appDone` AND (under an application root)
`pvcAnnotationsDone`, then computes the scope through D10's `podScopeUnderApp`
instead of `podScope`. `podsDone` semantics are unchanged, so the node and
controller waves need no edit.

The recovery starts at t=0 (request-derived), so its three round trips overlap
the first wave's Harvest and claim legs; the pod wave, which today starts when
`kube_pod_spec_volumes_persistentvolumeclaims_info` lands, starts at
max(bindings, recovery). Expected added latency per rooted request: two
upstream round trips on the critical path.

*Alternative rejected:* folding stage 3's `kube_pod_owner` rows into `v.PodOwner`
to skip re-reading them by pod name — it would make the pod wave's vector a
merge of two restrictions with different chunk orders and break "the vector is
a pure function of the scope".

### D3: Two renderers, same composition rule as `RenderScoped`

`pkg/promql/appscope.go`:

- `RenderTrackingIDScoped(q, window, keys, sel, apps) (string, bool)` — valid
  for the six annotation families only; renders
  `last_over_time(<q>{<fixed>,<request>,annotation_argocd_argoproj_io_tracking_id=~"(?:a|b)(?::.*)?"}[w])`.
  Each value is `escapeLiteral(regexp.QuoteMeta(v))`; the group is followed by
  an OPTIONAL `:`-anything suffix because `argoAppName` returns a value with
  no `:` verbatim, and the whole is anchored by PromQL's `=~`. The fixed
  `!=""` is redundant with the restriction but kept: the rule is "composed
  with, never replacing", and `TestRenderScoped_IsRenderPlusOneMatcher` is the
  template for pinning it.
- `RenderOwnerScoped(q, window, keys, sel, kind, names) (string, bool)` — valid
  for `kube_replicaset_owner`, `kube_job_owner`, `kube_pod_owner`; renders
  `{<fixed>,<request>,<kindMatcher>,owner_name=~"…"}` where `kindMatcher` is
  `owner_kind="<kind>"` for `kube_replicaset_owner`, `owner_is_controller="true",owner_kind="<kind>"`
  for `kube_pod_owner`, and NOTHING for `kube_job_owner` (its fixed selector
  already pins `owner_kind="CronJob",owner_is_controller="true"`; a kind other
  than `CronJob` is `ok=false`).

Both take the request `Selector` and render `queryDims[q]` exactly as
`RenderScoped` does, so `?namespace=` / `?cluster=` narrow the recovery.
`queryDims`, `fixedSelector`, `scopedLabel` and `render-baseline.txt` are
unchanged; the new renderers are pinned by their own tests plus a "is
`Render` plus N matchers" property test.

Chunking: `ChunkScope` over the values; for the tracking-id form the per-value
cost is the escaped value plus `|`, and the fixed wrapper
(`(?:` … `)(?::.*)?`) is charged once per chunk by subtracting its rendered
length from the budget (`MatcherCost`-style), so the budget bounds what is
sent.

*Alternative rejected:* reading the six families with only the fixed selector
and filtering in Go — that is exactly the unrestricted shape the storage plan
exists to avoid for `kube_job_annotations` / `kube_replicaset_annotations`; it
survives only as the D6 fallback.

### D4: Recovery vectors are wave-local; only a tally counter is shared

The forward controller wave writes `*fam.dst = merged` for the same families.
If the recovery wrote into `v.DeploymentAnnotations` etc., the later wave would
overwrite it or the merge order would depend on which wave finished first.
The recovery therefore parses its own local vectors into name sets and
discards them. Cost: the Application's own controllers' annotation rows are
read twice (once by tracking-id, once by name) — a handful of series per
request, accepted.

`tallySeries` needs the counts: `topologyVectors` gains `ExtraSeriesCount
map[promql.Query]int`, written under `scopeMu` by a new `addExtraSeries`, and
`tallySeries` adds it into the family's entry (creating the entry when the
by-reference wave did not issue the family). `largestLeg` is unchanged and
reads the summed entry.

### D5: Error classes mirror the forward wave; stage-1 degrades never set `JobAnnotationsDegraded`

Required / optional per family is copied from `readScopedControllers` (the
class is a property of the family, not of the key it is restricted on):
`kube_replicaset_annotations` and `kube_job_annotations` degrade,
everything else fails the build. A degraded stage-1 `kube_job_annotations`
loses the names of directly-annotated Jobs — subtractive, and complete: those
pods, if claimless, are never loaded, and if mounting, are loaded through their
binding and resolved by the forward read. Setting `JobAnnotationsDegraded`
would suppress the CronJob hop for EVERY Job-owned pod in the build, a strictly
larger loss than the recovery's failure justifies, and the flag's contract
("the by-reference read of this family could not establish that a loaded Job
has no annotation") does not apply — the forward read of the family is a
separate wave with its own chunks.

### D6: Stage 1 is bounded per family, or read unrestricted and filtered

`maxApplicationRootChunks = scopeConcurrency`, mirroring
`maxRootedVolumeLabelChunks`. When `ChunkScope(roots, budget)` for a family
exceeds it, that family is issued ONCE as `promql.Render(q, window, keys, sel)`
— the `/v1/graph` shape, fixed selector plus request matchers — and the reader
keeps rows with `argoAppName(tracking-id) ∈ roots`. Logged once per build at
Warn (`application_root_restriction_unbounded`, with the family and the chunk
count). Stages 2 and 3 are data-derived and chunk freely, exactly like the
controller wave.

*Alternative rejected:* rejecting the request past N values (400
`invalid_scope`). The volume-label precedent chose "degrade the read, not the
request", and a bound on repeat count would be the first parser rule of its
kind.

### D7: Projection — pods materialise, claims retain

`StorageRoots` gains `Applications map[string]struct{}`; `RequestedWorkload()`
includes it. `resolveStorageRoots` returns one more set, `claimHits`: for
`NodeTypePod`, `Application() ∈ Applications` ⇒ `workload[id]` (retention AND
materialisation, exactly like a `pod=` hit); for `NodeTypePVC`,
`Application() ∈ Applications` ⇒ `claimHits[id]` (retention only).
`ProjectStorage` sets `workloadExclusive = len(Pods) > 0 || len(Applications) > 0`
and keeps a unit iff `!workloadExclusive || u.intersects(workload) ||
u.intersects(claimHits)`. `admitRoot` runs over `workload` only, so a
matching PVC is drawn only on a retained path. The default arm's comment
("service / external / PVC are never storage or workload roots") is updated to
say PVCs are retention hits only.

Materialised pods honour cluster / namespace through `admitRoot(…, workload=true)`
as today.

### D8: Parser and engine surface

`ParseStorageValues` validates `application` with `validateSelectorValues`
(same 253-byte / UTF-8 / control-character rule) and passes `v["application"]`
to `NewStorageScope`, which gains an `applications []string` parameter and
fills `Roots.Applications` through `stringSet` (empties dropped, so a bare
`?application=` is a no-op). `deriveStorageNamespaces` returns `explicit`
when `len(scope.Roots.Applications) > 0`.

The signature move is a compile-time break for Go embedders; `StorageRoots` is
an exported struct they can also fill directly. Documented in `docs/BREAKING.md`
under the in-process-embedder heading. *Alternative rejected:* leaving
`NewStorageScope` alone and having the parser poke the field — it would need
`stringSet` exported and would let the two constructors drift.

### D9: `topologyPlan` carries the roots; the volume-label restriction ignores them

`storagePlan` fills `applicationRoots = sortedNames(keys(roots.Applications))`.
`restrictsVolumeLabels` is unchanged: an application root is workload-side,
the projection ANDs it with the storage roots, so every path the restriction
keeps is one the root can still retain — the same argument the spec already
makes for `pod=`.

### D10: Under an application root, only related binding pods enter the scope

Without narrowing, `?application=checkout` would still load every mounting pod
in the estate (the claim-binding half of `podScope`) and let the projection drop
all but a few — the whole-estate read the storage plan exists to avoid. A pure
`podScopeUnderApp(bindings, pvcAnnotations, recovered, podRoots, apps)` in
`appscope.go` replaces it when `len(plan.applicationRoots) > 0`:

- **keep set** K = recovered ∪ podRoots (pod names);
- **related claims** C = claims `(cluster, ns, claim)` with a
  `kube_persistentvolumeclaim_annotations` row whose `argoAppName(tracking-id)
  ∈ apps` (any row — a superset of the forward pick), ∪ claims of any binding
  row whose `pod ∈ K` (same namespace);
- **scope** = K ∪ { `pod` of every binding row whose claim ∈ C }.

Why every mounter, not just the recovered one: `flowUnit.n` is the mounter
count in the BUILT graph and drives the `1/n` split and `attribution="split"`;
`pvcInheritedApps` is a min over the mounters' Applications. Both must see the
same mounter set they see today, so a related claim pulls in all its binders.

Why this is output-preserving: a pod is materialised only when its forward
Application is a root value ⇒ it is in `recovered` (D1's superset) ⇒ in K. A
unit is retained only through a pod hit (pod ∈ K) or a claim hit — own
annotation (claim ∈ C by the first rule) or inherited (from a mounter whose
Application is a root value ⇒ that mounter ∈ recovered ⇒ claim ∈ C by the
second rule). Every pod on a retained unit, and every co-mounter its weight
depends on, is therefore in the scope. `pod=` roots keep their co-mounters for
the same `n` reason, exactly as today's unrestricted binding read gave them.

The wave gains one prerequisite: `kube_persistentvolumeclaim_annotations`
(already an unrestricted first-wave leg) gets a done-signal in `readTopology`'s
`signals` map, waited on only under an application root. Without an
application root `podScope` runs unchanged, so today's builds issue the same
queries.

*Alternative rejected:* deferring the narrowing to a later change. The user
chose to fold it in: without it the feature's cost on a large estate is the
cost of the whole-estate pod read, which is the cost the storage plan was built
to remove.

### D11: Fan-out pins

`TestBuildStorage_FanOutLegCount` gains rows (first wave 18, then per stage):

| Request | Recovery queries | Notes |
|---|---|---|
| `application=x`, nothing matches | 6 | stage 1 only; pods / nodes / controllers as the bindings dictate |
| one Deployment-managed claimless pod | 6 + 1 + 1 | `kube_replicaset_owner`, `kube_pod_owner{ReplicaSet}`; then pods 2, nodes 4, controllers 2 + 1 |
| one CronJob-managed claimless pod | 6 + 1 + 1 | `kube_job_owner`, `kube_pod_owner{Job}`; then pods 2, nodes 4, controllers 2 + 1 |
| every kind present | 6 + 2 + 6 | then the existing 38-leg maximum on the forward side |

Rows without `application=` keep today's numbers (18 / 22 / 25 / 27 / 32 / 38).

## Risks / Trade-offs

- [Over-admission loads pods the body drops] → bounded by the Application's
  own controller set; the projection drops them; pinned by the over-admission
  scenario. No wrong root is possible (D1).
- [The Application's annotation rows are read twice] → a handful of series;
  the alternative (D4) breaks the pod wave's determinism.
- [A `(?:a|b)(?::.*)?` regex on a high-cardinality label] → every branch has a
  literal prefix, which VictoriaMetrics turns into a prefix index scan; the
  six families are additionally pinned to `!=""` and the request matchers. The
  Job / RS families still accumulate with history, which is why they stay
  optional.
- [Thousands of `application=` values] → D6 fallback; the fallback read is the
  `/v1/graph` shape, so it cannot be worse than that endpoint.
- [A recovered pod name collides across namespaces] → the same harmless
  superset the pod scope already tolerates; the projection keys on
  `(cluster, namespace, name)`.
- [Two more round trips on the critical path] → they overlap the Harvest legs;
  a request without the root pays nothing.
- [Dead pods in the window] → `kube_pod_owner` over `[w]` names pods that
  existed at any instant; they enter the scope, `kube_pod_info` loads them,
  and they are drawn like any pod alive in the window — consistent with every
  other family's window semantics.
- [PVC-only Application hits draw nothing when the claim joins no filer] → by
  design (Non-Goals); a claim that joins draws its whole path.
- [Narrowing (D10) loads fewer pods, so the cluster-identity `adopt` step sees
  fewer entities] → a cluster established only by unrelated pods drops out of
  the identity table; rows keyed to it belong to pods and claims that are not
  loaded or are unmounted-and-dropped, so the body is unchanged. The same
  class of effect already exists under the pod-only namespace derivation;
  pinned by the parity test, which builds the narrowed and the whole-binding
  read against one fixture.

## Migration Plan

Additive on HTTP: deploy, then clients may send `application=`. Rollback is
not sending it — a request without the root is byte-identical in queries and
body. Go embedders update the `NewStorageScope` call (one added argument) or
fill `StorageRoots.Applications`. `make docs` regenerates the OpenAPI document;
`docs/BREAKING.md` records the embedder move.

## Open Questions

None that change the specs or tasks. The front end's root control (values
from the tracking-id label values, split at the first `:`) is a separate
change in `kube-state-graph-frontend`.
