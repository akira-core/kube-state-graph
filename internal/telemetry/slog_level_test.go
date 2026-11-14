package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// recordingLogExporter keeps the bodies of every OTLP log record it is handed,
// so a test can count what the bridge emitted per level.
type recordingLogExporter struct {
	mu      sync.Mutex
	records []string
}

func (r *recordingLogExporter) Export(_ context.Context, recs []sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range recs {
		r.records = append(r.records, rec.Body().AsString())
	}
	return nil
}

func (r *recordingLogExporter) Shutdown(context.Context) error   { return nil }
func (r *recordingLogExporter) ForceFlush(context.Context) error { return nil }

func (r *recordingLogExporter) bodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.records...)
}

// installRecordingLogs installs an SDK LoggerProvider whose simple processor
// exports synchronously to a recorder, restoring the previous global after the
// test. The bridge resolves the global provider when NewSlogHandler builds it.
func installRecordingLogs(t *testing.T) *recordingLogExporter {
	t.Helper()
	exp := &recordingLogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	prev := logglobal.GetLoggerProvider()
	logglobal.SetLoggerProvider(lp)
	t.Cleanup(func() {
		logglobal.SetLoggerProvider(prev)
		_ = lp.Shutdown(context.Background())
	})
	return exp
}

func localLines(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var msgs []string
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var line map[string]any
		require.NoError(t, dec.Decode(&line))
		msgs = append(msgs, line["msg"].(string))
	}
	return msgs
}

// At --log-level=info a Debug record reaches NEITHER sink and an Info record
// reaches each exactly once — also through loggers derived with With and
// WithGroup, whose handlers must keep the filter.
func TestSlogHandler_OTLPBridgeHonoursLevel(t *testing.T) {
	exp := installRecordingLogs(t)

	var buf bytes.Buffer
	local := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewSlogHandler(local, slog.LevelInfo))

	assert.False(t, logger.Enabled(t.Context(), slog.LevelDebug),
		"Enabled must be false for Debug at info, so callers can skip building Debug args")
	assert.True(t, logger.Enabled(t.Context(), slog.LevelInfo))

	logger.Debug("debug-root")
	logger.With("k", "v").Debug("debug-with")
	logger.WithGroup("g").Debug("debug-group")
	logger.Info("info-root")
	logger.With("k", "v").Info("info-with")
	logger.WithGroup("g").Warn("warn-group")

	want := []string{"info-root", "info-with", "warn-group"}
	assert.Equal(t, want, exp.bodies(), "OTLP receives each record at or above the level exactly once")
	assert.Equal(t, want, localLines(t, &buf), "the local sink sees the same records")
}

// At --log-level=debug the Debug record reaches the OTLP pipeline once.
func TestSlogHandler_OTLPBridgeDebugLevel(t *testing.T) {
	exp := installRecordingLogs(t)

	var buf bytes.Buffer
	local := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewSlogHandler(local, slog.LevelDebug))

	logger.Debug("debug-root")
	logger.With("k", "v").Debug("debug-with")

	assert.Equal(t, []string{"debug-root", "debug-with"}, exp.bodies())
	assert.Equal(t, []string{"debug-root", "debug-with"}, localLines(t, &buf))
}

// A nil Leveler means Info, the documented --log-level default, rather than a
// panic or an unfiltered bridge.
func TestSlogHandler_NilLevelerDefaultsToInfo(t *testing.T) {
	exp := installRecordingLogs(t)

	var buf bytes.Buffer
	local := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewSlogHandler(local, nil))

	logger.Debug("debug-root")
	logger.Info("info-root")

	assert.Equal(t, []string{"info-root"}, exp.bodies())
}
