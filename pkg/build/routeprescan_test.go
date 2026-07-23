package build

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// testDNSAnswer is the default destination IP stamped on every
// unknownPeerSample: destination IPs are a precondition for route resolution
// (design D6), so a sample without one is never collected. Tests exercising
// the no-IP path clear it via extra.
const testDNSAnswer = "198.51.100.7"

// fakeRouteResolver is an in-package stand-in for the mockery mock (which
// lives in pkg/build/mocks and cannot be imported from package build itself —
// import cycle). It records the requests it saw.
type fakeRouteResolver struct {
	fn   func(RouteRequest) (RouteDestination, RouteOutcome, error)
	seen []RouteRequest
}

func (f *fakeRouteResolver) ResolveRoute(_ context.Context, req RouteRequest) (RouteDestination, RouteOutcome, error) {
	f.seen = append(f.seen, req)
	return f.fn(req)
}

// scopeAwareResolver implements the optional BuildScopedRouteResolver upgrade:
// BuildScoped returns a distinct per-build resolver (scope) that records the
// requests it saw. It lets resolveRouteQueries prove it routes every call
// through the scope rather than the base resolver.
type scopeAwareResolver struct {
	fakeRouteResolver
	scope       *fakeRouteResolver
	scopedCalls int
}

func (s *scopeAwareResolver) BuildScoped() RouteResolver {
	s.scopedCalls++
	if s.scope == nil {
		s.scope = &fakeRouteResolver{fn: s.fn}
	}
	return s.scope
}

// When the resolver implements BuildScopedRouteResolver, resolveRouteQueries
// mints one scope per build and drives every ResolveRoute through it — the base
// resolver sees nothing. A plain fakeRouteResolver (no BuildScoped) is left
// untouched, so the non-implementing path is unchanged.
func TestResolveRouteQueries_UpgradesToBuildScoped(t *testing.T) {
	hit := func(RouteRequest) (RouteDestination, RouteOutcome, error) {
		return RouteDestination{}, RouteNoIngress, nil
	}
	res := &scopeAwareResolver{fakeRouteResolver: fakeRouteResolver{fn: hit}}
	keys := []routeKey{
		{callerCluster: "cluster-alpha", host: "api.example.com", path: "/", port: 443, ips: testDNSAnswer},
		{callerCluster: "cluster-beta", host: "other.example.com", path: "/", port: 443, ips: testDNSAnswer},
	}
	at := time.Unix(1_700_000_300, 0).UTC()

	idx := resolveRouteQueries(context.Background(), res, 0, keys, at)

	require.Len(t, idx, len(keys))
	assert.Equal(t, 1, res.scopedCalls, "exactly one scope minted per build")
	require.NotNil(t, res.scope)
	assert.Len(t, res.scope.seen, len(keys), "every call routed through the scope")
	assert.Empty(t, res.seen, "base resolver saw nothing once upgraded")
}

// unknownPeerSample builds one server="unknown" sample whose client resolves
// to the real topology pod "abc" (cluster-alpha), with peer set on
// client_net_peer_name, the default client_dns_answers IP, and any extra
// labels merged in (an explicit empty client_dns_answers clears the default).
func unknownPeerSample(peer string, extra model.Metric) model.Sample {
	m := model.Metric{
		"client":               "checkout",
		"server":               "unknown",
		"cluster":              "cluster-alpha",
		"client_k8s_pod_uid":   "abc",
		"server_k8s_pod_uid":   "",
		"client_net_peer_name": model.LabelValue(peer),
		"client_dns_answers":   testDNSAnswer,
	}
	for k, v := range extra {
		m[k] = v
	}
	return model.Sample{Metric: m, Value: 5}
}

// sampleTopologyTwoClusters extends sampleTopologyWithServices with a second
// cluster ("cluster-beta") holding shop/payments and its own backing pods —
// the fixture for a route-engine hit whose selected ingress cluster differs
// from the caller's (design D11/D12). cluster-alpha and cluster-beta are
// digit-free names, so each is its own exact-name family: the service node
// materialises in beta and fans out over beta's endpoints only.
func sampleTopologyTwoClusters() Topology {
	topo := sampleTopologyWithServices()
	bpay0 := &graph.PodNode{IDValue: "cluster-beta/bpay0", NameValue: "payments-0", LabelsValue: map[string]string{"cluster": "cluster-beta", "namespace": "shop"}}
	bpay1 := &graph.PodNode{IDValue: "cluster-beta/bpay1", NameValue: "payments-1", LabelsValue: map[string]string{"cluster": "cluster-beta", "namespace": "shop"}}
	topo.Pods = append(topo.Pods, bpay0, bpay1)
	topo.PodsByUID["bpay0"] = bpay0
	topo.PodsByUID["bpay1"] = bpay1
	topo.ServicesByNameNS[serviceKey{"cluster-beta", "shop", "payments"}] = ServiceObs{ClusterIP: "10.1.0.5"}
	topo.EndpointsByService[serviceKey{"cluster-beta", "shop", "payments"}] = []EndpointObs{{Pod: bpay0}, {Pod: bpay1}}
	return topo
}

