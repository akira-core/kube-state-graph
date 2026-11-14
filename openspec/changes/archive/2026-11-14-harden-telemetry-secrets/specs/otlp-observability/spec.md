# Spec Delta

## MODIFIED Requirements

### Requirement: OpenTelemetry SDK initialisation from standard environment variables

The server SHALL initialise the OpenTelemetry Go SDK at startup from the OTel-standard environment variables only, SHALL NOT introduce bespoke `--otlp-*` CLI flags, and SHALL NOT read the OTLP endpoint from any non-OTel-prefixed source. Each signal SHALL be enabled on its own: the trace exporter and SDK `TracerProvider` SHALL be created only when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set, and the log exporter and SDK `LoggerProvider` only when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` is set. A signal that is not enabled SHALL keep its no-op provider and SHALL create no exporter, batcher, or background goroutine.

#### Scenario: Recognised environment variables

- **WHEN** the server reads its telemetry configuration at startup
- **THEN** it reads only `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TIMEOUT`, `OTEL_EXPORTER_OTLP_INSECURE`, `OTEL_EXPORTER_OTLP_TRACES_*`, `OTEL_EXPORTER_OTLP_LOGS_*`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER`, and `OTEL_TRACES_SAMPLER_ARG`
- **AND** no `--otlp-*` CLI flag exists and no non-OTel-prefixed source can supply the OTLP endpoint

#### Scenario: Telemetry disabled by default

- **WHEN** the server starts with `OTEL_EXPORTER_OTLP_ENDPOINT` unset and neither per-signal override (`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`) set
- **THEN** the global `TracerProvider` is `noop.NewTracerProvider()`, the slog OTLP bridge writes to a no-op logger, and no OTLP connection is opened
- **AND** a no-op `TracerProvider` and a no-op `LoggerProvider` are installed, so no exporters, batchers, or background goroutines are created

#### Scenario: Telemetry enabled by environment

- **WHEN** the server starts with `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317`, `OTEL_SERVICE_NAME=kube-state-graph`, and `OTEL_RESOURCE_ATTRIBUTES=deployment.environment=prod`
- **THEN** the SDK installs an OTLP gRPC trace exporter and an OTLP gRPC log exporter targeting `otel-collector:4317`, and the resource carries `service.name=kube-state-graph` and `deployment.environment=prod`

#### Scenario: Only the traces endpoint is set

- **WHEN** the server starts with `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` set and both `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` unset
- **THEN** the SDK installs a trace exporter and an SDK `TracerProvider`, the global `LoggerProvider` stays no-op, and no log exporter is created

#### Scenario: Only the logs endpoint is set

- **WHEN** the server starts with `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` set and both `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` unset
- **THEN** the SDK installs a log exporter and an SDK `LoggerProvider`, the global `TracerProvider` stays no-op, and no trace exporter is created

#### Scenario: HTTP/protobuf protocol selection

- **WHEN** the server starts with `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` and `OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.com:4318`
- **THEN** the SDK installs OTLP HTTP/protobuf exporters for traces and logs against the configured endpoint

#### Scenario: Sampler configuration

- **WHEN** the server starts with `OTEL_TRACES_SAMPLER=parentbased_traceidratio` and `OTEL_TRACES_SAMPLER_ARG=0.1`
- **THEN** the configured `TracerProvider` uses a parent-based trace-ID-ratio sampler with ratio `0.1`

### Requirement: Structured logging via slog OTLP bridge with trace correlation

The server SHALL build its `*slog.Logger` using `go.opentelemetry.io/contrib/bridges/otelslog`, so every `slog` record at or above the configured `--log-level` is emitted both to the configured stderr text/JSON handler (preserving the existing operator log stream) and to the OTLP logs pipeline through the global `LoggerProvider`. A record below the configured level SHALL be emitted to neither sink, and the two sinks SHALL apply the same level. When the logs signal is disabled, the bridge SHALL write only to the local stderr handler.

#### Scenario: Log record written to both sinks

- **WHEN** telemetry is enabled and the server emits a `slog` record at or above the configured level
- **THEN** the record is written once to the local stderr handler and once to the OTLP logs pipeline through the global `LoggerProvider`

