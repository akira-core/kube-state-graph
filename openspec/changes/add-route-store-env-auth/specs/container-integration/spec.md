## ADDED Requirements

### Requirement: Route-store ClickHouse auth integration coverage

The integration suite SHALL exercise dialing the password-protected ClickHouse route-store container with credentials supplied via `store.WithAuth` (the production env-credential path) and SHALL prove unauthenticated Open fails against the same container.

#### Scenario: WithAuth succeeds against password-protected ClickHouse

- **WHEN** the ClickHouse container is started with `CLICKHOUSE_USER` / `CLICKHOUSE_PASSWORD`, the route store is opened with a credential-free DSN and `WithAuth` matching those credentials
- **THEN** Open succeeds (schema validation included)

#### Scenario: Unauthenticated Open fails against password-protected ClickHouse

- **WHEN** the same password-protected container is opened with a credential-free DSN and no `WithAuth`
- **THEN** Open fails (guards against a vacuous pass of the auth path)
