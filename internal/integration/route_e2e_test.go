package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	networking "istio.io/api/networking/v1alpha3"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/route"
	"github.com/akira-core/kube-state-graph/pkg/route/gwresolve"
	"github.com/akira-core/kube-state-graph/pkg/route/matchcheck"
	"github.com/akira-core/kube-state-graph/pkg/route/memwindow"
	routestore "github.com/akira-core/kube-state-graph/pkg/route/store"
	"github.com/akira-core/kube-state-graph/pkg/route/translate"
)

// CHImage is the pinned ClickHouse image for the route-store container (same
// pin the POC benchmarked against). Never `:latest`, per D20.
const CHImage = "clickhouse/clickhouse-server:25.5"

// routeChDDL is the TEST-SIDE seed of the route-store schema. In production
// the metadata-exporter owns this DDL; kube-state-graph only validates and
// reads it (design D7). Columns must stay a superset of
// pkg/route/store.expectedSchema.
var routeChDDL = []string{
	`CREATE TABLE service_versions (
		cluster     LowCardinality(String),
		namespace   LowCardinality(String),
		name        String,
		valid_from  DateTime64(3),
		valid_to    DateTime64(3),
		ingress_ips Array(String),
		selector_kv Array(String),
		spec_json   String,
		ingest_seq  UInt64
	) ENGINE = ReplacingMergeTree(ingest_seq) ORDER BY (cluster, namespace, name, valid_from)`,
	`CREATE TABLE deploy_versions (
		cluster       LowCardinality(String),
		namespace     LowCardinality(String),
		name          String,
		valid_from    DateTime64(3),
		valid_to      DateTime64(3),
		pod_labels_kv Array(String),
		ingest_seq    UInt64
	) ENGINE = ReplacingMergeTree(ingest_seq) ORDER BY (cluster, namespace, name, valid_from)`,
	`CREATE TABLE gw_versions (
		cluster      LowCardinality(String),
		namespace    LowCardinality(String),
		name         String,
		valid_from   DateTime64(3),
		valid_to     DateTime64(3),
		selector_kv  Array(String),
		server_hosts Array(String),
		spec_json    String,
		ingest_seq   UInt64
	) ENGINE = ReplacingMergeTree(ingest_seq) ORDER BY (cluster, namespace, name, valid_from)`,
	`CREATE TABLE vs_versions (
		cluster        LowCardinality(String),
		namespace      LowCardinality(String),
		name           String,
		valid_from     DateTime64(3),
		valid_to       DateTime64(3),
		bound_gateways Array(String),
		spec_json      String,
		ingest_seq     UInt64
	) ENGINE = ReplacingMergeTree(ingest_seq) ORDER BY (cluster, namespace, name, valid_from)`,
}

// Route-store fixture constants. Validity spans a wide fixed interval around
// fixedNow so the 5m request window always overlaps (D20: no time.Now()).
var (
	routeValidFrom = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	routeValidTo   = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
)

const (
	// ingressLBIP fronts cluster-alpha's ingress gateway.
	ingressLBIP = "203.0.113.50"
	// ingressLBIP2 fronts cluster-beta's ingress gateway — the cross-cluster
	// selection fixture (caller in alpha, ingress in beta).
	ingressLBIP2 = "203.0.113.60"
	// ingressLBIPGone belongs to an ingress Service version that was CLOSED by
	// a rewrite before the query window: the probe must see the pair collapse
	// (no-FINAL dedup) and report no cluster for it.
	ingressLBIPGone = "203.0.113.70"
)

