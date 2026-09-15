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

// legMode is the error class a by-reference family's chunk carries, mirroring
// the first wave's fetch / fetchOptional / fetchOptionalTracking split
// (topology.go): legRequired fails the build on any chunk error, exactly as
// the family's unscoped read would; legOptional logs and continues, costing
// only that chunk's contribution; legOptionalTracking does the same and ALSO
// sets a shared flag when any chunk of the family degraded, for the one
// family (kube_job_annotations) a downstream reader infers something from the
// absence of.
type legMode int

const (
	legRequired legMode = iota
	legOptional
	legOptionalTracking
)

// scopedFamily is one family a by-reference wave issues: its own scope
// (independent of every other family's — the controller wave's eight
// families are each scoped on a DIFFERENT set of names), the slot its merged
// vector lands in, its error class, and — for legOptionalTracking only — the
// flag to set when any chunk of it degraded.
type scopedFamily struct {
	query    promql.Query
	dst      *model.Vector
	scope    []string
	mode     legMode
	degraded *bool
}

// issueScopedFamilies issues several families, each restricted to its OWN
// scope, chunked and merged in chunk order, under one bounded errgroup shared
// across every chunk of every family — the generic engine behind
// readScopedPods, readScopedNodes and readScopedControllers' two stages
// (scope-controller-legs-by-reference D3).
//
// A family whose scope is empty is neither launched nor tallied: its
// topologyVectors slot is left untouched (nil, from the zero-valued
// topologyVectors) and v.ScopeIssued gains no entry for it — the same
// "absent means never read" contract every other second wave keeps.
//
// It returns the first legRequired-mode chunk error (fail closed, exactly as
// an unscoped read of that family would fail the build); a legOptional or
// legOptionalTracking chunk error is logged and costs only that chunk's
// contribution to the merged vector.
func issueScopedFamilies(
	ctx, callerCtx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	families []scopedFamily,
) error {
	chunksByFamily := make([][][]string, len(families))
	for fi, fam := range families {
		if len(fam.scope) == 0 {
			continue
		}
		chunksByFamily[fi] = promql.ChunkScope(fam.scope, opts.qosScopeBatchBytes())
	}

	parts := make([][]model.Vector, len(families))
	degradedChunks := make([][]bool, len(families))
	for fi := range families {
		n := len(chunksByFamily[fi])
		parts[fi] = make([]model.Vector, n)
		degradedChunks[fi] = make([]bool, n)
	}

	wave, wctx := errgroup.WithContext(ctx)
	wave.SetLimit(scopeConcurrency)
	for fi, fam := range families {
		for ci, chunk := range chunksByFamily[fi] {
			wave.Go(func() error {
				out, degraded, err := issueScopedChunk(wctx, callerCtx, q, fam.query, window, end, opts.LabelKeys, sel, chunk, fam.mode)
				if err != nil {
					return err
				}
				parts[fi][ci] = out
				degradedChunks[fi][ci] = degraded
				return nil
			})
		}
	}
	if err := wave.Wait(); err != nil {
		return err
	}

	for fi, fam := range families {
		if len(chunksByFamily[fi]) == 0 {
			continue // empty scope: not issued, not tallied
		}
		var merged model.Vector
		anyDegraded := false
		for ci, part := range parts[fi] {
			merged = append(merged, part...)
			anyDegraded = anyDegraded || degradedChunks[fi][ci]
		}
		*fam.dst = merged
		markScopeIssued(v, scopeMu, fam.query)
		if anyDegraded && fam.mode == legOptionalTracking && fam.degraded != nil {
			*fam.degraded = true
		}
	}
	return nil
}

// issueScopedChunk issues one family's query for one chunk of its scope,
// under the bare family name, honouring mode's error class. It recovers its
// own panics for the reason fetch does: errgroup does not propagate them, and
// an unrecovered panic here would kill the process.
func issueScopedChunk(
	ctx, callerCtx context.Context,
	q promql.Querier,
	name promql.Query,
	window time.Duration,
	end time.Time,
	keys promql.LabelKeys,
	sel promql.Selector,
	values []string,
	mode legMode,
) (out model.Vector, degraded bool, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(ctx, "panic in scoped topology query",
				"query", string(name),
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			out, degraded, err = nil, false, fmt.Errorf("panic in %s query: %v", name, rec)
		}
	}()

	rendered, ok := promql.RenderScoped(name, window, keys, sel, values)
	if !ok {
		// Unreachable for a non-empty chunk of a scopeable family. Failing is
		// the only honest answer: an unscoped fallback would read the whole
		// estate, and an empty vector would silently draw nothing.
		return nil, false, fmt.Errorf("%s: no scoped rendering for %d values", name, len(values))
	}
	res, qerr := q.Instant(ctx, string(name), rendered, end)
	if qerr == nil {
		return res, false, nil
	}
	if mode == legRequired {
		return nil, false, qerr
	}
	if cerr := optionalQueryFatal(callerCtx, qerr); cerr != nil {
		return nil, false, cerr
	}
	slog.WarnContext(ctx, "optional scoped topology query failed; continuing with empty vector",
		"query", string(name),
		"values", len(values),
		"error", qerr)
	return nil, true, nil
}
