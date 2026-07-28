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

// ---------------------------------------------------------------------------
// ResolveRoute over a mocked store: ingress-cluster selection wiring (D10).
// The mock returns empty snapshots, so every case below finishes BEFORE the
// translate/matchcheck stages — a zero matchcheck.Runner is never invoked.
// ---------------------------------------------------------------------------

// testAt is the resolution instant — the build window's end.
var testAt = time.Unix(1_700_000_000, 0).UTC().Add(5 * time.Minute)

func testRequest(caller string, ips ...string) build.RouteRequest {
	return build.RouteRequest{
		CallerCluster: caller,
		Host:          "api.example.com",
		Path:          "/",
		Port:          443,
		IPs:           ips,
		At:            testAt,
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

// The probe runs once per destination IP at the request's own instant; a
// selection miss (disagreeing candidates) short-circuits before any snapshot
// load — LoadTrafficAt must never run.
func TestResolveRoute_SelectionMissShortCircuitsBeforeWindowLoad(t *testing.T) {
	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testAt).
		Return([]string{"prod-02"}, nil).Once()
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.8", testAt).
		Return([]string{"staging-01"}, nil).Once()
	r := NewResolver(st, matchcheck.Runner{})

	_, outcome, err := r.ResolveRoute(context.Background(),
		testRequest("prod-01", "198.51.100.7", "198.51.100.8"))
	require.NoError(t, err)
	assert.Equal(t, build.RouteAmbiguousIngress, outcome)
}

// No cluster serves the IP → RouteNoIngress, again without a snapshot load.
func TestResolveRoute_NoIngressAnywhere(t *testing.T) {
	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testAt).
		Return(nil, nil).Once()
	r := NewResolver(st, matchcheck.Runner{})

	_, outcome, err := r.ResolveRoute(context.Background(), testRequest("prod-01", "198.51.100.7"))
	require.NoError(t, err)
	assert.Equal(t, build.RouteNoIngress, outcome)
}

// Once the ingress cluster is locked, every snapshot load is scoped to it —
// one LoadTrafficAt per IP, never any other cluster. Empty snapshots yield
// RouteNoGateway (nothing serves the host), proving the pipeline ran on the
// selected cluster and stopped before translate/matchcheck.
func TestResolveRoute_LockedClusterScopesSnapshotLoads(t *testing.T) {
	st := storemocks.NewMockStore(t)
	for _, ip := range []string{"198.51.100.7", "198.51.100.8"} {
		st.EXPECT().ClustersWithIngressIP(mock.Anything, ip, testAt).
			Return([]string{"prod-02"}, nil).Once()
		st.EXPECT().LoadTrafficAt(mock.Anything, "prod-02", ip, testAt).
			Return(store.TrafficSnapshot{}, nil).Once()
	}
	r := NewResolver(st, matchcheck.Runner{})

	_, outcome, err := r.ResolveRoute(context.Background(),
		testRequest("prod-01", "198.51.100.7", "198.51.100.8"))
	require.NoError(t, err)
	assert.Equal(t, build.RouteNoGateway, outcome,
		"empty snapshot on the selected cluster is an ordinary no-gateway miss")
}

