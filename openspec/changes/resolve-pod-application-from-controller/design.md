## Context

See `proposal.md` — Why. The relevant current state:

- `resolvePodApplications(v.PodOwner)` in `pkg/build/topology.go` reads an
  `argocd_tracking_id` label off `kube_pod_owner`. Stock kube-state-metrics never
  emits it, so the map is always empty and no pod carries `data.application`.
- `resolvePodOwners(v.PodOwner, v.ReplicaSetOwner, mc)` already resolves each
  pod's controller and already collapses `ReplicaSet → Deployment`. The value this
  change needs to key on therefore **already exists** at the point of use — the
  work is a lookup, not a new traversal.
- `resolveApplications[K comparable](vec, label, keyOf)` is a generic that picks
  the lexically-smallest raw tracking-id per key, drops values whose leading
  segment is empty, and derives the Application in place. It already serves the
  service and PVC resolvers.
- `ReadTopology`'s errgroup has two fetch wrappers: `fetch` (a query error fails
  the build) for the KSM legs, `fetchOptional` (a query error logs and yields an
  empty vector) for the Harvest/kubelet legs. Both existing annotation families
  use `fetch`.
- `queryDims` in `pkg/promql/queries.go` maps every `Query` constant to the
  request-scoped dimensions it accepts; a test parses `queries.go` and fails on a
  constant with no entry.
- Every pod's controller lives in the pod's own `(cluster, namespace)` —
  Kubernetes `ownerReferences` are namespace-local — so the join this change adds
  never crosses a cluster or namespace boundary.

## Goals / Non-Goals

**Goals:**

- One resolution path for the pod Application, keyed on the controller, covering
  every pod controller kind kube-state-metrics can describe.
