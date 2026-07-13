## MODIFIED Requirements

### Requirement: Unknown-server peer-label enrichment

The reader SHALL attempt to resolve the server side of a
`traces_service_graph_request_total` series from two additional client-recorded peer-address
labels — `client_net_peer_name` and `client_server_address` — instead of unconditionally
dropping the endpoint, when and only when `client_k8s_pod_uid` resolves to a **real topology
pod** (not a synthesised pod), the server side has no resolvable pod (`server_k8s_pod_uid`
empty, or non-empty but absent from the global pod-UID index), AND the raw `server` label is
exactly `"unknown"`. This is a narrow, additive carve-out from the sentinel-exclusion
outcome described in "Virtual sentinel endpoint exclusion (user / unknown)"; it SHALL NOT
apply when the client side is unresolved or when the server UID itself resolves to a
topology pod.

Resolution order, evaluated only under the trigger condition above:

1. If `client_net_peer_name` is non-empty, use its value as the peer address.
2. Otherwise, if `client_server_address` is non-empty, use its value as the peer address.
3. Otherwise (both empty or absent), the reader SHALL drop the endpoint — no node, no
   edge — identical to the outcome when this requirement does not apply.

When a peer address is obtained (step 1 or 2), the reader SHALL classify it:

1. Split an optional trailing `:<port>` suffix from the value (best-effort host/port split;
   a value with no splittable port yields the value unchanged as the host and no port). The
   **host** feeds classification below; the **port**, when present, feeds the listener-port
   derivation in "Istio route resolution of global FQDN peers".
2. Apply the same Kubernetes Service-DNS grammar used by connection-string resolution
   (2-label `<service>.<namespace>`, or 3-label headless `<pod>.<service>.<namespace>`
   with the leading pod-hostname dropped, `.svc[.<cluster-domain>]` suffix stripped) to
   the resulting host.
