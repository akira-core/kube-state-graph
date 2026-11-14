package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/auth"
	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// gatedQuerier holds every graph query until release is closed, answers the
// readiness probe at once, and records every query it saw.
type gatedQuerier struct {
	release chan struct{}
	mu      sync.Mutex
	seen    []string
}

func newGatedQuerier(t *testing.T) (*promqlmocks.MockQuerier, *gatedQuerier) {
	t.Helper()
	g := &gatedQuerier{release: make(chan struct{})}
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(ctx context.Context, name, query string, _ time.Time) (model.Vector, error) {
			g.mu.Lock()
			g.seen = append(g.seen, query)
			g.mu.Unlock()
			if name == string(promql.QUpProbe) {
				return model.Vector{&model.Sample{Metric: model.Metric{}, Value: 1}}, nil
			}
			select {
			case <-g.release:
				return model.Vector{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}).
		Maybe()
	return q, g
}

func (g *gatedQuerier) sawSubstring(s string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, q := range g.seen {
		if strings.Contains(q, s) {
			return true
		}
	}
	return false
}

// metricValue scrapes /metrics and returns the sample line of an unlabelled
// metric, e.g. "kube_state_graph_inflight_builds 2".
func metricValue(t *testing.T, base, name string) string {
	t.Helper()
	line, err := scrapeLine(base, name)
	require.NoError(t, err)
	return line
}

// metricIs is metricValue as a require.Eventually condition: it never calls
// into the testing.T, because the condition runs on testify's own goroutine.
func metricIs(base, name, want string) func() bool {
	return func() bool {
		line, err := scrapeLine(base, name)
		return err == nil && line == want
	}
}

func scrapeLine(base, name string) (string, error) {
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.HasPrefix(line, name+" ") {
			return line, nil
		}
	}
	return "", fmt.Errorf("metric %s not exposed", name)
}

// getGraphNS issues /v1/graph narrowed to namespace ns, so its queries are
// recognisable among those of concurrent requests.
func getGraphNS(t *testing.T, base, ns string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/v1/graph?start=1746442800&end=1746446400&namespace="+ns, nil)
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// Spec: "A request past the bound is shed", "Operational routes are never
// limited" and "Admission self-metrics".
func TestAdmission_ShedsPastTheBoundAndNeverLimitsOperationalRoutes(t *testing.T) {
	q, gate := newGatedQuerier(t)
	s := newServerWithMocks(t, q, func(c *config.Config) { c.MaxInflightBuilds = 2 })
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	statuses := make(chan int, 2)
	for _, ns := range []string{"held-a", "held-b"} {
		go func() {
			resp := getGraphNS(t, srv.URL, ns, nil)
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	require.Eventually(t, metricIs(srv.URL, "kube_state_graph_inflight_builds", "kube_state_graph_inflight_builds 2"),
		5*time.Second, 5*time.Millisecond, "both requests are admitted and held")

	resp := getGraphNS(t, srv.URL, "third", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "1", resp.Header.Get("Retry-After"))
	var body errorBody
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "overloaded", body.Error.Reason)
	assert.Equal(t, "too many graph requests in flight; retry later", body.Error.Message)
	assert.False(t, gate.sawSubstring(`namespace="third"`), "a shed request issues no upstream query")
	assert.Equal(t, "kube_state_graph_admission_rejected_total 1",
		metricValue(t, srv.URL, "kube_state_graph_admission_rejected_total"))

	for _, path := range []string{"/livez", "/readyz", "/metrics", "/docs", "/openapi.json", "/openapi.yaml"} {
		r, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		r.Body.Close()
		assert.Equal(t, http.StatusOK, r.StatusCode, "%s is never limited", path)
	}

	close(gate.release)
	for range 2 {
		assert.Equal(t, http.StatusOK, <-statuses)
	}
	assert.Equal(t, "kube_state_graph_inflight_builds 0", metricValue(t, srv.URL, "kube_state_graph_inflight_builds"))

	resp2 := getGraphNS(t, srv.URL, "after", nil)
	resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode, "a freed slot admits the next request")
}

// Spec: "Authentication runs first".
func TestAdmission_AuthenticationRunsFirst(t *testing.T) {
	q, gate := newGatedQuerier(t)
	ks := auth.NewKeySet()
	ks.LoadCSV("k1")
	s := newServerWithMocksAndKeys(t, q, ks, func(c *config.Config) { c.MaxInflightBuilds = 1 })
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp := getGraphNS(t, srv.URL, "held", http.Header{"X-Api-Key": {"k1"}})
		resp.Body.Close()
	}()
	require.Eventually(t, metricIs(srv.URL, "kube_state_graph_inflight_builds", "kube_state_graph_inflight_builds 1"),
		5*time.Second, 5*time.Millisecond)

	resp := getGraphNS(t, srv.URL, "anon", nil)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a 401 stays a 401 at the bound")

	resp = getGraphNS(t, srv.URL, "keyed", http.Header{"X-Api-Key": {"k1"}})
	resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	close(gate.release)
	<-done
}

// Spec: "Zero disables the bound".
func TestAdmission_ZeroDisablesTheBound(t *testing.T) {
	q, gate := newGatedQuerier(t)
	s := newServerWithMocks(t, q, func(c *config.Config) { c.MaxInflightBuilds = 0 })
	require.Nil(t, s.slots)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	const n = 5
	statuses := make(chan int, n)
	for i := range n {
		go func() {
			resp := getGraphNS(t, srv.URL, "ns-"+string(rune('a'+i)), nil)
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	require.Eventually(t, func() bool {
		for i := range n {
			if !gate.sawSubstring(`namespace="ns-` + string(rune('a'+i)) + `"`) {
				return false
			}
		}
		return true
	}, 5*time.Second, 5*time.Millisecond, "every request reached the upstream concurrently")
	close(gate.release)
	for range n {
		assert.Equal(t, http.StatusOK, <-statuses)
	}
	assert.Equal(t, "kube_state_graph_admission_rejected_total 0",
		metricValue(t, srv.URL, "kube_state_graph_admission_rejected_total"))
}

// Spec: "the gauge returns to its previous value when an admitted request
// finishes, also when its handler panics".
func TestAdmission_APanickingHandlerReleasesItsSlot(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), func(c *config.Config) { c.MaxInflightBuilds = 1 })
	r := gin.New()
	r.Use(s.recoveryMiddleware())
	r.GET("/boom", s.admissionMiddleware(), func(*gin.Context) { panic("boom") })
	r.GET("/ok", s.admissionMiddleware(), func(c *gin.Context) { c.Status(http.StatusOK) })

	for range 3 {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
	}
	assert.Empty(t, s.slots, "every slot was released")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "the bound of 1 still admits after three panics")
}
