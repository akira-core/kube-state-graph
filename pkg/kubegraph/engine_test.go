package kubegraph_test

import (
	"context"
	"errors"
	"net/url"
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
		{"non-integer depth", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "depth": {"x"}}, "invalid_depth"},
		{"negative depth", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "depth": {"-1"}}, "invalid_depth"},
		{"depth too large", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "root": {"c/p"}, "depth": {"99"}}, "depth_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := kubegraph.ParseValues(tc.values)
			require.Error(t, err)
			var pe *kubegraph.ParseError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, tc.wantReason, pe.Reason)
		})
	}
}

func TestParseValues_HappyPath(t *testing.T) {
	start, end, _, err := kubegraph.ParseValues(url.Values{
		"start": {"1700000000"},
		"end":   {"1700003600"},
	})
	require.NoError(t, err)
	assert.True(t, end.After(start))
	assert.Equal(t, time.Hour, end.Sub(start))
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
