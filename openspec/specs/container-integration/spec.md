# container-integration Specification

## Purpose
TBD - created by archiving change add-k8s-pod-graph-api. Update Purpose after archive.
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

The container-integration suite SHALL contain at least one test for each of the following behaviours:

- A single-cluster graph rendering with `pod-mounts-pvc` edges and pod→node compound nesting derived from each pod's `labels.node` (no edge links a pod to its host K8s node).
- A multi-cluster graph with at least one `pod-calls-pod` edge whose source-node `labels.cluster` differs from its target-node `labels.cluster` (cross-cluster edge recovered via the topology pod-UID index).
- A connection-string client/server label containing `"://"` that does NOT resolve to an in-cluster pod/service producing an `external`-typed node with `labels={}` (D29).
- A headless per-pod connection string (`<pod>.<svc>.<ns>.svc.cluster.local`) resolving to its `type=service` node (the pod-hostname dropped) plus `service-selects-pod` fan-out edges — NOT to a specific pod.
- A ClusterIP-service connection string resolving to a `type=service` node plus `service-selects-pod` edges to its backing pods.
- The missing pod-UID human-label fallback producing an `external`-typed node (D27).
- `/v1/clusters` listing both synthetic cluster names.
- `/v1/edge-types` returning the static catalogue.

#### Scenario: All listed behaviours covered

- **WHEN** an operator inspects `internal/integration/`
- **THEN** at least one `*_test.go` test exists (and passes) for each behaviour bullet above

### Requirement: Tests use testify/suite for the container lifecycle

The integration test suite SHALL use `github.com/stretchr/testify/suite` to manage the per-package VictoriaMetrics container lifecycle. `SetupSuite` SHALL start the container and wait for readiness; `TearDownSuite` SHALL stop and remove it; `SetupTest` SHALL reset any per-test fixture state (e.g., truncate VM data with `/api/v1/admin/tsdb/delete_series` or rotate per-test discriminator labels). Tests SHALL be methods on the suite struct.

The same suite SHALL use `require` (not `assert`) for setup-class assertions whose failure makes the rest of the test meaningless: container start, JSON unmarshal of the system-under-test response, fixture ingestion `2xx`. `assert` is reserved for individual checks within a test where multiple failures are diagnostically useful.

#### Scenario: Suite setup starts the container

- **WHEN** the integration suite begins
- **THEN** `SetupSuite` runs once and returns only after the VictoriaMetrics container is ready (per the readiness-wait requirement)

#### Scenario: Suite teardown removes the container

- **WHEN** the integration suite finishes
- **THEN** `TearDownSuite` runs and the testcontainer is stopped and removed; no orphan containers persist between `go test` invocations

#### Scenario: Setup failures use require

- **WHEN** a fixture ingestion call returns a non-2xx response
- **THEN** the helper calls `require.NoError` (or equivalent) so the test halts immediately rather than continuing into an assertion against missing data

### Requirement: Auth-enabled VictoriaMetrics container scenario

The integration suite SHALL provide a way to start the testcontainers-managed VictoriaMetrics instance with HTTP Basic Auth enabled (`-httpAuth.username` / `-httpAuth.password`) and SHALL exercise both the credentialed and the unauthenticated path against it:

- With matching `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD` configured on the in-process API server, a graph build over ingested fixture series SHALL succeed.
- With no credentials configured against the same auth-enabled container, the build SHALL fail with an upstream-failure error (the container's 401 surfacing through the builder), proving the container actually enforces auth and the credentialed pass is not vacuous.

The scenarios SHALL respect the existing Docker gating (`SkipIfDockerUnavailable`).

#### Scenario: Credentialed build succeeds against auth-enabled upstream

- **WHEN** the VictoriaMetrics container is started with `-httpAuth.username=ksg -httpAuth.password=s3cret`, fixture series are ingested using those credentials, and the in-process API server is configured with `KSG_PROM_USERNAME=ksg` / `KSG_PROM_PASSWORD=s3cret`
- **THEN** `/v1/graph` returns 200 with the expected graph elements

#### Scenario: Unauthenticated build fails against auth-enabled upstream

- **WHEN** the same auth-enabled container is queried by an API server configured without upstream credentials
- **THEN** `/v1/graph` returns the upstream-failure error mapping (non-200) and the response does not contain the container's credentials

