package build

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// The rooted Harvest volume-label read (scope-volume-labels-by-storage-root,
// read-storage-roots-through-volume-hub).
//
// A /v1/storage-graph request rooted at an ONTAP cluster, an aggregate or an SVM
// used to read the whole filer's volume-label family and discard nearly all of
// it at projection. Under a restriction — which is also what puts the build in
// hub mode (topologyPlan.hub) — the build reads it in phases:
//
//	phase 1           the family restricted to the rooted components: an
//	                  aggregate group {cluster=~OC?, aggr=~A} (or {cluster=~OC}
//	                  alone), an SVM group {cluster=~OC?, svm=~S}, or both —
//	                  a first-wave leg, because the restriction comes from the
//	                  request
//	owner completion  every aggregate an SVM-group row names, re-read whole,
//	                  waiting on phase 1 alone (below)
//	phase 2           the family restricted on `volume` to the derived tokens
//	                  of exactly the claims phase 1 matched, waiting on phase 1
//	                  and the claim-info family
//
// and merges the three, de-duplicated by label set, before anything but the
// hub's own claim read consumes the family. The claim read (claimscope.go)
// reads phase 1 ALONE: the rooted rows name the claims, and completion or
// phase-2 rows are never a source of claims.
//
// Phase 2 exists for correctness, not speed. pickAggr and pickSVM are
// lexically-smallest over a claim's WHOLE candidate set, so a phase-1-only read
// could place a claim on the rooted aggregate where an unrestricted read would
// place it on a lexically-smaller one — a Trident clone whose FlexVol name also
// matches the claim's token, or the same name on a second filer. Phase 2
// restores every matched claim's full candidate set, in the FORWARD direction of
// the configured derivation: the token is what the build already computed from a
// PersistentVolume name it holds.
//
// Why phase 1's matched set is sufficient: a claim can only be retained by a
// storage-side root if its picked aggregate or SVM is a rooted id, which needs at
// least one candidate on a rooted component, which means phase 1 matched it. And
// the converse closure holds too — any series on a rooted component that matches
// ANY claim's token is in phase 1, so the claims with a rooted candidate are
// exactly the claims phase 1 matched.
//
// Owner completion exists for the aggregate OWNER vote (netapp.go pickOwner),
// which reads every series of an aggregate that reached the merged vector. An
// aggregate group reads each of its aggregates whole; an SVM group reads only
// the SVM's share of every aggregate it touches, so the build re-reads those
// aggregates whole — except the ones an aggregate group of the same request
// already read whole. The rooted aggregates, and every aggregate an SVM root
// reaches, therefore vote over the same population an unrestricted read gives
// them, which is what keeps the takeover case (the series of one aggregate
// naming two controllers) byte-identical.
//
// What that does NOT cover, and why the body is still safe: an aggregate seen
// only through phase 2 — a clone's aggregate, say — votes over the one or two
// of its volumes a claim's token happened to match, and can elect a different
// controller than the whole filer would. Such an aggregate is never DRAWN: it
// is not a root, and the unit whose claim reached it does not intersect the
// rooted ids, so ProjectStorage drops it and pullNetAppParents never reaches
// its controller. That invariant lives in pkg/graph, not here, so widening
// resolveStorageRoots would start drawing a controller chosen from a partial
// vote — TestRootedVolumeLabels_PhaseTwoOnlyAggregateIsNeverDrawn pins it.

// phaseOneGroup names which phase-1 query group a rendered query belongs to.
// Owner completion reads the SVM group's rows alone.
type phaseOneGroup int

const (
	// groupAggr is {cluster=~OC?, aggr=~A}, or {cluster=~OC} for a request
	// rooted at ONTAP clusters alone. Every aggregate it names is read whole.
	groupAggr phaseOneGroup = iota
	// groupSVM is {cluster=~OC?, svm=~S}: each SVM's share of every aggregate
	// it touches.
	groupSVM
)

