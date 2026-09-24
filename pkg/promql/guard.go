package promql

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

// LimiterMetrics is the OPTIONAL upgrade interface a Metrics implementation may
// satisfy to observe the per-store concurrency limit. backend is the
// routing-table name the query was issued through.
type LimiterMetrics interface {
	// AddInflight moves the in-flight gauge for backend by delta (+1 / -1).
	AddInflight(backend string, delta int)
	// ObserveSlotWait records how long a query waited for a slot, in seconds.
	ObserveSlotWait(backend string, seconds float64)
}

func limiterMetricsOf(m Metrics) LimiterMetrics {
	if lm, ok := m.(LimiterMetrics); ok && lm != nil {
		return lm
	}
	return noopLimiterMetrics{}
}

type noopLimiterMetrics struct{}

func (noopLimiterMetrics) AddInflight(string, int)         {}
func (noopLimiterMetrics) ObserveSlotWait(string, float64) {}

// RouterOption configures the concurrency limit and query-result cache a
// Router (or Guard) applies to each backend store. With no option the defaults
// apply: DefaultMaxConcurrency and a DefaultQueryCacheMaxSeries /
// DefaultQueryCacheTTL cache.
type RouterOption func(*guardConfig)

type guardConfig struct {
	maxConcurrency int
	cacheMaxSeries int
	cacheTTL       time.Duration
	metrics        Metrics
	now            func() time.Time
}

func newGuardConfig(opts []RouterOption) guardConfig {
	cfg := guardConfig{
		maxConcurrency: DefaultMaxConcurrency,
		cacheMaxSeries: DefaultQueryCacheMaxSeries,
		cacheTTL:       DefaultQueryCacheTTL,
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return cfg
}

// WithMaxConcurrency bounds the upstream queries in flight to one backend
// store. n <= 0 disables the bound.
func WithMaxConcurrency(n int) RouterOption {
	return func(c *guardConfig) { c.maxConcurrency = max(0, n) }
}

// WithQueryCache sizes the query-result cache by total resident series and
// per-entry TTL. maxSeries <= 0 disables the cache; ttl <= 0 with a positive
// budget uses DefaultQueryCacheTTL.
func WithQueryCache(maxSeries int, ttl time.Duration) RouterOption {
	return func(c *guardConfig) {
		c.cacheMaxSeries = max(0, maxSeries)
		c.cacheTTL = ttl
		if c.cacheTTL <= 0 {
			c.cacheTTL = DefaultQueryCacheTTL
		}
	}
}

// WithGuardMetrics supplies the recorder Guard reports limiter and cache
// observations to. A Router ignores it — it reports to the Metrics passed to
// NewRouter.
func WithGuardMetrics(m Metrics) RouterOption {
	return func(c *guardConfig) { c.metrics = m }
}

// withClock replaces the cache's time source (tests).
func withClock(now func() time.Time) RouterOption {
	return func(c *guardConfig) { c.now = now }
}

// guard fronts ONE backend store: query-result cache, then miss coalescing,
// then the store's concurrency slot, then the real client. It is shared by
// every routing-table backend naming the same store and survives a reload that
// keeps the store, so the bound is per store and process-wide.
//
// Probe-family queries bypass all three: readiness and retention must observe
// the live upstream, and must never queue behind builds.
type guard struct {
	inner   Querier
	store   uint64
	sem     *semaphore.Weighted // nil ⇒ no bound
	cache   *queryCache         // shared across stores; nil ⇒ disabled
	flight  singleflight.Group
	limiter LimiterMetrics
	cm      CacheMetrics
}

var _ Querier = (*guard)(nil)

func newGuard(inner Querier, store uint64, maxConcurrency int, cache *queryCache, m Metrics) *guard {
	g := &guard{
		inner:   inner,
		store:   store,
		cache:   cache,
		limiter: limiterMetricsOf(m),
		cm:      cacheMetricsOf(m),
	}
	if maxConcurrency > 0 {
		g.sem = semaphore.NewWeighted(int64(maxConcurrency))
	}
	return g
}

// Guard wraps a single plain Querier in the same concurrency limit and
// query-result cache a Router applies to each of its stores, with the same
// defaults. Use it when handing the engine one client rather than a Router.
func Guard(q Querier, opts ...RouterOption) Querier {
	cfg := newGuardConfig(opts)
	cache := newQueryCache(cfg.cacheMaxSeries, cfg.cacheTTL, cacheMetricsOf(cfg.metrics), cfg.now)
	return newGuard(q, 1, cfg.maxConcurrency, cache, cfg.metrics)
}

// Instant satisfies Querier for a guard used outside a Router.
func (g *guard) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	return g.instant(ctx, "", name, query, ts)
}

