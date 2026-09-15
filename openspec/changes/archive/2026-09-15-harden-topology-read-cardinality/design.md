## Context

See proposal.md — Why. Five properties of the current code shape the design.

**The storage build is the graph build's topology read, unchanged.**
`Builder.BuildStorage` (`pkg/build/build.go`) calls the same exported
`ReadTopology` that `Builder.Build` does, so the 37-leg errgroup — including
`kube_pod_container_info` on abort-on-error `fetch` — runs for a body that
`assembleStorageFlow` builds from `topology.Pods`, `topology.Nodes`,
`topology.PVCs`, `PodPVCs`, the NetApp inventory and the storage edges only.
The service, endpointslice and container vectors are parsed and discarded.

**`parseTopology` already treats a nil vector as an absent family.** Every
OPTIONAL leg degrades by writing `nil` into its `topologyVectors` slot, and the
parse ranges over the slots without distinguishing nil from empty. Not
launching a leg is therefore the same code path as a degraded one, with one
difference this design has to add: `RawSeriesCount` is populated from a fixed
literal map, so a skipped leg would read `0` there unless the tally is built
from the legs actually launched.

**The claim-binding → pod join is by name, not UID.** The binding series
(`kube_pod_spec_volumes_persistentvolumeclaims_info`) is joined to its pod
through `canonicalPodUID[[3]string{cluster, namespace, pod}]`, built from
`kube_pod_info`. The binding family already carries every pod NAME the storage
body can draw; the request's `pod=<ns>/<name>` roots carry the rest. That
decides the scope label (D3).

**The scoped second wave already exists for Harvest.** `readScopedQoS`
(`pkg/build/qosscope.go`) waits on two named legs through `signalWhenDone`
channels inside the one errgroup, computes a sorted scope, chunks it with
`promql.ChunkQoSVolumeScope` under `Options.QoSScopeBatchBytes`, issues chunks
under a bounded errgroup, and merges in chunk order. `RenderQoSVolumeScoped`
renders the alternation as the query's ONLY matcher because Harvest takes no
request matcher; the pod families DO take one, so the renderer cannot be reused
as-is (D4).

**Optional metrics upgrades are type-asserted, never widened.**
`promql.Metrics` has two methods and is satisfied structurally by embedders;
`RouterMetrics` / `Prober` / `QuerierSource` are optional upgrade interfaces
recovered with a type assertion (`routerMetricsOf`). The new histogram follows
that shape (D9).

**The upstream limit counts selected series before aggregation.**
VictoriaMetrics' `-search.maxUniqueTimeseries` (auto-derived from vmselect
memory when unset) rejects a query whose matchers SELECT more series than the
cap, per query, regardless of any `sum by`. A range read (`[w]`) selects every
series with a sample in the window, so deleted pods count. Only fewer legs,
narrower matchers, or more queries move it.

## Goals / Non-Goals

**Goals:**

- A `/v1/storage-graph` build never issues the container family and fetches pod
  rows in proportion to pods that mount a claim, not to the estate.
- A `/v1/graph` build survives a `kube_pod_container_info` rejection with a
  200 and no `containers`, and nothing else moves.
- Every storage body is byte-identical to today's, minus `containers`; every
  `/v1/graph` body is byte-identical to today's.
- An operator can see result cardinality per leg before a limit rejects it.
- `queryDims`, the request surface, the flag set and the dependency set stay
  unchanged.

**Non-Goals:**

- Scoping the `/v1/graph` pod read. Its default projection needs every pod on
  a connectivity edge, which is not known before the service-graph read.
- Reverse scoping from storage roots (aggregate → claims → pods). The
  derive-then-match volume join runs Go-side over `volumename` and is not
  invertible into a PromQL matcher without the storage prefix; a later change.
- Scoping `kube_replicaset_owner` (one series per ReplicaSet including
  retained history). Noted as the next-largest required leg; separate change.
- A configurable per-endpoint read plan. The plan is a hardcoded property of
  the endpoint, like every other selector contract in this repo.
- Any frontend change. The Sankey reads none of the removed data.

## Decisions

### D1: `kube_pod_container_info` moves to `fetchOptional`, with no degrade flag

The leg becomes the third `fetchOptional` kube-state-metrics leg. The two
annotation legs needed `fetchOptionalTracking` because a downstream rule (the
Job → CronJob hop) infers something from a family's ABSENCE; nothing infers
anything from missing container info, so the plain `fetchOptional` wrapper is
enough and `topologyVectors` gains no flag. `RawSeriesCount` for the leg keeps
the existing convention for optional legs: `0` covers both "matched nothing"
and "errored and degraded", separated only by the `optional topology query
failed` Warn.

