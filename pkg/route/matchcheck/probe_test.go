//go:build unix

package matchcheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeScript writes an executable shell script into a fresh temp dir and
// returns its path. The stub stands in for router_check_tool.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "router_check_tool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeVersion(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "exit_zero_is_executable", body: "echo 'router_check_tool 1.34'; exit 0"},
		{name: "non_zero_exit_fails", body: "echo boom >&2; exit 3", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := probeVersion(t.Context(), writeScript(t, c.body))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

// A binary that never answers --version must fail the probe once ctx expires,
// not block startup forever. `exec sleep` keeps the stub a single process so
// the kill reaches the one holding the output pipe; WaitDelay covers the case
// where it does not.
func TestProbeVersion_HungBinaryHonoursContext(t *testing.T) {
	bin := writeScript(t, "exec sleep 30")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := probeVersion(ctx, bin)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("probe of a hung binary succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("probe took %v; the context deadline was not honoured", elapsed)
	}
}

func TestNewRunner_RejectsFailingBinary(t *testing.T) {
	if _, err := NewRunner(writeScript(t, "exit 1")); err == nil {
		t.Fatal("NewRunner accepted a binary whose --version exits non-zero")
	}
	r, err := NewRunner(writeScript(t, "exit 0"))
	if err != nil {
		t.Fatalf("NewRunner rejected a working binary: %v", err)
	}
	if r.bin == "" {
		t.Error("Runner bound no binary")
	}
}
