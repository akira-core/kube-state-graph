package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/marz32one/kube-state-graph/internal/api"
	"github.com/marz32one/kube-state-graph/internal/auth"
	"github.com/marz32one/kube-state-graph/internal/config"
	"github.com/marz32one/kube-state-graph/internal/observability"
	"github.com/marz32one/kube-state-graph/pkg/build"
	"github.com/marz32one/kube-state-graph/pkg/clock"
	"github.com/marz32one/kube-state-graph/pkg/promql"
)

// VMImage is the pinned VictoriaMetrics container image used across the
// integration suite. Pinned by tag — never `:latest` — per D20.
const VMImage = "victoriametrics/victoria-metrics:v1.107.0"

// VMSuite is the base suite type embedded by every integration suite that
// needs a real VictoriaMetrics backend. It starts one container per suite,
// exposes helpers for series ingestion + readiness, and tears the container
// down at the end.
type VMSuite struct {
	suite.Suite

	// HTTPAuthUsername / HTTPAuthPassword, when both non-empty, start the
	// container with `-httpAuth.username` / `-httpAuth.password` so every VM
	// endpoint except the exempt `/health` requires basic auth. Embedding
	// suites set them BEFORE calling VMSuite.SetupSuite. The suite's own
	// helpers (readiness, ingest, series polling) authenticate automatically.
	HTTPAuthUsername string
	HTTPAuthPassword string

	ctx       context.Context
	cancel    context.CancelFunc
	container testcontainers.Container
	vmURL     string
}

// SkipIfDockerUnavailable short-circuits the suite when Docker isn't usable.
// Used by `go test ./...` runs on developer machines without Docker so the
// rest of the test tree still runs.
func SkipIfDockerUnavailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not in PATH; skipping integration suite")
	}
	cmd := exec.Command("docker", "info")
	if err := cmd.Run(); err != nil {
		t.Skip("docker daemon unreachable; skipping integration suite")
	}
}

// SetupSuite starts the VictoriaMetrics container and waits for readiness.
func (s *VMSuite) SetupSuite() {
	SkipIfDockerUnavailable(s.T())
	s.ctx, s.cancel = context.WithCancel(context.Background())

	req := testcontainers.ContainerRequest{
		Image:        VMImage,
		ExposedPorts: []string{"8428/tcp"},
		// `-search.latencyOffset=0` disables VM's default 30s ingestion-latency
		// rewind so queries at time=T can immediately see samples ingested at T.
		// Without this, fixtures pinned to fixedNow are invisible until 30s pass.
		//
		// `-retentionPeriod=100y` keeps the statically-dated fixtures (anchored
		// at fixedNow, a fixed absolute date) ingestable regardless of how far
		// the container's real wall-clock has advanced past that date. VM's
		// default retention is 1 month, so once real time passes fixedNow+1mo it
		// rejects the samples as "too small timestamp ... outside the retention"
		// and every query returns empty — a wall-clock time-bomb. 100y removes it.
		// `/health` is exempt from -httpAuth.* in VM's httpserver, so the
		// readiness wait below works for auth-enabled containers too.
		Cmd:        []string{"-search.latencyOffset=0s", "-retentionPeriod=100y"},
		WaitingFor: wait.ForHTTP("/health").WithPort("8428/tcp").WithStartupTimeout(60 * time.Second),
	}
	if s.HTTPAuthUsername != "" {
		req.Cmd = append(req.Cmd,
			"-httpAuth.username="+s.HTTPAuthUsername,
			"-httpAuth.password="+s.HTTPAuthPassword,
		)
	}
	c, err := testcontainers.GenericContainer(s.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	s.Require().NoError(err, "start VictoriaMetrics container")
	s.container = c

	host, err := c.Host(s.ctx)
	s.Require().NoError(err)
	port, err := c.MappedPort(s.ctx, "8428/tcp")
	s.Require().NoError(err)
	s.vmURL = fmt.Sprintf("http://%s:%s", host, port.Port())

	s.WaitForReady(10 * time.Second)
}

// TearDownSuite stops and removes the container.
func (s *VMSuite) TearDownSuite() {
	if s.container != nil {
		_ = s.container.Terminate(s.ctx)
	}
	if s.cancel != nil {
		s.cancel()
	}
}

// VMURL returns the base URL of the running VictoriaMetrics instance.
func (s *VMSuite) VMURL() string { return s.vmURL }

// vmGet issues a GET against the VM container, attaching the suite's basic
// auth credentials when the container was started with -httpAuth.*.
func (s *VMSuite) vmGet(rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if s.HTTPAuthUsername != "" {
		req.SetBasicAuth(s.HTTPAuthUsername, s.HTTPAuthPassword)
	}
	return http.DefaultClient.Do(req)
}

// WaitForReady polls VM's `up{}` (effectively, /-/ready) until it answers or
// the budget is exhausted.
func (s *VMSuite) WaitForReady(budget time.Duration) {
	s.T().Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		resp, err := s.vmGet(s.vmURL + "/-/ready")
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.Require().FailNowf("vm_not_ready", "VictoriaMetrics did not become ready within %s", budget)
}

// IngestExpFmt POSTs Prometheus exposition-format text to VM's
// /api/v1/import/prometheus endpoint. Each line is one sample.
func (s *VMSuite) IngestExpFmt(exposition string) {
	s.T().Helper()
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost,
		s.vmURL+"/api/v1/import/prometheus", strings.NewReader(exposition))
	s.Require().NoError(err)
	if s.HTTPAuthUsername != "" {
		req.SetBasicAuth(s.HTTPAuthUsername, s.HTTPAuthPassword)
	}
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	s.Require().Truef(resp.StatusCode >= 200 && resp.StatusCode < 300,
		"VM ingest returned %d: %s", resp.StatusCode, body)
}

