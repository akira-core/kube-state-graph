# Tasks

## 1. OTLP resource (C2)

- [ ] 1.1 Write a failing `internal/telemetry` test: `buildResource` carries `process.pid` and `process.runtime.name` and no `process.command_args`, `process.executable.path` or `process.owner` key. Verify RED.
- [ ] 1.2 Replace `resource.WithProcess()` with the five named detectors (design D1). Verify 1.1 passes.

## 2. Level-filtered OTLP bridge (C4)

- [ ] 2.1 Extract `observability.ParseLevel` from `NewLogHandler`, with a table test for the four levels and the `info` fallback.
- [ ] 2.2 Write failing tests with a recording `sdklog` exporter behind a simple processor: at `info`, `slog.Debug` produces no OTLP record and `slog.Info` produces exactly one, also through `logger.With(...)` and `WithGroup(...)`; the logger's `Enabled` is false for Debug; at `debug`, Debug reaches OTLP once. Verify RED.
- [ ] 2.3 Add the `levelFilter` wrapper and the `slog.Leveler` argument to `NewSlogHandler`; pass `observability.ParseLevel(cfg.LogLevel)` from `main` (design D2). Verify 2.2 passes.

## 3. Per-signal exporters (#8)

- [ ] 3.1 Write failing tests: only `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` set → SDK tracer provider, no-op logger provider; only `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` set → the reverse; the shared endpoint → both; `Providers.Enabled` true in all three. Verify RED.
- [ ] 3.2 Build each signal's exporter and provider only when that signal is enabled; `Shutdown` covers only what was built (design D3). Verify 3.1 passes.

## 4. Shutdown deadline (O4)

- [ ] 4.1 Write failing `cmd/kube-state-graph` tests for `shutdownBudget` (grace unchanged, reserve `min(grace/4, 5s)`, drain never shorter than `--build-timeout`) and for `shutdown`: telemetry runs under the same deadline as the outer grace context, the drain's deadline is earlier by the reserve, and telemetry still runs and keeps time after a drain that failed. Verify RED.
- [ ] 4.2 Extract the shutdown sequence from `run` into `shutdown` and replace the separate 10s telemetry budget (design D4). Verify 4.1 passes.

## 5. Startup credential warnings (P-2, #1)

- [ ] 5.1 Write failing `internal/config` tests: `Config.APIKeysFromFlag` is true only when `--api-keys` appears on the command line, false for `KSG_API_KEYS`.
- [ ] 5.2 Write failing `cmd/kube-state-graph` tests: an empty / comment-only key file and a key list of only separators log a Warn saying auth is disabled and no "auth enabled" line; a populated file keeps its Info line; a literal `--api-keys` logs one Warn not containing the key; `KSG_API_KEYS` logs none; a DSN with userinfo logs one Warn not containing the password; a DSN without userinfo or an unparseable DSN logs none. Verify RED.
- [ ] 5.3 Add `APIKeysFromFlag` (set via `fs.Visit`), the zero-key Warn in `loadAPIKeys`, and `warnArgvSecrets` called from `run` (design D5, D6). Verify 5.1 and 5.2 pass.

## 6. URL redaction (#5)

- [ ] 6.1 Write failing tests: `config.RedactURL` redacts a password, passes a credential-free URL through, and renders a placeholder for an unparseable or opaque input; `config.Validate`'s invalid prom-url error contains no password; `promql.Backend.String()` and `Table.String()` render `xxxxx` for a URL password; `NewTable` errors for an invalid URL with userinfo contain no password; the startup log for a `--prom-url` with userinfo contains no password.
- [ ] 6.2 Implement the redaction in `internal/config`, `pkg/promql` (unexported helper) and the three `prom_url` log attributes in `cmd/kube-state-graph` (design D7). Verify 6.1 passes and `GOTOOLCHAIN=go1.27.1 go build ./pkg/...` succeeds.

## 7. Verification and docs

- [ ] 7.1 README: note per-signal OTLP enablement and that the OTLP log pipeline follows `--log-level`; note the boot Warn on the `--api-keys` row.
- [ ] 7.2 Run `go build ./... && go vet ./...`, `gofmt -l .`, and `go test -race -count=1` over `./internal/telemetry/... ./internal/observability/... ./internal/config/... ./cmd/... ./pkg/promql/...`; `openspec validate harden-telemetry-secrets`.
