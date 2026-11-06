package kubegraph_test

import (
	"context"
	"encoding/json"
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

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
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
		// An invalid byte decodes to U+FFFD when ranged over, which is not a
		// control rune, so the control-character scan alone would let it
		// through into a rendered PromQL string literal.
		{"selector value with an invalid UTF-8 byte", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "namespace": {"\xffpayments"}}, "invalid_scope"},
		{"cluster value with a lone surrogate half", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "cluster": {"\xed\xa0\x80"}}, "invalid_scope"},
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

	body, err := eng.BuildFromValues(t.Context(), url.Values{
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
func TestBuildStorageFromValues_EmptyUpstream(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Vector{}, nil).Maybe()

	eng := kubegraph.New(q, kubegraph.Options{APITimeout: 5 * time.Second})
	body, err := eng.BuildStorageFromValues(t.Context(), url.Values{
		"start": {"1700000000"},
		"end":   {"1700003600"},
		"az":    {"zone-a"},
		"env":   {"prod"},
		"aggr":  {"aggr1"},
	})
	require.NoError(t, err)
	assert.Equal(t, "v1", body.APIVersion)
	assert.Empty(t, body.Elements.Nodes)
	assert.Empty(t, body.Elements.Edges)
	assert.Empty(t, body.Clusters)
}

func TestBuildStorageFromValues_ParseErrorReason(t *testing.T) {
	eng := kubegraph.New(promqlmocks.NewMockQuerier(t), kubegraph.Options{})
	_, err := eng.BuildStorageFromValues(t.Context(), url.Values{
		"start": {"1700000000"},
		"end":   {"1700003600"},
		"env":   {"prod"},
	})
	var pe *kubegraph.ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "missing_az", pe.Reason)
}

func TestBuildStorageFromValues_AgreesWithComposedPipeline(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Vector{}, nil).Maybe()
	eng := kubegraph.New(q, kubegraph.Options{APITimeout: 5 * time.Second})
	vals := url.Values{
		"start": {"1700000000"},
		"end":   {"1700003600"},
		"az":    {"zone-a"},
		"env":   {"prod"},
		"aggr":  {"aggr1"},
	}

	facade, err := eng.BuildStorageFromValues(t.Context(), vals)
	require.NoError(t, err)

	req, err := kubegraph.ParseStorageValues(vals)
	require.NoError(t, err)
	g, err := eng.BuildStorage(t.Context(), req.End.Sub(req.Start), req.End, req.Selector, req.Scope.Roots)
	require.NoError(t, err)
	composed := cytoscape.Serialise(g, graph.ProjectStorage(g, req.Scope))
	assert.Equal(t, composed, facade)
}

func TestBuildStorageFromValues_IORollupMatchesBuildStorageWith(t *testing.T) {
	vals := url.Values{
		"start":     {"1700000000"},
		"end":       {"1700003600"},
		"az":        {"zone-a"},
		"env":       {"prod"},
		"aggr":      {"aggr1"},
		"io_rollup": {"avg"},
	}
	fromValues := promqlfake.New(namespaceEstate())
	typed := promqlfake.New(namespaceEstate())
	body, err := kubegraph.New(fromValues, kubegraph.Options{}).BuildStorageFromValues(t.Context(), vals)
	require.NoError(t, err)

	req, err := kubegraph.ParseStorageValues(vals)
	require.NoError(t, err)
	start, end := kubegraph.AlignWindow(req.Start, req.End, kubegraph.DefaultEndAlign)
	g, err := kubegraph.New(typed, kubegraph.Options{}).BuildStorageWith(t.Context(),
		end.Sub(start), end, req.Selector, req.Scope.Roots,
		build.StorageOptions{IORollup: promql.RollupAvg})
	require.NoError(t, err)
	assert.Equal(t, cytoscape.Serialise(g, graph.ProjectStorage(g, req.Scope)), body)

	for _, q := range []*promqlfake.Querier{fromValues, typed} {
		issued := q.QueriesFor(promql.QQoSReadOps)
		require.NotEmpty(t, issued)
		for _, query := range issued {
			assert.True(t, strings.HasPrefix(query, "avg_over_time("), query)
		}
	}
}

func TestBuildStorageFromValues_InvalidIORollup(t *testing.T) {
	q := promqlfake.New(namespaceEstate())
	_, err := kubegraph.New(q, kubegraph.Options{}).BuildStorageFromValues(t.Context(), url.Values{
		"start":     {"1700000000"},
		"end":       {"1700003600"},
		"az":        {"zone-a"},
		"env":       {"prod"},
		"aggr":      {"aggr1"},
		"io_rollup": {"p99"},
	})
	pe, ok := errors.AsType[*kubegraph.ParseError](err)
	require.True(t, ok)
	assert.Equal(t, "invalid_scope", pe.Reason)
	assert.Empty(t, q.Issued())
}

func TestEngineBuildStorage_QoSStaysLast(t *testing.T) {
	q := promqlfake.New(namespaceEstate())
	end := time.Unix(1700003600, 0).UTC()
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootAggr, []string{"aggr1"})
	require.NoError(t, err)
	_, err = kubegraph.New(q, kubegraph.Options{}).BuildStorage(t.Context(),
		time.Hour, end, promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}, scope.Roots)
	require.NoError(t, err)
	issued := q.QueriesFor(promql.QQoSReadOps)
	require.NotEmpty(t, issued)
	for _, query := range issued {
		assert.True(t, strings.HasPrefix(query, "last_over_time("), query)
	}
}

