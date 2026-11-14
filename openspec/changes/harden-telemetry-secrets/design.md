# Design

## Context

See proposal.md — Why. The current state:

- `internal/telemetry.Init` builds one resource with `resource.WithFromEnv()`, `WithProcess()`, `WithHost()` and `WithTelemetrySDK()`, then — when ANY of the three endpoint variables is set — builds BOTH a trace exporter and a log exporter and installs both SDK providers. `Providers.Shutdown` shuts both down.
- `telemetry.NewSlogHandler(local)` fans each record out to the local JSON handler (levelled by `--log-level` through `observability.NewLogHandler`) and to `otelslog.NewHandler`, whose `Enabled` asks the global `LoggerProvider`. With an SDK provider whose only processor is a `BatchProcessor`, that is always true.
- `cmd/kube-state-graph/main.go` drains HTTP under `shutdownCtx` (deadline `max(WriteTimeout, 10s)`), then flushes telemetry under a FRESH `context.WithTimeout(context.Background(), 10s)`.
- `loadAPIKeys` logs "api key auth enabled (file)" whatever the key count, and logs nothing about the source of `--api-keys`. Nothing inspects the route-store DSN for userinfo.
- `--prom-url` is logged verbatim in three places and echoed with `%q` by `config.Validate`. A routing-file backend renders `url=<raw>` in `Backend.String()`, which the startup log and every reload log carry; `validateBackendURL` wraps the `url.Parse` error (which quotes the whole input) and echoes a hostless URL with `%q`.

## Goals / Non-Goals

**Goals:**

- No secret an operator supplied on argv or in a URL reaches an OTLP resource, a log line or an error string through the paths above.
- The OTLP log pipeline carries exactly the records the local sink carries.
- Telemetry exports only the signals the operator configured an endpoint for, and its shutdown stays inside the server's grace deadline.
- No HTTP contract change: no route, parameter, status, body, header or self-metric moves; goldens are untouched.

**Non-Goals:**

- Removing `--api-keys` or rejecting a DSN with userinfo (both would break existing deployments and need a `docs/BREAKING.md` entry).
- Redacting the route-store DSN inside driver errors (a separate cleanup), or connection-string labels in response bodies.
- Supporting `OTEL_TRACES_EXPORTER` / `OTEL_LOGS_EXPORTER=none`; the spec's recognised-variable list is unchanged.

## Decisions

### D1. Name the process detectors instead of `WithProcess()` (C2)

`buildResource` uses `WithProcessPID`, `WithProcessExecutableName`, `WithProcessRuntimeName`, `WithProcessRuntimeVersion` and `WithProcessRuntimeDescription`. It drops `process.command_args` (carries argv, hence secrets), `process.executable.path` (filesystem layout, no correlation value beyond the name) and `process.owner` (the OS user name). A unit test asserts the three keys are absent and that `process.pid` and `process.runtime.name` are present, so the test fails if the detectors stop running altogether.

Rejected: keeping `WithProcess()` and filtering the three attributes out of the merged resource afterwards. `WithProcess()` is an aggregate whose member list is the SDK's to change; a filter written today would silently pass a detector added in a later SDK release. Naming the detectors is an allowlist. Also rejected: dropping process detection entirely — `process.pid` and the runtime version are what an operator uses to tell two replicas or two builds apart in a trace backend.

### D2. Filter the bridge by the configured level with a wrapper handler (C4)

`telemetry.NewSlogHandler(local, level slog.Leveler)` wraps the otelslog bridge in a `levelFilter` handler whose `Enabled` is `level >= minimum && inner.Enabled(...)`. The minimum is the same `slog.Level` the local sink uses: `observability.ParseLevel` is extracted from `NewLogHandler` and `main` passes its result to both, so there is ONE string-to-level parser. `levelFilter.WithAttrs` / `WithGroup` return a wrapped handler, so `logger.With(...)` keeps the filter. `fanoutHandler.Enabled` stays `local || bridge`; with both honouring one level it now answers false for a Debug record at `info`, which also lets callers gate expensive Debug argument construction on `Enabled`.

