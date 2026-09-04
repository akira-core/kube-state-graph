// Package testlog provides the shared slog-capture helper for pkg/ unit
// tests that assert on global-logger output (the engine's warn paths log via
// the process-wide slog default; see the D32 note in CLAUDE.md). Living under
// pkg/internal it is importable by every pkg/... test file while remaining
// invisible to external embedders.
package testlog

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"
)

// Buffer is a goroutine-safe writer for capturing log output.
type Buffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Capture swaps the default slog logger for the test's duration and returns
// the buffer collecting its output. Tests using it must not run in parallel —
// the default logger is process-global state.
//
// The captured logger runs at the handler's DEFAULT level (Info), so a Debug
// record is dropped. That is deliberate: a test asserting that something is
// logged at Debug rather than Warn wants both halves of the claim, and
// CaptureLevel(t, slog.LevelDebug) supplies the level-aware form.
func Capture(t *testing.T) *Buffer {
	t.Helper()
	return CaptureLevel(t, slog.LevelInfo)
}

// CaptureLevel is Capture with an explicit minimum level, so a test can assert
// on records the default Info threshold would drop. Pass slog.LevelDebug to
// see Debug records; the captured text still carries each record's own level,
// so a test can distinguish "logged at Debug" from "logged at Warn" rather
// than merely "logged".
func CaptureLevel(t *testing.T, level slog.Level) *Buffer {
	t.Helper()
	buf := &Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}