// loadSnapshot unions one per-IP load per destination IP. Two IPs published by
// the SAME ingress Service — a dual-stack Service carrying both an A and an AAAA
// address is the common shape — return byte-identical rows, and an undeduped
// union makes ScopedFor hand istiod the same VirtualService twice, which its
// config store rejects ("item already exists"): the whole endpoint then falls
// back to an external node. The union must carry each version once.
func TestLoadSnapshot_DedupsOverlappingPerIPLoads(t *testing.T) {
	from, to := testAt.Add(-time.Hour), testAt.Add(time.Hour)
	snap := store.TrafficSnapshot{
		Services: []store.ServiceRow{{
			Cluster: "prod-01", Namespace: "istio-system", Name: "ingress-lb",
			ValidFrom: from, ValidTo: to,
			LoadBalancerIPs: []string{"198.51.100.7", "2001:db8::7"},
			Selector:        []string{"istio=ingress"},
		}},
		Deploys: []store.DeployRow{{
			Cluster: "prod-01", Namespace: "istio-system", Name: "ingress",
			ValidFrom: from, ValidTo: to, PodLabels: []string{"istio=ingress"},
		}},
		Gateways: []store.GatewayRow{{
			Cluster: "prod-01", Namespace: "istio-system", Name: "gw-000",
			ValidFrom: from, ValidTo: to,
			SelectorKV: []string{"istio=ingress"}, ServerHosts: []string{"api.example.com"},
		}},
		VSes: []store.VSRow{{
			Cluster: "prod-01", Namespace: "istio-system", Name: "vs-api",
			ValidFrom: from, ValidTo: to, BoundGateways: []string{"istio-system/gw-000"},
		}},
	}

	st := storemocks.NewMockStore(t)
	for _, ip := range []string{"198.51.100.7", "2001:db8::7"} {
		// Both IPs resolve through the same Service, so the store returns the
		// same rows for each.
		st.EXPECT().LoadTrafficAt(mock.Anything, "prod-01", ip, testAt).Return(snap, nil).Once()
	}
	r := NewResolver(st, matchcheck.Runner{})

	got, err := r.loadSnapshot(context.Background(), "prod-01",
		testRequest("prod-01", "198.51.100.7", "2001:db8::7"))
	require.NoError(t, err)
	assert.Len(t, got.Services, 1, "same Service version loaded via two IPs must appear once")
	assert.Len(t, got.Deploys, 1)
	assert.Len(t, got.Gateways, 1)
	assert.Len(t, got.VSes, 1)
}

// Distinct versions of one resource — the identity repeats but the version slot
// does not — must both survive the union, so liveness selection downstream still
// has both to choose between.
func TestLoadSnapshot_KeepsDistinctVersionsOfOneResource(t *testing.T) {
	boundary := testAt.Add(-30 * time.Minute)
	older := store.GatewayRow{
		Cluster: "prod-01", Namespace: "istio-system", Name: "gw-000",
		ValidFrom: testAt.Add(-time.Hour), ValidTo: boundary,
	}
	newer := older
	newer.ValidFrom, newer.ValidTo = boundary, testAt.Add(time.Hour)

	st := storemocks.NewMockStore(t)
	st.EXPECT().LoadTrafficAt(mock.Anything, "prod-01", "198.51.100.7", testAt).
		Return(store.TrafficSnapshot{Gateways: []store.GatewayRow{older, newer}}, nil).Once()
	r := NewResolver(st, matchcheck.Runner{})

	got, err := r.loadSnapshot(context.Background(), "prod-01", testRequest("prod-01", "198.51.100.7"))
	require.NoError(t, err)
	assert.Len(t, got.Gateways, 2, "two version slots of one gateway are not duplicates")
}

// A gateway matched at the GATEWAY level (its ServerHosts union serves the
// host) can still have no :port server serving it — the listener gate must
// report RouteNoServerForHost and return BEFORE Translate and before r.run is
// touched (the zero matchcheck.Runner would blow up otherwise, which also pins
// that scoped.Host is actually threaded through: an unset Host would take the
// host-agnostic escape hatch into the RC path). The snapshot's ingress LB
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

	from, to := testAt.Add(-time.Hour), testAt.Add(time.Hour)
	snap := store.TrafficSnapshot{
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
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testAt).
		Return([]string{"prod-01"}, nil).Once()
	st.EXPECT().LoadTrafficAt(mock.Anything, "prod-01", "198.51.100.7", testAt).
		Return(snap, nil).Once()
	r := NewResolver(st, matchcheck.Runner{})

	req := testRequest("prod-01", "198.51.100.7")
	req.Host = "other.example.com"
	_, outcome, err := r.ResolveRoute(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, build.RouteNoServerForHost, outcome)
}

