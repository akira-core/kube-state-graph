package promql

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateBackend blocks every call until release is closed (or its ctx ends) and
// records the peak number of concurrent calls.
type gateBackend struct {
	release  chan struct{}
	started  chan struct{}
	calls    atomic.Int64
	inflight atomic.Int64
	peak     atomic.Int64
	vec      model.Vector
	err      error
}

func newGate() *gateBackend {
	return &gateBackend{release: make(chan struct{}), started: make(chan struct{}, 1024)}
}

func (g *gateBackend) Instant(ctx context.Context, _, _ string, _ time.Time) (model.Vector, error) {
	g.calls.Add(1)
	n := g.inflight.Add(1)
	defer g.inflight.Add(-1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	g.started <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if g.err != nil {
		return nil, g.err
	}
	return g.vec, nil
}

var ts0 = time.Unix(1_700_000_000, 0)

func TestGuard_CoalescesConcurrentIdenticalMisses(t *testing.T) {
	gate := newGate()
	gate.vec = vecN(2)
	m := newRecordingMetrics()
	q := Guard(gate, WithGuardMetrics(m))

	const n = 8
	var wg sync.WaitGroup
	results := make([]model.Vector, n)
	for i := range n {
		wg.Go(func() {
			v, err := q.Instant(t.Context(), string(QPodInfo), "kube_pod_info", ts0)
			assert.NoError(t, err)
			results[i] = v
		})
	}
	<-gate.started
	// Let the waiters pile up on the in-flight call before releasing it.
	require.Eventually(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.misses == n }, time.Second, time.Millisecond)
	close(gate.release)
	wg.Wait()

	assert.EqualValues(t, 1, gate.calls.Load(), "one upstream query for N identical misses")
	for _, r := range results {
		assert.Len(t, r, 2)
	}
	assert.Equal(t, n-1, m.coalesced)

	// Now cached.
	_, err := q.Instant(t.Context(), string(QPodInfo), "kube_pod_info", ts0)
	require.NoError(t, err)
	assert.EqualValues(t, 1, gate.calls.Load())
	assert.Equal(t, 1, m.hits)
}

func TestGuard_WaiterDeadlineDoesNotCancelLeader(t *testing.T) {
	gate := newGate()
	gate.vec = vecN(1)
	q := Guard(gate)

	leaderDone := make(chan error, 1)
	go func() {
		_, err := q.Instant(t.Context(), string(QPodInfo), "q", ts0)
		leaderDone <- err
	}()
	<-gate.started

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := q.Instant(ctx, string(QPodInfo), "q", ts0)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	close(gate.release)
	require.NoError(t, <-leaderDone, "the leader still receives its result")
}

// The caller that started a shared fetch leaving — a client disconnect — does
// not fail a waiter whose own context is live, and no second upstream query is
// issued for it.
func TestGuard_LeaderCancellationDoesNotFailLiveWaiter(t *testing.T) {
	gate := newGate()
	gate.vec = vecN(1)
	q := Guard(gate)

	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := q.Instant(leaderCtx, string(QPodInfo), "q", ts0)
		leaderDone <- err
	}()
	<-gate.started

	waiterDone := make(chan error, 1)
	go func() {
		_, err := q.Instant(t.Context(), string(QPodInfo), "q", ts0)
		waiterDone <- err
	}()
	waitForWaiters(t, q, "q", 2)
	cancelLeader()
	require.ErrorIs(t, <-leaderDone, context.Canceled, "the leaving caller gets its own error")

	close(gate.release)
	require.NoError(t, <-waiterDone, "the shared fetch outlived the caller that started it")
	assert.EqualValues(t, 1, gate.calls.Load(), "no second upstream query")
}

// When every waiter has left, the shared fetch is cancelled — nobody is
// waiting for it — and a later miss starts a new one.
func TestGuard_LastWaiterLeavingCancelsTheFetch(t *testing.T) {
	causes := make(chan error, 2)
	entered := make(chan struct{})
	var calls atomic.Int64
	inner := querierFunc(func(ctx context.Context, _, _ string, _ time.Time) (model.Vector, error) {
		if calls.Add(1) > 1 {
			return vecN(1), nil
		}
		close(entered)
		<-ctx.Done()
		causes <- context.Cause(ctx)
		return nil, ctx.Err()
	})
	q := Guard(inner)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := q.Instant(ctx, string(QPodInfo), "q", ts0)
		done <- err
	}()
	// Wait for the fetch to reach upstream: a flight abandoned before that
	// fails at its slot acquisition and never calls the inner querier.
	<-entered
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.ErrorIs(t, <-causes, errFlightAbandoned)

	v, err := q.Instant(t.Context(), string(QPodInfo), "q", ts0)
	require.NoError(t, err)
	assert.Len(t, v, 1)
	assert.EqualValues(t, 2, calls.Load(), "an abandoned flight is never rejoined")
}

