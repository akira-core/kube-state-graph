package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/marz32one/kube-state-graph/internal/config"
)

// Credentials for the auth-enabled VictoriaMetrics container. Test-only values.
const (
	vmAuthUser = "ksg-it"
	vmAuthPass = "s3cret-vm-pass"
)

// UpstreamAuthSuite runs against a VictoriaMetrics container started with
// -httpAuth.username / -httpAuth.password, proving the API server's
// KSG_PROM_USERNAME / KSG_PROM_PASSWORD plumbing end-to-end — and that the
// container actually enforces auth (the unauthenticated path must fail, so
// the credentialed pass is not vacuous).
type UpstreamAuthSuite struct {
	VMSuite
}

func TestUpstreamAuthSuite(t *testing.T) {
	suite.Run(t, new(UpstreamAuthSuite))
}

// SetupSuite enables container-side basic auth BEFORE the container starts,
// then seeds a minimal topology fixture (ingestion itself authenticates via
// the suite helpers).
func (s *UpstreamAuthSuite) SetupSuite() {
	s.HTTPAuthUsername = vmAuthUser
	s.HTTPAuthPassword = vmAuthPass
	s.VMSuite.SetupSuite()

	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(`# HELP kube_pod_info dummy
kube_pod_info{cluster="cluster-auth",namespace="shop",pod="checkout",uid="auth-1",node="worker-0"} 1 ` + strconv.FormatInt(t1, 10) + `
kube_node_info{cluster="cluster-auth",node="worker-0"} 1 ` + strconv.FormatInt(t1, 10) + `
`)
	s.Require().True(s.WaitForSeries(`kube_pod_info{cluster="cluster-auth"}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_pod_info")
}

// Credentialed build succeeds end-to-end against the auth-enabled upstream.
func (s *UpstreamAuthSuite) TestCredentialedBuildSucceeds() {
	srv := s.StartAPIServer(func(cfg *config.Config) {
		cfg.PromUsername = vmAuthUser
		cfg.PromPassword = vmAuthPass
	})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var body map[string]any
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	elements, _ := body["elements"].(map[string]any)
	nodes, _ := elements["nodes"].([]any)
	s.NotEmpty(nodes, "expected nodes from the auth-enabled upstream")

	var foundPod bool
	for _, raw := range nodes {
		n, _ := raw.(map[string]any)
		data, _ := n["data"].(map[string]any)
		if data["id"] == "cluster-auth/auth-1" {
			foundPod = true
		}
	}
	s.True(foundPod, "expected pod node cluster-auth/auth-1 in the graph")
}

// Without credentials the same container rejects the build (upstream 401 →
// the upstream-failure error mapping), proving auth is actually enforced.
// The response must never leak the container's credentials.
func (s *UpstreamAuthSuite) TestUnauthenticatedBuildFails() {
	srv := s.StartAPIServer(nil) // no PromUsername/PromPassword
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusBadGateway, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)

	var body map[string]any
	s.Require().NoError(json.Unmarshal(raw, &body))
	errField, _ := body["error"].(map[string]any)
	s.Equal("upstream", errField["reason"], "expected the upstream-failure error mapping")

	s.NotContains(string(raw), vmAuthUser, "response must not leak the upstream username")
	s.NotContains(string(raw), vmAuthPass, "response must not leak the upstream password")
}
