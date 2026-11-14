# Design

## Context

See proposal.md — Why. Current state:

- The storage build reads known pods through `issuePodFamiliesByNamespace` (pod wave, node-seed incarnation completion and by-pod bindings, application-root recovered-pod bindings) and known claims through `issueClaimFamiliesByNamespace` (claim side of the hub, the claim seed and the workload expansion; mounter completion). Both group the refs by namespace and render `promql.RenderNamesInNamespace` — `namespace="<ns>"` plus a name alternation — through `issueScopedFamilies`, one query per `(family × namespace × chunk)`.
- The Kubernetes-only build (`readK8sWorkload`) reads the same pods and claims through `k8sRead.issuePairReads`: `promql.ChunkNamePairs` (sorted by `(namespace, name)`, packed under the byte budget less `promql.RequestMatcherCost`, each alternation closed at `promql.MaxAlternationValues` distinct values) and `promql.RenderNamesAcrossNamespaces`, then `promql.KeepNamePairs` / the `claimKey` filter with `tallyDropped`.
- `TestBuildStorage_FanOutUnchanged` pins every storage query in `pkg/build/testdata/storage_fanout.golden`; the plan audit (P-8) recorded that a large filer root spans hundreds of namespaces and costs one to three thousand queries per cold build.

## Goals / Non-Goals

**Goals:**

- One query per chunk instead of one per namespace for every storage read of known pods and claims, with a body byte-identical to today's for every root kind.
- One grouped-read implementation for both builds, so the two cannot disagree about chunking, rendering or what a cross pair is.

**Non-Goals:**

- Moving the storage build's read ORDER or adopting the Kubernetes-only build's round overlap (P-9, kept: the Harvest chain is the storage build's critical path).
- Changing a `pvc` root's seed, the request-derived caps, or any Harvest, node, controller or application-metadata read.

## Decisions

### D1. Group with `RenderNamesAcrossNamespaces` + `ChunkNamePairs`, shared with the k8s build

`issuePairReads` (lifted out of `k8sRead` into a free function the method now delegates to) issues every chunk of every family under one `scopeConcurrency`-bounded errgroup, renders each chunk with `RenderNamesAcrossNamespaces`, and lands the rows in chunk order. The storage readers become `issuePodFamiliesByPair` / `queryPodsByPair` and `issueClaimFamiliesByPair` over it.

*Rejected — keep per-namespace reads (option A).* The query count stays O(namespaces) per family, which is the cost this change exists to remove.
*Rejected — a second, storage-only grouped reader.* Two chunkers and two renderers for the same restriction would drift; the k8s build's `TestBuildK8s_PlanIsOutputPreserving` already pins the shared helpers' semantics.
*Rejected — one PromQL join of the families on `(namespace, pod)`.* VictoriaMetrics pushes a join's labels down only up to 100 values per label and Prometheus not at all, so a join is neither portable nor bounded (the reason the k8s build has none).

### D2. Cross pairs are dropped in the reader and tallied

The two alternations are independent, so a chunk restricted to `namespace=~"a|b",pod=~"x|y"` can return `a/y` when only `a/x` and `b/y` were requested. Pod reads keep only rows whose `(namespace, pod)` is a requested pair (`promql.KeepNamePairs`), so every caller sees exactly the pairs a per-namespace read returned; claim reads keep the existing `claimKey` filter, which already drops cross pairs as well as same-named claims of other clusters. The dropped rows are added to `ExtraSeriesCount`, so `RawSeriesCount` keeps counting what the restrictions matched — the spec's tally definition, and what the k8s build does.

*Rejected — leave cross pairs in the vectors and rely on later keyed lookups.* The node, controller and claim reads are scoped from what the pod read returns; a cross pair would widen them (another namespace's node and controllers), which "A same-named pod in another namespace never widens the node or controller read" forbids.
*Rejected — drop without tallying (the old claim filter's behaviour).* The tally would undercount the upstream's work by exactly the rows grouping adds, hiding the one cost this change introduces.

### D3. Budget: the shared byte budget less the request matchers

A chunk is charged for its two alternations under `opts.qosScopeBatchBytes() - RequestMatcherCost(q)` (the k8s build's `pairBudget`), and `ChunkNamePairs` closes it before either alternation passes 100 distinct values. The per-namespace reads charged the namespace equality once per chunk and did not charge the request matchers.

*Rejected — keep the old accounting.* With an alternation on both sides the namespace side is no longer a constant, and the budget is meant to bound the whole selector a chunk sends.

### D4. A `pvc` root's seed stays per namespace

The seed's chunk count is the request-derived cap (`maxRootedVolumeLabelChunks`, summed across namespaces) and is checked before any query from the same split it then issues. Grouping it would change what the cap counts and needs its own decision about bounding the namespace side (policy P-7). Every read AFTER the seed — the claims it returned — is grouped.

*Rejected — group the seed too.* A cap change is a contract change for clients near the limit and belongs with P-7 / C1, not in a fan-out refactor.

### D5. A ref with an empty namespace or name reads nothing

`promql.SortedNamePairs` drops a pair with either half empty, and `RenderNamesAcrossNamespaces` cannot render an empty alternation. The per-namespace reader issued `namespace=""` for an empty namespace. Kubernetes pods and claims are always namespaced and the readers already drop rows without one (`claimSeedGroup.keep`, the parse's PVC key), so nothing that could reach the body is lost.

*Rejected — fall back to an equality read for the empty namespace.* It would keep a second rendering path alive for a shape kube-state-metrics never emits.

### D6. Refresh `storage_fanout.golden` deliberately

The golden pins every storage query; this change intends to move exactly the Kubernetes pod- and claim-keyed lines (34 added, 68 removed across nine root shapes) and nothing on the Harvest side. It is refreshed with `-update-storage-fanout`, and the body parity is checked separately: the serialised bodies of 17 root shapes over `planEstate()` (node, pod, application, aggr, ontap_cluster, svm and pvc roots) are byte-identical before and after.

### D7. Scenario titles are kept, bodies rewritten

`openspec validate` refuses a MODIFIED block that drops a scenario the current spec has, and offers no scenario rename. The `storage-graph-api` scenario "Pods are read one query per namespace" therefore keeps its title while its body now states the grouped read; it should be retitled ("Pods in several namespaces share one query") by hand in `openspec/specs/storage-graph-api/spec.md` when this change is archived.

*Rejected — REMOVED + ADDED of the whole requirement.* "Storage build reads every family by reference" holds 38 scenarios; removing and re-adding it to retitle one would hide the real delta in a 200-line churn.

## Risks / Trade-offs

- **Cross-pair over-read.** A grouped chunk can return up to (namespaces × names) − pairs extra rows. Bounded by the 100-value cap per side and discarded before anything is scoped from it; in practice pod and claim names are rarely shared across a tracked set's namespaces (StatefulSet ordinals `db-0`, claim names `data` are the common exception, and those are exactly the reads that used to cost one query per namespace).
- **Upstream lookup cost.** A two-alternation selector resolves both by exact value lookup up to 100 values each (VictoriaMetrics `getOrValues`), so a grouped query is not a regex scan.

## Migration Plan

None — no parameter, body, status or exported API changes. Rollback is a revert.