// ---------------------------------------------------------------------------
// Route-index consumption in resolveUnknownServerPeer (tasks 7.1–7.3, 13.4).
// ---------------------------------------------------------------------------

// A global FQDN whose route-engine answer is a hit materialises an ordinary
// service node + pod-calls-service edge — indistinguishable from any other
// D29-resolved service node — and no external node. The parse anchors on the
// engine-selected dest.Cluster (here the caller's own).
func TestParseServiceGraphRoutes_GlobalFQDNRouteHitResolvesService(t *testing.T) {
	vec := sampleVec(unknownPeerSample("api.example.com", nil))
	routes := routeIndex{
		{callerCluster: "cluster-alpha", host: "api.example.com", path: "/", port: 443, ips: testDNSAnswer}: {
			dest:    RouteDestination{Cluster: "cluster-alpha", Namespace: "shop", Service: "payments", Port: 8080},
			outcome: RouteHit,
		},
	}
	res := parseServiceGraphRoutes(vec, sampleTopologyWithServices(), routes)

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/abc", pcs[0].Source)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)
	assert.Equal(t, "cluster-alpha", pcs[0].Labels["cluster"], "client side is a pod → edge carries cluster")

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	assert.Len(t, ssp, 2, "route-resolved service fans out like any other")
	assert.Empty(t, res.ExternalNodes, "route hit must not leave an external node behind")
}

// An ingress-lb-service-fallback resolution (the Istio pipeline missed at
// gateway resolution but the destination IP uniquely mapped to an ingress LB
// Service) rides the exact same path as a RouteHit: service node in the
// engine-selected cluster, pod-calls-service edge, endpoint fan-out, no
// external node.
func TestParseServiceGraphRoutes_IngressLBServiceFallbackResolvesService(t *testing.T) {
	vec := sampleVec(unknownPeerSample("api.example.com", nil))
	routes := routeIndex{
		{callerCluster: "cluster-alpha", host: "api.example.com", path: "/", port: 443, ips: testDNSAnswer}: {
			// The fallback's destination carries no port/subset (the LB entry
			// point, not a routed backend).
			dest:    RouteDestination{Cluster: "cluster-alpha", Namespace: "shop", Service: "payments"},
			outcome: RouteIngressLBService,
		},
	}
	res := parseServiceGraphRoutes(vec, sampleTopologyWithServices(), routes)

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/abc", pcs[0].Source)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	assert.Len(t, ssp, 2, "the LB service fans out to its backing (controller) pods")
	assert.Empty(t, res.ExternalNodes, "a fallback resolution must not leave an external node behind")
}

// A hit whose selected ingress cluster differs from the caller's anchors the
// service node in THAT cluster: the pod-calls-service edge crosses clusters
// (design D11/D12) and the service-selects-pod fan-out runs over the selected
// cluster's endpoints, not the caller's.
func TestParseServiceGraphRoutes_CrossClusterHitAnchorsOnSelectedCluster(t *testing.T) {
	vec := sampleVec(unknownPeerSample("cross.example.com", nil))
	routes := routeIndex{
		{callerCluster: "cluster-alpha", host: "cross.example.com", path: "/", port: 443, ips: testDNSAnswer}: {
			dest:    RouteDestination{Cluster: "cluster-beta", Namespace: "shop", Service: "payments", Port: 8080},
			outcome: RouteHit,
		},
	}
	res := parseServiceGraphRoutes(vec, sampleTopologyTwoClusters(), routes)

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "cluster-beta/shop/payments", res.ServiceNodes[0].IDValue,
		"the service node lives in the engine-selected ingress cluster, not the caller's")

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/abc", pcs[0].Source, "caller pod stays in its own cluster")
	assert.Equal(t, "cluster-beta/shop/payments", pcs[0].Target, "a cross-cluster pod-calls-service edge")
	assert.Equal(t, "cluster-alpha", pcs[0].Labels["cluster"], "labels.cluster stays the client side's (D9)")

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 2, "fan-out runs over the selected cluster's endpoints")
	for _, e := range ssp {
		assert.Equal(t, "cluster-beta/shop/payments", e.Source)
	}
	assert.Empty(t, res.ExternalNodes)
}

