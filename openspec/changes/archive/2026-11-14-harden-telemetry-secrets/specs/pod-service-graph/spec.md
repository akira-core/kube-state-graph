# Spec Delta

## ADDED Requirements

### Requirement: Boot warning for route-store DSN userinfo

When route resolution is enabled and the route-store DSN (`--route-store-dsn` / `KSG_ROUTE_STORE_DSN`) carries userinfo, the server SHALL log one warning at boot that credentials are embedded in the DSN, naming `KSG_ROUTE_STORE_USERNAME` and `KSG_ROUTE_STORE_PASSWORD` as the env-only alternative. The warning SHALL NOT carry the DSN, any part of its userinfo, or a DSN parse error, and SHALL NOT change how the store is dialed.

#### Scenario: DSN with userinfo

- **WHEN** the server starts with `--route-store-dsn=clickhouse://ksg:s3cret@ch:9000/routing`
- **THEN** the boot log carries one warning naming `KSG_ROUTE_STORE_USERNAME` and `KSG_ROUTE_STORE_PASSWORD`, and no log line contains `s3cret`
- **AND** the route store dials exactly as before

#### Scenario: DSN without userinfo

- **WHEN** the server starts with `--route-store-dsn=clickhouse://ch:9000/routing`
- **THEN** no DSN-userinfo warning is logged
