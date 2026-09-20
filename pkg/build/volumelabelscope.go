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

// The rooted Harvest volume-label read (scope-volume-labels-by-storage-root).
//
// A /v1/storage-graph request rooted at an ONTAP cluster or an aggregate used to
// read the whole filer's volume-label family and discard nearly all of it at
// projection. Under a restriction the build reads it in two phases:
//
//	phase 1  the family restricted to the rooted components (cluster= / aggr=),
//	         a first-wave leg because the restriction comes from the request
//	phase 2  the family restricted on `volume` to the derived tokens of exactly
//	         the claims phase 1 matched, waiting on phase 1 and the claim-info
//	         family; merged into phase 1's vector before anything else reads it
//
// Phase 2 exists for correctness, not speed. pickAggr and pickSVM are
// lexically-smallest over a claim's WHOLE candidate set, so a phase-1-only read
// could place a claim on the rooted aggregate where an unrestricted read would
// place it on a lexically-smaller one — a Trident clone whose FlexVol name also
// matches the claim's token, or the same name on a second filer. Phase 2
// restores every matched claim's full candidate set, in the FORWARD direction of
// the configured derivation: the token is what the build already computed from a
// PersistentVolume name it holds, so nothing here recovers a PV name from a
// FlexVol name.
//
// Why phase 1's matched set is sufficient: a claim can only be retained by a
// storage-side root if its picked aggregate or SVM is a rooted id, which needs at
// least one candidate on a rooted component, which means phase 1 matched it. And
// the converse closure holds too — any series on a rooted component that matches
// ANY claim's token is in phase 1, so the claims with a rooted candidate are
// exactly the claims phase 1 matched.
//
// What that argument does NOT cover, and why the body is still safe: the
// aggregate OWNER vote (netapp.go pickOwner) reads every series of an aggregate
// that reached the merged vector, so an aggregate seen only through phase 2 — a
// clone's aggregate, say — votes over the one or two of its volumes a claim's
// token happened to match, and can elect a different controller than the whole
// filer would. Such an aggregate is never DRAWN: it is not a root, and the unit
// whose claim reached it does not intersect the rooted ids, so ProjectStorage
// drops it and pullNetAppParents never reaches its controller. That invariant
// lives in pkg/graph, not here, so widening resolveStorageRoots would start
// drawing a controller chosen from a partial vote —
// TestRootedVolumeLabels_PhaseTwoOnlyAggregateIsNeverDrawn pins it. The rooted
// aggregates themselves are unaffected: phase 1 reads every one of their series,
// which is exactly why an `svm=` or `node=` root disables the restriction
// instead of narrowing it.

// rootedVolumeLabelsQuery is one rendered phase-1 query, kept beside the value
// sets it was rendered from so a test can assert on what each chunk carries.
type rootedVolumeLabelsQuery struct {
	clusters []string
	aggrs    []string
}