// dt64s renders a time as the string form ClickHouse parses into DateTime64(3)
// on INSERT. Times must not go through `?` binds as time.Time: clickhouse-go
// interpolates them at second precision (toDateTime), which SATURATES the 2200
// sentinel past DateTime's 2106 ceiling — the same driver trap the reader's
// dt64Lit avoids on the query side.
func dt64s(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000") }

// RouteSuite exercises translate-global-fqdn-to-k8s-service end to end: a
// real VictoriaMetrics container carries the topology + service-graph
// fixtures, a real ClickHouse container carries the versioned Istio config,
// and the REAL router_check_tool binary performs the route match. The whole
// suite skips when the tool is unavailable (it is a Linux binary; macOS dev
// machines skip, CI runs it — mirroring SkipIfDockerUnavailable).
type RouteSuite struct {
	VMSuite

	runner      matchcheck.Runner
	chContainer testcontainers.Container
	chDSN       string
	chConn      driver.Conn
}

func TestRouteSuite(t *testing.T) {
	suite.Run(t, new(RouteSuite))
}

func (s *RouteSuite) SetupSuite() {
	SkipIfDockerUnavailable(s.T())

	// Native router_check_tool only (the docker fallback was deliberately
	// dropped): KSG_ROUTER_CHECK_BIN or PATH. Absent → skip, per task 8.4.
	runner, err := matchcheck.NewRunner(os.Getenv("KSG_ROUTER_CHECK_BIN"))
	if err != nil {
		s.T().Skipf("router_check_tool unavailable (set KSG_ROUTER_CHECK_BIN or put it on PATH): %v", err)
	}
	s.runner = runner

	s.VMSuite.SetupSuite()
	s.startClickHouse()
	s.seedRouteStore()
}

func (s *RouteSuite) TearDownSuite() {
	if s.chConn != nil {
		_ = s.chConn.Close()
	}
	if s.chContainer != nil {
		_ = s.chContainer.Terminate(context.Background())
	}
	s.VMSuite.TearDownSuite()
}

// startClickHouse boots the route-store container with a network-reachable
// user (the image's default user is localhost-only) and creates the schema.
func (s *RouteSuite) startClickHouse() {
	s.T().Helper()
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        CHImage,
		ExposedPorts: []string{"9000/tcp"},
		Env: map[string]string{
			"CLICKHOUSE_USER":     "ksg",
			"CLICKHOUSE_PASSWORD": "ksg-test",
		},
		WaitingFor: wait.ForListeningPort("9000/tcp").WithStartupTimeout(90 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	s.Require().NoError(err, "start ClickHouse container")
	s.chContainer = c

	host, err := c.Host(ctx)
	s.Require().NoError(err)
	port, err := c.MappedPort(ctx, "9000/tcp")
	s.Require().NoError(err)
	addr := fmt.Sprintf("%s:%s", host, port.Port())
	s.chDSN = fmt.Sprintf("clickhouse://ksg:ksg-test@%s/default", addr)

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "default", Username: "ksg", Password: "ksg-test"},
	})
	s.Require().NoError(err)
	// The port can accept TCP before ClickHouse finishes booting; retry the
	// first ping briefly.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err = conn.Ping(ctx); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.Require().NoError(err, "ping ClickHouse")
	s.chConn = conn

	for _, stmt := range routeChDDL {
		s.Require().NoError(conn.Exec(ctx, stmt))
	}
	// Freeze background merges so the seeded stale-open/closing row pairs stay
	// two physical rows for the whole suite: the no-FINAL client dedup (and
	// its CollapsedRows counter) is then exercised DETERMINISTICALLY instead
	// of racing ClickHouse's merge scheduler — exactly the pre-merge window
	// the reader must survive in production.
	s.Require().NoError(conn.Exec(ctx, "SYSTEM STOP MERGES"))
}

func (s *RouteSuite) mustSpec(m proto.Message) string {
	s.T().Helper()
	b, err := protojson.Marshal(m)
	s.Require().NoError(err)
	return string(b)
}

