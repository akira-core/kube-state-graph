package telemetry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	lognoop "go.opentelemetry.io/otel/log/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// restoreGlobals puts back the global providers Init replaces.
func restoreGlobals(t *testing.T) {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevLP := otel.GetLoggerProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetLoggerProvider(prevLP)
	})
}

func shutdownNow(t *testing.T, p Providers) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = p.Shutdown(ctx)
	})
}

// Each signal is enabled by the shared endpoint or by its OWN endpoint only. A
// signal with no endpoint keeps its no-op provider — otherwise its exporter
// falls back to localhost:4317 / :4318, where nothing listens in a pod.
func TestInit_EnablesSignalsIndependently(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        map[string]string
		wantTraces bool
		wantLogs   bool
	}{
		{
			name:       "traces endpoint only",
			env:        map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:65535"},
			wantTraces: true,
		},
		{
			name:     "logs endpoint only",
			env:      map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:65535"},
			wantLogs: true,
		},
		{
			name:       "shared endpoint",
			env:        map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:65535"},
			wantTraces: true,
			wantLogs:   true,
		},
		{
			name: "shared endpoint plus a per-signal override",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT":        "http://127.0.0.1:65535",
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:65534",
			},
			wantTraces: true,
			wantLogs:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearOTELEnv(t)
			restoreGlobals(t)
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			p, err := Init(t.Context(), "test")
			require.NoError(t, err)
			shutdownNow(t, p)

			assert.True(t, p.Enabled, "Enabled is true when either signal is on")

			_, sdkTP := otel.GetTracerProvider().(*sdktrace.TracerProvider)
			_, noopTP := otel.GetTracerProvider().(tracenoop.TracerProvider)
			assert.Equal(t, tc.wantTraces, sdkTP, "global tracer provider is the SDK one iff traces are enabled")
			assert.Equal(t, !tc.wantTraces, noopTP, "global tracer provider is no-op iff traces are disabled")

			_, sdkLP := otel.GetLoggerProvider().(*sdklog.LoggerProvider)
			_, noopLP := otel.GetLoggerProvider().(lognoop.LoggerProvider)
			assert.Equal(t, tc.wantLogs, sdkLP, "global logger provider is the SDK one iff logs are enabled")
			assert.Equal(t, !tc.wantLogs, noopLP, "global logger provider is no-op iff logs are disabled")

			_, retTP := p.Tracer.(*sdktrace.TracerProvider)
			_, retLP := p.Logger.(*sdklog.LoggerProvider)
			assert.Equal(t, tc.wantTraces, retTP, "Providers.Tracer matches the global")
			assert.Equal(t, tc.wantLogs, retLP, "Providers.Logger matches the global")
		})
	}
}
