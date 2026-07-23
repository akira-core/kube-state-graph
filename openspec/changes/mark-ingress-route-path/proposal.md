# Mark the ingress-gateway route path distinguishably

## Why

`route-hit-ingress-chain` made a routed global-FQDN hit emit **two** shapes at once: the chain
(`caller pod → ingress LB service → ingress gateway pod(s) → backend service`) and the direct
`caller pod → backend service` edge. Both are deliberate — the direct edge preserves the
per-caller dependency the shared ingress funnel would erase — but on the wire they are
**indistinguishable**: every edge in both shapes is `pod-calls-service` / `service-selects-pod`,
and the ingress LB Service is an ordinary `type="service"` node.

A consumer therefore cannot render the gateway path differently, nor offer the obvious UI control
("show gateway route" — direct edges always visible, the chain toggled on top). Hiding just the
ingress `service` node is not enough: two of the chain's edges are incident to it and disappear
with it, but the **synthesized `ingress gateway pod → backend service` edge is not**, leaving
gateway pods pointing at the backend with no visible cause.

A second, subtler consumer trap: the nginx `ingress-lb-service-fallback` path materialises the
same kind of ingress node with **no** chain behind it (there is no routed backend). Its
`caller → ingress` edge is the caller's ONLY dependency edge — hiding it as "chain" would erase
the dependency entirely. The two ingress nodes must be told apart.

## What Changes

- **New edge type `pod-routes-to-service`** replaces `pod-calls-service` for the synthesized
  ingress-gateway-pod → backend-Service edge of a `RouteHit` chain. It is the graph's only
  **config-derived** edge (translated Istio Gateway + VirtualService state, not observed traffic),
  and `type` is a flat field a consumer can select on directly. Source is always an ingress
  gateway pod, target the routed backend `service`; both live in the locked ingress cluster, so
  it is **`may_cross_cluster: false`**. It is a connectivity edge for the default-projection
  prune. The traced-edge-wins dedup against `(source, target)` pairs is unchanged.
- **New `role` key on the ingress `service` node's `labels`**, with exactly two values mirroring
  the two engine outcomes that materialise an ingress node:
  - `ingress-gateway` — the `RouteHit` chain's entry hop; a `pod-routes-to-service` edge always
    exists behind it;
  - `ingress-lb` — the `RouteIngressLBService` (nginx) fallback destination; no routed backend
    behind it.
  Assignment is monotone and order-free: `ingress-gateway` always wins over `ingress-lb` for a
  Service that both paths resolve to within one build, so the value never depends on series
  arrival order.
- **No new node type.** The ingress node stays `type="service"` — it IS a Kubernetes Service, and
  `materializeServiceNode` is idempotent by node id, so a type that depended on which resolution
  path materialised it first would be arrival-order dependent (a determinism break).
- **The direct `caller → backend` edge is unchanged** — no new label, byte-identical to today
  (including its collapse with a trace-derived edge for the same pair).
- No new node attribute, no engine outcome, no PromQL, no store read, no resolution-behaviour
  change. Every chain precondition, degrade path, and external fallback of
  `route-hit-ingress-chain` is untouched; a degrade materialises no ingress node, hence no marker.
  Feature-off (`RouteResolver == nil`) output stays byte-for-byte unchanged.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: adds a "Ingress route-path marking" requirement extending "Full ingress
  chain for routed global-FQDN hits" — the `pod-routes-to-service` edge type for the synthesized
  hop, the `labels.role` marker with its two values and monotone precedence, and the invariant
  that a degrade produces neither.
- `graph-api`: the "Edge-type discovery endpoint" requirement's catalogue gains
  `pod-routes-to-service` with its `source_type` / `target_type` / `directed` /
  `may_cross_cluster` / `labels` contract.

## Impact

- **Modified**: `pkg/graph/edge.go` (new `EdgeType` constant), `pkg/graph/registry.go` (new
  catalogue entry), `pkg/graph/project.go` (`connectivityExcluded` treats the new type as a
  connectivity edge), `pkg/build/servicegraph.go` (`markIngressService`, chain-edge emission type,
  `resolveRouteChain` marks `ingress-gateway`), `pkg/build/routeprescan.go` (LB-fallback branch
  marks `ingress-lb`), `CLAUDE.md`,
  `docs/istio-virtualservice-routing-history-design.md`, `internal/api/handlers.go` (swagger
  description → `make docs`).
- **Golden**: `internal/api/testdata/golden/edge-types.json` regenerated.
- **Dependencies**: none added. `pkg/build` still does not import `pkg/route`
  (`make check-route-containment` unchanged). `RouteResolver`, `RouteDestination`, and every
  mocked interface are unchanged — no mock regeneration.
- **Consumers**: additive. An existing consumer that filters `?edge_type=pod-calls-service` will
  no longer see the synthesized hop under that type — the one behavioural break, and the point of
  the change.
