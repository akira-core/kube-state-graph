package route

import (
	"context"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/build"
)

// BuildScoped implements build.BuildScopedRouteResolver: it returns a resolver
// scoped to one build that memoises the ingress-IP probe. Within a build the
// probe (store.ClustersWithIngressIP) is a pure function of (ip, at) — `at` is
// constant across the build's keys — so keys sharing a destination IP would
// otherwise repeat the same cross-cluster store read.
//
// The scope is used serially by the prescan loop, so no locking is needed. It
// lives for one build and is then discarded; the memo never outlives it, so
// there is no unbounded growth (a cache on the shared *Resolver would leak).
func (r *Resolver) BuildScoped() build.RouteResolver {
	return &scopedResolver{r: r, probes: map[probeKey][]string{}}
}

// probeKey identifies one ingress-IP probe. `at` is UnixMilli because a
// time.Time (with its monotonic-clock reading and *Location pointer) is not a
// sound map key; the store reads at millisecond precision (dt64Lit) so this
// loses nothing.
type probeKey struct {
	ip string
	at int64
}

// scopedResolver is a per-build wrapper around *Resolver that memoises the
// ingress-IP probe. NOT safe for concurrent use — one build, serial calls.
type scopedResolver struct {
	r      *Resolver
	probes map[probeKey][]string
}

var _ build.RouteResolver = (*scopedResolver)(nil)

func (s *scopedResolver) ResolveRoute(ctx context.Context, req build.RouteRequest) (build.RouteDestination, build.RouteOutcome, error) {
	return s.r.resolve(ctx, req, s.probe)
}

// probe is the memoising ingressIPProbe. A successful result (including a nil
// "no ingress cluster" slice) is cached; errors are NOT cached, so a later key
// sharing the IP retries — matching the base resolver's per-call behaviour.
func (s *scopedResolver) probe(ctx context.Context, ip string, at time.Time) ([]string, error) {
	k := probeKey{ip: ip, at: at.UnixMilli()}
	if cands, ok := s.probes[k]; ok {
		return cands, nil
	}
	cands, err := s.r.st.ClustersWithIngressIP(ctx, ip, at)
	if err != nil {
		return nil, err
	}
	s.probes[k] = cands
	return cands, nil
}
