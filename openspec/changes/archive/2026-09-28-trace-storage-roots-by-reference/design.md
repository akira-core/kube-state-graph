# Design

## Context

See proposal.md — Why. The current storage read is `storagePlan` (`pkg/build/topologyplan.go`):

- five families are skipped outright; the pod, Kubernetes-node and controller families are read by reference in three waves (`podscope.go`, `nodescope.go`, `controllerscope.go`), all ultimately scoped from the claim-binding family;
- every other family — the five claim-keyed families, `volume_labels`, the twelve Harvest gauge / policy families, the kubelet pair and `ALERTS` — is a first-wave leg read across the whole requested `az` / `env` (18 first-wave legs);
- a storage-exclusive root switches the build into hub mode (`volumelabelscope.go`, `claimscope.go`): `volume_labels` is read restricted in phases and the claim families are read from the rooted rows (13 first-wave legs). Past a chunk cap, both the rooted read and the hub's claim reads fall back to an unrestricted read;
- an `application=` root adds a three-stage recovery wave (`appscope.go`) that feeds the pod scope, but the claim families and `volume_labels` stay zone-wide;
- `ProjectStorage` (`pkg/graph/project_storage.go`) ANDs three selector groups (storage-exclusive, workload-exclusive, `node=`), matches `node=` on two tiers, and narrows `aggr=` / `svm=` by `ontap_cluster=`.

Readers that decide output from the WHOLE population they are handed, and so constrain any narrower read:

| Reader | Rule | Location |
|---|---|---|
| `pickOwner` | an aggregate's controller = lexically-smallest `node` over every `volume_labels` row of that aggregate; the `aggr_*` gauge `node` only when no row names one | `netapp.go:661`, `:494` |
| `pickAggr` / `pickSVM` | a claim's aggregate / SVM = lexically-smallest over every candidate its token matches | `netapp.go` |
| canonical pod | the newest incarnation of a `(cluster, namespace, pod)` wins, including its node | `topology.go:1029` |
| claim binding | a binding exists only if its pod was loaded | `topology.go:1076` |
| flow weight | a unit's share is the claim's IO ÷ the claim's mounter count in the BUILT graph | `project_storage.go` `scaleFlow` |
| PVC Application inheritance | lexically-smallest Application among the claim's mounting pods | `topology.go` `pvcInheritedApps` |

## Goals / Non-Goals

**Goals:**

- Every family a storage build reads is scoped by something the root leads to, except `ALERTS`.
- For any single-kind root, the body is BYTE-IDENTICAL to the body an unrestricted read of the same zone would produce under the same projection. That parity is the test oracle.
- One read architecture for all seven root kinds: a per-kind seed plus one shared expansion.

**Non-Goals:**

- `/v1/graph`'s read (`fullPlan`) and its request contract. Its claim join follows D14 like the storage build's, which is the one `/v1/graph` behaviour this change touches.
- The tier chain, the flow weights, serialisation, determinism and the fail-closed rule.
- Scoping `ALERTS` (see D11).
- Redesigning the query-result cache or chunking for cross-request reuse.
- Collapsing same-store hops into PromQL joins (see D5).

## Decisions

### D1. One root kind per request, enforced at parse time

`graph.StorageRoots` becomes a kind plus its values — `Kind` (`ontap_cluster`, `ontap_node`, `aggr`, `svm`, `node`, `pod`, `application`) and one sorted, de-duplicated value set (pods as `PodRef`) — instead of six independent sets. `kubegraph.ParseStorageValues` counts the root parameters carrying a non-empty value: zero → 400 `missing_root`; more than one → 400 `invalid_scope` naming the parameters.

- *Alternative:* keep six sets and validate them — rejected, because every downstream consumer would still have to handle a state the parser forbids.
- *Alternative:* one endpoint per kind — rejected, because it multiplies routes, OpenAPI entries and front-door proxying for no gain.

### D2. `node=` is Kubernetes-only; `ontap_node=` is new

The root's tier is known from the parameter, so the one-kind rule holds at parse time and no build has to classify a name.

