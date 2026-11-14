package observability

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"Warn":    slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"verbose": slog.LevelInfo,
	} {
		assert.Equal(t, want, ParseLevel(in), "ParseLevel(%q)", in)
	}
}

// NewLogHandler must apply exactly the level ParseLevel returns — the OTLP
// bridge filters by ParseLevel, so a divergence would split the two sinks.
func TestNewLogHandler_UsesParseLevel(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error"} {
		h := NewLogHandler(lvl)
		floor := ParseLevel(lvl)
		assert.True(t, h.Enabled(t.Context(), floor), "%s: enabled at its own level", lvl)
		assert.False(t, h.Enabled(t.Context(), floor-1), "%s: disabled below its level", lvl)
	}
}
