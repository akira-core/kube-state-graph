package build

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// qosScopeConcurrency bounds the second-wave QoS reads in flight at once
// (six families × however many chunks the scope was split into). It is a
// site-invariant tuning value like routeResolveConcurrency, not a knob: the
// queries it bounds are already narrow, and the upstream's own limits are the
// backstop that matters.
const qosScopeConcurrency = 8

// signalWhenDone closes done when fn returns, whatever it returns. Closing on
// the error path too is what keeps the scoped QoS read from blocking when a
// prerequisite leg fails: the group's context is cancelled at the same moment,
// and the reader selects on both.
func signalWhenDone(fn func() error, done chan<- struct{}) func() error {
	return func() error {
		defer close(done)
		return fn()
	}
}

// readScopedQoS issues the six Harvest QoS workload queries restricted to the
// FlexVol names the loaded claims actually matched.
//
// It waits on exactly the two families its scope is computed from —
// kube_persistentvolumeclaim_info and volume_labels — rather than on the whole
// first wave, so the Harvest tail is delayed only by what it truly depends on.
//
// An empty scope issues NO query at all. That is not an optimisation but the
// contract: an empty scope means no claim matched any volume-object series, so
// hop A drew no edge and hop B could not have contributed to one. It mirrors
// the rule that a selector loading no pods or services issues no
// traces_service_graph_* queries.
//
// Each chunk degrades on its own (log-and-continue, like every Harvest leg), so
// one failed chunk costs I/O measurements for the claims whose volumes it
// carried and nothing else. Chunk results are merged in CHUNK ORDER, never
// completion order: sumQoSIO adds float64s, so a timing-dependent merge would
// make the last bits of every I/O figure depend on which chunk answered first.
func readScopedQoS(
	ctx, callerCtx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	v *topologyVectors,
	prerequisites ...<-chan struct{},
) error {
	for _, done := range prerequisites {
		select {
		case <-done:
		case <-ctx.Done():
			// A sibling leg failed (or the caller went away). The group already
			// carries that error; adding another would only mask it.
			return nil
		}
	}

	scope := qosVolumeScope(v.PVCInfo, v.VolumeLabels, opts.volumeKey())
	if len(scope) == 0 {
		return nil
	}
	chunks := promql.ChunkQoSVolumeScope(scope, opts.qosScopeBatchBytes())

	targets := []struct {
		query promql.Query
		dst   *model.Vector
	}{
		{promql.QQoSReadOps, &v.QoSReadOps},
		{promql.QQoSWriteOps, &v.QoSWriteOps},
		{promql.QQoSReadLatency, &v.QoSReadLatency},
		{promql.QQoSWriteLatency, &v.QoSWriteLatency},
		{promql.QQoSReadData, &v.QoSReadData},
		{promql.QQoSWriteData, &v.QoSWriteData},
	}

	// One slot per (family, chunk). Writing into a pre-sized slot rather than
	// appending is what makes the merge below order-free.
	parts := make([][]model.Vector, len(targets))
	for i := range parts {
		parts[i] = make([]model.Vector, len(chunks))
	}

	var wave errgroup.Group
	wave.SetLimit(qosScopeConcurrency)
	for ti, t := range targets {
		for ci, chunk := range chunks {
			wave.Go(func() error {
				out, err := instantQoSChunk(ctx, callerCtx, q, t.query, window, end, chunk)
				if err != nil {
					return err
				}
				parts[ti][ci] = out
				return nil
			})
		}
	}
	if err := wave.Wait(); err != nil {
		return err
	}

	for ti, t := range targets {
		var merged model.Vector
		for _, part := range parts[ti] {
			merged = append(merged, part...)
		}
		*t.dst = merged
	}
	return nil
}

// instantQoSChunk issues one family's query for one chunk of the scope. It
// mirrors fetchOptionalTracking's contract — swallow a query error, fail only
// on caller cancellation, recover panics — and keeps the bare family name as
// the query NAME, so self-metrics and span dimensions still carry one label
// value per family however many chunks a build issues.
func instantQoSChunk(
	ctx, callerCtx context.Context,
	q promql.Querier,
	name promql.Query,
	window time.Duration,
	end time.Time,
	volumes []string,
) (out model.Vector, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(ctx, "panic in scoped QoS query",
				"query", string(name),
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			out, err = nil, fmt.Errorf("panic in %s query: %v", name, rec)
		}
	}()

	rendered, ok := promql.RenderQoSVolumeScoped(name, window, volumes)
	if !ok {
		// Unreachable for a non-empty chunk; never fall back to an unscoped
		// read, which would fetch the whole filer's workloads.
		return nil, nil
	}
	res, qerr := q.Instant(ctx, string(name), rendered, end)
	if qerr != nil {
		if cerr := optionalQueryFatal(callerCtx, qerr); cerr != nil {
			return nil, cerr
		}
		slog.WarnContext(ctx, "optional scoped QoS query failed; continuing with empty vector",
			"query", string(name),
			"volumes", len(volumes),
			"error", qerr)
		return nil, nil
	}
	return res, nil
}