- *Alternative:* keep `node=` and classify each value by running both seeds in parallel — rejected, because a request mixing tiers would only surface AFTER upstream reads, needing a new `build.Reason` for what is a request-shape error.

### D3. Bare `aggr=` / `svm=` span every filer; `ontap_cluster=` stands alone

An `aggr=aggr1` root roots `aggr1` on every filer the zone's Harvest backends return, as it does today without `ontap_cluster=`. Qualification is dropped rather than moved into the value. A `<oc>/<aggr>` value form remains a possible later ADDITIVE change if cross-filer collisions prove to matter.

### D4. The claim set is the hub

Every retained path is a `(claim, mounting pod)` unit, and the claim is the one entity both stores reference: the PV name in `kube_persistentvolumeclaim_info` and the FlexVol token in `volume_labels`. Every seed therefore converges on a claim set `C`, and one shared expansion `E(C)` walks `C` out to both ends:

```
 ontap_cluster / aggr / svm / ontap_node            node / pod / application
               │ seed (Harvest store)                        │ seed (KSM store)
               ▼                                             ▼
      volume_labels rows ──pvCandidates──► C ◄──bindings{pod}── pods
                                           │
                                  E(C) + completions ①–④
                                           │
                               ProjectStorage (single kind)
```

The read is a SUPERSET of what the body draws (completions pull in extra mounters, aggregates and clones). `ProjectStorage` stays the sole judge of what is drawn.

### D5. Hops are client-side, one keyed read each

Each hop is one scoped family read rendered like `promql.RenderScoped` (fixed selector, then the request matchers, then one anchored alternation on the key label), chunked by the shared byte budget, issued under `scopeConcurrency`, and merged in chunk order.

- *Alternative:* collapse same-store hops into a filtering join (`bindings and on(namespace,pod) kube_pod_info{node=~…}`) to save rounds — rejected. The join's outer selector is still evaluated over the whole zone upstream, which re-spends exactly the series and CPU this change removes. Only a filter on a label inside `on()` could be pushed down, and none of these hops filters on one.

### D6. Seeds

| Kind | Seed reads (in dependency order) | Yields `C` from | Flowless-root reads |
|---|---|---|---|
| `ontap_cluster` | `volume_labels{cluster=~OC}` | `pvCandidates` → `pvc_info{volumename}` | `aggr_*` / `node_*{cluster=~OC}` |
| `aggr` | `volume_labels{aggr=~A}` (each aggregate whole) | same | `aggr_*{aggr=~A}` |
| `svm` | `volume_labels{svm=~S}` → owner completion ① | same | none (an SVM exists only in `volume_labels`) |
| `ontap_node` | `volume_labels{node=~N}` → every touched `(cluster, aggr)` re-read whole (①) | rows of aggregates whose `pickOwner` ∈ N | `node_*{node=~N}` |
| `node` | `kube_pod_info{node=~N}` → incarnation completion ② by `(namespace, pod)` → `bindings` by `(namespace, pod)` for pods whose canonical node ∈ N | those bindings → `pvc_info{claim}` | `kube_node_*{node=~N}` |
| `pod` | `bindings{namespace=~,pod=~}` filtered to the `(namespace, pod)` refs ‖ `kube_pod_info{pod=~}` | same | `kube_pod_info{pod}` (already read) |
| `application` | recovery (`appscope.go`, three stages) → `(cluster, namespace, pod)` → `bindings` by `(namespace, pod)`; ‖ `pvc_annotations{tracking-id=~app}` | union of both halves | none (an Application is never materialised) |

**Closure (why the seed never misses a retained claim):**

- *Storage kinds.* A claim is retained only if its picked aggregate / SVM / controller is a root. That requires at least one of its candidates to sit on a rooted component, so the rooted rows contain it. This is the existing hub argument.
- *`ontap_node`.* The owner vote picks one of the aggregate's own row values, so every aggregate N owns has a row naming N and is found by the `{node}` read.
- *`node` and `pod`.* A retained unit's pod is a root or sits on one, so its bindings name the claim.
- *`application`.* A retained unit carries the Application on its pod — reached through recovery — or on its claim: either its own annotation, reached by the tracking-id read, or inherited from a mounting pod, which is again a recovered pod.

