// Package kubegraph is the convenience facade over the reusable graph engine.
// It folds request parsing, the multi-cluster build, projection, and Cytoscape
// serialisation into a single call so an embedding application can obtain the
// exact /v1/graph response body in-process, with no HTTP hop and no JSON
// round-trip. kube-state-graph's own HTTP handler shares the same parser
// (ParseValues) so the request contract cannot drift between the server and an
// embedded consumer.
package kubegraph

import (
	"context"
	"net/url"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/clock"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// Options configures an Engine. Clock and Metrics are optional: a nil Clock
// falls back to the system clock, and a nil Metrics disables build self-metrics
// (an embedder that does not want kube-state-graph's Prometheus series leaves it
// nil). APITimeout mirrors the build-layer setting.
type Options struct {
	// APITimeout bounds the cheap up{} retention probe inside the build.
	APITimeout time.Duration
	// Clock is the time source for "now"; nil means the system clock.
	Clock clock.Clock
	// Metrics records last-build observational gauges; nil means no-op.
	Metrics build.Metrics
	// RouteResolver mirrors build.Options.RouteResolver: an optional Istio
	// route-resolution engine for global-FQDN server="unknown" peers. Nil
	// (the default) disables the feature. An embedder that wants it imports
	// pkg/route itself and passes the resolver in — kubegraph deliberately
	// does not import pkg/route (design D1 dependency containment).
	RouteResolver build.RouteResolver
	// RouteResolveTimeout mirrors build.Options.RouteResolveTimeout.
	RouteResolveTimeout time.Duration
	// LabelKeys names the upstream labels the request's `az` / `env` filter
	// dimensions are matched against. Zero value ⇒ the defaults (`az`, `env`).
	LabelKeys promql.LabelKeys
}

// Engine wraps a build.Builder and exposes the build → project → serialise
// pipeline as a single call. Construct one per upstream Querier.
type Engine struct {
	builder *build.Builder
	q       promql.Querier
	clk     clock.Clock
}

// New constructs an Engine querying through q. The caller owns q (typically a
// *promql.Client built from a VictoriaMetrics URL, or any Querier).
func New(q promql.Querier, opts Options) *Engine {
	clk := opts.Clock
	if clk == nil {
		clk = clock.System{}
	}
	b := build.New(q, build.Options{
		APITimeout:          opts.APITimeout,
		RouteResolver:       opts.RouteResolver,
		RouteResolveTimeout: opts.RouteResolveTimeout,
		LabelKeys:           opts.LabelKeys,
	}, opts.Metrics, clk)
	return &Engine{builder: b, q: q, clk: clk}
}

// NewRouted constructs an Engine dispatching through a routing table, so a
// build's `az` values select which upstream installation answers each query
// family.
//
// It is New plus the routing seam made visible in the signature. Passing the
// router to New works identically — *promql.Router satisfies promql.Querier,
// and the builder type-asserts the promql.QuerierSource upgrade — but that is a
// fact an embedder would have to be told in prose; a named constructor states
// it instead (design D1 / D14).
//
// Build the table with promql.SingleBackendTable for a single upstream, with
// promql.NewTable for one assembled in code, or with backendsfile.Read for the
// file an operator mounts.
func NewRouted(r *promql.Router, opts Options) *Engine {
	return New(r, opts)
}

// Probe reports upstream reachability via a cheap up{} instant query — the same
// signal the build's retention check uses — suitable for a readiness check.
func (e *Engine) Probe(ctx context.Context) error {
	_, err := e.q.Instant(ctx, string(promql.QUpProbe), string(promql.QUpProbe), e.clk.Now().UTC())
	return err
}

// Build runs the multi-cluster build for [end-window, end] and returns the
// immutable graph. sel carries the request-scoped selector dimensions pushed
// into the upstream queries; a zero Selector is the unfiltered build. The
// caller supplies any build deadline via ctx.
func (e *Engine) Build(ctx context.Context, window time.Duration, end time.Time, sel promql.Selector) (*graph.Graph, error) {
	return e.builder.Build(ctx, window, end, sel)
}

// BuildFromValues parses the /v1/graph query parameters, builds the graph,
// applies the projection, and serialises to the Cytoscape body — the whole
// pipeline in one call. Parsing failures are returned as *ParseError (HTTP 400
// in kube-state-graph's API); build failures propagate the build layer's typed
// errors. The caller supplies any build deadline via ctx.
func (e *Engine) BuildFromValues(ctx context.Context, v url.Values) (cytoscape.Body, error) {
	req, err := ParseValues(v)
	if err != nil {
		return cytoscape.Body{}, err
	}
	g, err := e.builder.Build(ctx, req.End.Sub(req.Start), req.End, req.Selector)
	if err != nil {
		return cytoscape.Body{}, err
	}
	return cytoscape.Serialise(g, graph.Project(g, req.Scope)), nil
}