// An index entry that is a miss (any outcome — including the two
// ingress-cluster selection misses), an engine error, or a hit whose
// destination the selected cluster does not hold in topology all degrade to
// the pre-change external node.
func TestParseServiceGraphRoutes_IndexMissFallsExternal(t *testing.T) {
	key := routeKey{callerCluster: "cluster-alpha", host: "api.example.com", path: "/", port: 443, ips: testDNSAnswer}
	cases := []struct {
		name  string
		entry routeEntry
	}{
		{"no_route", routeEntry{outcome: RouteNoRoute}},
		{"no_gateway", routeEntry{outcome: RouteNoGateway}},
		{"no_listener_on_port", routeEntry{outcome: RouteNoListenerOnPort}},
		{"no_server_for_host", routeEntry{outcome: RouteNoServerForHost}},
		{"no_ingress", routeEntry{outcome: RouteNoIngress}},
		{"ambiguous_ingress_cluster", routeEntry{outcome: RouteAmbiguousIngress}},
		{"ambiguous_ingress_service", routeEntry{outcome: RouteAmbiguousIngressService}},
		{"engine_error", routeEntry{failed: true}},
		{"hit_but_topology_lacks_service", routeEntry{
			dest:    RouteDestination{Cluster: "cluster-alpha", Namespace: "nowhere", Service: "ghost"},
			outcome: RouteHit,
		}},
		{"hit_but_selected_cluster_lacks_service", routeEntry{
			// The engine picked a cluster whose topology does not hold the
			// destination — never resolve it via the caller's cluster instead.
			dest:    RouteDestination{Cluster: "cluster-gamma", Namespace: "shop", Service: "payments"},
			outcome: RouteHit,
		}},
		{"ingress_lb_service_but_topology_lacks_service", routeEntry{
			// D3: route store and VM topology disagree — the fallback hit
			// degrades exactly like a RouteHit whose service is missing.
			dest:    RouteDestination{Cluster: "cluster-alpha", Namespace: "ingress-nginx", Service: "ingress-nginx-controller"},
			outcome: RouteIngressLBService,
		}},
		{"hit_with_ingress_identity_but_backend_missing", routeEntry{
			// route-hit-ingress-chain D3: the backend resolves FIRST, so a
			// backend topology miss stays external with the ingress never
			// materialised — the identity must not leak an ingress node.
			dest: RouteDestination{
				Cluster: "cluster-alpha", Namespace: "nowhere", Service: "ghost",
				IngressNamespace: "istio-system", IngressService: "igw",
			},
			outcome: RouteHit,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vec := sampleVec(unknownPeerSample("api.example.com", nil))
			res := parseServiceGraphRoutes(vec, sampleTopologyWithServices(), routeIndex{key: c.entry})

			require.Len(t, res.ExternalNodes, 1)
			assert.Equal(t, "external/api.example.com", res.ExternalNodes[0].IDValue)
			assert.Empty(t, res.ServiceNodes)
		})
	}
}

// Regression (task 7.3): with a nil index the ladder is byte-for-byte the
// pre-change behaviour — in-cluster classifications still resolve, a global
// FQDN still falls external.
func TestParseServiceGraphRoutes_NilIndexIsPreChangeBehaviour(t *testing.T) {
	// .svc DNS grammar still resolves without any route index.
	res := parseServiceGraphRoutes(
		sampleVec(unknownPeerSample("payments.shop.svc.cluster.local", nil)),
		sampleTopologyWithServices(), nil)
	require.Len(t, res.ServiceNodes, 1)
	assert.Empty(t, res.ExternalNodes)

	// A global FQDN still falls external.
	res = parseServiceGraphRoutes(
		sampleVec(unknownPeerSample("api.example.com", nil)),
		sampleTopologyWithServices(), nil)
	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/api.example.com", res.ExternalNodes[0].IDValue)
	assert.Empty(t, res.ServiceNodes)
}

// With the engine ON (non-nil index) an endpoint carrying no destination IPs
// still falls external — same bytes as engine-off — via the route_engine_no_ip
// diagnostic branch (design D6): the graph output must be identical whether
// the index is nil or merely lacks the key.
func TestParseServiceGraphRoutes_NoIPsFallsExternalWithEngineOn(t *testing.T) {
	res := parseServiceGraphRoutes(
		sampleVec(unknownPeerSample("api.example.com", model.Metric{"client_dns_answers": ""})),
		sampleTopologyWithServices(), routeIndex{})
	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/api.example.com", res.ExternalNodes[0].IDValue)
	assert.Empty(t, res.ServiceNodes)
}

