# container-integration Specification

## Purpose

Prove the API server end to end against a real VictoriaMetrics: the integration suite starts a pinned VictoriaMetrics container through testcontainers-go, ingests hand-crafted fixture series, drives the in-process server over HTTP, and asserts the response bodies, so behaviour that unit tests fake behind `promql.Querier` is exercised against real PromQL evaluation.

## Requirements
### Requirement: Container integration tests live in `internal/integration/`

The repository SHALL include a Go test package at `internal/integration/` whose test files exercise the API server end-to-end against a real VictoriaMetrics container started via testcontainers-go. These tests SHALL be runnable via `go test ./internal/integration/...` and SHALL be executed by CI on every PR.

#### Scenario: Tests discoverable by go test

- **WHEN** a developer runs `go test ./internal/integration/...`
- **THEN** the integration tests run and exit 0 against a working setup

#### Scenario: CI workflow runs integration tests

- **WHEN** an operator inspects the repository's CI workflow
- **THEN** a job exists that runs `go test ./...` (or `go test ./internal/integration/...`) on `ubuntu-latest` and gates merges on its success

### Requirement: Per-package VictoriaMetrics container via testcontainers-go

Each test package under `internal/integration/` SHALL start exactly one VictoriaMetrics container in `TestMain`, share that container across all tests in the package, and tear it down on package completion. The container image SHALL be pinned to a specific tag (e.g., `victoriametrics/victoria-metrics:v1.107.0`) — never `:latest`.

#### Scenario: Single container per package

- **WHEN** a test package runs
- **THEN** at most one VictoriaMetrics container is started for the duration of the package's tests, and the container is stopped after the last test

#### Scenario: Image tag pinned

- **WHEN** an operator inspects the test helper that starts the container
- **THEN** the image reference contains an explicit version tag and does not use `:latest`

### Requirement: Series injection via VM `/api/v1/import/prometheus`

Tests SHALL inject synthetic series into VictoriaMetrics via HTTP `POST` to `/api/v1/import/prometheus` with a Prometheus exposition body. Tests SHALL NOT use a separate fixtures container, a scrape stub, or VM remote-write protobuf for v1.

#### Scenario: Direct injection

- **WHEN** a test ingests fixtures
- **THEN** the helper issues `POST <vm.URL>/api/v1/import/prometheus` with the exposition body and confirms a 2xx response before continuing

#### Scenario: No scrape stub container started

- **WHEN** the integration test suite runs
- **THEN** no second container (e.g., a fixtures Pod) is created by testcontainers-go for the integration package; only the VictoriaMetrics container is present

### Requirement: API server runs in-process

The API server under test SHALL be constructed in-process via `api.New(cfg, ...).Handler()` and exposed via `httptest.NewServer`. The integration tests SHALL NOT containerise the API server. The configuration passed to the in-process server SHALL include `--prom-url` pointing at the testcontainers-managed VictoriaMetrics URL.

#### Scenario: In-process server bound to container URL

- **WHEN** a test starts the API server
- **THEN** the server's `cfg.PromURL` is set to the URL returned by the testcontainers helper, and the server is reachable at the URL returned by `httptest.NewServer`

### Requirement: Absolute timestamps for deterministic time-bucket alignment

Tests SHALL use absolute timestamps (e.g., `time.Date(...)`) when injecting samples and when constructing the `?start=` and `?end=` query parameters. Tests SHALL NOT use `time.Now()`-relative values for either side of the contract under verification.

#### Scenario: Fixtures and queries share a fixed window

- **WHEN** a test asserts a graph for a window
- **THEN** the timestamps embedded in the injected exposition body and the timestamps passed to `?start=` / `?end=` are derived from the same fixed `time.Time` value, not `time.Now()`

### Requirement: VictoriaMetrics readiness wait

Before the first `GET /v1/graph` is issued, the test helper SHALL poll VictoriaMetrics' `up{}` (or equivalent) until the response is non-empty, with a configurable budget (default 10 s). Tests that fail to observe readiness within the budget SHALL fail with a clear error.

#### Scenario: Readiness budget exhausted

- **WHEN** VictoriaMetrics is unreachable for longer than the readiness budget
- **THEN** the helper returns an error tagged `vm_not_ready` and the test fails immediately rather than continuing into a query that would otherwise return empty for the wrong reason

### Requirement: Per-test discriminator for parallel safety

Tests within the same package that run in parallel SHALL label injected series with a per-test discriminator (e.g., `test="<TestName>"`) so concurrent tests do not collide. Helpers MUST NOT scope queries to the discriminator implicitly; explicit selectors stay the test author's responsibility.

