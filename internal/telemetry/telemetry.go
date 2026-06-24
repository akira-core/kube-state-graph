// Package telemetry wires the OpenTelemetry Go SDK into kube-state-graph.
//
// Configuration is OTel-standard environment variables only. When
// OTEL_EXPORTER_OTLP_ENDPOINT (and the per-signal _TRACES_/_LOGS_ overrides)
// are unset the package installs no-op tracer and logger providers so
// telemetry is disabled by default with zero export overhead.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Providers carries the configured trace and log providers plus a Shutdown
// closure that flushes both within the supplied context's deadline.
type Providers struct {
	Tracer   trace.TracerProvider
	Logger   otellog.LoggerProvider
	Enabled  bool
	Shutdown func(context.Context) error
}

// ServiceName is the default service.name resource attribute. OTEL_SERVICE_NAME
// overrides this when set.
const ServiceName = "kube-state-graph"

// noopShutdown is the Shutdown used whenever there is nothing to flush — the
// disabled path and every Init error path — so callers may invoke
// Providers.Shutdown unconditionally without a nil-func panic, even when they
// treated the Init error as non-fatal.
func noopShutdown(context.Context) error { return nil }

// Init configures the OpenTelemetry global TracerProvider, LoggerProvider, and
// text-map propagator. Reads only OTel-standard environment variables.
//
// Returns Providers whose Shutdown closure must be invoked during graceful
// shutdown so buffered spans and log records flush within the supplied
// context's deadline.
func Init(ctx context.Context, version string) (Providers, error) {
	enabled := exporterEnabled()
	res, err := buildResource(ctx, version)
	if err != nil {
		return Providers{Shutdown: noopShutdown}, fmt.Errorf("build resource: %w", err)
	}

	if !enabled {
		tp := tracenoop.NewTracerProvider()
		lp := lognoop.NewLoggerProvider()
		otel.SetTracerProvider(tp)
		logglobal.SetLoggerProvider(lp)
		setPropagator()
		return Providers{
			Tracer:   tp,
			Logger:   lp,
			Enabled:  false,
			Shutdown: noopShutdown,
		}, nil
	}

	traceExporter, err := newTraceExporter(ctx)
	if err != nil {
		return Providers{Shutdown: noopShutdown}, fmt.Errorf("trace exporter: %w", err)
	}
	logExporter, err := newLogExporter(ctx)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		return Providers{Shutdown: noopShutdown}, fmt.Errorf("log exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	logglobal.SetLoggerProvider(lp)
	setPropagator()

	shutdown := func(sctx context.Context) error {
		var errs []error
		if err := tp.Shutdown(sctx); err != nil {
			errs = append(errs, fmt.Errorf("tracer provider: %w", err))
		}
		if err := lp.Shutdown(sctx); err != nil {
			errs = append(errs, fmt.Errorf("logger provider: %w", err))
		}
		return errors.Join(errs...)
	}
	return Providers{Tracer: tp, Logger: lp, Enabled: true, Shutdown: shutdown}, nil
}

// exporterEnabled reports whether at least one OTLP endpoint env var is set.
func exporterEnabled() bool {
	return env("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		env("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" ||
		env("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") != ""
}

// buildResource combines detected SDK / process / host attributes with
// OTEL_RESOURCE_ATTRIBUTES and the explicit service identity overrides.
func buildResource(ctx context.Context, version string) (*resource.Resource, error) {
	serviceName := env("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = ServiceName
	}
	overrides := resource.NewSchemaless(
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
		semconv.ServiceInstanceID(uuid.NewString()),
	)
	detected, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		// resource.New is best-effort; partial failures still yield a usable
		// resource but the error reports detector issues. Surface to caller.
		return nil, err
	}
	merged, err := resource.Merge(detected, overrides)
	if err != nil {
		return nil, err
	}
	return merged, nil
}

// setPropagator installs the W3C Trace Context + Baggage composite propagator.
func setPropagator() {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}

// newTraceExporter selects gRPC or HTTP/protobuf based on
// OTEL_EXPORTER_OTLP_PROTOCOL (with per-signal _TRACES_ override). Default
// protocol is grpc per the OTel spec.
func newTraceExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	switch protocol("traces") {
	case "http/protobuf", "http/json":
		return otlptracehttp.New(ctx)
	default:
		return otlptracegrpc.New(ctx)
	}
}

// newLogExporter mirrors newTraceExporter for the log signal.
func newLogExporter(ctx context.Context) (sdklog.Exporter, error) {
	switch protocol("logs") {
	case "http/protobuf", "http/json":
		return otlploghttp.New(ctx)
	default:
		return otlploggrpc.New(ctx)
	}
}

// protocol returns the configured OTLP protocol for the supplied signal,
// falling back to the unscoped OTEL_EXPORTER_OTLP_PROTOCOL.
func protocol(signal string) string {
	if v := env("OTEL_EXPORTER_OTLP_" + strings.ToUpper(signal) + "_PROTOCOL"); v != "" {
		return strings.ToLower(v)
	}
	return strings.ToLower(env("OTEL_EXPORTER_OTLP_PROTOCOL"))
}

// env reads an environment variable, trimming whitespace.
func env(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}
