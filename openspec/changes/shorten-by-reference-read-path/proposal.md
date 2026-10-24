# Proposal

## Why

`GET /v1/k8s-graph` is slow. Reading the code points at the `node` root as the likely hot path; no trace has confirmed it yet. To draw what runs on a node, the build loads every pod on the root node, where `/v1/storage-graph` follows only the claim-mounting ones. Its owner, controller and application-metadata reads therefore span every namespace those pods live in, each issued one query per namespace per family, along a dependency chain of seven sequential upstream rounds. A `pod` root shares the chain at five rounds. `/v1/storage-graph` reads a far smaller pod set and is not reported slow; it stays exactly as it is.

## What Changes

Only the Kubernetes-only read plan behind `/v1/k8s-graph` changes. `/v1/storage-graph` keeps its queries, their order and its bodies.

- **Grouped namespace restriction.** A read of known pods or claims is issued one query per chunk, carrying `namespace=~"a|b"` beside the name alternation, instead of one query per namespace. A row whose `(namespace, name)` pair was not requested is discarded before any later read's scope is computed.
- **Rounds follow real dependencies.**
  - Node root: incarnation completion, the claim-binding read and `kube_pod_owner` are issued together on the seed's pairs.
  - Pod root: `kube_pod_info` / `kube_pod_owner` of the roots are issued with the seed.
  - The claim families, mounter completion, the application-metadata read, the node families and the root pods' controllers no longer wait for mounter completion: it adds no claim, and a mounter shares its claim's namespace.
  - Co-mounters' pods, controllers and nodes are read only for mounters no earlier read loaded.
- **Controller second stage read ahead.** `kube_deployment_annotations` / `kube_cronjob_annotations` are issued with the first stage on names derived from it (a ReplicaSet / Job name without its final `-` segment, plus direct owners), followed by a read of whatever names the first stage resolves that the early read missed.
- **Unchanged:** bodies stay byte-identical; seeds, root-set caps, zone routing, fail-closed and the element set are untouched. Every query keeps its selector-only shape — no PromQL binary-operator join — so the endpoint stays portable across Prometheus-compatible stores.

Expected sequential upstream rounds:

| Root | Today | After |
|---|---|---|
| `node`, every tracked claim mounted only by root-node pods | 7 | 3 |
| `node`, a tracked claim with a co-mounter elsewhere | 7 | 5 (6 when the early controller read misses) |
| `pod` mounting claims with no co-mounter | 5 | 2 |
| `pod` mounting nothing | 4 | 2 |

Non-goals:

- Changing `/v1/storage-graph`.
- PromQL `and on(...)` joins. They narrow the left-hand read only through VictoriaMetrics' label-filter pushdown, which stops at 100 values per label. On Prometheus they read the left-hand family across the whole zone.
- New configuration, wire or self-metric changes.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `k8s-graph-api`: "Reads by reference, Kubernetes families only" — the Kubernetes-only build's read grouping, read order, co-mounter reads and early controller second stage; `/v1/storage-graph`'s reads are stated as unchanged.

## Impact

- `pkg/build`: the k8s plan's read path, gated on `topologyPlan.kubernetesOnly` — `nodeseed.go`, `podseed.go`, `podscope.go`, `podnamespace.go`, `claimscope.go`, `expansion.go`, `controllerscope.go`, `nodescope.go`, `appmetadata_scope.go`, `topology_read.go`.
- `pkg/promql`: a `(namespace, name)` pair renderer and chunker, generalising `RenderClaimBindingsByPod`.
- Tests: `TestBuildK8s_FanOutIsTheStoragePlansKubernetesHalf` pins the k8s fan-out to the storage fan-out and is replaced. Add tests for the round count, the storage fan-out staying unchanged, and the early controller read. Extend `TestBuildK8s_PlanIsOutputPreserving`.
- Docs: the `/v1/k8s-graph` fan-out section of `docs/upstream-metrics.md`, the k8s section of `.claude/rules/storage-graph.md`, the request lifecycle in `CLAUDE.md`.
- No API, wire, configuration, mock-interface or dependency change. Self-metric names are unchanged. A k8s build's per-family series tally now also counts the cross-pair rows a grouped restriction matched, as the existing "every series a restriction matched" definition implies.
