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

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/store"
)

// testClock is a manually advanced clock for TTL tests.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func hitResult(svc string) routeResult {
	return routeResult{dest: build.RouteDestination{Cluster: "prod-01", Namespace: "shop", Service: svc}, outcome: build.RouteHit}
}

func countingFn(calls *atomic.Int64, val routeResult, err error) func(context.Context) (routeResult, error) {
	return func(context.Context) (routeResult, error) {
		calls.Add(1)
		return val, err
	}
}

func TestResultCache_HitAndTTL(t *testing.T) {
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	c := newResultCache(8, resultCacheTTL, clk.now)
	k := resultKey{host: "api.example.com", at: 1}
	var calls atomic.Int64
	fn := countingFn(&calls, hitResult("api"), nil)

	for range 3 {
		v, err := c.do(t.Context(), k, fn)
		require.NoError(t, err)
		assert.Equal(t, hitResult("api"), v)
	}
	assert.Equal(t, int64(1), calls.Load(), "served from the cache within the TTL")

	clk.advance(59 * time.Second)
	_, _ = c.do(t.Context(), k, fn)
	assert.Equal(t, int64(1), calls.Load())

	clk.advance(time.Second)
	_, _ = c.do(t.Context(), k, fn)
	assert.Equal(t, int64(2), calls.Load(), "an entry 60s old is expired")
}

func TestResultCache_DifferentInstantMisses(t *testing.T) {
	c := newResultCache(8, time.Hour, time.Now)
	var calls atomic.Int64
	fn := countingFn(&calls, hitResult("api"), nil)
	_, _ = c.do(t.Context(), resultKey{host: "api.example.com", at: 1}, fn)
	_, _ = c.do(t.Context(), resultKey{host: "api.example.com", at: 2}, fn)
	assert.Equal(t, int64(2), calls.Load())
}

func TestResultCache_ErrorsAreNotCached(t *testing.T) {
	c := newResultCache(8, time.Hour, time.Now)
	k := resultKey{host: "api.example.com", at: 1}
	boom := errors.New("store unreachable")
	var calls atomic.Int64

	_, err := c.do(t.Context(), k, countingFn(&calls, routeResult{}, boom))
	require.ErrorIs(t, err, boom)
	assert.Zero(t, c.len(), "an error leaves nothing behind")

	v, err := c.do(t.Context(), k, countingFn(&calls, hitResult("api"), nil))
	require.NoError(t, err)
	assert.Equal(t, hitResult("api"), v)
	assert.Equal(t, int64(2), calls.Load())
}

func TestResultCache_LRUBound(t *testing.T) {
	c := newResultCache(2, time.Hour, time.Now)
	var calls atomic.Int64
	fn := countingFn(&calls, hitResult("x"), nil)
	a, b, d := resultKey{host: "a"}, resultKey{host: "b"}, resultKey{host: "d"}

	_, _ = c.do(t.Context(), a, fn)
	_, _ = c.do(t.Context(), b, fn)
	_, _ = c.do(t.Context(), a, fn) // a is now most recently used
	_, _ = c.do(t.Context(), d, fn) // evicts b, the least recently used
	assert.Equal(t, 2, c.len())
	assert.Equal(t, int64(3), calls.Load())

	_, _ = c.do(t.Context(), a, fn)
	assert.Equal(t, int64(3), calls.Load(), "a survived")
	_, _ = c.do(t.Context(), b, fn)
	assert.Equal(t, int64(4), calls.Load(), "b was evicted")
}

// Concurrent identical misses — from different builds — share one resolution.
func TestResultCache_CoalescesConcurrentMisses(t *testing.T) {
	c := newResultCache(8, time.Hour, time.Now)
	k := resultKey{host: "api.example.com", at: 1}
	var calls atomic.Int64
	release := make(chan struct{})
	fn := func(context.Context) (routeResult, error) {
		calls.Add(1)
		<-release
		return hitResult("api"), nil
	}

	var wg sync.WaitGroup
	vals := make([]routeResult, 6)
	errs := make([]error, 6)
	for i := range 6 {
		wg.Go(func() { vals[i], errs[i] = c.do(t.Context(), k, fn) })
	}
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int64(1), calls.Load())
	for i := range 6 {
		require.NoError(t, errs[i])
		assert.Equal(t, hitResult("api"), vals[i])
	}
}