// ---------------------------------------------------------------------------
// Listener-port derivation (task 7.4, design D5).
// ---------------------------------------------------------------------------

func TestPeerRouteKey_PortDerivation(t *testing.T) {
	cases := []struct {
		name     string
		peer     peerLabels
		wantHost string
		wantPort int
	}{
		{"peer_address_port_wins",
			peerLabels{netPeerName: "api.example.com:8443", serverPort: "9999"},
			"api.example.com", 8443},
		{"label_next_server_port",
			peerLabels{netPeerName: "api.example.com", serverPort: "9443"},
			"api.example.com", 9443},
		{"label_next_net_peer_port",
			peerLabels{netPeerName: "api.example.com", netPeerPort: "8080"},
			"api.example.com", 8080},
		{"default_443",
			peerLabels{netPeerName: "api.example.com"},
			"api.example.com", 443},
		{"unparseable_label_falls_to_default",
			peerLabels{netPeerName: "api.example.com", serverPort: "https"},
			"api.example.com", 443},
		{"server_address_with_port",
			peerLabels{serverAddress: "api.example.com:8080"},
			"api.example.com", 8080},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, ok := peerRouteKey("cluster-alpha", c.peer)
			require.True(t, ok)
			assert.Equal(t, c.wantHost, key.host)
			assert.Equal(t, c.wantPort, key.port)
			assert.Equal(t, "/", key.path, "the metric carries no path dimension")
		})
	}

	_, ok := peerRouteKey("cluster-alpha", peerLabels{})
	assert.False(t, ok, "no peer value → nothing to resolve")
}

func TestParseDNSAnswers(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"10.0.0.1", []string{"10.0.0.1"}},
		{"10.0.0.1,10.0.0.2", []string{"10.0.0.1", "10.0.0.2"}},
		{"[10.0.0.1 10.0.0.2]", []string{"10.0.0.1", "10.0.0.2"}},
		{"10.0.0.1;not-an-ip;10.0.0.1", []string{"10.0.0.1"}},
		{"2001:db8::1", []string{"2001:db8::1"}},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, parseDNSAnswers(c.raw), "raw=%q", c.raw)
	}
}

// ---------------------------------------------------------------------------
// The prescan (task 7.5, design D3 + D6).
// ---------------------------------------------------------------------------

// collectRouteQueries must collect EXACTLY the endpoints that would fall to an
// external node — all three branches — and skip everything the in-cluster
// ladder resolves, the trigger never fires for, or that carries no destination
// IPs (design D6). Missing the anchor_lacks_service branch (a global FQDN
// classifies as service "example" in namespace "com" before failing
// membership) would make the whole feature a silent no-op for its motivating
// case.
func TestCollectRouteQueries_CoversAllThreeExternalBranches(t *testing.T) {
	vec := sampleVec(
		// Branch 1: no grammar matches (4-label host, not .svc).
		unknownPeerSample("a.b.c.example.com", nil),
		// Branch 2: IP literal with no ClusterIP hit in the anchor cluster.
		unknownPeerSample("203.0.113.9", nil),
		// Branch 3 — THE global-FQDN branch: 3-label host classifies as
		// (ns=com, svc=example) and then fails the anchor-membership test.
		unknownPeerSample("api.example.com", nil),
		// NOT collected: resolves in-cluster via .svc DNS.
		unknownPeerSample("payments.shop.svc.cluster.local", nil),
		// NOT collected: resolves via the anchor cluster's ClusterIP index.
		unknownPeerSample("10.0.0.5", nil),
		// NOT collected: client does not resolve to a real topology pod.
		unknownPeerSample("api.example.com", model.Metric{"client_k8s_pod_uid": "ghost-uid"}),
		// NOT collected: no peer value at all (endpoint drops).
		unknownPeerSample("", nil),
		// NOT collected: server side resolves to a real pod.
		unknownPeerSample("api.example.com", model.Metric{"server_k8s_pod_uid": "pay0"}),
		// NOT collected: no destination IPs (design D6 — the engine cannot
		// select an ingress cluster without one).
		unknownPeerSample("noip.example.com", model.Metric{"client_dns_answers": ""}),
		// NOT collected: dns answers present but nothing parses as an IP.
		unknownPeerSample("junk.example.com", model.Metric{"client_dns_answers": "not-an-ip"}),
		// Duplicate of branch 3 — dedupes to one key.
		unknownPeerSample("api.example.com", nil),
	)

	keys := collectRouteQueries(vec, sampleTopologyWithServices())

	require.Len(t, keys, 3)
	// Sorted by (callerCluster, host, port, ips) — deterministic regardless of
	// vector arrival order (D6).
	assert.Equal(t, "203.0.113.9", keys[0].host)
	assert.Equal(t, "a.b.c.example.com", keys[1].host)
	assert.Equal(t, "api.example.com", keys[2].host)
	for _, k := range keys {
		assert.Equal(t, "cluster-alpha", k.callerCluster, "the caller is the client pod's own cluster")
		assert.Equal(t, 443, k.port)
		assert.Equal(t, testDNSAnswer, k.ips, "every collected key carries destination IPs")
	}
}

