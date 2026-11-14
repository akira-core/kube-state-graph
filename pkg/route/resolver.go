// Package route is the concrete build.RouteResolver: the Istio route-resolution
// engine that answers "which Kubernetes Service did the ingress config route
// (host, path, port) to at instant `at`?" against a versioned config store
// written by the metadata-exporter.
//
// Pipeline (ported from poc/route2a/internal/rangequery, extended with
// ingress-cluster selection and simplified to a single instant): probe the
// store per
// destination IP for the clusters serving it (store.ClustersWithIngressIP —
// the one cross-cluster read) → select the ingress cluster C
// (pickIngressCluster: family-first, caller tie-break; any unresolvable case
// degrades, never guesses) → load C's snapshot at `at` (store.LoadTrafficAt per
// IP, unioned within C only) → narrow candidate gateways (IP 3-hop) and
// disambiguate the host (gwresolve) → translate that one config to a
// RouteConfiguration (in-process istiod, translate) and match with Envoy's
// router_check_tool (matchcheck). Exactly one configuration is evaluated, so
// exactly one outcome is produced — there is no segment loop and no
// config-signature dedup. Destination IPs are REQUIRED: an IP-less
// request misses with RouteNoIngress before touching the store.
//
// pkg/build MUST NOT import this package; only cmd/kube-state-graph
// or an opting-in embedder constructs a Resolver and injects it via
// build.Options.RouteResolver. Nothing here constructs a Kubernetes client,
// informer, watch, or kubeconfig — istio is a pure library.
package route

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	envoyroute "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/gwresolve"
	"github.com/akira-core/kube-state-graph/pkg/route/matchcheck"
	"github.com/akira-core/kube-state-graph/pkg/route/store"
	"github.com/akira-core/kube-state-graph/pkg/route/translate"
)

// Resolver folds store → snapshot → gwresolve → translate → matchcheck into
// the build.RouteResolver contract. Safe for concurrent use: the store is a
// connection pool, the Translator is stateless, every ResolveRoute call
// builds its own throwaway snapshot, and the only process-wide state — the
// engine limit — is a channel.
type Resolver struct {
	st  store.Store
	tr  translator
	run matcher

	// engine is the process-wide limit on CPU-heavy engine work: every istiod
	// translation and every router_check_tool invocation, across every
	// concurrent build, holds one slot for its duration. The per-build
	// routeResolveConcurrency bounds one build; without this, R concurrent
	// builds ran up to 4R istiod worlds and 4R child processes.
	engine chan struct{}
}

// errEngineSaturated is returned when a caller's context ended before an
// engine slot freed up. It carries no outcome, so the key degrades to an
// external node (route_engine_error), is never cached, and never opens the
// RouteNoGateway-gated LB fallback.
var errEngineSaturated = errors.New("route engine saturated")

// defaultEngineSlots sizes the engine limit: translation is CPU-bound and
// router_check_tool is a CPU-bound child process, so more than one per usable
// CPU only queues inside the scheduler instead of here.
func defaultEngineSlots() int { return max(1, runtime.GOMAXPROCS(0)) }

// acquireEngine takes an engine slot, waiting at most until ctx ends. The
// caller MUST call the returned release exactly once. It is only ever called by
// the caller doing the work — never while waiting on another caller's memo or
// cache flight — so waiting for a slot cannot deadlock against the limit.
func (r *Resolver) acquireEngine(ctx context.Context) (release func(), err error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w: %w", errEngineSaturated, context.Cause(ctx))
	}
	select {
	case r.engine <- struct{}{}:
		return func() { <-r.engine }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", errEngineSaturated, context.Cause(ctx))
	}
}

var _ build.RouteResolver = (*Resolver)(nil)

// translator is the istiod translation stage: one gateway's scoped config in,
// the RouteConfiguration istiod would push out. *translate.Translator is the
// production implementation; the seam exists so the resolver's orchestration
// (memoisation, batching, the engine limit) can be exercised without building
// an istiod world per call.
type translator interface {
	Translate(ctx context.Context, in translate.ScopedInput) (*envoyroute.RouteConfiguration, error)
}