Rejected: an sdk/log processor-level filter (`FilterProcessor`). It runs after otelslog has converted the record, so every Debug record would still be built and every attribute converted before being discarded, and its `Enabled` hook is experimental in sdk/log v0.22.0. The slog-level wrapper stops the record at `Enabled`, before any conversion. Also rejected: a second level flag for OTLP — the spec's "written once to each sink" means the two sinks see the same records, and no operator asked for a split.

### D3. Enable exporters per signal (#8)

`signalEnabled(signal)` is true when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT` is set. `Init` builds the trace exporter and SDK `TracerProvider` only when traces are enabled, and the log exporter and SDK `LoggerProvider` only when logs are enabled; the other signal gets its no-op provider. `Providers.Enabled` is true when either is. `Shutdown` shuts down only what was built. A failure building the second exporter shuts down the first, as before.

Rejected: keeping the current "any endpoint enables both" rule. The exporter for the unconfigured signal falls back to the SDK default (`localhost:4317` / `:4318`), where nothing listens in a pod, so it retries forever, fills its queue and logs export errors — cost with no possible benefit.

### D4. One shutdown deadline, with a flush reserve carved out of the drain (O4)

`shutdownBudget(writeTimeout)` returns the grace period `max(writeTimeout, 10s)` — unchanged — and a flush reserve of `min(grace/4, 5s)`. `shutdown` computes ONE grace deadline; the HTTP drain runs under a child context whose deadline is exactly `reserve` earlier, and telemetry shuts down under the parent. Both therefore end by the same grace deadline, so the exporter never extends the grace period, as the spec requires. The telemetry flush still runs when the drain fails, and it still has at least `reserve` left after a drain that timed out — the abnormal case whose spans and log records matter most.

The drain never becomes shorter than `--build-timeout`: with `WriteTimeout = BuildTimeout + 5s` and `reserve <= 5s`, `grace - reserve >= BuildTimeout`; at the 10s floor (`BuildTimeout < 5s`) the drain is 7.5s. Every graph request is bounded by its own `--build-timeout` deadline, so a drain of at least that long still lets a request admitted just before the signal finish its build.

The sequence is extracted into `shutdown(parent, grace, reserve, drain, flush, logger, stderr)` in `cmd/kube-state-graph/shutdown.go` so a unit test can assert that both run under the same deadline, that the drain's deadline is earlier, and that telemetry still runs after a failed drain.

Rejected: handing the drain context itself to telemetry. A drain that hits its deadline leaves an already-expired context, the OTel SDK `Shutdown`s return immediately, and nothing is exported — exactly the regression the separate budget was introduced to avoid. Rejected: keeping the separate 10s budget. Total shutdown can then reach `grace + 10s`, past what the spec allows, and past a `terminationGracePeriodSeconds` sized to the grace period, where the kubelet's SIGKILL loses the flush anyway.

### D5. Warn, never refuse, for argv secrets (P-2, option B)

`config.Parse` records `Config.APIKeysFromFlag` with `fs.Visit` (true only when `--api-keys` appeared on the command line, not when `KSG_API_KEYS` supplied it). At startup, `warnArgvSecrets` logs one Warn when `APIKeysFromFlag` is true and `--api-keys-file` is unset (the flag is then in use), naming `--api-keys-file` / `KSG_API_KEYS` as the alternatives; and one Warn when the route-store DSN parses with a non-nil `User` carrying a password or name, naming `KSG_ROUTE_STORE_USERNAME` / `KSG_ROUTE_STORE_PASSWORD`. Neither Warn carries a key, the DSN, or a DSN parse error (a `*url.Error` quotes its whole input); an unparseable DSN warns nothing here and fails at dial as before. The DSN Warn fires whatever the source (flag or `KSG_ROUTE_STORE_DSN`) — userinfo in the DSN is never the env-only credential path — and also when the env pair overrides it, because the secret is still on the process.

Rejected: (A) staying silent — the operator has no signal that a dev-only path reached production. (C) removing `--api-keys` or rejecting DSN userinfo — a breaking change to documented, scenario-pinned behaviour (`graph-api` uses `--api-keys=k1` in its scenarios; `pod-service-graph` keeps DSN userinfo for backward compatibility) that needs its own change and `docs/BREAKING.md` entry. Also rejected: inspecting `os.Args` directly in `main` — it would re-implement flag parsing (`-api-keys`, `--api-keys=…`, `--api-keys …`) that `fs.Visit` already answers exactly.

### D6. A configured key source that yields zero keys is reported as auth DISABLED (#1)

`loadAPIKeys` branches on `ks.Empty()` after loading: a file that parses to zero keys logs a Warn "api key auth DISABLED — --api-keys-file yielded no keys" (with the path), and still arms the reload loop so that filling the file later enables auth without a restart, as today. A `--api-keys` / `KSG_API_KEYS` value that parses to zero keys (`","`) logs the same kind of Warn. The non-empty paths keep their Info lines unchanged.

Rejected: failing startup on an empty key file. The keyset's startup load documents an empty file as the auth-disabled dev state, and a Secret mounted before it is populated would crash-loop the pod; the reload path already fails closed against an empty file once a non-empty set is active.

### D7. Redact userinfo wherever an upstream URL is logged or echoed (#5)

A URL is rendered for humans through a redaction helper: `url.Parse` it; if that fails, or the result is opaque (`user:pass@host:8428` parses as scheme `user` with opaque `pass@host:8428`, which `Redacted()` would print whole), render a fixed `<unparseable url>` placeholder; otherwise render `u.Redacted()` (the password becomes `xxxxx`; the user name stays, as the SDK's own redaction does). Applied to: the three `prom_url` log attributes in `cmd/kube-state-graph`, `config.Validate`'s invalid-URL error, `Backend.String()` (startup and reload logs), and `validateBackendURL`'s errors, which now carry the inner parse error (`(*url.Error).Err`) instead of the full `*url.Error` and the redacted form instead of `%q` of the raw input. `promql.New`'s parse error gets the same treatment. `Backend.URL()` stays raw — it is the dial target and the client-cache key.

The helper is duplicated as two unexported five-line functions (`pkg/promql.redactURL`, `internal/config.RedactURL` used by `cmd`) rather than exported from `pkg/promql`: the engine's exported surface is a contract with embedders, and URL formatting for logs is not something it should promise.

Rejected: rejecting a URL with userinfo at validation. Basic auth embedded in the URL is a legitimate Prometheus-client configuration some deployments rely on; refusing it is a breaking change for a logging problem. Rejected: dropping the URL from logs entirely — the endpoint is the first thing an operator checks when a store is unreachable.

## Risks / Trade-offs

- [The drain is up to 5s shorter than before] → bounded below by `--build-timeout`, which already bounds every graph request; a request slower than its own build deadline is a 504 either way.
- [Fewer resource attributes] → a dashboard keyed on `process.command_args` or `process.executable.path` loses them. Neither is a stable identity; `service.instance.id` and `process.pid` remain.
- [No Debug in OTLP at `info`] → an operator who relied on Debug reaching OTLP while the local sink was at `info` must now set `--log-level=debug`. This is the behaviour the spec already described.
- [Inner `url.Parse` errors can still quote a fragment] (an invalid port, a bad escape) → a fragment is not a credential in the common case, and the full input is no longer echoed.

## Migration Plan

None. Every change is observable only as logs, resource attributes and exporter setup; no flag, variable or route changes. Rollback is a revert.

## Open Questions

None.
