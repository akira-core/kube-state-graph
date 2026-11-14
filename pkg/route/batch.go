package route

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	envoyroute "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/matchcheck"
)

var _ build.BatchRouteResolver = (*scopedResolver)(nil)

// errBatchPanic marks a request whose batched resolution panicked.
var errBatchPanic = errors.New("route resolution panicked")

// batchItem is one request that reached the matcher stage of a batch, holding
// the cross-build cache lease it MUST finish.
type batchItem struct {
	idx   int
	lease *resultLease
	check *pendingCheck
}

// deferredWait is a request whose key another resolution — in another build —
// is already resolving. It is waited on only after this batch has finished
// every lease it holds: waiting while holding leases could deadlock two
// builds waiting on each other's keys.
type deferredWait struct {
	idx  int
	wait *flight[routeResult]
}

// ResolveRoutes implements build.BatchRouteResolver for one build:
//
//  1. Each request is looked up in the cross-build cache; a miss is prepared —
//     probe, load, gateway pick, listener gate, LB fallback, memoised
//     translation — under limits.Concurrency, each bounded by
//     limits.PerCallTimeout. Every miss that ends before the matcher is
//     answered and cached here.
//  2. The requests that reached the matcher are grouped by RouteConfiguration
//     (rcKey) and each group is matched in ONE router_check_tool run, its
//     queries de-duplicated, each run bounded by its own per-call timeout. A
//     failed run whose context is still live is retried query by query, so a
//     query the tool cannot answer fails only its own request.
//  3. Requests whose key another build was already resolving wait for that
//     resolution, and resolve alone if it failed.
//
// Requests the batch never reached (the context ended first) come back with
// Answered=false. Each answer equals what ResolveRoute returns for that
// request alone. Every goroutine recovers its own panics.
func (s *scopedResolver) ResolveRoutes(ctx context.Context, reqs []build.RouteRequest, limits build.RouteBatchLimits) []build.RouteAnswer {
	answers := make([]build.RouteAnswer, len(reqs))
	conc := max(1, limits.Concurrency)

	// Identical keys inside one batch share one resolution: the first
	// occurrence resolves, the rest copy its answer at the end. (The prescan
	// already dedups, so this only guards the contract — and without it a
	// duplicate would wait on its own batch's lease and deadlock phase 1.)
	first := make(map[resultKey]int, len(reqs))
	dupOf := make([]int, len(reqs))
	for i, req := range reqs {
		k := resultKeyOf(req)
		if j, seen := first[k]; seen {
			dupOf[i] = j
			continue
		}
		first[k] = i
		dupOf[i] = -1
	}

	items := make([]*batchItem, len(reqs))
	waits := make([]*deferredWait, len(reqs))

	// Phase 1: cache lookup + prepare.
	var g errgroup.Group
	g.SetLimit(conc)
	for i, req := range reqs {
		if dupOf[i] >= 0 {
			continue
		}
		if ctx.Err() != nil {
			break // the rest stay unanswered
		}
		g.Go(func() error {
			s.batchPrepare(ctx, limits.PerCallTimeout, i, req, answers, items, waits)
			return nil
		})
	}
	_ = g.Wait()

	// Phase 2: one matcher run per RouteConfiguration.
	s.batchMatch(ctx, conc, limits.PerCallTimeout, reqs, answers, items)

	// Phase 3: requests another build was resolving.
	var g3 errgroup.Group
	g3.SetLimit(conc)
	for _, w := range waits {
		if w == nil || ctx.Err() != nil {
			continue
		}
		g3.Go(func() error {
			s.batchAwait(ctx, limits.PerCallTimeout, w, reqs[w.idx], answers)
			return nil
		})
	}
	_ = g3.Wait()

	for i, j := range dupOf {
		if j >= 0 {
			answers[i] = answers[j]
		}
	}
	return answers
}

// batchPrepare is phase 1 for one request: answer it from the cache, defer it
// behind another build's resolution, or prepare it under a lease — answering
// (and caching) a final miss, or leaving an item for the matcher phase.
func (s *scopedResolver) batchPrepare(ctx context.Context, perCall time.Duration, i int, req build.RouteRequest,
	answers []build.RouteAnswer, items []*batchItem, waits []*deferredWait) {
	val, hit, wait, lease := s.r.results.acquire(resultKeyOf(req))
	switch {
	case hit:
		answers[i] = answered(val, nil)
		return
	case wait != nil:
		waits[i] = &deferredWait{idx: i, wait: wait}
		return
	}

	handedOff := false
	defer func() {
		// A panic (or any path that neither answered nor handed off) must
		// release the lease, or waiters in other builds block on it.
		if rec := recover(); rec != nil {
			logBatchPanic(ctx, rec, 1)
			err := fmt.Errorf("%w: %v", errBatchPanic, rec)
			lease.finish(routeResult{}, err)
			answers[i] = answered(routeResult{}, err)
			return
		}
		if !handedOff {
			lease.finish(routeResult{}, errFlightAbandoned)
		}
	}()

	callCtx, cancel := withPerCall(ctx, perCall)
	defer cancel()
	p, err := s.prepare(callCtx, req)
	if err != nil {
		lease.finish(routeResult{}, err)
		answers[i] = answered(routeResult{}, err)
		handedOff = true
		return
	}
	if p.check == nil {
		res := routeResult{dest: p.dest, outcome: p.outcome}
		lease.finish(res, nil)
		answers[i] = answered(res, nil)
		handedOff = true
		return
	}
	items[i] = &batchItem{idx: i, lease: lease, check: p.check}
	handedOff = true
}

