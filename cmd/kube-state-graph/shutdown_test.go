package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShutdownBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		buildTimeout time.Duration
		wantGrace    time.Duration
		wantReserve  time.Duration
	}{
		{"default build timeout", 15 * time.Second, 20 * time.Second, 5 * time.Second},
		{"floored grace", 2 * time.Second, 10 * time.Second, 2500 * time.Millisecond},
		{"long build timeout", 60 * time.Second, 65 * time.Second, 5 * time.Second},
		{"build timeout just under the floor", 4 * time.Second, 10 * time.Second, 2500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// main.go's http.Server WriteTimeout formula.
			writeTimeout := tc.buildTimeout + 5*time.Second
			grace, reserve := shutdownBudget(writeTimeout)
			assert.Equal(t, tc.wantGrace, grace, "grace period is unchanged: max(WriteTimeout, 10s)")
			assert.Equal(t, tc.wantReserve, reserve)
			assert.GreaterOrEqual(t, grace-reserve, tc.buildTimeout,
				"the drain must never be shorter than --build-timeout")
		})
	}
}

type ctxObservation struct {
	deadline  time.Time
	errAtCall error
}

func observe(ctx context.Context) ctxObservation {
	d, _ := ctx.Deadline()
	return ctxObservation{deadline: d, errAtCall: ctx.Err()}
}

// The drain and the flush share ONE grace deadline: the flush's deadline IS
// the grace deadline, and the drain's is the reserve earlier.
func TestShutdown_SharesTheGraceDeadline(t *testing.T) {
	const grace, reserve = 10 * time.Second, 2 * time.Second
	var drained, flushed ctxObservation

	start := time.Now()
	err := shutdown(t.Context(), grace, reserve,
		func(ctx context.Context) error { drained = observe(ctx); return nil },
		func(ctx context.Context) error { flushed = observe(ctx); return nil },
		quietLogger(), &bytes.Buffer{})
	require.NoError(t, err)

	require.False(t, flushed.deadline.IsZero(), "the flush must run under a deadline")
	assert.WithinDuration(t, start.Add(grace), flushed.deadline, time.Second,
		"the flush deadline is the grace deadline — not a fresh budget after the drain")
	assert.Equal(t, reserve, flushed.deadline.Sub(drained.deadline),
		"the drain ends exactly the flush reserve before the shared deadline")
}

// A drain that runs out its time still leaves the flush a live context with
// the reserve remaining, and both errors are reported.
func TestShutdown_FlushRunsAfterATimedOutDrain(t *testing.T) {
	const grace, reserve = 300 * time.Millisecond, 150 * time.Millisecond
	var flushed ctxObservation
	var remaining time.Duration
	var stderr bytes.Buffer

	err := shutdown(t.Context(), grace, reserve,
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		func(ctx context.Context) error {
			flushed = observe(ctx)
			remaining = time.Until(flushed.deadline)
			return errors.New("collector unreachable")
		},
		quietLogger(), &stderr)

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the drain error is kept")
	require.ErrorContains(t, err, "otlp shutdown: collector unreachable", "and the flush error beside it")
	require.NoError(t, flushed.errAtCall, "the flush must not be handed an expired context")
	assert.Greater(t, remaining, reserve/2, "the flush keeps (most of) the reserve")
	assert.Contains(t, stderr.String(), "otlp shutdown timed out")
}

// The whole sequence ends by the grace deadline even when the flush blocks.
func TestShutdown_NeverOutlivesTheGracePeriod(t *testing.T) {
	const grace, reserve = 200 * time.Millisecond, 100 * time.Millisecond

	start := time.Now()
	err := shutdown(t.Context(), grace, reserve,
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		quietLogger(), &bytes.Buffer{})

	require.Error(t, err)
	assert.Less(t, time.Since(start), grace+500*time.Millisecond,
		"the exporter must not extend the grace period")
}
