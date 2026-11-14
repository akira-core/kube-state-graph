# Spec Delta

## ADDED Requirements

### Requirement: Connection-string userinfo never leaves the reader

No response body and no per-series Debug log record derived from a service-graph `client` / `server` label containing `"://"` SHALL carry the URL userinfo of that label. Removing it SHALL leave every other byte of the label unchanged, and SHALL NOT change which node an endpoint resolves to.

#### Scenario: What counts as userinfo

- **WHEN** a label contains `"://"`
- **THEN** each `"://"`-delimited authority runs to the first `/`, `?` or `#` (or the end of the label), its userinfo is everything up to and including the LAST `@` inside it, and a label with no `@` in any authority is unchanged byte for byte

#### Scenario: Credentials are removed from an external node

- **WHEN** a series' `server` label is `mongodb://app:pw@mongo.example:27017/db` with an empty server UID, and the host is not a Kubernetes Service name
- **THEN** the endpoint becomes `external/mongodb://mongo.example:27017/db` with `name` `mongodb://mongo.example:27017/db`, and neither `app` nor `pw` appears in the response body

#### Scenario: A bare username is removed too

- **WHEN** an unresolvable `"://"` label is `mongodb+srv://admin@cluster0.example/db`
- **THEN** the external node is `external/mongodb+srv://cluster0.example/db`

#### Scenario: Opaque and malformed labels are covered

- **WHEN** an unresolvable `"://"` label is `jdbc:postgresql://app:pw@db.example:5432/orders`, or carries a password that is not a valid URL escape such as `postgres://app:%zz@db.example/x`
- **THEN** the external node is `external/jdbc:postgresql://db.example:5432/orders`, respectively `external/postgres://db.example/x`

#### Scenario: An @ outside the authority is kept

- **WHEN** an unresolvable `"://"` label is `https://api.example/users/@me` or `https://api.example/x?email=a@b.example`
- **THEN** the external node carries the label unchanged

#### Scenario: Labels that differ only in credentials are one endpoint

- **WHEN** two series from the same client carry `server` labels `amqp://app:old@queue.example:5672` and `amqp://app:new@queue.example:5672`, both unresolvable
- **THEN** the build emits one external node `external/amqp://queue.example:5672` and one edge to it

#### Scenario: A credentialled label still resolves to its Service

- **WHEN** a `"://"` label is `nats://app:pw@nats.messaging.svc:4222` and the anchor cluster holds `messaging/nats`
- **THEN** the endpoint resolves to that Service node exactly as `nats://nats.messaging.svc:4222` does

#### Scenario: Debug records carry no credentials

- **WHEN** Debug logging is enabled and a series whose `client` or `server` label carries userinfo falls back to an external node, resolves to a Service, is shadowed by a populated pod UID, or is dropped for a wholly empty side
- **THEN** every Debug record emitted for that series names the label with its userinfo removed

## MODIFIED Requirements

### Requirement: Unresolvable connection string falls back to an external node

When a `"://"` label is unresolvable, because its host is not a parseable Kubernetes `.svc` name or because the anchor cluster does not itself hold the addressed Service in its own family, the reader SHALL fall back to an external node named by the label value with any URL userinfo removed, as "Connection-string userinfo never leaves the reader" defines.

#### Scenario: Shape of the external node

- **WHEN** a `"://"` label is unresolvable
- **THEN** the endpoint becomes a node with `id` = `external/<label_value>`, `name` = `<label_value>` (verbatim apart from removed URL userinfo: no other normalisation, no trimming), `type` = `"external"`, and `labels` = `{}` (empty map, no `cluster` key)

#### Scenario: External endpoints stay visible

- **WHEN** a connection string is a truly-external URL (e.g. `https://payments.partner.example/api`), an unknown in-cluster name, or names a Service the caller's own cluster does not deploy
- **THEN** it stays visible as an `external` node
- **AND** every non-pod, non-service endpoint uses the `external` node type, whether it arises from an unresolvable `"://"` connection string or from the missing pod-UID human-label fallback

### Requirement: Filtered-build admission and out-of-scope endpoints

When any request-scoped selector dimension (`az`, `env`, `cluster`, `namespace`) is active, the reader SHALL admit a service-graph series only if **at least one** of its endpoints resolves to loaded topology, SHALL resolve every other endpoint of an admitted series to an external node, and SHALL NOT synthesise a pod node for any endpoint. None of these rules SHALL apply to an unfiltered build.

#### Scenario: Which series the rules apply to

- **WHEN** a filtered build runs, so the topology is a subset of the estate while the service-graph series are complete
- **THEN** the rules apply to every series that survives the sentinel exclusion and the wholly-empty-side drop, and the result is a pure function of the series set and the loaded topology (order-free, deterministic)

#### Scenario: Meaning of loaded topology and of a dropped series

- **WHEN** admission is decided
- **THEN** an endpoint reaches loaded topology if it is a pod present in the topology, or a Service present in the loaded service index (via connection-string resolution, the unknown-server peer ladder, or route resolution)
- **AND** a series whose endpoints both fail to reach loaded topology is dropped before any node or edge is materialised, whatever the unresolved sides would otherwise have become, which keeps the out-of-scope estate from rendering as a web of `external` nodes and bounds the output to the in-scope workload's direct neighbourhood