// rootedVolumeLabelsQuery is one rendered phase-1 query, kept beside the value
// sets it was rendered from so a test can assert on what each chunk carries.
type rootedVolumeLabelsQuery struct {
	group    phaseOneGroup
	clusters []string
	aggrs    []string
	svms     []string
	rendered string
}

// render is the query's PromQL. ok is false when its value sets normalise
// away, which only an embedder filling StorageRoots directly can produce.
func (r rootedVolumeLabelsQuery) render(window time.Duration) (string, bool) {
	if r.group == groupSVM {
		return promql.RenderVolumeLabelsSVMRooted(window, r.clusters, r.svms)
	}
	return promql.RenderVolumeLabelsRooted(window, r.clusters, r.aggrs)
}

// maxRootedVolumeLabelChunks bounds how many queries the phase-1 restriction may
// become, summed over its aggregate and SVM groups. Past it the build reads the
// family UNRESTRICTED instead — and, since the restriction is what engages the
// volume hub, reads every claim family as it did before the hub existed.
//
// This is the one scope in the package derived from the REQUEST rather than from
// upstream data, and `?aggr=` / `?svm=` / `?ontap_cluster=` are repeatable with
// no cap on how many values a client may send (the request parser bounds each value's
// length, never the count). Without a ceiling, one request naming thousands of
// aggregates would turn a single `last_over_time(volume_labels[w])` into
// thousands of queries, each still carrying the whole repeated cluster
// alternation — a self-inflicted fan-out no upstream limit protects against,
// and one no data-derived scope can produce.
//
// Falling back to the unrestricted read is the safe direction: it is exactly
// what this leg did before the restriction existed, it is one query, and the
// body is unchanged either way. The value matches scopeConcurrency, so a
// restricted read never spans more than one concurrency wave.
const maxRootedVolumeLabelChunks = scopeConcurrency

// rootedVolumeLabelsChunks splits the phase-1 restriction across as many
// queries as the byte budget requires, in (group, chunk) order: the aggregate
// group's chunks first, then the SVM group's. ok is false when that would take
// more than maxRootedVolumeLabelChunks queries IN TOTAL across both groups; the
// caller then reads unrestricted.
//
// Each group chunks ITS alternation — `aggr` for the aggregate group, `svm` for
// the SVM group, `cluster` for a request rooted at ONTAP clusters alone — and
// repeats the `cluster` alternation verbatim in every chunk. The matchers of
// one query are AND-combined, so a union over disjoint chunks of one of them is
// exactly the unchunked selection; and disjoint chunks of one group return
// disjoint series. The two groups DO overlap — a volume of a rooted SVM on a
// rooted aggregate is returned by both — which the caller's merge removes.
//
// The repeated alternation is not counted by ChunkScope, so its RENDERED length
// — escaping, the `=~"…"` wrapper and the separating comma included — is taken
// off the budget first. Measuring the rendered form rather than the raw values
// is what makes the budget a bound: a filer named `ontap.prod` is three bytes
// longer per dot once QuoteMeta and the string escape have run, and the wrapper
// is another eleven the raw join never sees. The floor of 1 keeps a
// pathological cluster set from producing a non-positive budget, which
// ChunkScope would read as "no limit"; the chunk cap above is what actually
// catches that case.
func rootedVolumeLabelsChunks(clusters, aggrs, svms []string, budget int) ([]rootedVolumeLabelsQuery, bool) {
	fixed := promql.MatcherCost(promql.VolumeLabelsClusterLabel, clusters)
	if fixed > 0 {
		fixed++ // the comma joining it to the chunked matcher
	}
	var out []rootedVolumeLabelsQuery
	if len(aggrs) > 0 {
		for _, chunk := range promql.ChunkScope(aggrs, max(budget-fixed, 1)) {
			out = append(out, rootedVolumeLabelsQuery{group: groupAggr, clusters: clusters, aggrs: chunk})
		}
	}
	if len(svms) > 0 {
		for _, chunk := range promql.ChunkScope(svms, max(budget-fixed, 1)) {
			out = append(out, rootedVolumeLabelsQuery{group: groupSVM, clusters: clusters, svms: chunk})
		}
	}
	if len(aggrs) == 0 && len(svms) == 0 {
		for _, chunk := range promql.ChunkScope(clusters, budget) {
			out = append(out, rootedVolumeLabelsQuery{group: groupAggr, clusters: chunk})
		}
	}
	if len(out) > maxRootedVolumeLabelChunks {
		return nil, false
	}
	return out, true
}