// seedRouteStore writes the Istio config corpus shaped exactly like the
// metadata-exporter's history ingest: no `rev` column (a POC-only oracle
// field), spec.gateways stored VERBATIM (so a bare gateway name appears
// unqualified), and a version "closed" by REWRITING the open row (same ORDER
// BY key, higher ingest_seq) — leaving a stale open row + closing row pair
// that only FINAL collapses.
//
//   - public-gw (istio-system): seeded as THREE rows — a stale open row with
//     WRONG hosts (stale.example.com, valid_to = far future, low ingest_seq),
//     its closing rewrite (same key, higher ingest_seq, valid_to pulled in to
//     an hour before fixedNow), and the current version (TLS-terminated :443
//     serving api.example.com → payments.shop:8080). Without FINAL the stale
//     row stays live inside the query window and the suite fails
//     nondeterministically.
//   - public-gw-http (istio-system): plain HTTP :8080 serving
//     api8080.example.com, bound by its VS via the BARE name "public-gw-http"
//     (same-namespace shorthand) — the boundTo production case.
//   - cluster-beta corpus: a second, family-foreign cluster whose ingress LB
//     carries ingressLBIP2 and whose public-gw serves cross.example.com :443 →
//     payments.shop:8080 — the cross-cluster ingress-selection fixture (D10).
//   - cluster-gone: an ingress Service version carrying ingressLBIPGone whose
//     open row was CLOSED by a rewrite well before the query window — the
//     probe's no-FINAL dedup fixture.
func (s *RouteSuite) seedRouteStore() {
	s.T().Helper()
	ctx := context.Background()
	const cluster = "cluster-alpha"
	gwCloseAt := fixedNow.Add(-time.Hour)

	gwStale := s.mustSpec(&networking.Gateway{
		Selector: map[string]string{"istio": "ingressgateway"},
		Servers: []*networking.Server{{
			Port:  &networking.Port{Number: 443, Name: "https", Protocol: "HTTPS"},
			Hosts: []string{"stale.example.com"},
			Tls:   &networking.ServerTLSSettings{Mode: networking.ServerTLSSettings_SIMPLE, CredentialName: "stale-cert"},
		}},
	})
	gw443 := s.mustSpec(&networking.Gateway{
		Selector: map[string]string{"istio": "ingressgateway"},
		Servers: []*networking.Server{{
			Port: &networking.Port{Number: 443, Name: "https", Protocol: "HTTPS"},
			// noip.example.com is served and routable (→ reviews.shop) but only
			// ever queried WITHOUT client_dns_answers: if the engine were
			// consulted for an IP-less endpoint, reviews would materialise —
			// the sharp negative probe for design D6.
			Hosts: []string{"api.example.com", "noip.example.com"},
			Tls:   &networking.ServerTLSSettings{Mode: networking.ServerTLSSettings_SIMPLE, CredentialName: "api-cert"},
		}},
	})
	gw8080 := s.mustSpec(&networking.Gateway{
		Selector: map[string]string{"istio": "ingressgateway"},
		Servers: []*networking.Server{{
			Port:  &networking.Port{Number: 8080, Name: "http", Protocol: "HTTP"},
			Hosts: []string{"api8080.example.com"},
		}},
	})
	vs443 := s.mustSpec(&networking.VirtualService{
		Hosts:    []string{"api.example.com"},
		Gateways: []string{"istio-system/public-gw"},
		Http: []*networking.HTTPRoute{{
			Route: []*networking.HTTPRouteDestination{{Destination: &networking.Destination{
				Host: "payments.shop.svc.cluster.local",
				Port: &networking.PortSelector{Number: 8080},
			}}},
		}},
	})
	vsNoIP := s.mustSpec(&networking.VirtualService{
		Hosts:    []string{"noip.example.com"},
		Gateways: []string{"istio-system/public-gw"},
		Http: []*networking.HTTPRoute{{
			Route: []*networking.HTTPRouteDestination{{Destination: &networking.Destination{
				Host: "reviews.shop.svc.cluster.local",
				Port: &networking.PortSelector{Number: 8080},
			}}},
		}},
	})
	// Bare gateway binding: legal Istio shorthand for a same-namespace gateway;
	// the exporter stores it verbatim, so bound_gateways carries just the name.
	vs8080 := s.mustSpec(&networking.VirtualService{
		Hosts:    []string{"api8080.example.com"},
		Gateways: []string{"public-gw-http"},
		Http: []*networking.HTTPRoute{{
			Route: []*networking.HTTPRouteDestination{{Destination: &networking.Destination{
				Host: "payments.shop.svc.cluster.local",
				Port: &networking.PortSelector{Number: 8080},
			}}},
		}},
	})
	backendSpec, err := routestore.MarshalPorts([]routestore.SvcPort{{Name: "http", Port: 8080, Protocol: "TCP"}})
	s.Require().NoError(err)

	exec := func(query string, args ...any) {
		s.Require().NoError(s.chConn.Exec(ctx, query, args...))
	}
	// Ingress LB Service (Hop 1 of the 3-hop) + ingress Deployment (Hop 2).
	exec(`INSERT INTO service_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		cluster, "istio-system", "igw", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{ingressLBIP}, []string{"istio=ingressgateway"}, "", uint64(1))
	exec(`INSERT INTO deploy_versions VALUES (?,?,?,?,?,?,?)`,
		cluster, "istio-system", "igw-deploy", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{"istio=ingressgateway"}, uint64(2))
	// public-gw, version 1: the STALE OPEN row (wrong hosts, far-future
	// valid_to, ingest_seq=3) followed by its closing REWRITE (same ORDER BY
	// key, ingest_seq=4, valid_to pulled in). FINAL must collapse the pair to
	// the closing row, which the query window then excludes.
	exec(`INSERT INTO gw_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		cluster, "istio-system", "public-gw", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{"istio=ingressgateway"}, []string{"stale.example.com"}, gwStale, uint64(3))
	exec(`INSERT INTO gw_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		cluster, "istio-system", "public-gw", dt64s(routeValidFrom), dt64s(gwCloseAt),
		[]string{"istio=ingressgateway"}, []string{"stale.example.com"}, gwStale, uint64(4))
	// public-gw, version 2: the current config.
	exec(`INSERT INTO gw_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		cluster, "istio-system", "public-gw", dt64s(gwCloseAt), dt64s(routeValidTo),
		[]string{"istio=ingressgateway"}, []string{"api.example.com", "noip.example.com"}, gw443, uint64(5))
	exec(`INSERT INTO gw_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		cluster, "istio-system", "public-gw-http", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{"istio=ingressgateway"}, []string{"api8080.example.com"}, gw8080, uint64(6))
	// Bound VirtualServices. vs8080's bound_gateways is the BARE name, and the
	// VS lives in the gateway's namespace — exactly what the exporter stores.
	exec(`INSERT INTO vs_versions VALUES (?,?,?,?,?,?,?,?)`,
		cluster, "shop", "api-vs", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{"istio-system/public-gw"}, vs443, uint64(7))
	exec(`INSERT INTO vs_versions VALUES (?,?,?,?,?,?,?,?)`,
		cluster, "shop", "noip-vs", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{"istio-system/public-gw"}, vsNoIP, uint64(15))
	exec(`INSERT INTO vs_versions VALUES (?,?,?,?,?,?,?,?)`,
		cluster, "istio-system", "api8080-vs", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{"public-gw-http"}, vs8080, uint64(8))
	// Backend destination Services.
	exec(`INSERT INTO service_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		cluster, "shop", "payments", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{}, []string{}, backendSpec, uint64(9))
	exec(`INSERT INTO service_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		cluster, "shop", "reviews", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{}, []string{}, backendSpec, uint64(16))

	// cluster-beta: a family-foreign cluster fronted by ingressLBIP2 whose
	// gateway serves cross.example.com. A caller in cluster-alpha reaches it
	// via the |F|==0, |G|==1 selection rule (design D10).
	const beta = "cluster-beta"
	gwBeta := s.mustSpec(&networking.Gateway{
		Selector: map[string]string{"istio": "ingressgateway"},
		Servers: []*networking.Server{{
			Port:  &networking.Port{Number: 443, Name: "https", Protocol: "HTTPS"},
			Hosts: []string{"cross.example.com"},
			Tls:   &networking.ServerTLSSettings{Mode: networking.ServerTLSSettings_SIMPLE, CredentialName: "cross-cert"},
		}},
	})
	vsBeta := s.mustSpec(&networking.VirtualService{
		Hosts:    []string{"cross.example.com"},
		Gateways: []string{"istio-system/public-gw"},
		Http: []*networking.HTTPRoute{{
			Route: []*networking.HTTPRouteDestination{{Destination: &networking.Destination{
				Host: "payments.shop.svc.cluster.local",
				Port: &networking.PortSelector{Number: 8080},
			}}},
		}},
	})
	exec(`INSERT INTO service_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		beta, "istio-system", "igw", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{ingressLBIP2}, []string{"istio=ingressgateway"}, "", uint64(10))
	exec(`INSERT INTO deploy_versions VALUES (?,?,?,?,?,?,?)`,
		beta, "istio-system", "igw-deploy", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{"istio=ingressgateway"}, uint64(11))
	exec(`INSERT INTO gw_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		beta, "istio-system", "public-gw", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{"istio=ingressgateway"}, []string{"cross.example.com"}, gwBeta, uint64(12))
	exec(`INSERT INTO vs_versions VALUES (?,?,?,?,?,?,?,?)`,
		beta, "shop", "cross-vs", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{"istio-system/public-gw"}, vsBeta, uint64(13))
	exec(`INSERT INTO service_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		beta, "shop", "payments", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{}, []string{}, backendSpec, uint64(14))

	// cluster-gone: an ingress Service version carrying ingressLBIPGone,
	// closed by a REWRITE (same version slot, higher ingest_seq, valid_to
	// pulled in) two hours before fixedNow. Without the probe's client-side
	// dedup the stale open row (far-future valid_to) would overlap the window
	// and a dead cluster would appear to serve the IP.
	exec(`INSERT INTO service_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		"cluster-gone", "istio-system", "igw", dt64s(routeValidFrom), dt64s(routeValidTo),
		[]string{ingressLBIPGone}, []string{"istio=ingressgateway"}, "", uint64(20))
	exec(`INSERT INTO service_versions VALUES (?,?,?,?,?,?,?,?,?)`,
		"cluster-gone", "istio-system", "igw", dt64s(routeValidFrom), dt64s(fixedNow.Add(-2*time.Hour)),
		[]string{ingressLBIPGone}, []string{"istio=ingressgateway"}, "", uint64(21))
}

// SetupTest seeds the VM fixtures every test shares: the client pod, the
// payments Service + backing pod in cluster-alpha (so the route-engine
// destination resolves in topology and fans out), plus cluster-beta's own
// payments Service + backing pod (the cross-cluster ingress fixture — a
// route hit anchored on beta must resolve against beta's topology), all
// discriminated by test name.
func (s *RouteSuite) SetupTest() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	exposition := fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="checkout",uid="alpha-1",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="payments-0",uid="alpha-2",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="cluster-alpha",node="worker-0",test=%q} 1 %d
kube_service_info{cluster="cluster-alpha",namespace="shop",service="payments",cluster_ip="10.96.0.20",test=%q} 1 %d
kube_service_info{cluster="cluster-alpha",namespace="shop",service="reviews",cluster_ip="10.96.0.30",test=%q} 1 %d
kube_endpointslice_labels{cluster="cluster-alpha",namespace="shop",endpointslice="payments-x1",label_kubernetes_io_service_name="payments",test=%q} 1 %d
kube_endpointslice_endpoints{cluster="cluster-alpha",namespace="shop",endpointslice="payments-x1",targetref_kind="Pod",targetref_name="payments-0",targetref_namespace="shop",test=%q} 1 %d
kube_pod_info{cluster="cluster-beta",namespace="shop",pod="payments-0",uid="beta-2",node="bworker-0",test=%q} 1 %d
kube_node_info{cluster="cluster-beta",node="bworker-0",test=%q} 1 %d
kube_service_info{cluster="cluster-beta",namespace="shop",service="payments",cluster_ip="10.97.0.20",test=%q} 1 %d
kube_endpointslice_labels{cluster="cluster-beta",namespace="shop",endpointslice="payments-b1",label_kubernetes_io_service_name="payments",test=%q} 1 %d
kube_endpointslice_endpoints{cluster="cluster-beta",namespace="shop",endpointslice="payments-b1",targetref_kind="Pod",targetref_name="payments-0",targetref_namespace="shop",test=%q} 1 %d
`, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1)
	s.IngestExpFmt(exposition)
	s.Require().True(s.WaitForSeries(`kube_pod_info{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_pod_info")
}

// ingestUnknownServerSeries writes one server="unknown" service-graph series
// (two counter samples so rate() is non-zero) with the given peer labels.
func (s *RouteSuite) ingestUnknownServerSeries(peerLabels string) {
	s.T().Helper()
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	base := `traces_service_graph_request_total{client="checkout",server="unknown",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",` + peerLabels + `,test=` + strconv.Quote(disc) + `}`
	s.IngestExpFmt(fmt.Sprintf("%s 0 %d\n%s 120 %d\n", base, t0, base, t1))
	s.Require().True(
		s.WaitForSeries(`traces_service_graph_request_total{server="unknown",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the ingested server=\"unknown\" series")
}

func (s *RouteSuite) startRouteAPIServer() string {
	s.T().Helper()
	st, err := routestore.Open(context.Background(), s.chDSN)
	s.Require().NoError(err, "route store must open and pass schema validation")
	s.T().Cleanup(func() { _ = st.Close() })
	srv := s.StartAPIServer(func(cfg *config.Config) {},
		WithRouteResolver(route.NewResolver(st, s.runner), 30*time.Second))
	return srv.URL
}

// TestGlobalFQDNRoutesToService is the motivating case end to end
// (traffic_simulation): server="unknown", peer FQDN api.example.com with NO
// port (default 443 → the TLS-terminated :443 listener, RouteConfiguration
// "https.443.https.public-gw.istio-system"), client_dns_answers narrowing to
// the ingress LB IP via the ClickHouse 3-hop. The graph must carry an
// ordinary pod-calls-service edge to payments — and no external node.
func (s *RouteSuite) TestGlobalFQDNRoutesToService() {
	s.ingestUnknownServerSeries(
		`client_net_peer_name="api.example.com",client_dns_answers="` + ingressLBIP + `"`)

	url := s.startRouteAPIServer()
	resp := s.httpGet(s.graphURL(url, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	s.Contains(bodyStr, `"id":"cluster-alpha/shop/payments"`,
		"route engine must resolve the global FQDN to the routed Service")
	s.Contains(bodyStr, `"type":"pod-calls-service"`)
	s.Contains(bodyStr, `"target":"cluster-alpha/shop/payments"`)
	s.Contains(bodyStr, `"type":"service-selects-pod"`)
	s.Contains(bodyStr, `"target":"cluster-alpha/alpha-2"`,
		"the resolved service fans out to its backing pod like any other")
	s.NotContains(bodyStr, `external/api.example.com`,
		"a route-resolved peer must not leave an external node behind")
}

// TestExplicitPortRoutesViaHTTPListener covers the explicit-port path: the
// peer value carries ":8080" (design D5 step 1), the destination IP selects
// the caller's own cluster, and the HTTP :8080 listener's RouteConfiguration
// ("http.8080") routes to payments.
func (s *RouteSuite) TestExplicitPortRoutesViaHTTPListener() {
	s.ingestUnknownServerSeries(
		`client_server_address="api8080.example.com:8080",client_dns_answers="` + ingressLBIP + `"`)

	url := s.startRouteAPIServer()
	resp := s.httpGet(s.graphURL(url, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	s.Contains(bodyStr, `"id":"cluster-alpha/shop/payments"`)
	s.Contains(bodyStr, `"type":"pod-calls-service"`)
	s.NotContains(bodyStr, `external/api8080.example.com:8080`,
		"the raw peer value must not leak as an external node")
}

// TestNoDNSAnswersStaysExternal proves design D6 end to end: with the engine
// ON, a series carrying no client_dns_answers never consults the route store —
// the endpoint stays the pre-change external node. The probe host
// noip.example.com is served by the alpha gateway and routed to reviews.shop
// (present in topology), and NO other test resolves reviews — so if an
// IP-less endpoint ever consulted the engine again, the reviews service node
// would materialise and the negative assertion below would catch it even in
// this suite's shared VictoriaMetrics (earlier tests' series stay visible).
func (s *RouteSuite) TestNoDNSAnswersStaysExternal() {
	s.ingestUnknownServerSeries(`client_net_peer_name="noip.example.com"`)

	url := s.startRouteAPIServer()
	resp := s.httpGet(s.graphURL(url, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	s.Contains(bodyStr, `external/noip.example.com`,
		"no destination IPs → engine not consulted → pre-change external node")
	s.NotContains(bodyStr, `"id":"cluster-alpha/shop/reviews"`,
		"the routed Service must NOT materialise without a destination IP")
}

// TestCrossClusterIngressResolves is the design-D10/D11/D12 case end to end:
// the caller pod lives in cluster-alpha, but the destination IP belongs to
// cluster-beta's ingress (|F|==0, |G|==1 → C=beta). The route engine reads
// BETA's Gateway + VirtualService config, the service node materialises in
// beta, the pod-calls-service edge crosses clusters, and the fan-out runs
// over beta's endpoints.
func (s *RouteSuite) TestCrossClusterIngressResolves() {
	s.ingestUnknownServerSeries(
		`client_net_peer_name="cross.example.com",client_dns_answers="` + ingressLBIP2 + `"`)

	url := s.startRouteAPIServer()
	resp := s.httpGet(s.graphURL(url, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	s.Contains(bodyStr, `"id":"cluster-beta/shop/payments"`,
		"the service node lives in the engine-selected ingress cluster, not the caller's")
	s.Contains(bodyStr, `"target":"cluster-beta/shop/payments"`,
		"the pod-calls-service edge crosses from the alpha caller to the beta service")
	s.Contains(bodyStr, `"target":"cluster-beta/beta-2"`,
		"service-selects-pod fans out over the selected cluster's endpoints")
	s.NotContains(bodyStr, `"id":"cluster-alpha/shop/cross"`,
		"nothing materialises in the caller's cluster for this host")
	s.NotContains(bodyStr, `external/cross.example.com`,
		"a cross-cluster route hit must not leave an external node behind")
}

// ---------------------------------------------------------------------------
// Tool-free store coverage: everything up to (but excluding) router_check_tool
// runs against the real ClickHouse container, so developer machines without
// the Linux-only binary still exercise the window loads, the 3-hop, host
// disambiguation, and istiod translation.
// ---------------------------------------------------------------------------

type RouteStoreSuite struct {
	suite.Suite
	chContainer testcontainers.Container
	chDSN       string
	chConn      driver.Conn
}

func TestRouteStoreSuite(t *testing.T) {
	suite.Run(t, new(RouteStoreSuite))
}

func (s *RouteStoreSuite) SetupSuite() {
	SkipIfDockerUnavailable(s.T())
	// Reuse RouteSuite's container + seed helpers via a shim suite value.
	shim := &RouteSuite{}
	shim.SetT(s.T())
	shim.startClickHouse()
	shim.seedRouteStore()
	s.chContainer, s.chDSN, s.chConn = shim.chContainer, shim.chDSN, shim.chConn
}

func (s *RouteStoreSuite) TearDownSuite() {
	if s.chConn != nil {
		_ = s.chConn.Close()
	}
	if s.chContainer != nil {
		_ = s.chContainer.Terminate(context.Background())
	}
}

// TestTrafficWindowThreeHopAndTranslate drives store → memwindow → gwresolve →
// translate off the real ClickHouse rows and asserts the RouteConfiguration
// that would be handed to router_check_tool: the 3-hop narrows to the seeded
// gateways, the default-443 port selects the TLS-terminated listener's RC, and
// its route cluster names payments.
func (s *RouteStoreSuite) TestTrafficWindowThreeHopAndTranslate() {
	ctx := context.Background()
	st, err := routestore.Open(ctx, s.chDSN)
	s.Require().NoError(err, "schema validation must pass against the seeded store")
	defer func() { _ = st.Close() }()

	w, err := st.LoadTrafficWindow(ctx, "cluster-alpha", ingressLBIP,
		fixedNow.Add(-5*time.Minute), fixedNow)
	s.Require().NoError(err)
	s.Require().NotEmpty(w.Gateways, "3-hop window must reach the seeded gateways")

	mw := memwindow.New(w, fixedNow.Add(-5*time.Minute), fixedNow)
	segs := mw.Segments()
	s.Require().NotEmpty(segs)
	t := segs[0].From

	cands := mw.ResolveIPToGateways(ingressLBIP, t)
	s.Require().NotEmpty(cands, "IP 3-hop must yield candidates")
	names := make([]string, len(cands))
	gws := make([]gwresolve.Gateway, len(cands))
	for i, c := range cands {
		names[i] = c.Name
		gws[i] = gwresolve.Gateway{Name: c.Name, Hosts: c.ServerHosts}
	}
	gw, ok := gwresolve.New(gws).ResolveAmong("api.example.com", names)
	s.Require().True(ok)
	s.Equal("public-gw", gw)

	scoped, found, err := mw.ScopedFor(gw, t)
	s.Require().NoError(err)
	s.Require().True(found)
	scoped.Port = 443

	rc, err := translate.NewTranslator().Translate(scoped)
	s.Require().NoError(err)
	s.Equal("https.443.https.public-gw.istio-system", rc.GetName(),
		"default-443 must select the TLS-terminated listener's RouteConfiguration")

	var clusters []string
	for _, vh := range rc.GetVirtualHosts() {
		for _, rt := range vh.GetRoutes() {
			clusters = append(clusters, rt.GetRoute().GetCluster())
		}
	}
	s.Contains(clusters, "outbound|8080||payments.shop.svc.cluster.local")
}

// TestClustersWithIngressIPProbe covers the ingress-cluster selection probe —
// the store's ONLY cross-cluster read (design D10): each seeded LB IP names
// exactly its own cluster, an unknown IP names none, and a version pair whose
// open row was closed by a rewrite BEFORE the window collapses under the
// probe's no-FINAL dedup instead of resurrecting a dead cluster.
func (s *RouteStoreSuite) TestClustersWithIngressIPProbe() {
	ctx := context.Background()
	st, err := routestore.Open(ctx, s.chDSN)
	s.Require().NoError(err)
	defer func() { _ = st.Close() }()
	t0, t1 := fixedNow.Add(-5*time.Minute), fixedNow

	alpha, err := st.ClustersWithIngressIP(ctx, ingressLBIP, t0, t1)
	s.Require().NoError(err)
	s.Equal([]string{"cluster-alpha"}, alpha)

	beta, err := st.ClustersWithIngressIP(ctx, ingressLBIP2, t0, t1)
	s.Require().NoError(err)
	s.Equal([]string{"cluster-beta"}, beta)

	none, err := st.ClustersWithIngressIP(ctx, "198.18.0.99", t0, t1)
	s.Require().NoError(err)
	s.Empty(none, "an IP no ingress Service ever carried names no cluster")

	// The cluster-gone pair: its closing rewrite (higher ingest_seq, valid_to
	// two hours before the window) must win the client-side dedup, so the
	// stale open row's far-future valid_to never resurrects the cluster.
	gone, err := st.ClustersWithIngressIP(ctx, ingressLBIPGone, t0, t1)
	s.Require().NoError(err)
	s.Empty(gone, "a version closed before the window must not name its cluster")
	s.Positive(st.CollapsedRows(), "the stale-open/closing pair must be counted as a collapse")
}

// TestTrafficWindowNoFinalDedupAndBareRef pins the window-load behaviours the
// removed config-window test used to cover, off the IP-rooted load: the
// rewritten (closed) gateway version collapses client-side, the bare-name VS
// binding survives the gwRefs superset load into ScopedFor, and the window is
// strictly scoped to the requested cluster.
func (s *RouteStoreSuite) TestTrafficWindowNoFinalDedupAndBareRef() {
	ctx := context.Background()
	st, err := routestore.Open(ctx, s.chDSN)
	s.Require().NoError(err)
	defer func() { _ = st.Close() }()
	t0, t1 := fixedNow.Add(-5*time.Minute), fixedNow

	w, err := st.LoadTrafficWindow(ctx, "cluster-alpha", ingressLBIP, t0, t1)
	s.Require().NoError(err)
	// Exactly 2: public-gw's current version + public-gw-http (both selected
	// by the ingress deployment's labels). The seeded stale-open/closing row
	// pair for public-gw MUST collapse client-side — without the no-FINAL
	// dedup the stale open row (valid_to = far future) still overlaps the
	// window and a third row appears here.
	s.Len(w.Gateways, 2, "the stale open row must be dedup-collapsed away")
	for _, gw := range w.Gateways {
		s.NotContains(gw.ServerHosts, "stale.example.com",
			"the rewritten (closed) version's stale open row leaked past the dedup")
	}
	// 3 VSes: the qualified-ref vs443 + noip-vs AND the bare-ref vs8080 — the
	// bare form must survive the gwRefs superset load.
	s.Len(w.VSes, 3)
	s.NotEmpty(w.Services, "ingress + backend services in one window")
	s.NotEmpty(w.Deploys, "the IP 3-hop loads the ingress deployment")
	s.Positive(st.CollapsedRows(), "the stale-open/closing pair must be counted as a collapse")

	// boundTo: the bare-name binding ("public-gw-http" in the gateway's own
	// namespace) must reach ScopedFor's translate input.
	mw := memwindow.New(w, t0, t1)
	scoped, found, err := mw.ScopedFor("public-gw-http", t0)
	s.Require().NoError(err)
	s.Require().True(found)
	s.Len(scoped.Configs, 2, "gateway CR + the bare-ref-bound VirtualService")

	// Cluster scoping: the same IP under a different cluster loads nothing —
	// window loads never mix clusters (only the probe is cross-cluster).
	other, err := st.LoadTrafficWindow(ctx, "cluster-beta", ingressLBIP, t0, t1)
	s.Require().NoError(err)
	s.Empty(other.Gateways, "route-store window loads are strictly cluster-scoped")
}

// TestUniqueRowsAgainstRewriteWriterResurrectsStaleRow documents WHY pruned
// mode must never be enabled against a rewrite-close writer (the seeded shape):
// the SQL-side valid_to prune drops the closing rewrite before dedup, so the
// stale sentinel twin wins unopposed and a dead version appears live. This is
// the exact failure the default no-prune mode exists to prevent — if this test
// ever starts failing because the stale row NO LONGER resurrects, the store
// semantics changed and the WithUniqueRows guardrails should be re-examined.
func (s *RouteStoreSuite) TestUniqueRowsAgainstRewriteWriterResurrectsStaleRow() {
	ctx := context.Background()
	st, err := routestore.Open(ctx, s.chDSN, routestore.WithUniqueRows())
	s.Require().NoError(err)
	defer func() { _ = st.Close() }()

	w, err := st.LoadTrafficWindow(ctx, "cluster-alpha", ingressLBIP,
		fixedNow.Add(-5*time.Minute), fixedNow)
	s.Require().NoError(err)

	stale := false
	for _, gw := range w.Gateways {
		for _, h := range gw.ServerHosts {
			if h == "stale.example.com" {
				stale = true
			}
		}
	}
	s.True(stale, "pruned mode against a rewrite-close writer resurrects the stale open row — the documented hazard")
	s.Zero(st.CollapsedRows(), "the closing row was pruned in SQL, so dedup saw nothing to collapse (the alarm stays silent — why the mode is opt-in)")
}

// TestOpenWithAuthUsesEnvStyleCredentials proves the production path where
// the DSN carries only host/db and credentials come from WithAuth (wired from
// KSG_ROUTE_STORE_USERNAME / KSG_ROUTE_STORE_PASSWORD). The password-protected
// container rejects a credential-free Open, so a vacuous pass is impossible.
func (s *RouteStoreSuite) TestOpenWithAuthUsesEnvStyleCredentials() {
	ctx := context.Background()
	freeDSN, err := stripDSNUserinfo(s.chDSN)
	s.Require().NoError(err)

	_, err = routestore.Open(ctx, freeDSN)
	s.Require().Error(err, "password-protected ClickHouse must reject unauthenticated Open")

	st, err := routestore.Open(ctx, freeDSN, routestore.WithAuth("ksg", "ksg-test"))
	s.Require().NoError(err, "WithAuth must supply credentials after ParseDSN")
	defer func() { _ = st.Close() }()
}

// stripDSNUserinfo returns a DSN with userinfo removed so Open + WithAuth can
// be exercised the way production config prefers (secrets in env, not URL).
func stripDSNUserinfo(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.User = nil
	return u.String(), nil
}