3. When the grammar in step 2 does not match AND the host is a single DNS-1123 label
   (no dots) that is not an IP literal, the reader SHALL treat it as a **bare short
   Service name resolved in the client pod's own namespace** — `(service=host,
   namespace=<client_k8s_namespace_name>)`. This is one grammar extension beyond
   connection-string resolution's own classification, and it applies ONLY within this
   requirement's trigger condition.
4. When steps 2 and 3 both fail to match AND the host is a valid IP literal (IPv4 or
   IPv6, per `net.ParseIP`), the reader SHALL look it up as a Kubernetes Service
   `ClusterIP` **within the already-resolved client pod's own (anchor) cluster only** —
   never any other cluster, including a family sibling. A match yields the `(namespace,
   service)` of the Service whose `ClusterIP` equals the host in that cluster. This is a
   second grammar extension beyond connection-string resolution's own classification,
   scoped ONLY to this requirement's trigger condition, and it is evaluated after, and
   independently of, step 3 (an IP literal never satisfies step 3, since step 3
   explicitly excludes IP literals).
5. Any other shape (multi-label non-`.svc` FQDN, an IP literal absent from the anchor
   cluster's own Service `ClusterIP` set, unparseable value) is **unresolvable** at this
   step.

When step 2, step 3, or step 4 yields a `(namespace, service)` pair, the reader SHALL
resolve it via the same same-cluster Service-node resolution used by connection-string
resolution ("Connection-string endpoint resolution", steps 3–4), anchored on the
**already-resolved client pod's own cluster** (no anchor-recovery ambiguity, since the
client side is guaranteed to be a real topology pod under this requirement's trigger
condition): AT MOST ONE service node in that cluster, materialised iff the anchor cluster
itself holds the addressed Service, with the same cross-cluster `service-selects-pod`
fan-out over every same-family cluster holding the Service. This applies identically
whether the `(namespace, service)` pair was obtained via DNS-grammar classification (step
2), bare-short-name classification (step 3), or IP-literal `ClusterIP` matching (step 4):
once identified, an IP-literal match is resolved exactly like any other classified
Service address — including the family-wide `service-selects-pod` fan-out — only the
*identification* step (step 4 itself) is restricted to the anchor cluster.

If two or more Services within the SAME anchor cluster share the identical `ClusterIP`
value (a data anomaly Kubernetes itself prevents in a healthy cluster, but the reader
stays defensive), step 4 SHALL deterministically select the Service with the
lexically-smaller `(namespace, service)` pair.

When classification (step 5) is unresolvable, OR the anchor cluster does not hold the
addressed Service, the reader SHALL — **before** falling back to an external node —
consult the Istio route-resolution engine as specified in "Istio route resolution of
global FQDN peers". This route-resolution step SHALL be reached from EVERY path that would
otherwise produce an external node under this requirement, without exception. In
particular, a global / ingress FQDN such as `api.example.com` is a 3-label host and is
therefore *successfully* classified by step 2 (as service `example` in namespace `com`)
before failing the anchor-cluster membership test — so it reaches route resolution via the
"anchor cluster does not hold the addressed Service" path, NOT via the step-5 unresolvable
path. An implementation that consults the engine only on step-5 unresolvability does NOT
satisfy this requirement.

When route resolution is disabled, or does not yield a destination, the reader SHALL fall
back to an **external** node from the RAW, unstripped peer-address value (not the
port-stripped host):

- `id`     = `external/<raw_peer_address_value>`
- `name`   = `<raw_peer_address_value>` (verbatim — no normalisation, no trimming)
- `type`   = `"external"`
- `labels` = `{}` (empty map — no `cluster` key)

The resulting edge follows the existing generic rules unchanged: `type` is
`pod-calls-service` when the resolved target is a service node, otherwise `pod-calls-pod`;
`labels.cluster` is present (the client pod's cluster) because the client side is a
resolved pod. No new node type and no new edge type are introduced by this requirement.

#### Scenario: Global FQDN reaches route resolution via the anchor-lacks-service path

- **WHEN** a `server="unknown"` series has a client resolving to a real topology pod and
  `client_net_peer_name="api.example.com"`
- **THEN** DNS-grammar classification (step 2) succeeds, yielding `(namespace="com",
  service="example")`
- **AND** the anchor cluster does not hold that Service, so same-cluster Service-node
  resolution misses
- **AND** the reader SHALL consult the route-resolution engine before emitting any external
  node

#### Scenario: Route resolution disabled falls back to external unchanged

- **WHEN** route resolution is not configured
- **AND** a `server="unknown"` endpoint's peer address is unresolvable by steps 2–4, or the
  anchor cluster does not hold the classified Service
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>` with `labels = {}`,
  byte-for-byte identical to the behaviour before route resolution existed

#### Scenario: In-cluster classification is unaffected by route resolution

