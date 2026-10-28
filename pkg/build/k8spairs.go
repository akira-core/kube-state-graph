package build

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// The read primitives of the Kubernetes-only build (readK8sWorkload). A read of
// known pods or claims is restricted on a namespace alternation and a name
// alternation together, one query per chunk, instead of one query per
// namespace. The two alternations are independent, so a chunk can match a cross
// pair — a name requested in one namespace that also exists in another — and the
// caller drops those rows before anything is scoped from them.

// pairChunk is one query's restriction: a namespace alternation and a name
// alternation.
type pairChunk struct {
	namespaces, names []string
}

// chunksOfPairs splits pairs into the restrictions ChunkNamePairs packs them
// under the byte budget and promql.MaxAlternationValues per alternation.
func chunksOfPairs(pairs []promql.NamePair, label string, budget int) []pairChunk {
	chunked := promql.ChunkNamePairs(pairs, label, budget)
	out := make([]pairChunk, 0, len(chunked))
	for _, c := range chunked {
		pc := pairChunk{namespaces: make([]string, 0, len(c)), names: make([]string, 0, len(c))}
		for _, p := range c {
			pc.namespaces = append(pc.namespaces, p.Namespace)
			pc.names = append(pc.names, p.Name)
		}
		out = append(out, pc)
	}
	return out
}

// pairRead is one family read restricted to namespaced pairs. It carries either
// the pairs, which issuePairReads chunks, or the chunks themselves — the pod
// root's seed split, which its sibling reads must follow. rows is filled by
// issuePairReads with every row the restriction matched, merged in chunk order
// and NOT filtered: what to keep depends on the caller's notion of "the same
// pod" or "the same claim".
type pairRead struct {
	query  promql.Query
	label  string
	pairs  []promql.NamePair
	chunks []pairChunk
	rows   model.Vector
}

// nameRead is one family read restricted to a set of names (a Kubernetes node or
// a controller), whose rows are appended to slot once the round that issued it
// has finished.
type nameRead struct {
	query promql.Query
	scope []string
	slot  *model.Vector
	rows  model.Vector
}

// k8sRead is the state one Kubernetes-only build threads through its rounds.
type k8sRead struct {
	ctx    context.Context
	q      promql.Querier
	window time.Duration
	end    time.Time
	opts   Options
	sel    promql.Selector
	plan   topologyPlan
	v      *topologyVectors
	mu     *sync.Mutex
	// lk is opts.LabelKeys with its defaults applied, the keys every row key is
	// read through.
	lk promql.LabelKeys

	ctrl      controllerReads
	nodesRead map[string]struct{}
}

// pairBudget is the byte budget one chunk of q's pair restriction may spend on
// its two alternations: the shared batch size less the request matchers every
// chunk repeats.
func (r *k8sRead) pairBudget(q promql.Query) int {
	return max(r.opts.qosScopeBatchBytes()-promql.RequestMatcherCost(q, r.opts.LabelKeys, r.sel), 1)
}

// parallel runs fns concurrently as one round and returns the first error. Each
// function recovers its own panic: errgroup does not propagate one, and scope
// computation runs outside any per-query recover.
func (r *k8sRead) parallel(name promql.Query, fns ...func(ctx context.Context) error) error {
	g, gctx := errgroup.WithContext(r.ctx)
	for _, fn := range fns {
		g.Go(func() (err error) {
			defer recoverScopedPanic(gctx, name, &err)
			return fn(gctx)
		})
	}
	return g.Wait()
}

// issuePairReads issues every chunk of every read under one bounded errgroup and
// fills each read's rows with its chunks' results in chunk order, so the vector
// is a pure function of the pair set rather than of upstream timing. A read with
// nothing to restrict is neither issued nor marked issued, the same "absent
// means never read" contract every other second wave keeps. The first chunk
// error fails the read, named with its family.
func (r *k8sRead) issuePairReads(ctx context.Context, reads []pairRead) error {
	chunks := make([][]pairChunk, len(reads))
	parts := make([][]model.Vector, len(reads))
	for i := range reads {
		chunks[i] = reads[i].chunks
		if chunks[i] == nil && len(reads[i].pairs) > 0 {
			chunks[i] = chunksOfPairs(reads[i].pairs, reads[i].label, r.pairBudget(reads[i].query))
		}
		parts[i] = make([]model.Vector, len(chunks[i]))
	}

	wave, wctx := errgroup.WithContext(ctx)
	wave.SetLimit(scopeConcurrency)
	for ri := range reads {
		read := reads[ri]
		for ci, chunk := range chunks[ri] {
			wave.Go(func() error {
				out, err := issueScopedChunk(wctx, r.q, read.query, r.window, r.end, r.opts.LabelKeys, r.sel, nil,
					func([]string) (string, bool) {
						return promql.RenderNamesAcrossNamespaces(read.query, r.window, r.opts.LabelKeys, r.sel,
							read.label, chunk.namespaces, chunk.names)
					})
				if err != nil {
					return err
				}
				parts[ri][ci] = out
				return nil
			})
		}
	}
	if err := wave.Wait(); err != nil {
		return err
	}
	for ri := range reads {
		reads[ri].rows = nil
		if len(chunks[ri]) == 0 {
			continue
		}
		for _, part := range parts[ri] {
			reads[ri].rows = append(reads[ri].rows, part...)
		}
		markScopeIssued(r.v, r.mu, reads[ri].query)
	}
	return nil
}

// issueNameReads issues each read restricted to its own name scope, chunked
// under the shared byte budget. A read whose scope is empty is neither issued
// nor tallied.
func (r *k8sRead) issueNameReads(ctx context.Context, reads []nameRead) error {
	families := make([]scopedFamily, 0, len(reads))
	for i := range reads {
		families = append(families, scopedFamily{query: reads[i].query, dst: &reads[i].rows, scope: reads[i].scope})
	}
	return issueScopedFamilies(ctx, r.q, r.window, r.end, r.opts, r.sel, r.v, r.mu, families)
}

// commitNameReads appends the rows of reads to their slots. It runs on the build
// goroutine once the round has finished, so no two goroutines touch a slot.
func commitNameReads(reads []nameRead) {
	for i := range reads {
		*reads[i].slot = append(*reads[i].slot, reads[i].rows...)
	}
}

// keepRequestedPairs lands the rows of a pair read that name a requested pair
// and tallies the rest — cross pairs the independent alternations matched — as
// series the restriction read but the body does not carry.
func (r *k8sRead) keepRequestedPairs(read pairRead, pairs []promql.NamePair) model.Vector {
	kept := promql.KeepNamePairs(read.rows, read.label, pairs)
	r.tallyDropped(read.query, len(read.rows)-len(kept))
	return kept
}

// tallyDropped counts series a read matched but did not land in a slot, so
// RawSeriesCount still reports what the upstream returned.
func (r *k8sRead) tallyDropped(q promql.Query, n int) {
	if n > 0 {
		addExtraSeries(r.v, r.mu, q, n)
	}
}