func TestCollectRouteQueries_CarriesDNSAnswersAndPort(t *testing.T) {
	vec := sampleVec(unknownPeerSample("api.example.com:8443", model.Metric{
		"client_dns_answers": "198.51.100.7,198.51.100.8",
	}))
	keys := collectRouteQueries(vec, sampleTopologyWithServices())
	require.Len(t, keys, 1)
	assert.Equal(t, 8443, keys[0].port)
	assert.Equal(t, "198.51.100.7,198.51.100.8", keys[0].ips)

	at := time.Unix(200, 0)
	req := keys[0].request(at)
	assert.Equal(t, []string{"198.51.100.7", "198.51.100.8"}, req.IPs)
	assert.Equal(t, "api.example.com", req.Host)
	assert.Equal(t, 8443, req.Port)
	assert.Equal(t, "cluster-alpha", req.CallerCluster)
	assert.Equal(t, at, req.At)
}

// ---------------------------------------------------------------------------
// ReadServiceGraph with a mocked resolver (task 7.6, design D9).
// ---------------------------------------------------------------------------

func TestReadServiceGraph_ResolverErrorDegradesToExternal(t *testing.T) {
	vec := sampleVec(unknownPeerSample("api.example.com", nil))
	end := time.Unix(1_700_000_000, 0)

	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, string(promql.QServiceGraphTotal), mock.Anything, end).
		Return(vec, nil)

	resolver := &fakeRouteResolver{fn: func(RouteRequest) (RouteDestination, RouteOutcome, error) {
		return RouteDestination{}, RouteNoGateway, errors.New("store unreachable")
	}}

	res, err := ReadServiceGraph(context.Background(), q, promql.Renderer{},
		5*time.Minute, end, sampleTopologyWithServices(), resolver, time.Second)
	require.NoError(t, err, "a resolver error must never fail the read")
	require.Len(t, resolver.seen, 1, "the prescan collected the endpoint")

	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/api.example.com", res.ExternalNodes[0].IDValue)
}

func TestReadServiceGraph_ResolverHitProducesServiceNode(t *testing.T) {
	vec := sampleVec(unknownPeerSample("api.example.com", nil))
	end := time.Unix(1_700_000_000, 0)
	window := 5 * time.Minute

	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, string(promql.QServiceGraphTotal), mock.Anything, end).
		Return(vec, nil)

	resolver := &fakeRouteResolver{fn: func(RouteRequest) (RouteDestination, RouteOutcome, error) {
		return RouteDestination{Cluster: "cluster-alpha", Namespace: "shop", Service: "payments", Port: 8080}, RouteHit, nil
	}}

	res, err := ReadServiceGraph(context.Background(), q, promql.Renderer{},
		window, end, sampleTopologyWithServices(), resolver, time.Second)
	require.NoError(t, err)
	require.Len(t, resolver.seen, 1)
	assert.Equal(t, RouteRequest{
		CallerCluster: "cluster-alpha",
		Host:          "api.example.com",
		Path:          "/",
		Port:          443,
		IPs:           []string{testDNSAnswer},
		At:            end,
	}, resolver.seen[0],
		"the request the engine sees is the prescan-derived one, evaluated at the window's END")

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)
	assert.Empty(t, res.ExternalNodes)
}

// With the engine configured, an endpoint whose series carries no destination
// IPs never reaches the resolver (design D6): no engine call, and the endpoint
// stays external exactly as when the engine is off.
func TestReadServiceGraph_NoDNSAnswersNeverConsultsResolver(t *testing.T) {
	vec := sampleVec(unknownPeerSample("api.example.com", model.Metric{"client_dns_answers": ""}))
	end := time.Unix(1_700_000_000, 0)

	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, string(promql.QServiceGraphTotal), mock.Anything, end).
		Return(vec, nil)

	resolver := &fakeRouteResolver{fn: func(RouteRequest) (RouteDestination, RouteOutcome, error) {
		t.Fatal("the resolver must not be consulted for an IP-less endpoint")
		return RouteDestination{}, RouteNoGateway, nil
	}}

	res, err := ReadServiceGraph(context.Background(), q, promql.Renderer{},
		5*time.Minute, end, sampleTopologyWithServices(), resolver, time.Second)
	require.NoError(t, err)
	assert.Empty(t, resolver.seen, "no engine call for an IP-less endpoint")

	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/api.example.com", res.ExternalNodes[0].IDValue)
	assert.Empty(t, res.ServiceNodes)
}

