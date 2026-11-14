package promql

import (
	"container/list"
	"crypto/sha256"
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
// credential; digest is the SHA-256 of the rendered query, so a resident entry
// costs 32 bytes of key however long the query was; ts is the evaluation
// instant in Unix nanoseconds. The query NAME is deliberately absent — two
// names rendering the same string share a result.
//
// queryBytes is the rendered query's length. It is a function of the digest's
// input, so it never splits two keys the digest would merge; it rides on the
// key so put can charge it without a second parameter.
type cacheKey struct {
	store      uint64
	digest     [sha256.Size]byte
	ts         int64
	queryBytes int
}

// cacheKeyFor composes the key one query at one instant is cached under.
func cacheKeyFor(store uint64, query string, ts time.Time) cacheKey {
	return cacheKey{
		store:      store,
		digest:     sha256.Sum256([]byte(query)),
		ts:         ts.UnixNano(),
		queryBytes: len(query),
	}
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
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	now   func() time.Time
	ll    *list.List // front = most recently used
	items map[cacheKey]*list.Element
	// size is the budget charged (entryCost summed); series is what the
	// series gauge reports — the two differ by the query-length charge.
	size    int
	series  int
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

// queryBytesPerUnit is how many bytes of rendered query cost one budget unit,
// the same unit a resident series costs. A series' label set is of the order of
// a kilobyte, so the budget stays a rough proxy for resident memory.
const queryBytesPerUnit = 1024

// entryCost charges an empty result one unit, so empty entries still consume
// budget and cannot grow the map without bound, plus one unit per started
// queryBytesPerUnit of the query the key stands for. The key itself is a fixed
// 32-byte digest; charging the query length bounds how many entries a burst of
// long, distinct, empty-result queries can pin, which is the shape an
// adversarial caller would use.
func entryCost(k cacheKey, v model.Vector) int {
	return max(1, len(v)) + k.queryBytes/queryBytesPerUnit
}

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
		c.metrics.SetCacheSeries(c.series)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return slices.Clone(e.vec), true
}

// put inserts v under k. It first releases expired entries from the
// least-recently-used end, then evicts live least-recently-used entries until
// the new one fits. A result larger than the whole budget is not cached.
func (c *queryCache) put(k cacheKey, v model.Vector) {
	if c == nil {
		return
	}
	cost := entryCost(k, v)
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
	now := c.now()
	c.sweepExpiredLocked(now)
	for c.size+cost > c.max {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.removeLocked(back)
		c.metrics.IncCacheEviction()
	}
	e := &cacheEntry{key: k, vec: v, cost: cost, expires: now.Add(c.ttl)}
	c.items[k] = c.ll.PushFront(e)
	c.size += cost
	c.series += len(v)
	c.metrics.SetCacheSeries(c.series)
}

// sweepExpiredLocked removes expired entries from the least-recently-used end,
// stopping at the first live one. Without it an entry nobody reads again
// stays resident until budget pressure evicts it, so a burst of one-off
// queries would stay pinned long after its TTL. The walk is bounded by what it
// frees: an expired entry read since its insertion sits nearer the front and
// is released by its next get, or by eviction. Expiry is not counted as an
// eviction, which stays a budget-pressure signal.
func (c *queryCache) sweepExpiredLocked(now time.Time) {
	for back := c.ll.Back(); back != nil; {
		if now.Before(back.Value.(*cacheEntry).expires) {
			return
		}
		prev := back.Prev()
		c.removeLocked(back)
		back = prev
	}
}

func (c *queryCache) removeLocked(el *list.Element) {
	e := el.Value.(*cacheEntry)
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.size -= e.cost
	c.series -= len(e.vec)
}
