## MODIFIED Requirements

### Requirement: Bounded root sets

When a root kind's first read would take more queries than the `/v1/storage-graph` cap for the same kind under the shared chunk bounds (the byte budget and the alternation value bound), the request SHALL be rejected 400 `invalid_scope` before any upstream query, and the build SHALL NOT fall back to reading the family across the selected clusters.

#### Scenario: Too many node roots

- **WHEN** a client sends enough `node=` values that the first `kube_pod_info` read exceeds the cap
- **THEN** the server returns 400 with `reason: "invalid_scope"` and issues no upstream query

#### Scenario: Many short roots reach the cap by count

- **WHEN** a client sends more `node=` values than the cap's chunk count times 100, each short enough that the byte budget alone would still admit them
- **THEN** the server returns 400 with `reason: "invalid_scope"` and issues no upstream query, because every chunk also holds at most 100 values

### Requirement: Known pods and claims are read across namespaces in one restriction

A read of known pods or claims SHALL be issued one query per chunk, carrying the chunk's namespaces as one alternation and its pod or claim names as another, chunked under the shared byte budget and so that neither alternation holds more than 100 values (a `pod` root's first read follows the seed's split instead: its name alternation is bounded, and its namespace alternation is taken from the request's roots and is not), instead of one query per namespace. A row whose `(namespace, name)` pair the read did not request SHALL be discarded before any later read's restriction is computed from it.

#### Scenario: Several namespaces in one query

- **WHEN** a client roots at `node=worker-1` and the seed returns `shop/orders-0`, `shop/web-0` and `platform/redis-0`
- **THEN** incarnation completion is issued as one query restricted to `namespace=~"platform|shop"` and `pod=~"orders-0|redis-0|web-0"` beside the request matchers, not one query per namespace

#### Scenario: A cross pair is discarded

- **WHEN** that query also returns `platform/web-0`, which runs on `worker-2`
- **THEN** the `platform/web-0` row is discarded, no claim, node or controller is read for it, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: Claims are grouped the same way

- **WHEN** the build tracks `shop/data` and `platform/queue`
- **THEN** the claim-info, claim-annotation, kubelet and mounter-completion reads are each one query restricted to `namespace=~"platform|shop"` and `persistentvolumeclaim=~"data|queue"`, and a returned `shop/queue` or `platform/data` row is discarded

#### Scenario: A large pair set is chunked

- **WHEN** the requested pairs exceed the byte budget
- **THEN** they are split into several queries, each carrying only its own pairs' namespaces and names, the results merge in chunk order, and a single pair larger than the budget is still issued

#### Scenario: A node with more than 100 pods keeps every alternation at 100 values or fewer

- **WHEN** a client roots at `node=worker-1`, the seed returns 150 pods whose names together fit the byte budget, and they span 3 namespaces
- **THEN** incarnation completion, the claim-binding read and the `kube_pod_owner` read are each issued as 2 queries, each carrying at most 100 distinct pod names and its own pairs' namespaces, issued in the same round and merged in chunk order
- **AND** the body is byte-identical to the body the same build produces with the pods in one query

#### Scenario: The bound counts each alternation separately

- **WHEN** the pairs a later read requests — pods a seed or a mounter read returned, or tracked claims — hold 100 or fewer distinct names but more than 100 distinct namespaces
- **THEN** the pairs are still split so that no query's namespace alternation holds more than 100 values

