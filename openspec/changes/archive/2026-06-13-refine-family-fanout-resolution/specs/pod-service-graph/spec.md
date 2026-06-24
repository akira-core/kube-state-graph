## MODIFIED Requirements

### Requirement: Connection-string endpoint resolution

When a service-graph series carries a connection string for an endpoint (an external dependency addressed by URL), that endpoint's pod UID is empty and the `client` / `server` label holds the connection string verbatim (e.g. `"mongodb://mongo-0.mongo.db.svc.cluster.local:27017"` or `"https://payments.partner.example/api"`). The reader SHALL detect connection strings by a hardcoded `"://"` substring check evaluated independently against the `client` and `server` label values. There is NO configurable knob for this detection: the reader SHALL NOT read any substring or pattern from configuration.

For each endpoint, the reader SHALL run **connection-string resolution** (Stage 0) when BOTH of the following hold:

1. the endpoint's pod UID (`client_k8s_pod_uid` or `server_k8s_pod_uid`) is empty or absent, AND
2. the corresponding label (`client` or `server`) contains the substring `"://"`.

When the pod UID is non-empty, normal pod-UID resolution applies unchanged and connection-string resolution is NOT run (connection strings only appear when the UID is empty).

Connection-string resolution proceeds as follows:

1. Parse the label as a URL and take the host (strip scheme, userinfo, port, and any path/query). If there is no host, the label is **unresolvable**.
2. Match the host against the Kubernetes DNS grammar. Strip an optional trailing `.svc.<cluster-domain>` suffix (e.g. `.svc.cluster.local`); also accept the shorter `<...>.svc` and the bare `<a>.<b>` forms. Count the dotted labels of the service-relative part and reduce BOTH forms to the addressed `(service, namespace)`:
   - 2 labels — `<service>.<namespace>` — the addressed service (regular ClusterIP service, or a headless service's service-level name).
   - 3 labels — `<pod-hostname>.<service>.<namespace>` — a headless per-pod DNS name; the reader SHALL DROP the leading `<pod-hostname>` and resolve the remaining `<service>.<namespace>`. A headless per-pod address and the bare service address resolve identically — there is NO per-pod resolution.
   - any other label count — **unresolvable**.
3. Resolve the addressed `(namespace, service)` against the topology index `ServicesByNameNS` (built from `kube_service_info`) **once per candidate cluster** in the caller's family (step 4), iterating candidates in lexicographically sorted order. **Endpoint-backed pruning** SHALL then be applied to the matched candidates: when AT LEAST ONE matched candidate cluster has one or more backing pods in its OWN cluster's `EndpointsByService` entry for the addressed `(namespace, service)`, the reader SHALL skip every matched candidate that PROVABLY has zero backing pods — no service node is materialised for it, no `pod-calls-service` edge targets it, and no fan-out is emitted from it. "Provably" requires **endpoint visibility**: a candidate counts as provably unbacked ONLY when its own cluster has at least one `EndpointsByService` entry for SOME service (the cluster demonstrably exports joinable endpoint data) yet none for the addressed service; a candidate whose WHOLE cluster has zero endpoint data (its kube-state-metrics lacks the `kubernetes.io/service-name` endpointslice allowlist — e.g. a staged rollout or config drift — or the slice→pod join produced nothing) SHALL be KEPT, because its zero is absence of evidence, not evidence of absence. When NO matched candidate has any backing pod (including deployments where the join is unavailable fleet-wide), ALL matched candidates resolve exactly as before — a known service with zero backing endpoints still materialises the service node, with no fan-out edges. For EACH surviving candidate cluster `c`, the reader SHALL materialise that cluster's **service** node: `id="<c>/<namespace>/<service>"`, `type="service"`, `labels={ cluster, namespace }`, `ipaddress=[cluster_ip]` when `cluster_ip != "None"` (omitted for headless services where `cluster_ip="None"`). For each materialised service node the reader SHALL ALSO materialise, on demand and deduplicated, one `service-selects-pod` edge from that service node to EACH backing pod found in that SAME cluster's `EndpointsByService` entry (built from `kube_endpointslice_endpoints` joined to topology pods by `(namespace, targetref_name) → pod UID`) — the fan-out is always **intra-cluster** within the resolved service's own cluster `c` (a service and its backing pods share a cluster). The endpoint resolves to the **SET** of all surviving service-node IDs; when NO candidate survives (zero matches from step 4 after the fallback, or — impossible by construction — pruning of a non-empty match set to empty), the label is **unresolvable**.
4. **Cluster-family fan-out with unknown-family fallback**: the **cluster-family key** of a cluster name SHALL be computed by replacing every maximal run of ASCII digits (`[0-9]+`) in the name with a single `0` sentinel character. Two clusters are in the same **family** if and only if their family keys are byte-equal. Examples: `prod-03` and `prod-12` both normalise to `prod-0` and are in the same family; `staging-1` (key `staging-0`) is NOT in `prod-1`'s family (key `prod-0`); clusters named bare digit runs such as `1` and `2` all normalise to `0` and form one family; a digit-free name normalises to itself, so its family contains only identically-named clusters. The sentinel SHALL be a digit so the mapping is collision-free without escaping: every `0` in a key originates from a digit run, and a non-digit byte can never equal the sentinel (a non-digit sentinel would collide with cluster names literally containing it). The key function SHALL be a hardcoded pure string function: there is NO configuration surface (flag / env var / config field) to alter it. The **family anchor** for a lookup SHALL be the client side's authoritative cluster: the UID-recovered client-pod cluster when the series' client side resolved to a topology pod (the trace `cluster` label is frequently missing or disagrees with topology), and otherwise the series' trace-source `cluster` label (bucketed to `"unknown"` when missing). The edge `labels.cluster` value is NOT affected by anchor recovery — it stays the raw trace label per the edge-cluster-label requirement. The **candidate clusters** SHALL be all clusters loaded in the build's topology whose family key equals the anchor's family key, iterated in sorted order. The anchor cluster is NOT special — it participates only as an ordinary family member. **Unknown-family fallback**: when the anchor's family key is not the family key of ANY loaded cluster — where a **loaded cluster** is any cluster evidenced by the build's topology (`ClustersObserved`, clusters holding services, clusters of topology pods), with the `"unknown"` missing-label bucket explicitly EXCLUDED from this evidence (the bucket is an absence of identity, not a cluster, so an `"unknown"` anchor always counts as unanchorable) — the reader SHALL look up the addressed `(namespace, service)` across the LOADED clusters; `"unknown"`-bucketed service entries (from `kube_service_info` samples missing their `cluster` label) are NOT holders in this lookup, so a single unlabelled duplicate can neither make a uniquely-held name read as ambiguous nor let a bogus-label anchor resolve into the `unknown` pseudo-cluster. When at least one loaded cluster holds the name AND every holding cluster belongs to exactly ONE family, those holding clusters REPLACE any candidates from the primary family lookup (endpoint-backed pruning from step 3 then applies to them) — identified holders always shadow a primary-lookup hit on `"unknown"`-bucketed entries. When the loaded holding clusters span TWO OR MORE distinct families (an ambiguous name), the candidate set is empty and the endpoint falls back to `external/<label>` — also overriding any `"unknown"`-bucketed primary hit. When NO loaded cluster holds the name, an `"unknown"`-anchored endpoint KEEPS its primary family-lookup hit on `"unknown"`-bucketed service entries, if any — a fully-unlabelled deployment (every topology series bucketed to `"unknown"`) retains connection-string resolution within its own pseudo-cluster, the only identity it has; any other unanchorable anchor (a bogus label, whose family key cannot equal `"unknown"`) is left with zero candidates and falls back to `external/<label>`. When the anchor's family key IS the family key of at least one loaded cluster but that family holds no matching service, the reader SHALL NOT fall back across families — the endpoint falls back to `external/<label>` (resolving a known-cluster caller against a foreign family would fabricate cross-family attribution). Family filtering and the fallback SHALL happen in-memory at the resolution layer: there is NO PromQL query change and NO new flag or environment knob (the "no filters pushed to PromQL" contract is preserved).

The reader SHALL emit one `pod-calls-service` edge per `(resolved source node, matched service-node ID)` pair — a single upstream series MAY therefore yield multiple edges. When BOTH sides of a series are `"://"` labels resolving to sets of service nodes, the reader SHALL emit the **cross product** of edges (each resolved source × each resolved target). A non-`"://"` side resolves to a single node ID exactly as before. Because a matched service node MAY live in a different (family) cluster than the caller — or, via the unknown-family fallback, in a family the anchor could not name — `pod-calls-service` edges MAY be cross-cluster; `service-selects-pod` fan-out edges remain strictly intra-cluster. Determinism SHALL be preserved: candidate clusters are iterated in sorted order, endpoint-backed pruning is a deterministic filter over the sorted candidate set, the family-uniqueness check of the fallback is an order-free set property, the existing `(source, target)` edge dedupe applies to each emitted edge, edge IDs remain deterministic UUIDv5 values over `<type>|<source>|<target>`, and the response body stays byte-identical for identical upstream data.

When the `"://"` label is **unresolvable** — the host is not a parseable Kubernetes `.svc` name, OR resolution yields zero candidates after the family lookup and (when engaged) the unknown-family fallback — the reader SHALL fall back to an **external** node:

- `id`     = `external/<label_value>`
- `name`   = `<label_value>` (verbatim — no normalisation, no trimming)
- `type`   = `"external"`
- `labels` = `{}` (empty map — no `cluster` key)

This keeps truly-external URLs (e.g. `https://payments.partner.example/api`), unknown in-cluster names, and ambiguous cross-family names visible. All non-pod, non-service endpoints use the `external` node type — whether they arise from an unresolvable `"://"` connection string or from the missing pod-UID human-label fallback.

The decision is per endpoint: a single series MAY produce edges with a pod source and service or external targets, an external source and a pod target, two pods, or any mix. The edge `type` is `pod-calls-service` when the target resolves to a service node, otherwise `pod-calls-pod`; `pod-calls-service` edges MAY be cross-cluster (cross-cluster status is derived by comparing the resolved source and target node `labels.cluster` values). The edge `labels.cluster` rule for the client side applies: present when the client side resolves to a pod (from a non-empty pod UID), omitted when the client side resolves to a service or external node — including ANY `"://"` connection string, which never resolves to a pod.

#### Scenario: Headless connection string resolves to its service node and fans out to backing pods

- **WHEN** the upstream contains a series with `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`), `server="mongodb://mongo-0.mongo.db.svc.cluster.local:27017"`, `server_k8s_pod_uid=""`, `cluster="cluster-alpha"`, and topology has a headless `mongo` service in namespace `db` whose `EndpointsByService` entry maps to a backing pod `cluster-alpha/pod-mongo-0-uid`
- **THEN** the leading pod-hostname `mongo-0` is dropped; the resulting `pod-calls-service` edge has `source: "cluster-alpha/abc"`, `target: "cluster-alpha/db/mongo"` (a `type="service"` node, NOT a specific pod), and `labels.cluster: "cluster-alpha"` (the client side is a pod); and the graph ALSO contains a `service-selects-pod` edge from `cluster-alpha/db/mongo` to `cluster-alpha/pod-mongo-0-uid`

