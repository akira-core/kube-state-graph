# Design — mark-ingress-route-path

Extends `route-hit-ingress-chain` (parent; its D-numbers referenced as C1–C5) and, through it,
`translate-global-fqdn-to-k8s-service` and `ingress-lb-service-fallback`. Purely a **labelling**
change: no resolution, precondition, degrade, or store behaviour moves. Settled decisions:

## D1 — The synthesized hop gets its own edge type, not a label

The chain has three elements. Two (`caller → ingress`, `ingress → gateway pods`) are incident to
the ingress `service` node and vanish with it; the third — the synthesized
`gateway pod → backend service` edge — is **not**, so a consumer that hides only the ingress node
is left with gateway pods pointing at the backend with no visible cause. That third element needs
its own handle.

It gets an edge **type** rather than an edge label because:

1. `type` is a flat field on the serialised edge `data`; `labels` is a nested map. Consumers
   (Cytoscape selectors, `?edge_type=` server-side filtering, `/v1/edge-types` documentation) all
   key off `type`.
2. The edge is categorically different from every other edge in the graph: it is the only one
   **derived from translated configuration** rather than observed traffic. `pod-calls-service`
   asserts "this pod was seen calling this service"; the synthesized hop asserts "this gateway
   would route to this service". Conflating them mislabels the evidence.
3. It doubles as the gateway-pod identifier — the source of a `pod-routes-to-service` edge IS an
   ingress gateway pod, so a consumer needs no traversal to find them.

Name: `pod-routes-to-service`, following the registry's existing `<source>-<verb>-<target>`
convention (`pod-calls-service`, `service-selects-pod`, `pod-to-node`). The considered alternative
`deployment-to-service` was rejected — the source node is a **pod**, so `source_type: [pod]` would
contradict the name.

`may_cross_cluster: false`: `resolveRouteChain` draws the source pods from
`endpointsByService[{dest.Cluster, ...}]` (locked-cluster fan-out, C2) and the backend node is
materialised in `dest.Cluster` by `resolveServiceLevel(dest.Cluster, ...)`, so both endpoints are
in the locked ingress cluster by construction. This is strictly more precise than the
`pod-calls-service` entry it leaves (which is `true` because the route-engine *caller* edge may
anchor on a family sibling).

The type is added to the connectivity case of `graph.connectivityExcluded` — it is a genuine
connectivity edge, and the switch is documented as exhaustive over `EdgeType`. (In practice a
gateway pod is already connected via its `service-selects-pod` edge, so no projection output
changes; the listing is a correctness/exhaustiveness requirement, not a behaviour fix.)

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
| `ingress-gateway` | `resolveRouteChain` (`RouteHit` chain entry hop) | gateway pods + `pod-routes-to-service` → backend |
| `ingress-lb` | `routeIndexResolve`'s `RouteIngressLBService` branch (nginx fallback) | controller pods only — **no routed backend** |

The values deliberately mirror the engine's `hit` / `ingress_lb_service` outcomes. Collapsing them
into a single `ingress` value would force every consumer into a two-hop graph query ("does a pod
behind this node have a `pod-routes-to-service` edge?") to avoid hiding an `ingress-lb` node — and
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
A chain degrade materialises **no** ingress node, so it produces neither marker — the degraded
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
     │                                          │ pod-routes-to-service   ← new type
     │                                          ▼
     └──pod-calls-service─────────────────► backend Service
                                                │ service-selects-pod (family-wide)
                                                ▼
                                           backend pod(s)
```

nginx / `RouteIngressLBService`: `caller → ingress Service` (`labels.role=ingress-lb`) plus its
locked-cluster `service-selects-pod` fan-out only — no `pod-routes-to-service` edge.
