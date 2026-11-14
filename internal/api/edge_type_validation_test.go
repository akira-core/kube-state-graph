package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `edge_type` is withdrawn. It joins `name` / `root` / `depth` / `direction`
// as an unknown parameter: ignored without error, its value never inspected.
//
// The assertion is byte-identity, not "still 200": the parameter used to be a
// projection-level gate over the edge list, so a request that kept it must now
// receive every edge type the default projection emits — here the
// pod-calls-pod edge it used to select FOR and the pod-to-node edge it used to
// filter OUT, in one body.
func TestGraphEndpoint_WithdrawnEdgeTypeDoesNotFilter(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, happyFixtures()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	base := url.Values{}
	base.Set("start", "2026-05-01T11:00:00Z")
	base.Set("end", "2026-05-01T12:00:00Z")

	unfiltered := getBody(t, srv.URL+"/v1/graph?"+base.Encode())

	var parsed struct {
		Elements struct {
			Edges []struct {
				Data struct {
					Type string `json:"type"`
				} `json:"data"`
			} `json:"edges"`
		} `json:"elements"`
	}
	require.NoError(t, json.Unmarshal(unfiltered, &parsed))
	types := map[string]bool{}
	for _, e := range parsed.Elements.Edges {
		types[e.Data.Type] = true
	}
	require.True(t, types["pod-calls-pod"], "fixture must produce a pod-calls-pod edge")
	require.True(t, types["pod-to-node"], "fixture must produce a pod-to-node edge the old filter would have dropped")

	for _, value := range []string{"pod-calls-pod", "storage-flow", ""} {
		t.Run(value, func(t *testing.T) {
			q := url.Values{}
			for k, vs := range base {
				q[k] = vs
			}
			q.Set("edge_type", value)
			assert.Equal(t, string(unfiltered), string(getBody(t, srv.URL+"/v1/graph?"+q.Encode())),
				"a withdrawn parameter must not change the body")
		})
	}
}

// An unregistered value used to be 400 invalid_scope, validated against the
// registry the withdrawn catalogue served. With the parameter unknown to the
// server its value is never read, so it is an ordinary 200.
func TestGraphEndpoint_UnregisteredEdgeTypeValueIsNotAnError(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, happyFixtures()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	q := url.Values{}
	q.Set("start", "2026-05-01T11:00:00Z")
	q.Set("end", "2026-05-01T12:00:00Z")
	q.Set("edge_type", "pod-calls-pods")

	resp, err := httpGet(t, srv.URL+"/v1/graph?"+q.Encode())
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func getBody(t *testing.T, u string) []byte {
	t.Helper()
	resp, err := httpGet(t, u)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return raw
}
