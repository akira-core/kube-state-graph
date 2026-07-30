## ADDED Requirements

### Requirement: Route-resolution snapshot integrity

The configuration state the route-resolution engine translates SHALL name each resource
version exactly once, regardless of how many destination IPs the request carries.

When the request carries several destination IPs, the engine loads the selected ingress
cluster's configuration once per IP and unions the results. Two IPs served by the same
ingress Service — a dual-stack Service publishing an IPv4 and an IPv6 address, or several
addresses of one load balancer — return overlapping rows. The union SHALL deduplicate by
resource identity (cluster, namespace, name, and version start), and the per-gateway
translate input SHALL contain at most one configuration entry per `(kind, namespace, name)`
and at most one registry entry per backend Service.

A multi-IP request SHALL therefore produce the same outcome a single-IP request against
the same configuration produces. A duplicated resource SHALL NOT surface as a translation
error, and SHALL NOT cause the endpoint to fall back to an external node.

#### Scenario: Dual-stack ingress resolves

- **GIVEN** an ingress Service whose ingress addresses include both an IPv4 and an IPv6 address
- **AND** a Gateway and VirtualService serving the requested host
- **WHEN** the endpoint's destination IPs contain both addresses
- **THEN** the engine resolves the routed backend Service, identically to a request carrying either address alone

#### Scenario: Overlapping loads do not error

- **GIVEN** two destination IPs whose configuration loads return the same VirtualService version
- **WHEN** the engine builds the translate input for the selected gateway
- **THEN** that VirtualService appears exactly once and translation succeeds

### Requirement: Backend destination host resolution

The engine SHALL resolve a VirtualService route's `destination.host` to a backend Service
identity using the same rules istiod itself applies, so that the destination the engine
reports is the destination a real mesh would route to.

A `destination.host` containing no dot is a short name and SHALL be resolved within the
owning VirtualService's namespace to the in-cluster Service FQDN
(`<name>.<namespace>.svc.<cluster domain>`). The resolved identity SHALL be used
consistently for the backend Service lookup, the translation registry, and the parse of
the resulting Envoy cluster string.

A `destination.host` containing a dot SHALL NOT be expanded — istiod treats it as already
qualified. A dotted value that is not a full in-cluster Service FQDN (for example
`checkout.shop` or `checkout.shop.svc`) therefore names no Service in the registry; the
engine SHALL NOT infer a Service identity from it, and the endpoint SHALL fall back to an
external node as it does for any unresolvable destination.

An Envoy cluster string's host SHALL be parsed as a Service identity only when it is a
well-formed `<name>.<namespace>.svc.<cluster domain>` with exactly two leading labels; any
other shape SHALL be treated as unresolvable rather than parsed by prefix.

#### Scenario: Bare short destination host resolves

- **GIVEN** a VirtualService in namespace `shop` routing to `destination: {host: checkout}`
- **AND** a Service `checkout` in namespace `shop`
- **WHEN** the engine resolves a request routed by that VirtualService
- **THEN** the destination is the Service `checkout` in namespace `shop`

#### Scenario: Dotted relative destination host does not resolve

- **GIVEN** a VirtualService routing to `destination: {host: checkout.shop}`
- **WHEN** the engine resolves a request routed by that VirtualService
- **THEN** no Service identity is inferred and the endpoint falls back to an external node

#### Scenario: Per-pod headless host is not a Service identity

- **WHEN** an Envoy cluster string names the host `mysql-0.mysql.db.svc.cluster.local`
- **THEN** it is treated as unresolvable rather than parsed as the Service `mysql-0` in namespace `mysql`

### Requirement: Deterministic ingress 3-hop selection

The IP-to-Gateway 3-hop SHALL be a pure function of the configuration state at the
resolution instant, independent of the order in which the store returns rows.

