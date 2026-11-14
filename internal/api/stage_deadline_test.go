package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// serveWith runs serveGraph directly with a fake build and a projection that
// records whether it ran, so the stage checks are driven without timing.
func serveWith(t *testing.T, s *Server, reqCtx context.Context, run buildFunc) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/graph", nil).WithContext(reqCtx)
	projected := false
	start := time.Unix(1746442800, 0).UTC()
	s.serveGraph(c, start, start.Add(time.Hour), promql.Selector{}, run, "kube-state-graph.project",
		func(g *graph.Graph) graph.View {
			projected = true
			return graph.Project(g, graph.Scope{})
		})
	return rec, projected
}

func emptyGraph() *graph.Graph { return graph.NewGraph(nil, nil, time.Unix(0, 0)) }

func reasonOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	return body.Error.Reason
}

// Spec: "A client that leaves during the build".
func TestServeGraph_AClientThatLeavesStopsBeforeProjection(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	reqCtx, leave := context.WithCancel(t.Context())
	rec, projected := serveWith(t, s, reqCtx,
		func(context.Context, time.Duration, time.Time, promql.Selector) (*graph.Graph, error) {
			leave() // the client disconnects while the build runs; the build still returns a graph
			return emptyGraph(), nil
		})
	assert.False(t, projected, "the projection is not run for a client that has gone")
	assert.Equal(t, statusClientClosedRequest, rec.Code)
	assert.Equal(t, "canceled", reasonOf(t, rec))
}

// Spec: "A build that finishes at its deadline".
func TestServeGraph_ABuildPastItsDeadlineStopsBeforeProjection(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), func(c *config.Config) { c.BuildTimeout = 20 * time.Millisecond })
	rec, projected := serveWith(t, s, t.Context(),
		func(ctx context.Context, _ time.Duration, _ time.Time, _ promql.Selector) (*graph.Graph, error) {
			<-ctx.Done() // the build ignores its deadline and returns a graph after it
			return emptyGraph(), nil
		})
	assert.False(t, projected, "the projection is not run past the deadline")
	assert.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.Equal(t, "timeout", reasonOf(t, rec))
	assert.Contains(t, scrapeMetrics(t, s.metrics), `kube_state_graph_build_rejected_total{reason="timeout"} 1`)
}

// A projection that runs past the deadline stops before serialisation and the
// encode: the deadline covers every stage, not only the build.
func TestServeGraph_AProjectionPastTheDeadlineStopsBeforeEncoding(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), func(c *config.Config) { c.BuildTimeout = 20 * time.Millisecond })
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/graph", nil)
	var buildCtx context.Context
	start := time.Unix(1746442800, 0).UTC()
	s.serveGraph(c, start, start.Add(time.Hour), promql.Selector{},
		func(ctx context.Context, _ time.Duration, _ time.Time, _ promql.Selector) (*graph.Graph, error) {
			buildCtx = ctx
			return emptyGraph(), nil
		}, "kube-state-graph.project",
		func(g *graph.Graph) graph.View {
			<-buildCtx.Done() // the projection outlives the deadline
			return graph.Project(g, graph.Scope{})
		})
	assert.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.False(t, strings.Contains(rec.Body.String(), `"elements"`), "no graph body is written")
}

// Spec: "A request that completes every stage".
func TestServeGraph_ALiveRequestIsUnchanged(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	rec, projected := serveWith(t, s, t.Context(),
		func(context.Context, time.Duration, time.Time, promql.Selector) (*graph.Graph, error) {
			return emptyGraph(), nil
		})
	assert.True(t, projected)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"apiVersion":"v1","clusters":[],"elements":{"nodes":[],"edges":[]}}`, rec.Body.String())
}
