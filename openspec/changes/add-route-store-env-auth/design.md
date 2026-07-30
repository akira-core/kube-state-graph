# Design: Env-only auth for the ClickHouse route store

## Context

`pkg/route/store.Open` dials ClickHouse from a single DSN string (`clickhouse.ParseDSN` → `clickhouse.Open`). Credentials today live only as DSN userinfo. Configuration follows env+flag dual-track for the DSN itself (`--route-store-dsn` / `KSG_ROUTE_STORE_DSN`), but secret-carrying flags are undesirable — the same constraint that produced `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD` for VictoriaMetrics.

## Goals / Non-Goals

**Goals:**

- Optional ClickHouse native auth credentials for the route store, sourced from env only.
- Fail-fast validation of half-configured credentials at startup.
- Credentials applied at dial time (override DSN userinfo when set).
- Zero credential disclosure in logs, spans, errors, metrics.
- Backward-compatible DSN-embedded userinfo when env credentials are unset.
- Integration proof against the existing password-protected ClickHouse testcontainer.

**Non-Goals:**

- TLS / mTLS ClickHouse transport changes.
- Credential hot-reload (restart to rotate — same as prom basic auth).
- Password-file sourcing.
- Forcing credentials whenever `RouteStoreDSN` is set (anonymous ClickHouse remains valid).
- RFC 7617 `:` rejection on usernames (HTTP Basic only; ClickHouse native auth differs).

## Decisions

### D1: Env-only configuration, no CLI flags

`KSG_ROUTE_STORE_USERNAME` / `KSG_ROUTE_STORE_PASSWORD` are read in `applyEnv` only; no `flag.StringVar`. Same rationale as prom basic auth D-A1 (`ps` / container-spec exposure).

### D2: Pair validation — both or neither

`config.Validate()` errors when exactly one of `RouteStoreUsername` / `RouteStorePassword` is non-empty. Error text names both env vars but never echoes values.

### D3: `WithAuth` functional option, apply before dial

`Option` becomes `func(*openConfig)` so auth and `WithUniqueRows` are collected before `clickhouse.Open`. `WithAuth` overwrites `chOpts.Auth.Username` / `Password` after `ParseDSN` when username is non-empty. Call sites that only use `WithUniqueRows()` remain source-compatible via the factory.

### D4: Env credentials win over DSN userinfo; DSN-only remains valid

When the env pair is set, dial uses those credentials regardless of DSN userinfo (operators can keep a credential-free DSN). When unset, embedded DSN credentials still work — no migration break for existing deployments or integration tests.

### D5: Credential non-disclosure

No log line, span attribute, metric label, or error string may contain the password (or username). Startup logs only `route_store_auth` as a boolean. Validation errors name env-var names only.

### D6: Integration coverage via existing password-protected CH

The route e2e ClickHouse container already sets `CLICKHOUSE_USER` / `CLICKHOUSE_PASSWORD`. A new test opens with a credential-free DSN + `WithAuth` (success) and the same DSN without auth (failure), proving auth is enforced.

## Risks / Trade-offs

- [Option type signature changes from `func(*CH)` to `func(*openConfig)`] → only factories are used in-tree; external callers constructing raw `Option` closures are unlikely. Acceptable for this opt-in package surface.
- [Intentionally empty ClickHouse password via env] → pair validation treats empty password with non-empty username as half-configured (same as prom). Operators who need an empty password keep credentials in the DSN instead.
