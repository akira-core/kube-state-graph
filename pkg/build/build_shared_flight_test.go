package build

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// foreignCancelQuerier answers every service-graph query with a
// context.Canceled for its first cancelled calls while the caller's context
// stays alive — what a promql guard hands a waiter whose shared flights were
// led, twice, by builds that cancelled their own prefetch.
type foreignCancelQuerier struct {
	fixtures map[promql.Query]model.Vector
	cancels  int // service-graph calls answered with Canceled before answering

	mu      sync.Mutex
	sgCalls int
}

func (q *foreignCancelQuerier) Instant(ctx context.Context, name, _ string, _ time.Time) (model.Vector, error) {
	if strings.HasPrefix(name, "traces_service_graph_") {
		q.mu.Lock()
		q.sgCalls++
		n := q.sgCalls
		q.mu.Unlock()
		if n <= q.cancels {
			return nil, fmt.Errorf("prom query %s: %w", name, context.Canceled)
		}
	}
	return q.fixtures[promql.Query(name)], nil
}

// A build whose own context is alive is never a client cancellation. When
// another build's cancellation reaches its prefetch through a shared flight,
// it fetches the service graph again under its own context and succeeds —
// including the two OPTIONAL RED companions, which would otherwise degrade
// silently on a healthy request.
func TestBuild_ForeignCancellationOfPrefetchIsRefetched(t *testing.T) {
	q := &foreignCancelQuerier{fixtures: syntheticEstate(3), cancels: 3}

	g, err := New(q, Options{}, nil, nil).Build(t.Context(), 5*time.Minute, probeTestEnd, promql.Selector{})

	require.NoError(t, err, "a live build is not cancelled by another build's prefetch")
	assert.NotEmpty(t, g.Edges)
	q.mu.Lock()
	defer q.mu.Unlock()
	assert.Equal(t, 6, q.sgCalls, "the three service-graph queries are issued once more, and only once")
}

// A refetch that is handed a cancellation again still does not answer 499:
// the build's own context is alive, so the failure is the upstream's.
func TestBuild_ForeignCancellationIsNeverReasonCanceled(t *testing.T) {
	q := &foreignCancelQuerier{fixtures: syntheticEstate(3), cancels: 1 << 30}

	_, err := New(q, Options{}, nil, nil).Build(t.Context(), 5*time.Minute, probeTestEnd, promql.Selector{})

	require.Error(t, err)
	be, ok := errors.AsType[*Error](err)
	require.True(t, ok, "a typed build error, got %v", err)
	assert.Equal(t, ReasonUpstream, be.Reason, "a context.Canceled the build did not cause is an upstream failure")
	q.mu.Lock()
	defer q.mu.Unlock()
	assert.Equal(t, 6, q.sgCalls, "one refetch, not a loop")
}

// flightUpstream is the one store several builds share through promql.Guard.
// A service-graph query blocks until release is closed or its (leader's)
// context ends, and announces itself on led — the guard issues it only for a
// flight's leader. Every other query answers at once from fixtures.
type flightUpstream struct {
	fixtures map[promql.Query]model.Vector
	release  chan struct{}
	led      chan struct{}
}

func (u *flightUpstream) Instant(ctx context.Context, name, _ string, _ time.Time) (model.Vector, error) {
	if strings.HasPrefix(name, "traces_service_graph_") {
		u.led <- struct{}{}
		select {
		case <-u.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return u.fixtures[promql.Query(name)], nil
}

// buildView is one build's view of the shared guard: it announces each
// service-graph query it hands the guard on entered, and, with fail set, its
// kube_pod_info fails once fail is closed without reaching the guard — a
// topology failure that is this build's alone while its service-graph queries
// share the guard's flights with every other build.
type buildView struct {
	shared  promql.Querier
	entered chan struct{}
	fail    <-chan struct{}
}

func (v buildView) Instant(ctx context.Context, name, query string, at time.Time) (model.Vector, error) {
	if v.fail != nil && promql.Query(name) == promql.QPodInfo {
		select {
		case <-v.fail:
			return nil, errors.New("topology store 503")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if v.entered != nil && strings.HasPrefix(name, "traces_service_graph_") {
		v.entered <- struct{}{}
	}
	return v.shared.Instant(ctx, name, query, at)
}

func receiveN(t *testing.T, ch <-chan struct{}, n int, what string) {
	t.Helper()
	for i := range n {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: got %d of %d", what, i, n)
		}
	}
}

// The service-graph queries carry no request matcher, so concurrent unfiltered
// builds at one aligned instant share each one's flight in the guard. Build A
// leads them and fails its topology; its prefetch is cancelled, but the guard's
// refcounted flights stay alive while B and C still wait, so no flight ends in
// context.Canceled and nobody re-leads. When C then fails its topology too, B
// is the last waiter and is healthy: it must answer 200.
func TestBuild_SharedServiceGraphFlightLeaderFailureDoesNotCancelWaiter(t *testing.T) {
	const sgQueries = 3
	settle := func() { time.Sleep(50 * time.Millisecond) } // let a waiter register its join

	up := &flightUpstream{
		fixtures: syntheticEstate(3),
		release:  make(chan struct{}),
		led:      make(chan struct{}, 64),
	}
	shared := promql.Guard(up)
	end, window := probeTestEnd, 5*time.Minute

	run := func(q promql.Querier) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := New(q, Options{}, nil, nil).Build(t.Context(), window, end, promql.Selector{})
			done <- err
		}()
		return done
	}

	failA, failC := make(chan struct{}), make(chan struct{})
	enteredB, enteredC := make(chan struct{}, 64), make(chan struct{}, 64)

	errA := run(buildView{shared: shared, fail: failA})
	receiveN(t, up.led, sgQueries, "A leads every service-graph flight")

	errB := run(buildView{shared: shared, entered: enteredB})
	receiveN(t, enteredB, sgQueries, "B joins")
	errC := run(buildView{shared: shared, entered: enteredC, fail: failC})
	receiveN(t, enteredC, sgQueries, "C joins")
	settle()

	close(failA)
	require.Equal(t, ReasonUpstream, AsReason(<-errA), "A fails on its own topology")
	settle()
	assert.Empty(t, up.led, "the flights outlive their first leader: no waiter re-leads")

	close(failC)
	require.Equal(t, ReasonUpstream, AsReason(<-errC), "C fails on its own topology")
	settle()
	assert.Empty(t, up.led, "B still holds every flight open")

	close(up.release)
	select {
	case err := <-errB:
		require.NoError(t, err, "a healthy build sharing a failed build's flight is not cancelled")
	case <-time.After(5 * time.Second):
		t.Fatal("B did not return")
	}
}
