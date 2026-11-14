package route

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFlightMemo_MemoisesSuccess(t *testing.T) {
	var m flightMemo[string, int]
	var calls atomic.Int64
	fn := func(context.Context) (int, error) { calls.Add(1); return 7, nil }

	for range 3 {
		v, err := m.do(t.Context(), "k", fn)
		require.NoError(t, err)
		assert.Equal(t, 7, v)
	}
	assert.Equal(t, int64(1), calls.Load())

	_, _ = m.do(t.Context(), "other", fn)
	assert.Equal(t, int64(2), calls.Load(), "keys are independent")
}

// Concurrent callers share ONE execution: the leader is held until every
// caller has arrived, so the count cannot be 1 by accident of scheduling.
func TestFlightMemo_CoalescesConcurrentCallers(t *testing.T) {
	var m flightMemo[string, int]
	var calls atomic.Int64
	release := make(chan struct{})
	fn := func(context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 42, nil
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]int, n)
	for i := range n {
		wg.Go(func() {
			v, err := m.do(t.Context(), "k", fn)
			assert.NoError(t, err)
			results[i] = v
		})
	}
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	time.Sleep(10 * time.Millisecond) // let the other callers reach the flight
	close(release)
	wg.Wait()

	assert.Equal(t, int64(1), calls.Load())
	for _, v := range results {
		assert.Equal(t, 42, v)
	}
}

func TestFlightMemo_ErrorsAreNotMemoised(t *testing.T) {
	var m flightMemo[string, int]
	boom := errors.New("store unreachable")
	var calls atomic.Int64
	fn := func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			return 0, boom
		}
		return 3, nil
	}

	_, err := m.do(t.Context(), "k", fn)
	require.ErrorIs(t, err, boom)
	v, err := m.do(t.Context(), "k", fn)
	require.NoError(t, err)
	assert.Equal(t, 3, v)
	assert.Equal(t, int64(2), calls.Load())
}

// A leader whose OWN context ends hands nothing to a waiter whose context is
// still live: the waiter runs the work itself and succeeds.
func TestFlightMemo_WaiterRetriesAfterTheLeadersContextEnds(t *testing.T) {
	var m flightMemo[string, int]
	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	// A send that gives up when nobody is receiving yet would be lost whenever
	// the leader reaches it before the test reaches <-entered, and the test
	// would then wait forever. Closing under a Once cannot be missed.
	entered := make(chan struct{})
	var enteredOnce sync.Once
	fn := func(ctx context.Context) (int, error) {
		enteredOnce.Do(func() { close(entered) })
		<-ctx.Done()
		return 0, context.Cause(ctx)
	}

	leaderErr := make(chan error, 1)
	go func() {
		_, err := m.do(leaderCtx, "k", fn)
		leaderErr <- err
	}()
	<-entered

	waiterDone := make(chan struct{})
	var got int
	var waiterErr error
	go func() {
		defer close(waiterDone)
		got, waiterErr = m.do(t.Context(), "k", func(context.Context) (int, error) { return 9, nil })
	}()
	time.Sleep(10 * time.Millisecond) // the waiter is now parked on the leader's flight
	cancelLeader()

	require.ErrorIs(t, <-leaderErr, context.Canceled)
	<-waiterDone
	require.NoError(t, waiterErr, "the leader's cancellation must not reach the waiter")
	assert.Equal(t, 9, got)
}

// A waiter whose own context ends stops waiting with its own cause.
func TestFlightMemo_WaiterHonoursItsOwnContext(t *testing.T) {
	var m flightMemo[string, int]
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	go func() {
		_, _ = m.do(t.Context(), "k", func(context.Context) (int, error) {
			close(started)
			<-release
			return 1, nil
		})
	}()
	<-started

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err := m.do(ctx, "k", func(context.Context) (int, error) { return 2, nil })
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// A panicking leader must release its waiters (they retry) instead of leaving
// them parked forever; the panic continues on the leader's own goroutine.
func TestFlightMemo_LeaderPanicReleasesWaiters(t *testing.T) {
	var m flightMemo[string, int]
	entered := make(chan struct{})
	release := make(chan struct{})

	go func() {
		defer func() { _ = recover() }()
		_, _ = m.do(t.Context(), "k", func(context.Context) (int, error) {
			close(entered)
			<-release
			panic("istiod blew up")
		})
	}()
	<-entered

	waiterDone := make(chan struct{})
	var got int
	go func() {
		defer close(waiterDone)
		got, _ = m.do(t.Context(), "k", func(context.Context) (int, error) { return 5, nil })
	}()
	time.Sleep(10 * time.Millisecond)
	close(release)

	select {
	case <-waiterDone:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter still blocked after the leader panicked")
	}
	assert.Equal(t, 5, got)
}
