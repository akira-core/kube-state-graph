package build

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// podTargets are the two pod families a pod-scoping plan reads by reference,
// with the slot each lands in. They are exactly promql.PodScopedQueries — the
// families promql.RenderScoped accepts.
func podTargets(v *topologyVectors) []scopedTarget {
	return []scopedTarget{
		{promql.QPodInfo, &v.Pod},
		{promql.QPodOwner, &v.PodOwner},
	}
}

// podScope is the sorted, de-duplicated set of pod names a storage body can
// draw: every pod a loaded claim-binding series binds, plus the request's pod
// roots. It mirrors the binding reader's own discard — parseTopology skips a
// series naming no claim — so a pod enters the scope iff it could be bound, or
// is a root, which must be drawable even when it mounts nothing.
func podScope(bindings model.Vector, roots []string) []string {
	names := make([]string, 0, len(bindings)+len(roots))
	for _, s := range bindings {
		if s.Metric["persistentvolumeclaim"] == "" && s.Metric["claim_name"] == "" {
			continue
		}
		names = append(names, string(s.Metric[promql.PodLabel]))
	}
	names = append(names, roots...)
	names = slices.DeleteFunc(names, func(n string) bool { return n == "" })
	slices.Sort(names)
	return slices.Compact(names)
}

// readScopedPods issues kube_pod_info and kube_pod_owner restricted to the pods
// a storage body can draw (podScope), composed with the request's own matchers.
// Pods that mount no claim — in a large estate, nearly all of them — are never
// fetched.
//
// It waits on the claim-binding family alone: the scope is computed from it and
// from the request's roots, and from nothing else.
//
// An empty scope issues no query at all and leaves both families unread: no pod
// could be bound or is a root, so no pod could be drawn. That mirrors the QoS
// read's empty-scope rule.
//
// Unlike a QoS chunk, a pod chunk FAILS CLOSED. A pod is topology, not a
// measurement: a missing chunk would be a smaller, plausible, wrong body with no
// signal — the partial fan-out that backend routing D6 forbids — so the first
// chunk error fails the build exactly as an unscoped kube_pod_info error does.
// Results merge in CHUNK ORDER, so the vectors and everything parsed from them
// are a pure function of the scope rather than of upstream timing.
//
// A pod name is unique within a namespace only, so the scope MAY admit a
// same-named pod from another namespace. Such a pod lies on no drawn path and is
// not a root, so the storage projection drops it; the body is unchanged.
func readScopedPods(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	roots []string,
	v *topologyVectors,
	bindingsDone <-chan struct{},
) error {
	select {
	case <-bindingsDone:
	case <-ctx.Done():
		// A sibling leg failed (or the caller went away). The group already
		// carries that error; adding another would only mask it.
		return nil
	}

	scope := podScope(v.PVC, roots)
	if len(scope) == 0 {
		return nil
	}
	v.PodScopeIssued = true
	chunks := promql.ChunkScope(scope, opts.qosScopeBatchBytes())
	targets := podTargets(v)

	// One slot per (family, chunk). Writing into a pre-sized slot rather than
	// appending is what makes the merge below order-free.
	parts := make([][]model.Vector, len(targets))
	for i := range parts {
		parts[i] = make([]model.Vector, len(chunks))
	}

	wave, wctx := errgroup.WithContext(ctx)
	wave.SetLimit(scopeConcurrency)
	for ti, t := range targets {
		for ci, chunk := range chunks {
			wave.Go(func() error {
				out, err := instantScopedPods(wctx, q, t.query, window, end, opts.LabelKeys, sel, chunk)
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

// instantScopedPods issues one pod family's query for one chunk of the scope,
// under the bare family name, so self-metrics and span dimensions carry one
// label value per family however many chunks a build issues. It recovers its
// own panics for the reason fetch does: errgroup does not propagate them, and
// an unrecovered panic here would kill the process.
func instantScopedPods(
	ctx context.Context,
	q promql.Querier,
	name promql.Query,
	window time.Duration,
	end time.Time,
	keys promql.LabelKeys,
	sel promql.Selector,
	pods []string,
) (out model.Vector, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(ctx, "panic in scoped pod query",
				"query", string(name),
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			out, err = nil, fmt.Errorf("panic in %s query: %v", name, rec)
		}
	}()

	rendered, ok := promql.RenderScoped(name, window, keys, sel, pods)
	if !ok {
		// Unreachable for a non-empty chunk of a scopeable family. Failing is the
		// only honest answer: an unscoped fallback would read the whole estate,
		// and an empty vector would silently draw no pods.
		return nil, fmt.Errorf("%s: no scoped rendering for %d pods", name, len(pods))
	}
	return q.Instant(ctx, string(name), rendered, end)
}