// batchMatch is phase 2: group the items by RouteConfiguration, run one
// matcher invocation per group, and finish every item's lease — including the
// items of groups the context ended before (answered=false).
func (s *scopedResolver) batchMatch(ctx context.Context, conc int, perCall time.Duration, reqs []build.RouteRequest,
	answers []build.RouteAnswer, items []*batchItem) {
	var order []rcKey
	groups := map[rcKey][]*batchItem{}
	for _, it := range items {
		if it == nil {
			continue
		}
		if _, ok := groups[it.check.rcKey]; !ok {
			order = append(order, it.check.rcKey)
		}
		groups[it.check.rcKey] = append(groups[it.check.rcKey], it)
	}

	var g errgroup.Group
	g.SetLimit(conc)
	for _, k := range order {
		if ctx.Err() != nil {
			break
		}
		group := groups[k]
		g.Go(func() error {
			s.matchGroup(ctx, perCall, group, answers)
			return nil
		})
	}
	_ = g.Wait()

	// Groups never run (the context ended first): release their leases; the
	// requests stay unanswered.
	for _, it := range items {
		if it != nil && !it.lease.isDone() {
			it.lease.finish(routeResult{}, context.Cause(ctx))
		}
	}
}

// matchGroup runs one RouteConfiguration's queries in a single matcher
// invocation and answers every item of the group.
func (s *scopedResolver) matchGroup(ctx context.Context, perCall time.Duration, group []*batchItem, answers []build.RouteAnswer) {
	defer func() {
		if rec := recover(); rec != nil {
			logBatchPanic(ctx, rec, len(group))
			err := fmt.Errorf("%w: %v", errBatchPanic, rec)
			for _, it := range group {
				if !it.lease.isDone() {
					it.lease.finish(routeResult{}, err)
					answers[it.idx] = answered(routeResult{}, err)
				}
			}
		}
	}()

	// De-duplicate the group's queries: two keys that differ only in their
	// caller cluster select the same RouteConfiguration and pose the same
	// (host, path).
	var queries []matchcheck.Query
	slot := make([]int, len(group))
	seen := map[matchcheck.Query]int{}
	for j, it := range group {
		q := it.check.query
		n, ok := seen[q]
		if !ok {
			n = len(queries)
			seen[q] = n
			queries = append(queries, q)
		}
		slot[j] = n
	}
	rc := group[0].check.rc

	clusters, err := s.matchWithTimeout(ctx, perCall, rc, queries)
	if err != nil && len(queries) > 1 && ctx.Err() == nil && !isContextErr(err) {
		// The run failed for a reason other than time — router_check_tool
		// reports all-or-nothing, so one query it cannot answer fails the
		// whole run. Retry query by query: each request then fails or
		// succeeds exactly as it would alone.
		s.matchSingly(ctx, perCall, group, answers)
		return
	}
	for j, it := range group {
		if err != nil {
			it.lease.finish(routeResult{}, err)
			answers[it.idx] = answered(routeResult{}, err)
			continue
		}
		dest, outcome := it.check.finish(clusters[slot[j]])
		res := routeResult{dest: dest, outcome: outcome}
		it.lease.finish(res, nil)
		answers[it.idx] = answered(res, nil)
	}
}

// matchSingly is the fallback for a failed group run: one matcher invocation
// per item, each under its own per-call timeout.
func (s *scopedResolver) matchSingly(ctx context.Context, perCall time.Duration, group []*batchItem, answers []build.RouteAnswer) {
	for _, it := range group {
		clusters, err := s.matchWithTimeout(ctx, perCall, it.check.rc, []matchcheck.Query{it.check.query})
		if err != nil {
			it.lease.finish(routeResult{}, err)
			answers[it.idx] = answered(routeResult{}, err)
			continue
		}
		dest, outcome := it.check.finish(clusters[0])
		res := routeResult{dest: dest, outcome: outcome}
		it.lease.finish(res, nil)
		answers[it.idx] = answered(res, nil)
	}
}

func (s *scopedResolver) matchWithTimeout(ctx context.Context, perCall time.Duration, rc *envoyroute.RouteConfiguration, queries []matchcheck.Query) ([]string, error) {
	callCtx, cancel := withPerCall(ctx, perCall)
	defer cancel()
	return s.r.match(callCtx, rc, queries)
}

// batchAwait is phase 3 for one request: wait (bounded by the per-call
// timeout) on the other build's resolution of the same key, and resolve alone
// through the single-request path if that resolution failed.
func (s *scopedResolver) batchAwait(ctx context.Context, perCall time.Duration, w *deferredWait, req build.RouteRequest, answers []build.RouteAnswer) {
	defer func() {
		if rec := recover(); rec != nil {
			logBatchPanic(ctx, rec, 1)
			answers[w.idx] = answered(routeResult{}, fmt.Errorf("%w: %v", errBatchPanic, rec))
		}
	}()
	callCtx, cancel := withPerCall(ctx, perCall)
	defer cancel()
	if v, ok, err := w.wait.wait(callCtx); err != nil || ok {
		answers[w.idx] = answered(v, err)
		return
	}
	dest, outcome, err := s.ResolveRoute(callCtx, req)
	answers[w.idx] = answered(routeResult{dest: dest, outcome: outcome}, err)
}

// answered builds an Answered RouteAnswer, clearing the outcome on error (an
// error carries no outcome).
func answered(v routeResult, err error) build.RouteAnswer {
	if err != nil {
		return build.RouteAnswer{Err: err, Answered: true}
	}
	return build.RouteAnswer{Dest: v.dest, Outcome: v.outcome, Answered: true}
}

func withPerCall(ctx context.Context, perCall time.Duration) (context.Context, context.CancelFunc) {
	if perCall <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, perCall)
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func logBatchPanic(ctx context.Context, rec any, keys int) {
	slog.ErrorContext(ctx, "panic in batched route resolution (affected endpoints degrade to external)",
		"keys", keys, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
}