**Alternative rejected — shrink the query instead (shorter lookback, `sum by`).**
Aggregation does not move the selected-series count, and a shorter lookback
changes the "latest image" semantics while still reading every live pod. The
leg stays as it is and simply stops being fatal.

### D2: A read plan inside one reader, not a second reader

`ReadTopology` keeps its exported signature and becomes
`readTopology(ctx, q, window, end, opts, sel, fullPlan)`. `BuildStorage` calls
`readTopology(..., storagePlan(roots))`. A plan is an unexported struct:

```go
type topologyPlan struct {
	skip      map[promql.Query]bool // first-wave legs never launched
	scopePods bool                  // read kube_pod_info / kube_pod_owner by reference
	podRoots  []string              // pod-name segments of the pod=<ns>/<name> roots
}
```

`storagePlan` skips `QPodContainerInfo`, `QServiceInfo`,
`QEndpointSliceEndpoints`, `QEndpointSliceLabels`, `QServiceAnnotations` and
sets `scopePods` with the request's sorted `pod=` root names. The launch loop and the
`RawSeriesCount` tally are driven from ONE table of `(query, *vector, fetch
kind)` entries, so a skipped leg is neither launched nor tallied — its key is
absent, never `0` — and a leg cannot be added to one without the other.

**Why skipping the four service-side families is output-preserving for the
storage body:** `ServicesByNameNS`, `EndpointsByService` and
`ServiceApplications` are read only by the service-graph resolver, which the
storage build never runs. `kube_service_info` also feeds the cluster-identity
FIRST PASS, but a cluster that holds Services and no pod, node or claim binding
mints no node in a storage body and never appears in its `clusters[]`, so the
identity table's extra entry is unobservable there.

**Alternatives rejected.** A separate `ReadStorageTopology` duplicating the
launch block: two copies of a 40-line fan-out drift. A field on
`build.Options`: the plan is per endpoint, `Options` is per builder.

### D3: The pod scope is an alternation on `pod` (name), not `uid`

Scope = sorted, de-duplicated union of every loaded binding series' `pod`
label (skipping a series naming no claim, as the binding reader does) and every
`pod=` root's `Name`. Rendered through the helper a request dimension uses:
`pod="a"` for one value, `pod=~"a|b|c"` for several, each value
`regexp.QuoteMeta`-escaped — the escaping `RenderQoSVolumeScoped` uses; PromQL
anchors `=~` itself.

Why names: the binding family carries names and joins to pods by name; roots
carry names; one matcher form covers both. A `uid` scope would need a second
matcher form for roots (`... or kube_pod_info{pod=~...}`), two rendered shapes,
and two test baselines. The cost of names is that a name is unique per
namespace only, so the scope MAY admit `platform/web-0` when `shop/web-0`
mounts a claim. The admitted pod sits on no retained path and is not a root, so
`ProjectStorage` drops it, and the body is unchanged — pinned by the parity
test in tasks group 1. Under item 4's derived namespace the collision cannot
even reach the upstream for pod-only-root requests.

### D4: `promql.RenderScoped` composes request matchers with a data-derived alternation

```go
// RenderScoped renders q with its fixed selector, the request matchers, and one
// data-derived alternation — in that order. ok is false when values holds no
// non-empty value or q is not scopeable.
func RenderScoped(q Query, window time.Duration, keys LabelKeys, sel Selector,
	values []string) (string, bool)
```

Scopeable queries are a hardcoded table `{QPodInfo: "pod", QPodOwner: "pod"}`
naming the label each is scoped on, the way `isQoSWorkloadQuery` gates the
Harvest renderer — so no caller can scope a leg whose reader was not written
for it, or scope one on a key its reader does not join on. `queryDims` is
untouched: the alternation is not a request dimension, and `Selector.Reaches`
still reads the table. `TestRenderScoped` pins that the output is `Render`'s
with exactly one matcher appended, and `render-baseline.txt` does not move.
`RenderQoSVolumeScoped` stays as the Harvest renderer (its "ONLY matcher"
contract is spec text); both share `normaliseValues` and the alternation
helper. `ChunkQoSVolumeScope` is generalised to `ChunkScope` with the old name
kept as a thin alias — one exported symbol fewer to break for the sake of a
rename is not worth it.

### D5: The pod wave is a REQUIRED scoped read that waits on the binding leg alone

