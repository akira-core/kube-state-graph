package observability

import (
	"log/slog"
	"os"
	"strings"
)

// NewLogger returns a slog.Logger backed by a JSON handler writing to stdout
// at the requested level (debug | info | warn | error).
func NewLogger(level string) *slog.Logger {
	return slog.New(NewLogHandler(level))
}

// NewLogHandler returns the JSON handler used by NewLogger so callers (e.g.
// the telemetry package) can wrap it in additional handlers before installing
// the global logger.
func NewLogHandler(level string) slog.Handler {
	return slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: ParseLevel(level)})
}

// ParseLevel maps a configured --log-level (debug | info | warn | error, any
// case) to its slog.Level; anything else is Info. It is the ONE parser: the
// local handler and the OTLP bridge's level filter both read it, so the two
// sinks cannot disagree about which records exist.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
