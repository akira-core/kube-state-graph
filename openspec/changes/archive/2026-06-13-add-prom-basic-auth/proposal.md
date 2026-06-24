# Add HTTP Basic Auth for the VictoriaMetrics upstream

## Why

Production VictoriaMetrics deployments are commonly fronted by basic auth (`-httpAuth.username` / `-httpAuth.password`, vmauth, or an authenticating reverse proxy). kube-state-graph currently has no way to present credentials, so it cannot connect to a protected upstream at all.

## What Changes

- New env-only configuration: `KSG_PROM_USERNAME` + `KSG_PROM_PASSWORD`. **No CLI flags** for these values — flags leak via `ps` / container specs. This is a deliberate exception to the repo's env+flag dual-track convention.
- Validation: both must be set together (non-empty) or both unset; setting exactly one fails startup with a clear error.
- `pkg/promql.New` gains variadic functional options (`New(url, metrics, opts ...Option)`) with `WithBasicAuth(user, pass)` — existing call sites stay source-compatible (additive pkg/ API change, D32 external consumers unaffected).
- When enabled, every outbound upstream HTTP request (topology, service-graph, cluster discovery, `/readyz` probe `up` query) carries an `Authorization: Basic ...` header via a small `RoundTripper` wrapper in the transport chain (`otelhttp → basicAuth → base`).
- Credentials never appear in logs, spans, error messages, or response bodies.
- Integration coverage: the VictoriaMetrics testcontainer suite gains an auth-enabled variant (`-httpAuth.username` / `-httpAuth.password`) verifying end-to-end success with credentials and upstream `401` failure without.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `cluster-topology-source`: new requirement — optional basic-auth credentials for the single upstream endpoint: env-only sourcing, pair validation at startup, header applied to all upstream queries, credential non-disclosure in telemetry.
- `container-integration`: new requirement — an auth-enabled VictoriaMetrics container scenario exercising the credentialed path and the unauthenticated 401 path.

## Impact

- `internal/config`: `PromUsername` / `PromPassword` fields, `applyEnv` entries, `Validate()` pair check. Env-only (no `flag.StringVar`).
- `pkg/promql/client.go`: `Option` type, `WithBasicAuth`, `basicAuthTransport` RoundTripper.
- `cmd/kube-state-graph/main.go`: pass `WithBasicAuth` when configured.
- `internal/integration`: VM container option for `-httpAuth.*`, new auth test; `StartAPIServer` plumbing for credentials.
- No new dependencies. No HTTP API surface change. No response-body change (golden tests unaffected).