// WaitForSeries polls VM until the supplied PromQL returns a non-empty
// vector at the given evaluation time or the budget is exhausted. evalTime
// is forwarded as the `time=` parameter; pass time.Time{} to evaluate at
// the server's current time. On budget exhaustion it logs the final probe
// URL and response so failures are debuggable from the test log.
func (s *VMSuite) WaitForSeries(query string, evalTime time.Time, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	var lastURL string
	var lastStatus int
	var lastBody []byte
	var lastErr error
	for time.Now().Before(deadline) {
		v := url.Values{"query": []string{query}}
		if !evalTime.IsZero() {
			v.Set("time", strconv.FormatInt(evalTime.Unix(), 10))
		}
		// `nocache=1` bypasses VM's response cache. Without this, the first
		// poll (run before the VM ingest pipeline has flushed) caches an
		// empty result for the historical time bucket and every subsequent
		// poll within the budget receives that cached empty.
		v.Set("nocache", "1")
		probeURL := s.vmURL + "/api/v1/query?" + v.Encode()
		lastURL = probeURL
		resp, err := s.vmGet(probeURL)
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		lastStatus = resp.StatusCode
		lastBody = body
		if resp.StatusCode == http.StatusOK && !bytes.Contains(body, []byte(`"result":[]`)) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.T().Logf("WaitForSeries timeout: url=%s status=%d err=%v body=%s",
		lastURL, lastStatus, lastErr, lastBody)
	return false
}

// APIOption tweaks the in-process API server constructed by StartAPIServer.
// Functional options keep production New() signatures stable while letting
// tests inject deterministic substitutes (e.g. a fixed clock).
type APIOption func(*apiOptions)

type apiOptions struct {
	clk clock.Clock
}

// WithClock pins the server's Clock dependency. nil falls back to clock.System.
func WithClock(clk clock.Clock) APIOption { return func(o *apiOptions) { o.clk = clk } }

// StartAPIServer constructs an in-process API server pointed at the running
// VictoriaMetrics container, wraps it in httptest.NewServer, and returns the
// server's base URL. Caller-supplied configure func may tweak the Config;
// optional APIOptions tweak Server-level dependencies.
func (s *VMSuite) StartAPIServer(configure func(*config.Config), opts ...APIOption) *httptest.Server {
	s.T().Helper()
	cfg := config.Defaults()
	cfg.PromURL = s.vmURL
	cfg.LogLevel = "error"
	if configure != nil {
		configure(&cfg)
	}
	s.Require().NoError(cfg.Validate())

	o := apiOptions{}
	for _, fn := range opts {
		fn(&o)
	}

	logger := observability.NewLogger(cfg.LogLevel)
	metrics := observability.NewMetrics()
	var promOpts []promql.Option
	if cfg.PromUsername != "" {
		promOpts = append(promOpts, promql.WithBasicAuth(cfg.PromUsername, cfg.PromPassword))
	}
	prom, err := promql.New(cfg.PromURL, metrics, promOpts...)
	s.Require().NoError(err)

	ks := auth.NewKeySet()
	if cfg.APIKeys != "" {
		ks.LoadCSV(cfg.APIKeys)
	}
	if cfg.APIKeysFile != "" {
		s.Require().NoError(ks.LoadFile(cfg.APIKeysFile))
	}
	builder := build.New(prom, build.Options{MetricPrefix: cfg.MetricPrefix, APITimeout: cfg.APITimeout}, metrics, o.clk)
	srv := api.New(cfg, builder, prom, metrics, logger, ks, o.clk)

	httpSrv := httptest.NewServer(srv.Handler())
	s.T().Cleanup(httpSrv.Close)
	return httpSrv
}