// Hop 3 is namespace-scoped (scope-gateway-candidates-to-ingress-namespace):
// the only selector-matching gateway serving the host lives OUTSIDE the ingress
// Service's namespace, so it is not a candidate and the pipeline misses with
// RouteNoGateway — which then feeds the ingress LB Service fallback (the
// ingress ServiceRow maps the IP to one identity), never reaching translate
// (zero matchcheck.Runner tripwire). Pre-change, the cross-namespace gateway
// was a candidate and resolution proceeded into its config.
func TestResolveRoute_CrossNamespaceGatewayNotACandidate(t *testing.T) {
	gwSpec := &networking.Gateway{
		Selector: map[string]string{"istio": "ingress"},
		Servers: []*networking.Server{{
			Port:  &networking.Port{Number: 443, Name: "api", Protocol: "HTTPS"},
			Hosts: []string{"api.example.com"},
			Tls:   &networking.ServerTLSSettings{Mode: networking.ServerTLSSettings_SIMPLE, CredentialName: "cred-api"},
		}},
	}
	specJSON, err := protojson.Marshal(gwSpec)
	require.NoError(t, err)

	from, to := testAt.Add(-time.Hour), testAt.Add(time.Hour)
	snap := store.TrafficSnapshot{
		Services: []store.ServiceRow{{
			Namespace: "istio-system", Name: "ingress-lb", ValidFrom: from, ValidTo: to,
			ExternalIPs: []string{"198.51.100.7"}, Selector: []string{"istio=ingress"},
		}},
		Deploys: []store.DeployRow{{
			Namespace: "istio-system", Name: "ingress", ValidFrom: from, ValidTo: to,
			PodLabels: []string{"istio=ingress"},
		}},
		Gateways: []store.GatewayRow{{
			// Selector matches the ingress workload and the hosts serve the
			// request — but the namespace is not the ingress Service's.
			Namespace: "team-b", Name: "gw-000", ValidFrom: from, ValidTo: to,
			SelectorKV: []string{"istio=ingress"}, ServerHosts: []string{"api.example.com"},
			SpecJSON: string(specJSON),
		}},
	}

	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testAt).
		Return([]string{"prod-01"}, nil).Once()
	st.EXPECT().LoadTrafficAt(mock.Anything, "prod-01", "198.51.100.7", testAt).
		Return(snap, nil).Once()
	r := NewResolver(st, matchcheck.Runner{})

	dest, outcome, err := r.ResolveRoute(context.Background(), testRequest("prod-01", "198.51.100.7"))
	require.NoError(t, err)
	assert.Equal(t, build.RouteIngressLBService, outcome,
		"cross-namespace gateway must not be a candidate; the no-gateway miss feeds the LB fallback")
	assert.Equal(t, "istio-system", dest.Namespace)
	assert.Equal(t, "ingress-lb", dest.Service)
	assert.Equal(t, "prod-01", dest.Cluster)
}

// The point-in-time core (simplify-route-resolution-to-point-in-time D1): with
// two Gateway versions in the loaded rows — an older one that DID serve the
// host and a current one that does not — resolution follows the version live at
// req.At. Under the old range semantics the older segment would have produced a
// hit; now it is invisible, and the current version's listener gate decides.
// The zero matchcheck.Runner is the tripwire: reaching translate would panic.
func TestResolveRoute_UsesVersionLiveAtInstant(t *testing.T) {
	served, err := protojson.Marshal(&networking.Gateway{
		Selector: map[string]string{"istio": "ingress"},
		Servers: []*networking.Server{{
			Port:  &networking.Port{Number: 443, Name: "api", Protocol: "HTTPS"},
			Hosts: []string{"api.example.com"},
			Tls:   &networking.ServerTLSSettings{Mode: networking.ServerTLSSettings_SIMPLE, CredentialName: "cred-api"},
		}},
	})
	require.NoError(t, err)
	unserved, err := protojson.Marshal(&networking.Gateway{
		Selector: map[string]string{"istio": "ingress"},
		Servers: []*networking.Server{{
			Port:  &networking.Port{Number: 443, Name: "admin", Protocol: "HTTPS"},
			Hosts: []string{"admin.example.com"},
			Tls:   &networking.ServerTLSSettings{Mode: networking.ServerTLSSettings_SIMPLE, CredentialName: "cred-admin"},
		}},
	})
	require.NoError(t, err)

	from, closed, to := testAt.Add(-time.Hour), testAt.Add(-time.Minute), testAt.Add(time.Hour)
	snap := store.TrafficSnapshot{
		Services: []store.ServiceRow{{
			Namespace: "istio-system", Name: "ingress-lb", ValidFrom: from, ValidTo: to,
			ExternalIPs: []string{"198.51.100.7"}, Selector: []string{"istio=ingress"},
		}},
		Deploys: []store.DeployRow{{
			Namespace: "istio-system", Name: "ingress", ValidFrom: from, ValidTo: to,
			PodLabels: []string{"istio=ingress"},
		}},
		Gateways: []store.GatewayRow{
			{ // superseded a minute before the instant — must not be consulted
				Namespace: "istio-system", Name: "gw-000", ValidFrom: from, ValidTo: closed,
				SelectorKV: []string{"istio=ingress"}, ServerHosts: []string{"api.example.com"},
				SpecJSON: string(served),
			},
			{ // live at the instant: serves only admin.example.com
				Namespace: "istio-system", Name: "gw-000", ValidFrom: closed, ValidTo: to, IngestSeq: 2,
				SelectorKV: []string{"istio=ingress"}, ServerHosts: []string{"*.example.com"},
				SpecJSON: string(unserved),
			},
		},
	}

	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testAt).
		Return([]string{"prod-01"}, nil).Once()
	st.EXPECT().LoadTrafficAt(mock.Anything, "prod-01", "198.51.100.7", testAt).
		Return(snap, nil).Once()
	r := NewResolver(st, matchcheck.Runner{})

	_, outcome, err := r.ResolveRoute(context.Background(), testRequest("prod-01", "198.51.100.7"))
	require.NoError(t, err)
	assert.Equal(t, build.RouteNoServerForHost, outcome,
		"the version live at the instant decides; the superseded one must be invisible")
}