// maxRootedVolumeLabelChunks bounds how many queries the phase-1 restriction may
// become. Past it the build reads the family UNRESTRICTED instead.
//
// This is the one scope in the package derived from the REQUEST rather than from
// upstream data, and `?aggr=` / `?ontap_cluster=` are repeatable with no cap on
// how many values a client may send (the request parser bounds each value's
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
// queries as the byte budget requires. ok is false when that would take more
// than maxRootedVolumeLabelChunks queries; the caller then reads unrestricted.
//
// The LARGER alternation is chunked — `aggr` when the request names any, else
// `cluster` — and the other is repeated verbatim in every chunk. The two
// matchers are AND-combined, so a union over disjoint chunks of one of them is
// exactly the unchunked selection; and disjoint chunks return disjoint series,
// so no cross-chunk de-duplication is needed.
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
func rootedVolumeLabelsChunks(clusters, aggrs []string, budget int) ([]rootedVolumeLabelsQuery, bool) {
	var chunks [][]string
	fixedClusters := clusters
	if len(aggrs) > 0 {
		fixed := promql.MatcherCost(promql.VolumeLabelsClusterLabel, clusters)
		if fixed > 0 {
			fixed++ // the comma joining it to the chunked matcher
		}
		chunks = promql.ChunkScope(aggrs, max(budget-fixed, 1))
	} else {
		chunks = promql.ChunkScope(clusters, budget)
		fixedClusters = nil
	}
	if len(chunks) > maxRootedVolumeLabelChunks {
		return nil, false
	}
	out := make([]rootedVolumeLabelsQuery, 0, len(chunks))
	for _, chunk := range chunks {
		if len(aggrs) > 0 {
			out = append(out, rootedVolumeLabelsQuery{clusters: fixedClusters, aggrs: chunk})
			continue
		}
		out = append(out, rootedVolumeLabelsQuery{clusters: chunk})
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
//
// The family is OPTIONAL, so a failed query logs and contributes nothing — it
// costs the aggregate edges of the claims whose volumes that query carried and
// never the build. Only the CALLER going away fails the build, which is
// optionalQueryFatal's rule for every optional leg.
func issueVolumeLabelsQueries(
	ctx, callerCtx context.Context,
	q promql.Querier,
	end time.Time,
	phase string,
	rendered []string,
) (model.Vector, error) {
	parts := make([]model.Vector, len(rendered))
	// WithContext, so the one error this path can return — the CALLER went
	// away — cancels the chunks still in flight instead of making the build
	// wait out every remaining round-trip before it can report the timeout.
	// Mirrors issueScopedFamilies (scopedread.go).
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
				if cerr := optionalQueryFatal(callerCtx, qerr); cerr != nil {
					return cerr
				}
				slog.WarnContext(ctx, "optional rooted volume-label query failed; continuing with empty vector",
					"query", string(promql.QVolumeLabels),
					"phase", phase,
					"chunk", i,
					"error", qerr)
				return nil
			}
			parts[i] = out
			return nil
		})
	}
	if err := wave.Wait(); err != nil {
		return nil, err
	}
	var merged model.Vector
	for _, part := range parts {
		merged = append(merged, part...)
	}
	return merged, nil
}

// readRootedVolumeLabels is phase 1: the volume-label family restricted to the
// request's rooted ONTAP clusters and aggregates. It returns the leg's run
// function so the launch loop can slot it in where fetchOptional stood, and
// writes the result into the same topologyVectors slot the unrestricted leg
// does — nothing downstream can tell which read produced it, which is the point.
func readRootedVolumeLabels(
	ctx, callerCtx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	plan topologyPlan,
	v *topologyVectors,
	dst *model.Vector,
) func() error {
	return func() (err error) {
		// The same guard fetch / fetchOptional put on this leg. errgroup does
		// not propagate a goroutine panic to Wait, so an unrecovered one here
		// kills the process rather than becoming a sanitised 500 — and unlike
		// those wrappers this closure runs chunking and rendering of its own
		// before any per-query recover is in scope.
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

		chunks, ok := rootedVolumeLabelsChunks(plan.volumeClusters, plan.volumeAggrs, opts.qosScopeBatchBytes())
		rendered := make([]string, 0, len(chunks))
		for _, c := range chunks {
			query, rok := promql.RenderVolumeLabelsRooted(window, c.clusters, c.aggrs)
			if !rok {
				ok = false // an un-renderable restriction reads unrestricted
				break
			}
			rendered = append(rendered, query)
		}
		if !ok {
			// Too many chunks, or a value set that normalised away. Read the
			// family as it was read before the restriction existed: one query,
			// the same body, and a bound on what one request can ask for.
			// NEVER an empty vector — that would silently draw no storage.
			v.VolumeLabelsRestricted = false
			slog.WarnContext(ctx, "storage roots did not yield a bounded volume-label restriction; reading the family unrestricted",
				"query", string(promql.QVolumeLabels),
				"ontap_clusters", len(plan.volumeClusters),
				"aggrs", len(plan.volumeAggrs),
				"max_chunks", maxRootedVolumeLabelChunks)
			out, qerr := issueVolumeLabelsQueries(ctx, callerCtx, q, end, "unrestricted",
				[]string{promql.Render(promql.QVolumeLabels, window, opts.LabelKeys, promql.Selector{})})
			if qerr != nil {
				return qerr
			}
			*dst = out
			return nil
		}
		out, qerr := issueVolumeLabelsQueries(ctx, callerCtx, q, end, "roots", rendered)
		if qerr != nil {
			return qerr
		}
		*dst = out
		return nil
	}
}

