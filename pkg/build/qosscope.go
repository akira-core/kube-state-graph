package build

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// scopeConcurrency bounds one second wave's reads in flight at once (its
// families × however many chunks its scope was split into). Each wave — the QoS
// workload read, both phases of the rooted volume-label read, and the storage
// build's pod, node and controller reads — has its own limit, and waves overlap:
// the node and controller waves run beside the QoS wave, so one storage build
// can hold up to three times this many scoped reads in flight. It is a
// site-invariant tuning value like routeResolveConcurrency, not a knob: the
// queries it bounds are already narrow, and the upstream's own limits are the
// backstop that matters. It binds only when a wave's families × chunks exceeds
// it, so it throttles only a scope large enough to split into several chunks.
const scopeConcurrency = 16

// scopedTarget is one family a second wave reads, with the slot it lands in.
type scopedTarget struct {
	query promql.Query
	dst   *model.Vector
}

// qosTargets are the six QoS workload families the scoped QoS read issues.
func qosTargets(v *topologyVectors) []scopedTarget {
	return []scopedTarget{
		{promql.QQoSReadOps, &v.QoSReadOps},
		{promql.QQoSWriteOps, &v.QoSWriteOps},
		{promql.QQoSReadLatency, &v.QoSReadLatency},
		{promql.QQoSWriteLatency, &v.QoSWriteLatency},
		{promql.QQoSReadData, &v.QoSReadData},
		{promql.QQoSWriteData, &v.QoSWriteData},
	}
}

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
// With delta (a /v1/storage-graph build with io_delta=true) the same wave also
// issues, per family, the end-minus-start change of every series over the same
// scope (promql.RenderQoSVolumeScopedDelta). A delta query renders the volume
// alternation twice, so the delta read chunks the scope with half the byte
// budget and the two alternations of one query together stay within the budget
// the operator set for one. Delta chunks are issued under the same concurrency
// limit, merge in chunk order into the family's delta vector, fail closed like
// every storage-build query, and are deliberately NOT written to ScopeIssued
// or ExtraSeriesCount: the per-family series tally keeps describing the
// measurement read. Delta is only ever set for a fail-closed storage plan.
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
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	failClosed bool,
	rollup promql.Rollup,
	delta bool,
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

	targets := qosTargets(v)

	// One slot per (family, chunk). Writing into a pre-sized slot rather than
	// appending is what makes the merge below order-free.
	parts := make([][]model.Vector, len(targets))
	for i := range parts {
		parts[i] = make([]model.Vector, len(chunks))
	}

	var deltaChunks [][]string
	var deltaTargets []scopedTarget
	var deltaParts [][]model.Vector
	if delta {
		deltaTargets = qosDeltaTargets(v)
		deltaChunks = promql.ChunkQoSVolumeScope(scope, deltaBudget(opts.qosScopeBatchBytes()))
		deltaParts = make([][]model.Vector, len(deltaTargets))
		for i := range deltaParts {
			deltaParts[i] = make([]model.Vector, len(deltaChunks))
		}
	}

	var wave errgroup.Group
	wave.SetLimit(scopeConcurrency)
	for ti, t := range deltaTargets {
		for ci, chunk := range deltaChunks {
			wave.Go(func() error {
				out, err := instantQoSDeltaChunk(ctx, callerCtx, q, t.query, window, end, opts.LabelKeys, sel, chunk, failClosed)
				if err != nil {
					return err
				}
				deltaParts[ti][ci] = out
				return nil
			})
		}
	}
	for ti, t := range targets {
		for ci, chunk := range chunks {
			wave.Go(func() error {
				out, err := instantQoSChunk(ctx, callerCtx, q, t.query, window, end, opts.LabelKeys, sel, chunk, failClosed, rollup)
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
		markScopeIssued(v, scopeMu, t.query)
	}
	for ti, t := range deltaTargets {
		var merged model.Vector
		for _, part := range deltaParts[ti] {
			merged = append(merged, part...)
		}
		*t.dst = merged
	}
	return nil
}

// qosDeltaTargets are the six slots the delta read lands in, in the same
// family order as qosTargets.
func qosDeltaTargets(v *topologyVectors) []scopedTarget {
	return []scopedTarget{
		{promql.QQoSReadOps, &v.QoSReadOpsDelta},
		{promql.QQoSWriteOps, &v.QoSWriteOpsDelta},
		{promql.QQoSReadLatency, &v.QoSReadLatencyDelta},
		{promql.QQoSWriteLatency, &v.QoSWriteLatencyDelta},
		{promql.QQoSReadData, &v.QoSReadDataDelta},
		{promql.QQoSWriteData, &v.QoSWriteDataDelta},
	}
}

// deltaBudget halves the scoped-read byte budget for a delta query, which
// renders the volume alternation twice. A non-positive budget stays as it is:
// ChunkScope reads it as "no byte limit", leaving only the per-alternation
// value cap.
func deltaBudget(budget int) int {
	if budget <= 0 {
		return budget
	}
	return max(budget/2, 1)
}

// instantQoSChunk issues one family's query for one chunk of the scope. It
// mirrors fetchOptionalTracking's contract — swallow a query error, fail only
// on caller cancellation, recover panics — unless failClosed, when a query
// error fails the build (a /v1/storage-graph build; see topologyPlan). It
// keeps the bare family name as the query NAME, so self-metrics and span
// dimensions still carry one label value per family however many chunks a
// build issues.
func instantQoSChunk(
	ctx, callerCtx context.Context,
	q promql.Querier,
	name promql.Query,
	window time.Duration,
	end time.Time,
	keys promql.LabelKeys,
	sel promql.Selector,
	volumes []string,
	failClosed bool,
	rollup promql.Rollup,
) (model.Vector, error) {
	return runQoSChunk(ctx, callerCtx, q, name, func() (string, bool) {
		return promql.RenderQoSVolumeScopedRollup(name, window, keys, sel, volumes, rollup)
	}, end, len(volumes), failClosed)
}

// instantQoSDeltaChunk is instantQoSChunk for the end-minus-start form: the
// same query NAME (the bare family, so self-metrics and the fail-closed error
// keep one label value per family), the same recover / fail-closed contract.
func instantQoSDeltaChunk(
	ctx, callerCtx context.Context,
	q promql.Querier,
	name promql.Query,
	window time.Duration,
	end time.Time,
	keys promql.LabelKeys,
	sel promql.Selector,
	volumes []string,
	failClosed bool,
) (model.Vector, error) {
	return runQoSChunk(ctx, callerCtx, q, name, func() (string, bool) {
		return promql.RenderQoSVolumeScopedDelta(name, window, keys, sel, volumes)
	}, end, len(volumes), failClosed)
}

// runQoSChunk renders and issues one scoped QoS query. render runs AFTER the
// recover is armed, so a panic while rendering becomes this chunk's error like
// a panic in the query itself, never an escaped goroutine panic.
func runQoSChunk(
	ctx, callerCtx context.Context,
	q promql.Querier,
	name promql.Query,
	render func() (string, bool),
	end time.Time,
	volumes int,
	failClosed bool,
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

	rendered, ok := render()
	if !ok {
		// Unreachable for a non-empty chunk; never fall back to an unscoped
		// read, which would fetch the whole filer's workloads.
		return nil, nil
	}
	res, qerr := q.Instant(ctx, string(name), rendered, end)
	if qerr != nil {
		if failClosed {
			return nil, wrapQueryError(name, qerr)
		}
		if cerr := optionalQueryFatal(callerCtx, qerr); cerr != nil {
			return nil, cerr
		}
		slog.WarnContext(ctx, "optional scoped QoS query failed; continuing with empty vector",
			"query", string(name),
			"volumes", volumes,
			"error", qerr)
		return nil, nil
	}
	return res, nil
}
