## Context

See proposal.md — Why. The code this change sits beside:

- `resolvePodOwners`, `resolveControllerApplications`, `resolveJobCronJobOwners`
  and `resolvePodApplications` (`pkg/build/topology.go`) are pure functions over
  whole-estate vectors keyed by `podNameKey{cluster, namespace, pod}`, where
  `cluster` is the composed identity `<az>-<env>-<cluster>`. The zone is never a
  join key on its own; it is a component of the identity and a request matcher.
- `controllerAnnotationFamilies` already binds each owner kind to its
  annotation family and that family's resource-identity label (`job_name` for
  Jobs).
- `Router.QueryLabels` (`pkg/promql/labelquery.go`) issues one routed,
  family-scoped query for an arbitrary metric with exact label equalities and
  returns label sets. It is the only routed path that can filter on `pod`,
  `owner_kind` or a controller name — `promql.Selector` carries only
  az/env/cluster/namespace.

## Goals / Non-Goals

**Goals:**

- The same Application the graph build would attach to the pod, for every
  resolution path the build has.
- Only the series the pod's path needs: two queries for a directly-owned pod,
  at most five for a CronJob-managed one.
- No change to the graph build's output or query set.

**Non-Goals:**

- Batch lookup of many pods — a caller with many pods wants the build.
- Service or PVC Applications, or PVC inheritance from mounting pods.
- A `pkg/kubegraph` facade method or an HTTP route. `Engine` holds a
  `promql.Querier`, which need not be a `*Router`.

## Decisions

### D1. Live in `pkg/build`, not `pkg/promql` or a new package

The rules are the build's: `argoAppName`, `controllerAnnotationFamilies` and the
tie-breaks are unexported there. Placing the lookup beside them lets both paths
share one table and one usability helper. `pkg/promql` stays a query layer with
no knowledge of ArgoCD. A new package would need those helpers exported.

### D2. Identity is `(cluster, namespace, pod)`; `az` and `env` are pinned

The request mirrors `podNameKey`. `cluster` is the raw label, matching the
request surface's `?cluster=` convention. The graph request needs no zone —
`?cluster=c1` alone admits every zone's `c1`, and the composed identity keeps
them apart — so the lookup does not require one either.

A raw cluster name can recur across zones and environments, and the composed
identity then differs only by `az` or `env`. For each of the two the caller
leaves empty, the lookup reads the label (the configured `LabelKeys.AZ` /
`LabelKeys.Env`) off the pod-owner rows: a single `(az, env)` pins every later
query to it, several fail with `ErrAmbiguousPod`. Pinning matters beyond the
first query — without it a same-named Deployment in the other zone or
environment would feed the annotation read.

A zone value goes in `LabelQuery.AZ`, so it routes as well as renders its
matcher: with no `AZ` only the pod-owner query reaches every `ksm` backend. An
absent label pins to `""`, which PromQL matches as absent-or-empty, so an estate
without the label still resolves. An empty zone cannot route, so it is matched
as an `az=""` filter with `LabelQuery.AZ` left empty — the escape hatch the
label query already permits.

### D3. Explicit tie-breaks in Go, never the result order

`QueryLabels` sorts label sets by full label-key order, so incidental labels
such as `instance` decide ordering before `owner_kind` does. Each pick is
therefore explicit and mirrors its batch counterpart: min `(owner_kind,
owner_name)` for the controller owner; min non-empty `owner_name` for the
ReplicaSet → Deployment and Job → CronJob hops; min raw tracking-id among values
passing `usableTrackingID` for the annotation.

### D4. Every upstream error fails the lookup

The build degrades `kube_replicaset_annotations` and `kube_job_annotations` on a
query error and suppresses the CronJob hop when the Job family degraded, because
a build must survive a single leg. A single lookup has no such pressure, and
degrading would force it to re-derive that suppression gate. Returning the error
(wrapped with the query name) is subtractive by construction.

### D5. Absence is `""` with a nil error

No controller owner, an owner kind with no annotation family, and a controller
with no usable tracking-id all return `""`, matching `PodNode.Application()`.

### D6. Test fake, not the generated mock, inside `pkg/build`

`pkg/build/mocks` already imports `pkg/build` for the `RouteResolver` mock, so
an in-package test importing it is a cycle. The in-package tests use a small
fixture store implementing `LabelQuerier`; that also lets one fixture feed both
the batch chain and the lookup in a parity test. The generated mock is for
consumers outside the package.

## Risks / Trade-offs

- **Sequential round-trips** — up to five for a CronJob-managed pod. Each leg
  depends on the previous leg's answer, so they cannot run concurrently.
- **No `AZ` fans the first query out.** The pod-owner query then reaches every
  `ksm` backend; a caller that knows the zone should pass it.
- **`Cluster` is an exact equality.** `cluster="unknown"` matches only the
  literal label, not an absent one, unlike the graph request's `unknown` bucket.
  `Cluster` is required, so a pod whose series carry no `cluster` label cannot be
  looked up. Such an estate is out of scope.
