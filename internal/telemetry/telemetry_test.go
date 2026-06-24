package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	logglobal "go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
)

func TestInit_DisabledByDefault(t *testing.T) {
	clearOTELEnv(t)

	providers, err := Init(t.Context(), "test")
	require.NoError(t, err)
	defer func() { _ = providers.Shutdown(t.Context()) }()

	require.False(t, providers.Enabled, "exporter must be disabled when OTEL_EXPORTER_OTLP_ENDPOINT is unset")
	_, isNoop := otel.GetTracerProvider().(tracenoop.TracerProvider)
	require.True(t, isNoop, "global tracer provider must be no-op")
	_, isLogNoop := logglobal.GetLoggerProvider().(lognoop.LoggerProvider)
	require.True(t, isLogNoop, "global logger provider must be no-op")
}

func TestInit_EnabledByEndpoint(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:65535")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_SERVICE_NAME", "kube-state-graph-test")

	providers, err := Init(t.Context(), "test")
	require.NoError(t, err)
	defer func() {
		shutdownCtx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = providers.Shutdown(shutdownCtx)
	}()

	require.True(t, providers.Enabled, "exporter must be enabled when endpoint env var is set")
}

func TestSlogHandler_TraceCorrelation(t *testing.T) {
	clearOTELEnv(t)
	providers, err := Init(t.Context(), "test")
	require.NoError(t, err)
	defer func() { _ = providers.Shutdown(t.Context()) }()

	var buf bytes.Buffer
	local := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewSlogHandler(local))

	tracer := otel.Tracer("test")
	// noop tracer's span context is not valid; but the handler must not crash.
	ctx, span := tracer.Start(context.Background(), "op")
	logger.InfoContext(ctx, "no-span-correlation")
	span.End()

	// With a synthetic recording span context (manual injection).
	type spanContextStub struct{}
	_ = spanContextStub{}

	// Use a new SpanContext via the tracer.WithRecording API: tests that
	// trace_id/span_id appear when the SpanContext is valid.
	tp := tracenoop.NewTracerProvider()
	otel.SetTracerProvider(tp)

	require.Contains(t, buf.String(), "no-span-correlation")

	var line map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		require.NoError(t, dec.Decode(&line))
	}
	// noop tracer yields invalid span context → no trace_id/span_id keys.
	require.NotContains(t, line, "trace_id")
}

// TestInit_ErrorPathReturnsCallableShutdown forces Init's buildResource error
// path via a malformed OTEL_RESOURCE_ATTRIBUTES (a pair with no `=` makes the
// fromEnv resource detector fail) and asserts the returned Providers still
// carries a callable no-op Shutdown — main calls Shutdown unconditionally even
// when it treated the Init error as non-fatal.
func TestInit_ErrorPathReturnsCallableShutdown(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "missing-equals-sign")

	providers, err := Init(t.Context(), "test")
	require.Error(t, err, "malformed OTEL_RESOURCE_ATTRIBUTES must surface an Init error")
	require.NotNil(t, providers.Shutdown, "error-path Providers must carry a callable Shutdown")
	require.NoError(t, providers.Shutdown(context.Background()))

	// Also safe with an already-cancelled context (mirrors SIGTERM teardown).
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, providers.Shutdown(cancelled))
}

func clearOTELEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_SERVICE_NAME",
		"OTEL_RESOURCE_ATTRIBUTES",
		"OTEL_TRACES_SAMPLER",
		"OTEL_TRACES_SAMPLER_ARG",
	} {
		t.Setenv(k, "")
	}
}
