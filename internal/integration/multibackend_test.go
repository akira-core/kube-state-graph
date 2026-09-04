package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
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
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// MultiBackendSuite runs the API against TWO real VictoriaMetrics
// installations, which is the configuration the routing capability exists for:
// an estate whose series are split by metric family and by availability zone,
// served as one graph by one process.
//
// The embedded VMSuite owns container A; this suite starts container B itself.
type MultiBackendSuite struct {
	VMSuite

	secondCtx    context.Context
	secondCancel context.CancelFunc
	secondC      testcontainers.Container
	secondURL    string
}

func TestMultiBackendSuite(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(MultiBackendSuite))
}

func (s *MultiBackendSuite) SetupSuite() {
	s.VMSuite.SetupSuite()
	s.secondCtx, s.secondCancel = context.WithCancel(context.Background())

	c, err := testcontainers.GenericContainer(s.secondCtx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        VMImage,
			ExposedPorts: []string{"8428/tcp"},
			// Same flags as VMSuite's container, for the same reasons: no
			// latency rewind, and a retention long enough that the statically
			// dated fixtures stay ingestable.
			Cmd:        []string{"-search.latencyOffset=0s", "-retentionPeriod=100y"},
			WaitingFor: wait.ForHTTP("/health").WithPort("8428/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	s.Require().NoError(err, "start the second VictoriaMetrics container")
	s.secondC = c

	host, err := c.Host(s.secondCtx)
	s.Require().NoError(err)
	port, err := c.MappedPort(s.secondCtx, "8428/tcp")
	s.Require().NoError(err)
	s.secondURL = fmt.Sprintf("http://%s:%s", host, port.Port())

	s.seedFixtures()
}

func (s *MultiBackendSuite) TearDownSuite() {
	if s.secondC != nil {
		_ = s.secondC.Terminate(s.secondCtx)
	}
	if s.secondCancel != nil {
		s.secondCancel()
	}
	s.VMSuite.TearDownSuite()
}

// ingestInto POSTs exposition text to an arbitrary VM instance and flushes it,
// so the suite can seed the second container the same way VMSuite seeds the
// first.
func (s *MultiBackendSuite) ingestInto(baseURL, exposition string) {
	s.T().Helper()
	req, err := http.NewRequestWithContext(s.secondCtx, http.MethodPost,
		baseURL+"/api/v1/import/prometheus", strings.NewReader(exposition))
	s.Require().NoError(err)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	s.Require().Truef(resp.StatusCode >= 200 && resp.StatusCode < 300,
		"VM ingest returned %d: %s", resp.StatusCode, body)

	flush, err := http.NewRequestWithContext(s.secondCtx, http.MethodGet, baseURL+"/internal/force_flush", nil)
	s.Require().NoError(err)
	if fresp, ferr := http.DefaultClient.Do(flush); ferr == nil {
		_ = fresp.Body.Close()
	}
}

// waitForSeriesAt polls an arbitrary VM instance until query returns a
// non-empty vector, mirroring VMSuite.WaitForSeries for the second container.
func (s *MultiBackendSuite) waitForSeriesAt(baseURL, query string, at time.Time, budget time.Duration) bool {
	s.T().Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		q := url.Values{}
		q.Set("query", query)
		q.Set("time", strconv.FormatInt(at.Unix(), 10))
		req, err := http.NewRequestWithContext(s.secondCtx, http.MethodGet,
			baseURL+"/api/v1/query?"+q.Encode(), nil)
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

// Fixture constants. The service-graph counter is ingested as two monotonic
// samples 60s apart so rate() over the test window recovers a non-zero value.
const (
	mbCounterStep = 60.0
	mbRate        = 5.0
)

// seedFixtures splits one estate across the two containers:
//
//   - container A holds the kube-state-metrics side for BOTH zones, plus the
//     service-graph counter, plus a SHADOW volume_labels series for the same PV
//     name bound to a different aggregate;
//   - container B holds the NetApp Harvest volume labels, and a byte-identical
//     copy of the same service-graph counter.
//
// Neither Harvest series carries an `az` or `env` label: the family is routed
// by zone, never narrowed by matcher, so the store a query is sent to is the
// only thing deciding which of the two colliding series joins the claim.
//
// That split is what the tests below need: a storage join spanning two
// installations, zone routing over the kube-state-metrics AND Harvest
// families (observable through which aggregate the claim joins), and a
// duplicated service-graph series whose rate must not double.
func (s *MultiBackendSuite) seedFixtures() {
	s.T().Helper()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000

	// Container A — kube-state-metrics for zone-a and zone-b, plus traces.
	s.IngestExpFmt(fmt.Sprintf(`
kube_pod_info{cluster="mb-alpha",namespace="db",pod="mongo-0",uid="mb-uid-1",node="mb-worker-0",pod_ip="10.4.0.1",az="zone-a"} 1 %[1]d
kube_pod_info{cluster="mb-alpha",namespace="db",pod="mongo-1",uid="mb-uid-2",node="mb-worker-1",pod_ip="10.4.0.2",az="zone-b"} 1 %[1]d
kube_node_info{cluster="mb-alpha",node="mb-worker-0",az="zone-a"} 1 %[1]d
kube_node_info{cluster="mb-alpha",node="mb-worker-1",az="zone-b"} 1 %[1]d
kube_persistentvolumeclaim_info{cluster="mb-alpha",namespace="db",persistentvolumeclaim="mb-data",storageclass="netapp-nas",volumename="pvc-mb-9f3a",az="zone-a"} 1 %[1]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="mb-alpha",namespace="db",pod="mongo-0",volume="data",persistentvolumeclaim="mb-data",az="zone-a"} 1 %[1]d
volume_labels{volume="trident_pvc_mb_9f3a",cluster="ontap-mb-shadow",node="ontap-node-9",aggr="aggr-shadow",svm="svm-mb"} 1 %[1]d
traces_service_graph_request_total{client="mongo-0",server="mongo-1",cluster="mb-alpha",client_k8s_pod_uid="mb-uid-1",server_k8s_pod_uid="mb-uid-2",client_k8s_namespace_name="db",server_k8s_namespace_name="db"} 0 %[2]d
traces_service_graph_request_total{client="mongo-0",server="mongo-1",cluster="mb-alpha",client_k8s_pod_uid="mb-uid-1",server_k8s_pod_uid="mb-uid-2",client_k8s_namespace_name="db",server_k8s_namespace_name="db"} %[3]g %[1]d
`, t1, t0, mbRate*mbCounterStep))

	// Container B — the NetApp side, plus a BYTE-IDENTICAL copy of the same
	// service-graph counter. Both containers serve the service-graph family,
	// so the fan-out sees the series twice and must collapse it.
	s.ingestInto(s.secondURL, fmt.Sprintf(`
volume_labels{volume="trident_pvc_mb_9f3a",cluster="ontap-mb",node="ontap-node-1",aggr="aggr-mb",svm="svm-mb"} 1 %[1]d
traces_service_graph_request_total{client="mongo-0",server="mongo-1",cluster="mb-alpha",client_k8s_pod_uid="mb-uid-1",server_k8s_pod_uid="mb-uid-2",client_k8s_namespace_name="db",server_k8s_namespace_name="db"} 0 %[2]d
traces_service_graph_request_total{client="mongo-0",server="mongo-1",cluster="mb-alpha",client_k8s_pod_uid="mb-uid-1",server_k8s_pod_uid="mb-uid-2",client_k8s_namespace_name="db",server_k8s_namespace_name="db"} %[3]g %[1]d
`, t1, t0, mbRate*mbCounterStep))

	s.Require().True(s.WaitForSeries(`kube_persistentvolumeclaim_info{volumename="pvc-mb-9f3a"}`, fixedNow, 30*time.Second),
		"container A did not observe the claim fixture")
	s.Require().True(s.WaitForSeries(`volume_labels{volume="trident_pvc_mb_9f3a",aggr="aggr-shadow"}`, fixedNow, 30*time.Second),
		"container A did not observe the shadow volume_labels fixture")
	s.Require().True(s.waitForSeriesAt(s.secondURL, `volume_labels{volume="trident_pvc_mb_9f3a"}`, fixedNow, 30*time.Second),
		"container B did not observe the volume_labels fixture")
	s.Require().True(s.waitForSeriesAt(s.secondURL, `rate(traces_service_graph_request_total[5m]) > 0`, fixedNow, 30*time.Second),
		"container B did not observe a non-zero service-graph rate")
}

// --- routed server -------------------------------------------------------

// startRoutedAPI builds the API server over a Router across the two
// containers. It mirrors VMSuite.StartAPIServer's wiring exactly, except that
// the Querier handed to build.New and api.New is the router — which is also
// how cmd/kube-state-graph wires it.
func (s *MultiBackendSuite) startRoutedAPI(backends []promql.Backend) *httptest.Server {
	s.T().Helper()
	cfg := config.Defaults()
	cfg.PromURL = s.VMURL()
	cfg.LogLevel = "error"
	s.Require().NoError(cfg.Validate())

	table, err := promql.NewTable(backends)
	s.Require().NoError(err)

	logger := observability.NewLogger(cfg.LogLevel)
	metrics := observability.NewMetrics()
	router, err := promql.NewRouter(table, metrics, promql.DefaultClientFactory(metrics))
	s.Require().NoError(err)

	builder := build.New(router, build.Options{
		APITimeout: cfg.APITimeout,
		LabelKeys:  promql.LabelKeys{AZ: cfg.AZLabel, Env: cfg.EnvLabel},
	}, metrics, clock.System{})
	srv := api.New(cfg, builder, router, metrics, logger, auth.NewKeySet(), clock.System{})

	httpSrv := httptest.NewServer(srv.Handler())
	s.T().Cleanup(httpSrv.Close)
	return httpSrv
}

// familySplitBackends: kube-state-metrics, kubelet, service graph and the
// probe on container A; NetApp Harvest on container B.
func (s *MultiBackendSuite) familySplitBackends() []promql.Backend {
	return []promql.Backend{
		promql.NewBackend("k8s", s.VMURL(),
			[]promql.Family{promql.FamilyKSM, promql.FamilyKubelet, promql.FamilyServiceGraph, promql.FamilyProbe},
			nil, "", ""),
		promql.NewBackend("netapp", s.secondURL,
			[]promql.Family{promql.FamilyHarvest}, nil, "", ""),
	}
}

func (s *MultiBackendSuite) fetchGraph(srv *httptest.Server, configure func(url.Values)) cytoscape.Body {
	s.T().Helper()
	resp := s.httpGet(s.graphURL(srv.URL, configure))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	return body
}

func inventory(q url.Values) { q.Set("prune", "false") }

// --- tests ---------------------------------------------------------------

// The storage chain is assembled from two installations: the claim comes from
// the kube-state-metrics store, the volume it is bound to from the NetApp
// store, and the join is the derive-then-match of the claim's `volumename`
// against the Harvest series' stock `volume` label.
func (s *MultiBackendSuite) TestStorageJoinAcrossTwoBackends() {
	srv := s.startRoutedAPI(s.familySplitBackends())
	body := s.fetchGraph(srv, inventory)

	var storageEdges int
	for _, e := range body.Elements.Edges {
		if e.Data.Type == string(graph.EdgeTypePVCToNetAppAggr) {
			storageEdges++
			s.Equal("mb-alpha/db/mb-data", e.Data.Source)
			s.Equal("netapp/ontap-mb/aggr/aggr-mb", e.Data.Target)
		}
	}
	s.Equal(1, storageEdges,
		"the claim read from one installation must join the volume read from the other")

	var aggr, netappNode bool
	for _, n := range body.Elements.Nodes {
		switch n.Data.ID {
		case "netapp/ontap-mb/aggr/aggr-mb":
			aggr = true
		case "netapp/ontap-mb/ontap-node-1":
			netappNode = true
		}
	}
	s.True(aggr, "the aggregate materialises from the second installation")
	s.True(netappNode, "and pulls its owning controller with it")
}

// A family served only by container B must never be asked of container A, and
// vice versa — otherwise the split buys nothing.
func (s *MultiBackendSuite) TestFamiliesReachOnlyTheirOwnBackend() {
	// Point EVERY family at the kube-state-metrics store. It holds the shadow
	// volume_labels for the same PV name, so the claim must join the shadow
	// aggregate and never container B's — the destination, not the data,
	// decides.
	srv := s.startRoutedAPI([]promql.Backend{
		promql.NewBackend("k8s", s.VMURL(), promql.Families, nil, "", ""),
	})
	body := s.fetchGraph(srv, inventory)

	s.Equal([]string{"netapp/ontap-mb-shadow/aggr/aggr-shadow"}, storageTargets(body),
		"only the store the Harvest family is routed to may supply the join")
	ids := nodeIDs(body)
	s.Contains(ids, "mb-alpha/db/mb-data", "the claim is unaffected by the Harvest family's destination")
	s.NotContains(ids, "netapp/ontap-mb/aggr/aggr-mb", "container B was never asked")
}

// Zone routing: with one backend per zone, a `?az=` request reaches only that
// zone's store, and an unfiltered request reaches both.
func (s *MultiBackendSuite) TestZoneRoutingSelectsTheBackend() {
	// Container B holds no kube-state-metrics series at all, so declaring it
	// the zone-b store makes the routing decision observable: a zone-b request
	// finds nothing, a zone-a request finds the estate, and an unfiltered
	// request finds it too (both backends are asked and merged).
	backends := []promql.Backend{
		promql.NewBackend("zone-a", s.VMURL(),
			[]promql.Family{promql.FamilyKSM, promql.FamilyKubelet, promql.FamilyHarvest, promql.FamilyServiceGraph, promql.FamilyProbe},
			[]string{"zone-a"}, "", ""),
		promql.NewBackend("zone-b", s.secondURL,
			[]promql.Family{promql.FamilyKSM, promql.FamilyKubelet, promql.FamilyHarvest},
			[]string{"zone-b"}, "", ""),
	}
	srv := s.startRoutedAPI(backends)

	zoneA := s.fetchGraph(srv, func(q url.Values) {
		inventory(q)
		q.Set("az", "zone-a")
	})
	s.True(hasPod(zoneA, "mb-alpha/mb-uid-1"), "the zone-a pod is served by the zone-a store")

	// Harvest is zone-routed WITHOUT a matcher. Neither store's volume_labels
	// carries an az label, both hold the claim's PV name, and only the zone-a
	// store's copy may join: routing alone keeps the zone-b series out.
	s.Equal([]string{"netapp/ontap-mb-shadow/aggr/aggr-shadow"}, storageTargets(zoneA),
		"the zone-a claim joins the zone-a store's Harvest series, which carries no az label")
	s.NotContains(nodeIDs(zoneA), "netapp/ontap-mb/aggr/aggr-mb",
		"the zone-b store's colliding volume_labels is not loaded under ?az=zone-a")

	zoneB := s.fetchGraph(srv, func(q url.Values) {
		inventory(q)
		q.Set("az", "zone-b")
	})
	s.False(hasPod(zoneB, "mb-alpha/mb-uid-1"),
		"a zone-b request must not reach the zone-a store")
	s.False(hasPod(zoneB, "mb-alpha/mb-uid-2"),
		"the zone-b store holds no kube-state-metrics series, so the request is empty")

	unfiltered := s.fetchGraph(srv, inventory)
	s.True(hasPod(unfiltered, "mb-alpha/mb-uid-1"))
	s.True(hasPod(unfiltered, "mb-alpha/mb-uid-2"),
		"an unfiltered request fans out to every backend and merges the results")
}

// A zone no backend declares is a legitimate empty graph, not an error.
func (s *MultiBackendSuite) TestUnmatchedZoneReturnsEmptyGraph() {
	srv := s.startRoutedAPI([]promql.Backend{
		promql.NewBackend("zone-a", s.VMURL(), promql.Families, []string{"zone-a"}, "", ""),
	})
	body := s.fetchGraph(srv, func(q url.Values) {
		inventory(q)
		q.Set("az", "zone-z")
	})
	s.Empty(body.Elements.Nodes, "a zone nothing serves yields an empty graph, not an error")
}

// The same service-graph series present in BOTH stores must contribute once.
// Several readers sum across contributing series, so an undeduplicated merge
// would report exactly twice the real rate.
func (s *MultiBackendSuite) TestDuplicateServiceGraphSeriesDoesNotDoubleTheRate() {
	// Both containers serve the service-graph family and both hold the same
	// counter, so the fan-out sees the series twice.
	srv := s.startRoutedAPI([]promql.Backend{
		promql.NewBackend("k8s", s.VMURL(),
			[]promql.Family{promql.FamilyKSM, promql.FamilyKubelet, promql.FamilyServiceGraph, promql.FamilyProbe},
			nil, "", ""),
		promql.NewBackend("netapp", s.secondURL,
			[]promql.Family{promql.FamilyHarvest, promql.FamilyServiceGraph}, nil, "", ""),
	})
	body := s.fetchGraph(srv, nil)

	var found int
	for _, e := range body.Elements.Edges {
		if e.Data.Type != string(graph.EdgeTypePodCallsPod) {
			continue
		}
		s.Require().NotNil(e.Data.Metrics, "the pod-call edge must carry RED metrics")
		s.Require().NotNil(e.Data.Metrics.Rate, "the pod-call edge must carry a rate")
		found++
		s.InDelta(mbRate, *e.Data.Metrics.Rate, 0.01,
			"the duplicated series must contribute once, not twice (got %v, single-backend value is %v)",
			*e.Data.Metrics.Rate, mbRate)
	}
	s.Equal(1, found, "expected exactly one pod-calls-pod edge")
}

// A table that leaves alerts unserved still starts and serves both graph
// endpoints. When a dedicated alerts backend is declared, the ALERTS query
// reaches only it.
func (s *MultiBackendSuite) TestAlertsFamilyOptionalAndDedicated() {
	srv := s.startRoutedAPI(s.familySplitBackends())
	s.fetchGraph(srv, inventory)
	resp := s.httpGet(s.storageGraphURL(srv.URL, nil))
	s.Equal(http.StatusOK, resp.StatusCode, "an unserved alerts family still serves /v1/storage-graph")
	_ = resp.Body.Close()

	t1 := fixedNow.Unix() * 1000
	s.ingestInto(s.secondURL, fmt.Sprintf(`
ALERTS{alertname="KubePodCrashLooping",alertstate="firing",severity="warning",cluster="mb-alpha",namespace="db",pod="mongo-0",az="zone-a"} 1 %d
`, t1))
	s.Require().True(s.waitForSeriesAt(s.secondURL, `ALERTS{alertname="KubePodCrashLooping"}`, fixedNow, 30*time.Second),
		"container B did not observe the ALERTS fixture")

	var mu sync.Mutex
	var log [][2]string
	cfg := config.Defaults()
	cfg.PromURL = s.VMURL()
	cfg.LogLevel = "error"
	s.Require().NoError(cfg.Validate())
	table, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("k8s", s.VMURL(),
			[]promql.Family{promql.FamilyKSM, promql.FamilyKubelet, promql.FamilyServiceGraph, promql.FamilyProbe},
			nil, "", ""),
		promql.NewBackend("netapp", s.secondURL,
			[]promql.Family{promql.FamilyHarvest}, nil, "", ""),
		promql.NewBackend("vmalert", s.secondURL,
			[]promql.Family{promql.FamilyAlerts}, nil, "", ""),
	})
	s.Require().NoError(err)
	logger := observability.NewLogger(cfg.LogLevel)
	metrics := observability.NewMetrics()
	inner := promql.DefaultClientFactory(metrics)
	router, err := promql.NewRouter(table, metrics, func(b promql.Backend) (promql.Querier, error) {
		q, err := inner(b)
		if err != nil {
			return nil, err
		}
		return recQuerier{name: b.Name(), inner: q, mu: &mu, log: &log}, nil
	})
	s.Require().NoError(err)
	builder := build.New(router, build.Options{
		APITimeout: cfg.APITimeout,
		LabelKeys:  promql.LabelKeys{AZ: cfg.AZLabel, Env: cfg.EnvLabel},
	}, metrics, clock.System{})
	httpSrv := httptest.NewServer(api.New(cfg, builder, router, metrics, logger, auth.NewKeySet(), clock.System{}).Handler())
	s.T().Cleanup(httpSrv.Close)

	body := s.fetchGraph(httpSrv, func(q url.Values) {
		inventory(q)
		q.Set("az", "zone-a")
	})
	mu.Lock()
	seen := append([][2]string(nil), log...)
	mu.Unlock()

	var alertsBackends []string
	for _, e := range seen {
		if e[1] == "ALERTS" {
			alertsBackends = append(alertsBackends, e[0])
			s.NotEqual("k8s", e[0], "ALERTS must not reach the kube-state-metrics store")
		}
	}
	s.NotEmpty(alertsBackends, "ALERTS must be issued")
	// netapp and vmalert share a URL, so the router reuses one client and the
	// log names the first backend that claimed that URL. What holds is that
	// the query never went to k8s, and the series (ingested only on B) attached.

	var alerted bool
	for _, n := range body.Elements.Nodes {
		if n.Data.ID == "mb-alpha/mb-uid-1" && len(n.Data.Alerts) > 0 {
			alerted = true
			s.Equal("KubePodCrashLooping", n.Data.Alerts[0].Name)
		}
	}
	s.True(alerted, "the alert ingested only in the alerts store must attach")
}

type recQuerier struct {
	name  string
	inner promql.Querier
	mu    *sync.Mutex
	log   *[][2]string
}

func (q recQuerier) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	q.mu.Lock()
	*q.log = append(*q.log, [2]string{q.name, name})
	q.mu.Unlock()
	return q.inner.Instant(ctx, name, query, ts)
}

// storageTargets lists the distinct pvc-to-netapp-aggr edge targets in the
// serialised graph, sorted.
func storageTargets(body cytoscape.Body) []string {
	seen := map[string]bool{}
	for _, e := range body.Elements.Edges {
		if e.Data.Type == string(graph.EdgeTypePVCToNetAppAggr) {
			seen[e.Data.Target] = true
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// hasPod reports whether the serialised graph carries a pod node with id.
func hasPod(body cytoscape.Body, id string) bool {
	for _, n := range body.Elements.Nodes {
		if n.Data.ID == id && n.Data.Type == string(graph.NodeTypePod) {
			return true
		}
	}
	return false
}
