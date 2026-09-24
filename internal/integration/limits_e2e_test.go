package integration

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// issueCounter counts the upstream queries each backend store actually
// received, keyed by (backend, rendered query, instant), and the peak number
// in flight at once.
type issueCounter struct {
	mu       sync.Mutex
	issued   map[string]int
	inflight int
	peak     int
}

type countedQuerier struct {
	backend string
	inner   promql.Querier
	c       *issueCounter
}

func (q countedQuerier) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	if name == string(promql.QUpProbe) {
		return q.inner.Instant(ctx, name, query, ts) // bypasses the guard; not counted
	}
	q.c.mu.Lock()
	q.c.issued[q.backend+"\x00"+query+"\x00"+ts.String()]++
	q.c.inflight++
	q.c.peak = max(q.c.peak, q.c.inflight)
	q.c.mu.Unlock()
	defer func() { q.c.mu.Lock(); q.c.inflight--; q.c.mu.Unlock() }()
	return q.inner.Instant(ctx, name, query, ts)
}

func (c *issueCounter) wrap(b promql.Backend, q promql.Querier) promql.Querier {
	return countedQuerier{backend: b.Name(), inner: q, c: c}
}

// concurrentGraphs issues n identical /v1/graph requests at once and returns
// their status codes and bodies.
func (s *MultiBackendSuite) concurrentGraphs(base string, n int) ([]int, []string) {
	s.T().Helper()
	target := s.graphURL(base, inventory)
	codes, bodies := make([]int, n), make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			req, err := http.NewRequestWithContext(s.T().Context(), http.MethodGet, target, nil)
			if err != nil {
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			codes[i], bodies[i] = resp.StatusCode, string(b)
		})
	}
	wg.Wait()
	return codes, bodies
}

// Concurrent identical requests against real VictoriaMetrics: the default
// cache + coalescing sends each distinct query to each store ONCE, and every
// response body is identical.
func (s *MultiBackendSuite) TestUpstreamLimitAndCache() {
	counter := &issueCounter{issued: map[string]int{}}
	srv := s.startRoutedAPIWith(s.familySplitBackends(), counter.wrap)

	codes, bodies := s.concurrentGraphs(srv.URL, 8)
	for i := range codes {
		s.Require().Equal(http.StatusOK, codes[i], bodies[i])
		s.Require().Equal(bodies[0], bodies[i], "every concurrent body is identical")
	}
	s.Require().Contains(bodies[0], "mb-uid-1", "a vacuous graph would prove nothing")

	counter.mu.Lock()
	defer counter.mu.Unlock()
	s.Require().NotEmpty(counter.issued)
	for key, n := range counter.issued {
		s.Equalf(1, n, "query issued %d times to its store: %q", n, key)
	}
}

// A limit of 1 serialises every store's queries yet every request still
// completes within the build deadline.
func (s *MultiBackendSuite) TestUpstreamLimitOneStillServes() {
	counter := &issueCounter{issued: map[string]int{}}
	srv := s.startRoutedAPIWith(s.familySplitBackends(), counter.wrap,
		promql.WithMaxConcurrency(1), promql.WithQueryCache(0, 0))

	codes, bodies := s.concurrentGraphs(srv.URL, 4)
	for i := range codes {
		s.Require().Equal(http.StatusOK, codes[i], bodies[i])
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	// Two stores, one slot each: at most one query in flight per store, so at
	// most two across the process.
	s.LessOrEqual(counter.peak, 2)
}
