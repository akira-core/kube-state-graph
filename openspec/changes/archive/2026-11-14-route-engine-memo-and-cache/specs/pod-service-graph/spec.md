# Spec Delta

## ADDED Requirements

### Requirement: Route-resolution work is shared within a build

Within one build the route engine SHALL run the ingress probe once per `(destination IP, instant)`, the configuration load once per `(cluster, destination IP, instant)` and a gateway's translation once per `(cluster, destination IP list, gateway namespace, gateway name, RouteConfiguration name)`, sharing one execution between concurrent callers. A failed execution SHALL NOT be remembered or handed to another caller. Sharing SHALL NOT change any key's answer.

#### Scenario: Keys behind one ingress IP load its configuration once

- **GIVEN** a build whose route keys are distinct hosts carrying the same single destination IP, selecting the same ingress cluster
- **WHEN** the engine resolves them
- **THEN** the ingress probe and the configuration load for that IP each run once for the build
- **AND** every key's outcome and destination are those it produces when resolved alone

#### Scenario: Keys reaching one RouteConfiguration translate it once

- **GIVEN** route keys that select the same ingress cluster, destination IPs and gateway, and whose port and host select the same RouteConfiguration name
- **WHEN** the engine resolves them
- **THEN** that RouteConfiguration is translated once for the build

#### Scenario: A failure is retried, not remembered

- **WHEN** a shared probe, load or translation fails
- **THEN** the key that ran it records an engine error, and a later key needing the same work runs it again rather than receiving the remembered failure

#### Scenario: Another caller's timeout is not shared

- **GIVEN** two keys waiting on one shared execution
- **WHEN** the key running it fails because its own per-call context ended while the other key's context is still live
- **THEN** the other key runs the work itself under its own context instead of recording the first key's timeout

### Requirement: Batched route matching per RouteConfiguration

A route resolver MAY offer a batch entry point that receives a build's whole route-key set. When the resolver offers it, the reader SHALL use it, and the engine SHALL invoke the matcher once per distinct RouteConfiguration among the keys that reach the matcher, posing every such key's host and path in that one invocation. Each key's outcome and destination SHALL equal those a per-key resolution produces. A resolver without the batch entry point SHALL be driven per key exactly as before.

#### Scenario: One matcher invocation per RouteConfiguration

- **GIVEN** 32 route keys behind one ingress IP whose ports and hosts all select one RouteConfiguration of one gateway
- **WHEN** the reader resolves them through a batch-capable resolver
- **THEN** the matcher is invoked once with all 32 host and path queries, and every key is answered

#### Scenario: A failed batch is retried query by query

- **WHEN** a batched matcher invocation fails while the caller's context is still live
- **THEN** the engine retries each of that batch's queries in its own invocation, so a query the matcher cannot answer fails only its own key

#### Scenario: Batch outcomes equal per-key outcomes

- **GIVEN** a key set mixing routed hits, route misses, listener misses, ingress LB Service fallbacks and keys selecting different RouteConfigurations
- **WHEN** it is resolved once through the batch entry point and once key by key
- **THEN** the two index contents are identical

#### Scenario: The build deadline still leaves unanswered keys without an entry

- **WHEN** the build deadline ends a batch before a key is answered
- **THEN** that key has no entry in the resolved index and keeps its pre-existing external reason, exactly as a key the per-key loop never reached

#### Scenario: The per-call timeout bounds each stage of a batch

- **WHEN** the reader passes its per-call route timeout to the batch entry point
- **THEN** each key's preparation (probe, configuration load, gateway selection, translation) and each RouteConfiguration's matcher invocation is bounded by that timeout independently

### Requirement: Process-wide route-engine limit

The route engine SHALL bound, across every concurrent build in the process, the number of translations and matcher invocations in progress at once, sized from the number of CPUs the process may use. A caller waiting for capacity SHALL stop waiting when its context ends and SHALL then receive an error carrying no routing outcome, so the endpoint degrades to an external node with the engine-error diagnostic reason and no miss is cached or used to gate the ingress LB Service fallback.

#### Scenario: Concurrent builds share one ceiling

- **GIVEN** more concurrent builds resolving route keys than the limit admits
- **WHEN** they translate and invoke the matcher
- **THEN** at no instant are more translations and matcher invocations in progress than the limit

#### Scenario: Saturation degrades to an external node