func TestReadServiceGraph_NilResolverNeverPrescans(t *testing.T) {
	vec := sampleVec(unknownPeerSample("api.example.com", nil))
	end := time.Unix(1_700_000_000, 0)

	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, string(promql.QServiceGraphTotal), mock.Anything, end).
		Return(vec, nil)

	res, err := ReadServiceGraph(context.Background(), q, promql.Renderer{},
		5*time.Minute, end, sampleTopologyWithServices(), nil, 0)
	require.NoError(t, err)
	require.Len(t, res.ExternalNodes, 1, "feature off: pre-change external fallback")
}

// ---------------------------------------------------------------------------
// RouteHit ingress chain (route-hit-ingress-chain).
// ---------------------------------------------------------------------------

// sampleTopologyWithIngress extends sampleTopologyWithServices with the ingress
// entry-point hop: the istio-system/igw LB Service in cluster-alpha backed by
// one gateway pod (igw0).
func sampleTopologyWithIngress() Topology {
	topo := sampleTopologyWithServices()
	igw0 := &graph.PodNode{IDValue: "cluster-alpha/igw0", NameValue: "igw-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "istio-system"}}
	topo.Pods = append(topo.Pods, igw0)
	topo.PodsByUID["igw0"] = igw0
	topo.ServicesByNameNS[serviceKey{"cluster-alpha", "istio-system", "igw"}] = ServiceObs{ClusterIP: "10.0.0.80"}
	topo.EndpointsByService[serviceKey{"cluster-alpha", "istio-system", "igw"}] = []EndpointObs{{Pod: igw0}}
	return topo
}

// chainHitEntry is the canonical RouteHit-with-ingress-identity index entry the
// chain tests share: backend shop/payments fronted by istio-system/igw, both in
// cluster-alpha.
func chainHitEntry() routeEntry {
	return routeEntry{
		dest: RouteDestination{
			Cluster: "cluster-alpha", Namespace: "shop", Service: "payments", Port: 8080,
			IngressNamespace: "istio-system", IngressService: "igw",
		},
		outcome: RouteHit,
	}
}

var chainKey = routeKey{callerCluster: "cluster-alpha", host: "api.example.com", path: "/", port: 443, ips: testDNSAnswer}

// A RouteHit whose destination carries the ingress LB Service identity emits
// the full chain — caller → ingress service → ingress pod(s) → backend service
// → backend pods — PLUS the direct caller→backend edge, so the caller's
// dependency on the backend survives the shared ingress funnel.
func TestParseServiceGraphRoutes_RouteHitWithIngressIdentityEmitsChain(t *testing.T) {
	vec := sampleVec(unknownPeerSample("api.example.com", nil))
	res := parseServiceGraphRoutes(vec, sampleTopologyWithIngress(), routeIndex{chainKey: chainHitEntry()})

	assert.ElementsMatch(t, []string{
		"cluster-alpha/istio-system/igw",
		"cluster-alpha/shop/payments",
	}, svcNodeIDs(res), "ingress and backend service nodes")

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 3)
	got := map[string]string{}
	for _, e := range pcs {
		got[e.Source+"->"+e.Target] = e.Labels["cluster"]
	}
	assert.Equal(t, map[string]string{
		"cluster-alpha/abc->cluster-alpha/istio-system/igw": "cluster-alpha",
		"cluster-alpha/abc->cluster-alpha/shop/payments":    "cluster-alpha",
		"cluster-alpha/igw0->cluster-alpha/shop/payments":   "cluster-alpha",
	}, got, "caller targets the ingress entry point AND the backend directly; the synthesized ingress-pod edge targets the backend")

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	sspPairs := make([]string, 0, len(ssp))
	for _, e := range ssp {
		sspPairs = append(sspPairs, e.Source+"->"+e.Target)
	}
	assert.ElementsMatch(t, []string{
		"cluster-alpha/istio-system/igw->cluster-alpha/igw0",
		"cluster-alpha/shop/payments->cluster-alpha/pay0",
		"cluster-alpha/shop/payments->cluster-alpha/pay1",
	}, sspPairs)
	assert.Empty(t, res.ExternalNodes)
}

