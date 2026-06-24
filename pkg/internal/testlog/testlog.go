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
func Capture(t *testing.T) *Buffer {
	t.Helper()
	buf := &Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}
