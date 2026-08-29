## Context

See proposal.md — Why. Two properties of the existing code shape everything below.

**The readers already discard exactly what the new matchers exclude, and they do
it before anything is counted.** `resolveApplications` (`pkg/build/topology.go`)
opens each iteration with

```go
if raw == "" || argoAppName(raw) == "" { continue }
key, ok := keyOf(s.Metric)   // keyOf is where mc.bucket runs
```

so a series with an empty or absent tracking-id never reaches `keyOf` and never
reaches `mc.bucket`. `resolveJobCronJobOwners` is the same shape with
`owner_kind` / `owner_is_controller` in place of the tracking-id. This is what
makes the pushdown output-preserving down to the diagnostic counters, and it is
the property any future edit to those readers must preserve for the matchers to
stay honest.

**`fetch` and `fetchOptional` already differ on exactly the right axis.**
`fetchOptional` routes a query error through

```go
func optionalQueryFatal(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil { return err }
	return nil
}
```

with `ctx` bound to `callerCtx` — the caller's context, captured before the
errgroup shadows it. An upstream fault degrades; the caller's own deadline still
fails the request; a sibling leg's failure cancelling `gctx` does not make an
optional leg fatal. No new mechanism is needed, only a different call for two
legs.

## Goals / Non-Goals

**Goals:**

- Make the cost of the seven legs proportional to what the graph actually uses.
- Bound the blast radius of the two legs whose cardinality grows with history.
- Keep the change provably output-preserving: identical nodes, identical edges,
  identical attributes, identical `missing_cluster` tallies.

**Non-Goals:**

- Changing which pods resolve an Application, or how one is parsed.
- Revisiting whether `data.application` should exist, or adding a
  `kube_pod_annotations` tier (an Open Question of the archived change).
- Making the abort/degrade split configurable. It stays a hardcoded contract,
  like every other selector in `pkg/promql`.
- Reducing the fan-out. It stays 37 legs.

## Decisions

### D1: The two new matchers are metric-selection contracts, not caller filters

Both go in `braces(...)` ahead of the request-scoped matchers, exactly as
`lun=""`, `condition="Ready"` and `type=~"ExternalIP|InternalIP"` do, and both
get a named constant with a comment saying so:

```go
const jobOwnerCronJobSelector = `owner_kind="CronJob",owner_is_controller="true"`
const argoTrackingIDPresentSelector = `annotation_argocd_argoproj_io_tracking_id!=""`
```

The distinction is load-bearing in this repo: a *caller* filter is a request
dimension in `queryDims` and may vary per request; a *metric-selection contract*
is request-invariant, composed with (never replaced by) the request matchers, and
tested by `TestRender_ComposesFixedSelectorFirst`. These are the latter. The
`queryDims` entries stay `dimsNamespaced` and the request surface does not move.

**Why a constant rather than an inline string** for the annotation matcher: six
`Render` cases share it, and the label name is long enough that a typo in one of
six inline copies would be a silent per-family regression — the family would
return everything and the reader would still filter correctly, so no test outside
the render baseline would notice.

**Alternative rejected — push the tracking-id parse upstream too**
(`annotation_argocd_argoproj_io_tracking_id!~"^:.*"` to drop empty leading
segments). It duplicates `argoAppName`'s grammar in PromQL where it cannot be
unit-tested, and it buys nothing: those series are a rounding error next to the
un-annotated ones the `!=""` matcher already removes.

### D2: `!=""` excludes an absent label, and that is the desired behaviour

PromQL treats a missing label as `""`, so `annotation_...!=""` drops series that
never carried the label. That is precisely the un-allowlisted case, which is the
stock kube-state-metrics state and the single largest source of wasted series.

The consequence is a **meaning change in `RawSeriesCount`** for those six
families: it now counts annotated objects rather than all objects. This is
observable only in the build's debug log, and it makes the counter equal to the
`count(kube_<kind>_annotations{annotation_argocd_argoproj_io_tracking_id!=""})`
probe `docs/kube-state-metrics-preconditions.md` already documents as the
allowlist verification query. An operator reading `0` now learns "no annotated
objects reach me", which is the actionable fact; previously they read the
Deployment count and learned nothing.

**Alternative rejected — keep `RawSeriesCount` on the unfiltered population** by
issuing a second count query per family. Seven extra legs to preserve a debug
number is a bad trade, and it would reintroduce the cost the change exists to
remove.

### D3: The abort/degrade line is drawn at accumulating cardinality

`kube_replicaset_annotations` and `kube_job_annotations` degrade; the other four
annotation families and `kube_job_owner` keep `fetch`.

The criterion is **does the series count track the live object count, or does it
accumulate with history?** A Deployment, StatefulSet, DaemonSet or CronJob
contributes one series and keeps contributing one. A Deployment contributes up to
`revisionHistoryLimit` (default 10) ReplicaSet series, and a CronJob up to
`successfulJobsHistoryLimit + failedJobsHistoryLimit` (default 3 + 1) Job series —
plus every one-off, CI-triggered and Helm-hook Job in the estate. Only the
accumulating families can exceed an upstream limit while the live object count is
unremarkable, so only they need the degrade.