#### Scenario: ClusterIP service connection string resolves to a service node with backing-pod edges

- **WHEN** the upstream contains a series with `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`), `server="https://payments.payments-ns.svc.cluster.local/api"`, `server_k8s_pod_uid=""`, `cluster="cluster-alpha"`, and topology has a ClusterIP `payments` service in namespace `payments-ns` with `cluster_ip="10.0.0.5"` whose `EndpointsByService` entry maps to two backing pods `cluster-alpha/p1` and `cluster-alpha/p2`
- **THEN** the resulting `pod-calls-service` edge has `target: "cluster-alpha/payments-ns/payments"`; the target node has `type: "service"`, `name="payments"` (or service identity per the graph-api capability), `labels={ cluster: "cluster-alpha", namespace: "payments-ns" }`, and `ipaddress: ["10.0.0.5"]`; and the graph ALSO contains two `service-selects-pod` edges from `cluster-alpha/payments-ns/payments` to `cluster-alpha/p1` and `cluster-alpha/p2` respectively; the original edge has `labels.cluster: "cluster-alpha"` (the client side is a pod)

#### Scenario: Unresolvable external URL becomes an external node

- **WHEN** the upstream contains a series with `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`), `server="https://payments.partner.example/api"`, `server_k8s_pod_uid=""`, `cluster="cluster-alpha"`, and the host `payments.partner.example` is not a parseable Kubernetes `.svc` name (no service or pod in topology)
- **THEN** the resulting `pod-calls-pod` edge has `target: "external/https://payments.partner.example/api"`; the target node has `type: "external"`, `name: "https://payments.partner.example/api"`, `labels={}` (empty — no `cluster` key); and the edge has `labels.cluster: "cluster-alpha"` (the client side is a pod)