// matchedClaimTokens is phase 2's scope: the sorted, de-duplicated derived
// tokens of exactly the claims phase 1 matched.
//
// It reads the token from the SAME matcher the parse joins through
// (newVolumeMatcher), rather than re-deriving it, so a claim can never be
// fetched for under one token and then joined under another — the failure the
// VolumeKey comment on topologyVectors describes for the QoS scope.
//
// The claim set is every distinct PV name kube-state-metrics reports on
// kube_persistentvolumeclaim_info, which is a SUPERSET of the claims the parse
// binds to a pod. That is deliberate and harmless: an unmounted claim that
// phase 1 matched contributes a token, phase 2 reads a few extra candidates for
// it, and the projection drops the claim exactly as it always has.
func matchedClaimTokens(pvcInfo, phaseOne model.Vector, rw *VolumeKeyRewriter) []string {
	if len(pvcInfo) == 0 || len(phaseOne) == 0 {
		return nil
	}
	claims, m := claimVolumeMatcher(pvcInfo, rw)
	if m == nil {
		return nil
	}
	matchedClaim := make([]bool, len(claims))
	var hits []int
	for _, s := range phaseOne {
		vol := string(s.Metric[promql.HarvestVolumeLabel])
		if vol == "" {
			continue
		}
		hits = m.match(vol, hits)
		for _, ci := range hits {
			matchedClaim[ci] = true
		}
	}

	tokens := make([]string, 0, len(claims))
	for ci, ok := range matchedClaim {
		if ok && m.tokens[ci] != "" {
			tokens = append(tokens, m.tokens[ci])
		}
	}
	slices.Sort(tokens)
	return slices.Compact(tokens)
}

// mergeVolumeLabels appends to base every series of extra whose label set base
// does not already hold, preserving base's order and then extra's.
//
// The de-duplication is mandatory rather than tidy. The two phases overlap by
// construction — a volume on a rooted aggregate that matches a claim's token is
// returned by both — and volIndex / allByAggr accumulate per series, so an
// undeduplicated merge would make that series vote twice in pickOwner and count
// twice in every per-aggregate tally. It is the same reason the routing layer
// de-duplicates a multi-backend fan-out by label-set fingerprint.
func mergeVolumeLabels(base, extra model.Vector) model.Vector {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[model.Fingerprint]struct{}, len(base))
	for _, s := range base {
		seen[s.Metric.Fingerprint()] = struct{}{}
	}
	out := slices.Clone(base)
	for _, s := range extra {
		fp := s.Metric.Fingerprint()
		if _, dup := seen[fp]; dup {
			continue
		}
		seen[fp] = struct{}{}
		out = append(out, s)
	}
	return out
}

// issueVolumeLabelsQueries issues each rendered query under the bare family
// name and returns the results merged in QUERY order, never completion order.
func issueVolumeLabelsQueries(
	ctx context.Context,
	q promql.Querier,
	end time.Time,
	phase string,
	rendered []string,
) (model.Vector, error) {
	parts, err := issueVolumeLabelsParts(ctx, q, end, phase, rendered)
	if err != nil {
		return nil, err
	}
	var merged model.Vector
	for _, part := range parts {
		merged = append(merged, part...)
	}
	return merged, nil
}

