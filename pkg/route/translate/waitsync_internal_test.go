package translate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitSynced must stop when its caller gives up rather than spinning until
// syncTimeout: a cancelled build would otherwise keep a translation (and its
// engine slot) busy for up to five seconds.
func TestWaitSynced_StopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := waitSynced(ctx, func() bool { return false })

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "the context's cause is returned: %v", err)
	assert.Less(t, time.Since(start), time.Second, "returns on the context, not on syncTimeout")
}

// The wait backs off instead of polling every 100µs: over ~50ms a fixed
// 100µs poll checks ~500 times, the 2ms-capped backoff a few dozen.
func TestWaitSynced_BacksOff(t *testing.T) {
	var polls atomic.Int64
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_ = waitSynced(ctx, func() bool { polls.Add(1); return false })

	assert.Less(t, polls.Load(), int64(100), "polls must back off, got %d", polls.Load())
	assert.Greater(t, polls.Load(), int64(1))
}

// A synced world returns at once, without waiting a single poll interval.
func TestWaitSynced_ReturnsWhenSynced(t *testing.T) {
	var polls atomic.Int64
	ready := func() bool { return polls.Add(1) >= 3 }

	require.NoError(t, waitSynced(t.Context(), ready))
	assert.Equal(t, int64(3), polls.Load())
}

// An already-ended context never builds a world.
func TestTranslate_EndedContextReturnsItsCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := NewTranslator().Translate(ctx, ScopedInput{})
	assert.ErrorIs(t, err, context.Canceled)
}