// The shared fetch is bounded by the LATEST waiter deadline: the starting
// caller's short deadline passing neither cancels it nor shortens what the
// client sees as its deadline.
func TestGuard_SharedFetchUsesLatestWaiterDeadline(t *testing.T) {
	release := make(chan struct{})
	seen := make(chan time.Time, 1)
	var calls atomic.Int64
	inner := querierFunc(func(ctx context.Context, _, _ string, _ time.Time) (model.Vector, error) {
		calls.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		d, _ := ctx.Deadline()
		seen <- d
		return vecN(1), nil
	})
	q := Guard(inner)

	short, cancelShort := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancelShort()
	shortDone := make(chan error, 1)
	go func() {
		_, err := q.Instant(short, string(QPodInfo), "q", ts0)
		shortDone <- err
	}()
	waitForWaiters(t, q, "q", 1)

	long, cancelLong := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelLong()
	longDeadline, _ := long.Deadline()
	longDone := make(chan error, 1)
	go func() {
		_, err := q.Instant(long, string(QPodInfo), "q", ts0)
		longDone <- err
	}()
	waitForWaiters(t, q, "q", 2)
	require.ErrorIs(t, <-shortDone, context.DeadlineExceeded)
	time.Sleep(20 * time.Millisecond) // well past the short deadline
	close(release)

	require.NoError(t, <-longDone)
	assert.Equal(t, longDeadline, <-seen, "the client sees the latest waiter's deadline")
	assert.EqualValues(t, 1, calls.Load())
}

// A waiter with no deadline leaves the shared fetch with none.
func TestGuard_WaiterWithoutDeadlineUnboundsTheFetch(t *testing.T) {
	release := make(chan struct{})
	bounded := make(chan bool, 1)
	inner := querierFunc(func(ctx context.Context, _, _ string, _ time.Time) (model.Vector, error) {
		<-release
		_, ok := ctx.Deadline()
		bounded <- ok
		return vecN(1), nil
	})
	q := Guard(inner)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() {
		_, err := q.Instant(ctx, string(QPodInfo), "q", ts0)
		first <- err
	}()
	waitForWaiters(t, q, "q", 1)
	second := make(chan error, 1)
	go func() {
		_, err := q.Instant(context.Background(), string(QPodInfo), "q", ts0)
		second <- err
	}()
	waitForWaiters(t, q, "q", 2)
	close(release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	assert.False(t, <-bounded)
}

// A flight ended by its (latest-waiter) deadline reports the waiter's OWN
// DeadlineExceeded — the 504 mapping — never the shared context's Canceled,
// which the API would map to 499.
func TestGuard_DeadlineCutFlightReportsDeadlineExceeded(t *testing.T) {
	inner := querierFunc(func(ctx context.Context, _, _ string, _ time.Time) (model.Vector, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	q := Guard(inner)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := q.Instant(ctx, string(QPodInfo), "q", ts0)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, context.Canceled)
}

// A context error the inner querier produced on its own (its per-call
// timeout, say) is returned at once: only a flight its deadline timer ended
// makes a waiter wait out its own deadline.
func TestGuard_InnerContextErrorIsNotWaitedOut(t *testing.T) {
	inner := querierFunc(func(context.Context, string, string, time.Time) (model.Vector, error) {
		return nil, context.DeadlineExceeded
	})
	q := Guard(inner)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err := q.Instant(ctx, string(QPodInfo), "q", ts0)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second, "the caller is not held until its own deadline")
}

// waitForWaiters blocks until the flight for query has n waiters.
func waitForWaiters(t *testing.T, q Querier, query string, n int) {
	t.Helper()
	g := q.(*guard)
	key := cacheKeyFor(g.store, query, ts0)
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		f, ok := g.flights[key]
		return ok && f.waiters == n
	}, time.Second, time.Millisecond)
}