This revises **D4 of the archived `resolve-pod-application-from-controller`**,
whose argument was that using `fetchOptional` "would silently diverge two
annotation families from the other two" (the Service and PVC annotation legs). The
argument holds for the four non-accumulating families and they are left alone; it
does not extend to the two accumulating ones, because the sibling it appeals to —
`kube_service_annotations`, one series per Service — is in the non-accumulating
class. The consistency being defended was between different cardinality classes.

**Alternative considered — demote all seven** (the cleaner-looking line, "pure
enrichment never fails a build"). Rejected: it removes fail-fast from four legs
that cannot plausibly trigger it, and a silent degrade is worse than a loud
failure when the failure is a genuine upstream fault rather than a cost ceiling.

**Alternative rejected — a cardinality guard in Go** (cap the vector, warn, carry
on). It cannot help: the cost is paid upstream before the first sample is
returned, and the failure being defended against happens in VictoriaMetrics.

### D4: The fixed selectors ship with the degrade, in one change

They are independently correct but jointly motivated: the pushdown lowers the
probability of hitting an upstream limit, the degrade bounds the consequence when
it is hit anyway. Splitting them would land a change whose stated purpose —
"a decoration cannot take the API down" — is only half true in either half.

### D5: A degrade must stay subtractive, so the Job → CronJob hop is gated on the family having been read

Making `kube_job_annotations` optional is not purely subtractive on its own.
`resolvePodApplications` tries the Job's own annotation first and falls through
to the owning CronJob's only **on a miss** — the "nearest managed ancestor wins"
rule. A degraded family makes every Job miss, the annotated ones included, so
the fallthrough would attribute a directly-managed Job's pod to its CronJob's
Application. That is a *different value*, not an absent one: alone among the
package's optional legs, this degrade would report something wrong rather than
report less.

The fix is to carry one bit out of the fan-out — `topologyVectors.
JobAnnotationsDegraded`, set by `fetchOptionalTracking` on the swallowed-error
path only — and suppress the hop when it is set. The hop's precondition ("this
Job carries no annotation of its own") is unknowable from an unread family, so
the correct response to not knowing is to not infer.

The cost is a second-order loss: a Job that genuinely carries no annotation,
under a CronJob that does, also loses its Application for that build. Losing a
string is strictly better than reporting the wrong one, and it restores the
invariant that every optional leg in this package is subtractive.

`kube_replicaset_annotations` — the other degrading family — needs no flag. It
is consulted only for a **bare** ReplicaSet (the D34 skip has already collapsed
a Deployment-owned one), and a bare ReplicaSet has no further ancestor, so its
miss resolves no Application either way. Only a family with a fallback hop
behind it needs the distinction, which is why the flag is one named bool rather
than a general per-family degraded map.

**Alternative rejected — leave `kube_job_annotations` on `fetch`.** It removes
the mis-attribution by removing the degrade, but Job annotations are one of the
two accumulating-cardinality families D3 exists to protect, and it is the
likelier of the two to blow a series limit (every CI and Helm-hook Job in the
estate). Keeping the degrade and gating the inference costs one bool.

**Alternative rejected — thread a general `map[promql.Query]bool` of degraded
families through the parse.** Only one reader has an inference to gate; a map
would be state nobody reads, and the compiler would stop catching a new
fallthrough that forgot to consult it. Add a second bool when a second hop
appears.

## Risks / Trade-offs

- **A future edit to `resolveApplications` or `resolveJobCronJobOwners` loosens
  what the reader keeps, while the query still excludes it** → the readers become
  strictly narrower than the queries and Applications silently disappear.
  Mitigated by a test that asserts the equivalence directly: feed the reader a
  vector containing the series the matcher would have excluded and assert the
  resulting index is identical to the filtered input's.

- **An operator relies on `RawSeriesCount` for the six families as an object
  count** → the number drops, possibly to zero, with no behaviour change.
  Mitigated by documenting the new meaning in `docs/upstream-metrics.md` and
  `docs/kube-state-metrics-preconditions.md` next to the verification probe it now
  equals. Not mitigated further: it is a debug-log field, not an API.

- **A degrading family fails every request for an unrelated reason and nobody
  notices**, because the graph still returns `200` → Applications quietly vanish
  for bare-ReplicaSet and Job-owned pods. Mitigated by the log that
  `fetchOptional` already emits per failure; the same trade-off the 15 Harvest and
  kubelet legs have carried since D1 of the original design.

- **An exporter that is kube-state-metrics-shaped but stamps `owner_is_controller`
  differently** (e.g. `True`) loses the Job → CronJob hop entirely, where before
  the reader would also have missed it — same outcome, but now invisible in
  `RawSeriesCount` too. Accepted: the label's value casing is already part of the
  series contract the reader depends on, and no stock producer differs.

## Migration Plan

No migration. The change is output-preserving for every deployment whose upstream
answers all seven queries, and strictly better for one whose upstream cannot.
Rollback is a revert: the readers are untouched, so an older binary reads a
narrower vector correctly and a newer binary reads a wider one correctly.

Operators gain one new observable: `kube_replicaset_annotations` /
`kube_job_annotations` failing upstream now produces a logged warning and a
`200` instead of a `5xx`.
