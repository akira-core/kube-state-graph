package promql

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/common/model"
)

// errFlightAbandoned is the cause a shared fetch is cancelled with when every
// caller waiting on it has stopped waiting.
var errFlightAbandoned = errors.New("every caller waiting on the shared upstream query left")

// flight is one shared upstream fetch for a cache key, coalescing the
// concurrent misses on that key.
//
// Its context is detached from every caller's cancellation
// (context.WithoutCancel of the caller that started it, so trace parentage and
// other values survive) and is ended by the guard alone, for one of two
// reasons: every waiter left (refcount reached zero), or the LATEST deadline
// among its waiters passed. A waiter with no deadline makes the flight
// unbounded. A caller's cancellation therefore never fails another caller's
// query, which a fetch bound to the starting caller's context did.
//
// Every field below done is guarded by the owning guard's mu.
type flight struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{} // closed once vec / err / reused are final

	vec    model.Vector
	err    error
	reused bool // answered from a cache entry another flight left behind

	waiters   int
	deadline  time.Time   // latest waiter deadline; meaningful while !unbounded
	unbounded bool        // some waiter has no deadline
	timer     *time.Timer // fires at deadline; nil until a bounded waiter joins
	finished  bool        // removed from the map; never rejoined
}

// flightContext is a flight's context. Deadline reports the flight's current
// (latest-waiter) deadline, so the client derives the upstream evaluation
// timeout from the waiter that will wait longest, not from the one that
// happened to start the fetch. Cancellation and values are the embedded
// context's.
type flightContext struct {
	context.Context
	g *guard
	f *flight
}

func (c flightContext) Deadline() (time.Time, bool) {
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	if c.f.unbounded || c.f.deadline.IsZero() {
		return time.Time{}, false
	}
	return c.f.deadline, true
}

// join attaches the caller to the in-flight fetch for key, creating it when
// none is running. leader reports whether the caller created it and must start
// it.
func (g *guard) join(ctx context.Context, key cacheKey) (f *flight, leader bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.flights == nil {
		g.flights = make(map[cacheKey]*flight)
	}
	f, ok := g.flights[key]
	if !ok {
		shared, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
		f = &flight{cancel: cancel, done: make(chan struct{})}
		f.ctx = flightContext{Context: shared, g: g, f: f}
		g.flights[key] = f
		leader = true
	}
	f.waiters++
	g.extendLocked(key, f, ctx)
	return f, leader
}

// extendLocked widens f's deadline to cover the joining caller's.
func (g *guard) extendLocked(key cacheKey, f *flight, ctx context.Context) {
	if f.unbounded {
		return
	}
	d, ok := ctx.Deadline()
	if !ok {
		f.unbounded = true
		if f.timer != nil {
			f.timer.Stop()
		}
		return
	}
	if f.timer == nil {
		f.deadline = d
		f.timer = time.AfterFunc(time.Until(d), func() { g.expire(key, f) })
		return
	}
	if d.After(f.deadline) {
		f.deadline = d
		f.timer.Reset(time.Until(d))
	}
}

// expire ends f when its deadline has passed. The checks run under the lock
// because a waiter may have extended or unbounded the flight after the timer
// fired but before this ran; it is then a no-op and the re-armed timer, if
// any, fires again.
func (g *guard) expire(key cacheKey, f *flight) {
	g.mu.Lock()
	if f.finished || f.unbounded || time.Now().Before(f.deadline) {
		g.mu.Unlock()
		return
	}
	g.forgetLocked(key, f)
	g.mu.Unlock()
	f.cancel(context.DeadlineExceeded)
}

// leave detaches a waiter that stopped waiting. The last one out cancels the
// fetch nobody is waiting for and removes it, so a later miss starts afresh.
func (g *guard) leave(key cacheKey, f *flight) {
	g.mu.Lock()
	f.waiters--
	abandon := f.waiters == 0 && !f.finished
	if abandon {
		g.forgetLocked(key, f)
	}
	g.mu.Unlock()
	if abandon {
		f.cancel(errFlightAbandoned)
	}
}

// forgetLocked removes f from the flight map (when it is still the entry for
// key) and stops its timer. A forgotten flight is never joined again.
func (g *guard) forgetLocked(key cacheKey, f *flight) {
	f.finished = true
	if g.flights[key] == f {
		delete(g.flights, key)
	}
	if f.timer != nil {
		f.timer.Stop()
	}
}

// run performs the shared fetch and publishes its outcome.
//
// It re-reads the cache first: a flight that completed between a caller's
// lookup and its join has already been forgotten but left its entry behind
// (the entry is put BEFORE the flight is forgotten), so no second upstream
// query is issued for it.
func (g *guard) run(key cacheKey, f *flight, backend, name, query string, ts time.Time) {
	if v, ok := g.cache.get(key); ok {
		f.vec, f.reused = v, true
	} else {
		v, err := g.fetch(f.ctx, backend, name, query, ts)
		if err == nil {
			g.cache.put(key, v)
		}
		f.vec, f.err = v, err
	}
	g.mu.Lock()
	g.forgetLocked(key, f)
	g.mu.Unlock()
	close(f.done)
	f.cancel(nil)
}

// await waits for f on behalf of one caller and returns that caller's result.
func (g *guard) await(ctx context.Context, key cacheKey, f *flight, leader bool, name string) (model.Vector, error) {
	select {
	case <-ctx.Done():
		g.leave(key, f)
		return nil, fmt.Errorf("prom query %s: %w", name, ctx.Err())
	case <-f.done:
	}
	if f.err != nil {
		// Only the flight's deadline can end it while a waiter remains, and
		// that deadline is the latest waiter's — so a waiter handed the
		// flight's context error is at (or within a timer tick of) its own
		// deadline. Report its OWN error, which keeps the ordinary 504
		// mapping instead of surfacing the shared context's Canceled as 499.
		if isContextErr(f.err) {
			if ctx.Err() == nil {
				if _, bounded := ctx.Deadline(); !bounded {
					return nil, f.err
				}
				<-ctx.Done()
			}
			return nil, fmt.Errorf("prom query %s: %w", name, ctx.Err())
		}
		return nil, f.err
	}
	if !leader || f.reused {
		g.cm.IncCacheCoalesced()
	}
	return cloneVector(f.vec), nil
}
