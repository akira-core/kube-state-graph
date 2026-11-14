package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// windowOf returns params with start/end spanning d, ending at a fixed instant
// that is NOT on the 30s alignment grid, so alignment runs.
func windowOf(params url.Values, d time.Duration) url.Values {
	end := int64(1746446417)
	return withParams(params, url.Values{
		"start": {strconv.FormatInt(end-int64(d/time.Second), 10)},
		"end":   {strconv.FormatInt(end, 10)},
	})
}

func getStatus(t *testing.T, srv *httptest.Server, path string, params url.Values) (int, errorBody) {
	t.Helper()
	resp, err := httpGet(t, srv.URL+path+"?"+params.Encode())
	require.NoError(t, err)
	defer resp.Body.Close()
	var body errorBody
	if resp.StatusCode != http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	}
	return resp.StatusCode, body
}

// Spec: "A window past the cap", "Every graph endpoint applies it" and "The cap
// reads the caller's window".
func TestMaxWindow_EveryGraphEndpoint(t *testing.T) {
	const capWindow = 24 * time.Hour
	for _, ep := range graphEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			// Over the cap: no upstream query at all (no expectation registered).
			s := newServerWithMocks(t, promqlmocks.NewMockQuerier(t), func(c *config.Config) { c.MaxWindow = capWindow })
			srv := httptest.NewServer(s.Handler())
			t.Cleanup(srv.Close)
			status, body := getStatus(t, srv, ep.path, windowOf(ep.params, capWindow+time.Second))
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Equal(t, "invalid_range", body.Error.Reason)
			assert.Equal(t, "window (end - start) exceeds the maximum of 24h0m0s", body.Error.Message)

			// Equal to the cap: accepted.
			s = newServerWithMocks(t, newMockQuerier(t, nil), func(c *config.Config) { c.MaxWindow = capWindow })
			srv2 := httptest.NewServer(s.Handler())
			t.Cleanup(srv2.Close)
			status, _ = getStatus(t, srv2, ep.path, windowOf(ep.params, capWindow))
			assert.Equal(t, http.StatusOK, status, "a window equal to the cap is accepted")
		})
	}
}

// Spec: "Other parameter errors win".
func TestMaxWindow_ParseErrorsWin(t *testing.T) {
	s := newServerWithMocks(t, promqlmocks.NewMockQuerier(t), func(c *config.Config) { c.MaxWindow = time.Hour })
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	noRoot := url.Values{"az": {"zone-a"}, "env": {"prod"}}
	status, body := getStatus(t, srv, "/v1/storage-graph", windowOf(noRoot, 48*time.Hour))
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "missing_root", body.Error.Reason)
}

// Spec: "No window cap or future-time guard" — the default applies no cap.
func TestMaxWindow_OffByDefault(t *testing.T) {
	require.Zero(t, config.Defaults().MaxWindow)
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	status, _ := getStatus(t, srv, "/v1/graph", windowOf(url.Values{}, 400*24*time.Hour))
	assert.Equal(t, http.StatusOK, status)
}
