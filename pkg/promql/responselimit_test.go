package promql

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bodyRoundTripper answers every request with body, declaring its length or
// not (chunked) as asked.
type bodyRoundTripper struct {
	body     string
	declared bool
	closed   bool
}

func (b *bodyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cl := int64(-1)
	if b.declared {
		cl = int64(len(b.body))
	}
	return &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: cl,
		Body:          &closeRecorder{Reader: strings.NewReader(b.body), closed: &b.closed},
		Request:       req,
	}, nil
}

type closeRecorder struct {
	io.Reader
	closed *bool
}

func (c *closeRecorder) Close() error { *c.closed = true; return nil }

func readThrough(t *testing.T, rt http.RoundTripper) (string, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://upstream.invalid/api/v1/query", nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func TestResponseLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		body     string
		declared bool
		max      int64
		wantErr  bool
	}{
		"declared over the bound refused":  {strings.Repeat("x", 11), true, 10, true},
		"streamed over the bound fails":    {strings.Repeat("x", 11), false, 10, true},
		"declared exactly at the bound":    {strings.Repeat("x", 10), true, 10, false},
		"streamed exactly at the bound":    {strings.Repeat("x", 10), false, 10, false},
		"under the bound":                  {"ok", false, 10, false},
		"empty body":                       {"", false, 10, false},
		"far over, streamed, small reads":  {strings.Repeat("y", 4096), false, 100, true},
		"far over, declared, never opened": {strings.Repeat("y", 4096), true, 100, true},
	} {
		t.Run(name, func(t *testing.T) {
			inner := &bodyRoundTripper{body: tc.body, declared: tc.declared}
			got, err := readThrough(t, &responseLimitTransport{inner: inner, max: tc.max})
			if tc.wantErr {
				require.ErrorIs(t, err, ErrResponseTooLarge)
				assert.True(t, inner.closed, "the refused body is closed")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.body, got)
		})
	}
}

func TestResponseBound_DefaultAndOverride(t *testing.T) {
	assert.Equal(t, DefaultMaxResponseBytes, clientOptions{}.responseBound(), "on by default")
	var o clientOptions
	WithMaxResponseBytes(1024)(&o)
	assert.EqualValues(t, 1024, o.responseBound())
	WithMaxResponseBytes(0)(&o)
	assert.Zero(t, o.responseBound(), "0 disables")
	WithMaxResponseBytes(-5)(&o)
	assert.Zero(t, o.responseBound(), "negative disables")
}

// End to end through the real client: an oversized answer is an ordinary
// upstream query error, distinguishable by ErrResponseTooLarge.
func TestClient_OversizedResponseFailsTheQuery(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"vector","result":[` +
		strings.Repeat(`{"metric":{"pod":"p"},"value":[1,"1"]},`, 100) +
		`{"metric":{"pod":"q"},"value":[1,"1"]}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	small, err := New(srv.URL, nil, WithMaxResponseBytes(256))
	require.NoError(t, err)
	_, err = small.Instant(t.Context(), string(QPodInfo), "kube_pod_info", ts0)
	require.ErrorIs(t, err, ErrResponseTooLarge)

	unbounded, err := New(srv.URL, nil, WithMaxResponseBytes(0))
	require.NoError(t, err)
	v, err := unbounded.Instant(t.Context(), string(QPodInfo), "kube_pod_info", ts0)
	require.NoError(t, err)
	assert.Len(t, v, 101)

	def, err := New(srv.URL, nil)
	require.NoError(t, err)
	_, err = def.Instant(t.Context(), string(QPodInfo), "kube_pod_info", ts0)
	require.NoError(t, err, "the default bound is far above an ordinary answer")
}

// The factory's process-wide options reach every backend's client.
func TestDefaultClientFactory_AppliesSharedOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(srv.Close)

	q, err := DefaultClientFactory(nil, WithMaxResponseBytes(8))(
		NewBackend("zone-a", srv.URL, []Family{FamilyKSM}, nil, "", ""))
	require.NoError(t, err)
	_, err = q.Instant(t.Context(), string(QPodInfo), "kube_pod_info", ts0)
	require.ErrorIs(t, err, ErrResponseTooLarge)
}