// issueVolumeLabelsParts issues each rendered query under the bare family name
// and returns one result per query, in query order.
//
// A failed query fails the build, named with its family. This read runs only
// on /v1/storage-graph, which fails closed (fail-storage-graph-on-any-leg-error):
// a lost chunk would draw the rooted components with no path through the
// claims it carried, indistinguishable from a filer that serves nothing.
func issueVolumeLabelsParts(
	ctx context.Context,
	q promql.Querier,
	end time.Time,
	phase string,
	rendered []string,
) ([]model.Vector, error) {
	parts := make([]model.Vector, len(rendered))
	// WithContext, so the first failed chunk cancels the chunks still in flight
	// instead of making the build wait out every remaining round-trip before it
	// can report the failure. Mirrors issueScopedFamilies (scopedread.go).
	wave, wctx := errgroup.WithContext(ctx)
	wave.SetLimit(scopeConcurrency)
	for i, query := range rendered {
		wave.Go(func() (err error) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.ErrorContext(ctx, "panic in rooted volume-label query",
						"phase", phase,
						"panic", fmt.Sprint(rec),
						"stack", string(debug.Stack()),
					)
					err = fmt.Errorf("panic in %s query: %v", promql.QVolumeLabels, rec)
				}
			}()
			out, qerr := q.Instant(wctx, string(promql.QVolumeLabels), query, end)
			if qerr != nil {
				return wrapQueryError(promql.QVolumeLabels, qerr)
			}
			parts[i] = out
			return nil
		})
	}
	if err := wave.Wait(); err != nil {
		return nil, err
	}
	return parts, nil
}

// readRootedVolumeLabels is phase 1: the volume-label family restricted to the
// request's rooted components, one query per chunk of the resolved plan's
// groups. It returns the leg's run function so the launch loop can slot it in
// where the unrestricted fetch stood, and writes the merged result into the
// same topologyVectors slot the unrestricted leg does. The SVM group's rows are
// also kept apart in svmRows, for owner completion.
//
// The merge runs in (group, chunk) order and de-duplicates by label set: a
// volume of a rooted SVM on a rooted aggregate is returned by both groups, and
// an undeduplicated series would vote twice in pickOwner.
func readRootedVolumeLabels(
	ctx context.Context,
	q promql.Querier,
	end time.Time,
	queries []rootedVolumeLabelsQuery,
	dst *model.Vector,
	svmRows *model.Vector,
) func() error {
	return func() (err error) {
		// The same guard fetch puts on this leg. errgroup does not propagate a
		// goroutine panic to Wait, so an unrecovered one here kills the process
		// rather than becoming a sanitised 500.
		defer func() {
			if rec := recover(); rec != nil {
				slog.ErrorContext(ctx, "panic in rooted volume-label query",
					"query", string(promql.QVolumeLabels),
					"panic", fmt.Sprint(rec),
					"stack", string(debug.Stack()),
				)
				err = fmt.Errorf("panic in %s query: %v", promql.QVolumeLabels, rec)
			}
		}()

		rendered := make([]string, len(queries))
		for i, r := range queries {
			rendered[i] = r.rendered
		}
		parts, qerr := issueVolumeLabelsParts(ctx, q, end, "roots", rendered)
		if qerr != nil {
			return qerr
		}
		var aggrRows, svmGroup model.Vector
		for i, part := range parts {
			if queries[i].group == groupSVM {
				svmGroup = append(svmGroup, part...)
				continue
			}
			aggrRows = append(aggrRows, part...)
		}
		*svmRows = svmGroup
		*dst = mergeVolumeLabels(aggrRows, svmGroup)
		return nil
	}
}

