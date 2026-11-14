package route

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	envoyroute "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/encoding/protojson"
	networking "istio.io/api/networking/v1alpha3"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/matchcheck"
	"github.com/akira-core/kube-state-graph/pkg/route/store"
	"github.com/akira-core/kube-state-graph/pkg/route/translate"
)

// ---------------------------------------------------------------------------
// Counting fakes for the three expensive stages. They exist so the resolver's
// orchestration — memoisation, batching, the engine limit, the result cache —
// can be pinned by call counts and driven under -race without ClickHouse, an
// istiod world, or router_check_tool. The in-memory stages (snapshot, gwresolve,
// ListenerFor) stay REAL, so every fixture walks the production path up to the
// translate seam.
// ---------------------------------------------------------------------------

// sharedIP / sharedCluster are the one ingress every shared-ingress key routes
// through.
const (
	sharedIP      = "198.51.100.7"
	sharedCluster = "prod-01"
)

// fakeStore is a store.Store over one fixed snapshot, counting every read.
// Hooks, when set, replace the default answer for one call (fault injection).
type fakeStore struct {
	snap    store.TrafficSnapshot
	latency time.Duration

	probes, loads atomic.Int64

	probeHook func(ip string) ([]string, error)
	loadHook  func(cluster, ip string) (store.TrafficSnapshot, error)
}

var _ store.Store = (*fakeStore)(nil)

func (f *fakeStore) Close() error { return nil }

func (f *fakeStore) ClustersWithIngressIP(ctx context.Context, ip string, _ time.Time) ([]string, error) {
	f.probes.Add(1)
	if err := sleepCtx(ctx, f.latency/4); err != nil {
		return nil, err
	}
	if f.probeHook != nil {
		return f.probeHook(ip)
	}
	return []string{sharedCluster}, nil
}

func (f *fakeStore) LoadTrafficAt(ctx context.Context, cluster, ip string, _ time.Time) (store.TrafficSnapshot, error) {
	f.loads.Add(1)
	if err := sleepCtx(ctx, f.latency); err != nil {
		return store.TrafficSnapshot{}, err
	}
	if f.loadHook != nil {
		return f.loadHook(cluster, ip)
	}
	return f.snap, nil
}

// fakeTranslator returns an RC named exactly as the real translator names it
// (translate.ListenerFor), so a test can tell which listener was translated.
type fakeTranslator struct {
	latency time.Duration
	calls   atomic.Int64
	hook    func(in translate.ScopedInput) (*envoyroute.RouteConfiguration, error)
}

func (f *fakeTranslator) Translate(in translate.ScopedInput) (*envoyroute.RouteConfiguration, error) {
	f.calls.Add(1)
	time.Sleep(f.latency)
	if f.hook != nil {
		return f.hook(in)
	}
	name, _ := translate.ListenerFor(in)
	return &envoyroute.RouteConfiguration{Name: name}, nil
}

// fakeMatcher routes every host "<label>.<rest>" to the Service <label> in
// namespace shop, so each host has its own distinct, checkable destination.
// Hosts listed in miss route nowhere ("").
type fakeMatcher struct {
	latency time.Duration

	execs, queries atomic.Int64

	mu       sync.Mutex
	inflight int
	peak     int

	miss map[string]bool
	hook func(rc *envoyroute.RouteConfiguration, queries []matchcheck.Query) ([]string, error)
}

func (f *fakeMatcher) Resolve(ctx context.Context, rc *envoyroute.RouteConfiguration, qs []matchcheck.Query) ([]string, error) {
	f.execs.Add(1)
	f.queries.Add(int64(len(qs)))
	f.mu.Lock()
	f.inflight++
	f.peak = max(f.peak, f.inflight)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
	}()
	if err := sleepCtx(ctx, f.latency); err != nil {
		return nil, err
	}
	if f.hook != nil {
		return f.hook(rc, qs)
	}
	out := make([]string, len(qs))
	for i, q := range qs {
		if f.miss[q.Host] {
			continue
		}
		label, _, _ := strings.Cut(q.Host, ".")
		out[i] = "outbound|8080||" + label + ".shop.svc.cluster.local"
	}
	return out, nil
}