#### Scenario: Forms of an out-of-scope endpoint

- **WHEN** an endpoint of an admitted series does not reach loaded topology
- **THEN** it resolves to `external/<raw label>` with `labels={}`: a non-empty pod UID that is not loaded falls to that side's `client` / `server` human label exactly as the missing-UID human-label fallback does (`external/cart`), a `"://"` connection string whose Service is not loaded keeps its label with any URL userinfo removed (as "Unresolvable connection string falls back to an external node" defines), and a peer address that matches no loaded Service `ClusterIP` or Pod IP keeps the raw peer value
- **AND** a side whose UID is not loaded AND whose human label is empty is dropped together with its series, as a wholly empty side is

#### Scenario: Two-phase materialisation

- **WHEN** a series is resolved and later dropped
- **THEN** it leaves no node or edge behind, because service nodes, `service-selects-pod` fan-out edges, route-chain edges, and external nodes are materialised only for admitted series

#### Scenario: Existing conventions are unchanged

- **WHEN** a filtered build emits external endpoints
- **THEN** an edge whose client side is `external` carries no `labels.cluster`, and an edge with an `external` endpoint never carries `metrics`
- **AND** the unknown-server peer-label enrichment still requires the client to be a loaded topology pod (an out-of-scope client with `server="unknown"` is dropped), same-named out-of-scope peers collapse into one external node, and an out-of-scope caller of a loaded pod or Service appears as an inbound external

#### Scenario: Cross-namespace call renders the peer as external

- **WHEN** a build with `namespace={shop}` sees a series from loaded pod `shop/checkout` (`client_k8s_pod_uid="c1"`) to `server="cart"`, `server_k8s_pod_uid="s1"`, where `s1` lives in namespace `payments` and is not loaded
- **THEN** the reader emits one `pod-calls-pod` edge from `<cluster>/c1` to `external/cart`; `external/cart` has `type: "external"`, `name: "cart"`, `labels: {}`; no pod node exists for `s1`

#### Scenario: Series between two out-of-scope pods is dropped

- **WHEN** a build with `namespace={shop}` sees a series whose client UID and server UID both belong to `payments` pods (neither loaded)
- **THEN** no edge and no node is produced for the series — in particular no `external/<client>` → `external/<server>` edge

#### Scenario: Inbound call from an out-of-scope caller

- **WHEN** a build with `namespace={shop}` sees a series from `client="frontend"`, `client_k8s_pod_uid="f1"` (a `web` namespace pod, not loaded) to loaded pod `shop/checkout`
- **THEN** the reader emits one `pod-calls-pod` edge from `external/frontend` to `<cluster>/<checkout-uid>` with no `labels.cluster` and no `metrics`

#### Scenario: Out-of-scope Service via connection string

- **WHEN** a build with `namespace={shop}` sees a series from a loaded `shop` pod whose `server` label is `http://cart.payments.svc.cluster.local:8080` with an empty server UID, and `payments/cart` is not in the loaded service index
- **THEN** the reader emits a `pod-calls-pod` edge to `external/http://cart.payments.svc.cluster.local:8080` with `labels={}`; no service node is materialised

#### Scenario: In-scope Service called by an out-of-scope pod is materialised with its fan-out

- **WHEN** a build with `namespace={shop}` sees a series from an unloaded `web` pod (`client="frontend"`) whose `server` label is `http://api.shop.svc.cluster.local` and `shop/api` is loaded with backing pods in `shop`
- **THEN** the reader admits the series (the server reaches loaded topology), materialises `<cluster>/shop/api` with its `service-selects-pod` edges to the loaded backing pods, and emits a `pod-calls-service` edge from `external/frontend` to the service node

#### Scenario: Peer IP of an out-of-scope pod becomes an external IP node

- **WHEN** a build with `namespace={shop}` sees a series from a loaded `shop` pod with `server="unknown"`, empty server UID, and `client_network_peer_address="10.1.2.3"`, where `10.1.2.3` is the Pod IP of an unloaded `payments` pod
- **THEN** the endpoint resolves to `external/10.1.2.3` (the ClusterIP and Pod-IP lookups see only loaded topology) and the edge is emitted to it

#### Scenario: Dropped series leaves no trace

- **WHEN** a build with `cluster={cluster-alpha}` sees a series from an unloaded `cluster-beta` pod to `http://cart.payments.svc.cluster.local` where `payments/cart` is also not loaded
- **THEN** the series is dropped and the built graph contains neither an `external/cart…` node nor any edge for it

#### Scenario: Cross-cluster partner under a cluster filter

- **WHEN** a build with `cluster={cluster-alpha}` sees a series from a loaded `cluster-alpha` pod to `server="cart"`, `server_k8s_pod_uid="s9"` where `s9` is a `cluster-beta` pod
- **THEN** the reader emits a `pod-calls-pod` edge to `external/cart` with `labels.cluster: "cluster-alpha"`; no `cluster-beta` pod node is synthesised or loaded

#### Scenario: Unfiltered build keeps the synthesised-pod behaviour

- **WHEN** a build with no request-scoped dimension sees a series whose server UID is absent from topology
- **THEN** the existing "Synthesised pod node fallback" applies unchanged and a synthesised pod node is produced