// CloseIdleConnections forwards to the wrapped client so a Router reload still
// releases a retired store's pool.
func (g *guard) CloseIdleConnections() { closeIdle(g.inner) }

// instantVia issues through q, passing the backend name when q is a guard.
func instantVia(ctx context.Context, q Querier, backend, name, query string, ts time.Time) (model.Vector, error) {
	if g, ok := q.(*guard); ok {
		return g.instant(ctx, backend, name, query, ts)
	}
	return q.Instant(ctx, name, query, ts)
}

func isProbe(name string) bool {
	fam, ok := FamilyOf(Query(name))
	return ok && fam == FamilyProbe
}

func (g *guard) instant(ctx context.Context, backend, name, query string, ts time.Time) (model.Vector, error) {
	if isProbe(name) {
		return g.inner.Instant(ctx, name, query, ts)
	}
	if g.cache == nil {
		return g.fetch(ctx, backend, name, query, ts)
	}

	key := cacheKey{store: g.store, query: query, ts: ts.UnixNano()}
	if v, ok := g.cache.get(key); ok {
		g.cm.IncCacheHit()
		return v, nil
	}
	g.cm.IncCacheMiss()

	flightKey := query + "\x00" + strconv.FormatInt(key.ts, 10)
	// Two attempts: a waiter whose shared call died of the LEADER's context
	// (not its own) retries once, becoming the leader itself.
	for attempt := 0; ; attempt++ {
		var mine, reused atomic.Bool
		ch := g.flight.DoChan(flightKey, func() (any, error) {
			mine.Store(true)
			// A flight that completed between our lookup and this join has
			// already been forgotten by the group but left its entry behind:
			// a flight puts before the group forgets its key, so this lookup
			// sees it and no second upstream query is issued.
			if v, ok := g.cache.get(key); ok {
				reused.Store(true)
				return v, nil
			}
			v, err := g.fetch(ctx, backend, name, query, ts)
			if err == nil {
				g.cache.put(key, v)
			}
			return v, err
		})
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("prom query %s: %w", name, ctx.Err())
		case r := <-ch:
			led := mine.Load()
			if r.Err != nil {
				if !led && attempt == 0 && ctx.Err() == nil && isContextErr(r.Err) {
					continue
				}
				return nil, r.Err
			}
			if !led || reused.Load() {
				g.cm.IncCacheCoalesced()
			}
			v, _ := r.Val.(model.Vector)
			return cloneVector(v), nil
		}
	}
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// cloneVector returns a fresh slice header over the same samples; a nil vector
// stays nil so the uncached path's shape is preserved.
func cloneVector(v model.Vector) model.Vector {
	if v == nil {
		return nil
	}
	out := make(model.Vector, len(v))
	copy(out, v)
	return out
}

// fetch acquires the store's slot and issues the real query.
func (g *guard) fetch(ctx context.Context, backend, name, query string, ts time.Time) (model.Vector, error) {
	if g.sem != nil {
		start := time.Now()
		if err := g.sem.Acquire(ctx, 1); err != nil {
			return nil, fmt.Errorf("prom query %s: waiting for upstream slot: %w", name, err)
		}
		defer g.sem.Release(1)
		wait := time.Since(start)
		g.limiter.ObserveSlotWait(backend, wait.Seconds())
		ctx = withSlotWait(ctx, wait)
	}
	g.limiter.AddInflight(backend, 1)
	defer g.limiter.AddInflight(backend, -1)
	return g.inner.Instant(ctx, name, query, ts)
}

type slotWaitKey struct{}

func withSlotWait(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, slotWaitKey{}, d)
}

// slotWaitFrom returns the time the current query waited for its store slot.
func slotWaitFrom(ctx context.Context) time.Duration {
	d, _ := ctx.Value(slotWaitKey{}).(time.Duration)
	return d
}
