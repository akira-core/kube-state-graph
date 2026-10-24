# Design

## Context

See proposal.md for motivation. Today `k8sPlan` is `storagePlan` with Harvest switched off, so a `/v1/k8s-graph` build runs the storage build's goroutine graph in `readTopology`. Every second wave waits on a channel that closes only when the wave before it has fully returned:

```
node root today (each line waits on the one above)
r1  kube_pod_info{node=~R}                         readNodeSeed
r2  kube_pod_info  per namespace (incarnation)      readNodeSeed
r3  bindings       per namespace (by pod)           readNodeSeed
r4  bindings       per namespace (by claim)         readMountersOf  -> closes bindingsDone
r5  pod_info/pod_owner per namespace || claim families per namespace
                                                    readScopedPods  -> closes podsDone
r6  kube_node_* || controller stage A || app_metadata
r7  controller stage B (Deployment / CronJob annotations)
```

Two facts make most of that chain unnecessary:

- **Mounter completion never adds a claim.** `readMountersOf` keeps only rows whose claim is already tracked, so the tracked set is final once the root pods' bindings are filtered.
- **A mounter shares its claim's namespace and cluster**, because a pod can mount only a claim of its own namespace. So the application-metadata scope is fixed by the root pods alone.

The node root's `RootNodePods` path is the existing precedent for k8s-only behaviour gated on `topologyPlan.kubernetesOnly`.

## Goals / Non-Goals

**Goals:**

- Cut the critical path to the rounds in proposal.md's table.
- Cut the query count by grouping namespaces.
- Keep every `/v1/k8s-graph` body byte-identical.

**Non-Goals:**

- Any change to a `storagePlan` build: its queries, their order and its bodies.
- Any change to the seeds' first reads or the root-set cap.
- Any change to the `/v1/graph` first wave.

## Decisions

### D1. Everything is gated on `kubernetesOnly`; the storage path is not touched

Every new behaviour runs only when the plan is `kubernetesOnly`, and the storage readers are not edited.

- **Pinning:** a new test records the issued query strings of storage `pod` and `node` builds over `planEstate()` and asserts they are unchanged. `TestBuildStorage_PlanIsOutputPreserving` keeps pinning the storage bodies.
- **Alternative: change both endpoints.** Rejected. The storage graph reads only claim-mounting pods and is not slow. Changing it would rewrite a large part of `storage-graph-api` for no measured gain.

### D2. A dedicated k8s workload reader runs the rounds explicitly

Under `kubernetesOnly`, `readTopology` launches one reader, `readK8sWorkload`, in place of:

- the node or pod seed;
- `readWorkloadClaims`;
- `readScopedPods`, `readScopedNodes`, `readScopedControllers` and `readScopedAppMetadata`.

It runs the rounds as explicit errgroups and writes the same `topologyVectors` slots, so `parseTopology`, `assemble` and `freeze` are untouched. It closes `bindingsDone`, `pvcInfoDone` and `pvcAnnotationsDone` itself, so the scoped QoS read still computes an empty scope and returns. ALERTS stays in the first wave, and `refuseHarvest` stays.

It is built from the storage path's helpers, not copies of them:

- pod and claim selection: `podsNewestOn`, `keepPodBindings`, `keepRootBindings`, `trackedClaimKeys`;
- scope computation: `controllerScope`, `deploymentScope`, `cronJobScope`, `nodeScope`, `appMetadataScopes`;
- query issue: `issueScopedFamilies`, `issueClaimKeyed`.

So what a pod, claim or controller *is* cannot drift between the endpoints; only the order of the reads differs.

```
node root (k8s)
r1  seed kube_pod_info{node=~R}                                   [+ ALERTS]
r2  on seed pairs, grouped: incarnation || bindings || pod_owner
      Go: pair filter -> podsNewestOn -> root-node pods, tracked claims, root owners
r3  mounter completion || claim families || app_metadata(root pod rows)
    || kube_node_*(root pod rows + roots) || controller stage A + early B (root owners)
r4  only for new mounters: pod_info || pod_owner     || stage-B fallback (root owners)
r5  only for new mounters: stage A + early B || kube_node_* (names not yet read)
r6  only on an early-B miss: stage-B fallback

pod root (k8s)
r1  bindings seed || pod_info + pod_owner of the roots (same chunks as the seed)   [+ ALERTS]
r2  mounter completion || claim families || app_metadata(root pod rows + tracked claims)
    || kube_node_*(root pod rows) || controller stage A + early B (root owners)
r3+ as node root r4..r6
```

- **Alternative: add `kubernetesOnly` branches inside each existing reader and re-wire the signal channels.** Rejected. Early starts would cut across five functions, and each channel has exactly one closer today; moving closers conditionally invites double-close panics and leaves storage readers carrying k8s branches.

### D3. Grouped `(namespace, name)` restriction with a Go pair filter

Under `kubernetesOnly`, every read of known pods or claims is issued through a pair renderer instead of `RenderNamesInNamespace`. The renderer generalises `RenderClaimBindingsByPod` to any family and identity label:

```
<family>{<fixed selector>, <request matchers>, namespace=~"a|b", <label>=~"x|y"}
```

- **Chunking:** the pair chunker sorts pairs by `(namespace, name)` and packs them greedily. Each chunk is charged the request-matcher cost, its own namespace alternation and its own name alternation. A single pair is always issued. Sorting by namespace first keeps each chunk's namespace set small, which also limits cross pairs.
- **Filtering:** each read's rows are filtered to the requested pairs before anything else reads them, the way `keepRootBindings` already does for the pod seed. The node root's full `podSeriesKey` filter and `issueClaimKeyed`'s `claimKey` filter apply on top, unchanged.
- **Tally:** keeps counting every series a restriction matched, cross pairs included.
- **Pod-root cap:** the root pods' `kube_pod_info` / `kube_pod_owner` read reuses the pod seed's chunk split, so `preparePodSeed`'s cap still bounds every request-derived read and no request is rejected after a query.
- **Alternative: keep one query per namespace.** Rejected. A node root's pods span many namespaces, so the query count is `families × namespaces`.

