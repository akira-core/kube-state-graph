## MODIFIED Requirements

### Requirement: Hot reload of the routing table

When a routing-table file is configured and `--backends-reload-interval` is positive, the server SHALL re-read the file on that interval and, when its parsed content differs from the live table, validate it and **atomically** replace the live table. A file that fails to read, parse, or validate SHALL be rejected **wholesale**, and a partially applied table SHALL never be observable. The content the reload compares against SHALL be the exact bytes the live table was parsed from — never a second read of the file — so a change written between the startup read and the first reload is applied rather than mistaken for the live content. An embedder that cannot supply those bytes SHALL get one re-parse and swap on the first reload instead.

#### Scenario: New backend picked up without restart

- **WHEN** the mounted file is updated to add a backend for `zone-c` and the reload interval elapses
- **THEN** the next request carrying `az=zone-c` is dispatched to it, with no process restart

#### Scenario: Invalid file leaves the live table serving

- **WHEN** the mounted file is replaced with unparseable content and the reload interval elapses
- **THEN** requests continue to be dispatched by the previous table, an Error log names the parse failure, and the reload-failure counter increments

#### Scenario: Any rejected reload keeps the live table

- **WHEN** a reloaded file fails to read, parse, or validate
- **THEN** the previous table keeps serving unchanged, an Error log names the reason, and the reload-failure counter increments

#### Scenario: Reload disabled

- **WHEN** the server starts with a reload interval of zero and the file is subsequently changed
- **THEN** the live table is unchanged and no reload is attempted
- **AND** the table read at startup serves for the process lifetime

#### Scenario: Reload covers every backend field

- **WHEN** an accepted reload changes backend membership, URLs, families, zones, or credential-variable names
- **THEN** each change takes effect in the new live table

#### Scenario: Retired backend's connections released

- **WHEN** a reload removes a backend from the table
- **THEN** the idle connections held for that backend's URL are closed

#### Scenario: A change made right after startup is applied

- **WHEN** the routing file is rewritten after the server parsed it at startup but before the reload loop first runs
- **THEN** the first reload applies the new content, because the digest it compares against is that of the bytes the startup table was parsed from

### Requirement: Per-backend credentials sourced from the environment

A backend MAY name the environment variables holding its HTTP Basic Auth pair via `usernameEnv` and `passwordEnv`. The **values** SHALL be read from the process environment and SHALL NOT appear in the routing file, in any CLI flag, log line, trace span attribute, metric label, error message, or HTTP response body. Credentials SHALL be attached only to requests addressed to that backend's own scheme AND host.

#### Scenario: Per-backend credentials applied

- **WHEN** backend `zone-a` declares `usernameEnv: KSG_PROM_USERNAME_A` / `passwordEnv: KSG_PROM_PASSWORD_A` and both variables are set
- **THEN** every request to `zone-a`'s URL carries `Authorization: Basic` for that pair, and requests to other backends do not

#### Scenario: Cross-host redirect carries no credentials

- **WHEN** a request to a backend with credentials is redirected to a different host
- **THEN** the redirected request carries no `Authorization` header

#### Scenario: Same-host scheme downgrade carries no credentials

- **WHEN** a request to an `https` backend with credentials is redirected to `http` on the same host
- **THEN** the redirected request carries no `Authorization` header

#### Scenario: Credential rotation

- **WHEN** an operator rotates a credential **value** in the environment
- **THEN** the new value takes effect only after a process restart, while the variable **names** are reloadable

#### Scenario: Credentials never logged

- **WHEN** the server runs at `debug` level with per-backend credentials configured and a backend query fails
- **THEN** no log line, span attribute, or error string contains either credential value, while the backend name and variable names may appear

### Requirement: Multi-backend readiness and retention probes