// Chain degrade matrix: every precondition failure keeps today's RouteHit
// shape byte-for-byte — direct caller→backend edge, family-wide backend
// fan-out, no ingress node, no external.
func TestParseServiceGraphRoutes_ChainDegradesToDirectEdge(t *testing.T) {
	noEndpoints := sampleTopologyWithIngress()
	noEndpoints.EndpointsByService = map[serviceKey][]EndpointObs{
		{"cluster-alpha", "shop", "payments"}: noEndpoints.EndpointsByService[serviceKey{"cluster-alpha", "shop", "payments"}],
		{"cluster-alpha", "db", "mongo"}:      noEndpoints.EndpointsByService[serviceKey{"cluster-alpha", "db", "mongo"}],
		// istio-system/igw held in topology but with zero backing pods.
	}

	equalsBackend := chainHitEntry()
	equalsBackend.dest.IngressNamespace, equalsBackend.dest.IngressService = "shop", "payments"

	cases := []struct {
		name  string
		topo  Topology
		entry routeEntry
	}{
		// Identity ambiguous/absent in the window: the engine left Ingress* empty.
		{"no_ingress_identity", sampleTopologyWithIngress(), routeEntry{
			dest:    RouteDestination{Cluster: "cluster-alpha", Namespace: "shop", Service: "payments", Port: 8080},
			outcome: RouteHit,
		}},
		// The locked cluster's topology does not hold the ingress Service.
		{"ingress_missing_from_topology", sampleTopologyWithServices(), chainHitEntry()},
		// Held but with no backing pods: an empty middle would disconnect the
		// caller from the backend.
		{"ingress_has_no_endpoints", noEndpoints, chainHitEntry()},
		// The destination IS the ingress entry point: a self-sandwich chain is
		// meaningless.
		{"ingress_equals_backend", sampleTopologyWithIngress(), equalsBackend},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vec := sampleVec(unknownPeerSample("api.example.com", nil))
			res := parseServiceGraphRoutes(vec, c.topo, routeIndex{chainKey: c.entry})

			assert.Equal(t, []string{"cluster-alpha/shop/payments"}, svcNodeIDs(res),
				"exactly the backend node — no ingress node materialised")

			pcs := edgesByType(res, graph.EdgeTypePodCallsService)
			require.Len(t, pcs, 1)
			assert.Equal(t, "cluster-alpha/abc", pcs[0].Source)
			assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target, "direct caller->backend edge")

			assert.Len(t, edgesByType(res, graph.EdgeTypeServiceSelectsPod), 2, "backend fan-out only")
			assert.Empty(t, res.ExternalNodes, "a degrade never falls external")
		})
	}
}

// A trace-derived edge for the same (ingress pod, backend service) pair wins
// over the synthesized chain edge — the two would otherwise share one
// deterministic UUIDv5 edge ID.
func TestParseServiceGraphRoutes_ChainSynthEdgeDedupsAgainstTracedEdge(t *testing.T) {
	traced := model.Sample{Metric: model.Metric{
		"client":             "igw",
		"server":             "http://payments.shop",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "igw0",
		"server_k8s_pod_uid": "",
	}, Value: 3}
	vec := sampleVec(unknownPeerSample("api.example.com", nil), traced)

	res := parseServiceGraphRoutes(vec, sampleTopologyWithIngress(), routeIndex{chainKey: chainHitEntry()})

	var igwToPayments int
	for _, e := range edgesByType(res, graph.EdgeTypePodCallsService) {
		if e.Source == "cluster-alpha/igw0" && e.Target == "cluster-alpha/shop/payments" {
			igwToPayments++
			assert.Equal(t, "cluster-alpha", e.Labels["cluster"])
		}
	}
	assert.Equal(t, 1, igwToPayments, "exactly one edge for the pair (traced wins; identical edge ID otherwise)")
}

// The chain's direct caller→backend edge shares the traced-edge pairKey (and
// deterministic UUIDv5 edge ID): a trace-derived series that already yields
// caller→backend collapses with it into a single edge.
func TestParseServiceGraphRoutes_ChainDirectEdgeDedupsAgainstTracedEdge(t *testing.T) {
	traced := model.Sample{Metric: model.Metric{
		"client":             "checkout",
		"server":             "http://payments.shop",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "abc",
		"server_k8s_pod_uid": "",
	}, Value: 3}
	vec := sampleVec(unknownPeerSample("api.example.com", nil), traced)

	res := parseServiceGraphRoutes(vec, sampleTopologyWithIngress(), routeIndex{chainKey: chainHitEntry()})

	var callerToPayments int
	for _, e := range edgesByType(res, graph.EdgeTypePodCallsService) {
		if e.Source == "cluster-alpha/abc" && e.Target == "cluster-alpha/shop/payments" {
			callerToPayments++
			assert.Equal(t, "cluster-alpha", e.Labels["cluster"])
		}
	}
	assert.Equal(t, 1, callerToPayments, "the pairs map collapses the chain's direct edge with the traced one")
}