- **WHEN** a `server="unknown"` endpoint's peer address classifies to a `(namespace,
  service)` pair the anchor cluster DOES hold — via `.svc` DNS, bare short name, or
  ClusterIP literal
- **THEN** the reader SHALL resolve it to that service node exactly as before
- **AND** the route-resolution engine SHALL NOT be consulted

## ADDED Requirements

### Requirement: Istio route resolution of global FQDN peers

The reader SHALL support an OPTIONAL Istio route-resolution engine that resolves a
global / ingress FQDN peer to the Kubernetes Service that the Istio Gateway and
VirtualService configuration routed it to during the request's own time window. The engine
is consulted ONLY from "Unknown-server peer-label enrichment", at every point that would
otherwise emit an external node.

The engine SHALL be configured by a route-store connection string. When that setting is
empty the engine is **disabled**, and the reader's behaviour SHALL be byte-for-byte
identical to its behaviour before this requirement existed. Disabled is the default.

**Inputs.** For one candidate endpoint the reader SHALL supply:

- the **anchor cluster** — the already-resolved client pod's own cluster;
- the **host** — the port-stripped peer address;
- the **path** — fixed to `"/"`. The service-graph metric carries no HTTP path or route
  dimension, so no per-request path is available;
- the **listener port** — derived as specified below;
- the **destination IPs** — from the OPTIONAL `client_dns_answers` dimension, empty when
  absent;
- the **time window** — the build's own `[start, end]`.

**Listener-port derivation.** The port SHALL be derived by this precedence:

1. the `:<port>` suffix split from the peer-address value, when present;
2. otherwise the OPTIONAL `client_server_port` or `client_net_peer_port` dimension, when
   present;
3. otherwise the default **443**.

**Destination-IP handling.** When `client_dns_answers` supplies at least one IP, the engine
SHALL narrow the candidate Gateways to those reachable from that destination IP before
disambiguating the host among them. When it is absent, the engine SHALL resolve the host
against all of the anchor cluster's Gateways. Both modes are valid; the absence of the
dimension costs precision, not correctness.

**Store scoping.** Every route-store query SHALL be scoped to the anchor cluster. The
reader SHALL be strictly read-only against the store: it SHALL NOT create schema and SHALL
NOT write. The reader SHALL validate the expected schema at startup and fail fast on drift,
rather than silently returning empty results.

**Store-shape tolerance.** The store is written by an exporter whose version-close
operation REWRITES the previous open row; until background merges collapse them, a stale
open row and its closing rewrite coexist. The reader SHALL NOT treat such a pair as two
live versions (deduplicating at query time), so results do not depend on merge timing.
Stored resource specs are the API server's JSON verbatim, whose field set follows the
cluster's CRD version: the reader SHALL tolerate unknown spec fields rather than failing
the query. A VirtualService binding its gateway by the bare `<name>` form (Istio shorthand
for a gateway in the VirtualService's own namespace) SHALL bind exactly as the qualified
`<namespace>/<name>` form does.

**Resolution.** A hit yields a destination `(namespace, service)`. The reader SHALL resolve
that pair through the SAME same-cluster Service-node resolution every other path uses —
anchored on the anchor cluster, materialising AT MOST ONE service node iff that cluster
holds the Service, with the same cross-cluster `service-selects-pod` fan-out. The
destination's port and DestinationRule subset SHALL be discarded. **No new node type, no new
edge type, no new node attribute, and no new `labels` key** are introduced.

**Degradation.** Every failure — engine disabled, store error, no Gateway serving the host,
no route matched, no listener on the derived port, or a resolution timeout — SHALL fall back
to the external node specified in "Unknown-server peer-label enrichment". Route resolution
SHALL NEVER fail a build. The reader SHALL record a distinct diagnostic reason for a
"no listener on the derived port" outcome, separate from a "no route matched" outcome, so a
mis-derived port is diagnosable.

**Determinism.** Route resolution SHALL be performed outside the pure service-graph parse
and its results supplied to the parse as a prefetched index, so the emitted graph remains a
deterministic function of the upstream data and the resolved destinations.

#### Scenario: Global FQDN resolves to its routed Service

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint's peer address is
  `api.example.com`, which the anchor cluster's Istio Gateway and VirtualService route to
  Service `checkout` in namespace `shop` during the request window
- **THEN** the reader SHALL emit a `service` node for `(anchor cluster, shop, checkout)`
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

- **WHEN** the anchor cluster's Gateway serves the host but declares no routable HTTP
  listener on the derived port
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>`
- **AND** SHALL record a diagnostic reason distinct from the "no route matched" reason

#### Scenario: Store failure never fails the build

- **WHEN** the route store is unreachable or returns an error while resolving an endpoint
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>` for that endpoint
- **AND** the build SHALL complete successfully

#### Scenario: Destination IP narrows the candidate Gateways

- **WHEN** `client_dns_answers` supplies a destination IP that reaches exactly one of two
  Gateways whose server hosts both match the peer FQDN
- **THEN** the reader SHALL resolve the host against that Gateway only

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

#### Scenario: Missing destination IP resolves over all Gateways

- **WHEN** `client_dns_answers` is absent
- **THEN** the reader SHALL resolve the host against all of the anchor cluster's Gateways,
  selecting the most-specific host match
