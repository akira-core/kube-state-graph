## Context

D29 connection-string resolution (`pkg/build/servicegraph.go`) turns a `"://"` endpoint into graph nodes/edges. Today, for an endpoint addressing `(namespace, service)`, `resolveServiceLevel` looks up `svcCandidates[famSvcKey{family, ns, svc}]` — every cluster in the anchor's family that holds the Service — and materialises **one service node per candidate cluster**, each fanning out `service-selects-pod` edges to its **own** cluster's backing pods. The caller then gets one `pod-calls-service` edge per candidate (a cross product when both sides are `"://"`). Two refinements layer on top: an **unknown-family fallback** (resolve across all loaded clusters when the anchor names no loaded family, unique-family-holder wins) and **endpoint-backed pruning** (drop candidates provably without backing pods).

This is the inverse of how a multi-cluster mesh actually routes. In Istio multi-primary and Cilium Cluster Mesh, the Service object exists in **every** cluster under the **same** `.svc` DNS name (no rename). A pod can only dial `web.shop.svc.cluster.local` because its **own** cluster holds that Service; the mesh then aggregates **endpoints** — the call may land on backing pods in sibling clusters. Critically, each cluster's kube-state-metrics only sees its **own** EndpointSlices (the mesh programs remote endpoints into the dataplane, not into local EndpointSlice objects), so reconstructing the cross-cluster endpoint set requires unioning `EndpointsByService` across the family siblings that hold the same-named Service.

Constraints carried from the existing design: determinism (byte-identical body for identical upstream data — D6); no filters pushed to PromQL (resolution stays in-memory); strict `labels` (no booleans/numbers — cross-cluster status is derived, never a label); edge IDs are UUIDv5 over `<type>|<source>|<target>`; `/v1/edge-types` reads only the in-code `graph.EdgeTypes` registry.

## Goals / Non-Goals

**Goals:**
- `pod-calls-service` resolves to exactly **one** service node, in the **caller's own (anchor) cluster** — never cross-cluster.
- `service-selects-pod` fans out from that one local node to backing pods across **every family-sibling cluster that also holds the same-named Service object** — may be cross-cluster.
- An anchor cluster that does not itself hold the same-named Service falls back to `external` — one membership test that also covers `""`/`"unknown"`/bogus anchors, while preserving the legitimate fully-unlabelled (`"unknown"` family-of-one) case.
- Remove the now-unreachable unknown-family fallback and endpoint-backed pruning machinery; keep `clusterFamilyKey` and `svcCandidates`.
- Preserve determinism and the "no PromQL filtering" contract.

**Non-Goals:**
- No change to pod-UID resolution, the missing-UID human-label fallback (D27), the self-loop UID guard (D33), or the sentinel-endpoint exclusion (D30).
- No new node or edge type; no new config knob/flag.
- No change to `clusterFamilyKey` semantics or to the topology indexes (`ServicesByNameNS`, `EndpointsByService`).
- No numeric/typed edge attributes (still deferred).

## Decisions

### D-L1: Anchor cluster is the caller's cluster; resolution is single-node, local-only
`resolveServiceLevel(anchorCluster, ns, svc)` returns at most one service-node ID:

1. `cands := svcCandidates[famSvcKey{clusterFamilyKey(anchorCluster), ns, svc}]` (already sorted by cluster at build).
2. Find the candidate whose `cluster == anchorCluster`. If none → return nil (caller falls back to `external`). This **single membership test** subsumes every "no local Service" case — an anchor whose own cluster lacks the Service (a sibling holding it is not enough), an `"unknown"` anchor with no `"unknown"`-bucketed holder, a bogus trace label, and an empty anchor — because none of them is a holder in its own family. It also **preserves** the fully-unlabelled single-cluster case: `clusterFamilyKey("unknown") = "unknown"` is a family-of-one, so an `"unknown"`-bucketed Service makes `"unknown"` a legitimate holder and resolution stays inside the pseudo-cluster. No special-casing of `"unknown"`/`""` is needed.
3. Materialise **one** service node at `anchorCluster` (idempotent), using that candidate's `ServiceObs` (so `cluster_ip` / headless status is the local cluster's).
4. For **every** candidate in `cands` (all same-family clusters holding the Service), union its `EndpointsByService[{cand.cluster, ns, svc}]` and emit a deduped `service-selects-pod` edge from the local service node to each backing pod. These edges may cross cluster boundaries. (The `"unknown"` family-of-one fans out only to `"unknown"`-bucketed endpoints; a real family never unions the `"unknown"` bucket, since `"unknown" ≠ "prod-0"`.)
5. Return `[localSvcID]`.

The anchor threading is **unchanged** from today: the server-side `"://"` anchor is the UID-recovered client-pod cluster (falling back to the raw trace label), and the client-side `"://"` anchor is the trace label. Only the meaning of "what we do with the family candidates" changes — from "one node each" to "one local node + cross-cluster endpoint union".

*Alternative considered — keep the cross-family node fan-out but reverse only the edges:* rejected. It would leave N service nodes the caller never dialled. The single-local-node model matches the mesh's "one logical VIP, aggregated endpoints" reality and is strictly simpler.

*Alternative considered — synthesise a virtual local service node when the anchor lacks the Service (mesh "imported" service):* rejected during brainstorming. A healthy multi-primary/global-service mesh requires the same-named Service in the caller's cluster for the call to route at all; absence is an anomaly or a genuinely external target, so `external` is correct and we never fabricate a node without a backing `kube_service_info` row.

