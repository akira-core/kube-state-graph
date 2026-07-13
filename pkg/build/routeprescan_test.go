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

// unknownPeerSample builds one server="unknown" sample whose client resolves
// to the real topology pod "abc" (cluster-alpha), with peer set on
// client_net_peer_name and any extra labels merged in.
func unknownPeerSample(peer string, extra model.Metric) model.Sample {
	m := model.Metric{
		"client":               "checkout",
		"server":               "unknown",
		"cluster":              "cluster-alpha",
		"client_k8s_pod_uid":   "abc",
		"server_k8s_pod_uid":   "",
		"client_net_peer_name": model.LabelValue(peer),
	}
	for k, v := range extra {
		m[k] = v
	}
	return model.Sample{Metric: m, Value: 5}
}

// ---------------------------------------------------------------------------
// Route-index consumption in resolveUnknownServerPeer (tasks 7.1–7.3).
// ---------------------------------------------------------------------------

// A global FQDN whose route-engine answer is a hit materialises an ordinary
// service node + pod-calls-service edge — indistinguishable from any other
// D29-resolved service node — and no external node.
func TestParseServiceGraphRoutes_GlobalFQDNRouteHitResolvesService(t *testing.T) {
	vec := sampleVec(unknownPeerSample("api.example.com", nil))
	routes := routeIndex{
		{cluster: "cluster-alpha", host: "api.example.com", path: "/", port: 443}: {
			dest:    RouteDestination{Namespace: "shop", Service: "payments", Port: 8080},
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

// An index entry that is a miss (any outcome), an engine error, or a hit whose
// destination the anchor cluster does not hold in topology all degrade to the
// pre-change external node.
func TestParseServiceGraphRoutes_IndexMissFallsExternal(t *testing.T) {
	key := routeKey{cluster: "cluster-alpha", host: "api.example.com", path: "/", port: 443}
	cases := []struct {
		name  string
		entry routeEntry
	}{
		{"no_route", routeEntry{outcome: RouteNoRoute}},
		{"no_gateway", routeEntry{outcome: RouteNoGateway}},
		{"no_listener_on_port", routeEntry{outcome: RouteNoListenerOnPort}},
		{"engine_error", routeEntry{failed: true}},
		{"hit_but_topology_lacks_service", routeEntry{
			dest:    RouteDestination{Namespace: "nowhere", Service: "ghost"},
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
// The prescan (task 7.5, design D3).
// ---------------------------------------------------------------------------

// collectRouteQueries must collect EXACTLY the endpoints that would fall to an
// external node — all three branches — and skip everything the in-cluster
// ladder resolves or the trigger never fires for. Missing the
// anchor_lacks_service branch (a global FQDN classifies as service "example"
// in namespace "com" before failing membership) would make the whole feature
// a silent no-op for its motivating case.
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
		// Duplicate of branch 3 — dedupes to one key.
		unknownPeerSample("api.example.com", nil),
	)

	keys := collectRouteQueries(vec, sampleTopologyWithServices())

	require.Len(t, keys, 3)
	// Sorted by (cluster, host, port, ips) — deterministic regardless of
	// vector arrival order (D6).
	assert.Equal(t, "203.0.113.9", keys[0].host)
	assert.Equal(t, "a.b.c.example.com", keys[1].host)
	assert.Equal(t, "api.example.com", keys[2].host)
	for _, k := range keys {
		assert.Equal(t, "cluster-alpha", k.cluster, "anchor is the client pod's own cluster")
		assert.Equal(t, 443, k.port)
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

	req := keys[0].request(time.Unix(100, 0), time.Unix(200, 0))
	assert.Equal(t, []string{"198.51.100.7", "198.51.100.8"}, req.IPs)
	assert.Equal(t, "api.example.com", req.Host)
	assert.Equal(t, 8443, req.Port)
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
		return RouteDestination{Namespace: "shop", Service: "payments", Port: 8080}, RouteHit, nil
	}}

	res, err := ReadServiceGraph(context.Background(), q, promql.Renderer{},
		window, end, sampleTopologyWithServices(), resolver, time.Second)
	require.NoError(t, err)
	require.Len(t, resolver.seen, 1)
	assert.Equal(t, RouteRequest{
		Cluster: "cluster-alpha",
		Host:    "api.example.com",
		Path:    "/",
		Port:    443,
		Start:   end.Add(-window),
		End:     end,
	}, resolver.seen[0], "the request the engine sees is exactly the prescan-derived one")

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)
	assert.Empty(t, res.ExternalNodes)
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
