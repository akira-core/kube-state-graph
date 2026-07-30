## ADDED Requirements

### Requirement: Optional env-only ClickHouse credentials for the route store

When route resolution is enabled (`KSG_ROUTE_STORE_DSN` / `--route-store-dsn` non-empty), the server SHALL accept optional ClickHouse native auth credentials from `KSG_ROUTE_STORE_USERNAME` and `KSG_ROUTE_STORE_PASSWORD` only. The server MUST NOT register CLI flags for these credentials. Both env vars MUST be set together or both left unset; a half-configured pair MUST fail startup with an error that names both env vars and MUST NOT echo their values. When both are set, dial SHALL use those credentials (overriding any userinfo embedded in the DSN). When both are unset, DSN-embedded credentials SHALL continue to work. Credential values MUST NEVER appear in logs, spans, metric labels, or error messages — startup MAY log only a boolean indicating whether route-store auth is configured.

#### Scenario: Env credentials dial successfully

- **WHEN** `KSG_ROUTE_STORE_DSN` is a credential-free ClickHouse URL and `KSG_ROUTE_STORE_USERNAME` / `KSG_ROUTE_STORE_PASSWORD` are both set to valid credentials for that server
- **THEN** the process starts and the route store connection is established

#### Scenario: Half-configured credentials rejected at startup

- **WHEN** exactly one of `KSG_ROUTE_STORE_USERNAME` or `KSG_ROUTE_STORE_PASSWORD` is set
- **THEN** startup fails with an error naming both env vars and not containing either configured value

#### Scenario: DSN-embedded credentials remain valid when env unset

- **WHEN** `KSG_ROUTE_STORE_DSN` embeds `user:pass` and both route-store auth env vars are unset
- **THEN** the route store dials using the DSN userinfo (backward compatible)