The pod seed replaces `deriveStorageNamespaces`: it is keyed by `(namespace, pod)` directly. `RenderTrackingIDScoped` is extended to `kube_persistentvolumeclaim_annotations`, which carries the same tracking-id label.

### D7. Shared expansion E(C) and its four completions

Once `C` is known (with each claim's `volumename`), the expansion issues, as dependency edges inside one errgroup rather than as barriers:

- **Claim side.** `pvc_info` (if the seed did not already read it), `pvc_annotations`, kubelet ×2 and **③ mounter completion** `bindings{claim}`, all keyed on `C`. Then `kube_pod_info` + `kube_pod_owner` for EVERY mounter, by `(namespace, pod)` (D15).
- **Workload side.** Then `kube_node_*` for the mounters' nodes ∪ `node` roots, and the two controller stages for their owners — the existing waves, now scoped from all mounters.
- **Storage side.** **④ candidate completion**: `volume_labels{volume=~token}` for `C`, which is the existing phase 2, plus the existing svm-root completion of aggregates only phase 2 named. Then **① owner completion**: every touched aggregate re-read whole. Then `aggr_*{cluster, aggr}` and `node_*{cluster, controller}` for the aggregates and owners reached ∪ the flowless-root reads. In parallel with that, QoS ×6 `{volume}` (existing) and `qos_policy_fixed{cluster, svm}` for the claims' SVMs.

| # | Completion | Preserves | Failure without it |
|---|---|---|---|
| ① | owner | `pickOwner` over every row | a takeover aggregate is drawn under the wrong controller |
| ② | incarnation (`node` seed only — every other pod read names pods the build already placed) | canonical newest incarnation | a StatefulSet pod rescheduled from N to Y within the window is drawn on N |
| ③ | mounter | binding needs a loaded pod; weight ÷ built-graph `n`; PVC Application inheritance | a claim with three mounters on three nodes shows its full IO on one node's path, and an unannotated claim inherits the wrong Application |
| ④ | candidate | `pickAggr` / `pickSVM` over the full candidate set | a clone or a same-named FlexVol on another filer moves a claim onto or off the rooted aggregate |

- ③ reads other nodes' pods and nodes that the projection then drops. That cost is bounded by the claims' mounter count, never by the zone.
- The QoS-policy read keys on `(cluster, svm)` rather than the policy name. The name is only known after the QoS workload read, and keying on it would add a round for a family whose per-SVM cardinality is small.

### D8. Multi-label Harvest keys render per ONTAP cluster

`aggr_*`, `node_*`, the owner completion and `qos_policy_fixed` are keyed by `(cluster, x)`. They render one query per ONTAP cluster, `{cluster="oc", x=~"a|b"}`, chunked by the byte budget, exactly as `RenderVolumeLabelsOwnerCompletion` does today. There is no cross-cluster cross product, so no row outside the key set is read.

### D9. Storage-kind seeds absorb the volume-hub phases unconditionally

Phase 1 is now always issued — there is no hub on/off decision. It is one group per request because a request carries one kind:

- `{cluster=~OC}` for `ontap_cluster`
- `{aggr=~A}` for `aggr`, with no cluster matcher
- `{svm=~S}` for `svm`

The composition rules (union of aggregate and SVM groups, the `cluster!~` exclusion for cluster-only phase 2, the `application` / `pod` / `node` composition) are removed. The "mode cannot render a token, read unrestricted" branch goes away with D14.

### D10. Caps: request-derived scopes reject, data-derived scopes chunk

- A root value set whose seed renders into more than the cap's chunks — the existing `maxRootedVolumeLabelChunks` = `scopeConcurrency` rule, applied to every kind's seed — is rejected 400 `invalid_scope` BEFORE any query is issued. This needs no upstream data because the seed's render is a pure function of the values and the byte budget.
- Data-derived scopes (`C`, mounters, touched aggregates, controllers) are chunked without a ceiling and issued under `scopeConcurrency`. `maxHubClaimChunks`' fallback to an unrestricted read is removed, because an unrestricted read is the read a series limit rejects first and no unrestricted path remains.

### D11. `ALERTS` stays zone-wide

Its identity labels differ per alert (`pod`, `persistentvolumeclaim`, `node`, `aggr`, or namespace-only). Scoping it would need one query per identity label, for a family whose cardinality is the number of firing alerts. It is optional on this endpoint (`failsClosed` exempts it), so its read cannot fail a build.

### D12. Projection is single-kind reachability

`ProjectStorage` resolves the one kind's values to ids:

| Kind | Resolves to |
|---|---|
| `ontap_cluster` | every NetApp entity of the filer |
| `ontap_node` | the controller |
| `aggr` / `svm` | that name on every filer |
| `node` | the Kubernetes node in every cluster |
| `pod` | the pod ref |
| `application` | pods (materialised) and claim hits (retention only) |

A unit is retained iff it intersects those ids and passes the `cluster` / `namespace` filters. Resolved roots are admitted flowless as today (workload roots still honour the filters). `pullNetAppParents` is unchanged. The three-group AND, the `node=` two-tier match and `inOC` are deleted.

### D13. Plan shape

`topologyPlan` for storage carries the root kind and values. The first-wave table (`topologyLegs`) remains `/v1/graph`'s. The storage plan launches only `ALERTS` in its first wave, and everything else hangs off the seed. `failClosed` is unchanged. The per-family series tally keeps its rule: a family that was not issued is absent, never zero.

### D14. The join is suffix-only, on both endpoints

Tracking needs two properties from the join.

- **Forward, as PromQL** — candidate completion ④ and the QoS scope. A claim's token must render as one anchored alternation branch, `.*<token>`.
- **Backward, as a generator** — the storage seeds. A FlexVol name must yield the PersistentVolume names it can embed (`pvCandidates`: every `pvc_` boundary suffix with `_` → `-`), and that generator must find every claim the forward join would attach.

How each mode fares:

| Mode | Forward | Backward |
|---|---|---|
| `suffix` | ✓ | ✓ |
| `exact` | ✓ | ✓, but adds nothing: a FlexVol named exactly the token also ends with it |
| `contains` | could render `.*tok.*` | ✗ — it attaches `trident_pvc_x_clone` to claim `x`, while the generator derives `pvc-x-clone` and never reaches `x`, so a storage root silently misses the claim |
| `regex` | ✗ — the token is operator RE2 whose own anchors change meaning inside an anchored alternation | ✗ — not invertible |

So the join keeps only `suffix`:

- `VolumeMatchMode`, its constants, the mode argument of `NewVolumeKeyRewriter`, `volumeModeTokenScope` and the scanning match path are removed.
- The length-bucketed index serves suffix alone.
- The setting is deleted outright — flag, environment variable and `Config` field. No deployment sets it, so no tombstone is kept.
- The rewrite rules stay. A custom rule set still makes the backward generator find FEWER claims (never a wrong one), exactly as the volume hub documents today.

Alternatives considered:

- *Keep the modes on `/v1/graph` only* — rejected. Two join semantics would let the two endpoints disagree about which aggregate a claim sits on, which the storage body's `aggr` label and the `pvc-to-netapp-aggr` edge must never do.
- *Reject the storage route under the other modes* — rejected. The operator chose to drop the modes rather than carry a configuration the storage endpoint cannot serve.

### D15. Known pods are read by `(namespace, pod)`, one query per namespace

A pod name is unique within a namespace only. Every read of pods the build already knows is therefore keyed by the `(namespace, pod)` pair and issued as one query per namespace: `{namespace="ns", pod=~"a|b"}` beside the fixed selector and the request matchers (`promql.RenderPodsInNamespace`). That covers:

- the pod wave (mounters, `pod` roots, node-seeded and recovered pods);
- the `node` seed's incarnation completion;
- the claim-binding reads of node-seeded and recovered pods.

The namespace of each pair is the one carried by the row that named the pod, so no read is added. That row is a claim binding (a pod mounts only claims of its own namespace), a `pod=` root, a `kube_pod_info{node}` row or a recovering `kube_pod_owner` row.

A pod read keyed by name alone admits every same-named pod in the zone, and nothing filtered those rows:

- the node and controller waves, which scope from what the pod read returns, fetched those pods' nodes and controllers too;
- the application seed tracked a same-named pod's claims through the whole expansion.

The projection dropped all of it, so the body never changed, but the read grew with how common a pod name is (`postgres-0`, `web-0`). For `pod` roots, the namespace derivation this change removed had hidden the problem.

- *Alternative:* two independent alternations (`namespace=~"a|b",pod=~"x|y"`) plus a row filter — rejected. It reads every cross pair, and a large namespace set repeats a long alternation in every chunk.
- *Cost:* one query per namespace per family, chunked under the shared byte budget (the namespace equality is charged once per chunk) and issued under `scopeConcurrency`.
- *Exception:* the `pod` seed keeps its two-alternation binding read with an exact `(namespace, pod)` row filter. Its query count is the request-derived bound of D10, and one query per namespace would reject a root set that merely spans many namespaces. Its cross pairs are limited to the roots' own names and namespaces.

## Risks / Trade-offs

- **[Latency] Roughly two to three more sequential upstream rounds per request.** → Every hop is a dependency edge, not a barrier. Flowless-root reads and `ALERTS` run beside the seed. The build span records per-hop timings so a slow hop is attributable.
- **[Cache] Seed and expansion queries are root-specific, so the query-result cache rarely serves one root's reads to another.** → Accepted. The query cache still coalesces concurrent identical requests. Cross-root reuse (canonical per-entity chunking) is left for a later change.
- **[Silent break] An old client sending an ONTAP controller as `node=` gets an empty 200 — the withdrawn-parameter failure class.** → Recorded in `docs/BREAKING.md`. The frontend and demo changes land with the backend. The demo's `verify.sh` asserts an `ontap_node=` body is non-empty and that `node=<controller>` draws nothing.
- **[Parity] A completion bug changes the body without any error.** → A parity harness builds every kind twice — tracking plan versus an unrestricted read of the same fixtures, same projection — and requires byte-identical bodies. The fixture corpus covers takeover, clone, cross-filer FlexVol collision, an RWX claim across nodes, a pod rescheduled within the window, an inherited Application, a FlexGroup claim and a flowless root of every kind.
- **[Join] An estate that configured `contains` or `regex` changes its join on BOTH endpoints (D14).** → No known deployment sets a mode; `docs/BREAKING.md` still gives each mode's migration:
  - `exact` → no change;
  - `contains` → clone attribution is gone, by design;
  - `regex` → express the naming through the rewrite rules.

  Stock Trident estates already run `suffix`.
- **[Large roots] An `ontap_cluster=` root on a large filer reads that filer's whole claim population.** → Proportional to the root by definition. Chunked under `scopeConcurrency`, never zone-wide.
- **[Embedders] `graph.StorageRoots` changes shape.** → Called out as a breaking `pkg/` change. `graph-api-gateway` needs a matching change.

## Migration Plan

1. Merge this backend change. The storage route is breaking, and `/v1/graph` is untouched.
2. Frontend change: the Sankey sends exactly one root kind (switching kind clears the other roots), adds an `ontap_node` kind, and treats `missing_root` as "choose a root".
3. Demo change: bump both submodules together, update `verify.sh` §10/§11 root choices, and add the `ontap_node=` assertions.

**Rollback:** revert the backend image together with the frontend, since an old frontend can send mixed kinds, which the new backend rejects. The demo pins both submodules, so the rollback is one pointer move.

## Open Questions

- Whether each new hop gets its own byte budget or keeps sharing `--netapp-qos-scope-batch-bytes`. This is tunable later without changing any spec.
- The exact root-value cap — whether it stays at `scopeConcurrency` chunks or becomes a flag. It is tunable later; the spec only requires that exceeding it is a 400.