`readScopedPods` (`pkg/build/podscope.go`) mirrors `readScopedQoS`: it selects
on the `QPVCBindings` done-channel and `ctx.Done()`, computes the scope from
`v.PVC` plus `plan.podScope.roots`, returns immediately when the scope is empty
(no query issued, both vectors nil, both tally keys absent), chunks under
`opts.QoSScopeBatchBytes`, issues `2 × chunks` queries under the existing
bounded errgroup limit, and merges each family in chunk-index order.

Two deliberate differences from the QoS wave. (1) **Chunks fail closed.** A
pod is topology: a missing chunk is a smaller, plausible, wrong body — exactly
the partial-fan-out failure D6 of backend routing forbids — so a chunk error is
returned into the group and the build fails, as an unscoped `kube_pod_info`
error does today. (2) **Nothing waits on the pod wave.** The parse runs after
`g.Wait()`, so no downstream channel is needed.

The QoS wave is unaffected: it waits on `QPVCInfo` and `QVolumeLabels`, neither
of which moves. `warnSelectorFamilyEmpty` is gated on `raw[QPodInfo] == 0`, so
an empty scope simply stays quiet.

### D6: The pod alternation reuses the QoS byte budget

`Options.QoSScopeBatchBytes` / `--netapp-qos-scope-batch-bytes` /
`KSG_NETAPP_QOS_SCOPE_BATCH_BYTES` bound the pod alternation too. The default
(8192) sits under VictoriaMetrics' default `-search.maxQueryLen` (16 KiB) with
room for the request matchers. Documentation gains "bounds every data-derived
alternation"; the flag name is kept because renaming a flag is a configuration
break for no operator benefit.