// matcher is the router_check_tool stage: one RouteConfiguration plus a batch
// of host+path queries in, one Envoy cluster string per query out ("" = no
// route). matchcheck.Runner is the production implementation.
type matcher interface {
	Resolve(ctx context.Context, rc *envoyroute.RouteConfiguration, queries []matchcheck.Query) ([]string, error)
}

// NewResolver wires a Resolver over an opened store and a verified
// router_check_tool runner.
func NewResolver(st store.Store, run matchcheck.Runner) *Resolver {
	return newResolver(st, translate.NewTranslator(), run)
}

// newResolver is NewResolver over any translator and matcher.
func newResolver(st store.Store, tr translator, run matcher) *Resolver {
	return &Resolver{st: st, tr: tr, run: run, engine: make(chan struct{}, defaultEngineSlots())}
}

// ResolveRoute implements build.RouteResolver. The configuration is evaluated
// at req.At — one snapshot load, one gateway pick, at most one translate + one
// router_check_tool invocation, one outcome. A standalone call runs through a
// throwaway scope; a build drives all its keys through one BuildScoped scope.
func (r *Resolver) ResolveRoute(ctx context.Context, req build.RouteRequest) (build.RouteDestination, build.RouteOutcome, error) {
	return r.newScope().resolve(ctx, req)
}

// match runs one router_check_tool invocation over rc for queries, holding an
// engine slot for its duration.
func (r *Resolver) match(ctx context.Context, rc *envoyroute.RouteConfiguration, queries []matchcheck.Query) ([]string, error) {
	release, err := r.acquireEngine(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return r.run.Resolve(ctx, rc, queries)
}

// translate runs one istiod translation, holding an engine slot for its
// duration.
func (r *Resolver) translate(ctx context.Context, in translate.ScopedInput) (*envoyroute.RouteConfiguration, error) {
	release, err := r.acquireEngine(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return r.tr.Translate(ctx, in)
}

// ParseEnvoyCluster parses an istiod outbound cluster string —
// "outbound|<port>|<subset>|<svc>.<ns>.svc.cluster.local" — into the
// destination Service it names. ok=false for an empty string (route miss), a
// non-outbound direction, an unparseable port, or a host that is not an
// in-cluster Service FQDN.
func ParseEnvoyCluster(cluster string) (build.RouteDestination, bool) {
	parts := strings.Split(cluster, "|")
	if len(parts) != 4 || parts[0] != "outbound" {
		return build.RouteDestination{}, false
	}
	port, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return build.RouteDestination{}, false
	}
	name, ns, ok := store.ParseBackendHost(parts[3])
	if !ok {
		return build.RouteDestination{}, false
	}
	return build.RouteDestination{
		Namespace: ns,
		Service:   name,
		Port:      uint32(port),
		Subset:    parts[2],
	}, true
}

// pickCandidate recovers the full candidate — namespace included — that
// gwresolve selected by name. gwresolve matches on host patterns and returns a
// bare name, which is unambiguous within one namespace (Kubernetes guarantees
// it) and therefore for any single-IP request, since hop 3 scopes candidates to
// the ingress namespace. ok=false only when a multi-IP request contributed
// same-named candidates from two namespaces.
func pickCandidate(cands []store.GatewayCand, name string) (store.GatewayCand, bool) {
	var found store.GatewayCand
	n := 0
	for _, c := range cands {
		if c.Name == name {
			found, n = c, n+1
		}
	}
	return found, n == 1
}

// candsToGateways / candNames adapt store.GatewayCand rows for gwresolve.
func candsToGateways(cands []store.GatewayCand) []gwresolve.Gateway {
	out := make([]gwresolve.Gateway, len(cands))
	for i, c := range cands {
		out[i] = gwresolve.Gateway{Name: c.Name, Hosts: c.ServerHosts}
	}
	return out
}

func candNames(cands []store.GatewayCand) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Name
	}
	return out
}
