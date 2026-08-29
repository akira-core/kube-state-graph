# Design — mark-ingress-route-path

Extends `route-hit-ingress-chain` (parent; its D-numbers referenced as C1–C5) and, through it,
`translate-global-fqdn-to-k8s-service` and `ingress-lb-service-fallback`. Purely a **labelling**
change: no resolution, precondition, degrade, or store behaviour moves. Settled decisions:

## D1 — Distinguishability via the ingress node's `role`, not a new edge type

The chain has three elements. Two (`caller → ingress`, `ingress → gateway pods`) are incident to
the ingress `service` node and vanish with it; the third — the synthesized
`gateway pod → backend service` edge — is **not**, so a consumer that hides only the ingress node
is left with gateway pods pointing at the backend with no visible cause.

A dedicated edge type for that third hop was considered and **rejected**: it would split
`pod-calls-service` into two catalogue entries for what is still a pod→service relationship, and
consumers can already identify the gateway path by selecting nodes with
`labels.role="ingress-gateway"` (plus their backing pods via `service-selects-pod`). The
synthesized hop therefore stays typed `pod-calls-service`, sharing the traced-edge-wins dedup and
connectivity-prune membership of every other pod→service edge.

The remaining problem — telling a routed chain's ingress entry point apart from an nginx
`ingress-lb` fallback that has **no** chain behind it — is solved by the node marker in D3.

## D2 — No new node type for the ingress Service

`materializeServiceNode` is **idempotent by node id** (`<cluster>/<ns>/<svc>`). Within one build
the same Service can be materialised by more than one path — as a routed backend for one endpoint,
as a chain entry hop for another, as a plain `"://"` connection-string target for a third. If the
node's `type` depended on which path materialised it first, the emitted type would be a function
of vector/map arrival order — a direct violation of the determinism rule the golden tests encode.

A node **type** is also the wrong model: the object IS a Kubernetes Service; "ingress" is a role
it plays in one resolution, not a different kind of thing. And the ripple is large — both
`compoundParent`/group-collection switches in `pkg/cytoscape`, the `SourceType`/`TargetType` of
three registry entries, golden files, and the OpenAPI node-type enum.

A `labels` key carries the same information at a fraction of the cost, and a **monotone**
(set-only) assignment is order-free by construction (D3).

## D3 — Two role values, mirroring the two engine outcomes, with a monotone precedence

Two distinct code paths materialise an ingress node, and a consumer must tell them apart:

| Value | Emitted by | Behind it |
|---|---|---|
| `ingress-gateway` | `resolveRouteChain` (`RouteHit` chain entry hop) | gateway pods + synthesized `pod-calls-service` → backend |
| `ingress-lb` | `routeIndexResolve`'s `RouteIngressLBService` branch (nginx fallback) | controller pods only — **no routed backend** |

The values deliberately mirror the engine's `hit` / `ingress_lb_service` outcomes. Collapsing them
into a single `ingress` value would force every consumer into a two-hop graph query ("does a pod
behind this node have a synthesized hop to a backend?") to avoid hiding an `ingress-lb` node — and
hiding one erases the caller's only dependency edge, since that path emits no direct edge.

**Precedence.** One Service can hit both paths in a single build (one endpoint's route resolves
through it as a chain entry; another endpoint's route misses at gateway selection and LB-falls-back
to the same Service). A plain first-write-wins assignment would then be arrival-order dependent.
Rule: `ingress-gateway` always overwrites; `ingress-lb` writes only into an empty value. Both
arrival orders yield `ingress-gateway`, which is also the more informative claim (a chain provably
exists behind the node). Assignment is otherwise set-only — never cleared, never downgraded — so
it stays idempotent under repeated series sharing one route key.

The key lives in `labels` (strict `map[string]string`), alongside `cluster` / `namespace` — no
typed node attribute, no new sealed-interface method. It is absent (not empty-string) on every
node that is not an ingress entry point, matching the repo's "a key is set only when its value
resolves non-empty" convention.

## D4 — The direct `caller → backend` edge stays unmarked

Marking it (e.g. `labels.route="via-ingress"`, "this pair also has an ingress path") was
considered and dropped: the motivating consumer control always shows direct edges and only toggles
the chain on top, so the mark buys nothing today. It would also add a merge rule — the direct edge
can collapse with a trace-derived edge for the same pair, so the mark would have to be a monotone
OR over contributing samples — and would churn the labels of edges that are today pure trace
output. The design stays additive: the mark can be introduced later without breaking this one.

## D5 — Nothing about resolution changes

`resolveRouteChain`'s four preconditions, the pure-before-materialisation ordering (C3), the
backend-resolves-first rule, the locked-cluster vs family-wide fan-out split (C2), the
traced-edge-wins dedup (C4), `extReasons`, and every `route_engine_*` external path are untouched.
A chain degrade materialises **no** ingress node, so it produces no marker — the degraded
output stays byte-identical to `route-hit-ingress-chain`'s. Feature-off output is unchanged.

Marking happens strictly **after** successful materialisation, inside the same two call sites that
already own the two outcomes, so no signature changes propagate through
`resolveServer` → `resolveUnknownServerPeer` → `routeIndexResolve`.

## Resulting graph contract

```
caller pod ──pod-calls-service──────────► ingress Service   labels.role=ingress-gateway
     │                                          │
     │                                          │ service-selects-pod (locked cluster)
     │                                          ▼
     │                                     gateway pod(s)
     │                                          │
     │                                          │ pod-calls-service (synthesized hop)
     │                                          ▼
     └──pod-calls-service─────────────────► backend Service
                                                │ service-selects-pod (family-wide)
                                                ▼
                                           backend pod(s)
```

nginx / `RouteIngressLBService`: `caller → ingress Service` (`labels.role=ingress-lb`) plus its
locked-cluster `service-selects-pod` fan-out only — no synthesized hop to a routed backend.
