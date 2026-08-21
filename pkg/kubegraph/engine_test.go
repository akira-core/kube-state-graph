package kubegraph_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// ParseValues is the single source of truth for the /v1/graph request contract,
// shared by the HTTP handler and Engine.BuildFromValues. These cases lock the
// stable reason codes the handler maps to HTTP 400.
func TestParseValues_Errors(t *testing.T) {
	cases := []struct {
		name       string
		values     url.Values
		wantReason string
	}{
		{"missing start", url.Values{"end": {"1700003600"}}, "missing_start"},
		{"missing end", url.Values{"start": {"1700000000"}}, "missing_end"},
		{"invalid start", url.Values{"start": {"nope"}, "end": {"1700003600"}}, "invalid_start"},
		{"invalid end", url.Values{"start": {"1700000000"}, "end": {"nope"}}, "invalid_end"},
		{"end not after start", url.Values{"start": {"1700003600"}, "end": {"1700000000"}}, "invalid_range"},
		{"prune not a boolean", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "prune": {"maybe"}}, "invalid_scope"},
		{"selector value with a control character", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "env": {"prod\n"}}, "invalid_scope"},
		{"selector value too long", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {strings.Repeat("z", 254)}}, "invalid_scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := kubegraph.ParseValues(tc.values)
			require.Error(t, err)
			var pe *kubegraph.ParseError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, tc.wantReason, pe.Reason)
		})
	}
}

func TestParseValues_HappyPath(t *testing.T) {
	req, err := kubegraph.ParseValues(url.Values{
		"start": {"1700000000"},
		"end":   {"1700003600"},
	})
	require.NoError(t, err)
	assert.True(t, req.End.After(req.Start))
	assert.Equal(t, time.Hour, req.End.Sub(req.Start))
}

// BuildFromValues runs the full parse → build → project → serialise pipeline in
// one call. With an empty upstream it yields the canonical empty body shape —
// proving the facade wiring (and that an embedder needs no metrics/clock).
func TestBuildFromValues_EmptyUpstream(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Vector{}, nil).Maybe()

	eng := kubegraph.New(q, kubegraph.Options{APITimeout: 5 * time.Second})

	body, err := eng.BuildFromValues(context.Background(), url.Values{
		"start": {"1700000000"},
		"end":   {"1700003600"},
	})
	require.NoError(t, err)
	assert.Equal(t, "v1", body.APIVersion)
	assert.Empty(t, body.Elements.Nodes)
	assert.Empty(t, body.Elements.Edges)
}

// Probe issues an up{} query through the engine's querier — reachability for a
// readiness check, surfacing the upstream error verbatim.
func TestEngine_Probe(t *testing.T) {
	t.Run("reachable", func(t *testing.T) {
		q := promqlmocks.NewMockQuerier(t)
		q.EXPECT().Instant(mock.Anything, "up", mock.Anything, mock.Anything).
			Return(model.Vector{}, nil)
		require.NoError(t, kubegraph.New(q, kubegraph.Options{}).Probe(context.Background()))
	})
	t.Run("unreachable", func(t *testing.T) {
		q := promqlmocks.NewMockQuerier(t)
		q.EXPECT().Instant(mock.Anything, "up", mock.Anything, mock.Anything).
			Return(nil, errors.New("dial tcp: connection refused"))
		require.Error(t, kubegraph.New(q, kubegraph.Options{}).Probe(context.Background()))
	})
}

// A parse failure surfaces as a *ParseError before any build is attempted.
func TestBuildFromValues_ParseErrorShortCircuits(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t) // no Instant expectation: must not be called

	eng := kubegraph.New(q, kubegraph.Options{})

	_, err := eng.BuildFromValues(context.Background(), url.Values{"end": {"1700003600"}})
	var pe *kubegraph.ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "missing_start", pe.Reason)
}