#### Scenario: "://" label with empty UID is always handled by connection-string resolution

- **WHEN** a series has an endpoint whose pod UID is empty and whose `client` / `server` label contains `"://"` (whether or not it resolves)
- **THEN** that endpoint is resolved by connection-string resolution (one service node per surviving candidate cluster, or — when zero candidates survive — an `external/<label>` node) and the missing pod-UID human-label fallback is NEVER consulted for it

#### Scenario: Service deployed in two family clusters resolves to both service nodes

- **WHEN** clusters `prod-1` and `prod-2` are loaded (both family key `prod-0`), EACH holds a `payments` service in namespace `payments-ns` with its own backing pods, and the upstream contains a series with `cluster="prod-1"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `prod-1`), `server="http://payments.payments-ns.svc.cluster.local"`, `server_k8s_pod_uid=""`
- **THEN** the reader emits TWO `pod-calls-service` edges — `prod-1/abc → prod-1/payments-ns/payments` and `prod-1/abc → prod-2/payments-ns/payments` — and materialises BOTH service nodes (both candidates are endpoint-backed, so pruning removes neither); each service node carries its own intra-cluster `service-selects-pod` fan-out to its OWN cluster's backing pods only (the `prod-2` fan-out edges target only `prod-2` pods); both `pod-calls-service` edges carry `labels.cluster: "prod-1"` (the client side is a pod); the edge to `prod-2/payments-ns/payments` is cross-cluster, detectable by comparing source (`labels.cluster="prod-1"`) and target (`labels.cluster="prod-2"`) node labels

#### Scenario: Endpointless family sibling is pruned from the fan-out

- **WHEN** clusters `prod-1` and `prod-2` are loaded (family `prod-0`), BOTH hold a `nats` service in namespace `messaging` (the Service object is applied fleet-wide), but ONLY `prod-2`'s service has backing pods in its `EndpointsByService` entry (the mesh routes the DNS name to `prod-2`), and a series has `cluster="prod-1"`, `client_k8s_pod_uid="abc"` (a `prod-1` pod), `server="nats://nats.messaging.svc:4222"`, `server_k8s_pod_uid=""`
- **THEN** the `prod-1` candidate is pruned: exactly ONE `pod-calls-service` edge is emitted, targeting `prod-2/messaging/nats`; NO `prod-1/messaging/nats` service node is materialised by this resolution and NO edge targets it; the surviving service node fans out `service-selects-pod` edges to `prod-2`'s backing pods only

#### Scenario: All family candidates endpointless — every service node still materialises

- **WHEN** clusters `prod-1` and `prod-2` are loaded (family `prod-0`), BOTH hold a `nats` service in namespace `messaging`, NEITHER has any backing pod in `EndpointsByService` (e.g. the `kubernetes.io/service-name` endpointslice label is not allowlisted, or no backends are ready anywhere), and a series has `cluster="prod-1"`, `client_k8s_pod_uid="abc"` (a `prod-1` pod), `server="nats://nats.messaging.svc:4222"`, `server_k8s_pod_uid=""`
- **THEN** pruning does not engage (no candidate is endpoint-backed); BOTH service nodes materialise with zero `service-selects-pod` edges, and TWO `pod-calls-service` edges are emitted — exactly the pre-pruning behaviour

#### Scenario: Same service in an out-of-family cluster is not resolved

- **WHEN** clusters `prod-1` (family key `prod-0`) and `staging-1` (family key `staging-0`) are loaded, BOTH hold a `payments` service in namespace `payments-ns`, and a series has `cluster="prod-1"`, `client_k8s_pod_uid="abc"` (a `prod-1` pod), `server="http://payments.payments-ns.svc"`, `server_k8s_pod_uid=""`
- **THEN** only `prod-1` is a candidate cluster (`staging-0` ≠ `prod-0`); exactly ONE `pod-calls-service` edge is emitted, targeting `prod-1/payments-ns/payments`; no edge targets and no on-demand service node is materialised for `staging-1/payments-ns/payments` by this resolution

#### Scenario: No family cluster holds the service — external fallback, no cross-family escape

- **WHEN** clusters `prod-1` and `prod-2` are loaded (family `prod-0`), NEITHER holds a `my-nats` service in namespace `messaging`, an out-of-family cluster (e.g. `staging-1`) DOES hold it, and a series has `cluster="prod-1"`, `client_k8s_pod_uid="abc"` (a `prod-1` pod), `server="nats://my-nats.messaging.svc:4222"`, `server_k8s_pod_uid=""`
- **THEN** zero family candidates match; the unknown-family fallback does NOT engage (the anchor family `prod-0` IS a loaded family — it merely lacks the service), so the endpoint falls back to an external node exactly as today: the edge has `type: "pod-calls-pod"`, `target: "external/nats://my-nats.messaging.svc:4222"`; the target node has `type: "external"`, `labels={}`; and the edge has `labels.cluster: "prod-1"` (the client side is a pod)

#### Scenario: Both sides are "://" labels — cross product of edges

- **WHEN** a series has `client="http://frontend.web.svc"` and `server="http://payments.payments-ns.svc"`, BOTH pod UIDs empty, `cluster="prod-1"`, and both `(web, frontend)` and `(payments-ns, payments)` exist in BOTH family clusters `prod-1` and `prod-2`
- **THEN** the client side resolves to the set `{prod-1/web/frontend, prod-2/web/frontend}` and the server side to `{prod-1/payments-ns/payments, prod-2/payments-ns/payments}`; the reader emits the cross product of FOUR `pod-calls-service` edges (each resolved source × each resolved target); every edge `labels` map contains no `cluster` key (the client side resolved to a non-pod node)

#### Scenario: Missing cluster label recovers the family from the UID-resolved client pod

- **WHEN** a series missing its `cluster` external label (bucketed to `cluster="unknown"`) has `client_k8s_pod_uid="abc"` resolving via the global UID index to a topology pod in `prod-1`, `server="http://payments.payments-ns.svc.cluster.local"`, `server_k8s_pod_uid=""`, and the `payments` service exists in family clusters `prod-1` and `prod-2`
- **THEN** the family anchor is the recovered client-pod cluster `prod-1` (NOT the `"unknown"` bucket); the fan-out emits `pod-calls-service` edges to BOTH `prod-1/payments-ns/payments` and `prod-2/payments-ns/payments`; each emitted edge's `labels.cluster` is `"unknown"` (the raw trace label, unaffected by anchor recovery)

#### Scenario: Unanchorable series resolves via the unknown-family fallback when one family holds the service

- **WHEN** a series missing its `cluster` external label is bucketed to `cluster="unknown"`, its client side does NOT resolve to a topology pod (e.g. `client="admin"` with an empty client UID), it has `server="http://payments.payments-ns.svc.cluster.local"`, `server_k8s_pod_uid=""`, no loaded cluster's family key equals the family key of `unknown`, and the `(payments-ns, payments)` service is held ONLY by clusters `prod-1` and `prod-2` — both family `prod-0`, a single family
- **THEN** the unknown-family fallback engages: the holding clusters span exactly one family, so they become the candidate set; endpoint-backed pruning applies; `pod-calls-service` edges are emitted to the surviving `prod-*` service nodes; the edges carry NO `labels.cluster` key (the client side is non-pod) — the same connection string no longer degrades to `external/<label>` merely because this series lacked a usable cluster identity

#### Scenario: Unanchorable series with a cross-family-ambiguous name stays external

- **WHEN** a series is bucketed to `cluster="unknown"` with a non-pod client side, it has `server="nats://nats.messaging.svc:4222"`, `server_k8s_pod_uid=""`, and the `(messaging, nats)` service is held by clusters spanning TWO families (e.g. `prod-1` family `prod-0` AND `staging-1` family `staging-0`)
- **THEN** the unknown-family fallback finds holders in more than one family — the name is ambiguous, no candidate set is chosen, and the endpoint falls back to `external/nats://nats.messaging.svc:4222` (`type="external"`, `labels={}`)

