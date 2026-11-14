package promql

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeFake counts probe queries and answers per its current mode: ok,
// failing, or blocking until the probe's context ends.
type probeFake struct {
	calls   atomic.Int64
	fail    atomic.Bool
	block   atomic.Bool
	release chan struct{} // nil: answer at once
	queries sync.Map
}

func (p *probeFake) Instant(ctx context.Context, _, query string, _ time.Time) (model.Vector, error) {
	p.calls.Add(1)
	p.queries.Store(query, true)
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.block.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if p.fail.Load() {
		return nil, errors.New("connection refused")
	}
	return model.Vector{sample("", nil, 3)}, nil
}

func probeRouter(t *testing.T, clk *fakeClock, fakes map[string]*probeFake) *Router {
	t.Helper()
	r, err := NewRouter(twoZoneTable(t), nil, func(b Backend) (Querier, error) {
		return fakes[b.Name()], nil
	}, withClock(clk.now))
	require.NoError(t, err)
	return r
}

func TestProbeAll_SendsCountUp(t *testing.T) {
	a, b := &probeFake{}, &probeFake{}
	r := probeRouter(t, &fakeClock{t: time.Unix(0, 0)}, map[string]*probeFake{"zone-a": a, "zone-b": b})
	require.NoError(t, r.ProbeAll(t.Context(), ts0))
	_, ok := a.queries.Load("count(up)")
	assert.True(t, ok, "the probe asks for one series per store")
}

// A burst of concurrent readiness probes costs each backend one probe query.
func TestProbeAll_ConcurrentCallsShareOneProbe(t *testing.T) {
	a := &probeFake{release: make(chan struct{})}
	b := &probeFake{release: a.release}
	r := probeRouter(t, &fakeClock{t: time.Unix(0, 0)}, map[string]*probeFake{"zone-a": a, "zone-b": b})

	const n = 16
	errs := make(chan error, n)
	for range n {
		go func() { errs <- r.ProbeAll(t.Context(), ts0) }()
	}
	require.Eventually(t, func() bool { return a.calls.Load() == 1 && b.calls.Load() == 1 }, time.Second, time.Millisecond)
	time.Sleep(10 * time.Millisecond) // let the rest of the burst join
	close(a.release)
	for range n {
		require.NoError(t, <-errs)
	}
	assert.EqualValues(t, 1, a.calls.Load())
	assert.EqualValues(t, 1, b.calls.Load())
}

// An outcome — success or failure — is reused for one second, then the probe
// is live again.
func TestProbeAll_OutcomeReusedForOneSecond(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	a, b := &probeFake{}, &probeFake{}
	r := probeRouter(t, clk, map[string]*probeFake{"zone-a": a, "zone-b": b})

	require.NoError(t, r.ProbeAll(t.Context(), ts0))
	b.fail.Store(true)
	clk.advance(999 * time.Millisecond)
	require.NoError(t, r.ProbeAll(t.Context(), ts0), "within the TTL the last outcome is reused")
	assert.EqualValues(t, 1, b.calls.Load())

	clk.advance(time.Millisecond)
	err := r.ProbeAll(t.Context(), ts0)
	var pe *ProbeError
	require.ErrorAs(t, err, &pe, "after the TTL the probe is live and sees the failure")
	assert.Equal(t, []string{"zone-b"}, pe.Failed)

	b.fail.Store(false)
	require.ErrorAs(t, r.ProbeAll(t.Context(), ts0), &pe, "a failure is reused too")
	assert.EqualValues(t, 2, b.calls.Load())
}

// A probe cut short by its own deadline says how long its caller waited, not
// whether the store is healthy, so it is never reused.
func TestProbeAll_TimedOutOutcomeNotReused(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	a, b := &probeFake{}, &probeFake{}
	r := probeRouter(t, clk, map[string]*probeFake{"zone-a": a, "zone-b": b})

	b.block.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var pe *ProbeError
	require.ErrorAs(t, r.ProbeAll(ctx, ts0), &pe)
	assert.Contains(t, pe.Failed, "zone-b")

	b.block.Store(false)
	require.Eventually(t, func() bool { return r.ProbeAll(t.Context(), ts0) == nil }, time.Second, time.Millisecond)
	assert.EqualValues(t, 2, b.calls.Load(), "the timed-out outcome was not reused")
}

// A caller that leaves does not fail the probe for another caller sharing it.
func TestProbeAll_CallerLeavingDoesNotFailOthers(t *testing.T) {
	a := &probeFake{release: make(chan struct{})}
	b := &probeFake{release: a.release}
	r := probeRouter(t, &fakeClock{t: time.Unix(0, 0)}, map[string]*probeFake{"zone-a": a, "zone-b": b})

	first, cancelFirst := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() { firstDone <- r.ProbeAll(first, ts0) }()
	require.Eventually(t, func() bool { return a.calls.Load() == 1 }, time.Second, time.Millisecond)
	secondDone := make(chan error, 1)
	go func() { secondDone <- r.ProbeAll(t.Context(), ts0) }()
	time.Sleep(10 * time.Millisecond)

	cancelFirst()
	var pe *ProbeError
	require.ErrorAs(t, <-firstDone, &pe, "the leaving caller is told no backend answered in its budget")
	assert.Equal(t, []string{"zone-a", "zone-b"}, pe.Failed)

	close(a.release)
	require.NoError(t, <-secondDone)
	assert.EqualValues(t, 1, a.calls.Load())
}

// A reload is never answered with the previous table's outcome.
func TestProbeAll_SwapInvalidatesTheOutcome(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	a, b := &probeFake{}, &probeFake{}
	r := probeRouter(t, clk, map[string]*probeFake{"zone-a": a, "zone-b": b})
	require.NoError(t, r.ProbeAll(t.Context(), ts0))

	require.NoError(t, r.Swap(twoZoneTable(t)))
	require.NoError(t, r.ProbeAll(t.Context(), ts0))
	assert.EqualValues(t, 2, a.calls.Load())
}
