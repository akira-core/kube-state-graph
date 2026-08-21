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
	_, err := NewScope(nil, nil, []string{"pod-calls-pods"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"pod-calls-pods"`, "error must name the offending value")
}

func TestNewScope_RegisteredEdgeTypesAccepted(t *testing.T) {
	for _, def := range EdgeTypes {
		s, err := NewScope(nil, nil, []string{string(def.Type)}, false)
		require.NoError(t, err, "registered type %q must be accepted", def.Type)
		assert.Contains(t, s.EdgeTypes, def.Type)
	}
}

func TestNewScope_EmptyEdgeTypeValueIsNoOp(t *testing.T) {
	s, err := NewScope(nil, nil, []string{""}, false)
	require.NoError(t, err, "a bare `edge_type=` value must stay a no-op")
	assert.Empty(t, s.EdgeTypes)
}

// Inventory maps the request's `prune` parameter onto the projection: the
// ZERO Scope must keep the default prune on, so the flag is stored inverted.
func TestNewScope_InventoryFlag(t *testing.T) {
	s, err := NewScope(nil, nil, nil, false)
	require.NoError(t, err)
	assert.False(t, s.Inventory, "prune=true (the default) leaves Inventory unset")

	s, err = NewScope(nil, nil, nil, true)
	require.NoError(t, err)
	assert.True(t, s.Inventory)
}