func TestGuard_ErrorsAreNotCached(t *testing.T) {
	f := &fakeBackend{err: errors.New("503 Service Unavailable")}
	q := Guard(f)
	_, err := q.Instant(t.Context(), string(QPodInfo), "q", ts0)
	require.Error(t, err)
	_, err = q.Instant(t.Context(), string(QPodInfo), "q", ts0)
	require.Error(t, err)
	calls, _ := f.seen()
	assert.Equal(t, 2, calls, "a failed query is re-sent, never served from cache")
}

func TestGuard_LimitBoundsInflightPerStore(t *testing.T) {
	gate := newGate()
	q := Guard(gate, WithMaxConcurrency(32), WithQueryCache(0, 0))

	var wg sync.WaitGroup
	for i := range 120 {
		wg.Go(func() {
			_, err := q.Instant(t.Context(), string(QPodInfo), fmt.Sprintf("q%d", i), ts0)
			assert.NoError(t, err)
		})
	}
	for range 32 {
		<-gate.started
	}
	time.Sleep(20 * time.Millisecond) // give any over-admission a chance to show
	assert.EqualValues(t, 32, gate.inflight.Load())
	close(gate.release)
	wg.Wait()
	assert.LessOrEqual(t, gate.peak.Load(), int64(32))
	assert.EqualValues(t, 120, gate.calls.Load())
}

func TestGuard_DefaultsWithNoOptions(t *testing.T) {
	gate := newGate()
	q := Guard(gate).(*guard)
	require.NotNil(t, q.sem, "limit on by default")
	require.NotNil(t, q.cache, "cache on by default")
	assert.Equal(t, DefaultQueryCacheMaxSeries, q.cache.max)
	assert.Equal(t, DefaultQueryCacheTTL, q.cache.ttl)

	var wg sync.WaitGroup
	for i := range DefaultMaxConcurrency + 8 {
		wg.Go(func() {
			_, _ = q.Instant(t.Context(), string(QPodInfo), fmt.Sprintf("q%d", i), ts0)
		})
	}
	for range DefaultMaxConcurrency {
		<-gate.started
	}
	time.Sleep(20 * time.Millisecond)
	assert.EqualValues(t, DefaultMaxConcurrency, gate.inflight.Load())
	close(gate.release)
	wg.Wait()
}

func TestGuard_ExplicitDisable(t *testing.T) {
	f := &fakeBackend{vec: vecN(1)}
	q := Guard(f, WithMaxConcurrency(0), WithQueryCache(0, 0)).(*guard)
	assert.Nil(t, q.sem)
	assert.Nil(t, q.cache)
	for range 2 {
		_, err := q.Instant(t.Context(), string(QPodInfo), "q", ts0)
		require.NoError(t, err)
	}
	calls, _ := f.seen()
	assert.Equal(t, 2, calls)
}

func TestGuard_WaitEndsOnDeadline(t *testing.T) {
	gate := newGate()
	q := Guard(gate, WithMaxConcurrency(1), WithQueryCache(0, 0))
	go func() { _, _ = q.Instant(t.Context(), string(QPodInfo), "holder", ts0) }()
	<-gate.started

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := q.Instant(ctx, string(QPodInfo), "queued", ts0)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(gate.release)
}

func TestGuard_ProbeBypassesLimitAndCache(t *testing.T) {
	gate := newGate()
	probe := &fakeBackend{vec: vecN(1)}
	// One store: the gate holds the only slot; the probe must not queue.
	split := &splitBackend{probe: probe, other: gate}
	q := Guard(split, WithMaxConcurrency(1))
	go func() { _, _ = q.Instant(t.Context(), string(QPodInfo), "holder", ts0) }()
	<-gate.started

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for range 2 {
		_, err := q.Instant(ctx, string(QUpProbe), string(QUpProbe), ts0)
		require.NoError(t, err)
	}
	calls, _ := probe.seen()
	assert.Equal(t, 2, calls, "probe never served from cache")
	close(gate.release)
}

// splitBackend sends probe-family queries to one fake and the rest to another.
type splitBackend struct {
	probe Querier
	other Querier
}

func (s *splitBackend) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	if isProbe(name) {
		return s.probe.Instant(ctx, name, query, ts)
	}
	return s.other.Instant(ctx, name, query, ts)
}

func TestGuard_HitRecordsNoUpstreamObservation(t *testing.T) {
	m := newRecordingMetrics()
	f := &fakeBackend{vec: vecN(1)}
	// The client-level observations happen inside the wrapped querier; a
	// recording Client stand-in shows a hit never reaches it.
	rec := &observingBackend{inner: f, m: m}
	q := Guard(rec, WithGuardMetrics(m))
	for range 3 {
		_, err := q.Instant(t.Context(), string(QPodInfo), "q", ts0)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, m.durations)
	assert.Equal(t, 1, m.seriesObservations)
	assert.Equal(t, 0, m.failures)
	assert.Equal(t, 2, m.hits)
}