// ---------------------------------------------------------------------------
// Ingress LB Service fallback (ingress-lb-service-fallback change). The nginx
// snapshot shape — LB Service + Deployment, NO Gateway CR — never reaches
// gwresolve, so the zero matchcheck.Runner tripwire applies throughout.
// ---------------------------------------------------------------------------

// nginxSnapshot is the motivating fixture: Hop 1 and Hop 2 succeed, Hop 3 has no
// Istio Gateway CR, so resolution misses at gateway selection
// (RouteNoGateway) and the fallback fires.
func nginxSnapshot() store.TrafficSnapshot {
	from, to := testAt.Add(-time.Hour), testAt.Add(time.Hour)
	return store.TrafficSnapshot{
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
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testAt).
		Return([]string{"prod-01"}, nil).Once()
	st.EXPECT().LoadTrafficAt(mock.Anything, "prod-01", "198.51.100.7", testAt).
		Return(nginxSnapshot(), nil).Once()
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
// cluster's snapshot: the fallback degrades ambiguous instead of guessing.
func TestResolveRoute_AmbiguousLBServiceOnOneIP(t *testing.T) {
	w := nginxSnapshot()
	other := w.Services[0]
	other.Namespace, other.Name = "other-ns", "some-other-lb"
	w.Services = append(w.Services, other)

	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testAt).
		Return([]string{"prod-01"}, nil).Once()
	st.EXPECT().LoadTrafficAt(mock.Anything, "prod-01", "198.51.100.7", testAt).
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
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testAt).
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
// call. Distinct hosts/callers prove the memo keys on (ip, at), not on
// the whole request. Both take the no-ingress miss path, so no snapshot load runs.
func TestBuildScoped_ProbeMemoised(t *testing.T) {
	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testAt).
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
// pipeline (empty snapshot → RouteNoGateway).
func TestBuildScoped_ProbeErrorNotCached(t *testing.T) {
	probeErr := errors.New("store unreachable")
	st := storemocks.NewMockStore(t)
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testAt).
		Return(nil, probeErr).Once()
	st.EXPECT().ClustersWithIngressIP(mock.Anything, "198.51.100.7", testAt).
		Return([]string{"prod-01"}, nil).Once()
	st.EXPECT().LoadTrafficAt(mock.Anything, "prod-01", "198.51.100.7", testAt).
		Return(store.TrafficSnapshot{}, nil).Once()
	scope := NewResolver(st, matchcheck.Runner{}).BuildScoped()

	req := testRequest("prod-01", "198.51.100.7")

	_, _, err := scope.ResolveRoute(context.Background(), req)
	require.ErrorIs(t, err, probeErr)

	_, outcome, err := scope.ResolveRoute(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, build.RouteNoGateway, outcome)
}
