# Design — scope-gateway-candidates-to-ingress-namespace

Fixes the same-named cross-namespace Gateway hazard in the route-resolution engine
(parent: translate-global-fqdn-to-k8s-service; time semantics per
simplify-route-resolution-to-point-in-time). Settled decisions:

## D1 — Namespace restriction over (namespace, name) identity threading

Two candidate fixes existed:

1. Thread `(namespace, name)` end-to-end: `gwresolve.Gateway`/`pat.gw` keyed on a Ref
   pair, `ResolveAmong` returning the pair, `ScopedFor(ns, name)`.
2. Restrict Hop 3 so a candidate Gateway must live in the ingress Service's own
   namespace.

Option 2 is chosen. Kubernetes guarantees name uniqueness within a namespace, so once
candidates are namespace-scoped the bare-name identity is unambiguous by construction —
the entire collision class disappears without touching any public signature
(`gwresolve.Gateway`, `Resolve`, `ResolveAmong`, `ScopedFor` all keep their shapes). The
cost is a REAL narrowing: Istio's cross-namespace gateway attachment (a `team-a` Gateway
selector-attaching to the shared `istio-system` ingressgateway) stops resolving. That
shape has not been observed in the operator's environments; if met, the extension path is
option 1 as a follow-up change (this design documents it deliberately). Until then the
smaller surface wins.

## D2 — Both layers change, keeping the SQL/snapshot mirror discipline

The deploy hop already models the pattern: SQL scopes by the ingress namespaces
(`has(?, namespace)` bound to the hop-1 namespace list) and the in-memory hop re-applies
the exact per-service check. The gateway hop now does the same:

- `gw_versions` SQL gains `AND has(?, namespace)` bound to the SAME `nsList` hop 2
  already uses — strictly fewer rows, no schema change. The load stays a correct
  SUPERSET (nsList unions all ingress Services matching the IP), and
- `snapshot.ResolveIPToGateways` applies the exact predicate `r.Namespace == svcNS`
  against the single hop-1-selected Service's namespace.

The snapshot layer must NOT rely on the SQL filter alone: the `Store` interface allows
other implementations, and the snapshot doc promises it mirrors the reader's predicates.

## D3 — Identical-pattern determinism fixed at the source (sortPats)

Even with unique names, two different-named gateways may declare the identical host
pattern with equal specificity; `sortPats` previously fell through to `SliceStable` input
order = ClickHouse row order. The comparator gains `pat.gw` ascending between the pattern
and idx comparators: score desc → pattern asc → gw asc → idx asc.

- Real istiod treats cross-gateway duplicate server hosts as a config error
  (`CheckDuplicates`); this engine picks deterministically (lexically-smallest name,
  matching the repo-wide D6 lexical-smallest-on-collision convention) instead of failing
  a build over an upstream config smell.
- `PickHosts` stamps `gw: ""` on every pattern, so the new comparator is a no-op there —
  its numeric-index guarantee (`TestPickHostsIndexNotLexicographic`) is untouched.
- Sorting `resolver.candidates()` instead was rejected: it would fix only this caller and
  mask the ordering dependence inside gwresolve.

## D4 — Degradation shape

A cross-namespace gateway is simply not a candidate, so the pipeline misses exactly as
"no gateway serves the host" (`RouteNoGateway`) — the same outcome class as a non-Istio
ingress. The ingress LB Service fallback keeps its `no_gateway` gate and still applies;
no new outcome, no new diagnostic reason, no node/edge/attribute/labels change.

## Risks

- **Narrowing** (intended, D1): cross-namespace attachment stops resolving. Disclosed in
  proposal.md; extension path documented.
- **Tie-break behaviour shift**: identical-pattern configs previously resolved by row
  order now resolve lexically — affected configs were already non-deterministic.