#### Scenario: Endpointless sibling without endpoint visibility is spared from pruning

- **WHEN** clusters `prod-1` and `prod-2` (family `prod-0`) both hold a `nats` service in namespace `messaging`, `prod-2`'s deployment is endpoint-backed, and `prod-1` has ZERO `EndpointsByService` entries for ANY service (its kube-state-metrics lacks the `kubernetes.io/service-name` endpointslice allowlist — a staged rollout), and a `prod-1`-anchored series addresses `nats://nats.messaging.svc:4222`
- **THEN** the `prod-1` candidate is NOT pruned (its zero backing pods is absence of evidence, not evidence of absence): BOTH service nodes materialise and BOTH `pod-calls-service` edges are emitted — pruning never destroys resolution on observability asymmetry

#### Scenario: Unlabelled service duplicate neither poisons nor shadows the fallback

- **WHEN** the `(data, cache)` service is held by loaded clusters `prod-1` and `prod-2` (one family `prod-0`) AND additionally appears in one `kube_service_info` series missing its `cluster` label (bucketed to the `"unknown"` pseudo-cluster), and an unanchorable series (missing trace `cluster` label, non-pod client) addresses `redis://cache.data.svc:6379`
- **THEN** the unlabelled duplicate is not a holder: the loaded holder set `{prod-1, prod-2}` spans exactly one family and resolves — the fallback is NOT flipped to ambiguous by the duplicate, and NO `unknown/data/cache` pseudo-cluster service node materialises (identified holders shadow the `"unknown"`-bucketed primary hit)