- **WHEN** a key cannot obtain capacity before its per-call timeout or the build deadline
- **THEN** its resolution ends with an error and no outcome, and its endpoint falls back to the external node with the engine-error reason

#### Scenario: A translation stops when its caller gives up

- **WHEN** the context of a translation ends while it waits for its configuration controllers to synchronise
- **THEN** the translation returns the context's error instead of continuing to wait

### Requirement: Cross-build route result cache

The route engine SHALL keep successful answers in a process-wide cache keyed by caller cluster, host, path, port, destination IP list and resolution instant in milliseconds, holding at most 4096 answers (least recently used evicted) for at most 60 seconds each. Errors SHALL NOT be cached. Concurrent identical resolutions from different builds SHALL share one resolution, and a failed one SHALL NOT be handed to a waiter.

#### Scenario: A repeated build reuses the answers

- **GIVEN** a build that resolved its route keys at instant T
- **WHEN** a second build within 60 seconds resolves the same keys at the same instant T
- **THEN** it performs no probe, configuration load, translation or matcher invocation, and its index equals the first build's

#### Scenario: A different instant is a different answer

- **WHEN** a build resolves a key at an instant that differs from a cached answer's
- **THEN** the cached answer is not used

#### Scenario: Entries expire

- **WHEN** 60 seconds have passed since an answer was stored
- **THEN** the next resolution of that key at that instant runs the engine again

#### Scenario: Errors are not cached

- **WHEN** a key's resolution ends with an error
- **THEN** no answer is stored, and the next resolution of that key runs the engine again

#### Scenario: The cache is bounded

- **WHEN** more than 4096 distinct answers have been stored
- **THEN** the least recently used answers are evicted so at most 4096 are held

### Requirement: Route-store reads deduplicate version slots on the server

In its default read mode the route store SHALL keep, on the server, the highest-ingest-sequence row per version slot `(cluster, namespace, name, valid_from)` and SHALL apply the liveness test `at < valid_to` only to that result, so neither ended versions nor stale rewrite twins are transferred. The unique-rows mode SHALL keep its query shape. Client-side deduplication and liveness SHALL remain in both modes.

#### Scenario: A closed version is not transferred

- **GIVEN** a Gateway version whose closing rewrite (higher ingest sequence, `valid_to` before the instant) and stale open row (far-future `valid_to`) both exist in the store
- **WHEN** the reader loads the snapshot at the instant
- **THEN** the closing rewrite wins the server-side deduplication, fails the liveness test, and neither row is returned

#### Scenario: Liveness is filtered after deduplication

- **WHEN** the reader renders a version read in the default mode
- **THEN** `valid_to` does not appear in the predicate of the query that performs the per-slot deduplication, only in the query that consumes its result

#### Scenario: The pruned mode is unchanged

- **WHEN** the store is opened with the unique-rows mode
- **THEN** its version reads carry the `at < valid_to` predicate in the scanning query and no per-slot server-side deduplication

## MODIFIED Requirements

### Requirement: Optional env-only ClickHouse credentials for the route store

When route resolution is enabled (`KSG_ROUTE_STORE_DSN` / `--route-store-dsn` non-empty), the server SHALL accept optional ClickHouse native auth credentials from `KSG_ROUTE_STORE_USERNAME` and `KSG_ROUTE_STORE_PASSWORD` only, set together or both left unset, and MUST NOT register CLI flags for them. Credential values MUST NEVER appear in logs, spans, metric labels, or error messages — including the error reported for a route-store connection string that does not parse, which SHALL NOT quote the connection string or any part of it. When credentials will be sent over a connection without TLS, opening the store SHALL log a warning.

#### Scenario: Env credentials override DSN userinfo

- **WHEN** both env vars are set and the DSN also embeds userinfo
- **THEN** the dial uses the env-var credentials, overriding the userinfo embedded in the DSN

#### Scenario: Startup logs only whether auth is configured

- **WHEN** the server starts with route resolution enabled
- **THEN** it MAY log only booleans indicating whether route-store auth is configured and whether the route-store connection uses TLS (`route_store_tls`), never a credential value

#### Scenario: Env credentials dial successfully

- **WHEN** `KSG_ROUTE_STORE_DSN` is a credential-free ClickHouse URL and `KSG_ROUTE_STORE_USERNAME` / `KSG_ROUTE_STORE_PASSWORD` are both set to valid credentials for that server
- **THEN** the process starts and the route store connection is established

#### Scenario: Half-configured credentials rejected at startup

