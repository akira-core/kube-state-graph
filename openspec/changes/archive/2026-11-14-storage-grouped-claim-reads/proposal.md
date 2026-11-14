# Proposal

## Why

A `/v1/storage-graph` build reads every pod and claim it already knows one query per `(family × namespace)`. That keeps a common pod or claim name from being read across the estate, but it makes the query count grow with the number of namespaces the tracked set spans, not with its size. A large filer root, a shared Kubernetes node or an Application spread over tenants reaches hundreds of namespaces, and one cold build then issues one to three thousand queries — each through the per-store slot (32 by default), each a round trip, each able to fail the fail-closed build. On a node root whose mounting pods live in 40 namespaces the pod- and claim-keyed families alone take 281 queries.

`/v1/k8s-graph` already reads the same pods and claims grouped: one query per chunk carrying a namespace alternation beside the name alternation, chunked under the shared byte budget and the 100-value alternation bound, with cross pairs dropped in the reader. The storage build kept its per-namespace reads only to keep `storage_fanout.golden` still while the Kubernetes-only read plan was introduced; nothing in its body depends on them.

## What Changes

- Every storage-build read of known pods — the pod wave, a `node` root's incarnation completion and by-pod claim-binding read, an `application` root's recovered-pod binding read — and of known claims — the claim side of every seed kind (claim info, claim annotations, the two kubelet families) and mounter completion — is issued grouped across namespaces: one query per chunk restricted to `namespace=~"…"` and `pod=~"…"` / `persistentvolumeclaim=~"…"` beside the request matchers, chunked so that neither alternation holds more than 100 distinct values and the chunk fits the shared byte budget less the request matchers it repeats.
- A row whose `(namespace, name)` pair the read did not request — a cross pair the two independent alternations admit — is discarded before any later read's restriction is computed from it (pods by their pair, claims by the existing `(az, env, cluster, namespace, claim)` key). The dropped rows are still counted in the per-family series tally, which counts what the restrictions matched.
- The body is byte-identical for every root kind. The query set changes: `pkg/build/testdata/storage_fanout.golden` is refreshed deliberately.
- Unchanged: a `pvc` root's seed (the claim-info read on the root `(namespace, claim)` pairs) stays one query per namespace, because its chunk count is the request-derived cap `maxRootedVolumeLabelChunks` summed across namespaces; a `pod` root's seed already reads its bindings grouped; every Harvest, node, controller and application-metadata read; `/v1/graph`; `/v1/k8s-graph`.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `storage-graph-api`: the claim side and every read of known pods are grouped across namespaces instead of one query per namespace; cross pairs are discarded before any later read and counted in the tally; the 100-value bound applies to each alternation of a grouped read.
- `k8s-graph-api`: the scenario that said the storage graph keeps one query per namespace now says it groups its reads the same way and keeps only its own order.

## Impact

- **Code**: `pkg/build` only — `issuePodFamiliesByPair` / `queryPodsByPair` (were `…ByNamespace`) and `issueClaimFamiliesByPair` (was `issueClaimFamiliesByNamespace`) read through the shared `issuePairReads` the Kubernetes-only build already uses. No exported symbol, signature, interface or mock changes.
- **API**: none. Bodies are byte-identical; no parameter, status or field changes.
- **Upstream load**: fewer queries — a pod- or claim-keyed family drops from one query per namespace to one per chunk (one for up to 100 namespaces and 100 names within the byte budget). Each grouped query can return cross-pair rows a per-namespace read did not; they are bounded by the product of the two alternations within one chunk and discarded in the reader.
- **Docs**: `docs/upstream-metrics.md`, `.claude/rules/storage-graph.md`, `.claude/rules/netapp-storage-join.md`. No `docs/BREAKING.md` entry: nothing a client sees changes.
- **Tests**: `storage_fanout.golden` refreshed (Kubernetes pod/claim-keyed lines only); per-namespace pins in `pkg/build`, `pkg/kubegraph` and `internal/api` updated to the grouped form; a query-count test for a 40-namespace node root and a cross-pair test added.
