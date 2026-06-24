# Refine cluster-family fan-out: endpoint-backed pruning + unknown-family fallback

## Why

Two field-observed defects in the D29 cluster-family fan-out shipped by
`cross-cluster-service-fallback`:

1. **Fan-out edges to endpointless Service deployments.** A `(namespace, service)`
   is commonly created in every family cluster (GitOps applies the Service object
   fleet-wide) while its backing pods run in only one cluster — the mesh routes
   the in-cluster DNS name to the cluster that actually has endpoints. Today the
   fan-out emits one `pod-calls-service` edge per family cluster *holding the
   Service object*, so a caller in `c1` gets edges to both `c1`'s endpointless
   Service and `c2`'s real one. The `c1` edge depicts traffic that cannot
   terminate there.

2. **Same connection string resolves inconsistently — service node for some
   series, `external/<label>` for others.** Resolution is anchored per series:
   the UID-recovered client-pod cluster when the client side is a topology pod,
   else the raw trace `cluster` label. Series whose client side is non-pod (both
   sides `"://"`, missing client UID, UID unknown to topology) fall back to the
   label, which is frequently missing (bucketed `"unknown"`) or wrong — the
   anchor's family then matches no loaded cluster, the lookup yields zero
   candidates, and the endpoint degrades to `external/<label>` even though the
   addressed service unambiguously exists in exactly one family.

## What Changes

- **Endpoint-backed pruning (fan-out candidate selection)**: when at least one
  matched family candidate has ≥1 backing pod (its own cluster's
  `EndpointsByService` entry is non-empty), candidates PROVABLY without backing
  pods are skipped entirely — no service node, no `pod-calls-service` edge, no
  fan-out. "Provably" requires endpoint visibility: only a candidate whose own
  cluster demonstrably exports joinable endpoint data (≥1 `EndpointsByService`
  entry for some service) yet has none for the addressed service is pruned; a
  candidate whose whole cluster has zero endpoint data (per-cluster allowlist
  gap — staged rollout, config drift) is kept, since its zero is absence of
  evidence. When NO matched candidate is endpoint-backed, ALL candidates
  materialise exactly as today (service nodes with zero fan-out edges) — this
  preserves the "known service with zero backing endpoints still materialises"
  contract and keeps deployments without the `kubernetes.io/service-name`
  endpointslice allowlist fully functional, fleet-wide or per-cluster.
- **Unknown-family global fallback (anchor recovery of last resort)**: when the
  anchor's family key is not the family of ANY loaded cluster (covers the
  `"unknown"` bucket and bogus trace labels), the addressed
  `(namespace, service)` is looked up across the LOADED clusters —
  `"unknown"`-bucketed service entries are not holders, so an unlabelled
  duplicate can neither flip a uniquely-held name to ambiguous nor resolve a
  bogus anchor into the pseudo-cluster. If every loaded holding cluster belongs
  to ONE family, those clusters become the candidate set (endpoint-backed
  pruning then applies), replacing any primary-lookup hit on
  `"unknown"`-bucketed entries; if holders span two or more families
  (genuinely ambiguous name), the endpoint falls back to `external/<label>`.
  If NO loaded cluster holds the name, an `"unknown"` anchor keeps its primary
  hit on `"unknown"`-bucketed entries (fully-unlabelled deployments retain
  resolution); a bogus-label anchor falls back to `external/<label>`. An
  anchor whose family IS loaded but lacks the service does NOT fall back
  across families — resolving a known-cluster caller against a foreign family
  would fabricate cross-family attribution, which the family scoping exists to
  prevent.
- Both rules are hardcoded pure functions of `(series labels, topology)` — no
  knob, no PromQL change, no new node/edge type, no response-shape change.
  Determinism is preserved (sorted candidate iteration; pruning and the
  uniqueness check are order-free set operations).
- Edge `labels.cluster` rule unchanged (raw trace label iff the client side is a
  pod, D9). `/v1/edge-types` wire values unchanged (`may_cross_cluster` already
  `true`); only the human-readable `description` strings are extended.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: the "Connection-string endpoint resolution" requirement
  gains endpoint-backed pruning in step 3 and the unknown-family fallback in
  step 4; the "Missing pod-UID human-label fallback" requirement's resolution
  order enumeration is updated to match. One existing scenario is rewritten
  (unknown anchor + single-family holder now RESOLVES instead of degrading to
  external) and new scenarios pin the pruning, the all-unbacked degenerate case,
  the ambiguous-name external fallback, and the known-family-no-fallback rule.

## Impact

- **Code**: `pkg/build/servicegraph.go` only — a global `(namespace, service)`
  candidate index and a loaded-family-key set built alongside the existing
  per-parse `svcCandidates` index; `resolveServiceLevel` gains the fallback and
  the pruning filter. `pkg/graph/registry.go` `pod-calls-pod` /
  `pod-calls-service` description strings extended (wire fields unchanged).
- **Tests**: `pkg/build/servicegraph_family_test.go` (pruning; all-unbacked;
  unknown-anchor single-family fallback; unknown-anchor ambiguous → external;
  known-family-miss stays external), existing tests unchanged (verified:
  `redis` zero-endpoint single-candidate case is protected by the all-unbacked
  rule; the `UnknownClusterNonPodClient` fixture's `nats` spans two families →
  still external); `internal/api/testdata/golden/edge-types.json` refresh;
  `internal/integration` end-to-end cases for both rules.
- **Docs**: `CLAUDE.md` D29 bullet; OpenAPI regenerated if handler annotations
  change.
- **No new dependencies, no new endpoints, no response-shape change.**
