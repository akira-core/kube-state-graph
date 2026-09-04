package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// An unknown ?edge_type= value (e.g. the plural typo "pod-calls-pods") must be
// a 400 with the standard error envelope — not a 200 with every edge silently
// filtered out. /v1/edge-types documents the valid set; the parser validates
// against the same registry.
func TestGraphEndpoint_UnknownEdgeTypeRejected(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	q := url.Values{}
	q.Set("start", "2026-05-01T11:00:00Z")
	q.Set("end", "2026-05-01T12:00:00Z")
	q.Set("edge_type", "pod-calls-pods")
	resp, err := http.Get(srv.URL + "/v1/graph?" + q.Encode())
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var body struct {
		APIVersion string `json:"apiVersion"`
		Error      struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "v1", body.APIVersion)
	assert.Equal(t, "invalid_scope", body.Error.Reason)
	assert.Contains(t, body.Error.Message, "pod-calls-pods")
}

// A registered edge_type still passes validation and reaches the build → 200.
func TestGraphEndpoint_ValidEdgeTypeAccepted(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	q := url.Values{}
	q.Set("start", "2026-05-01T11:00:00Z")
	q.Set("end", "2026-05-01T12:00:00Z")
	q.Set("edge_type", "pod-calls-pod")
	resp, err := http.Get(srv.URL + "/v1/graph?" + q.Encode())
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// storage-flow is registered, so /v1/graph accepts it as an ?edge_type= value
// — and returns zero edges, because /v1/graph never emits that type. Both
// halves matter: rejecting it would make the registry and the parser disagree,
// and emitting anything would break the "storage-flow belongs to
// /v1/storage-graph alone" rule.
func TestGraphEndpoint_StorageFlowEdgeTypeAcceptedAndEmpty(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	q := url.Values{}
	q.Set("start", "2026-05-01T11:00:00Z")
	q.Set("end", "2026-05-01T12:00:00Z")
	q.Set("edge_type", string(graph.EdgeTypeStorageFlow))
	resp, err := http.Get(srv.URL + "/v1/graph?" + q.Encode())
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Elements struct {
			Edges []json.RawMessage `json:"edges"`
		} `json:"elements"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Empty(t, body.Elements.Edges, "/v1/graph never emits a storage-flow edge")
}