- **WHEN** exactly one of `KSG_ROUTE_STORE_USERNAME` or `KSG_ROUTE_STORE_PASSWORD` is set
- **THEN** startup fails with an error naming both env vars and not containing either configured value

#### Scenario: DSN-embedded credentials remain valid when env unset

- **WHEN** `KSG_ROUTE_STORE_DSN` embeds `user:pass` and both route-store auth env vars are unset
- **THEN** the route store dials using the DSN userinfo (backward compatible)

#### Scenario: A malformed DSN is not echoed

- **WHEN** `KSG_ROUTE_STORE_DSN` does not parse as a URL, for example because its password contains `/` or an invalid `%` escape
- **THEN** startup fails with an error that names the route-store DSN as malformed and contains neither the DSN nor any substring of its userinfo

#### Scenario: Credentials over plaintext warn

- **WHEN** the route store is opened with a password, from the DSN userinfo or the env vars, and the connection does not use TLS
- **THEN** a warning is logged saying credentials are sent unencrypted, without the credential values, and the store still opens

### Requirement: Route-store shape tolerance

The reader SHALL NOT treat a stale open row and its closing rewrite as two live versions, SHALL tolerate unknown fields in stored resource specs rather than failing the query, and SHALL bind a VirtualService gateway reference in the bare `<name>` form exactly as the qualified `<namespace>/<name>` form. A stored VirtualService or Service row whose spec cannot be parsed at all SHALL be skipped with a warning naming the row, and SHALL NOT fail the load of the destination IP it was read for.

#### Scenario: Version-close rewrites are deduplicated at query time

- **WHEN** the exporter's version-close operation REWRITES the previous open row, so until background merges collapse them a stale open row and its closing rewrite coexist
- **THEN** the reader deduplicates them at query time, so results do not depend on merge timing

#### Scenario: Specs follow the cluster's CRD version

- **WHEN** a stored resource spec, the API server's JSON verbatim, carries fields from a different CRD version
- **THEN** the reader tolerates the unknown spec fields

#### Scenario: Bare gateway names are namespace shorthand

- **WHEN** a VirtualService binds its gateway by the bare `<name>` form
- **THEN** it is Istio shorthand for a gateway in the VirtualService's own namespace and binds exactly as the qualified form does

#### Scenario: A bare binding is only loaded from a candidate gateway's namespace

- **WHEN** a VirtualService in a namespace holding no candidate gateway binds a gateway by a bare name that a candidate gateway in another namespace carries
- **THEN** the reader does not load it, while a VirtualService in any namespace that binds a candidate gateway by its qualified `<namespace>/<name>` form is still loaded

#### Scenario: An unparseable row is skipped

- **GIVEN** a VirtualService row bound to a candidate gateway whose stored spec is not valid JSON
- **WHEN** the reader loads the destination IP's configuration
- **THEN** that row is skipped with a warning, the load succeeds, and every other row is returned as before

### Requirement: Bounded route-resolution work per build

Route resolution SHALL NOT let its per-build cost grow without bound on the request path. Within one build the deduplicated route keys SHALL be resolved with bounded concurrency, the number of keys resolved SHALL be capped, and a truncation SHALL be logged with the number of dropped keys, never silently. A panic while resolving one key SHALL be recovered and recorded as that key's engine error; it SHALL NOT end the process or affect any other key.

#### Scenario: Dropped keys keep the external fallback

- **WHEN** the cap drops keys
- **THEN** their endpoints fall back to the pre-existing external-node behaviour, exactly as an unanswered key does when the build deadline fires

#### Scenario: Concurrency does not change the index

- **WHEN** keys are resolved concurrently
- **THEN** each key's answer is independent of every other key's, so the resolved index's contents are unchanged, and any per-build memoisation shared across concurrent resolutions is safe for concurrent use

#### Scenario: Independent keys resolve concurrently

- **GIVEN** several distinct route keys collected from one service-graph vector
- **WHEN** the reader resolves them
- **THEN** the resulting index is identical to the index a strictly serial resolution produces

#### Scenario: Truncation is reported

- **GIVEN** a build whose collected route keys exceed the cap
- **WHEN** the reader resolves them
- **THEN** the number of dropped keys is logged and the corresponding endpoints fall back to external nodes

#### Scenario: A panicking key degrades alone

- **WHEN** resolving one route key panics
- **THEN** the panic is logged, that key's endpoint falls back to the external node with the engine-error reason, every other key is answered as usual, and the build completes