// The four selector-level parameters must reach BOTH the upstream selector and
// (for cluster / namespace) the projection scope, and `prune` must invert onto
// Scope.Inventory so the default request keeps the connectivity prune on.
func TestParseValues_SelectorAndScope(t *testing.T) {
	req, err := kubegraph.ParseValues(url.Values{
		"start":     {"1700000000"},
		"end":       {"1700003600"},
		"cluster":   {"cluster-alpha"},
		"namespace": {"shop", "billing"},
		"az":        {"zone-a"},
		"env":       {"prod"},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"zone-a"}, req.Selector.AZ)
	assert.Equal(t, []string{"prod"}, req.Selector.Env)
	assert.Equal(t, []string{"cluster-alpha"}, req.Selector.Cluster)
	assert.Equal(t, []string{"shop", "billing"}, req.Selector.Namespace)
	assert.True(t, req.Selector.Active())

	assert.Contains(t, req.Scope.Clusters, "cluster-alpha")
	assert.Contains(t, req.Scope.Namespaces, "shop")
	assert.Contains(t, req.Scope.Namespaces, "billing")
	assert.False(t, req.Scope.Inventory, "prune defaults to true")
}

func TestParseValues_PruneFalseSetsInventory(t *testing.T) {
	base := url.Values{"start": {"1700000000"}, "end": {"1700003600"}}

	req, err := kubegraph.ParseValues(base)
	require.NoError(t, err)
	assert.False(t, req.Scope.Inventory)
	assert.False(t, req.Selector.Active(), "no selector-level parameter ⇒ unfiltered build")

	withPrune := url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "prune": {"false"}}
	req, err = kubegraph.ParseValues(withPrune)
	require.NoError(t, err)
	assert.True(t, req.Scope.Inventory)
	assert.False(t, req.Selector.Active(), "prune is projection-only — it never filters upstream")
}

// The withdrawn parameters are ignored, not rejected: an old client gets the
// unanchored view instead of a 400.
func TestParseValues_WithdrawnParametersIgnored(t *testing.T) {
	req, err := kubegraph.ParseValues(url.Values{
		"start":     {"1700000000"},
		"end":       {"1700003600"},
		"name":      {"checkout"},
		"root":      {"cluster-alpha/abc"},
		"depth":     {"99"},
		"direction": {"sideways"},
	})
	require.NoError(t, err)
	assert.False(t, req.Selector.Active())
	assert.False(t, req.Scope.Inventory)
	assert.Empty(t, req.Scope.Clusters)
}

// BuildFromValues must push the request's selector into the topology queries
// while leaving the service-graph queries untouched — the contract an embedder
// inherits without writing any of the plumbing itself.
func TestBuildFromValues_PushesSelectorIntoTopologyQueries(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name, query string, _ time.Time) (model.Vector, error) {
			mu.Lock()
			seen[name] = query
			mu.Unlock()
			return model.Vector{}, nil
		}).Maybe()

	eng := kubegraph.New(q, kubegraph.Options{APITimeout: 5 * time.Second})
	_, err := eng.BuildFromValues(context.Background(), url.Values{
		"start":     {"1700000000"},
		"end":       {"1700003600"},
		"namespace": {"shop"},
		"az":        {"zone-a"},
	})
	require.NoError(t, err)

	assert.Equal(t, `last_over_time(kube_pod_info{az="zone-a",namespace="shop"}[1h])`, seen["kube_pod_info"])
	assert.Equal(t,
		`rate(traces_service_graph_request_total{client!~"user|unknown",server!~"user"}[1h])`,
		seen["traces_service_graph_request_total"])
}

// A bare `?namespace=` is a no-op: it renders no matcher, so the build must
// stay unfiltered (retention classification on, service-graph resolver rules
// disarmed) rather than silently switching mode.
func TestParseValues_EmptyFilterValueIsNoOp(t *testing.T) {
	req, err := kubegraph.ParseValues(url.Values{
		"start":     {"1700000000"},
		"end":       {"1700003600"},
		"namespace": {""},
		"az":        {""},
	})
	require.NoError(t, err)
	assert.False(t, req.Selector.Active())
	assert.Empty(t, req.Scope.Namespaces)
}
