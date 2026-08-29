package promql

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/prometheus/common/model"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
)

// RouterMetrics is the OPTIONAL upgrade interface a Metrics implementation may
// satisfy to receive routing observations. It is deliberately separate from
// Metrics rather than an extension of it: Metrics is exported and an embedder
// may implement it, so widening it would be a breaking change. The router
// type-asserts for this interface and no-ops when it is absent.
//
// The pre-existing kube_state_graph_upstream_query_* metrics keep their label
// sets — adding a `backend` label to an established self-metric is a contract
// change — so per-backend detail lives here instead.
type RouterMetrics interface {
	// SetBackends records the live routing table's backends by name. The
	// count feeds the backend gauge; the names let a recorder pre-create the
	// per-backend failure counter at zero, so "no failures" is visible as a
	// zero series rather than as an absent one.
	SetBackends(names []string)
	// IncBackendQueryFailure increments the failure counter for one backend.
	IncBackendQueryFailure(backend string)
	// IncBackendConfigReload increments the reload counter, labelled by
	// result ("ok", "error", or "unchanged").
	IncBackendConfigReload(result string)
}

// Reload results recorded through RouterMetrics.IncBackendConfigReload. They
// are metric label VALUES, so they are constants rather than free strings.
const (
	ReloadResultOK        = "ok"
	ReloadResultError     = "error"
	ReloadResultUnchanged = "unchanged"
)

// routerMetricsOf returns m as a RouterMetrics when it satisfies the optional
// upgrade, and a no-op otherwise. A nil Metrics, or one implementing only the
// two required methods, yields the no-op.
func routerMetricsOf(m Metrics) RouterMetrics {
	if rm, ok := m.(RouterMetrics); ok && rm != nil {
		return rm
	}
	return noopRouterMetrics{}
}

type noopRouterMetrics struct{}

func (noopRouterMetrics) SetBackends([]string)          {}
func (noopRouterMetrics) IncBackendQueryFailure(string) {}
func (noopRouterMetrics) IncBackendConfigReload(string) {}

// mergeVectors folds the per-backend results of one fan-out into a single
// vector.
//
// parts MUST already be in ascending backend-name order; the caller holds that
// order because Table keeps its backends sorted. Concatenation follows that
// order and a sample whose label set was already contributed is DROPPED, so a
// series held by two backends contributes exactly once and the surviving copy
// is the lexically-smallest backend's.
//
// De-duplication is required for correctness, not tidiness: several readers sum
// across contributing series — the service-graph request and failure totals
// most visibly — so a series present in two backends would multiply an edge's
// rate and error_rate by the number of backends holding it. A catch-all backend
// sitting alongside a per-zone one makes that overlap ordinary.
//
// The result is a pure function of the value SETS returned, never of response
// arrival order, which is what keeps the serialised response body
// byte-deterministic.
func mergeVectors(parts []model.Vector) (model.Vector, int) {
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	if total == 0 {
		return model.Vector{}, 0
	}

	out := make(model.Vector, 0, total)
	seen := make(map[model.Fingerprint]*model.Sample, total)
	conflicts := 0
	for _, part := range parts {
		for _, s := range part {
			if s == nil {
				continue
			}
			fp := s.Metric.Fingerprint()
			if kept, dup := seen[fp]; dup {
				// A duplicate carrying a DIFFERENT value is a genuine data
				// ambiguity — two stores disagreeing about one series. Keeping
				// the first (lexically-smallest backend) is deterministic;
				// counting it makes the disagreement visible without turning a
				// benign scrape overlap into an outage.
				if kept.Value != s.Value {
					conflicts++
				}
				continue
			}
			seen[fp] = s
			out = append(out, s)
		}
	}
	return out, conflicts
}

// fanoutQuerier is the Querier a Router hands to one build. It closes over a
// single immutable table snapshot, so a reload cannot change which backends
// this build's queries reach.
type fanoutQuerier struct {
	table   *Table
	clients map[string]Querier // backend name → client
	az      []string
	metrics Metrics
}

// Instant resolves the query's family, selects the backends that must answer
// it, issues the IDENTICAL query string to each, and merges the results.
//
// The query string is never rewritten per backend: routing decides WHICH store
// is asked, the rendered selector decides WHAT it returns, and the two compose
// rather than substitute for one another.
func (f *fanoutQuerier) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	fam, ok := FamilyOf(Query(name))
	if !ok {
		// Unreachable for code in this repository — the completeness test makes
		// every Query constant carry a family — but a caller passing an
		// arbitrary name must fail loudly rather than silently reach no store.
		return nil, fmt.Errorf("prom query %s: no upstream family declared for this query", name)
	}

	selected := f.table.Select(fam, f.az)
	if len(selected) == 0 {
		// A requested zone no backend declares. An empty filtered result is a
		// legitimate empty graph, not an error, so this returns an empty vector
		// — but it is logged, because it is otherwise indistinguishable from an
		// estate that genuinely holds nothing.
		slog.WarnContext(ctx, "no upstream backend serves this query for the requested zones",
			"query_name", name,
			"family", string(fam),
			"az", f.az,
		)
		return model.Vector{}, nil
	}

	parts := make([]model.Vector, len(selected))
	g, gctx := errgroup.WithContext(ctx)
	// The limit is the selection size, so the fan-out adds no unbounded
	// concurrency of its own. Zone restriction usually resolves this to one or
	// two backends; the build deadline remains the real ceiling.
	g.SetLimit(len(selected))
	for i, b := range selected {
		g.Go(func() error {
			q, ok := f.clients[b.Name()]
			if !ok {
				return fmt.Errorf("prom query %s: backend %q has no client", name, b.Name())
			}
			out, err := q.Instant(gctx, name, query, ts)
			if err != nil {
				routerMetricsOf(f.metrics).IncBackendQueryFailure(b.Name())
				// Naming the backend is the whole point: with six upstreams,
				// "upstream unreachable" is not actionable.
				return fmt.Errorf("backend %q: %w", b.Name(), err)
			}
			parts[i] = out
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		// Fail closed. A partial fan-out result is indistinguishable from a
		// smaller estate: missing pods lose their edges, the connectivity prune
		// then removes their nodes, claims and aggregates, and the response is
		// a plausible, smaller, WRONG graph. Legs the builder already treats as
		// optional keep degrading at their own level.
		return nil, err
	}

	merged, conflicts := mergeVectors(parts)
	if conflicts > 0 {
		slog.DebugContext(ctx, "upstream backends disagreed about a series value",
			"query_name", name,
			"family", string(fam),
			"conflicts", conflicts,
		)
	}
	if len(selected) > 1 {
		if span := trace.SpanFromContext(ctx); span.IsRecording() {
			span.SetAttributes(attribute.Int("kube_state_graph.backend_fanout", len(selected)))
		}
	}
	return merged, nil
}

// backendNames returns the selected backends' names, sorted — used by logs and
// by tests asserting a selection.
func backendNames(bs []Backend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name()
	}
	sort.Strings(out)
	return out
}