#### Scenario: Fully-unlabelled deployment keeps its pseudo-cluster resolution

- **WHEN** EVERY topology series lacks the `cluster` label (single-cluster install without an external cluster label: pods, services, and endpointslices are all bucketed to `"unknown"`), and an equally unlabelled series with a non-pod client addresses a `"://"` name held by the `"unknown"`-bucketed topology
- **THEN** no loaded cluster holds the name, so the `"unknown"`-anchored endpoint keeps its primary family-lookup hit: the `unknown/<namespace>/<service>` service node materialises with its own `service-selects-pod` fan-out — connection-string resolution is not regressed for label-less deployments

#### Scenario: Bogus-label anchor with only an unlabelled holder stays external

- **WHEN** the addressed `(namespace, service)` is known ONLY from `"unknown"`-bucketed `kube_service_info` series, and a series carries a bogus trace label (`cluster="legacy-7"`, family `legacy-0` matching no loaded cluster) with a `"://"` endpoint addressing it
- **THEN** the fallback finds zero loaded holders and the anchor's own family key (`legacy-0`) cannot hit the `"unknown"`-keyed entries, so the endpoint falls back to `external/<label>` — real traffic is never attributed to a pseudo-cluster built from two pieces of non-identity

### Requirement: Missing pod-UID human-label fallback

