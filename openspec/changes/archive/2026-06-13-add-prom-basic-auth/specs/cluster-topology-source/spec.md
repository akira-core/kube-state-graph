# cluster-topology-source delta — add-prom-basic-auth

## ADDED Requirements

### Requirement: Optional basic-auth credentials for the upstream endpoint

The server SHALL support optional HTTP Basic Auth credentials for the single upstream Prometheus-compatible endpoint, sourced **exclusively** from the environment variables `KSG_PROM_USERNAME` and `KSG_PROM_PASSWORD`. No CLI flag SHALL exist for either value — credential-carrying flags leak through process listings and container specs; this is a deliberate exception to the env+flag dual-track configuration convention.

When both variables are set (non-empty), every outbound HTTP request to the upstream — topology queries, the service-graph query, the cluster-discovery query, and the `/readyz` `up` probe — SHALL carry an `Authorization: Basic` header for those credentials. When both are unset, requests SHALL carry no `Authorization` header and behaviour is unchanged from an unauthenticated deployment.

Setting exactly one of the two variables (non-empty) SHALL fail server startup with a validation error that names both environment variables but does NOT echo either value.

The credential values SHALL NOT appear in any log line, trace span attribute, metric label, error message, or HTTP response body. Rotation requires a process restart — there is no hot reload for upstream credentials.

#### Scenario: Credentials applied to all upstream queries

- **WHEN** the server starts with `KSG_PROM_USERNAME=ksg` and `KSG_PROM_PASSWORD=s3cret` and serves a `/v1/graph` request
- **THEN** every upstream HTTP request issued for the build (topology fan-out, service-graph, and any cluster-discovery or readiness query) carries `Authorization: Basic` for `ksg:s3cret`

#### Scenario: No credentials configured

- **WHEN** the server starts with neither `KSG_PROM_USERNAME` nor `KSG_PROM_PASSWORD` set
- **THEN** upstream requests carry no `Authorization` header and startup validation passes

#### Scenario: Half-configured credentials rejected at startup

- **WHEN** the server starts with `KSG_PROM_USERNAME=ksg` and no `KSG_PROM_PASSWORD` (or vice versa)
- **THEN** `config.Validate` returns an error naming `KSG_PROM_USERNAME` and `KSG_PROM_PASSWORD`, the error does not contain the configured value, and the process exits non-zero before binding the listener

#### Scenario: No CLI flag exists for credentials

- **WHEN** the server is started with `--prom-username=x` or `--prom-password=x`
- **THEN** flag parsing fails with an unknown-flag error, because credentials are env-only

#### Scenario: Credentials never logged

- **WHEN** the server runs with credentials configured at any log level, including `debug`, and upstream queries succeed or fail
- **THEN** no emitted log line, span attribute, or error string contains the configured username or password
