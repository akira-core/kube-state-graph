## ADDED Requirements

### Requirement: Ingress route-path marking

The two shapes emitted by a routed global-FQDN hit ("Full ingress chain for routed global-FQDN
hits") — the ingress chain and the direct `caller pod → backend service` edge — SHALL be
distinguishable in the emitted graph without traversal, so a consumer can render or toggle the
gateway path independently of the direct dependency.

**Synthesized hop edge type.** The synthesized `ingress gateway pod → backend service` edge of a
chain SHALL carry the edge type `pod-routes-to-service` instead of `pod-calls-service`. The type
denotes a **configuration-derived** relationship (translated Gateway + VirtualService state), not
observed traffic. Its source SHALL always be an ingress gateway pod and its target the routed
backend `service` node; both endpoints are in the selected ingress cluster by construction, so the
type SHALL be registered as **not** cross-cluster. It SHALL be treated as a connectivity edge by
the default projection's connectivity prune. Its `cluster` label, the traced-edge-wins dedup
against trace-derived `(source, target)` pairs, and the set of edges emitted SHALL be otherwise
unchanged from the pre-existing chain emission.

**Ingress node marker.** A `service` node materialised as an ingress entry point SHALL carry a
`role` key on its `labels`, with exactly one of two values:

- `ingress-gateway` — the entry hop of a routed hit's chain; at least one `pod-routes-to-service`
  edge exists behind it;
- `ingress-lb` — the destination of the "Ingress LB Service fallback" (no routed backend behind
  it, therefore no `pod-routes-to-service` edge).

The key SHALL be absent (not empty-valued) on every `service` node that is not an ingress entry
point, and the node SHALL remain `type="service"` — no new node type is introduced.

**Determinism.** Marking SHALL be monotone and order-free. When one Service is materialised as
both a chain entry hop and an LB-fallback destination within a single build, the resulting value
SHALL be `ingress-gateway` regardless of the order in which the upstream series are resolved
(`ingress-gateway` overwrites; `ingress-lb` is written only into an unset value). Marking SHALL be
idempotent under repeated series that share one route key, and SHALL never clear or downgrade a
value.

**Degrade invariance.** Marking SHALL occur only after an ingress `service` node has been
successfully materialised. Every chain-precondition degrade of the parent requirement
materialises no ingress node and SHALL therefore produce neither the marker nor a
`pod-routes-to-service` edge, leaving the degraded output identical to the pre-existing
direct-edge shape. The direct `caller pod → backend service` edge SHALL be emitted unchanged — it
carries no additional label. No resolution behaviour, precondition, external fallback,
external-fallback reason, engine outcome, upstream query, or store read is altered by this
requirement, and output with route resolution disabled SHALL be unchanged.

#### Scenario: Chained routed hit marks its ingress node and types its synthesized hop

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint resolves to a routed hit
  whose chain preconditions all hold
- **THEN** the ingress `service` node SHALL carry `labels.role = "ingress-gateway"` (and keep
  `type="service"`)
- **AND** each synthesized edge from an ingress gateway pod to the backend `service` node SHALL
  have type `pod-routes-to-service`, carrying the ingress cluster as its `cluster` label
- **AND** no edge of type `pod-calls-service` SHALL exist from an ingress gateway pod to the
  backend `service` node
- **AND** the `caller pod → ingress service` edge, the `caller pod → backend service` direct edge,
  and both `service-selects-pod` fan-outs SHALL be emitted exactly as before, the direct edge
  carrying no additional label

#### Scenario: Ingress LB Service fallback marks its node distinguishably

- **WHEN** the Istio pipeline misses with no Gateway and the destination IPs resolve to a unique
  ingress LB Service ("Ingress LB Service fallback")
- **THEN** the materialised `service` node SHALL carry `labels.role = "ingress-lb"`
- **AND** no `pod-routes-to-service` edge SHALL be emitted for that endpoint

#### Scenario: One Service resolved by both paths is marked deterministically

- **WHEN** within a single build one endpoint resolves a chained hit whose entry hop is a given
  Service, and another endpoint's resolution falls back to that same Service as its ingress LB
  Service
- **THEN** that single `service` node SHALL carry `labels.role = "ingress-gateway"` irrespective
  of the order in which the two endpoints are resolved

#### Scenario: Degraded chain emits neither marker nor synthesized edge type

- **WHEN** a routed hit's chain degrades for any precondition reason (no unique ingress identity,
  ingress identity equal to the backend identity, selected cluster's topology lacking the ingress
  Service, or the ingress Service having no backing pod in the selected cluster)
- **THEN** no `service` node SHALL carry a `role` label for that endpoint
- **AND** no `pod-routes-to-service` edge SHALL be emitted
- **AND** the emitted nodes and edges SHALL be identical to the pre-existing direct-edge shape

#### Scenario: Backend service node is never marked

- **WHEN** a routed hit resolves a backend `service` node that is not itself an ingress entry
  point
- **THEN** that node's `labels` SHALL NOT contain a `role` key