**Hop 1 (IP to ingress Service).** When more than one distinct Service identity is live at
the instant carrying the destination IP, the hop SHALL degrade rather than select one:
no candidate gateways are produced, matching the ambiguity rule the ingress LB Service
identity dedup already applies to the same situation. A single identity with several
versions is not an ambiguity — version liveness resolves it before this rule applies.

**Hop 2 (ingress Service selector to workload labels).** When more than one ingress
Deployment satisfies the Service selector — the normal state during a revision-based
canary gateway upgrade — the hop SHALL take the **union** of their pod labels, matching
the label union the store query itself computes. It SHALL NOT select one Deployment.

**Gateway identity.** A candidate Gateway SHALL be identified by `(namespace, name)`
throughout resolution, including when its configuration and bound VirtualServices are
rebuilt for translation. A same-named Gateway in another namespace present in the loaded
rows SHALL NOT be selectable, and SHALL NOT contribute its bound VirtualServices to the
translated configuration.

#### Scenario: Same-named gateway in another namespace is never translated

- **GIVEN** live Gateways `istio-system/public-gw` and `nginx-ingress/public-gw` in the loaded rows
- **AND** the ingress Service selecting the candidate lives in `istio-system`
- **WHEN** the engine rebuilds the gateway's configuration for translation
- **THEN** only `istio-system/public-gw` and the VirtualServices bound to it are translated

#### Scenario: Canary gateway Deployment does not change the candidate set

- **GIVEN** two live ingress Deployments whose pod labels both satisfy the ingress Service selector
- **WHEN** the engine derives candidate Gateways
- **THEN** the candidate set is derived from the union of both Deployments' pod labels and does not depend on row order

#### Scenario: Ambiguous ingress Service identity degrades

- **GIVEN** two distinct Service identities live at the instant carrying the destination IP
- **WHEN** the engine runs the 3-hop for that IP
- **THEN** no candidate gateway is produced

### Requirement: Bounded route-resolution work per build

Route resolution SHALL NOT let its per-build cost grow without bound on the request path.

Within one build the deduplicated route keys SHALL be resolved with bounded concurrency,
and the number of keys resolved SHALL be capped. When the cap truncates the key set, the
reader SHALL log the number of dropped keys; truncation SHALL NOT be silent. Dropped keys
fall back to their pre-existing external-node behaviour, exactly as an unanswered key does
when the build deadline fires.

Concurrent resolution SHALL NOT change the resolved index's contents: each key's answer is
independent of every other key's. Any per-build memoisation shared across concurrent
resolutions SHALL be safe for concurrent use.

#### Scenario: Independent keys resolve concurrently

- **GIVEN** several distinct route keys collected from one service-graph vector
- **WHEN** the reader resolves them
- **THEN** the resulting index is identical to the index a strictly serial resolution produces

#### Scenario: Truncation is reported

- **GIVEN** a build whose collected route keys exceed the cap
- **WHEN** the reader resolves them
- **THEN** the number of dropped keys is logged and the corresponding endpoints fall back to external nodes

### Requirement: Resolution errors carry no routing outcome

When route resolution fails with an error — a store read failure, a translation failure,
or a matcher failure — the engine SHALL NOT also report a routing outcome that is
meaningful to the caller. In particular it SHALL NOT report the outcome that gates the
ingress LB Service fallback, so that an infrastructure failure can never be mistaken for
"no Istio Gateway serves this host".

The matcher's per-query results SHALL be recovered for every query posed or reported as an
error; a partially recovered result set SHALL NOT be returned.

#### Scenario: Store failure is not reported as a missing gateway

- **GIVEN** the route store fails a read
- **WHEN** the engine resolves an endpoint
- **THEN** the failure is reported as an error with no routing outcome, and the endpoint falls back to an external node

#### Scenario: Incomplete matcher output errors

- **GIVEN** the matcher's output yields fewer results than the number of queries posed
- **WHEN** the engine parses that output
- **THEN** the parse reports an error rather than returning a result for any query