func (f *fakeMatcher) peakInflight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// sharedIngressSnapshot is one ingress cluster's state: an LB Service carrying
// sharedIP, its Deployment, and one Gateway serving *.example.com on :443
// (TLS-terminated) and :80 (plain HTTP), with one bound VirtualService. Every
// "<label>.example.com" host resolves through the real snapshot / gwresolve /
// ListenerFor stages to the gateway's :443 or :80 RouteConfiguration.
func sharedIngressSnapshot(tb testing.TB, at time.Time) store.TrafficSnapshot {
	tb.Helper()
	gwSpec, err := protojson.Marshal(&networking.Gateway{
		Selector: map[string]string{"istio": "ingress"},
		Servers: []*networking.Server{
			{
				Port:  &networking.Port{Number: 443, Name: "https", Protocol: "HTTPS"},
				Hosts: []string{"*.example.com"},
				Tls:   &networking.ServerTLSSettings{Mode: networking.ServerTLSSettings_SIMPLE, CredentialName: "cred"},
			},
			{
				Port:  &networking.Port{Number: 80, Name: "http", Protocol: "HTTP"},
				Hosts: []string{"*.example.com"},
			},
		},
	})
	require.NoError(tb, err)
	vsSpec, err := protojson.Marshal(&networking.VirtualService{
		Hosts:    []string{"*.example.com"},
		Gateways: []string{"istio-system/public-gw"},
		Http: []*networking.HTTPRoute{{
			Route: []*networking.HTTPRouteDestination{{
				Destination: &networking.Destination{Host: "backend.shop.svc.cluster.local"},
			}},
		}},
	})
	require.NoError(tb, err)

	from, to := at.Add(-time.Hour), at.Add(time.Hour)
	return store.TrafficSnapshot{
		Services: []store.ServiceRow{
			{
				Cluster: sharedCluster, Namespace: "istio-system", Name: "ingress-lb",
				ValidFrom: from, ValidTo: to,
				LoadBalancerIPs: []string{sharedIP}, Selector: []string{"istio=ingress"},
			},
			{
				Cluster: sharedCluster, Namespace: "shop", Name: "backend",
				ValidFrom: from, ValidTo: to,
				Ports: []store.SvcPort{{Name: "http", Port: 8080, Protocol: "TCP"}},
			},
		},
		Deploys: []store.DeployRow{{
			Cluster: sharedCluster, Namespace: "istio-system", Name: "ingress",
			ValidFrom: from, ValidTo: to, PodLabels: []string{"istio=ingress"},
		}},
		Gateways: []store.GatewayRow{{
			Cluster: sharedCluster, Namespace: "istio-system", Name: "public-gw",
			ValidFrom: from, ValidTo: to,
			SelectorKV: []string{"istio=ingress"}, ServerHosts: []string{"*.example.com"},
			SpecJSON: string(gwSpec),
		}},
		VSes: []store.VSRow{{
			Cluster: sharedCluster, Namespace: "istio-system", Name: "vs-api",
			ValidFrom: from, ValidTo: to,
			BoundGateways: []string{"istio-system/public-gw"}, SpecJSON: string(vsSpec),
		}},
	}
}

// sharedIngressRequests is n distinct keys behind the one shared ingress IP —
// the shape C8/C9 target: many FQDNs, one gateway, one RouteConfiguration.
func sharedIngressRequests(n int, at time.Time) []build.RouteRequest {
	reqs := make([]build.RouteRequest, n)
	for i := range reqs {
		reqs[i] = build.RouteRequest{
			CallerCluster: sharedCluster,
			Host:          fmt.Sprintf("h%02d.example.com", i),
			Path:          "/",
			Port:          443,
			IPs:           []string{sharedIP},
			At:            at,
		}
	}
	return reqs
}

// routeAnswer is one request's result, comparable across resolution paths.
type routeAnswer struct {
	Dest    build.RouteDestination
	Outcome build.RouteOutcome
	Err     string
}

// prescanConcurrency mirrors pkg/build's routeResolveConcurrency.
const prescanConcurrency = 4

// resolveBuild drives one build's keys through r exactly as pkg/build's
// resolveRouteQueries does: one BuildScoped scope per build, keys resolved
// under bounded concurrency.
func resolveBuild(ctx context.Context, r build.RouteResolver, reqs []build.RouteRequest) []routeAnswer {
	if s, ok := r.(build.BuildScopedRouteResolver); ok {
		r = s.BuildScoped()
	}
	out := make([]routeAnswer, len(reqs))
	var g errgroup.Group
	g.SetLimit(prescanConcurrency)
	for i, req := range reqs {
		g.Go(func() error {
			dest, outcome, err := r.ResolveRoute(ctx, req)
			out[i] = routeAnswer{Dest: dest, Outcome: outcome}
			if err != nil {
				out[i].Err = err.Error()
			}
			return nil
		})
	}
	_ = g.Wait()
	return out
}