// observingBackend records what promql.Client records per real query.
type observingBackend struct {
	inner Querier
	m     *recordingMetrics
}

func (o *observingBackend) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	v, err := o.inner.Instant(ctx, name, query, ts)
	o.m.ObserveQueryDuration(name, 0)
	if err != nil {
		o.m.IncQueryFailure(name)
		return nil, err
	}
	o.m.ObserveQuerySeries(name, len(v))
	return v, nil
}

func TestGuard_SlotWaitMetricsAndContext(t *testing.T) {
	m := newRecordingMetrics()
	var sawWait atomic.Int64
	inner := querierFunc(func(ctx context.Context, _, _ string, _ time.Time) (model.Vector, error) {
		sawWait.Store(int64(slotWaitFrom(ctx)))
		return nil, nil
	})
	g := newGuard(inner, 1, 4, nil, m)
	_, err := g.instant(t.Context(), "zone-a", string(QPodInfo), "q", ts0)
	require.NoError(t, err)
	assert.Equal(t, 1, m.waits["zone-a"])
	assert.Equal(t, 1, m.maxInflight["zone-a"])
	assert.Equal(t, 0, m.inflight["zone-a"])
	assert.GreaterOrEqual(t, sawWait.Load(), int64(0))
}

func TestGuard_ExpiredEntryRefetched(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	f := &fakeBackend{vec: vecN(1)}
	q := Guard(f, WithQueryCache(10, time.Minute), withClock(clk.now))
	_, _ = q.Instant(t.Context(), string(QPodInfo), "q", ts0)
	_, _ = q.Instant(t.Context(), string(QPodInfo), "q", ts0)
	clk.advance(time.Minute)
	_, _ = q.Instant(t.Context(), string(QPodInfo), "q", ts0)
	calls, _ := f.seen()
	assert.Equal(t, 2, calls)
}

// missGateMetrics holds the FIRST cache miss — reported between the guard's
// cache lookup and its coalescing join — until release is closed, so a test can
// run another caller's whole flight inside that window.
type missGateMetrics struct {
	*recordingMetrics
	first   atomic.Bool
	held    chan struct{}
	release chan struct{}
}

func (m *missGateMetrics) IncCacheMiss() {
	m.recordingMetrics.IncCacheMiss()
	if m.first.CompareAndSwap(false, true) {
		close(m.held)
		<-m.release
	}
}

// A caller that missed the cache while no entry existed, and reaches the
// coalescing group only after another caller's flight for the key completed
// and was forgotten, is served from the entry that flight left behind — never
// a second upstream query for the key.
func TestGuard_MissThatOutlivesTheFlightIsServedFromTheCache(t *testing.T) {
	var calls atomic.Int64
	inner := querierFunc(func(context.Context, string, string, time.Time) (model.Vector, error) {
		calls.Add(1)
		return vecN(2), nil
	})
	m := &missGateMetrics{recordingMetrics: newRecordingMetrics(), held: make(chan struct{}), release: make(chan struct{})}
	q := Guard(inner, WithGuardMetrics(m))

	late := make(chan model.Vector, 1)
	go func() {
		v, err := q.Instant(t.Context(), string(QPodInfo), "kube_pod_info", ts0)
		assert.NoError(t, err)
		late <- v
	}()
	<-m.held // the late caller has missed and not yet joined a flight

	v, err := q.Instant(t.Context(), string(QPodInfo), "kube_pod_info", ts0)
	require.NoError(t, err)
	require.Len(t, v, 2)
	require.EqualValues(t, 1, calls.Load())

	close(m.release)
	assert.Len(t, <-late, 2)
	assert.EqualValues(t, 1, calls.Load(), "the late miss reuses the completed flight's entry")
	m.mu.Lock()
	defer m.mu.Unlock()
	assert.Equal(t, 2, m.misses)
	assert.Equal(t, 0, m.hits)
	assert.Equal(t, 1, m.coalesced, "served by another caller's upstream query")
}

// querierFunc adapts a function to Querier (test-only).
type querierFunc func(ctx context.Context, name, query string, ts time.Time) (model.Vector, error)

func (f querierFunc) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	return f(ctx, name, query, ts)
}
