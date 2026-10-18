package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/akira-core/kube-state-graph/internal/api"
	"github.com/akira-core/kube-state-graph/internal/auth"
	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/internal/observability"
	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/clock"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	"github.com/akira-core/kube-state-graph/pkg/promql/backendsfile"
)

// AppMetadataSuite proves the application-metadata metric against two real
// VictoriaMetrics installations. Container A (the embedded VMSuite) holds the
// kube-state-metrics estate, including the controller-annotation chain.
// Container B holds only argocd_app_pod_info and is the sole URL of a backend
// whose only family is appmeta. A query sent to A cannot invent the metric's
// Application, namespace owner, or deployer.
type AppMetadataSuite struct {
	VMSuite

	metaCtx    context.Context
	metaCancel context.CancelFunc
	metaC      testcontainers.Container
	metaURL    string
}

func TestAppMetadataSuite(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(AppMetadataSuite))
}

func (s *AppMetadataSuite) SetupSuite() {
	s.VMSuite.SetupSuite()
	s.metaCtx, s.metaCancel = context.WithCancel(context.Background())

	c, err := testcontainers.GenericContainer(s.metaCtx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        VMImage,
			ExposedPorts: []string{"8428/tcp"},
			Cmd:          []string{"-search.latencyOffset=0s", "-retentionPeriod=100y"},
			WaitingFor:   wait.ForHTTP("/health").WithPort("8428/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	s.Require().NoError(err, "start the application-metadata VictoriaMetrics container")
	s.metaC = c

	host, err := c.Host(s.metaCtx)
	s.Require().NoError(err)
	port, err := c.MappedPort(s.metaCtx, "8428/tcp")
	s.Require().NoError(err)
	s.metaURL = fmt.Sprintf("http://%s:%s", host, port.Port())

	s.seedFixtures()
}

func (s *AppMetadataSuite) TearDownSuite() {
	if s.metaC != nil {
		_ = s.metaC.Terminate(s.metaCtx)
	}
	if s.metaCancel != nil {
		s.metaCancel()
	}
	s.VMSuite.TearDownSuite()
}

func (s *AppMetadataSuite) seedFixtures() {
	s.T().Helper()
	ts := fixedNow.Unix() * 1000

	// Container A — topology and the controller chain. chain-app is what the
	// body shows when the metric is unset or appmeta is unserved.
	s.IngestExpFmt(fmt.Sprintf(`
kube_pod_info{cluster="c1",namespace="shop",pod="web-0",uid="uid-web",node="worker-0",pod_ip="10.1.0.8",az="zone-a",env="prod"} 1 %[1]d
kube_node_info{cluster="c1",node="worker-0",az="zone-a",env="prod"} 1 %[1]d
kube_pod_owner{cluster="c1",namespace="shop",pod="web-0",owner_kind="ReplicaSet",owner_name="web-7d9",owner_is_controller="true",az="zone-a",env="prod"} 1 %[1]d
kube_replicaset_owner{cluster="c1",namespace="shop",replicaset="web-7d9",owner_kind="Deployment",owner_name="web",owner_is_controller="true",az="zone-a",env="prod"} 1 %[1]d
kube_deployment_annotations{cluster="c1",namespace="shop",deployment="web",annotation_argocd_argoproj_io_tracking_id="chain-app:apps/Deployment:shop/web",az="zone-a",env="prod"} 1 %[1]d
`, ts))

	// Container B — the metric, and nothing else. az and env are present so
	// the row's cluster identity composes to the same zone-a-prod-c1 the pod
	// uses; the namespace owner and deployer are keyed on that identity.
	s.ingestMeta(fmt.Sprintf(`
argocd_app_pod_info{cluster="c1",namespace="shop",pod="web-0",argocd_app="metric-app",owner="team-a",user="alice",az="zone-a",env="prod"} 1 %d
`, ts))

	s.Require().True(s.WaitForSeries(`kube_pod_info{pod="web-0",uid="uid-web"}`, fixedNow, 30*time.Second),
		"container A did not observe the pod fixture")
	s.Require().True(s.WaitForSeries(`kube_deployment_annotations{deployment="web"}`, fixedNow, 30*time.Second),
		"container A did not observe the deployment annotation")
	s.Require().True(s.waitMeta(`argocd_app_pod_info{pod="web-0",argocd_app="metric-app"}`, fixedNow, 30*time.Second),
		"container B did not observe the application-metadata fixture")
}

func (s *AppMetadataSuite) ingestMeta(exposition string) {
	s.T().Helper()
	req, err := http.NewRequestWithContext(s.metaCtx, http.MethodPost,
		s.metaURL+"/api/v1/import/prometheus", strings.NewReader(exposition))
	s.Require().NoError(err)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	s.Require().Truef(resp.StatusCode >= 200 && resp.StatusCode < 300,
		"VM ingest returned %d: %s", resp.StatusCode, body)

	flush, err := http.NewRequestWithContext(s.metaCtx, http.MethodGet, s.metaURL+"/internal/force_flush", nil)
	s.Require().NoError(err)
	if fresp, ferr := http.DefaultClient.Do(flush); ferr == nil {
		_ = fresp.Body.Close()
	}
}

func (s *AppMetadataSuite) waitMeta(query string, at time.Time, budget time.Duration) bool {
	s.T().Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		q := url.Values{}
		q.Set("query", query)
		q.Set("time", strconv.FormatInt(at.Unix(), 10))
		req, err := http.NewRequestWithContext(s.metaCtx, http.MethodGet,
			s.metaURL+"/api/v1/query?"+q.Encode(), nil)
		s.Require().NoError(err)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			var parsed struct {
				Data struct {
					Result []json.RawMessage `json:"result"`
				} `json:"data"`
			}
			decErr := json.NewDecoder(resp.Body).Decode(&parsed)
			_ = resp.Body.Close()
			if decErr == nil && len(parsed.Data.Result) > 0 {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// backendsFile writes a routing table. withAppMeta adds the dedicated appmeta
// backend; without it the table covers only the five required families.
func (s *AppMetadataSuite) backendsFile(withAppMeta bool) string {
	s.T().Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "backends:\n  - name: k8s\n    url: %q\n    families: [ksm, kubelet, harvest, servicegraph, probe]\n", s.VMURL())
	if withAppMeta {
		fmt.Fprintf(&b, "  - name: appmeta\n    url: %q\n    families: [appmeta]\n    zones: [zone-a]\n", s.metaURL)
	}
	path := filepath.Join(s.T().TempDir(), "backends.yaml")
	s.Require().NoError(os.WriteFile(path, []byte(b.String()), 0o600))
	return path
}

// startAPI serves the two containers through a backends file. metric is the
// configured series name; empty leaves the feature unset. withAppMeta selects
// the file that declares the appmeta backend.
func (s *AppMetadataSuite) startAPI(metric string, withAppMeta bool) *httptest.Server {
	s.T().Helper()
	cfg := config.Defaults()
	cfg.PromURL = s.VMURL()
	cfg.LogLevel = "error"
	cfg.AppMetadataMetric = metric
	s.Require().NoError(cfg.Validate())
	src, err := cfg.AppMetadata()
	s.Require().NoError(err)

	// No process environment: the file names no credential variables, and a
	// developer shell must not leak a global pair into the table.
	table, err := backendsfile.Read(s.backendsFile(withAppMeta), func(string) (string, bool) { return "", false })
	s.Require().NoError(err)

	logger := observability.NewLogger(cfg.LogLevel)
	metrics := observability.NewMetrics()
	router, err := promql.NewRouter(table, metrics, promql.DefaultClientFactory(metrics))
	s.Require().NoError(err)
	builder := build.New(router, build.Options{
		APITimeout:  cfg.APITimeout,
		LabelKeys:   promql.LabelKeys{AZ: cfg.AZLabel, Env: cfg.EnvLabel},
		AppMetadata: src,
	}, metrics, clock.System{})
	srv := api.New(cfg, builder, router, metrics, logger, auth.NewKeySet(), clock.System{})

	httpSrv := httptest.NewServer(srv.Handler())
	s.T().Cleanup(httpSrv.Close)
	return httpSrv
}

func (s *AppMetadataSuite) fetchBody(rawURL string) cytoscape.Body {
	s.T().Helper()
	resp := s.httpGet(rawURL)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equalf(http.StatusOK, resp.StatusCode, "body: %s", raw)
	var body cytoscape.Body
	s.Require().NoError(json.Unmarshal(raw, &body))
	return body
}

func (s *AppMetadataSuite) graphBody(srv *httptest.Server) cytoscape.Body {
	s.T().Helper()
	return s.fetchBody(s.graphURL(srv.URL, func(q url.Values) {
		q.Set("prune", "false")
		q.Set("az", "zone-a")
		q.Set("env", "prod")
	}))
}

func (s *AppMetadataSuite) storageBody(srv *httptest.Server) cytoscape.Body {
	s.T().Helper()
	return s.fetchBody(s.storageGraphURL(srv.URL, func(q url.Values) {
		q.Set("pod", "shop/web-0")
	}))
}

const (
	amCluster = "zone-a-prod-c1"
	amPodID   = amCluster + "/uid-web"
	amNSID    = amCluster + "/namespace/shop"
)

func (s *AppMetadataSuite) assertMetric(body cytoscape.Body) {
	s.T().Helper()
	byID := nodesByID(body)
	s.Require().Contains(byID, amPodID)
	pod := byID[amPodID]
	s.Equal("metric-app", pod.Application)
	s.Empty(pod.NamespaceOwner, "namespace_owner belongs on the namespace group")
	s.Empty(pod.DeployedBy, "deployed_by belongs on the application group")

	s.Require().Contains(byID, amNSID)
	ns := byID[amNSID]
	s.Equal("namespace", ns.Type)
	s.Equal("team-a", ns.NamespaceOwner)
	s.Empty(ns.Labels)

	appID := amNSID + "/application/metric-app"
	s.Require().Contains(byID, appID)
	app := byID[appID]
	s.Equal("application", app.Type)
	s.Equal("alice", app.DeployedBy)
}

func (s *AppMetadataSuite) assertChain(body cytoscape.Body) {
	s.T().Helper()
	byID := nodesByID(body)
	s.Require().Contains(byID, amPodID)
	pod := byID[amPodID]
	s.Equal("chain-app", pod.Application)
	s.Empty(pod.NamespaceOwner)
	s.Empty(pod.DeployedBy)

	s.Require().Contains(byID, amNSID)
	s.Empty(byID[amNSID].NamespaceOwner)
	s.Require().Contains(byID, amNSID+"/application/chain-app")
	s.Empty(byID[amNSID+"/application/chain-app"].DeployedBy)
	s.NotContains(byID, amNSID+"/application/metric-app")
}

// A configured metric on the dedicated appmeta backend wins over the
// controller chain on both endpoints, for the same window.
func (s *AppMetadataSuite) TestDedicatedBackendDrivesGraphAndStorage() {
	srv := s.startAPI("argocd_app_pod_info", true)
	s.assertMetric(s.graphBody(srv))
	s.assertMetric(s.storageBody(srv))
}

// A backends file that leaves appmeta unserved, with the metric still
// configured, returns the same body as a server whose metric is unset: the
// controller chain, and neither group field.
func (s *AppMetadataSuite) TestUnservedBackendMatchesUnset() {
	unserved := s.startAPI("argocd_app_pod_info", false)
	unset := s.startAPI("", false)

	unservedGraph := s.graphBody(unserved)
	unsetGraph := s.graphBody(unset)
	s.Equal(unsetGraph, unservedGraph)
	s.assertChain(unsetGraph)

	unservedStorage := s.storageBody(unserved)
	unsetStorage := s.storageBody(unset)
	s.Equal(unsetStorage, unservedStorage)
	s.assertChain(unsetStorage)
}