// A waiter whose resolver failed is not handed that failure: it resolves for
// itself under its own context.
func TestResultCache_WaiterResolvesAfterLeaderFailure(t *testing.T) {
	c := newResultCache(8, time.Hour, time.Now)
	k := resultKey{host: "api.example.com", at: 1}
	entered := make(chan struct{})
	release := make(chan struct{})
	boom := errors.New("leader's per-call timeout")

	leaderErr := make(chan error, 1)
	go func() {
		_, err := c.do(t.Context(), k, func(context.Context) (routeResult, error) {
			close(entered)
			<-release
			return routeResult{}, boom
		})
		leaderErr <- err
	}()
	<-entered

	waiter := make(chan routeResult, 1)
	go func() {
		v, err := c.do(t.Context(), k, func(context.Context) (routeResult, error) { return hitResult("api"), nil })
		assert.NoError(t, err)
		waiter <- v
	}()
	time.Sleep(10 * time.Millisecond)
	close(release)

	require.ErrorIs(t, <-leaderErr, boom)
	assert.Equal(t, hitResult("api"), <-waiter)
}

// A panicking resolver releases its waiters.
func TestResultCache_PanicReleasesWaiters(t *testing.T) {
	c := newResultCache(8, time.Hour, time.Now)
	k := resultKey{host: "api.example.com", at: 1}
	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		defer func() { _ = recover() }()
		_, _ = c.do(t.Context(), k, func(context.Context) (routeResult, error) {
			close(entered)
			<-release
			panic("boom")
		})
	}()
	<-entered
	done := make(chan routeResult, 1)
	go func() {
		v, _ := c.do(t.Context(), k, func(context.Context) (routeResult, error) { return hitResult("api"), nil })
		done <- v
	}()
	time.Sleep(10 * time.Millisecond)
	close(release)
	select {
	case v := <-done:
		assert.Equal(t, hitResult("api"), v)
	case <-time.After(2 * time.Second):
		t.Fatal("waiter still blocked after the resolver panicked")
	}
}

func TestResultCache_DisabledRunsEveryTime(t *testing.T) {
	c := newResultCache(0, time.Hour, time.Now)
	require.Nil(t, c)
	var calls atomic.Int64
	fn := countingFn(&calls, hitResult("api"), nil)
	_, _ = c.do(t.Context(), resultKey{}, fn)
	_, _ = c.do(t.Context(), resultKey{}, fn)
	assert.Equal(t, int64(2), calls.Load())
}

func TestResultKeyOf(t *testing.T) {
	at := time.Unix(1_700_000_000, 123_456_789).UTC()
	k := resultKeyOf(build.RouteRequest{
		CallerCluster: "prod-01", Host: "api.example.com", Path: "/", Port: 443,
		IPs: []string{"198.51.100.7", "2001:db8::7"}, At: at,
	})
	assert.Equal(t, resultKey{
		caller: "prod-01", host: "api.example.com", path: "/", port: 443,
		ips: "198.51.100.7,2001:db8::7", at: at.UnixMilli(),
	}, k)
}

// End to end through the Resolver: a repeated build at the same instant does
// no store, translation or matcher work and returns the same answers; a build
// at another instant resolves again; a failed key is retried next build.
func TestResolver_RepeatBuildIsServedFromTheCache(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	snap := sharedIngressSnapshot(t, at)
	st := &fakeStore{snap: snap}
	tr, mt := &fakeTranslator{}, &fakeMatcher{}
	r := newResolver(st, tr, mt)
	reqs := sharedIngressRequests(16, at)

	first := resolveBuild(t.Context(), r, reqs)
	probes, loads, translates, execs := st.probes.Load(), st.loads.Load(), tr.calls.Load(), mt.execs.Load()

	second := resolveBuild(t.Context(), r, reqs)
	assert.Equal(t, first, second)
	assert.Equal(t, probes, st.probes.Load(), "no probe on a repeat build")
	assert.Equal(t, loads, st.loads.Load(), "no load on a repeat build")
	assert.Equal(t, translates, tr.calls.Load(), "no translation on a repeat build")
	assert.Equal(t, execs, mt.execs.Load(), "no matcher run on a repeat build")

	later := sharedIngressRequests(16, at.Add(30*time.Second))
	_ = resolveBuild(t.Context(), r, later)
	assert.Greater(t, st.loads.Load(), loads, "a different instant is a different answer")
}

func TestResolver_FailedKeyIsResolvedAgainNextBuild(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	snap := sharedIngressSnapshot(t, at)
	var fail atomic.Bool
	fail.Store(true)
	st := &fakeStore{snap: snap}
	st.loadHook = func(string, string) (store.TrafficSnapshot, error) {
		if fail.Load() {
			return store.TrafficSnapshot{}, errors.New("clickhouse down")
		}
		return snap, nil
	}
	r := newResolver(st, &fakeTranslator{}, &fakeMatcher{})
	reqs := sharedIngressRequests(1, at)

	first := resolveBuild(t.Context(), r, reqs)
	require.NotEmpty(t, first[0].Err)

	fail.Store(false)
	second := resolveBuild(t.Context(), r, reqs)
	assert.Empty(t, second[0].Err)
	assert.Equal(t, build.RouteHit, second[0].Outcome)
}
