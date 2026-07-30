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
Gateways to those reachable from the destination IPs within it, scoped to the ingress
namespace as specified below.

**Gateway candidate scoping.** A candidate Gateway SHALL reside in the **same namespace as
the ingress Service** the destination IP resolved to (which is also its ingress
Deployment's namespace). A Gateway in any other namespace SHALL NOT be a candidate, even
when its workload selector matches the ingress Deployment's pod labels — Istio's
cross-namespace gateway attachment is deliberately out of scope, and such a configuration
degrades exactly as "no Gateway serves the host" (feeding the ingress LB Service fallback
under its own requirement). Because Kubernetes guarantees name uniqueness within a
namespace, the candidate set can never contain two same-named Gateways, so gateway
identity within a resolution is unambiguous by construction. When several candidate
Gateways declare an identical, equally specific server-host pattern matching the peer
FQDN, the engine SHALL resolve through the **lexically-smallest gateway name** —
deterministic, independent of declaration order and of storage row order.

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

