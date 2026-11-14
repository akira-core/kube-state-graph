package route

import (
	"context"
	"errors"
	"sync"
)

// errFlightAbandoned is what a waiter observes when the execution it waited on
// ended without a value — the leader failed, panicked or was cancelled. It is
// never returned to a caller: a waiter that sees a failed flight runs the work
// itself.
var errFlightAbandoned = errors.New("route: shared execution ended without a value")

// flight is one in-progress execution other callers may wait on. done closes
// once val/err are final.
type flight[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// wait blocks until f completes or ctx ends. ok reports a successful flight;
// a failed one returns ok=false with a nil error, telling the caller to run
// the work itself.
func (f *flight[V]) wait(ctx context.Context) (val V, ok bool, err error) {
	select {
	case <-f.done:
		return f.val, f.err == nil, nil
	case <-ctx.Done():
		var zero V
		return zero, false, context.Cause(ctx)
	}
}

// flightMemo memoises a pure, request-invariant computation for the lifetime
// of one build scope, sharing a single execution among concurrent callers.
//
// Failures are never remembered and never handed to another caller: a waiter
// whose leader failed — for ANY reason, including the leader's own per-call
// timeout — runs the work itself under its own context. Sharing an error would
// let one key's deadline become another key's route_engine_error, and during
// an outage the retry costs exactly what the un-memoised path did.
//
// The zero value is ready to use. Values are shared between callers, so they
// MUST be treated as read-only.
type flightMemo[K comparable, V any] struct {
	mu       sync.Mutex
	done     map[K]V
	inflight map[K]*flight[V]
}

// do returns the memoised value for k, waits for an execution already in
// progress, or runs fn. A leader that panics releases its waiters (they retry)
// and the panic continues on the leader's goroutine.
func (m *flightMemo[K, V]) do(ctx context.Context, k K, fn func(context.Context) (V, error)) (V, error) {
	for {
		m.mu.Lock()
		if v, ok := m.done[k]; ok {
			m.mu.Unlock()
			return v, nil
		}
		if f, ok := m.inflight[k]; ok {
			m.mu.Unlock()
			v, ok, err := f.wait(ctx)
			if err != nil {
				return v, err
			}
			if ok {
				return v, nil
			}
			continue // the leader failed: retry, possibly as the next leader
		}
		f := &flight[V]{done: make(chan struct{})}
		if m.inflight == nil {
			m.inflight = map[K]*flight[V]{}
		}
		m.inflight[k] = f
		m.mu.Unlock()
		return m.lead(ctx, k, f, fn)
	}
}

// lead runs fn as k's leader and publishes the outcome. The deferred release
// runs even when fn panics, so waiters never block on a dead leader.
func (m *flightMemo[K, V]) lead(ctx context.Context, k K, f *flight[V], fn func(context.Context) (V, error)) (v V, err error) {
	f.err = errFlightAbandoned // overwritten on a normal return
	defer func() {
		m.mu.Lock()
		delete(m.inflight, k)
		if f.err == nil {
			if m.done == nil {
				m.done = map[K]V{}
			}
			m.done[k] = f.val
		}
		m.mu.Unlock()
		close(f.done)
	}()
	v, err = fn(ctx)
	f.val, f.err = v, err
	return v, err
}
