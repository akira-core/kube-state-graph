## ADDED Requirements

### Requirement: Ingress route-path marking

The two shapes emitted by a routed global-FQDN hit ("Full ingress chain for routed global-FQDN
hits") — the ingress chain and the direct `caller pod → backend service` edge — SHALL be
distinguishable in the emitted graph without traversal, so a consumer can render or toggle the
gateway path independently of the direct dependency.

**Ingress node marker.** A `service` node materialised as an ingress entry point SHALL carry a
`role` key on its `labels`, with exactly one of two values:

- `ingress-gateway` — the entry hop of a routed hit's chain; gateway pods and a synthesized
  `pod-calls-service` hop to the backend exist behind it;
- `ingress-lb` — the destination of the "Ingress LB Service fallback" (no routed backend behind
  it).

The key SHALL be absent (not empty-valued) on every `service` node that is not an ingress entry
point, and the node SHALL remain `type="service"` — no new node type is introduced. The
synthesized `ingress gateway pod → backend service` hop SHALL remain typed `pod-calls-service`
(no dedicated edge type).

**Determinism.** Marking SHALL be monotone and order-free. When one Service is materialised as
both a chain entry hop and an LB-fallback destination within a single build, the resulting value
SHALL be `ingress-gateway` regardless of the order in which the upstream series are resolved
(`ingress-gateway` overwrites; `ingress-lb` is written only into an unset value). Marking SHALL be
idempotent under repeated series that share one route key, and SHALL never clear or downgrade a
value.

**Degrade invariance.** Marking SHALL occur only after an ingress `service` node has been
successfully materialised. Every chain-precondition degrade of the parent requirement
materialises no ingress node and SHALL therefore produce no marker, leaving the degraded output
identical to the pre-existing direct-edge shape. The direct `caller pod → backend service` edge
SHALL be emitted unchanged — it carries no additional label. No resolution behaviour,
precondition, external fallback, external-fallback reason, engine outcome, upstream query, or
store read is altered by this requirement, and output with route resolution disabled SHALL be
unchanged.

#### Scenario: Chained routed hit marks its ingress node

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint resolves to a routed hit
  whose chain preconditions all hold
- **THEN** the ingress `service` node SHALL carry `labels.role = "ingress-gateway"` (and keep
  `type="service"`)
- **AND** each synthesized edge from an ingress gateway pod to the backend `service` node SHALL
  have type `pod-calls-service`, carrying the ingress cluster as its `cluster` label
- **AND** the `caller pod → ingress service` edge, the `caller pod → backend service` direct edge,
  and both `service-selects-pod` fan-outs SHALL be emitted exactly as before, the direct edge
  carrying no additional label

#### Scenario: Ingress LB Service fallback marks its node distinguishably

- **WHEN** the Istio pipeline misses with no Gateway and the destination IPs resolve to a unique
  ingress LB Service ("Ingress LB Service fallback")
- **THEN** the materialised `service` node SHALL carry `labels.role = "ingress-lb"`

#### Scenario: One Service resolved by both paths is marked deterministically

- **WHEN** within a single build one endpoint resolves a chained hit whose entry hop is a given
  Service, and another endpoint's resolution falls back to that same Service as its ingress LB
  Service
- **THEN** that single `service` node SHALL carry `labels.role = "ingress-gateway"` irrespective
  of the order in which the two endpoints are resolved

#### Scenario: Degraded chain emits no marker

- **WHEN** a routed hit's chain degrades for any precondition reason (no unique ingress identity,
  ingress identity equal to the backend identity, selected cluster's topology lacking the ingress
  Service, or the ingress Service having no backing pod in the selected cluster)
- **THEN** no `service` node SHALL carry a `role` label for that endpoint
- **AND** the emitted nodes and edges SHALL be identical to the pre-existing direct-edge shape

#### Scenario: Backend service node is never marked

- **WHEN** a routed hit resolves a backend `service` node that is not itself an ingress entry
  point
- **THEN** that node's `labels` SHALL NOT contain a `role` key
