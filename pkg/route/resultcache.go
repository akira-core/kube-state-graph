package route

import (
	"container/list"
	"context"
	"strings"
	"sync"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/build"
)

// Bounds of the cross-build result cache. Fixed, like the per-build
// routeResolveConcurrency / maxRouteKeys: nothing an operator can observe
// tells them a better value. 4096 answers is eight builds' worth of the
// per-build key cap; one entry is a handful of short strings, so the cache
// stays within a few MiB. 60s matches the PromQL query cache's TTL.
const (
	resultCacheMaxEntries = 4096
	resultCacheTTL        = 60 * time.Second
)

// resultKey identifies one route answer across builds: every RouteRequest
// field, with the IP list joined in request order and the instant at the
// store's millisecond precision. Builds over the same aligned window end share
// an instant, so repeated builds share keys.
type resultKey struct {
	caller, host, path string
	port               int
	ips                string
	at                 int64
}

func resultKeyOf(req build.RouteRequest) resultKey {
	return resultKey{
		caller: req.CallerCluster,
		host:   req.Host,
		path:   req.Path,
		port:   req.Port,
		ips:    strings.Join(req.IPs, ","),
		at:     req.At.UnixMilli(),
	}
}

// routeResult is one successful answer. Both fields are plain values, so a
// cached answer can be handed to any number of callers.
type routeResult struct {
	dest    build.RouteDestination
	outcome build.RouteOutcome
}

type resultEntry struct {
	key     resultKey
	val     routeResult
	expires time.Time
}

// resultCache is the process-wide, cross-build cache of route answers: a
// bounded LRU whose entries expire after a TTL, holding successful answers
// only, with concurrent identical misses coalesced into one resolution. It
// replaces the old "the shared *Resolver stays stateless" rule, whose concern —
// an instance cache leaking without bound across builds — a size bound and a
// TTL answer.
//
// An answer is a pure function of its key EXCEPT for a late store write: a
// version whose valid_from is at or before an already-answered instant, written
// afterwards, changes that instant's answer. The TTL bounds how long such a
// stale answer can be served.
//
// A nil *resultCache is a disabled cache: do just runs the resolution.
type resultCache struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	now     func() time.Time
	ll      *list.List // front = most recently used
	items   map[resultKey]*list.Element
	flights map[resultKey]*flight[routeResult]
}

func newResultCache(maxEntries int, ttl time.Duration, now func() time.Time) *resultCache {
	if maxEntries <= 0 {
		return nil
	}
	return &resultCache{
		max:     maxEntries,
		ttl:     ttl,
		now:     now,
		ll:      list.New(),
		items:   map[resultKey]*list.Element{},
		flights: map[resultKey]*flight[routeResult]{},
	}
}

// resultLease is the right — and the obligation — to resolve one key and
// publish the outcome. finish MUST be called exactly once; a nil lease is a
// no-op (disabled cache).
type resultLease struct {
	c    *resultCache
	k    resultKey
	f    *flight[routeResult]
	done bool
}

// acquire looks k up. It returns the cached answer (hit), or the flight of a
// resolution already in progress to wait on, or — when neither exists — a
// lease registering the caller as the key's resolver. Exactly one of the three
// is meaningful. A disabled cache always returns a nil lease and nothing else.
func (c *resultCache) acquire(k resultKey) (val routeResult, hit bool, wait *flight[routeResult], lease *resultLease) {
	if c == nil {
		return routeResult{}, false, nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		e := el.Value.(*resultEntry)
		if c.now().Before(e.expires) {
			c.ll.MoveToFront(el)
			return e.val, true, nil, nil
		}
		c.ll.Remove(el)
		delete(c.items, k)
	}
	if f, ok := c.flights[k]; ok {
		return routeResult{}, false, f, nil
	}
	f := &flight[routeResult]{done: make(chan struct{})}
	c.flights[k] = f
	return routeResult{}, false, nil, &resultLease{c: c, k: k, f: f}
}

// finish publishes the lease's outcome: a successful answer is stored, every
// waiter is released either way (a waiter of a failed flight resolves for
// itself). Idempotent.
func (l *resultLease) finish(val routeResult, err error) {
	if l == nil || l.done {
		return
	}
	l.done = true
	c := l.c
	c.mu.Lock()
	delete(c.flights, l.k)
	if err == nil {
		c.putLocked(l.k, val)
	}
	c.mu.Unlock()
	l.f.val, l.f.err = val, err
	close(l.f.done)
}

// run resolves under the lease and publishes the result; a panic in fn
// releases the waiters before it continues up the caller's goroutine.
func (l *resultLease) run(ctx context.Context, fn func(context.Context) (routeResult, error)) (val routeResult, err error) {
	defer func() {
		if l != nil && !l.done {
			l.finish(routeResult{}, errFlightAbandoned)
		}
	}()
	val, err = fn(ctx)
	l.finish(val, err)
	return val, err
}

// putLocked inserts (or refreshes) k, evicting least-recently-used entries
// past the bound.
func (c *resultCache) putLocked(k resultKey, val routeResult) {
	if el, ok := c.items[k]; ok {
		c.ll.Remove(el)
		delete(c.items, k)
	}
	c.items[k] = c.ll.PushFront(&resultEntry{key: k, val: val, expires: c.now().Add(c.ttl)})
	for c.ll.Len() > c.max {
		back := c.ll.Back()
		c.ll.Remove(back)
		delete(c.items, back.Value.(*resultEntry).key)
	}
}

// do returns k's cached answer, waits for a resolution of k already in
// progress (in any build), or resolves it with fn and caches a success.
// Errors are never cached and never handed to a waiter: a waiter whose
// resolver failed resolves for itself under its own context.
func (c *resultCache) do(ctx context.Context, k resultKey, fn func(context.Context) (routeResult, error)) (routeResult, error) {
	for {
		val, hit, wait, lease := c.acquire(k)
		if hit {
			return val, nil
		}
		if wait != nil {
			v, ok, err := wait.wait(ctx)
			if err != nil {
				return routeResult{}, err
			}
			if ok {
				return v, nil
			}
			continue
		}
		if lease == nil { // disabled cache
			return fn(ctx)
		}
		return lease.run(ctx, fn)
	}
}

// len reports the number of resident answers (expired ones included until
// they are looked up or evicted).
func (c *resultCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
