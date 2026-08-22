## MODIFIED Requirements

### Requirement: Istio route resolution of global FQDN peers

The reader SHALL support an OPTIONAL Istio route-resolution engine that resolves a
global / ingress FQDN peer to the Kubernetes Service that the Istio Gateway and
VirtualService configuration routed it to **at a single instant** — the END of the
request's own time window. The engine is consulted ONLY from "Unknown-server peer-label
enrichment", at every point that would otherwise emit an external node.

The engine SHALL evaluate the configuration **as of that one instant** and SHALL NOT
resolve per-version over the window: exactly one configuration state is consulted, exactly
one outcome is produced, and the request window's start plays no part in route resolution.

The engine SHALL be configured by a route-store connection string. When that setting is
empty the engine is **disabled**, and the reader's behaviour SHALL be byte-for-byte
identical to its behaviour before this requirement existed. Disabled is the default.

**Inputs.** For one candidate endpoint the reader SHALL supply:

- the **caller cluster** — the already-resolved client pod's own cluster, used ONLY to
  derive the cluster-family key and to break candidate ties in ingress-cluster selection;
  it SHALL NOT by itself scope any route-store query;
- the **host** — the port-stripped peer address;
- the **path** — fixed to `"/"`. The service-graph metric carries no HTTP path or route
  dimension, so no per-request path is available;
- the **listener port** — derived as specified below;
- the **destination IPs** — from the `client_dns_answers` dimension. These are a
  **precondition**: when the endpoint carries no parseable destination IP, the engine
  SHALL NOT be consulted at all — no store read occurs, the endpoint falls back to the
  external node directly, and the reader SHALL record a distinct diagnostic reason for
  the skip (separate from every engine outcome);
- the **resolution instant** — the END of the build's own time window. It is fixed (not
  configurable) and is the same instant the service-graph samples are evaluated at.

**Listener-port derivation.** The port SHALL be derived by this precedence:

1. the `:<port>` suffix split from the peer-address value, when present;
2. otherwise the OPTIONAL `client_server_port` or `client_net_peer_port` dimension, when
   present;
3. otherwise the default **443**.

**Ingress-cluster selection.** The destination IPs select the ingress cluster; the caller
cluster contributes only its family key and a tie-break. For each destination IP the
engine SHALL probe the store for the candidate set `G` — the clusters whose ingress Service
carrying that IP was live at the resolution instant — and derive `F`, the subset of `G` in the
caller's cluster family (the same digit-run-collapsing family rule used by
`service-selects-pod` fan-out). Selection per IP:

1. exactly one family candidate → that cluster;
2. several family candidates → the caller's own cluster if it is among them, otherwise
   **ambiguous**;
3. no family candidate and exactly one global candidate → that cluster;
4. no family candidate and several global candidates → the caller's own cluster if it is
   among them, otherwise **ambiguous**;
5. no candidate at all → **no ingress**.

With several destination IPs, each IP SHALL be selected independently and all selections
MUST agree on one cluster; every IP yielding "no ingress" degrades as **no ingress**, and
any other combination — an ambiguous IP, disagreeing selections, or a mix of "no ingress"
and a selected cluster — degrades as **ambiguous**. Candidate sets and store snapshots SHALL
NEVER be unioned across clusters. Once a cluster is selected, every subsequent resolution
step — snapshot load, gateway narrowing, host disambiguation, translation, route matching —
SHALL operate on that single cluster only, and the engine SHALL narrow the candidate
Gateways to those reachable from the destination IPs within it.

**Store scoping.** Every route-store *snapshot* query SHALL be scoped to the selected
ingress cluster. The ONLY cross-cluster store read SHALL be the ingress probe that answers
"which clusters had an ingress Service with this IP at the resolution instant", and its result SHALL
be deterministic (deduplicated and ordered). The reader SHALL be strictly read-only
against the store: it SHALL NOT create schema and SHALL NOT write. The reader SHALL
validate the expected schema at startup and fail fast on drift, rather than silently
returning empty results.

