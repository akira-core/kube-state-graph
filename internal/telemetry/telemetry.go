// Package telemetry wires the OpenTelemetry Go SDK into kube-state-graph.
//
// Configuration is OTel-standard environment variables only. Each signal is
// enabled by OTEL_EXPORTER_OTLP_ENDPOINT or its own _TRACES_ / _LOGS_
// endpoint; a signal with neither keeps a no-op provider, so telemetry is
// disabled by default with zero export overhead.
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
// Each signal is enabled on its own (signalEnabled): a signal with no endpoint
// keeps its no-op provider and builds no exporter. Otherwise the SDK would
// point that exporter at its localhost default, where nothing listens in a
// pod, and it would retry and queue for the life of the process.
//
// Returns Providers whose Shutdown closure must be invoked during graceful
// shutdown so buffered spans and log records flush within the supplied
// context's deadline. Shutdown covers only the providers Init built.
func Init(ctx context.Context, version string) (Providers, error) {
	res, err := buildResource(ctx, version)
	if err != nil {
		return Providers{Shutdown: noopShutdown}, fmt.Errorf("build resource: %w", err)
	}

	var (
		tp        trace.TracerProvider   = tracenoop.NewTracerProvider()
		lp        otellog.LoggerProvider = lognoop.NewLoggerProvider()
		shutdowns []namedShutdown
	)
	if signalEnabled("traces") {
		sdkTP, err := newTracerProvider(ctx, res)
		if err != nil {
			return Providers{Shutdown: noopShutdown}, err
		}
		tp = sdkTP
		shutdowns = append(shutdowns, namedShutdown{"tracer provider", sdkTP.Shutdown})
	}
	if signalEnabled("logs") {
		sdkLP, err := newLoggerProvider(ctx, res)
		if err != nil {
			_ = joinShutdowns(shutdowns)(ctx)
			return Providers{Shutdown: noopShutdown}, err
		}
		lp = sdkLP
		shutdowns = append(shutdowns, namedShutdown{"logger provider", sdkLP.Shutdown})
	}

	otel.SetTracerProvider(tp)
	logglobal.SetLoggerProvider(lp)
	setPropagator()

	if len(shutdowns) == 0 {
		return Providers{Tracer: tp, Logger: lp, Enabled: false, Shutdown: noopShutdown}, nil
	}
	return Providers{Tracer: tp, Logger: lp, Enabled: true, Shutdown: joinShutdowns(shutdowns)}, nil
}

// namedShutdown is one built provider's Shutdown, named for the joined error.
type namedShutdown struct {
	name string
	fn   func(context.Context) error
}

// joinShutdowns runs every Shutdown under the one context — none is skipped
// because an earlier one failed — and joins their errors.
func joinShutdowns(shutdowns []namedShutdown) func(context.Context) error {
	return func(sctx context.Context) error {
		var errs []error
		for _, s := range shutdowns {
			if err := s.fn(sctx); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
			}
		}
		return errors.Join(errs...)
	}
}

func newTracerProvider(ctx context.Context, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	exp, err := newTraceExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	), nil
}

func newLoggerProvider(ctx context.Context, res *resource.Resource) (*sdklog.LoggerProvider, error) {
	exp, err := newLogExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("log exporter: %w", err)
	}
	return sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
		sdklog.WithResource(res),
	), nil
}

// signalEnabled reports whether the given signal ("traces" or "logs") has an
// endpoint: the shared OTEL_EXPORTER_OTLP_ENDPOINT or its own
// OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT.
func signalEnabled(signal string) bool {
	return env("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		env("OTEL_EXPORTER_OTLP_"+strings.ToUpper(signal)+"_ENDPOINT") != ""
}

// buildResource combines detected SDK / process / host attributes with
// OTEL_RESOURCE_ATTRIBUTES and the explicit service identity overrides.
//
// The process detectors are named one by one rather than through
// resource.WithProcess(): that aggregate also detects process.command_args,
// which carries argv — and argv can hold --api-keys or a DSN with userinfo,
// which would then ride on every exported span and log record. The
// executable path and the OS owner are left out with it. Naming the detectors
// is an allowlist, so a detector a later SDK adds to WithProcess() cannot slip
// in unnoticed.
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
		resource.WithProcessPID(),
		resource.WithProcessExecutableName(),
		resource.WithProcessRuntimeName(),
		resource.WithProcessRuntimeVersion(),
		resource.WithProcessRuntimeDescription(),
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
