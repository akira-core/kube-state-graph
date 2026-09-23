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

// scopedFamily is one family a by-reference wave issues: its own scope
// (independent of every other family's — the controller wave's eight
// families are each scoped on a DIFFERENT set of names) and the slot its
// merged vector lands in.
//
// There is no per-family error class. Only a by-reference plan — the
// /v1/storage-graph read — issues these waves, and that endpoint fails
// closed on every query error but ALERTS (fail-storage-graph-on-any-leg-error),
// so a chunk error of ANY family here fails the build — including
// kube_replicaset_annotations and kube_job_annotations, which /v1/graph's
// first wave reads as degrading families.
//
// render, when non-nil, replaces promql.RenderScoped for this family's
// chunks. nil keeps today's identity-label scope. budgetOverhead is the
// per-value argument of promql.ChunkScopeWithOverhead (zero for an
// identity-label scope). budgetReserve is subtracted once from the byte
// budget before chunking — the tracking-id wrapper, which a chunk pays once
// rather than once per value.
type scopedFamily struct {
	query          promql.Query
	dst            *model.Vector
	scope          []string
	render         func(chunk []string) (string, bool)
	budgetOverhead int
	budgetReserve  int
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
// It returns the first chunk error, named with its family (fail closed: a
// missing chunk would be a smaller, plausible, wrong body with no signal).
func issueScopedFamilies(
	ctx context.Context,
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
		budget := opts.qosScopeBatchBytes() - fam.budgetReserve
		if budget < 1 {
			budget = 1
		}
		chunksByFamily[fi] = promql.ChunkScopeWithOverhead(fam.scope, budget, fam.budgetOverhead)
	}

	parts := make([][]model.Vector, len(families))
	for fi := range families {
		parts[fi] = make([]model.Vector, len(chunksByFamily[fi]))
	}

	wave, wctx := errgroup.WithContext(ctx)
	wave.SetLimit(scopeConcurrency)
	for fi, fam := range families {
		for ci, chunk := range chunksByFamily[fi] {
			wave.Go(func() error {
				out, err := issueScopedChunk(wctx, q, fam.query, window, end, opts.LabelKeys, sel, chunk, fam.render)
				if err != nil {
					return err
				}
				parts[fi][ci] = out
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
		for _, part := range parts[fi] {
			merged = append(merged, part...)
		}
		*fam.dst = merged
		markScopeIssued(v, scopeMu, fam.query)
	}
	return nil
}

// issueScopedChunk issues one family's query for one chunk of its scope,
// under the bare family name; a query error is returned named with its
// family. It recovers its own panics for the reason fetch does: errgroup does
// not propagate them, and an unrecovered panic here would kill the process.
func issueScopedChunk(
	ctx context.Context,
	q promql.Querier,
	name promql.Query,
	window time.Duration,
	end time.Time,
	keys promql.LabelKeys,
	sel promql.Selector,
	values []string,
	render func(chunk []string) (string, bool),
) (out model.Vector, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(ctx, "panic in scoped topology query",
				"query", string(name),
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			out, err = nil, fmt.Errorf("panic in %s query: %v", name, rec)
		}
	}()

	var (
		rendered string
		ok       bool
	)
	if render != nil {
		rendered, ok = render(values)
	} else {
		rendered, ok = promql.RenderScoped(name, window, keys, sel, values)
	}
	if !ok {
		// Unreachable for a non-empty chunk of a scopeable family. Failing is
		// the only honest answer: an unscoped fallback would read the whole
		// estate, and an empty vector would silently draw nothing.
		return nil, fmt.Errorf("%s: no scoped rendering for %d values", name, len(values))
	}
	res, qerr := q.Instant(ctx, string(name), rendered, end)
	if qerr != nil {
		return nil, wrapQueryError(name, qerr)
	}
	return res, nil
}
