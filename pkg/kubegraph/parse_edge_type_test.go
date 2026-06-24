package kubegraph_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/graph"
	"github.com/marz32one/kube-state-graph/pkg/kubegraph"
)

// edgeTypeBaseValues returns the minimal valid query so each case only varies
// edge_type.
func edgeTypeBaseValues() url.Values {
	return url.Values{
		"start": {"1700000000"},
		"end":   {"1700003600"},
	}
}

// Every entry in the graph.EdgeTypes registry must be accepted — the validator
// is derived from the same registry that backs /v1/edge-types, so the two can
// never drift.
func TestParseValues_EdgeType_AcceptsEveryRegistryEntry(t *testing.T) {
	require.NotEmpty(t, graph.EdgeTypes)
	for _, def := range graph.EdgeTypes {
		t.Run(string(def.Type), func(t *testing.T) {
			v := edgeTypeBaseValues()
			v.Set("edge_type", string(def.Type))
			_, _, scope, err := kubegraph.ParseValues(v)
			require.NoError(t, err)
			assert.Contains(t, scope.EdgeTypes, def.Type)
		})
	}
}

func TestParseValues_EdgeType_AcceptsMultipleValid(t *testing.T) {
	v := edgeTypeBaseValues()
	v["edge_type"] = []string{"pod-calls-pod", "pod-mounts-pvc"}
	_, _, scope, err := kubegraph.ParseValues(v)
	require.NoError(t, err)
	assert.Len(t, scope.EdgeTypes, 2)
}

// An unregistered edge_type (e.g. the plural typo "pod-calls-pods") is a 400,
// not a silent filter-everything-out 200. The message names the offending
// value so the caller can fix it.
func TestParseValues_EdgeType_UnknownRejected(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"plural typo", "pod-calls-pods"},
		{"arbitrary string", "bogus"},
		{"case mismatch", "Pod-Calls-Pod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := edgeTypeBaseValues()
			v.Set("edge_type", tc.value)
			_, _, _, err := kubegraph.ParseValues(v)
			require.Error(t, err)
			var pe *kubegraph.ParseError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, "invalid_scope", pe.Reason)
			assert.Contains(t, pe.Message, tc.value)
		})
	}
}

// Multiple edge_type values are each validated — one unknown among valid
// values still rejects the request.
func TestParseValues_EdgeType_UnknownAmongValidRejected(t *testing.T) {
	v := edgeTypeBaseValues()
	v["edge_type"] = []string{"pod-calls-pod", "pod-calls-pods"}
	_, _, _, err := kubegraph.ParseValues(v)
	require.Error(t, err)
	var pe *kubegraph.ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "invalid_scope", pe.Reason)
	assert.Contains(t, pe.Message, "pod-calls-pods")
}

// An empty edge_type value stays ignored (graph.NewScope drops empty strings),
// preserving the pre-validation behaviour for `?edge_type=`.
func TestParseValues_EdgeType_EmptyValueIgnored(t *testing.T) {
	v := edgeTypeBaseValues()
	v.Set("edge_type", "")
	_, _, scope, err := kubegraph.ParseValues(v)
	require.NoError(t, err)
	assert.Empty(t, scope.EdgeTypes)
}
