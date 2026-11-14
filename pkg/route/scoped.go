package route

import (
	"context"
	"time"

	envoyroute "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/store"
	"github.com/akira-core/kube-state-graph/pkg/route/translate"
)

// BuildScoped implements build.BuildScopedRouteResolver: it returns a resolver
// scoped to one build that memoises the work every key of the build would
// otherwise repeat. Within a build `at` is constant, so three stages are pure
// functions of inputs many keys share:
//
//   - the ingress probe (store.ClustersWithIngressIP) of (ip, at);
//   - the configuration load (store.LoadTrafficAt) of (cluster, ip, at) —
//     every FQDN behind one ingress IP loads the same rows;
//   - the istiod translation of (cluster, IP list, gateway, RouteConfiguration
//     name) — every FQDN served by one gateway listener translates the same
//     RouteConfiguration.
//
// The scope lives for one build and is then discarded, so its memos never
// outlive the build. The prescan resolves keys concurrently, so every memo is
// a flightMemo: concurrent callers share one execution, and a failure is never
// remembered or handed to another caller.
func (r *Resolver) BuildScoped() build.RouteResolver {
	return r.newScope()
}

func (r *Resolver) newScope() *scopedResolver {
	return &scopedResolver{r: r}
}

// probeKey identifies one ingress-IP probe. `at` is UnixMilli because a
// time.Time (with its monotonic-clock reading and *Location pointer) is not a
// sound map key; the store reads at millisecond precision (dt64Lit) so this
// loses nothing.
type probeKey struct {
	ip string
	at int64
}

// loadKey identifies one per-IP configuration load in the selected cluster.
type loadKey struct {
	cluster, ip string
	at          int64
}

// rcKey identifies one translated RouteConfiguration within a build. The IP
// list (in request order — the order the per-IP rows are unioned in, which is
// the order istiod receives the configs in) fixes the snapshot, the gateway
// identity fixes ScopedFor's configs, registry and proxy, and the
// RouteConfiguration name (translate.ListenerFor) is the only thing Translate
// reads from the request's port and host. `at` is constant per build.
type rcKey struct {
	cluster, ips string
	gwNS, gwName string
	rcName       string
}

// scopedResolver is a per-build wrapper around *Resolver that memoises the
// probe, the configuration load and the translation. Safe for concurrent use
// within its one build.
type scopedResolver struct {
	r *Resolver

	probes flightMemo[probeKey, []string]
	loads  flightMemo[loadKey, store.TrafficSnapshot]
	rcs    flightMemo[rcKey, *envoyroute.RouteConfiguration]
}

var _ build.RouteResolver = (*scopedResolver)(nil)

// ResolveRoute answers from the cross-build cache, joins a resolution of the
// same key already running in any build, or resolves through this scope's
// memos and caches a success.
func (s *scopedResolver) ResolveRoute(ctx context.Context, req build.RouteRequest) (build.RouteDestination, build.RouteOutcome, error) {
	res, err := s.r.results.do(ctx, resultKeyOf(req), func(ctx context.Context) (routeResult, error) {
		dest, outcome, err := s.resolve(ctx, req)
		return routeResult{dest: dest, outcome: outcome}, err
	})
	if err != nil {
		return build.RouteDestination{}, "", err
	}
	return res.dest, res.outcome, nil
}

// probe is the memoised ingress-IP probe. A successful result (including a nil
// "no ingress cluster" slice) is kept for the build; an error is not, so a
// later key sharing the IP retries.
func (s *scopedResolver) probe(ctx context.Context, ip string, at time.Time) ([]string, error) {
	return s.probes.do(ctx, probeKey{ip: ip, at: at.UnixMilli()}, func(ctx context.Context) ([]string, error) {
		return s.r.st.ClustersWithIngressIP(ctx, ip, at)
	})
}

// loadTraffic is the memoised per-IP configuration load. The returned rows are
// shared by every key of the build that selects the same cluster through the
// same IP, and are read-only.
func (s *scopedResolver) loadTraffic(ctx context.Context, cluster, ip string, at time.Time) (store.TrafficSnapshot, error) {
	return s.loads.do(ctx, loadKey{cluster: cluster, ip: ip, at: at.UnixMilli()}, func(ctx context.Context) (store.TrafficSnapshot, error) {
		return s.r.st.LoadTrafficAt(ctx, cluster, ip, at)
	})
}

// translate is the memoised istiod translation. The RouteConfiguration is
// shared by every key with the same rcKey and is read-only (the matcher only
// marshals it).
func (s *scopedResolver) translate(ctx context.Context, k rcKey, in translate.ScopedInput) (*envoyroute.RouteConfiguration, error) {
	return s.rcs.do(ctx, k, func(ctx context.Context) (*envoyroute.RouteConfiguration, error) {
		return s.r.translate(ctx, in) // the engine slot is taken by the leader only
	})
}
