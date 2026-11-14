# Proposal

## Why

The server's own telemetry and startup logging can carry secrets an operator supplied, and its OTLP pipeline behaves differently from what the `otlp-observability` spec describes:

- The OTLP resource is assembled with `resource.WithProcess()`, which in otel/sdk v1.46.0 includes the `process.command_args` detector. Every exported span and log record therefore carries the full argv, including `--api-keys=k1,k2`, a `--route-store-dsn` with userinfo, or a `--prom-url` with userinfo. A telemetry backend usually has a wider audience than the pod spec, and the same spec already promises that API-key values never reach the OTLP bridge.
- The slog OTLP bridge ignores `--log-level`: `fanoutHandler.Enabled` is local OR bridge, and the bridge's batch processor is always enabled, so every `slog.Debug` record (full PromQL on each cache miss, per-series service-graph Debug) is built and exported at every level. Under a heavy build the batch queue (2048) overflows and drops the Info / Warn records that matter.
- Setting only `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` also builds a log exporter that falls back to `localhost:4317`, and the reverse, so a single-signal deployment retries a collector that does not exist.
- Telemetry shutdown gets its own fresh 10s budget after the HTTP drain, so the whole shutdown can outlast the grace period the spec says it shares.
- Operational gaps around credentials: an `--api-keys-file` that parses to zero keys logs "api key auth enabled" while auth is in fact off; a literal `--api-keys` flag and a `--route-store-dsn` carrying userinfo put secrets in the process listing without any warning; `--prom-url` and routing-file `url` values are logged (and echoed in validation errors) with their userinfo.

## What Changes

- The OTLP resource carries `process.pid`, `process.executable.name`, `process.runtime.name`, `process.runtime.version` and `process.runtime.description` from the process detectors, and no longer `process.command_args`, `process.executable.path` or `process.owner`.
- The slog OTLP bridge honours the configured `--log-level`: a record below it is written to neither sink, and a record at or above it is written once to each.
- The trace and log exporters are enabled per signal: a signal's exporter is built only when `OTEL_EXPORTER_OTLP_ENDPOINT` or that signal's own `OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT` is set; the other signal keeps its no-op provider.
- Telemetry shutdown shares the server-shutdown grace deadline. The HTTP drain ends a short reserve before that deadline so the flush still has time after a drain that timed out.
- Startup Warns (no contract change, no value logged): auth disabled because the configured key source yielded zero keys; API keys passed as a literal `--api-keys` flag; a route-store DSN carrying userinfo.
- Every logged or error-echoed upstream URL (`--prom-url`, routing-file `url`) has its userinfo password redacted with `url.URL.Redacted()`; a URL that cannot be parsed is never echoed.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `otlp-observability`: per-signal exporter enablement, level-filtered OTLP bridge, and the process attributes the resource carries.
- `graph-api`: the boot warnings for auth disabled by an empty key source and for keys passed as a literal flag.
- `upstream-backend-routing`: upstream endpoint URLs are logged and echoed with userinfo redacted.
- `pod-service-graph`: a startup warning when the route-store DSN carries userinfo.

## Impact

- **Code**: `internal/telemetry` (resource detectors, per-signal providers, level filter on the bridge), `internal/observability` (level parsing shared with the bridge), `internal/config` (`APIKeysFromFlag`, URL redaction helper, redacted validation error), `cmd/kube-state-graph` (startup Warns, redacted URLs, shutdown sequence), `pkg/promql` (`Backend.String` and URL validation errors redact userinfo; `promql.New` no longer echoes a raw URL).
- **API**: none. No route, parameter, status code, body, header or self-metric changes; goldens are untouched.
- **Go API**: `telemetry.NewSlogHandler` gains a `slog.Leveler` argument (internal package). `pkg/` exported surface is unchanged.
- **Behaviour an operator sees**: fewer resource attributes on spans and log records; no Debug records in the OTLP log pipeline unless `--log-level=debug`; no log exporter when only a traces endpoint is set (and vice versa); new Warn lines at startup for the three credential situations above.
- **Docs**: `.claude/rules` unaffected; README telemetry section notes per-signal enablement and level filtering. No `docs/BREAKING.md` entry: nothing an operator configured stops working.
