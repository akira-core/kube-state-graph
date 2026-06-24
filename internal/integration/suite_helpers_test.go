package integration

import (
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Shared request helpers for every suite embedding VMSuite. They live in a
// _test.go file (not vmsuite.go) because graphURL anchors on fixedNow, which
// is test-only state.

// graphURL builds a /v1/graph URL for the standard fixedNow-anchored window;
// configureQuery (nillable) adds extra query parameters.
func (s *VMSuite) graphURL(srv string, configureQuery func(url.Values)) string {
	q := url.Values{}
	q.Set("start", strconv.FormatInt(fixedNow.Add(-5*time.Minute).Unix(), 10))
	q.Set("end", strconv.FormatInt(fixedNow.Unix(), 10))
	if configureQuery != nil {
		configureQuery(q)
	}
	return srv + "/v1/graph?" + q.Encode()
}

// httpGet issues a GET against the API server under test (NOT the VM
// container — VM traffic goes through vmGet, which authenticates).
func (s *VMSuite) httpGet(rawURL string) *http.Response {
	s.T().Helper()
	req, err := http.NewRequestWithContext(s.T().Context(), http.MethodGet, rawURL, nil)
	s.Require().NoError(err)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	return resp
}
