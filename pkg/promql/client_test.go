package promql

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRoundTripper captures the request it receives without opening a socket,
// per the test-stack boundary rule (unit tests must not contact upstream).
type fakeRoundTripper struct {
	got *http.Request
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	f.got = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

// roundTrip drives rt with a GET to rawURL and returns the request the fake
// inner transport received.
func roundTrip(t *testing.T, rt http.RoundTripper, inner *fakeRoundTripper, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NotNil(t, inner.got, "inner transport not invoked")

	// RoundTrippers must not mutate the caller's request (D-A4).
	assert.Empty(t, req.Header.Get("Authorization"), "original request mutated")
	return inner.got
}

func TestNewTransport_WithBasicAuth_SetsHeader(t *testing.T) {
	inner := &fakeRoundTripper{}
	rt := newTransport(clientOptions{username: "ksg", password: "s3cret"}, "upstream.invalid", inner)

	got := roundTrip(t, rt, inner, "http://upstream.invalid/api/v1/query")
	user, pass, ok := got.BasicAuth()
	require.True(t, ok, "Authorization: Basic header missing on forwarded request")
	assert.Equal(t, "ksg", user)
	assert.Equal(t, "s3cret", pass)
}

func TestNewTransport_WithoutBasicAuth_NoAuthHeader(t *testing.T) {
	// The real chain New assembles, minus only the concrete base transport: no
	// WithBasicAuth options → no Authorization header reaches the wire.
	inner := &fakeRoundTripper{}
	rt := newTransport(clientOptions{}, "upstream.invalid", inner)

	got := roundTrip(t, rt, inner, "http://upstream.invalid/api/v1/query")
	assert.Empty(t, got.Header.Get("Authorization"))
}

func TestNewTransport_BasicAuth_NotSentCrossHost(t *testing.T) {
	// A redirect hop to a foreign host re-enters the transport below net/http's
	// sensitive-header stripping; the transport itself must withhold the
	// credentials for any host other than the configured upstream.
	inner := &fakeRoundTripper{}
	rt := newTransport(clientOptions{username: "ksg", password: "s3cret"}, "upstream.invalid", inner)

	got := roundTrip(t, rt, inner, "http://evil.invalid/api/v1/query")
	assert.Empty(t, got.Header.Get("Authorization"), "credentials leaked to a foreign host")
}

func TestNew_OptionVariants(t *testing.T) {
	// Both call shapes must construct successfully: legacy two-arg and with
	// WithBasicAuth. Construction-only — no requests are issued.
	_, err := New("http://upstream.invalid", nil)
	require.NoError(t, err)

	_, err = New("http://upstream.invalid", nil, WithBasicAuth("ksg", "s3cret"))
	require.NoError(t, err)
}