#### Scenario: Two tests run in parallel without collision

- **WHEN** two tests in the same package both ingest series and run with `t.Parallel()`
- **THEN** each test reads back only its own series via a discriminator in its API query / fixture set, and neither test fails because of the other's data

### Requirement: Coverage of the API contract

The container-integration suite SHALL contain at least one passing test for each behaviour named by this requirement's coverage scenarios.

#### Scenario: All listed behaviours covered

- **WHEN** an operator inspects `internal/integration/`
- **THEN** at least one `*_test.go` test exists (and passes) for each coverage scenario of this requirement

#### Scenario: Coverage — single-cluster graph

- **WHEN** the suite requests a single-cluster graph
- **THEN** a test asserts `pod-mounts-pvc` edges and pod→node compound nesting derived from each pod's `labels.node`
- **AND** no edge links a pod to its host K8s node

#### Scenario: Coverage — cross-cluster edge

- **WHEN** the suite requests a multi-cluster graph without a `cluster` filter
- **THEN** a test asserts at least one `pod-calls-pod` edge whose source-node `labels.cluster` differs from its target-node `labels.cluster`
- **AND** the cross-cluster edge is recovered via the topology pod-UID index

#### Scenario: Coverage — unresolved connection string

- **WHEN** a client/server label is a connection string containing `"://"` that does NOT resolve to an in-cluster pod/service
- **THEN** a test asserts it produces an `external`-typed node with `labels={}`

#### Scenario: Coverage — headless per-pod connection string

- **WHEN** a label is a headless per-pod connection string (`<pod>.<svc>.<ns>.svc.cluster.local`)
- **THEN** a test asserts it resolves to its `type=service` node (the pod-hostname dropped) plus `service-selects-pod` fan-out edges
- **AND** it does NOT resolve to a specific pod

#### Scenario: Coverage — ClusterIP-service connection string

- **WHEN** a label is a ClusterIP-service connection string
- **THEN** a test asserts it resolves to a `type=service` node plus `service-selects-pod` edges to its backing pods

#### Scenario: Coverage — missing pod-UID human-label fallback

- **WHEN** an endpoint carries no pod UID but a human label
- **THEN** a test asserts the fallback produces an `external`-typed node

#### Scenario: Coverage — az / env filter

- **WHEN** an `az` / `env` filtered request runs against fixtures that stamp the configured labels on every topology family (kube-state-metrics, kubelet, Harvest)
- **THEN** a test asserts the response contains only the matching zone / environment's workload and infrastructure
- **AND** `clusters` lists only its clusters

#### Scenario: Coverage — az / env filter matching nothing

- **WHEN** an `az` / `env` filtered request matches no series
- **THEN** a test asserts `200` with empty `elements` and `clusters: []` (not `outside_retention`)

#### Scenario: Coverage — namespace filter loading

- **WHEN** a `namespace` filtered request runs against fixtures spanning two namespaces
- **THEN** a test asserts only the requested namespace's pods, claims, and services are loaded
- **AND** K8s nodes and NetApp aggregates appear only by reference from that namespace's pods and claims

#### Scenario: Coverage — namespace filter with out-of-namespace peer

- **WHEN** a `namespace` filtered request's in-scope pod calls an out-of-namespace pod
- **THEN** a test asserts the peer is rendered as `external/<server label>` with `labels={}` and no pod is synthesised
- **AND** a series between two out-of-namespace pods produces nothing

#### Scenario: Coverage — cluster filter with other-cluster peer

- **WHEN** a `cluster` filtered request's in-scope pod calls a pod in another cluster
- **THEN** a test asserts the partner is `external/<server label>` and no other-cluster pod node is present

#### Scenario: Coverage — prune=false

- **WHEN** the suite issues `prune=false` requests
- **THEN** a test asserts a connectivity-disconnected pod surfaces together with its `pod-to-node`, `pod-mounts-pvc`, and `pvc-to-netapp-aggr` chain
- **AND** a `prune=false` request with no filter surfaces a podless K8s node

#### Scenario: Coverage — withdrawn edge_type parameter and route

- **WHEN** a `GET /v1/graph` request carries the withdrawn `edge_type` parameter
- **THEN** a test asserts the body is byte-identical to the same request without it
- **AND** `GET /v1/edge-types` returns `404` with the standard error body

### Requirement: Integration fixtures stamp the selector labels

