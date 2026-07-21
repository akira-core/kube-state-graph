package route

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	networking "istio.io/api/networking/v1alpha3"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/matchcheck"
	"github.com/akira-core/kube-state-graph/pkg/route/store"
	storemocks "github.com/akira-core/kube-state-graph/pkg/route/store/mocks"
)

func TestParseEnvoyCluster(t *testing.T) {
	cases := []struct {
		name    string
		cluster string
		want    build.RouteDestination
		wantOK  bool
	}{
		{"plain",
			"outbound|8080||reviews.prod.svc.cluster.local",
			build.RouteDestination{Namespace: "prod", Service: "reviews", Port: 8080}, true},
		{"with_subset",
			"outbound|8443|v2|reviews.prod.svc.cluster.local",
			build.RouteDestination{Namespace: "prod", Service: "reviews", Port: 8443, Subset: "v2"}, true},
		{"route_miss_empty", "", build.RouteDestination{}, false},
		{"inbound_direction", "inbound|8080||reviews.prod.svc.cluster.local", build.RouteDestination{}, false},
		{"bad_port", "outbound|http||reviews.prod.svc.cluster.local", build.RouteDestination{}, false},
		{"not_in_cluster_fqdn", "outbound|443||api.example.com", build.RouteDestination{}, false},
		{"too_few_parts", "outbound|8080|reviews.prod.svc.cluster.local", build.RouteDestination{}, false},
		{"bare_service_no_suffix", "outbound|8080||reviews", build.RouteDestination{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseEnvoyCluster(c.cluster)
			assert.Equal(t, c.wantOK, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestOutcomeRank(t *testing.T) {
	// no_route (deepest pipeline progress) must outrank no_server_for_host
	// (right port, unserved host), which outranks no_listener (wrong port),
	// which outranks no_gateway — the fold reports the most informative miss.
	assert.Greater(t, outcomeRank(build.RouteNoRoute), outcomeRank(build.RouteNoServerForHost))
	assert.Greater(t, outcomeRank(build.RouteNoServerForHost), outcomeRank(build.RouteNoListenerOnPort))
	assert.Greater(t, outcomeRank(build.RouteNoListenerOnPort), outcomeRank(build.RouteNoGateway))
}

// ---------------------------------------------------------------------------
// ResolveRoute over a mocked store: ingress-cluster selection wiring (D10).
// The mock returns empty windows, so every case below finishes BEFORE the
// translate/matchcheck stages — a zero matchcheck.Runner is never invoked.
// ---------------------------------------------------------------------------

var (
	testStart = time.Unix(1_700_000_000, 0).UTC()
	testEnd   = testStart.Add(5 * time.Minute)
)

func testRequest(caller string, ips ...string) build.RouteRequest {
	return build.RouteRequest{
		CallerCluster: caller,
		Host:          "api.example.com",
		Path:          "/",
		Port:          443,
		IPs:           ips,
		Start:         testStart,
		End:           testEnd,
	}
}

// An IP-less request is a defensive miss: RouteNoIngress without a single
// store call (the prescan never emits one — design D6).
func TestResolveRoute_NoIPsMissesWithoutStoreCall(t *testing.T) {
	st := storemocks.NewMockStore(t) // no expectations: any call fails the test
	r := NewResolver(st, matchcheck.Runner{})

	dest, outcome, err := r.ResolveRoute(context.Background(), testRequest("prod-01"))
	require.NoError(t, err)
	assert.Equal(t, build.RouteNoIngress, outcome)
	assert.Equal(t, build.RouteDestination{}, dest)
}

// The probe runs once per destination IP with the request's own window; a
// selection miss (disagreeing candidates) short-circuits before any window
// load — LoadTrafficWindow must never run.
func TestResolveRoute_SelectionMissShortCircuitsBeforeWindowLoad(t *testing.T) {
	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testStart, testEnd).
		Return([]string{"prod-02"}, nil).Once()
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.8", testStart, testEnd).
		Return([]string{"staging-01"}, nil).Once()
	r := NewResolver(st, matchcheck.Runner{})

	_, outcome, err := r.ResolveRoute(context.Background(),
		testRequest("prod-01", "198.51.100.7", "198.51.100.8"))
	require.NoError(t, err)
	assert.Equal(t, build.RouteAmbiguousIngress, outcome)
}

// No cluster serves the IP → RouteNoIngress, again without a window load.
func TestResolveRoute_NoIngressAnywhere(t *testing.T) {
	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testStart, testEnd).
		Return(nil, nil).Once()
	r := NewResolver(st, matchcheck.Runner{})

	_, outcome, err := r.ResolveRoute(context.Background(), testRequest("prod-01", "198.51.100.7"))
	require.NoError(t, err)
	assert.Equal(t, build.RouteNoIngress, outcome)
}

// Once the ingress cluster is locked, every window load is scoped to it — one
// LoadTrafficWindow per IP, never any other cluster. Empty windows yield
// RouteNoGateway (nothing serves the host), proving the pipeline ran on the
// selected cluster and stopped before translate/matchcheck.
func TestResolveRoute_LockedClusterScopesWindowLoads(t *testing.T) {
	st := storemocks.NewMockStore(t)
	for _, ip := range []string{"198.51.100.7", "198.51.100.8"} {
		st.EXPECT().ClustersWithIngressIP(mock.Anything, ip, testStart, testEnd).
			Return([]string{"prod-02"}, nil).Once()
		st.EXPECT().LoadTrafficWindow(mock.Anything, "prod-02", ip, testStart, testEnd).
			Return(store.TrafficWindow{}, nil).Once()
	}
	r := NewResolver(st, matchcheck.Runner{})

	_, outcome, err := r.ResolveRoute(context.Background(),
		testRequest("prod-01", "198.51.100.7", "198.51.100.8"))
	require.NoError(t, err)
	assert.Equal(t, build.RouteNoGateway, outcome,
		"empty window on the selected cluster is an ordinary no-gateway miss")
}

// A gateway matched at the GATEWAY level (its ServerHosts union serves the
// host) can still have no :port server serving it — the listener gate must
// report RouteNoServerForHost and return BEFORE Translate and before r.run is
// touched (the zero matchcheck.Runner would blow up otherwise, which also pins
// that scoped.Host is actually threaded through: an unset Host would take the
// host-agnostic escape hatch into the RC path). The window's ingress LB
// ServiceRow additionally pins the fallback's no-gateway gate: a deep Istio
// miss must NOT be masked by an ingress_lb_service resolution.
func TestResolveRoute_HostNotServedByAnyServerOnPort(t *testing.T) {
	gwSpec := &networking.Gateway{
		Selector: map[string]string{"istio": "ingress"},
		Servers: []*networking.Server{
			{
				Port:  &networking.Port{Number: 443, Name: "api", Protocol: "HTTPS"},
				Hosts: []string{"api.example.com"},
				Tls:   &networking.ServerTLSSettings{Mode: networking.ServerTLSSettings_SIMPLE, CredentialName: "cred-api"},
			},
			{
				Port:  &networking.Port{Number: 443, Name: "admin", Protocol: "HTTPS"},
				Hosts: []string{"admin.example.com"},
				Tls:   &networking.ServerTLSSettings{Mode: networking.ServerTLSSettings_SIMPLE, CredentialName: "cred-admin"},
			},
		},
	}
	specJSON, err := protojson.Marshal(gwSpec)
	require.NoError(t, err)

	from, to := testStart.Add(-time.Hour), testEnd.Add(time.Hour)
	window := store.TrafficWindow{
		Services: []store.ServiceRow{{
			Namespace: "istio-system", Name: "ingress-lb", ValidFrom: from, ValidTo: to,
			ExternalIPs: []string{"198.51.100.7"}, Selector: []string{"istio=ingress"},
		}},
		Deploys: []store.DeployRow{{
			Namespace: "istio-system", Name: "ingress", ValidFrom: from, ValidTo: to,
			PodLabels: []string{"istio=ingress"},
		}},
		Gateways: []store.GatewayRow{{
			Namespace: "istio-system", Name: "gw-000", ValidFrom: from, ValidTo: to,
			SelectorKV: []string{"istio=ingress"},
			// Gateway-level union matches the request host; the per-server
			// hosts inside SpecJSON do not.
			ServerHosts: []string{"*.example.com"},
			SpecJSON:    string(specJSON),
		}},
	}

	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testStart, testEnd).
		Return([]string{"prod-01"}, nil).Once()
	st.EXPECT().LoadTrafficWindow(mock.Anything, "prod-01", "198.51.100.7", testStart, testEnd).
		Return(window, nil).Once()
	r := NewResolver(st, matchcheck.Runner{})

	req := testRequest("prod-01", "198.51.100.7")
	req.Host = "other.example.com"
	_, outcome, err := r.ResolveRoute(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, build.RouteNoServerForHost, outcome)
}

// ---------------------------------------------------------------------------
// Ingress LB Service fallback (ingress-lb-service-fallback change). The nginx
// window shape — LB Service + Deployment, NO Gateway CR — never reaches
// gwresolve, so the zero matchcheck.Runner tripwire applies throughout.
// ---------------------------------------------------------------------------

// nginxWindow is the motivating fixture: Hop 1 and Hop 2 succeed, Hop 3 has no
// Istio Gateway CR, so every segment misses at gateway resolution (the folded
// miss stays RouteNoGateway) and the fallback fires.
func nginxWindow() store.TrafficWindow {
	from, to := testStart.Add(-time.Hour), testEnd.Add(time.Hour)
	return store.TrafficWindow{
		Services: []store.ServiceRow{{
			Namespace: "ingress-nginx", Name: "ingress-nginx-controller",
			ValidFrom: from, ValidTo: to,
			LoadBalancerIPs: []string{"198.51.100.7"},
			Selector:        []string{"app=ingress-nginx"},
		}},
		Deploys: []store.DeployRow{{
			Namespace: "ingress-nginx", Name: "ingress-nginx-controller",
			ValidFrom: from, ValidTo: to,
			PodLabels: []string{"app=ingress-nginx"},
		}},
	}
}

func TestResolveRoute_NginxIngressFallsBackToLBService(t *testing.T) {
	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testStart, testEnd).
		Return([]string{"prod-01"}, nil).Once()
	st.EXPECT().LoadTrafficWindow(mock.Anything, "prod-01", "198.51.100.7", testStart, testEnd).
		Return(nginxWindow(), nil).Once()
	r := NewResolver(st, matchcheck.Runner{})

	dest, outcome, err := r.ResolveRoute(context.Background(), testRequest("prod-01", "198.51.100.7"))
	require.NoError(t, err)
	assert.Equal(t, build.RouteIngressLBService, outcome)
	assert.Equal(t, build.RouteDestination{
		Cluster: "prod-01", Namespace: "ingress-nginx", Service: "ingress-nginx-controller",
		IngressNamespace: "ingress-nginx", IngressService: "ingress-nginx-controller",
	}, dest, "dest carries the locked ingress cluster (D11), the ingress identity mirror, and no port/subset")
}

// Two differently-named LB Services carrying the same IP in the selected
// cluster's window: the fallback degrades ambiguous instead of guessing.
func TestResolveRoute_AmbiguousLBServiceOnOneIP(t *testing.T) {
	w := nginxWindow()
	other := w.Services[0]
	other.Namespace, other.Name = "other-ns", "some-other-lb"
	w.Services = append(w.Services, other)

	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testStart, testEnd).
		Return([]string{"prod-01"}, nil).Once()
	st.EXPECT().LoadTrafficWindow(mock.Anything, "prod-01", "198.51.100.7", testStart, testEnd).
		Return(w, nil).Once()
	r := NewResolver(st, matchcheck.Runner{})

	dest, outcome, err := r.ResolveRoute(context.Background(), testRequest("prod-01", "198.51.100.7"))
	require.NoError(t, err)
	assert.Equal(t, build.RouteAmbiguousIngressService, outcome)
	assert.Equal(t, build.RouteDestination{}, dest)
}

