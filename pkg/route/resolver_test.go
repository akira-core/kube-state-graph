package route

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

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
	// no_route (deepest pipeline progress) must outrank no_listener, which
	// outranks no_gateway — the fold reports the most informative miss.
	assert.Greater(t, outcomeRank(build.RouteNoRoute), outcomeRank(build.RouteNoListenerOnPort))
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