// ownerCompletionTargets is the owner-completion scope: every (ONTAP cluster,
// aggregate) pair an SVM-group row names, minus the aggregates an aggregate
// group of the same request already read whole — an aggregate named by an
// aggr= root, on a filer inside the ontap_cluster= roots (any filer, when there
// are none). Keyed by ONTAP cluster, each aggregate set sorted.
//
// A row with an empty `aggr` (a FlexGroup) names no aggregate and completes
// nothing: there is no single aggregate whose vote it could skew.
func ownerCompletionTargets(svmRows model.Vector, plan topologyPlan) map[string][]string {
	readWhole := func(cluster, aggr string) bool {
		if !slices.Contains(plan.volumeAggrs, aggr) {
			return false
		}
		return len(plan.volumeClusters) == 0 || slices.Contains(plan.volumeClusters, cluster)
	}
	out := map[string][]string{}
	for _, s := range svmRows {
		cluster := string(s.Metric[promql.VolumeLabelsClusterLabel])
		aggr := string(s.Metric["aggr"])
		if aggr == "" || readWhole(cluster, aggr) {
			continue
		}
		out[cluster] = append(out[cluster], aggr)
	}
	for c, aggrs := range out {
		slices.Sort(aggrs)
		out[c] = slices.Compact(aggrs)
	}
	return out
}

// readOwnerCompletion re-reads, whole, every aggregate the SVM group touched
// but no aggregate group read whole, one chunked query per ONTAP cluster, so
// the owning-controller vote runs over the aggregate's full population.
//
// Its rows feed the owner vote and the storage inventory only: they are merged
// into the family AFTER the hub's claim read has taken its candidates from
// phase 1, so a claim on another SVM of a touched aggregate — on no rooted
// component — is never loaded through them. A request with no svm= root, or
// whose SVM rows name only aggregates already read whole, issues nothing.
func readOwnerCompletion(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	plan topologyPlan,
	svmRows model.Vector,
) (model.Vector, error) {
	targets := ownerCompletionTargets(svmRows, plan)
	if len(targets) == 0 {
		return nil, nil
	}
	clusters := slices.Sorted(maps.Keys(targets))
	var rendered []string
	for _, c := range clusters {
		budget := max(opts.qosScopeBatchBytes()-promql.OwnerCompletionClusterCost(c), 1)
		for _, chunk := range promql.ChunkScope(targets[c], budget) {
			query, ok := promql.RenderVolumeLabelsOwnerCompletion(window, c, chunk)
			if !ok {
				continue // unreachable: targets hold no empty aggregate
			}
			rendered = append(rendered, query)
		}
	}
	return issueVolumeLabelsQueries(ctx, q, end, "owner_completion", rendered)
}

