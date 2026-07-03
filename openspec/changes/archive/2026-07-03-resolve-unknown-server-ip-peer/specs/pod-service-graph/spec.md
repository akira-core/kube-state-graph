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

1. Strip an optional trailing `:<port>` suffix (best-effort host/port split; a value with
   no splittable port is used unchanged).
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
addressed Service, the reader SHALL fall back to an **external** node from the RAW,
unstripped peer-address value (not the port-stripped host):

- `id`     = `external/<raw_peer_address_value>`
- `name`   = `<raw_peer_address_value>` (verbatim — no normalisation, no trimming)
- `type`   = `"external"`
- `labels` = `{}` (empty map — no `cluster` key)

The resulting edge follows the existing generic rules unchanged: `type` is
`pod-calls-service` when the resolved target is a service node, otherwise `pod-calls-pod`;
`labels.cluster` is present (the client pod's cluster) because the client side is a
resolved pod. No new node type and no new edge type are introduced by this requirement.

#### Scenario: `client_net_peer_name` resolves to an in-cluster Service

- **WHEN** a series has `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`, namespace `shop`), `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="payments.payments-ns.svc.cluster.local"`, and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with backing pods
- **THEN** the resulting edge has `type: "pod-calls-service"`, `target: "cluster-alpha/payments-ns/payments"`, `labels.cluster: "cluster-alpha"`; the target service node materialises with its usual `service-selects-pod` fan-out to its backing pods

#### Scenario: `client_server_address` used only when `client_net_peer_name` is absent

- **WHEN** a series has `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`), `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name=""` (absent), and `client_server_address="payments.payments-ns.svc.cluster.local:8080"`
- **THEN** the port suffix `:8080` is stripped before classification, and the resulting edge targets `cluster-alpha/payments-ns/payments` exactly as if `client_net_peer_name` had carried the same host

#### Scenario: Bare short Service name resolves in the client pod's own namespace

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, namespace `shop`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="payments"` (a bare single-label name, no `.svc` suffix), and topology has a `payments` service in namespace `shop` (the client's own namespace) in `cluster-alpha`
- **THEN** the reader treats `payments` as addressing `(namespace="shop", service="payments")` and resolves it to `cluster-alpha/shop/payments`, exactly as the 2-label `.svc` form would

#### Scenario: Bare IP-literal peer address resolves to its ClusterIP-matching Service

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="172.20.10.5"` (no `:port`, no DNS name), and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with `cluster_ip="172.20.10.5"`
- **THEN** the resulting edge has `type: "pod-calls-service"`, `target: "cluster-alpha/payments-ns/payments"`, exactly as if the peer address had been the Service's `.svc` DNS name

#### Scenario: IP-literal peer address with a port suffix is stripped before matching

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="172.20.10.5:8080"`, and topology has a service in `cluster-alpha` with `cluster_ip="172.20.10.5"`
- **THEN** the `:8080` suffix is stripped before IP-literal classification, and the endpoint resolves to that service node

#### Scenario: IP-literal peer address present only in a family-sibling cluster — external, not cross-resolved

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="172.20.10.5"`, `cluster-alpha` holds NO service with `cluster_ip="172.20.10.5"`, but a same-family sibling cluster `cluster-alpha-2` DOES hold a service with that `ClusterIP`
- **THEN** the endpoint falls back to `external/172.20.10.5` — the IP-literal lookup (step 4) is scoped to the anchor cluster only and does NOT consult family siblings, unlike the subsequent `service-selects-pod` fan-out that would apply once a Service is identified

#### Scenario: IP-literal peer address absent from the anchor cluster's Service set — external

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="192.0.2.55"`, and no Service in `cluster-alpha` has that `ClusterIP`
- **THEN** the endpoint falls back to `external/192.0.2.55`

#### Scenario: External peer address becomes an external node

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, and `client_net_peer_name="payments.partner.example"` (a multi-label host that is not a `.svc` name and not a bare short name)
- **THEN** the resulting edge has `type: "pod-calls-pod"`, `target: "external/payments.partner.example"`; the target node has `type: "external"`, `name: "payments.partner.example"`, `labels={}`

#### Scenario: Anchor cluster lacks the addressed Service — external, not dropped

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="web.shop.svc.cluster.local"`, and `cluster-alpha` does NOT hold a `web` service in namespace `shop` (a family sibling holding it does not count, per the existing same-cluster rule)
- **THEN** the endpoint falls back to `external/web.shop.svc.cluster.local` rather than resolving to a service node in a different cluster, and rather than being dropped

#### Scenario: Neither peer label present — dropped

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a real topology pod, `server="unknown"`, `server_k8s_pod_uid=""`, and both `client_net_peer_name` and `client_server_address` are empty or absent
- **THEN** the endpoint is dropped: no node and no edge are produced for it

#### Scenario: Client side not a resolved real pod — enrichment does not apply

- **WHEN** a series has `client="admin"`, `client_k8s_pod_uid=""` (client does not resolve to a topology pod), `server="unknown"`, `server_k8s_pod_uid=""`, and `client_net_peer_name="payments.payments-ns.svc.cluster.local"` is present
- **THEN** the trigger condition (client resolved to a real pod) is not met, so this requirement does not apply and the endpoint is dropped per the sentinel-exclusion requirement, even though a peer label is present

#### Scenario: Server UID present but unresolved — enrichment still applies

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a real topology pod, `server="unknown"`, `server_k8s_pod_uid="stale-uid"` (non-empty but absent from the global pod-UID index), and `client_net_peer_name="payments.payments-ns.svc.cluster.local"` resolves to an in-cluster Service
- **THEN** the reader resolves via peer-label enrichment (target: the service node) rather than synthesising a pod node for `stale-uid` — a non-empty but unresolvable server UID does not take priority over this requirement when `server` is literally `"unknown"`

#### Scenario: Duplicate ClusterIP within the anchor cluster resolves deterministically

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="172.20.10.5"`, and (as a data anomaly) `cluster-alpha` holds two Services with `cluster_ip="172.20.10.5"` — `(namespace="ops", service="zeta")` and `(namespace="ops", service="alpha")`
- **THEN** the endpoint resolves to `(namespace="ops", service="alpha")` — the lexically-smaller `(namespace, service)` pair — deterministically and identically across rebuilds of the same upstream data
