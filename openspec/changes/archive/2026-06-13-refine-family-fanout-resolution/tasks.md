# Tasks — refine-family-fanout-resolution

## 1. Resolver indexes (`pkg/build/servicegraph.go`)

- [x] 1.1 Add per-parse `svcGlobal map[nsSvcKey][]svcCandidate` (key: `(namespace, service)`) built in the same loop as the existing `svcCandidates` family index; sort each bucket by cluster (D6 determinism). Add `type nsSvcKey struct{ namespace, service string }`
- [x] 1.2 Add per-parse `knownFamilies map[string]struct{}` — family keys of every loaded cluster, derived from `topology.ClustersObserved` ∪ clusters of `ServicesByNameNS` keys ∪ topology pods' `labels.cluster`, EXCLUDING `""` and the `"unknown"` bucket sentinel (D-2)

## 2. Resolution rules (`pkg/build/servicegraph.go`)

- [x] 2.1 Unknown-family fallback in `resolveServiceLevel`: when the family lookup yields zero candidates AND the anchor's family key is absent from `knownFamilies`, take `svcGlobal[{ns, svc}]`; if all holders share ONE family key, use them as the candidate set; else (≥2 families or zero holders) keep zero candidates → external fallback (D-2, D-3)
- [x] 2.2 Endpoint-backed pruning: filter the chosen candidate set to candidates with `len(endpointsByService[{cluster, ns, svc}]) > 0`; use the filtered set when non-empty, the full set when empty (all-unbacked degenerate case keeps today's behaviour) (D-1)
- [x] 2.3 Verify the `resolveConnString` memo stays valid (resolution remains a pure function of `(label, anchor family, topology)`) and no `materializeService` / `addServiceEdge` / edge-emission change is needed

## 3. Unit tests (`pkg/build/servicegraph_family_test.go`)

- [x] 3.1 Pruning: nats in prod-1+prod-2, endpoints only in prod-2 → exactly one `pod-calls-service` edge targeting `prod-2/messaging/nats`; no prod-1 service node; fan-out only to prod-2 pods
- [x] 3.2 All-unbacked: both candidates endpointless → both service nodes materialise, two edges, zero `service-selects-pod` (pre-pruning behaviour preserved)
- [x] 3.3 Unknown-anchor single-family fallback: missing cluster label + non-pod client + `(data, cache)` held only by the prod family → resolves to both prod service nodes, edges omit `labels.cluster`
- [x] 3.4 Unknown-anchor ambiguous name: `(messaging, nats)` spans prod + staging families → stays `external/<label>` (existing `UnknownClusterNonPodClient` test already pins this; extend its assertion message or add a sibling)
- [x] 3.5 Known-family miss does NOT engage the fallback: existing `OutOfFamilyOnlyServiceFallsBackToExternal` keeps passing unchanged (prod family loaded but lacks nats → external, no staging match)
- [x] 3.6 Determinism: extend `FamilyFanout_Deterministic` sample set with an unanchorable series so the fallback path is order-shuffled too
- [x] 3.7 Confirm existing `servicegraph_test.go` is untouched: `HeadlessServiceWithNoEndpoints_StillResolvesToServiceNode` (single unbacked candidate → all-unbacked rule) and all external-fallback tests still pass

## 4. Registry + golden (`pkg/graph/registry.go`, `internal/api`)

- [x] 4.1 Extend `pod-calls-service` description: endpoint-backed pruning + unknown-family fallback; review `pod-calls-pod` description's "held by NO cluster in the trace-source cluster's family" phrasing for staleness. Wire fields unchanged
- [x] 4.2 Refresh `internal/api/testdata/golden/edge-types.json` (`go test ./internal/api/ -update -run Golden`); confirm graph goldens byte-identical without `-update`

## 5. Integration tests (`internal/integration/graph_e2e_test.go`)

- [x] 5.1 E2E pruning: ingest `kube_service_info` for both prod clusters, endpointslice series only for prod-2 → response has only the prod-2 service node + one cross-cluster `pod-calls-service` edge
- [x] 5.2 E2E unknown-family fallback: service-graph series with NO `cluster` label and non-pod client, service held only by the prod family → response has the prod service nodes (not `external/<label>`)

## 6. Docs

- [x] 6.1 Update `CLAUDE.md` D29 bullet: endpoint-backed pruning + unknown-family fallback (and the "known service with zero backing endpoints" sentence's new scope)
- [x] 6.2 Update `internal/api/handlers.go` `/v1/edge-types` `@Description` if it states family-only resolution; run `make docs` and commit `docs/` if changed

## 7. Verification

- [x] 7.1 `make test` (unit + component + golden + property, race + shuffle) green
- [x] 7.2 `make lint` clean; integration suite green where Docker is available
- [x] 7.3 `openspec validate refine-family-fanout-resolution` passes

## 8. Adversarial review round (12 surviving findings, all fixed)

- [x] 8.1 Exclude `"unknown"`-bucketed service entries from `svcGlobal`; identified holders REPLACE the `"unknown"` primary hit (shadow + poison fixes); keep the direct hit only when no loaded cluster holds the name (fully-unlabelled deployments)
- [x] 8.2 Per-cluster endpoint-visibility gate (`epVisibleClusters`) so allowlist asymmetry never prunes an unobservable cluster
- [x] 8.3 Reconcile design.md (D-1/D-2/D-3/D-4) and the delta spec with the implemented semantics; new scenarios: visibility sparing, unlabelled-duplicate shadow/poison, fully-unlabelled deployment, bogus-anchor + unknown-only holder
- [x] 8.4 New unit pins: unknown-holder shadow/poison, fully-unlabelled direct hit, bogus-anchor external, visibility sparing, fallback-path pruning, cross-product with pruned client side, client-side fallback, same-label different-anchors memo keying
- [x] 8.5 E2E hardening: prune test gates count(kube_service_info)==2 + both endpointslice rows + rate>0 and gives prod-1 endpoint visibility via prune-vis; unknown-family test gets a test-unique namespace + full gates
- [x] 8.6 CLAUDE.md / registry / handlers descriptions updated; golden + docs regenerated; lint clean
