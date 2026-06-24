package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NewScope must reject edge types absent from the EdgeTypes registry: a typo
// like "pod-calls-pods" would otherwise build a scope that silently filters
// every edge out. The check lives here (not only in the HTTP parser) so D32
// embedders constructing scopes directly get the same guard.
func TestNewScope_UnknownEdgeTypeRejected(t *testing.T) {
	_, err := NewScope(nil, nil, []string{"pod-calls-pods"}, nil, "", 0, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"pod-calls-pods"`, "error must name the offending value")
}

func TestNewScope_RegisteredEdgeTypesAccepted(t *testing.T) {
	for _, def := range EdgeTypes {
		s, err := NewScope(nil, nil, []string{string(def.Type)}, nil, "", 0, "")
		require.NoError(t, err, "registered type %q must be accepted", def.Type)
		assert.Contains(t, s.EdgeTypes, def.Type)
	}
}

func TestNewScope_EmptyEdgeTypeValueIsNoOp(t *testing.T) {
	s, err := NewScope(nil, nil, []string{""}, nil, "", 0, "")
	require.NoError(t, err, "a bare `edge_type=` value must stay a no-op")
	assert.Empty(t, s.EdgeTypes)
}
