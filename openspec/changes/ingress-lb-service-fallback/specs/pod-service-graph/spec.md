## ADDED Requirements

### Requirement: Ingress LB Service fallback for unresolved global FQDN peers

When the Istio route-resolution engine ("Istio route resolution of global FQDN peers") has
selected an ingress cluster, loaded its window, and run the Gateway + VirtualService pipeline
over every segment WITHOUT producing a hit, the engine SHALL — before returning the pipeline's
miss — attempt one fallback: resolve the destination IP set to a **unique ingress LB Service**
inside the already-selected ingress cluster's loaded window.

The fallback SHALL NOT run when the pipeline produced a hit (a routed `hit` always wins), and
CANNOT run when ingress-cluster selection itself failed (the "no candidate ingress cluster" and
"ambiguous ingress cluster" outcomes return before any window is loaded).

**Candidate set (window-wide, no per-instant evaluation).** For each destination IP, the
candidates are every ingress Service version in the loaded (selected-cluster) window whose
validity overlaps the request's `[start, end]` and whose ingress IPs (external or load-balancer)
contain that IP, deduplicated to distinct `(namespace, name)` identities. Multiple versions of
the same identity therefore count once; an identity change within the window (a Service deleted
and a differently-named one created on the same IP) yields two identities. The fallback SHALL
NOT read the store again — it operates on the rows already loaded for the pipeline.

**Uniqueness rule (order-free over IPs).** Per destination IP the identity set `S_ip` is
computed independently; then:

1. any `|S_ip| > 1` → the fallback degrades as **ambiguous ingress Service**;
2. otherwise any `|S_ip| == 0` → the fallback yields nothing and the engine SHALL return the
   Istio pipeline's own (deepest) miss unchanged;
3. otherwise all (singleton) candidates MUST be the same identity → that identity is the
   fallback destination; disagreeing identities degrade as **ambiguous ingress Service**.

No lexicographic or recency tie-break SHALL be applied — ambiguity degrades rather than guesses.

**Resolution.** A fallback destination yields `(cluster, namespace, service)` with the cluster
being the engine-selected ingress cluster, reported under a distinct engine outcome (separate
from `hit`). The reader SHALL resolve it through the SAME same-cluster Service-node resolution
as a routed hit — anchored on the selected ingress cluster, AT MOST ONE service node iff that
cluster holds the Service in topology (a topology miss follows the existing
"selected cluster lacks the Service" external path), the same cross-cluster
`service-selects-pod` fan-out over its family, and a `pod-calls-service` edge that MAY cross
clusters. The destination's port and subset are absent/discarded. The fallback's semantics are
deliberately coarse — "which ingress LB Service owns this IP" — and SHALL ignore the request's
host, path, and derived listener port. **No new node type, no new edge type, no new node
attribute, and no new `labels` key** are introduced.

**Degradation.** The **ambiguous ingress Service** outcome SHALL fall back to the external node
with its own diagnostic reason, distinct from every existing engine reason. When the fallback
yields nothing (rule 2), the engine's outcome and diagnostic reason SHALL be byte-for-byte the
ones the pipeline would have returned without the fallback. Route resolution SHALL still NEVER
fail a build.

#### Scenario: nginx ingress resolves to its LB Service

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint's destination IP selects
  an ingress cluster whose window holds exactly one ingress LB Service carrying that IP, with no
  Istio Gateway CR reachable from the IP (the pipeline misses every segment)
- **THEN** the reader SHALL emit a `service` node for that `(selected cluster, namespace,
  service)`
- **AND** a `pod-calls-service` edge from the client pod to it
- **AND** the family-wide `service-selects-pod` fan-out to the Service's backing pods (the
  ingress controller pods)
- **AND** SHALL NOT emit an external node for the peer

#### Scenario: A routed hit always beats the fallback

- **WHEN** the selected ingress cluster's Gateway and VirtualService route the host to a backend
  Service in some segment, and the window also holds an ingress LB Service carrying the
  destination IP
- **THEN** the reader SHALL resolve the routed backend Service (`hit`), not the LB Service

#### Scenario: Two LB Services on one IP degrade to external

- **WHEN** the pipeline misses and the selected cluster's window holds two differently-named
  ingress LB Services whose ingress IPs both contain the destination IP
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>`
- **AND** the reader SHALL record the "ambiguous ingress Service" diagnostic reason

#### Scenario: Identity change within the window degrades to external

- **WHEN** the pipeline misses and the window holds one ingress LB Service identity valid early
  in the window and a differently-named one valid later, both carrying the destination IP
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>` with the
  "ambiguous ingress Service" diagnostic reason

#### Scenario: Version churn of one identity still resolves

- **WHEN** the pipeline misses and the window holds several versions (rows) of the SAME
  `(namespace, name)` ingress LB Service carrying the destination IP
- **THEN** the fallback SHALL resolve that single identity

#### Scenario: No LB Service keeps the pipeline miss

- **WHEN** the pipeline misses and no ingress Service in the selected cluster's window carries
  the destination IP
- **THEN** the engine SHALL return the pipeline's own miss outcome and diagnostic reason,
  byte-for-byte as without the fallback

#### Scenario: Disagreeing per-IP identities degrade to external

- **WHEN** the pipeline misses and two destination IPs each resolve to exactly one ingress LB
  Service, but to different identities
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>` with the
  "ambiguous ingress Service" diagnostic reason

#### Scenario: Fallback hit whose Service is absent from topology

- **WHEN** the fallback resolves an ingress LB Service but the selected cluster does not hold
  that Service in topology
- **THEN** the endpoint SHALL fall back to the external node with the existing "selected cluster
  lacks the Service" diagnostic reason