*Alternative considered — explicitly externalise every `"unknown"`/`""` anchor:* rejected as both unnecessary and a regression. The membership test in step 2 already externalises an `"unknown"` anchor that names no `"unknown"`-bucketed holder, while still resolving the legitimate fully-unlabelled single-cluster deployment within its `"unknown"` family-of-one. A blanket `"unknown" → external` rule would needlessly break that common single-cluster install.

### D-L2: Drop the unknown-family fallback and endpoint-backed pruning
Both mechanisms only existed to choose which cluster(s) get a service NODE in the cross-family fan-out. With resolution pinned to the anchor cluster:
- The unknown-family fallback (cross-family guessing for an unanchorable anchor) contradicts the same-cluster rule — an unknown anchor now goes straight to `external`.
- Endpoint-backed pruning is unnecessary: a sibling holding the Service but with zero local endpoints simply contributes no `service-selects-pod` edges; the local node still materialises on Service-object presence. The "known service, zero endpoints anywhere, node still materialises (operator signal)" behaviour is preserved as a free consequence.

Removed: `sgResolver` fields `svcHolderFamilies`, `knownFamilies`, `epVisibleClusters`; types `holderFamily`, `nsSvcKey`; functions `loadedUniqueFamilyHolders`, `endpointBacked`; the `parseServiceGraph` build loops for `svcHolderFamilies`/`knownFamilies`/`epVisibleClusters`. Retained: `svcCandidates` (now the endpoint-union candidate set) and `clusterFamilyKey`.

### D-L3: `materializeService` split
Split into (a) `materializeServiceNode(cluster, ns, svc, obs) string` — idempotent node creation only, and (b) the cross-cluster endpoint fan-out, driven by `resolveServiceLevel` over the candidate set. This keeps node identity/IP local while letting the fan-out span siblings. `addServiceEdge` (deduped by `"svcID|podID"`) is reused unchanged; cross-cluster targets need no special casing because the edge stores raw node IDs and final ordering goes through `SortEdges`.

### D-L4: Cross-cluster flags swap (registry)
In `pkg/graph/registry.go`, the two cross-cluster flags **swap**:
- `EdgeTypeServiceSelectsPod.MayCrossCluster`: `false → true` (a local service node now fans out to backing pods in family siblings).
- `EdgeTypePodCallsService.MayCrossCluster`: `true → false` — a `pod-calls-service` edge now always connects a pod (or both-`"://"` service/external source) to a service node in the **anchor's own** cluster, so source and target clusters always agree; it can no longer cross. This is the registry-level embodiment of "pod→svc is same-cluster only".

The registry-driven `neverCrossCluster` gate in `pkg/graph/graph.go` then automatically: routes `service-selects-pod` through `isCrossCluster` (source/target node `cluster` comparison) and SKIPS the per-edge lookup for `pod-calls-service` (now declared intra). No code change in `graph.go`. Rewrite the `pod-calls-service` (single local node, no per-family fan-out, intra-cluster), `service-selects-pod` (may-cross-cluster endpoint union), and `pod-calls-pod` (drop unknown-family-fallback wording) descriptions for accuracy; `/v1/edge-types` reflects the new contract automatically.

### D-L5: Determinism
`svcCandidates` is sorted by cluster at build; the endpoint union iterates candidates in that order; `service-selects-pod` edges are deduped in a map and the final edge slice is canonicalised by `SortEdges`; the single local service node is a deterministic function of `(anchorCluster, ns, svc)`. No map-iteration order reaches the output. Edge IDs remain UUIDv5 over `<type>|<source>|<target>`.

## Risks / Trade-offs

- **[Wire-format change breaks golden tests and existing consumers]** → Expected and intended; regenerate goldens with `-update`, and document the body change in CLAUDE.md / README. This is a deliberate v1 behaviour correction, not an additive change.
- **[Family heuristic may union endpoints across two clusters that share a Service name + family but are NOT actually meshed]** → Same family-key assumption the current code already relies on, now applied to endpoints instead of nodes; further constrained by requiring the same-named Service object on both sides. Accepted as the established heuristic; no new risk surface.
- **[Cross-cluster `service-selects-pod` could surprise `?cluster=` consumers]** → Edges whose target pod is out of the filtered scope are pruned by the existing orphan-edge logic in `graph.Project`; compound nesting (`cluster > service`, `cluster > node > pod`) is unaffected. Verified against the projection contract; no code change needed there.
- **[Dropping the unknown-family fallback de-resolves some unanchorable series]** → A series whose anchor names no holding cluster in its own family — a non-pod client side with a missing/`"unknown"`/bogus trace label, where the addressed service is held only by some *other* family — previously resolved via the cross-family unique-holder fallback and now resolves to `external`. This is the intended same-cluster correction: without a caller cluster that holds the Service, there is no in-cluster service to point at. **Not affected:** the fully-unlabelled single-cluster deployment (everything in the `"unknown"` family-of-one) still resolves, because its `"unknown"` anchor IS a holder of its `"unknown"`-bucketed Service.

## Migration Plan

Single in-process behaviour change, no data migration. Deploy = ship the new binary; rollback = redeploy the prior binary (no persisted state, no schema). Coordinate OpenSpec archival with the active `node-internal-ip-fallback` change — they modify **different** requirements (`pod-service-graph`/`graph-api` here vs. "Topology series consumed"/prefix there), so no MODIFIED-requirement clobber is expected; archive order is otherwise free.

## Open Questions

None — the anchor-unknown → `external` and anchor-lacks-Service → `external` decisions were resolved during brainstorming.