// readTokenScopedVolumeLabels is phase 2: it re-reads the family restricted on
// `volume` to the derived tokens of the claims phase 1 matched, and merges the
// result into v.VolumeLabels.
//
// It waits on the claim-info family and on phase 1, and on nothing else. It is
// NOT issued when no claim matched: phase 1 then drew no edge for a candidate
// set to be recovered for, and a rooted component with no claim is still drawn
// from the aggregate, controller and policy families, which are unrestricted.
//
// Like every leg of this family it degrades on a query error rather than
// failing the build. A failed chunk can leave a pick over an incomplete
// candidate set — no worse than the whole-family failure the unrestricted read
// already degrades to, at chunk granularity.
func readTokenScopedVolumeLabels(
	ctx, callerCtx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	plan topologyPlan,
	v *topologyVectors,
	prerequisites ...<-chan struct{},
) (err error) {
	// Same reason as phase 1: matchedClaimTokens and the chunking below run
	// outside any per-query recover, in an errgroup goroutine whose panic
	// would otherwise take the process down.
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(ctx, "panic in token-scoped volume-label query",
				"query", string(promql.QVolumeLabels),
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			err = fmt.Errorf("panic in %s query: %v", promql.QVolumeLabels, rec)
		}
	}()

	for _, done := range prerequisites {
		select {
		case <-done:
		case <-ctx.Done():
			// A sibling leg failed (or the caller went away). The group already
			// carries that error; adding another would only mask it.
			return nil
		}
	}

	// Phase 1 may have fallen back to the unrestricted read (an unbounded root
	// set). Every candidate of every claim is then already in hand, so phase 2
	// could only re-fetch what the merge would discard. The flag is written
	// before phase 1 returns and read after its done-channel closes, which is
	// what orders the two.
	if !v.VolumeLabelsRestricted {
		return nil
	}
	// The FIELD, not opts.volumeKey(): readTopology stores the rewriter once so
	// the parse and every scope derive a claim's token identically, and
	// re-resolving it here would recompile every rewrite rule's regexp.
	//
	// Read as a field and never through v.volumeKey(). That method has a VALUE
	// receiver, so calling it on the shared *topologyVectors copies the whole
	// struct — every model.Vector slot the sibling first-wave legs are still
	// writing — which is a data race the detector catches. Nothing in the
	// fan-out may take a copy of this struct; that is why readScopedQoS reaches
	// for opts.volumeKey() instead. VolumeKey itself is written once, before
	// the errgroup starts, so reading the one field is safe.
	rw := v.VolumeKey
	if rw == nil {
		rw = defaultVolumeKeyRewriter()
	}
	kind := rw.tokenScope()
	if kind == tokenScopeNone {
		return nil // unreachable under the predicate; never guess a branch shape
	}
	tokens := matchedClaimTokens(v.PVCInfo, v.VolumeLabels, rw)
	if len(tokens) == 0 {
		return nil
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
	// only sound with no aggregate root: phase 1 then read part of a cluster,
	// and a candidate on another aggregate of the same cluster is exactly what
	// phase 2 must still find.
	var excludeClusters []string
	if len(plan.volumeAggrs) == 0 {
		excludeClusters = plan.volumeClusters
	}
	chunks := promql.ChunkScopeWithOverhead(tokens, opts.qosScopeBatchBytes(), overhead)
	rendered := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		query, ok := promql.RenderVolumeLabelsTokenScoped(window, chunk, suffix, excludeClusters)
		if !ok {
			// Unreachable for a non-empty chunk. Degrading is the honest
			// answer for this OPTIONAL family: the candidate set stays
			// phase 1's, which is what a failed phase-2 chunk already leaves.
			slog.WarnContext(ctx, "no token-scoped rendering for the volume-label read; keeping the rooted candidate set",
				"query", string(promql.QVolumeLabels), "tokens", len(chunk))
			return nil
		}
		rendered = append(rendered, query)
	}
	extra, qerr := issueVolumeLabelsQueries(ctx, callerCtx, q, end, "tokens", rendered)
	if qerr != nil {
		return qerr
	}
	v.VolumeLabels = mergeVolumeLabels(v.VolumeLabels, extra)
	return nil
}