#### Scenario: Records below the configured level reach neither sink

- **WHEN** the logs signal is enabled, the server runs with `--log-level=info`, and code emits `slog.Debug(...)`, including through a logger derived with `With(...)` or `WithGroup(...)`
- **THEN** no OTLP log record is emitted and nothing is written to the local stderr handler
- **AND** the logger's `Enabled` reports false for `slog.LevelDebug`

#### Scenario: Tracing disabled logs locally only

- **WHEN** `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` are both unset and the server emits a `slog` record
- **THEN** the record is written to the local stderr handler only and no OTLP log record is emitted

### Requirement: Resource attributes and service identity

The OpenTelemetry resource SHALL be assembled by merging, in this order: the detected process / runtime / host / SDK attributes, then the value of `OTEL_RESOURCE_ATTRIBUTES`, then explicit overrides for `service.name`, `service.version`, and `service.instance.id`. The process attributes SHALL be limited to `process.pid`, `process.executable.name`, `process.runtime.name`, `process.runtime.version`, and `process.runtime.description`; the resource SHALL NOT carry `process.command_args`, `process.executable.path`, or `process.owner`, because the command line can carry credentials passed as flags.

#### Scenario: Default service identity

- **WHEN** the server starts with `OTEL_EXPORTER_OTLP_ENDPOINT` set and `OTEL_SERVICE_NAME` unset
- **THEN** the exported resource carries `service.name=kube-state-graph`, `service.version=<build version>`, and a non-empty `service.instance.id`

#### Scenario: Service identity override sources

- **WHEN** the resource is assembled
- **THEN** `service.name` comes from `OTEL_SERVICE_NAME`, defaulting to `kube-state-graph`
- **AND** `service.version` is the build's compiled-in version string
- **AND** `service.instance.id` is a UUIDv4 generated once at process start

#### Scenario: User-supplied resource attributes

- **WHEN** the server starts with `OTEL_RESOURCE_ATTRIBUTES=cluster=prod-eu1,team=platform`
- **THEN** every exported span and log record carries resource attributes `cluster=prod-eu1` and `team=platform` in addition to the defaults

#### Scenario: Command-line arguments never reach the resource

- **WHEN** the server is started with `--api-keys=k1` and a `--route-store-dsn` carrying userinfo, and telemetry is enabled
- **THEN** the resource carries `process.pid` and `process.runtime.name`
- **AND** it carries no `process.command_args`, `process.executable.path`, or `process.owner` attribute, so neither the key nor the DSN appears on any exported span or log record

### Requirement: Graceful shutdown flushes pending exports

On `SIGTERM` or `SIGINT`, the server SHALL invoke `TracerProvider.Shutdown(ctx)` and `LoggerProvider.Shutdown(ctx)` after the HTTP server has stopped accepting new connections and in-flight requests have drained. The shutdown context SHALL share the existing server-shutdown grace deadline; the exporter SHALL NOT extend the grace period. The HTTP drain SHALL end a flush reserve before that deadline, so the flush keeps time to run after a drain that timed out. When shutdown of either provider returns an error, the server SHALL log the error via the local stderr handler and exit with a non-zero status code.

#### Scenario: Successful flush on SIGTERM

- **WHEN** the server receives `SIGTERM` while telemetry is enabled and a recently completed request span is still buffered
- **THEN** the buffered span and its child spans are exported to the configured OTLP endpoint before the process exits, the process exit code is `0`, and the total shutdown duration does not exceed the configured shutdown grace period

#### Scenario: Shutdown timeout does not block exit

- **WHEN** the OTLP collector is unreachable on `SIGTERM` and the shutdown grace period elapses with exports still pending
- **THEN** the process exits with a non-zero status code, the local stderr handler logs an `otlp shutdown timed out` error, and the process does not exceed the grace period waiting for the collector

#### Scenario: A timed-out drain still leaves time to flush

- **WHEN** an in-flight request outlives the HTTP drain on `SIGTERM`
- **THEN** the drain stops at the grace deadline minus the flush reserve, the providers are still shut down with the remaining time before the same grace deadline, and the drain is never shorter than `--build-timeout`