**Store-shape tolerance.** The store is written by an exporter whose version-close
operation REWRITES the previous open row; until background merges collapse them, a stale
open row and its closing rewrite coexist. The reader SHALL NOT treat such a pair as two
live versions (deduplicating at query time), so results do not depend on merge timing.
Stored resource specs are the API server's JSON verbatim, whose field set follows the
cluster's CRD version: the reader SHALL tolerate unknown spec fields rather than failing
the query. A VirtualService binding its gateway by the bare `<name>` form (Istio shorthand
for a gateway in the VirtualService's own namespace) SHALL bind exactly as the qualified
`<namespace>/<name>` form does.

**Version selection (as-of).** Every resource version the engine consults — ingress Service,
ingress Deployment, Gateway, VirtualService, backend Service — SHALL be the version **live at
the resolution instant**: its validity interval starts at or before that instant and ends
strictly after it. Versions that ended at or before the instant, and versions that begin after
it, SHALL NOT participate in resolution, translation, or ingress identification. The
duplicate-row discipline above SHALL be applied BEFORE the liveness test, so a stale open row
whose closing rewrite has not yet merged can never present itself as the live version.

**Resolution.** A hit yields a destination `(cluster, namespace, service)`, where the
cluster is the engine-selected ingress cluster. The reader SHALL resolve that triple
through the SAME same-cluster Service-node resolution every other path uses — anchored on
the **selected ingress cluster**, materialising AT MOST ONE service node iff that cluster
holds the Service in topology, with the same cross-cluster `service-selects-pod` fan-out
over its family. Because the selected cluster may differ from the caller's, the resulting
`pod-calls-service` edge MAY cross clusters, and the edge-type registry SHALL declare
`may_cross_cluster: true` for `pod-calls-service` (connection-string-resolved edges remain
intra-cluster by construction; per-edge cross-cluster status is still derived by comparing
the resolved endpoints' clusters). The destination's port and DestinationRule subset SHALL
be discarded. **No new node type, no new edge type, no new node attribute, and no new
`labels` key** are introduced.

**Degradation.** Every failure — engine disabled, endpoint carrying no destination IPs,
store error, no candidate ingress cluster for the IPs, an ambiguous candidate set, no
Gateway serving the host, no route matched, no listener on the derived port, or a
resolution timeout — SHALL fall back to the external node specified in "Unknown-server
peer-label enrichment". Route resolution SHALL NEVER fail a build. The reader SHALL record
distinct diagnostic reasons for a "no listener on the derived port" outcome (separate from
"no route matched", so a mis-derived port is diagnosable), for a "no candidate ingress
cluster" outcome, and for an "ambiguous ingress cluster" outcome.

**Determinism.** Route resolution SHALL be performed outside the pure service-graph parse
and its results supplied to the parse as a prefetched index, so the emitted graph remains a
deterministic function of the upstream data and the resolved destinations.

#### Scenario: Global FQDN resolves to its routed Service

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint's peer address is
  `api.example.com` with a destination IP selecting the caller's own cluster as ingress,
  whose Istio Gateway and VirtualService route the host to Service `checkout` in namespace
  `shop` at the resolution instant
- **THEN** the reader SHALL emit a `service` node for `(selected cluster, shop, checkout)`
- **AND** a `pod-calls-service` edge from the client pod to it
- **AND** SHALL NOT emit `external/api.example.com`

#### Scenario: Listener port taken from the peer address

- **WHEN** the peer address is `api.example.com:8080`
- **THEN** the host SHALL be `api.example.com` and the derived listener port SHALL be `8080`

#### Scenario: Listener port defaults to 443

- **WHEN** the peer address carries no port and neither `client_server_port` nor
  `client_net_peer_port` is present
- **THEN** the derived listener port SHALL be `443`

#### Scenario: No listener on the derived port degrades to external

- **WHEN** the selected ingress cluster's Gateway serves the host but declares no routable
  HTTP listener on the derived port
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>`
- **AND** SHALL record a diagnostic reason distinct from the "no route matched" reason

#### Scenario: The server owning the host on the port is selected

- **WHEN** the selected ingress cluster's Gateway declares two TLS-terminated HTTPS servers
  on the derived port — one whose hosts match the peer FQDN and one whose hosts do not —
  in either declaration order
- **THEN** the reader SHALL resolve the request through the RouteConfiguration of the
  server whose hosts most-specifically match the peer FQDN (Istio exact/wildcard
  semantics, any `<ns>/` binding prefix stripped before matching)

#### Scenario: No server on the derived port serves the host

- **WHEN** the selected ingress cluster's Gateway declares servers on the derived port but
  none of their hosts match the peer FQDN
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>`
- **AND** SHALL record a diagnostic reason distinct from both the "no listener on the
  derived port" reason and the "no route matched" reason

#### Scenario: Store failure never fails the build

- **WHEN** the route store is unreachable or returns an error while resolving an endpoint
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>` for that endpoint
- **AND** the build SHALL complete successfully

#### Scenario: Destination IP narrows the candidate Gateways within the selected cluster

- **WHEN** `client_dns_answers` supplies a destination IP that, within the selected ingress
  cluster, reaches exactly one of two Gateways whose server hosts both match the peer FQDN
- **THEN** the reader SHALL resolve the host against that Gateway only

#### Scenario: No destination IPs means the engine is not consulted

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint's peer address
  would fall external, but the series carries no parseable `client_dns_answers` IP
- **THEN** the engine SHALL NOT be consulted (no store read for this endpoint)
- **AND** the reader SHALL emit `external/<raw_peer_address_value>` exactly as when route
  resolution is disabled
- **AND** SHALL record a diagnostic reason distinct from every engine outcome

#### Scenario: Same-family ingress candidate wins over a cross-family one

- **WHEN** a destination IP's candidate ingress clusters are one cluster in the caller's
  family and one cluster outside it
- **THEN** the engine SHALL select the same-family cluster

#### Scenario: Caller breaks a same-family ingress-IP collision

- **WHEN** a destination IP's candidate ingress clusters include the caller's own cluster
  and a family sibling
- **THEN** the engine SHALL select the caller's own cluster

#### Scenario: Unresolvable ingress-cluster tie degrades to external

- **WHEN** a destination IP's candidate ingress clusters are two family siblings, neither
  of which is the caller's own cluster
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>`
- **AND** the reader SHALL record the "ambiguous ingress cluster" diagnostic reason

#### Scenario: No cluster serves the destination IP

- **WHEN** no cluster had an ingress Service carrying any of the destination IPs live at the
  resolution instant
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>`
- **AND** the reader SHALL record the "no candidate ingress cluster" diagnostic reason

#### Scenario: Disagreeing multi-IP selections degrade to external

- **WHEN** `client_dns_answers` supplies two IPs whose independent selections pick two
  different ingress clusters
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>` with the
  "ambiguous ingress cluster" diagnostic reason

#### Scenario: Cross-cluster ingress resolves to a Service in the sibling cluster

- **WHEN** the caller pod lives in cluster A and the destination IP selects ingress
  cluster B, whose Gateway and VirtualService route the host to Service `payments` in
  namespace `shop`, and B holds that Service in topology
- **THEN** the reader SHALL emit the `service` node `B/shop/payments`
- **AND** a `pod-calls-service` edge from the cluster-A client pod to it (a cross-cluster
  edge)
- **AND** SHALL NOT emit an external node for the peer

#### Scenario: A rewritten (closed) version does not double-count

- **WHEN** the store carries both a stale open row (far-future `valid_to`) and its closing
  rewrite (same version key, higher ingest sequence) for one Gateway version
- **THEN** the reader SHALL see exactly one live version per instant, regardless of
  whether the store's background merge has run

#### Scenario: Unknown spec fields do not fail the query

- **WHEN** a stored Gateway or VirtualService spec carries a field unknown to the reader's
  compiled Istio API version
- **THEN** the reader SHALL parse the known fields and resolve routing from them

#### Scenario: Bare gateway reference binds

- **WHEN** a VirtualService in the gateway's own namespace lists the gateway by bare name
  in `spec.gateways`
- **THEN** its routes SHALL bind to that gateway exactly as a qualified
  `<namespace>/<name>` reference would

#### Scenario: Only the configuration live at the resolution instant is used

- **WHEN** the selected ingress cluster's store holds one Gateway version that ended inside the
  request window and a newer version live at the window's end, and only the newer version serves
  the peer FQDN on the derived port
- **THEN** the engine SHALL resolve the request through the newer version
- **AND** SHALL NOT consult the version that ended before the resolution instant

#### Scenario: Configuration that stopped serving the host degrades to external

- **WHEN** a Gateway routed the peer FQDN earlier in the request window but the version live at
  the window's end no longer serves that host
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>` with the
  corresponding miss diagnostic reason

#### Scenario: Cross-namespace Gateway is not a candidate

- **WHEN** the selected ingress cluster's ingress Service and Deployment live in namespace
  `istio-system`, and the only Gateway whose selector matches the ingress Deployment's pod
  labels and whose server hosts serve the peer FQDN lives in namespace `team-b`
- **THEN** that Gateway SHALL NOT be a candidate and the pipeline SHALL miss with the
  "no Gateway serves the host" outcome
- **AND** the ingress LB Service fallback SHALL still apply per its own requirement

#### Scenario: Identical host patterns resolve to the lexically-smallest gateway

- **WHEN** two candidate Gateways in the ingress namespace declare the identical, equally
  specific server-host pattern matching the peer FQDN, in either declaration or storage
  order
- **THEN** the engine SHALL resolve the request through the Gateway with the
  lexically-smallest name

### Requirement: Ingress LB Service fallback for unresolved global FQDN peers

When the Istio route-resolution engine ("Istio route resolution of global FQDN peers") has
selected an ingress cluster, loaded its snapshot at the resolution instant, and run the Gateway +
VirtualService pipeline WITHOUT producing a hit AND without progressing past gateway
resolution (the pipeline's miss is the "no gateway serves the host" outcome — the
signature of a non-Istio ingress such as nginx, whose Hop 3 finds no Gateway CR), the engine
SHALL — before returning that miss — attempt one fallback: resolve the destination IP set to a
**unique ingress LB Service** inside the already-selected ingress cluster's loaded snapshot.

The fallback SHALL NOT run when the pipeline produced a hit (a routed `hit` always wins), SHALL
NOT run when the miss is any deeper pipeline outcome ("no listener on the derived
port", "no server for host", "no route matched" — an Istio Gateway DID serve the host and its
diagnostic reason MUST NOT be masked by an LB-entry-point edge), and CANNOT run when
ingress-cluster selection itself failed (the "no candidate ingress cluster" and "ambiguous
ingress cluster" outcomes return before any snapshot is loaded).

**Candidate set (as-of the resolution instant).** For each destination IP, the candidates are
every ingress Service version in the loaded (selected-cluster) snapshot that is **live at the
resolution instant** and whose ingress IPs (external or load-balancer) contain that IP,
deduplicated to distinct `(namespace, name)` identities. An identity that was replaced BEFORE
the resolution instant therefore does not count — only owners live at that instant do. The
fallback SHALL NOT read the store again — it operates on the rows already loaded for the
pipeline.

**Uniqueness rule (order-free over IPs).** Per destination IP the identity set `S_ip` is
computed independently; then:

1. any `|S_ip| > 1` → the fallback degrades as **ambiguous ingress Service**;
2. otherwise any `|S_ip| == 0` → the fallback yields nothing and the engine SHALL return the
   Istio pipeline's own miss unchanged;
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
  an ingress cluster whose snapshot holds exactly one ingress LB Service carrying that IP, with
  no Istio Gateway CR reachable from the IP (the pipeline misses at gateway resolution)
- **THEN** the reader SHALL emit a `service` node for that `(selected cluster, namespace,
  service)`
- **AND** a `pod-calls-service` edge from the client pod to it
- **AND** the family-wide `service-selects-pod` fan-out to the Service's backing pods (the
  ingress controller pods)
- **AND** SHALL NOT emit an external node for the peer

#### Scenario: A deep Istio miss is not masked by the fallback

- **WHEN** the selected ingress cluster's Gateway serves the host but the pipeline misses at a
  stage past gateway resolution (no listener on the derived port, no server for the host, or no
  route matched), and the snapshot also holds an ingress LB Service carrying the destination IP
- **THEN** the engine SHALL return that deeper miss outcome and diagnostic reason unchanged
- **AND** SHALL NOT resolve the ingress LB Service

#### Scenario: A routed hit always beats the fallback

- **WHEN** the selected ingress cluster's Gateway and VirtualService route the host to a backend
  Service at the resolution instant, and the snapshot also holds an ingress LB Service carrying
  the destination IP
- **THEN** the reader SHALL resolve the routed backend Service (`hit`), not the LB Service

#### Scenario: Two LB Services on one IP degrade to external

- **WHEN** the pipeline misses and the selected cluster's snapshot holds two differently-named
  ingress LB Services, both live at the resolution instant, whose ingress IPs both contain the
  destination IP
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>`
- **AND** the reader SHALL record the "ambiguous ingress Service" diagnostic reason

#### Scenario: A superseded identity does not make the IP ambiguous

- **WHEN** the pipeline misses and one ingress LB Service identity carrying the destination IP
  ended BEFORE the resolution instant while a differently-named one carrying the same IP is live
  at it
- **THEN** the fallback SHALL resolve the identity live at the resolution instant
- **AND** SHALL NOT report the "ambiguous ingress Service" diagnostic reason

#### Scenario: Version churn of one identity still resolves

- **WHEN** the pipeline misses and the store holds several versions (rows) of the SAME
  `(namespace, name)` ingress LB Service carrying the destination IP, one of them live at the
  resolution instant
- **THEN** the fallback SHALL resolve that single identity

#### Scenario: No LB Service keeps the pipeline miss

- **WHEN** the pipeline misses and no ingress Service live at the resolution instant in the
  selected cluster carries the destination IP
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

### Requirement: Full ingress chain for routed global-FQDN hits

When the Istio route-resolution engine produces a routed hit ("Istio route resolution of global
FQDN peers"), the engine SHALL additionally attempt to recover the identity of the **ingress LB
Service** the destination IP set uniquely maps to inside the already-selected ingress cluster's
loaded snapshot, using the same as-of identity-dedup rule as the "Ingress LB Service fallback"
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
  ingress cluster whose snapshot uniquely maps the IP set to one ingress LB Service, the pipeline
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
  ingress LB Service identity live at the resolution instant in the selected cluster (or some IP
  maps to none)
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
