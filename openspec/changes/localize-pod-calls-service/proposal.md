## Why

In a multi-cluster service mesh (Istio multi-primary, Cilium Cluster Mesh), the **same-named** Kubernetes Service exists in every cluster of a family — the `.svc` DNS name is identical and a pod can only dial it because its OWN cluster holds the Service. The cross-cluster behaviour is **endpoint aggregation**: the local Service's traffic may land on backing pods running in sibling clusters. The current D29 connection-string resolution models this backwards: it fans out one `pod-calls-service` edge to a service node **in every family cluster** (cross-cluster `pod-calls-service`), while each service node only selects its OWN cluster's pods (intra-cluster `service-selects-pod`). This invents service nodes the caller never dialled and never shows the real cross-cluster routing (a local service reaching a remote pod).

## What Changes

- **BREAKING (wire format):** `pod-calls-service` is **locked to the caller's own cluster**. A `"://"` endpoint resolving to `(namespace, service)` now produces **exactly one** service node — in the anchor (caller) cluster — and a single `pod-calls-service` edge. The per-family cross product of service nodes/edges is removed.
- **BREAKING (wire format):** `service-selects-pod` becomes **may-cross-cluster**. The single local service node fans out `service-selects-pod` edges to backing pods across **every family-sibling cluster that ALSO holds the same-named Service object** — the union of each sibling's `EndpointsByService`. Gate: *iff family cluster with the same-name Service*.
- A `"://"` endpoint resolves to a service node **only in the anchor (caller) cluster, and only when that cluster itself holds the same-named Service object** (a `ServicesByNameNS` entry for `{anchor, namespace, service}`); otherwise it falls back to an **`external`** node. This single membership test uniformly covers an anchor whose own cluster lacks the Service (a sibling holding it is not enough — the local Service is a mesh precondition), an unanchorable `"unknown"` anchor, and a bogus trace label naming no holding cluster. Because `clusterFamilyKey("unknown") = "unknown"` is its own family-of-one, a **fully-unlabelled single-cluster deployment** (everything bucketed to `"unknown"`) still resolves within its `unknown` pseudo-cluster — that behaviour is **preserved, not regressed**.
- **Removed:** the unknown-family fallback (`svcHolderFamilies`, `holderFamily`, `loadedUniqueFamilyHolders`, `knownFamilies`) and endpoint-backed pruning (`epVisibleClusters`, `endpointBacked`) — both existed only to disambiguate the cross-family service-NODE fan-out that the same-cluster rule eliminates. "A known service with zero endpoints anywhere still materialises its (local) node" is preserved, now as a natural consequence of materialising on Service-object presence rather than endpoint presence.

## Capabilities

### New Capabilities
<!-- none -->

### Modified Capabilities
- `pod-service-graph`: the "Connection-string endpoint resolution" requirement changes from per-family-cluster service-node fan-out (with unknown-family fallback + endpoint-backed pruning) to single local-cluster service-node resolution with a cross-cluster `service-selects-pod` endpoint union over same-named-Service family siblings; anchor-unknown and anchor-lacks-Service both fall back to `external`.
- `graph-api`: the edge-type registry's cross-cluster flags **swap** — `service-selects-pod` `MayCrossCluster` flips `false → true` and `pod-calls-service` flips `true → false` (a `pod-calls-service` edge now always connects a pod to a service node in its **own** cluster, so it can no longer cross). The `pod-calls-service`, `service-selects-pod`, and `pod-calls-pod` descriptions are updated (drop per-family node fan-out and unknown-family-fallback wording). The "Edge-type discovery endpoint", "Cross-cluster edge representation", and "Filter parameters" requirements are updated so the cross-cluster service-related type is now `service-selects-pod` (local service → remote backing pod) rather than `pod-calls-service`.

## Impact

- **Code:** `pkg/build/servicegraph.go` (`sgResolver` struct trimmed; `parseServiceGraph` index build simplified — keep `svcCandidates`, drop the fallback/pruning indexes; `resolveServiceLevel` rewritten; `materializeService` split into idempotent node-creation + caller-driven cross-cluster endpoint fan-out). `pkg/graph/registry.go` (`service-selects-pod` `MayCrossCluster=true` + descriptions). `pkg/build/clusterfamily.go` unchanged (family key still scopes the endpoint-union candidate set).
- **Behaviour derivation:** `pkg/graph/graph.go` `isCrossCluster` / `EdgeCountByType` now evaluate `service-selects-pod` against source/target node clusters (automatic via the registry-driven `neverCrossCluster` gate — no code change there).
- **Tests:** `pkg/build/servicegraph_family_test.go` (heavy rewrite), `servicegraph_test.go`, `servicegraph_fixes_test.go`; `internal/integration` cross-cluster connection-string cases; `internal/api` golden files (`-update`).
- **Docs:** CLAUDE.md D29 section; README (EN + zh-tw); regenerated swagger (`make docs`).
- **No upstream/PromQL change** — resolution stays in-memory; the "no filters pushed to PromQL" contract is preserved.