Integration fixtures SHALL stamp `cluster` on every kube-state-metrics and kubelet series and `az` / `env` (under the default keys) on every topology family, so the selector-level filters can be exercised end-to-end against a real VictoriaMetrics.

#### Scenario: Filtered requests meet stamped fixtures

- **WHEN** a filtered integration test ingests its kube-state-metrics, kubelet and Harvest fixtures
- **THEN** every kube-state-metrics and kubelet series carries `cluster`
- **AND** every topology-family series carries `az` / `env` under the default keys

### Requirement: Tests use testify/suite for the container lifecycle

The integration test suite SHALL use `github.com/stretchr/testify/suite` to manage the per-package VictoriaMetrics container lifecycle, with tests written as methods on the suite struct. `SetupSuite` SHALL start the container and wait for readiness, `TearDownSuite` SHALL stop and remove it, and `SetupTest` SHALL reset any per-test fixture state.

#### Scenario: Suite setup starts the container

- **WHEN** the integration suite begins
- **THEN** `SetupSuite` runs once and returns only after the VictoriaMetrics container is ready (per the readiness-wait requirement)

#### Scenario: Suite teardown removes the container

- **WHEN** the integration suite finishes
- **THEN** `TearDownSuite` runs and the testcontainer is stopped and removed; no orphan containers persist between `go test` invocations

#### Scenario: Per-test state reset

- **WHEN** a test in the suite starts
- **THEN** `SetupTest` resets per-test fixture state (e.g., truncate VM data with `/api/v1/admin/tsdb/delete_series` or rotate per-test discriminator labels)

### Requirement: Setup-class assertions use require

The integration suite SHALL use `require` (not `assert`) for setup-class assertions whose failure makes the rest of the test meaningless: container start, JSON unmarshal of the system-under-test response, and fixture ingestion `2xx`. `assert` SHALL be reserved for individual checks within a test where multiple failures are diagnostically useful.

#### Scenario: Setup failures use require

- **WHEN** a fixture ingestion call returns a non-2xx response
- **THEN** the helper calls `require.NoError` (or equivalent) so the test halts immediately rather than continuing into an assertion against missing data

#### Scenario: Per-check assertions use assert

- **WHEN** a test makes several independent checks on a decoded response
- **THEN** each check uses `assert`, so one run reports every failing check

### Requirement: Auth-enabled VictoriaMetrics container scenario

The integration suite SHALL provide a way to start the testcontainers-managed VictoriaMetrics instance with HTTP Basic Auth enabled (`-httpAuth.username` / `-httpAuth.password`) and SHALL exercise against it both the credentialed path (matching `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD` on the in-process API server) and the unauthenticated path, respecting the existing Docker gating (`SkipIfDockerUnavailable`).

#### Scenario: Credentialed build succeeds against auth-enabled upstream

- **WHEN** the VictoriaMetrics container is started with `-httpAuth.username=ksg -httpAuth.password=s3cret`, fixture series are ingested using those credentials, and the in-process API server is configured with matching `KSG_PROM_USERNAME=ksg` / `KSG_PROM_PASSWORD=s3cret`
- **THEN** the graph build over the ingested fixture series succeeds and `/v1/graph` returns 200 with the expected graph elements

#### Scenario: Unauthenticated build fails against auth-enabled upstream

- **WHEN** the same auth-enabled container is queried by an API server configured without upstream credentials
- **THEN** the build fails with an upstream-failure error (the container's 401 surfacing through the builder) and `/v1/graph` returns the upstream-failure error mapping (non-200)
- **AND** the response does not contain the container's credentials, proving the container actually enforces auth and the credentialed pass is not vacuous

#### Scenario: Docker unavailable

- **WHEN** Docker is not usable on the host running the auth scenarios
- **THEN** `SkipIfDockerUnavailable` skips them like every other integration test

### Requirement: Route-store ClickHouse auth integration coverage

The integration suite SHALL exercise dialing the password-protected ClickHouse route-store container with credentials supplied via `store.WithAuth` (the production env-credential path) and SHALL prove unauthenticated Open fails against the same container.

#### Scenario: WithAuth succeeds against password-protected ClickHouse

- **WHEN** the ClickHouse container is started with `CLICKHOUSE_USER` / `CLICKHOUSE_PASSWORD`, the route store is opened with a credential-free DSN and `WithAuth` matching those credentials
- **THEN** Open succeeds (schema validation included)

#### Scenario: Unauthenticated Open fails against password-protected ClickHouse

- **WHEN** the same password-protected container is opened with a credential-free DSN and no `WithAuth`
- **THEN** Open fails (guards against a vacuous pass of the auth path)