// The ingress-lb-service-fallback destination is the LB entry point: its
// service-selects-pod fan-out is LOCKED to the selected cluster's own
// endpoints — a family sibling's same-named Service is not behind the IP
// (route-hit-ingress-chain D2, superseding the family-wide fan-out).
func TestParseServiceGraphRoutes_IngressLBServiceFanOutIsLockedClusterOnly(t *testing.T) {
	vec := sampleVec(unknownPeerSample("nats.example.com", model.Metric{"cluster": "prod-1"}))
	routes := routeIndex{
		{callerCluster: "prod-1", host: "nats.example.com", path: "/", port: 443, ips: testDNSAnswer}: {
			dest: RouteDestination{
				Cluster: "prod-1", Namespace: "messaging", Service: "nats",
				IngressNamespace: "messaging", IngressService: "nats",
			},
			outcome: RouteIngressLBService,
		},
	}
	res := parseServiceGraphRoutes(vec, familyTopology(), routes)

	assert.Equal(t, []string{"prod-1/messaging/nats"}, svcNodeIDs(res))
	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 1, "locked-cluster fan-out: prod-2's same-named Service contributes no pod")
	assert.Equal(t, "prod-1/n1", ssp[0].Target)
	assert.Empty(t, res.ExternalNodes)
}

// In a cluster family, the chain's ingress hop stays locked to the selected
// cluster (fan-out AND synthesized edges) while the backend keeps the
// family-wide endpoint union.
func TestParseServiceGraphRoutes_ChainIngressLockedBackendFamilyWide(t *testing.T) {
	topo := familyTopology()
	igwP1 := &graph.PodNode{IDValue: "prod-1/igw1", NameValue: "igw-0", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "istio-system"}}
	igwP2 := &graph.PodNode{IDValue: "prod-2/igw2", NameValue: "igw-0", LabelsValue: map[string]string{"cluster": "prod-2", "namespace": "istio-system"}}
	topo.Pods = append(topo.Pods, igwP1, igwP2)
	topo.PodsByUID["igw1"] = igwP1
	topo.PodsByUID["igw2"] = igwP2
	topo.ServicesByNameNS[serviceKey{"prod-1", "istio-system", "igw"}] = ServiceObs{ClusterIP: "10.1.0.80"}
	topo.ServicesByNameNS[serviceKey{"prod-2", "istio-system", "igw"}] = ServiceObs{ClusterIP: "10.2.0.80"}
	topo.EndpointsByService[serviceKey{"prod-1", "istio-system", "igw"}] = []EndpointObs{{Pod: igwP1}}
	topo.EndpointsByService[serviceKey{"prod-2", "istio-system", "igw"}] = []EndpointObs{{Pod: igwP2}}

	vec := sampleVec(unknownPeerSample("nats.example.com", model.Metric{"cluster": "prod-1"}))
	routes := routeIndex{
		{callerCluster: "prod-1", host: "nats.example.com", path: "/", port: 443, ips: testDNSAnswer}: {
			dest: RouteDestination{
				Cluster: "prod-1", Namespace: "messaging", Service: "nats",
				IngressNamespace: "istio-system", IngressService: "igw",
			},
			outcome: RouteHit,
		},
	}
	res := parseServiceGraphRoutes(vec, topo, routes)

	assert.ElementsMatch(t, []string{"prod-1/istio-system/igw", "prod-1/messaging/nats"}, svcNodeIDs(res))

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	pcsPairs := make([]string, 0, len(pcs))
	for _, e := range pcs {
		pcsPairs = append(pcsPairs, e.Source+"->"+e.Target)
	}
	assert.ElementsMatch(t, []string{
		"prod-1/abc->prod-1/istio-system/igw",
		"prod-1/abc->prod-1/messaging/nats",
		"prod-1/igw1->prod-1/messaging/nats",
	}, pcsPairs, "synthesized edges from the locked cluster's ingress pods only — prod-2/igw2 emits nothing; the direct caller->backend edge is kept")

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	sspPairs := make([]string, 0, len(ssp))
	for _, e := range ssp {
		sspPairs = append(sspPairs, e.Source+"->"+e.Target)
	}
	assert.ElementsMatch(t, []string{
		"prod-1/istio-system/igw->prod-1/igw1", // ingress: locked-cluster only
		"prod-1/messaging/nats->prod-1/n1",     // backend: family-wide union
		"prod-1/messaging/nats->prod-2/n2",
	}, sspPairs)
	assert.Empty(t, res.ExternalNodes)
}
