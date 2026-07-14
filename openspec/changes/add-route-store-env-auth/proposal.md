# Add env-only auth credentials for the ClickHouse route store

## Why

Production ClickHouse deployments for the versioned Istio-config store typically require native username/password auth. Today operators must embed credentials in `KSG_ROUTE_STORE_DSN` / `--route-store-dsn` (`clickhouse://user:pass@host/...`), which puts secrets in URLs that surface in process listings, container specs, and config dumps. VictoriaMetrics already solved this with env-only `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD`; route store should follow the same pattern.

## What Changes

- New env-only configuration: `KSG_ROUTE_STORE_USERNAME` + `KSG_ROUTE_STORE_PASSWORD`. **No CLI flags** for these values — flags leak via `ps` / container specs. Deliberate exception to the repo's env+flag dual-track convention (same rationale as prom basic auth).
- Validation: both must be set together (non-empty) or both unset; setting exactly one fails startup with a clear error naming the env vars (never echoing values).
- `pkg/route/store.Open` gains `WithAuth(username, password string) Option` applied after `ParseDSN` and before dial — env credentials override any DSN-embedded userinfo.
- When unset, DSN-embedded `user:pass` remains supported (backward compatible).
- Credentials never appear in logs, spans, error messages, or response bodies. Startup logs only a boolean `route_store_auth`.
- Integration coverage: password-protected ClickHouse container — Open with credential-free DSN + `WithAuth` succeeds; same DSN without auth fails.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `pod-service-graph`: optional native auth credentials for the route-store ClickHouse connection — env-only sourcing, pair validation at startup, applied at dial via `WithAuth`, credential non-disclosure in telemetry.
- `container-integration`: route-store suite exercises env-style `WithAuth` against the password-protected ClickHouse container and asserts unauthenticated Open fails.

## Impact

- `internal/config`: `RouteStoreUsername` / `RouteStorePassword` fields, `applyEnv` entries, `Validate()` pair check. Env-only (no `flag.StringVar`).
- `pkg/route/store`: `Option` collects into `openConfig` before dial; new `WithAuth`; `WithUniqueRows` unchanged for callers.
- `cmd/kube-state-graph/main.go`: pass `WithAuth` when username is non-empty.
- `internal/integration`: `TestOpenWithAuthUsesEnvStyleCredentials` on `RouteStoreSuite`.
- No new dependencies. No HTTP API surface change. No response-body change (golden tests unaffected).