The readiness probe SHALL probe **every** backend declared in the live table and SHALL report ready only when all of them answer successfully. Its failure body SHALL name the backends that did not answer. The probe query SHALL be `count(up)`, which answers one series per backend whatever the number of scrape targets, and which is empty exactly when `up` is. Concurrent readiness probes SHALL share one probe of each backend, and a completed outcome MAY be reused for at most 1 second (see the query cache's "Probes bypass the cache").

#### Scenario: One unreachable backend makes the server not ready

- **WHEN** two backends are declared and one refuses connections
- **THEN** the readiness probe returns not-ready and its body names the refusing backend

#### Scenario: Probe cost independent of scrape targets

- **WHEN** a backend scrapes 10,000 targets
- **THEN** its readiness probe answer carries one series, not 10,000

### Requirement: Routing observability

The server SHALL expose self-metrics describing the live routing table and per-backend query outcomes — at minimum a gauge of the number of backends in the live table, a counter of routing-table reload attempts labelled by result, and a counter of upstream query failures labelled by backend. Every per-backend series SHALL follow the live table: a reload that removes a backend SHALL remove that backend's in-flight gauge and slot-wait histogram series, and a query still in flight on the removed backend SHALL NOT recreate them. The client span for every upstream query SHALL carry the backend name as an attribute.

#### Scenario: Backend gauge reflects the live table

- **WHEN** a reload changes the table from two backends to three
- **THEN** the backend-count gauge reads 3 after the reload

#### Scenario: Reload result counted

- **WHEN** one reload succeeds and a later one is rejected as invalid
- **THEN** the reload counter carries one increment for the success result and one for the failure result

#### Scenario: Span names the backend

- **WHEN** a query is issued to backend `zone-b` with tracing enabled
- **THEN** the emitted client span carries an attribute identifying `zone-b`

#### Scenario: Removed backend's limiter series disappear

- **WHEN** a reload removes backend `zone-c` while one of its queries is still in flight
- **THEN** after the reload no `kube_state_graph_upstream_inflight` or `kube_state_graph_upstream_slot_wait_seconds` series is labelled `zone-c`, including after that query completes

## ADDED Requirements

### Requirement: Upstream evaluation timeout follows the caller's deadline

Every upstream instant query issued under a context that carries a deadline SHALL send the upstream API's `timeout` parameter, set to the time remaining before that deadline less a 500 ms margin, truncated to whole seconds. When less than one whole second would remain the parameter SHALL be omitted and the context deadline alone SHALL bound the query. A coalesced shared query SHALL derive it from the shared query's own deadline. No per-request share of a store's concurrency slots SHALL be imposed.

#### Scenario: Build deadline reaches the store

- **WHEN** a query is issued with 14.9 s left before its build deadline
- **THEN** the upstream request carries `timeout=14s`

#### Scenario: No deadline, no parameter

- **WHEN** a query is issued under a context with no deadline
- **THEN** the upstream request carries no `timeout` parameter

#### Scenario: Too little time left

- **WHEN** a query is issued with 1.2 s left before its deadline
- **THEN** the upstream request carries no `timeout` parameter

### Requirement: Upstream response size bound

Every upstream response body SHALL be bounded by `--upstream-max-response-bytes` / `KSG_UPSTREAM_MAX_RESPONSE_BYTES` (default 536870912, 512 MiB); `0` SHALL disable the bound and a negative value SHALL be a configuration error. A response whose declared length exceeds the bound SHALL be refused without reading its body, and a body that grows past it SHALL fail when the bound is crossed, never be truncated into a shorter result. Exceeding the bound SHALL be an upstream query error like any other: a required leg fails closed, an optional leg degrades, and nothing is cached. An embedder's client SHALL apply the same default unless it opts out.

#### Scenario: Oversized response fails the query

- **WHEN** the bound is 1 MiB and a backend answers a required query with a 2 MiB body
- **THEN** the query fails as an upstream error naming the backend and no result is cached

#### Scenario: Bound disabled

- **WHEN** the server starts with `--upstream-max-response-bytes=0`
- **THEN** no response body is bounded
