## ADDED Requirements

### Requirement: Full ingress chain for routed global-FQDN hits

When the Istio route-resolution engine produces a routed hit ("Istio route resolution of global
FQDN peers"), the engine SHALL additionally attempt to recover the identity of the **ingress LB
Service** the destination IP set uniquely maps to inside the already-selected ingress cluster's
loaded window, using the same window-wide identity-dedup rule as the "Ingress LB Service fallback"
requirement (per-IP distinct `(namespace, name)` sets; same-IP collision or disagreeing
singletons → no identity; any IP with zero rows → no identity; all singletons agreeing → that
identity). Identity recovery SHALL NOT read the store again and SHALL NEVER demote the hit: an
ambiguous or absent identity leaves the hit's destination without an ingress identity and the
reader emits the pre-existing direct shape.

**Chain emission.** When the hit's destination carries an ingress identity AND every chain
precondition below holds, the reader SHALL emit — instead of the direct
`caller pod → backend service` edge — the full chain:

- a `service` node for the ingress LB Service in the selected ingress cluster, with
  `service-selects-pod` edges to **the selected cluster's own backing pods only** (locked-cluster
  fan-out — NOT the family-wide union: an LB IP is a per-cluster address, so a family sibling's
  same-named Service is not behind it);
- one `pod-calls-service` edge from the client pod to the ingress `service` node (this edge MAY
  cross clusters, exactly as the direct edge it replaces);
- one synthesized `pod-calls-service` edge from EACH of those locked-cluster ingress backing pods
  to the backend `service` node, carrying the ingress cluster as the edge's `cluster` label (the
  client side is a pod in that cluster);
- the backend `service` node and its family-wide `service-selects-pod` fan-out, unchanged from
  the pre-existing hit resolution.

The direct `caller pod → backend service` edge SHALL NOT be emitted alongside the chain. A
synthesized ingress-pod→backend edge whose `(source pod, target service)` pair already exists as
a trace-derived edge SHALL be skipped — the traced edge wins (the two would otherwise share one
deterministic edge ID). Synthesized-edge emission SHALL be deterministic (order-free over the
endpoint set; idempotent under repeated series sharing one route key). **No new node type, no new
edge type, no new node attribute, and no new `labels` key** are introduced.

**Chain preconditions (each failure degrades to the pre-existing direct shape).** The chain SHALL
be emitted only when ALL hold:

1. the destination carries an ingress identity (recovered as above);
2. the ingress identity differs from the backend destination's `(namespace, service)` in the
   selected cluster (a destination that IS the ingress entry point keeps the direct shape);
3. the selected ingress cluster holds the ingress Service in topology;
4. that Service has at least one backing pod in the selected cluster's endpoints (an empty middle
   would disconnect the caller from the backend).

Degradation SHALL be observable via a debug-level diagnostic only — it SHALL NOT emit an external
node, SHALL NOT count toward external-fallback reasons, and SHALL leave no partially-materialised
ingress node or edge. A topology miss on the **backend** Service keeps the existing "selected
cluster lacks the Service" external path, with the ingress Service never materialised. Route
resolution SHALL still NEVER fail a build.

**Locked-cluster fan-out for the LB fallback.** The "Ingress LB Service fallback" requirement's
resolution SHALL likewise fan out `service-selects-pod` edges over the selected ingress cluster's
own backing pods only (superseding its family-wide fan-out): the fallback answers "which ingress
LB Service owns this IP", and the pods behind that IP are the owning cluster's endpoints. No
synthesized pod-calls-service edges are emitted on the fallback path (there is no routed backend
behind the LB entry point).

#### Scenario: Routed hit with recoverable ingress identity emits the full chain

- **WHEN** route resolution is enabled, a `server="unknown"` endpoint's destination IP selects an
  ingress cluster whose window uniquely maps the IP set to one ingress LB Service, the pipeline
  routes `(host, path, port)` to a backend Service, and the selected cluster's topology holds
  both Services with at least one ingress backing pod
- **THEN** the reader SHALL emit `service` nodes for the ingress LB Service and the backend
  Service
- **AND** a `pod-calls-service` edge from the client pod to the ingress `service` node
- **AND** `service-selects-pod` edges from the ingress node to the selected cluster's own ingress
  backing pods only
- **AND** one synthesized `pod-calls-service` edge from each such ingress pod to the backend
  `service` node
- **AND** the family-wide `service-selects-pod` fan-out from the backend node
- **AND** SHALL NOT emit a direct `pod-calls-service` edge from the client pod to the backend
  node, and SHALL NOT emit an external node

#### Scenario: Ambiguous ingress identity keeps the hit as a direct edge

- **WHEN** the pipeline produces a routed hit but the destination IPs map to more than one
  ingress LB Service identity in the selected cluster's window (or some IP maps to none)
- **THEN** the reader SHALL resolve the hit exactly as without this change: a direct
  `pod-calls-service` edge from the client pod to the backend `service` node and the family-wide
  backend fan-out
- **AND** the engine SHALL still report the routed hit (never a miss)

#### Scenario: Ingress Service missing from topology degrades to the direct edge

- **WHEN** a routed hit carries an ingress identity but the selected ingress cluster does not
  hold that Service in topology
- **THEN** the reader SHALL emit the direct-edge shape
- **AND** SHALL NOT emit an ingress `service` node or an external node for the peer

#### Scenario: Ingress Service with no backing pods degrades to the direct edge

- **WHEN** a routed hit carries an ingress identity, the selected cluster holds the ingress
  Service in topology, but the Service has no backing pods in that cluster's endpoints
- **THEN** the reader SHALL emit the direct-edge shape
- **AND** SHALL NOT emit an ingress `service` node

#### Scenario: Destination equal to the ingress identity keeps the direct edge

- **WHEN** a routed hit's backend destination `(namespace, service)` equals the recovered ingress
  identity in the selected cluster
- **THEN** the reader SHALL emit the direct-edge shape with that single `service` node

#### Scenario: Backend missing from topology stays external, ingress never materialised

- **WHEN** a routed hit carries an ingress identity but the selected ingress cluster does not
  hold the BACKEND Service in topology
- **THEN** the endpoint SHALL fall back to the external node with the existing "selected cluster
  lacks the Service" diagnostic reason
- **AND** SHALL NOT emit an ingress `service` node or any `service-selects-pod` edge

#### Scenario: Traced edge wins over the synthesized edge

- **WHEN** the chain is emitted and the service graph also carries a trace-derived
  `pod-calls-service` edge from one of the ingress backing pods to the same backend `service`
  node
- **THEN** the response SHALL carry exactly one edge for that `(pod, service)` pair

#### Scenario: LB fallback fans out over the selected cluster only

- **WHEN** the ingress LB Service fallback resolves an ingress Service that a family sibling
  cluster also holds under the same `(namespace, name)` with its own backing pods
- **THEN** the reader SHALL emit `service-selects-pod` edges to the selected ingress cluster's
  backing pods only
- **AND** SHALL NOT emit edges to the sibling cluster's pods

#### Scenario: Chain ingress fan-out ignores family siblings while the backend keeps them

- **WHEN** the chain is emitted in a cluster family where a sibling cluster holds a same-named
  ingress Service (with its own pods) and the backend Service is also held by the sibling (with
  its own endpoints)
- **THEN** the ingress node's `service-selects-pod` edges and the synthesized
  ingress-pod→backend edges SHALL cover the selected cluster's ingress pods only
- **AND** the backend node's `service-selects-pod` fan-out SHALL still cover the family-wide
  endpoint union
