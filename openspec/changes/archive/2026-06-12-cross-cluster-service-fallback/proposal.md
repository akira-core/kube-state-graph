# Cross-cluster service fallback for connection-string resolution

## Why

A `"://"` connection string such as `nats://my-nats.messaging.svc:4222` is today resolved only against the trace-source (client-side) cluster's topology. In a service-mesh deployment the same Kubernetes Service DNS name routes across clusters, and the same `(namespace, service)` is commonly deployed in several clusters at once. When the addressed Service lives in a different cluster than the caller, the lookup misses and the endpoint degrades to an `external/<label>` node — losing the service node, its `service-selects-pod` fan-out, and the cross-cluster dependency picture the API exists to provide.

## What Changes

- **Cluster-family fan-out in connection-string resolution (D29 Stage 0)**: a `"://"` label classified to `(service, namespace)` is resolved against the `ServicesByNameNS` of **every cluster in the trace-source cluster's family**, not only the trace-source cluster itself. Two clusters are in the same family when their names are equal after replacing every maximal digit run with a fixed sentinel (e.g. `prod-03` ↔ `prod-12` match; `staging-1` does not match `prod-1`). The rule is a hardcoded pure string function — no knob, no PromQL change (family filtering happens at the resolution layer, preserving the "no filters pushed to PromQL" contract). For **each** family cluster holding that `(namespace, service)`, the resolver materialises that cluster's service node and emits one `pod-calls-service` edge to it (one upstream series → N edges). Zero matches in the family → today's `external/<label>` fallback, unchanged.
- When **both** sides of a series are `"://"` labels resolving to service sets, the resolver emits the cross product of edges (each resolved source × each resolved target).
- **BREAKING (contract, not wire)**: `pod-calls-service` edges are no longer guaranteed intra-cluster. The `/v1/edge-types` catalogue entry for `pod-calls-service` flips `may_cross_cluster` from `false` to `true`. Response schema is unchanged (cross-cluster status was already derived by comparing source/target node `labels.cluster`).
- `service-selects-pod` fan-out is **unchanged**: it stays intra-cluster within the resolved service's own cluster (the service and its backing pods always share a cluster).
- Edge `labels.cluster` rule is **unchanged**: still the trace-source cluster when the client side is a pod, omitted otherwise.
- Determinism is preserved: resolution is a pure function of (series labels, topology); candidate clusters are iterated in sorted order, the existing `(src, tgt)` edge dedupe and lexically-smaller `srcCluster` tie-break apply, and edge IDs remain UUIDv5 over `<type>|<source>|<target>` with the unchanged compiled-in namespace — the response body stays byte-identical for the same upstream data.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: the "Connection-string endpoint resolution" requirement replaces the trace-cluster-scoped lookup with cluster-family fan-out (one `pod-calls-service` edge per family cluster holding the service); the "unresolvable → external" condition narrows to "no family cluster holds the service". The "Missing pod-UID human-label fallback" requirement's resolution-order enumeration is updated to match.
- `graph-api`: the "Edge-type discovery endpoint" requirement's `pod-calls-service` catalogue entry changes `may_cross_cluster` to `true`; the "Cross-cluster edge representation" requirement extends to `pod-calls-service` edges; the "Filter parameters" requirement's unified edge-retention rule keeps its behaviour (it is edge-type-agnostic) but its enumerated example (b) is generalised so the re-added cross-cluster partner may be a pod or a service node.

## Impact

- **Code**: `pkg/build/servicegraph.go` (`resolveServiceLevel` / `resolveConnString` / `resolveEmptyUID` return a set of IDs; cross-product edge emission in `parseServiceGraph`), a cluster-family key function, possibly a small index addition on `build.Topology` for by-`(namespace, service)` lookup; `pkg/graph` edge-type registry (`pkg/graph/registry.go`): the `pod-calls-service` entry flips `may_cross_cluster` to `true` AND its `Description` string — currently "Always intra-cluster — the service is resolved in the trace-source (client) cluster." — is rewritten to describe cluster-family fan-out (may cross clusters within the trace-source cluster's family); the `pod-calls-pod` and `service-selects-pod` `Description` strings are reviewed for stale single-cluster phrasing (the `service-selects-pod` text gains the "resolved service's own cluster" phrasing). The registry is served verbatim by `/v1/edge-types`, so the `internal/api/testdata/golden/edge-types.json` golden refresh picks up the new text.
- **Tests**: `pkg/build/servicegraph_test.go` unit tests (family-key function; multi-cluster fan-out; zero-match → external; out-of-family cluster excluded; both-sides cross product); `pkg/graph/service_test.go` (invert the hard-coded `MayCrossCluster` assertion for `pod-calls-service` — it currently fails the build the moment the registry flips; keep the `service-selects-pod` intra-cluster assertion); `internal/api` golden tests for catalogue output; `internal/integration` end-to-end case (client in cluster `prod-1`, `kube_service_info` only in `prod-2`).
- **Docs**: `CLAUDE.md` load-bearing rules ("always intra-cluster" wording for `pod-calls-service`), OpenAPI annotations if edge-type descriptions mention intra-cluster.
- **No new dependencies, no new endpoints, no response-shape change.**