// readTokenScopedVolumeLabels is phase 2: it re-reads the family restricted on
// `volume` to the derived tokens of the claims phase 1 matched, and returns
// the rows for the caller to merge. v.VolumeLabels MUST still hold phase 1
// alone, and v.PVCInfo MUST have landed.
//
// Nothing is issued when no claim matched: phase 1 then drew no edge for a
// candidate set to be recovered for, and a rooted component with no claim is
// still drawn from the aggregate, controller and policy families, which are
// unrestricted.
//
// A failed chunk fails the build (fail-storage-graph-on-any-leg-error).
func readTokenScopedVolumeLabels(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	plan topologyPlan,
	v *topologyVectors,
) (model.Vector, error) {
	// The FIELD, not opts.volumeKey(): readTopology stores the rewriter once so
	// the parse and every scope derive a claim's token identically, and
	// re-resolving it here would recompile every rewrite rule's regexp.
	//
	// Read as a field and never through a value-receiver method: that would
	// copy the whole shared *topologyVectors — every model.Vector slot the
	// sibling legs are still writing — which is a data race the detector
	// catches. VolumeKey itself is written once, before the errgroup starts, so
	// reading the one field is safe.
	rw := v.VolumeKey
	if rw == nil {
		rw = defaultVolumeKeyRewriter()
	}
	kind := rw.tokenScope()
	if kind == tokenScopeNone {
		return nil, nil // unreachable under the predicate; never guess a branch shape
	}
	tokens := matchedClaimTokens(v.PVCInfo, v.VolumeLabels, rw)
	if len(tokens) == 0 {
		return nil, nil
	}

	suffix := kind == tokenScopeSuffix
	overhead := 0
	if suffix {
		overhead = promql.VolumeTokenBranchOverhead
	}
	// A restriction by ONTAP cluster ALONE read those filers whole, so every
	// candidate on them is already in hand and the only ones left to recover
	// are elsewhere. Excluding them turns phase 2 from a second full-family
	// regex scan into the narrow cross-filer lookup it exists to be. It is
	// only sound with no aggregate and no SVM root: phase 1 then read part of a
	// cluster, and a candidate on another aggregate or SVM of the same cluster
	// is exactly what phase 2 must still find.
	var excludeClusters []string
	if len(plan.volumeAggrs) == 0 && len(plan.volumeSVMs) == 0 {
		excludeClusters = plan.volumeClusters
	}
	chunks := promql.ChunkScopeWithOverhead(tokens, opts.qosScopeBatchBytes(), overhead)
	rendered := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		query, ok := promql.RenderVolumeLabelsTokenScoped(window, chunk, suffix, excludeClusters)
		if !ok {
			// Unreachable for a non-empty chunk. Failing is the only honest
			// answer under a fail-closed read: an incomplete candidate set
			// could move a pick.
			return nil, fmt.Errorf("%s: no token-scoped rendering for %d tokens", promql.QVolumeLabels, len(chunk))
		}
		rendered = append(rendered, query)
	}
	return issueVolumeLabelsQueries(ctx, q, end, "tokens", rendered)
}

// readVolumeLabelsTail runs the rest of the hub's volume-label read after phase
// 1 — owner completion (waiting on phase 1 alone) beside phase 2 (waiting on
// the claim-info family too) — and merges both into v.VolumeLabels, in the
// order phase 1, completion, phase 2, de-duplicated by label set. The QoS wave
// and the parse read the merged result; the caller closes their done-channel
// when this returns.
//
// The merge writes v.VolumeLabels, which the hub's claim-info read reads, so it
// happens only once pvcInfoDone has been OBSERVED closed — phase 2 waits on it
// — and never on a path where the build is already failing.
func readVolumeLabelsTail(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	plan topologyPlan,
	v *topologyVectors,
	svmRows *model.Vector,
	phaseOneDone, pvcInfoDone <-chan struct{},
) (err error) {
	// matchedClaimTokens, the target computation and the chunking run outside
	// any per-query recover, in an errgroup goroutine whose panic would
	// otherwise take the process down.
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(ctx, "panic in volume-label tail read",
				"query", string(promql.QVolumeLabels),
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			err = fmt.Errorf("panic in %s query: %v", promql.QVolumeLabels, rec)
		}
	}()
	select {
	case <-phaseOneDone:
	case <-ctx.Done():
		// A sibling leg failed (or the caller went away). The group already
		// carries that error; adding another would only mask it.
		return nil
	}

	var completion, phaseTwo model.Vector
	tail, tctx := errgroup.WithContext(ctx)
	tail.Go(func() (err error) {
		completion, err = readOwnerCompletion(tctx, q, window, end, opts, plan, *svmRows)
		return err
	})
	tail.Go(func() (err error) {
		select {
		case <-pvcInfoDone:
		case <-tctx.Done():
			return nil
		}
		phaseTwo, err = readTokenScopedVolumeLabels(tctx, q, window, end, opts, plan, v)
		return err
	})
	if err := tail.Wait(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		// Phase 2 may have left on the cancellation rather than on pvcInfoDone,
		// so the claim-info read may still be reading v.VolumeLabels. The build
		// is failing anyway; do not write.
		return nil
	}
	v.VolumeLabels = mergeVolumeLabels(mergeVolumeLabels(v.VolumeLabels, completion), phaseTwo)
	return nil
}