- Byte-identical output for a deployment that has not enabled the annotation
  allowlist (the empty-vector case, which is today's stock behaviour).
- Provable non-interference with `data.owner`: the new Job→CronJob hop must be
  structurally incapable of changing the owner attribute.

**Non-Goals (design-level, beyond the proposal's scope statement):**

- No change to `resolvePodOwners` or its `rsToDeployment` behaviour. The existing
  ReplicaSet skip stays exactly as it is, including its lack of an
  `owner_is_controller` filter.
- No new generic helper, no refactor of `resolveApplications`, no change to the
  service / PVC resolvers.
- No upstream aggregation or `label_replace` in PromQL. Every join stays in Go,
  consistent with every other topology join in this package.

## Decisions

### D1: One flat index keyed `(cluster, namespace, kind, name)`, not six per-kind maps

The six controller-annotation families collapse into a single map whose key
carries the owner kind. The lookup is then one map access against the `ownerRef`
the controller pick already produced, with the kind as *data* rather than as a
`switch` in the resolution path.

Because keys of different kinds are disjoint, the existing generic
`resolveApplications` can be called once per family with a `keyOf` that stamps a
constant kind, and the six results merged into one map. The merge is order-free
by construction (disjoint key spaces), so no cross-family tie-break rule is
needed and D6 determinism is inherited from the generic.

*Alternatives:* six maps plus a `switch owner.kind` at the lookup site — same
behaviour, more code, and each added controller kind touches two places. A single
per-kind interface with six implementations — over-engineered for a map lookup.

### D2: The Job→CronJob hop lives in the Application resolver, not in the owner resolver

`resolvePodApplications` gains the hop and takes three inputs: the resolved
owners, the controller-annotation index, and a `(cluster, namespace, job) →
cronjob` index. `resolvePodOwners` is not touched at all.

This is what makes "the hop does not change `data.owner`" a structural property
rather than a convention: the owner attribute is written from
`resolvePodOwners`' output, and that function never sees the CronJob index.

Mirroring the ReplicaSet skip inside `resolvePodOwners` would have been more
symmetric, but it would rewrite `owner={kind:"Job"}` to
`owner={kind:"CronJob"}` on every CronJob-managed pod — changing an attribute and
a `controller` compound group that consumers already read. That is a separate
breaking change, deliberately not bundled here.

*Ordering inside the resolver:* the Job's **own** annotation is tried first, and
the hop only runs on a miss. A Job that ArgoCD manages directly carries its own
tracking-id, and preferring it keeps the "nearest managed ancestor wins" rule
that the ReplicaSet→Deployment collapse already implies.

### D3: The new hop filters `owner_is_controller="true"`; `rsToDeployment` is left alone

The new `(cluster, namespace, job) → cronjob` index selects on
`owner_kind="CronJob"` **and** `owner_is_controller="true"`. The pre-existing
`rsToDeployment` filters only on `owner_kind="Deployment"`.

The asymmetry is deliberate: new code should honour the authoritative label, and
tightening `rsToDeployment` would change `data.owner` for any pod whose
ReplicaSet carries a non-controller Deployment owner reference — out of scope per
the Non-Goals.

### D4: Seven separate queries at bare metric names, using `fetch` (not `fetchOptional`)

Each family gets its own `Query` constant, `Render` case and `queryDims` entry,
and is issued as `last_over_time(<bare name>[w])` like every sibling.

- **Why not one union query** (`kube_deployment_annotations or
  kube_statefulset_annotations or …`)? The identity label differs per family, so
  a union needs upstream `label_replace` gymnastics; it also collapses the
  per-family `RawSeriesCount` telemetry, breaks the one-entry-per-`Query`
  `queryDims` contract that a test enforces, and breaks the spec's "exactly one
  PromQL query per family" scenario.
- **Why not a `__name__` regex** (`{__name__=~"kube_(deployment|…)_annotations"}`)?
  A regex on `__name__` is an index-wide scan in VictoriaMetrics, and the repo
  queries bare names by contract.
- **Why `fetch` and not `fetchOptional`?** The families being *absent* is the
  normal case, and an absent metric returns an **empty vector, not an error** —
  so fail-fast never triggers on the common path. The only trigger is a genuine
  upstream failure (timeout, 5xx), where failing the build matches how the
  sibling `kube_service_annotations` / `kube_persistentvolumeclaim_annotations`
  legs already behave. Using `fetchOptional` would silently diverge two
  annotation families from the other two.

Fan-out goes 30 → 37 legs; the pin in `pkg/build/netapp_test.go` moves with it.

### D5: All seven families are `[AECN]`

They carry `namespace`, so they take all four request-scoped dimensions. This is
not merely permissible, it is **required for correctness under a filter**: a
pod's controller is always in the pod's own `(cluster, namespace)`, so narrowing
both sides by the same `cluster` / `namespace` matcher keeps every join intact.
A family narrowed differently from the pods that reference it would drop
Applications inside the requested scope.

### D6: The `bucketCluster` special case disappears

`resolvePodApplications` currently uses the pure `bucketCluster` helper instead of
`mc.bucket` to avoid double-tallying `kube_pod_owner`'s missing-`cluster` samples,
which `resolvePodOwners` already counts for the same vector. After this change the
pod Application reads seven vectors that nothing else reads, so each one tallies
normally through `mc.bucket(promql.Q…, …)` and the documented diagnostic gap
("a tracking-id carried only on a non-controller row with a missing cluster label
is bucketed silently") goes away.

### D7: Unsupported owner kinds degrade, and the degrade is per-family

A resolved owner kind absent from the index table — `ReplicationController` (no
such kube-state-metrics annotation family exists), `Node` (static / mirror pods),
or any CRD controller (argo-rollouts `Rollout`, OpenKruise `CloneSet`) — yields no
Application, no error, and no change to the pod's other attributes.

Because `--metric-annotations-allowlist` is per-resource, the degrade is also
per-family: an operator can enable the annotation for `deployments` alone and get
Applications on Deployment-managed pods while every other kind stays absent. No
code path assumes the families arrive as a set.

## Risks / Trade-offs

- **`kube_replicaset_annotations` and `kube_job_annotations` are the two
  high-cardinality families** (retained old ReplicaSets under
  `revisionHistoryLimit`; accumulated Jobs under a CronJob's history limits), yet
  the ReplicaSet family is only ever consulted for a *bare* ReplicaSet — a rare
  shape, since the normal case is already collapsed to its Deployment. → The
  per-resource allowlist is the mitigation: an operator who does not run
  ArgoCD-managed bare ReplicaSets simply omits `replicasets` from
  `--metric-annotations-allowlist` and pays nothing, with graceful degradation
  per D7. Documented in `docs/kube-state-metrics-preconditions.md` rather than
  hardcoded, because which families are worth their cardinality is a
  deployment-shaped question.
- **Seven more upstream legs per `/v1/graph` request** — a ~23 % wider topology
  fan-out, all issued in parallel inside the existing errgroup and each bounded
  by the existing per-call `APITimeout`. → No new concurrency control is
  introduced; the legs are index lookups on small families (one series per
  controller object, not per pod) and the build already fans out 30 ways. If
  latency regresses, the honest lever is the allowlist (fewer families exported ⇒
  empty vectors, same query count) rather than serialising the group.
- **BREAKING for any deployment still synthesising the pod-level label** —
  including the `kube-state-graph-demo` repository's vmalert recording rule. →
  Called out in the proposal's Impact; the demo repository needs a real
  `argocd.argoproj.io/tracking-id` annotation on its workload manifests plus the
  annotation allowlist, tracked separately in that repository.
- **A pod's Application now depends on a second object's annotation surviving in
  the query window.** A controller deleted mid-window while its pods linger loses
  the Application for those pods. → Accepted: the topology reader is
  window-scoped throughout, `last_over_time` covers the whole window, and the
  failure mode is an omitted `omitempty` attribute, not a wrong one.
- **Golden files change** for any fixture pod that gains or loses an Application.
  → The wire *shape* is unchanged (`data.application`, `omitempty`), so the diff
  is fixture data only; `-update` regenerates and the diff is reviewable.

## Migration Plan

Read-path only — no stored state, no schema, no data migration.

1. **Operator, ahead of the rollout (optional, harmless):** add the
   `deployments` / `statefulsets` / `daemonsets` / `jobs` / `cronjobs` collectors
   (and `replicasets`, already required for `kube_replicaset_owner`) plus
   `--metric-annotations-allowlist=<resource>=[argocd.argoproj.io/tracking-id]`
   for each family they want. The current binary ignores these series, so this
   step is safe to land first and independently.
2. **Deploy the new binary.** Pods whose controllers carry the annotation gain
   `data.application`; pods relying on the withdrawn pod-level label lose it.
3. **Rollback** is a plain binary rollback. The previous binary reads the
   pod-level label, which is empty under stock kube-state-metrics, so rolling back
   returns to "no pod Applications" — there is nothing to undo and no state to
   repair. The extra KSM series left behind are inert.

## Open Questions

- Should `data.owner` eventually collapse `Job → CronJob` the way it collapses
  `ReplicaSet → Deployment`? Deferred deliberately (D2): it is a breaking change
  to an existing attribute and its `controller` compound group, and it does not
  affect this change's specs, approach, or task breakdown.
- Should `kube_pod_annotations` be added later as an *additional* source, for the
  minority of charts that copy the tracking-id into the pod template themselves?
  It would be a strictly additive precedence tier and can be decided once there is
  a deployment that actually needs it.