**Alternative rejected — a second flag.** Two knobs for one concern ("how long
may a generated alternation be against this upstream") with no case in which an
operator would set them differently.

### D7: Roots reach the build through the signature

`Builder.BuildStorage(ctx, window, end, sel, roots graph.StorageRoots)` and
`Engine.BuildStorage` likewise; `Engine.BuildStorageFromValues` and the HTTP
handler pass `req.Scope.Roots`. A claimless `pod=` root is drawable only if its
name is in the scope, so the build MUST know the roots.

This revises `add-netapp-storage-graph-api` design D2, which rejected passing
roots into the build because it "would make the build a function of the
request, which the caching key and the 'selectors only reach queries' rule both
forbid". Neither reason survives here. v1 has no result cache, so no key widens
today; a future cache for this endpoint keys on the pod-root name set as well,
or caches the binding-derived scope and reads the (at most `len(roots)`) root
pods beside it. And the roots reach exactly one read, only ever ADD one pod
name each to a scope that is otherwise data-derived exactly like the QoS
volume scope, and never narrow a read — which paths are drawn stays a
projection decision. The alternative was reading every pod in the estate.

**Alternative rejected — keep the old signature and add a roots-taking
sibling.** An embedder calling the old `BuildStorage` and then projecting with
pod roots would silently lose every claimless root — the "Roots are always
materialised" requirement broken with no error. A compile error is the honest
failure. It is a Go-API break for the storage surface (weeks old, one known
consumer: this repo), recorded in `docs/BREAKING.md`.

### D8: The derived namespace is computed in the single request parser

`ParseStorageValues` gains a pure `deriveStorageNamespaces(scope
graph.StorageScope, explicit []string) []string`: when
`scope.Roots.RequestedStorage()` is false, `len(scope.Roots.Pods) > 0`,
`scope.Roots.Nodes` is empty and `explicit` holds no non-empty value, it returns
the sorted, de-duplicated `PodRef.Namespace` set; otherwise it returns
`explicit` unchanged. The result becomes `req.Selector.Namespace`, so the
rendering path is the existing one (`dimsNamespaced` families get
`namespace=~"a|b"`, `ALERTS` gets the or-absent form) and `Selector.Active()`
semantics are untouched. `scope.Namespaces` (the projection re-application)
stays as parsed — the derivation narrows the upstream read only.

Why the parser: it is the ONE place both the HTTP handler and the in-process
facade go through, so "embedder and server agree" holds by construction, and
the rule is a request-shape rule, not a build rule. Why "explicit wins, never
intersected": an explicit `?namespace=` is already the narrower read; deriving
an intersection could produce an EMPTY value set, which the selector treats as
"no filter" — the one outcome that would WIDEN a read.

Output preservation argument, pinned by a parity test: with pod-only roots,
`ProjectStorage` retains a unit iff it intersects a root pod; a unit is one
claim's chain, its claim is in the root's namespace (`pod-mounts-pvc` is
same-namespace), every other pod on it mounts that claim, and the unit's node,
aggregate, SVM and controller come from families the namespace matcher never
reaches. Nothing retained lies outside the derived set.

### D9: The histogram is an optional `promql.SeriesMetrics` upgrade

```go
type SeriesMetrics interface {
	// ObserveQuerySeries records how many series one successful upstream
	// query returned, labelled by query name.
	ObserveQuerySeries(name string, n int)
}
```

`Client.Instant` recovers it with `seriesMetricsOf(c.metrics)` (the
`routerMetricsOf` shape) and observes `len(vec)` after a successful decode —
where the span attribute `kube_state_graph.result_series_count` is already
set, so the two signals cannot disagree. A failed query contributes nothing.
`internal/observability` adds
`kube_state_graph_upstream_query_result_series` as a `HistogramVec{query}` with
`prometheus.ExponentialBuckets(1024, 2, 11)` (1024 … 1048576) and the adapter
method. `promql.Metrics` is NOT widened, so every existing embedder
implementation still compiles and gets no histogram.

Why a histogram and not a per-query gauge: a gauge is the LAST build's value
and turns a function of request mix (filtered vs unfiltered, graph vs storage)
into a number that looks like a property of the estate — the same reason
`BuildStorage` does not write the three graph gauges. A histogram's `_bucket`
answers the operator's question directly: "did any query return more than
65536 series in the last hour". Why powers of two: 2^16 = 65536 is the bucket
just below the observed memory-derived cap (67108), and every halving of
vmselect memory moves the cap by one bucket.

Cardinality: ~46 query names × 12 buckets ≈ 550 series, the same order as the
existing duration histogram.

### D10: Both build logs name the largest leg; the full tally goes to Debug

`graph built` and `storage graph built` gain `largest_leg` and
`largest_leg_series` from a pure `largestLeg(map[string]int) (string, int)`
with a lexical tie-break, and both paths emit `raw_series_counts` at Debug.
An Info line with a 30-key map on every request is noise; one name and one
number is what an operator reads when a 422 is a week away.

### D11: Fan-out pins move from one pair to three

`TestReadTopology_FanOutLegCount` keeps 37 / 43 for the full plan. The storage
plan is pinned at 32 / 38 with a one-chunk pod scope (37 − 5 skipped; the two
pod legs still count once each) and at 30 / 36 with an empty scope. A leg
added to one plan and not the other fails exactly one pin, which is the point.

## Risks / Trade-offs

- [`/v1/graph` silently loses `containers` on a rejected leg] → the existing
  `optional topology query failed` Warn plus
  `kube_state_graph_upstream_query_failures_total{query="kube_pod_container_info"}`;
  the same alerting guidance the two annotation legs already carry, restated in
  `docs/BREAKING.md`.
- [Name-scoped pod read admits same-named pods from other namespaces] → dropped
  at projection; parity test pins body equality; item 4 removes the case for
  pod-only-root requests.
- [Claim-heavy estates produce many chunks (5 000 mounting pods ≈ 35 chunks ×
  2 families)] → each chunk is tiny and bounded by the existing concurrency
  limit; the budget is operator-tunable; the total is still a strict subset of
  today's unscoped read.
- [A pod chunk failure now fails the storage build where a QoS chunk would
  degrade] → intended (D5); the error names the leg, and the alternative is a
  wrong body with no signal.
- [`BuildStorage` signature change breaks an embedder] → compile-time,
  documented; the storage engine surface has one known consumer.
- [Component tests with strict mock expectations for the storage goldens
  expect the five skipped queries] → fixtures updated; the storage goldens
  carry no `containers`, so their bytes do not move.
- [Derived namespace depends on `pod-mounts-pvc` being same-namespace] → a
  Kubernetes invariant (a pod can only reference a claim in its own namespace)
  and a parity test.
- [Histogram adds ~550 series to `/metrics`] → same label discipline and order
  of magnitude as the duration histogram; no per-backend label.

## Migration Plan

- Deploy with no configuration change. Operators alerting on `/v1/graph` 5xx
  for the container leg move the alert to the failures counter. Embedders
  calling `Builder.BuildStorage` / `Engine.BuildStorage` directly add the
  `graph.StorageRoots` argument (`graph.StorageRoots{}` reproduces "no roots").
- Rollback is a revert; no persisted state, no schema.

## Open Questions

- Whether `kube_replicaset_owner` — one series per ReplicaSet including
  history retained by `revisionHistoryLimit`, still a required `fetch` — should
  be scoped by reference to the loaded pods' owners in a later change. It does
  not affect this change's specs or tasks.
