package promql

import (
	"context"
	"time"

	"github.com/prometheus/common/model"
)

// Querier is the minimal contract Server and Builder depend on. Production
// wiring uses *Client; tests inject a mockery-generated mock so unit tests do
// not need an httptest.NewServer fronting hand-rolled JSON fixtures.
//
// Defined on the consumer side per Go convention ("accept interfaces, return
// structs") would mean api + build each redeclare a near-identical interface;
// keeping it here avoids that duplication while *Client trivially satisfies it.
type Querier interface {
	Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error)
}

// Compile-time assertion that *Client satisfies Querier.
var _ Querier = (*Client)(nil)

// QuerierSource yields the Querier a single build must dispatch through, given
// that build's request-scoped Selector.
//
// It exists because Querier.Instant carries the query NAME but not the
// Selector, and the selector's `az` dimension is what picks an upstream
// backend (see the upstream-backend-routing capability). Three bridges were
// considered; this one was chosen because it changes no existing signature:
//
//   - widening Querier with a selector parameter breaks every mock, every test
//     helper, and every external embedder of the graph engine;
//   - smuggling the Selector through context.Value is invisible coupling, and a
//     caller that forgets to attach it silently fans out to everything;
//   - an OPTIONAL second interface, type-asserted by the consumer, leaves a
//     plain Querier behaving exactly as it does today.
//
// This is the same optional-upgrade shape build.BuildScopedRouteResolver
// already established for the route resolver.
//
// The returned Querier MUST close over one immutable routing snapshot, so a
// table reload cannot change which backends a build in flight dispatches to.
type QuerierSource interface {
	QuerierFor(sel Selector) Querier
}

// FamilyZoneQuerierSource is the OPTIONAL upgrade of QuerierSource a build
// type-asserts for when it must route some families by zone and others by
// none (read-storage-roots-through-volume-hub D8): a hub-mode storage build
// reads Harvest from the stores its `az` selects, and the kube-state-metrics,
// kubelet and alerts families from every store serving them.
//
// Two QuerierFor calls — one with the zone, one without — would load two
// routing snapshots and could straddle a reload, so a build could read from one
// table and the other half from another. The bound Querier this returns closes
// over ONE snapshot and makes the zone decision per family instead.
//
// A plain QuerierSource lacks it; a consumer then falls back to QuerierFor,
// which routes every zone-routable family by `az`. Same shape as QuerierSource,
// Prober, RouterMetrics and SeriesMetrics.
type FamilyZoneQuerierSource interface {
	QuerierSource
	// QuerierForFamilyZones binds ONE routing snapshot. Families in zoned
	// dispatch by sel.AZ exactly as QuerierFor does; every other family
	// dispatches with no zone, to every backend serving it.
	QuerierForFamilyZones(sel Selector, zoned ...Family) Querier
}

// Static adapts a plain Querier into a QuerierSource that ignores the selector
// and always yields the same Querier. It is the single-upstream case expressed
// in the routed vocabulary, so a consumer can hold a QuerierSource
// unconditionally.
func Static(q Querier) QuerierSource { return staticSource{q: q} }

type staticSource struct{ q Querier }

func (s staticSource) QuerierFor(Selector) Querier { return s.q }