// A probe error is an infrastructure failure: returned to the caller (which
// records route_engine_error and degrades to external), never swallowed.
func TestResolveRoute_ProbeErrorPropagates(t *testing.T) {
	probeErr := errors.New("store unreachable")
	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testStart, testEnd).
		Return(nil, probeErr).Once()
	r := NewResolver(st, matchcheck.Runner{})

	_, _, err := r.ResolveRoute(context.Background(), testRequest("prod-01", "198.51.100.7"))
	assert.ErrorIs(t, err, probeErr)
}

// ---------------------------------------------------------------------------
// BuildScoped: per-build memoisation of the ingress-IP probe (perf fix).
// ---------------------------------------------------------------------------

// *Resolver satisfies the optional upgrade interface, so the prescan drives
// every ResolveRoute of a build through one scope.
func TestResolver_ImplementsBuildScoped(t *testing.T) {
	var _ build.BuildScopedRouteResolver = NewResolver(storemocks.NewMockStore(t), matchcheck.Runner{})
}

// Two requests in one scope sharing a destination IP probe the store ONCE:
// ClustersWithIngressIP.Once() fails if the memo does not collapse the second
// call. Distinct hosts/callers prove the memo keys on (ip, start, end), not on
// the whole request. Both take the no-ingress miss path, so no window load runs.
func TestBuildScoped_ProbeMemoised(t *testing.T) {
	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testStart, testEnd).
		Return(nil, nil).Once()
	scope := NewResolver(st, matchcheck.Runner{}).BuildScoped()

	req1 := testRequest("prod-01", "198.51.100.7")
	req1.Host = "api.example.com"
	req2 := testRequest("prod-02", "198.51.100.7")
	req2.Host = "other.example.com"

	for _, req := range []build.RouteRequest{req1, req2} {
		_, outcome, err := scope.ResolveRoute(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, build.RouteNoIngress, outcome)
	}
}

// A failed probe is NOT cached: a later request sharing the IP retries the
// store. First call errors (propagated), second succeeds and proceeds into the
// pipeline (empty window → RouteNoGateway).
func TestBuildScoped_ProbeErrorNotCached(t *testing.T) {
	probeErr := errors.New("store unreachable")
	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testStart, testEnd).
		Return(nil, probeErr).Once()
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testStart, testEnd).
		Return([]string{"prod-01"}, nil).Once()
	st.EXPECT().LoadTrafficWindow(mock.Anything, "prod-01", "198.51.100.7", testStart, testEnd).
		Return(store.TrafficWindow{}, nil).Once()
	scope := NewResolver(st, matchcheck.Runner{}).BuildScoped()

	req := testRequest("prod-01", "198.51.100.7")

	_, _, err := scope.ResolveRoute(context.Background(), req)
	require.ErrorIs(t, err, probeErr)

	_, outcome, err := scope.ResolveRoute(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, build.RouteNoGateway, outcome)
}