### D4. The application-metadata scope comes from the root pods and tracked claims

The k8s reader computes the read's per-cluster namespace sets from the root pods' `kube_pod_info` rows and the tracked claim keys, rather than from the loaded pods and claim-info rows after the pod wave.

**Node root:** the two sets are equal.
- Every tracked claim is bound by a root-node pod in its own `(cluster, namespace)`.
- Every co-mounter shares its claim's `(cluster, namespace)`.

**Pod root:** the new set is a superset. It is strictly larger only when a tracked claim's `(cluster, namespace)` holds neither:
- a root pod row,
- a co-mounter row, nor
- a claim-info row.

In that case nothing in that namespace is drawn and no mounter there resolves an Application, so the body is unchanged.

### D5. Co-mounters are read as a delta

After mounter completion, the reader computes the new mounters: the binding pod keys not already loaded.

- **Pod families:** it reads `kube_pod_info` and `kube_pod_owner` for the new mounters only.
- **Controllers:** it reads the controller families only for owner names not already read, per family.
- **Nodes:** it reads `kube_node_*` only for nodes not already read.

Results are appended to the slots in round order. The parse is keyed, so cross-key row order cannot change the body. A new test proves this by shuffling each vector's order across keys and asserting a byte-identical body. All rows of one key still come from one query, because a pod, claim, controller or node name lands in exactly one chunk of one round.

### D6. The controller second stage is read early, then completed

Stage A's scope is known from `kube_pod_owner`. In the same round the reader issues `kube_deployment_annotations` / `kube_cronjob_annotations` on candidate names:

- each ReplicaSet / Job name with its final `-` segment removed;
- the direct Deployment / CronJob owner names.

The naming is guaranteed by the controllers that create those objects:
- the Deployment controller names a ReplicaSet `<deployment>-<pod-template-hash>`;
- the CronJob controller names a Job `<cronjob>-<scheduled-time>`.

So only hand-made objects miss, for example `kubectl create job --from=cronjob/x`.

When stage A returns, `deploymentScope` / `cronJobScope` give the true names. The difference from the candidates is read in the next round and its rows appended. A wrong candidate either:
- matches nothing (the fixed `tracking_id!=""` selector drops most), or
- returns a row no loaded pod's owner chain consults.

Either way the body is unchanged, and the tally counts the row.

- **Alternative: wait for stage A.** This is today's behaviour and costs one full round on every Deployment- or CronJob-owned pod.
- **Alternative: read `pod-template-hash` from `kube_pod_labels`.** Rejected. It requires an allowlist the operator may not set.

### D7. No PromQL joins, no capability gate

`<family> and on(az,env,cluster,namespace,pod) <root-anchored read>` would collapse rounds. It was evaluated and rejected.

**VictoriaMetrics.** v1.107.0 and v1.150.0 both evaluate an `and` right-hand side first and push its common label values into the left as filters.
- On a single-store 3-node estate, `pod_owner and on(...) pod_info{node=...}` fetched 26 series instead of 54.
- The pushdown stops at 100 values per label: hardcoded in v1.107; `-search.maxBinaryOpPushdownLabelValues` in v1.150, default 100. A synthetic 3450-pod store showed:

| Seed pods | Namespaces | Series fetched | What was pushed down |
|---|---|---|---|
| 50 | 5 | 100 | everything |
| 150 | 30 | 1800 | the namespace filter only |
| 150 | 120 | 3600 | nothing (whole cluster) |

**Prometheus.** It evaluates both operands independently (`rangeEval(..., e.LHS, e.RHS)`), so the left family is read across the zone. A large estate would hit a sample limit and, failing closed, return 502.

**Capability gate.** A per-backend capability flag would keep joins on VictoriaMetrics only. It would cost two read plans and a configuration surface. Rejected in favour of portable, selector-only queries.

## Risks / Trade-offs

- **[The k8s orchestration diverges from the storage build's]** → Both endpoints share the semantic helpers (D2). `TestBuildK8s_PlanIsOutputPreserving` compares every k8s case against an unrestricted reference read, not against the storage path. New cases cover a co-mounter, a rescheduled pod, a cross pair and an early-B miss.
- **[Cross-pair overfetch when one pod name repeats across many namespaces]** (a StatefulSet `postgres-0` in 50 namespaces) → Bounded by the chunk budget. The `(namespace, name)` sort keeps chunks namespace-local. Every extra row is dropped in Go before any scope is computed.
- **[More reads in flight in one round]** (r3 starts up to five waves at once) → The per-store guard still caps concurrency at 32 slots, and grouping lowers the total query count.
- **[`TestBuildK8s_FanOutIsTheStoragePlansKubernetesHalf` encodes the old invariant]** (k8s fan-out equals storage fan-out for pod roots) → Replace it with explicit k8s fan-out assertions (D1, D3) and the storage fan-out pin (D1).
- **[Latency gain is structural, not measured]** → A lockstep test querier releases queries one round at a time and asserts the round counts in the spec. After deployment, compare `kube-state-graph.build_k8s` span durations before and after on a real estate.

## Migration Plan

No configuration, wire or data migration. Deploy as an ordinary release. Roll back by reverting. The change touches only the in-process read order of `/v1/k8s-graph`.
