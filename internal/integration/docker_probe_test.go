package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// recordingTB records which of Skipf / Fatalf dockerUnavailable called, so the
// decision can be asserted without skipping or failing the real test.
type recordingTB struct {
	skipped, fatal string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Skipf(format string, args ...any) { r.skipped = fmt.Sprintf(format, args...) }

func (r *recordingTB) Fatalf(format string, args ...any) { r.fatal = fmt.Sprintf(format, args...) }

// These need no container: they pin the skip-or-fail decision of
// SkipIfDockerUnavailable, which runs before any container starts.
func TestDockerUnavailable_SkipsOutsideCIAndFailsInCI(t *testing.T) {
	t.Parallel()

	const reason = "docker daemon unreachable: exit status 1"
	tests := map[string]struct {
		inCI      bool
		wantSkip  bool
		wantFatal bool
	}{
		"developer machine skips": {inCI: false, wantSkip: true},
		"CI fails":                {inCI: true, wantFatal: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var rec recordingTB
			dockerUnavailable(&rec, tc.inCI, reason)

			assert.Equal(t, tc.wantSkip, rec.skipped != "", "skipped: %q", rec.skipped)
			assert.Equal(t, tc.wantFatal, rec.fatal != "", "fatal: %q", rec.fatal)
			assert.Contains(t, rec.skipped+rec.fatal, reason)
		})
	}
}

func TestDockerProbeFailure_NamesATimeoutSeparately(t *testing.T) {
	t.Parallel()

	runErr := errors.New("signal: killed")

	timedOut := dockerProbeFailure(context.DeadlineExceeded, runErr)
	assert.Contains(t, timedOut, "did not answer within "+dockerInfoTimeout.String())
	assert.NotContains(t, timedOut, "unreachable")

	refused := dockerProbeFailure(nil, errors.New("exit status 1"))
	assert.Contains(t, refused, "docker daemon unreachable: exit status 1")
	assert.NotContains(t, refused, "did not answer")
}
