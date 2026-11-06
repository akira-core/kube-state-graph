package api

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/auth"
	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/internal/observability"
	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/clock"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// fixtureSet maps a query-substring needle to the model.Vector returned when
// the upstream Querier is asked a query containing that substring. Mirrors
// the substring-match semantics of the previous httptest-based promMock but
// without spinning a TCP listener.
//
// A needle built with deltaNeedle matches ONLY end-minus-start (delta) queries,
// and an unmarked needle matches only non-delta ones. Every substring of a
// `last` measurement query is also a substring of its delta query, so without
// the split a measurement needle and a delta needle for the same family would
// both match the delta query and fail as ambiguous. The ambiguity rule applies
// within each class.
type fixtureSet map[string]model.Vector

// deltaMark prefixes a fixtureSet needle that targets delta queries only. It
// is not a valid query byte, so it never collides with a real substring.
const deltaMark = "\x00delta\x00"

// deltaNeedle marks needle as matching only delta-shaped queries.
func deltaNeedle(needle string) string { return deltaMark + needle }

// isDeltaQuery reports whether query is the end-minus-start form
// promql.RenderQoSVolumeScopedDelta renders.
func isDeltaQuery(query string) bool { return strings.Contains(query, "] offset ") }

// fixtureNeedleMatches reports whether a fixtureSet needle applies to query,
// honouring the delta mark.
func fixtureNeedleMatches(needle, query string) bool {
	body, wantDelta := strings.CutPrefix(needle, deltaMark)
	if wantDelta != isDeltaQuery(query) {
		return false
	}
	return strings.Contains(query, body)
}

// newMockQuerier returns a Querier mock that responds to Instant by matching
// the query string against fixtures. No match → empty Vector. Any number of
// calls is allowed (Maybe()) so tests focusing on routing/auth do not fail
// when a handler happens not to query upstream.
func newMockQuerier(t *testing.T, fixtures fixtureSet) *promqlmocks.MockQuerier {
	t.Helper()
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, query string, _ time.Time) (model.Vector, error) {
			// Substring dispatch is order-sensitive: if two needles both match a
			// query, Go's randomised map iteration would pick a non-deterministic
			// response across runs. Fail loudly on ambiguity so an overlapping
			// fixtureSet is a clear test error, not a flaky one. t.Errorf (not
			// Fatalf) because this runs on parallel errgroup goroutines.
			var hits []string
			var match model.Vector
			for needle, vec := range fixtures {
				if fixtureNeedleMatches(needle, query) {
					hits = append(hits, needle)
					match = vec
				}
			}
			if len(hits) > 1 {
				sort.Strings(hits)
				t.Errorf("ambiguous fixture dispatch for query %q: matched needles %v", query, hits)
			}
			if len(hits) >= 1 {
				return match, nil
			}
			return model.Vector{}, nil
		}).
		Maybe()
	return q
}

// newErrQuerier returns a Querier mock whose every Instant call fails. Used
// for the upstream-failure / 502 path.
func newErrQuerier(t *testing.T, err error) *promqlmocks.MockQuerier {
	t.Helper()
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, err).
		Maybe()
	return q
}

// newStallQuerier returns a Querier mock whose every Instant call blocks until
// the context is cancelled, then returns ctx.Err(). Used to drive build-timeout
// (504) and readiness-timeout (503) paths deterministically.
func newStallQuerier(t *testing.T) *promqlmocks.MockQuerier {
	t.Helper()
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(ctx context.Context, _, _ string, _ time.Time) (model.Vector, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}).
		Maybe()
	return q
}

// newServerWithMocks constructs a Server backed by the supplied querier mock
// and an empty KeySet (auth disabled). override may tweak the Config before
// the server is built.
func newServerWithMocks(t *testing.T, q *promqlmocks.MockQuerier, override func(*config.Config)) *Server {
	t.Helper()
	return newServerWithMocksAndKeys(t, q, auth.NewKeySet(), override)
}

// newServerWithMocksAndKeys is like newServerWithMocks but lets the caller
// supply a populated *auth.KeySet so the API-key middleware can be exercised
// end-to-end. *auth.KeySet is a pure in-memory construct — using the real
// type here exercises the production validator instead of its mock.
func newServerWithMocksAndKeys(t *testing.T, q *promqlmocks.MockQuerier, ks *auth.KeySet, override func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.PromURL = "http://unused" // satisfies cfg.Validate; the mock bypasses HTTP
	if override != nil {
		override(&cfg)
	}
	require.NoError(t, cfg.Validate())

	logger := observability.NewLogger("error")
	metrics := observability.NewMetrics()
	// Mirror cmd/kube-state-graph's wiring so a test that rebinds a label key
	// through Config exercises the same path production does.
	builder := build.New(q, build.Options{
		APITimeout: cfg.APITimeout,
		LabelKeys:  promql.LabelKeys{AZ: cfg.AZLabel, Env: cfg.EnvLabel},
	}, metrics, clock.System{})
	return New(cfg, builder, q, metrics, logger, ks, clock.System{})
}

// vec is a small helper that builds a model.Vector from per-series label
// maps. Callers express expected upstream rows declaratively without writing
// JSON or worrying about timestamps (which the handlers never read).
func vec(samples ...map[string]string) model.Vector {
	out := make(model.Vector, 0, len(samples))
	for _, s := range samples {
		metric := make(model.Metric, len(s))
		for k, v := range s {
			metric[model.LabelName(k)] = model.LabelValue(v)
		}
		out = append(out, &model.Sample{Metric: metric, Value: 1, Timestamp: 0})
	}
	return out
}
