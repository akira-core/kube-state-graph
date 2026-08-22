# Scope gateway candidates to the ingress namespace

## Why

The route-resolution engine's Hop 3 (ingress Deployment pod labels → candidate Gateways)
matches by workload selector only, across every namespace in the selected cluster. That is
faithful to Istio — a Gateway CR in any namespace may selector-attach to a shared
ingressgateway workload — but the pipeline downstream identifies a gateway by its **bare
name**: `gwresolve.ResolveAmong` returns a name string and `snapshot.ScopedFor` matches
`r.Name == gwName` only. Two same-named Gateways in different namespaces (both attached to
one ingress workload) therefore:

- merge their server hosts into one logical gateway during FQDN host disambiguation, and
- make `ScopedFor` pick whichever same-named row happens to come first — ClickHouse reads
  carry no `ORDER BY`, so the winning namespace is non-deterministic across builds —
  poisoning the bare-name VirtualService binding set, the translate input, and the final
  destination.

A separate, smaller determinism hole exists even without name collisions: two
different-named gateways declaring an identical, equally specific host pattern fall
through `sortPats`' comparators to input (row) order.

## What Changes

- **Hop 3 becomes namespace-scoped**: a candidate Gateway MUST reside in the same
  namespace as the ingress Service (= its Deployment's namespace). Applied in both layers,
  mirroring the existing deploy-hop discipline: the `gw_versions` SQL gains the same
  namespace predicate the deploy query already has, and `snapshot.ResolveIPToGateways`
  re-applies the exact per-service-namespace check in memory.
- Within one namespace Kubernetes guarantees Gateway-name uniqueness, so the bare-name
  identity downstream (`ResolveAmong`, `ScopedFor`) becomes unambiguous **by
  construction** — no signature changes anywhere.
- **Deliberate narrowing (disclosed)**: cross-namespace gateway attachment no longer
  resolves — it degrades as `no_gateway` (→ ingress LB Service fallback → external), the
  same outcome as any non-candidate gateway. If a real deployment is met that relies on
  cross-namespace attachment, the extension path is threading `(namespace, name)` through
  gwresolve/ScopedFor as a follow-up change.
- **Deterministic identical-pattern tie-break**: `gwresolve.sortPats` gains a lexical
  gateway-name comparator between the pattern and idx comparators, so an identical
  equally-specific pattern resolves to the lexically-smallest gateway name instead of row
  order. `PickHosts` stamps an empty name on every pattern, so its behaviour (numeric
  index tie-break) is untouched.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: "Istio route resolution of global FQDN peers" — candidate Gateways
  are restricted to the ingress Service's own namespace; identical equal-specificity host
  patterns resolve to the lexically-smallest gateway name.

## Impact

- **Modified**: `pkg/route/snapshot/snapshot.go` (Hop 3 namespace check),
  `pkg/route/store/clickhouse.go` (gw_versions namespace predicate),
  `pkg/route/gwresolve/gwresolve.go` (sortPats tie-break), tests in
  `pkg/route/{snapshot,gwresolve}` + `pkg/route/resolver_test.go` +
  `internal/integration/route_e2e_test.go`, `CLAUDE.md`.
- **Behaviour**: deployments whose Gateways live in the ingress namespace (the common
  single-namespace ingress shape, and every existing fixture) are byte-for-byte
  unchanged. A config relying on cross-namespace attachment stops resolving (degrades
  `no_gateway`); a config with identical duplicate host patterns — previously
  non-deterministic — now resolves lexically.
- **Dependencies**: none added or removed. Store schema unchanged; the gw_versions query
  reads strictly fewer rows. `make check-route-containment` unchanged.