func TestEngine_Probe(t *testing.T) {
	t.Run("reachable", func(t *testing.T) {
		q := promqlmocks.NewMockQuerier(t)
		q.EXPECT().Instant(mock.Anything, "up", mock.Anything, mock.Anything).
			Return(model.Vector{}, nil)
		require.NoError(t, kubegraph.New(q, kubegraph.Options{}).Probe(t.Context()))
	})
	t.Run("unreachable", func(t *testing.T) {
		q := promqlmocks.NewMockQuerier(t)
		q.EXPECT().Instant(mock.Anything, "up", mock.Anything, mock.Anything).
			Return(nil, errors.New("dial tcp: connection refused"))
		require.Error(t, kubegraph.New(q, kubegraph.Options{}).Probe(t.Context()))
	})
}

// A parse failure surfaces as a *ParseError before any build is attempted.
func TestBuildFromValues_ParseErrorShortCircuits(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t) // no Instant expectation: must not be called

	eng := kubegraph.New(q, kubegraph.Options{})

	_, err := eng.BuildFromValues(t.Context(), url.Values{"end": {"1700003600"}})
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
			// One pod keeps the topology non-empty: a filtered build that
			// loads nothing skips the service-graph read, and this test
			// asserts what those queries look like when issued.
			if name == "kube_pod_info" {
				return model.Vector{{Metric: model.Metric{
					"cluster": "cluster-alpha", "namespace": "shop", "pod": "checkout", "uid": "alpha-1",
				}, Value: 1}}, nil
			}
			return model.Vector{}, nil
		}).Maybe()

	eng := kubegraph.New(q, kubegraph.Options{APITimeout: 5 * time.Second})
	_, err := eng.BuildFromValues(t.Context(), url.Values{
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

func TestBuildStorageFromValues_IODeltaMatchesBuildStorageWith(t *testing.T) {
	vals := url.Values{
		"start":    {"1700000000"},
		"end":      {"1700003600"},
		"az":       {"zone-a"},
		"env":      {"prod"},
		"aggr":     {"aggr1"},
		"io_delta": {"true"},
	}
	// Both fakes answer the delta query, so the equality below compares two
	// bodies that carry changes rather than two that carry none.
	fromValues := promqlfake.New(ioDeltaEstate())
	fromValues.Delta = readOpsDelta()
	typed := promqlfake.New(ioDeltaEstate())
	typed.Delta = readOpsDelta()
	body, err := kubegraph.New(fromValues, kubegraph.Options{}).BuildStorageFromValues(t.Context(), vals)
	require.NoError(t, err)
	assert.Equal(t, wantAggr1ReadOps, bodyReadOps(body.Elements.Edges),
		"data.metrics carries read_ops_delta on every tier")
	for _, e := range body.Elements.Edges {
		if e.Data.Labels["tier"] == graph.StorageTierNodeAggr {
			wire, err := json.Marshal(e.Data.Metrics)
			require.NoError(t, err)
			assert.JSONEq(t, `{"read_ops":200,"read_ops_delta":-27.5}`, string(wire))
		}
	}

	req, err := kubegraph.ParseStorageValues(vals)
	require.NoError(t, err)
	start, end := kubegraph.AlignWindow(req.Start, req.End, kubegraph.DefaultEndAlign)
	g, err := kubegraph.New(typed, kubegraph.Options{}).BuildStorageWith(t.Context(),
		end.Sub(start), end, req.Selector, req.Scope.Roots,
		build.StorageOptions{IODelta: true})
	require.NoError(t, err)
	orders := claimIO(t, g, ordersClaimID)
	assert.Equal(t, "read_ops=100 read_ops_delta=12.5", fmtReadOps(orders.ReadOps, orders.ReadOpsDelta))
	assert.Equal(t, cytoscape.Serialise(g, graph.ProjectStorage(g, req.Scope)), body)

	for _, q := range []*promqlfake.Querier{fromValues, typed} {
		var deltas int
		for _, family := range promql.QoSWorkloadQueries {
			for _, query := range q.QueriesFor(family) {
				if strings.Contains(query, "] offset ") {
					deltas++
				}
			}
		}
		assert.Equal(t, len(promql.QoSWorkloadQueries), deltas, "one delta query per QoS family")
	}
}

func TestBuildStorageFromValues_InvalidIODelta(t *testing.T) {
	q := promqlfake.New(namespaceEstate())
	_, err := kubegraph.New(q, kubegraph.Options{}).BuildStorageFromValues(t.Context(), url.Values{
		"start":    {"1700000000"},
		"end":      {"1700003600"},
		"az":       {"zone-a"},
		"env":      {"prod"},
		"aggr":     {"aggr1"},
		"io_delta": {"yes"},
	})
	pe, ok := errors.AsType[*kubegraph.ParseError](err)
	require.True(t, ok)
	assert.Equal(t, "invalid_scope", pe.Reason)
	assert.Empty(t, q.Issued())
}

func TestEngineBuildStorage_IssuesNoDeltaQuery(t *testing.T) {
	q := promqlfake.New(namespaceEstate())
	end := time.Unix(1700003600, 0).UTC()
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootAggr, []string{"aggr1"})
	require.NoError(t, err)
	_, err = kubegraph.New(q, kubegraph.Options{}).BuildStorage(t.Context(),
		time.Hour, end, promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}, scope.Roots)
	require.NoError(t, err)
	for _, issued := range q.Issued() {
		assert.NotContains(t, issued.Query, "] offset ", issued.Query)
	}
}
