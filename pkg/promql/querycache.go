package promql

import (
	"container/list"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"
)

// Defaults shared by the Router, Guard and the server's flag defaults, so an
// embedder that writes no option gets exactly what the server runs with.
const (
	// DefaultMaxConcurrency bounds the upstream queries in flight to one
	// backend store.
	DefaultMaxConcurrency = 32
	// DefaultQueryCacheMaxSeries is the query-result cache's total
	// resident-series budget.
	DefaultQueryCacheMaxSeries = 100000
	// DefaultQueryCacheTTL is how long a cached result may be served.
	DefaultQueryCacheTTL = 60 * time.Second
)

// CacheMetrics is the OPTIONAL upgrade interface a Metrics implementation may
// satisfy to observe the query-result cache. Separate from Metrics for the same
// reason RouterMetrics is: widening an exported interface breaks embedders.
type CacheMetrics interface {
	IncCacheHit()
	IncCacheMiss()
	// IncCacheCoalesced counts a caller that missed and was answered by
	// another caller's upstream query instead of issuing its own.
	IncCacheCoalesced()
	IncCacheEviction()
	// SetCacheSeries records the series currently resident in the cache.
	SetCacheSeries(n int)
}

func cacheMetricsOf(m Metrics) CacheMetrics {
	if cm, ok := m.(CacheMetrics); ok && cm != nil {
		return cm
	}
	return noopCacheMetrics{}
}

type noopCacheMetrics struct{}

func (noopCacheMetrics) IncCacheHit()       {}
func (noopCacheMetrics) IncCacheMiss()      {}
func (noopCacheMetrics) IncCacheCoalesced() {}
func (noopCacheMetrics) IncCacheEviction()  {}
func (noopCacheMetrics) SetCacheSeries(int) {}

// cacheKey identifies one upstream result. store is a per-Router id standing
// for a backend store's (url, credentials) identity, so the key never holds a
// credential; ts is the evaluation instant in Unix nanoseconds. The query NAME
// is deliberately absent — two names rendering the same string share a result.
type cacheKey struct {
	store uint64
	query string
	ts    int64
}

type cacheEntry struct {
	key     cacheKey
	vec     model.Vector
	cost    int
	expires time.Time
}

// queryCache is a least-recently-used cache of upstream results bounded by the
// total number of resident series. A nil *queryCache is a disabled cache: get
// always misses and put does nothing.
//
// Cached vectors are shared read-only: get hands out a fresh slice header over
// the same *model.Sample pointers, so a reader may append to or reorder its
// vector but MUST NOT mutate a sample or its label set.
type queryCache struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	now     func() time.Time
	ll      *list.List // front = most recently used
	items   map[cacheKey]*list.Element
	size    int
	metrics CacheMetrics
}

// newQueryCache returns nil when maxSeries <= 0, disabling the cache.
func newQueryCache(maxSeries int, ttl time.Duration, m CacheMetrics, now func() time.Time) *queryCache {
	if maxSeries <= 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = DefaultQueryCacheTTL
	}
	if now == nil {
		now = time.Now
	}
	if m == nil {
		m = noopCacheMetrics{}
	}
	return &queryCache{
		max:     maxSeries,
		ttl:     ttl,
		now:     now,
		ll:      list.New(),
		items:   make(map[cacheKey]*list.Element),
		metrics: m,
	}
}

// entryCost charges an empty result one unit, so empty entries still consume
// budget and cannot grow the map without bound.
func entryCost(v model.Vector) int { return max(1, len(v)) }

func (c *queryCache) get(k cacheKey) (model.Vector, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	e := el.Value.(*cacheEntry)
	if !c.now().Before(e.expires) {
		c.removeLocked(el)
		c.metrics.SetCacheSeries(c.size)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return slices.Clone(e.vec), true
}

// put inserts v under k, evicting least-recently-used entries until it fits.
// A result larger than the whole budget is not cached.
func (c *queryCache) put(k cacheKey, v model.Vector) {
	if c == nil {
		return
	}
	cost := entryCost(v)
	if cost > c.max {
		return
	}
	if v == nil {
		v = model.Vector{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.removeLocked(el)
	}
	for c.size+cost > c.max {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.removeLocked(back)
		c.metrics.IncCacheEviction()
	}
	e := &cacheEntry{key: k, vec: v, cost: cost, expires: c.now().Add(c.ttl)}
	c.items[k] = c.ll.PushFront(e)
	c.size += cost
	c.metrics.SetCacheSeries(c.size)
}

func (c *queryCache) removeLocked(el *list.Element) {
	e := el.Value.(*cacheEntry)
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.size -= e.cost
}
