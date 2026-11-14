package promql

import (
	"context"
	"time"
)

// probeOutcomeTTL is how long a completed readiness probe's outcome is reused.
// Short enough that readiness still tracks the live upstream (a kubelet probes
// every 10s by default), long enough that a burst of unauthenticated /readyz
// requests is answered by one probe of each backend.
const probeOutcomeTTL = time.Second

// probeFlightKey is the one key readiness probes coalesce under: every probe
// asks the same backends the same question.
const probeFlightKey = "probe"

// probeOutcome is a completed readiness probe, remembered for probeOutcomeTTL.
// It is tied to the routing snapshot it probed, so a reload is never answered
// with the previous table's outcome.
type probeOutcome struct {
	st  *routerState
	err error
	at  time.Time
}

func (r *Router) now() time.Time {
	if r.guard.now != nil {
		return r.guard.now()
	}
	return time.Now()
}

// probeShared answers a readiness probe from a fresh-enough outcome, or by
// joining (or starting) the one in-flight probe.
//
// The shared probe runs under context.WithoutCancel of the caller that started
// it, bounded by that caller's deadline, so one caller leaving does not fail
// the others; every caller waits on its own context. An outcome the shared
// probe's own deadline cut short is not remembered: it says how long the
// starting caller was willing to wait, not whether the upstream is healthy.
//
// A caller whose own context ends first is answered as every backend not
// having answered within its budget — the same ProbeError a probe running
// under that context would have produced — so /readyz keeps naming backends.
func (r *Router) probeShared(ctx context.Context, ts time.Time) error {
	st := r.state.Load()
	r.probeMu.Lock()
	if last := r.probeLast; last != nil && last.st == st && r.now().Sub(last.at) < probeOutcomeTTL {
		r.probeMu.Unlock()
		return last.err
	}
	r.probeMu.Unlock()

	ch := r.probeFlight.DoChan(probeFlightKey, func() (any, error) {
		pctx := context.WithoutCancel(ctx)
		if d, ok := ctx.Deadline(); ok {
			var cancel context.CancelFunc
			pctx, cancel = context.WithDeadline(pctx, d)
			defer cancel()
		}
		err := r.probeBackends(pctx, st, ts)
		if pctx.Err() == nil {
			r.probeMu.Lock()
			r.probeLast = &probeOutcome{st: st, err: err, at: r.now()}
			r.probeMu.Unlock()
		}
		return nil, err
	})
	select {
	case <-ctx.Done():
		return &ProbeError{Failed: backendNames(st.table.Select(FamilyProbe, nil))}
	case res := <-ch:
		return res.Err
	}
}
