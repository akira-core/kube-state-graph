package promql

import (
	"crypto/sha256"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recordingMetrics records every optional-upgrade observation the guard and
// cache emit, plus the base Metrics / SeriesMetrics ones.
type recordingMetrics struct {
	mu                                      sync.Mutex
	hits, misses, coalesced, evictions      int
	series                                  int
	inflight                                map[string]int
	maxInflight                             map[string]int
	waits                                   map[string]int
	durations, failures, seriesObservations int
}

func newRecordingMetrics() *recordingMetrics {
	return &recordingMetrics{inflight: map[string]int{}, maxInflight: map[string]int{}, waits: map[string]int{}}
}

func (m *recordingMetrics) ObserveQueryDuration(string, float64) {
	m.mu.Lock()
	m.durations++
	m.mu.Unlock()
}
func (m *recordingMetrics) IncQueryFailure(string) { m.mu.Lock(); m.failures++; m.mu.Unlock() }
func (m *recordingMetrics) ObserveQuerySeries(string, int) {
	m.mu.Lock()
	m.seriesObservations++
	m.mu.Unlock()
}
func (m *recordingMetrics) IncCacheHit()       { m.mu.Lock(); m.hits++; m.mu.Unlock() }
func (m *recordingMetrics) IncCacheMiss()      { m.mu.Lock(); m.misses++; m.mu.Unlock() }
func (m *recordingMetrics) IncCacheCoalesced() { m.mu.Lock(); m.coalesced++; m.mu.Unlock() }
func (m *recordingMetrics) IncCacheEviction()  { m.mu.Lock(); m.evictions++; m.mu.Unlock() }
func (m *recordingMetrics) SetCacheSeries(n int) {
	m.mu.Lock()
	m.series = n
	m.mu.Unlock()
}
func (m *recordingMetrics) AddInflight(b string, d int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inflight[b] += d
	m.maxInflight[b] = max(m.maxInflight[b], m.inflight[b])
}
func (m *recordingMetrics) ObserveSlotWait(b string, _ float64) {
	m.mu.Lock()
	m.waits[b]++
	m.mu.Unlock()
}

func vecN(n int) model.Vector {
	v := make(model.Vector, n)
	for i := range v {
		v[i] = sample("m", map[string]string{"i": string(rune('a' + i%26))}, float64(i))
	}
	return v
}

func key(q string) cacheKey { return cacheKeyFor(1, q, time.Unix(0, 0)) }

func TestQueryCache_HitMissAndTTL(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	m := newRecordingMetrics()
	c := newQueryCache(100, time.Minute, m, clk.now)

	_, ok := c.get(key("a"))
	assert.False(t, ok, "empty cache misses")

	c.put(key("a"), vecN(3))
	got, ok := c.get(key("a"))
	require.True(t, ok)
	assert.Len(t, got, 3)
	assert.Equal(t, 3, m.series)

	_, ok = c.get(cacheKeyFor(1, "a", time.Unix(0, 1)))
	assert.False(t, ok, "a different evaluation instant is a different key")
	_, ok = c.get(cacheKeyFor(2, "a", time.Unix(0, 0)))
	assert.False(t, ok, "a different store is a different key")

	clk.advance(time.Minute)
	_, ok = c.get(key("a"))
	assert.False(t, ok, "an entry at its TTL is expired")
	assert.Equal(t, 0, m.series, "expired entry released its budget")
}

func TestQueryCache_LRUEviction(t *testing.T) {
	m := newRecordingMetrics()
	c := newQueryCache(1000, time.Minute, m, nil)
	c.put(key("A"), vecN(600))
	c.put(key("B"), vecN(300))
	_, ok := c.get(key("A")) // A now more recent than B
	require.True(t, ok)

	c.put(key("C"), vecN(300))
	_, okA := c.get(key("A"))
	_, okB := c.get(key("B"))
	_, okC := c.get(key("C"))
	assert.True(t, okA)
	assert.False(t, okB, "least-recently-used entry evicted")
	assert.True(t, okC)
	assert.Equal(t, 1, m.evictions)
	assert.Equal(t, 900, m.series)
}

func TestQueryCache_OversizedResultNotCached(t *testing.T) {
	c := newQueryCache(10, time.Minute, nil, nil)
	c.put(key("small"), vecN(5))
	c.put(key("big"), vecN(11))
	_, ok := c.get(key("big"))
	assert.False(t, ok)
	_, ok = c.get(key("small"))
	assert.True(t, ok, "an oversized insert evicts nothing")
}

func TestQueryCache_EmptyResultCachedAndCharged(t *testing.T) {
	m := newRecordingMetrics()
	c := newQueryCache(10, time.Minute, m, nil)
	c.put(key("empty"), model.Vector{})
	got, ok := c.get(key("empty"))
	require.True(t, ok)
	assert.Empty(t, got)
	assert.Equal(t, 1, c.size, "an empty result costs one unit of budget")
	assert.Zero(t, m.series, "but holds no series")
}

func TestQueryCache_ZeroBudgetDisables(t *testing.T) {
	c := newQueryCache(0, time.Minute, nil, nil)
	assert.Nil(t, c)
	c.put(key("a"), vecN(1)) // nil-safe
	_, ok := c.get(key("a"))
	assert.False(t, ok)
}

func TestQueryCache_HitReturnsFreshSliceHeader(t *testing.T) {
	c := newQueryCache(100, time.Minute, nil, nil)
	c.put(key("a"), vecN(2))
	first, _ := c.get(key("a"))
	first[0] = sample("other", nil, 99)
	_ = append(first, sample("extra", nil, 1))

	second, _ := c.get(key("a"))
	require.Len(t, second, 2)
	assert.Equal(t, model.LabelValue("m"), second[0].Metric[model.MetricNameLabel],
		"a reader's slice writes never reach the cached entry")
}

// The key holds a fixed-size digest of the query, never the query itself, and
// two different queries never share an entry.
func TestQueryCache_KeyIsADigestOfTheQuery(t *testing.T) {
	long := strings.Repeat("x", 16<<10)
	k := cacheKeyFor(1, long, time.Unix(0, 0))
	assert.Equal(t, sha256.Sum256([]byte(long)), k.digest)
	assert.Equal(t, len(long), k.queryBytes)

	c := newQueryCache(100, time.Minute, nil, nil)
	c.put(key(`up{a="1"}`), vecN(1))
	c.put(key(`up{a="2"}`), vecN(2))
	a, ok := c.get(key(`up{a="1"}`))
	require.True(t, ok)
	b, ok := c.get(key(`up{a="2"}`))
	require.True(t, ok)
	assert.Len(t, a, 1)
	assert.Len(t, b, 2, "distinct queries keep distinct entries")
}

// A long query is charged for its length on top of its series, so a burst of
// long, distinct, empty-result queries cannot pin one entry per budget unit.
func TestQueryCache_LongQueryChargedForItsLength(t *testing.T) {
	m := newRecordingMetrics()
	c := newQueryCache(100, time.Minute, m, nil)
	long := strings.Repeat("x", 16<<10)
	c.put(key(long), model.Vector{})
	assert.Equal(t, 1+16, c.size, "one unit for the empty result plus one per KiB of query")
	assert.Zero(t, m.series, "the series gauge reports series, not budget")

	c.put(key(long+"2"), vecN(3))
	assert.Equal(t, 17+3+16, c.size)
	assert.Equal(t, 3, m.series)

	// 16 KiB queries with empty results cost 17 units each: 100 units hold five.
	for i := range 10 {
		c.put(key(long+strconv.Itoa(i+10)), model.Vector{})
	}
	assert.LessOrEqual(t, c.size, 100)
	assert.Equal(t, 5, c.ll.Len())
}

// put releases expired entries from the least-recently-used end even when the
// budget has room, so a burst of one-off queries does not stay resident after
// its TTL until budget pressure happens to evict it.
func TestQueryCache_PutSweepsExpiredEntries(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	m := newRecordingMetrics()
	c := newQueryCache(1000, time.Minute, m, clk.now)
	for i := range 5 {
		c.put(key("burst"+strconv.Itoa(i)), vecN(10))
	}
	require.Equal(t, 50, m.series)

	clk.advance(time.Minute)
	c.put(key("fresh"), vecN(2))
	assert.Equal(t, 1, c.ll.Len(), "every expired entry was released")
	assert.Equal(t, 2, c.size)
	assert.Equal(t, 2, m.series)
	assert.Zero(t, m.evictions, "expiry is not counted as budget eviction")
	_, ok := c.get(key("fresh"))
	assert.True(t, ok)
}

// The sweep stops at the first live entry from the tail and never releases a
// live entry.
func TestQueryCache_SweepKeepsLiveEntries(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	c := newQueryCache(1000, time.Minute, nil, clk.now)
	c.put(key("old"), vecN(1))
	clk.advance(30 * time.Second)
	c.put(key("mid"), vecN(1))
	clk.advance(30 * time.Second) // old expired, mid live
	c.put(key("new"), vecN(1))

	assert.Equal(t, 2, c.ll.Len(), "the expired tail entry was swept")
	_, okMid := c.get(key("mid"))
	_, okNew := c.get(key("new"))
	assert.True(t, okMid)
	assert.True(t, okNew)
}