When a service-graph series lacks a pod UID for an endpoint (`client_k8s_pod_uid` or `server_k8s_pod_uid` is empty or absent) AND the corresponding human-readable label (`client` or `server`) is non-empty AND that label does NOT contain the substring `"://"`, the reader SHALL promote that endpoint to an **external** node derived from the human label, instead of dropping the edge. (A label containing `"://"` with an empty UID is handled by connection-string resolution, not this fallback.)

This fallback fires AFTER connection-string resolution (the hardcoded `"://"` check) and BEFORE the synthesised-pod fallback. It is unconditionally on (no knob) and SHALL apply symmetrically to client and server sides.

For the affected endpoint, the reader SHALL produce a node with:

- `id`     = `external/<label_value>`  (no cluster prefix — the endpoint is not a pod and has no cluster identity)
- `name`   = `<label_value>` (verbatim — no normalisation, no trimming)
- `type`   = `"external"`
- `labels` = `{}` (empty map — no `cluster` key)

Both unresolvable `"://"` connection strings (from connection-string resolution) and NON-URL missing-UID human labels (from this fallback) produce `external/<label>` nodes sharing the same dedupe map and `id` namespace.

The edge `labels.cluster` rule is unchanged: present (set to the metric's `cluster` label) when the **client** side resolves to a pod; omitted when the client side is non-pod — whether the client became `service` via connection-string resolution or `external` via this missing-UID fallback or the unresolvable connection-string path.

When BOTH the pod UID AND the human label are empty for an endpoint, the reader SHALL drop the edge (no identity remains to construct any node).

The per-endpoint resolution order is:

1. Connection-string resolution (hardcoded `"://"` check; only when UID is empty AND label contains `"://"`) → one service node PER surviving candidate cluster — the caller's family members holding the addressed `(namespace, service)` (anchored on the UID-recovered client-pod cluster when available, else the trace label), replaced by the unknown-family fallback's loaded holders when the anchor names no loaded family and exactly one family holds the service, then narrowed by endpoint-backed pruning (provably endpointless candidates — zero backing pods in an endpoint-visible cluster — are skipped when an endpoint-backed sibling exists) — each with on-demand `service-selects-pod` fan-out; one `pod-calls-service` edge per surviving service node, which MAY be cross-cluster. Zero surviving candidates → `external/<label>` node with `labels={}` (edge type `pod-calls-pod`). Never a pod.
2. Pod-UID resolution against topology / synth-pod fallback (only when UID is non-empty).
3. Missing-UID human-label fallback (this requirement; only when UID is empty AND label is non-empty AND label does NOT contain `"://"`).
4. Drop (both UID and label empty).

A series with a **wholly empty side** (its pod UID AND its human label both empty) SHALL be dropped BEFORE any resolution runs for EITHER side: no edge is emitted and no node (service, external, or synthesised pod) is materialised for either endpoint — the other side's `"://"` label must not leak an orphan service/external subgraph for an edge that cannot exist.

#### Scenario: Client UID missing, client label promoted to external

- **WHEN** a service-graph series has `client="admin"`, `cluster="cluster-alpha"`, `server="rest-api"`, `server_k8s_pod_uid="abc"` (resolving to a pod with `cluster="cluster-alpha"`), and `client_k8s_pod_uid` is absent (empty string)
- **THEN** the resulting edge has `type: "pod-calls-pod"`, `source: "external/admin"`, `target: "cluster-alpha/abc"`; the source node has `id: "external/admin"`, `name: "admin"`, `type: "external"`, no `cluster` key under its `labels`; and the **edge** `labels` map contains no `cluster` key (client side is external)

#### Scenario: Server UID missing, server label promoted to external

- **WHEN** a service-graph series has `client="checkout"`, `cluster="cluster-alpha"`, `client_k8s_pod_uid="abc"` (resolving to a pod), `server="payments"`, and `server_k8s_pod_uid` is absent
- **THEN** the resulting edge has `target: "external/payments"`; the target node has `id: "external/payments"`, `name: "payments"`, `type: "external"`, no `cluster` key under its `labels`; and the edge has `labels.cluster: "cluster-alpha"` (the client side is still a pod)

#### Scenario: Both UIDs missing, both human labels present

- **WHEN** a series has `client="admin"`, `server="payments"`, `cluster="cluster-alpha"`, and both `client_k8s_pod_uid` and `server_k8s_pod_uid` are absent
- **THEN** the resulting edge has `source: "external/admin"`, `target: "external/payments"`, edge `type: "pod-calls-pod"`, and the edge `labels` map contains no `cluster` key (client side is external)

#### Scenario: Both UID and human label empty — edge dropped

- **WHEN** a series has `client_k8s_pod_uid=""` AND `client=""` (or symmetrically empty server pair)
- **THEN** no edge is emitted for that series and no node is synthesised for that endpoint

#### Scenario: Wholly empty side drops the series before the other side materialises

- **WHEN** a series has `client=""` AND `client_k8s_pod_uid=""` while `server="nats://nats.messaging.svc:4222"` with `server_k8s_pod_uid=""` (or the symmetric server-empty case with a `"://"` client)
- **THEN** the series is dropped before resolution: no edge is emitted AND no service node, no `service-selects-pod` fan-out edge, and no external node is materialised from the non-empty side's label

#### Scenario: Connection-string resolution wins over missing-UID fallback

- **WHEN** a series has `client="https://api.example.com"` with `client_k8s_pod_uid` also empty (the label contains `"://"` but the host does not resolve to any in-cluster service or pod)
- **THEN** the client side resolves via connection-string resolution to `external/https://api.example.com` (`type="external"`, `labels={}`); the missing-UID fallback is NOT consulted (the label contains `"://"`, so connection-string resolution already produced the external node)

#### Scenario: UID present — fallback does not fire

- **WHEN** a series has `client="checkout"` with `client_k8s_pod_uid="abc"`
- **THEN** the client side resolves via pod-UID lookup (with the synth-pod fallback on topology miss); the missing-UID fallback is NOT consulted (UID is non-empty)

#### Scenario: Unresolvable connection-string and non-URL missing-UID endpoints both become external nodes

- **WHEN** series A has `client="https://api.example.com"`, `client_k8s_pod_uid=""` (label contains `"://"`, host unresolvable) and series B has `client="stray-caller"`, `client_k8s_pod_uid=""` (NON-URL label; UID empty so the fallback fires)
- **THEN** series A's client resolves to `id="external/https://api.example.com"` (`type="external"`, `labels={}`) via connection-string resolution and series B's client resolves to `id="external/stray-caller"` (`type="external"`, `labels={}`) via the missing-UID fallback. Both nodes appear in the same response as `type="external"` nodes.
