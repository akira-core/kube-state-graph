# Design: HTTP Basic Auth for the VictoriaMetrics upstream

## Context

`pkg/promql.Client` (constructed by `promql.New` in `pkg/promql/client.go`) is the single HTTP path to upstream VictoriaMetrics — topology fan-out, service-graph query, cluster discovery, and the `/readyz` `up` probe all go through it. The transport today is `otelhttp.NewTransport(base)` with no credential support. Configuration follows an env+flag dual-track (`internal/config`, env first, flag overrides), but no secret-carrying value exists yet on the upstream side; the API-key auth (`--api-keys-file`) is inbound-only.

Production VictoriaMetrics is frequently protected by basic auth (`-httpAuth.*`, vmauth, reverse proxy). The repo constraint set that shapes this design: `pkg/` must stay importable without `internal/*` (D32), no new dependencies without a design note, credentials must never reach logs/spans, and unit tests must not contact a real upstream socket.

## Goals / Non-Goals

**Goals:**

- Optional basic-auth credentials for the upstream connection, sourced from env only.
- Fail-fast validation of half-configured credentials at startup.
- Credentials applied uniformly to every upstream HTTP request.
- Zero credential disclosure in logs, spans, errors, metrics.
- Backward-compatible `pkg/promql` API (additive options; existing call sites compile unchanged).
- Integration proof against a real auth-enabled VictoriaMetrics container.

**Non-Goals:**

- Bearer-token / OAuth / mTLS upstream auth (future change if needed).
- Credential hot-reload (unlike `--api-keys-file`; restart to rotate — acceptable because upstream creds rotate rarely and a K8s Deployment env change forces a rollout anyway).
- Password-file (`KSG_PROM_PASSWORD_FILE`) sourcing — K8s Secrets inject env vars directly (`secretKeyRef`); add a file variant later only if demanded.
- Inbound API-key auth changes.

## Decisions

### D-A1: Env-only configuration, no CLI flags

`KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD` are read in `applyEnv` only; no `flag.StringVar` is registered. A password flag would be visible in `ps`, container specs, and shell history. This is a deliberate, documented exception to the repo's env+flag dual-track convention. The flag help for `--prom-url` is the natural place to cross-reference the env vars in docs.

*Alternative considered:* env + flag parity (repo convention) — rejected for the `ps` exposure; *URL-embedded credentials* (`http://user:pass@host`) — rejected because `PromURL` flows into validation errors and operator-facing config, making accidental disclosure likely.

### D-A2: Pair validation — both or neither

`config.Validate()` errors when exactly one of `PromUsername` / `PromPassword` is non-empty. A half-set pair is always a configuration mistake; silently running unauthenticated (or with an empty password) would 401 at first query with a far less obvious diagnosis. Error text names both env vars but never echoes values.

### D-A3: Functional options on `promql.New`

`New(promURL string, metrics Metrics, opts ...Option)` with `WithBasicAuth(username, password string) Option`. Variadic options keep the two existing call sites (`cmd/kube-state-graph/main.go`, `internal/integration/vmsuite.go`) and any external D32 consumer source-compatible, and give future transport options (TLS config, bearer token) a home without further signature churn.

*Alternative considered:* a `ClientConfig` struct parameter — breaking change for external consumers; *a separate `NewWithAuth` constructor* — combinatorial explosion as options accumulate.

### D-A4: Custom ~10-line `basicAuthTransport`, chain `otelhttp → basicAuth → base`

A private `http.RoundTripper` that clones the request (RoundTrippers must not mutate the caller's request), calls `SetBasicAuth`, and delegates to the inner transport. Sits inside the otelhttp wrapper so the traced request is the final authenticated one; otelhttp records method/URL/status, never headers, so the `Authorization` value stays out of spans.

*Alternative considered:* `prometheus/common/config.NewBasicAuthRoundTripper` — pulls the `common/config` package surface (secret types, file loading) into the dependency graph for what is ten lines of code; violates the "don't add dependencies casually" rule for no gain.

### D-A5: Credential non-disclosure

The config struct is never logged today; this change adds a guard obligation, not a mechanism: no new log line, span attribute, metric label, or error string may contain `PromPassword` (or the username, which can be sensitive in some setups). The existing `Instant` logging only emits query name/statement/timestamp — unchanged. Validation errors name env-var names only.

### D-A6: Integration coverage via VM `-httpAuth.*`

The testcontainers VictoriaMetrics helper gains an option to start the container with `-httpAuth.username` / `-httpAuth.password`. Two integration scenarios: (1) API server configured with matching `KSG_PROM_USERNAME`/`KSG_PROM_PASSWORD` env → graph builds successfully; (2) no credentials against the auth-enabled container → build fails (upstream 401 surfaces as an upstream-failure build error), proving auth is actually enforced by the container (guards against a vacuous pass). Unit layer covers the transport itself with a fake inner RoundTripper — no socket, per the test-stack boundary rule.

## Risks / Trade-offs

- [Credentials in env are readable via `/proc/<pid>/environ` and `kubectl describe` on the pod spec] → standard K8s posture; mitigated by Secret-backed `secretKeyRef` env injection and RBAC, out of this server's control. Documented in README.
- [No hot rotation — restart required] → acceptable; env change in a Deployment triggers a rollout. Revisit only if operators demand file-based reload.
- [Future option creep on `promql.New`] → functional options absorb it; each new option is additive.
- [Integration test depends on VM `-httpAuth.*` flag behaviour] → flag is stable VM single-node API; test asserts the 401 path so a silently-disabled auth container fails the suite.

## Migration Plan

Additive, default-off. Deploys with no env set behave byte-identically to today. Rollback = unset the env vars. No API, wire-format, or golden-file impact.

## Open Questions

(none)
