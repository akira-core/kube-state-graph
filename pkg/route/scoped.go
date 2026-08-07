package route

import (
	"context"
	"sync"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/build"
)

// BuildScoped implements build.BuildScopedRouteResolver: it returns a resolver
// scoped to one build that memoises the ingress-IP probe. Within a build the
// probe (store.ClustersWithIngressIP) is a pure function of (ip, at) — `at` is
// constant across the build's keys — so keys sharing a destination IP would
// otherwise repeat the same cross-cluster store read.
//
// The scope lives for one build and is then discarded; the memo never outlives
// it, so there is no unbounded growth (a cache on the shared *Resolver would
// leak). The prescan resolves keys with bounded concurrency, so the memo is
// mutex-guarded.
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
// ingress-IP probe. Safe for concurrent use within its one build.
type scopedResolver struct {
	r *Resolver

	mu     sync.Mutex
	probes map[probeKey][]string
}

var _ build.RouteResolver = (*scopedResolver)(nil)

func (s *scopedResolver) ResolveRoute(ctx context.Context, req build.RouteRequest) (build.RouteDestination, build.RouteOutcome, error) {
	return s.r.resolve(ctx, req, s.probe)
}

// probe is the memoising ingressIPProbe. A successful result (including a nil
// "no ingress cluster" slice) is cached; errors are NOT cached, so a later key
// sharing the IP retries — matching the base resolver's per-call behaviour.
//
// The store read runs OUTSIDE the lock, so two keys racing on the same IP may
// both issue it. That is accepted rather than serialised behind a singleflight:
// the probe is idempotent and read-only, the race window is one build, and
// holding the lock across a network round-trip would serialise the very
// concurrency this exists alongside.
func (s *scopedResolver) probe(ctx context.Context, ip string, at time.Time) ([]string, error) {
	k := probeKey{ip: ip, at: at.UnixMilli()}
	s.mu.Lock()
	cands, hit := s.probes[k]
	s.mu.Unlock()
	if hit {
		return cands, nil
	}
	cands, err := s.r.st.ClustersWithIngressIP(ctx, ip, at)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.probes[k] = cands
	s.mu.Unlock()
	return cands, nil
}
