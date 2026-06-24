# container-integration delta — add-prom-basic-auth

## ADDED Requirements

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
