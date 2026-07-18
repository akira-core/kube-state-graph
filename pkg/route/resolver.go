// Package route is the concrete build.RouteResolver: the Istio route-resolution
// engine that answers "which Kubernetes Service did the ingress config route
// (host, path, port) to during [start, end]?" against a versioned config
// store written by the metadata-exporter.
//
// Pipeline (ported from poc/route2a/internal/rangequery, extended by design
// D10): probe the store per destination IP for the clusters serving it
// (store.ClustersWithIngressIP — the one cross-cluster read) → select the
// ingress cluster C (pickIngressCluster: family-first, caller tie-break; any
// unresolvable case degrades, never guesses) → load C's window once
// (store.LoadTrafficWindow per IP, unioned within C only) → slice it at every
// version boundary (memwindow) → per segment, narrow candidate gateways (IP
// 3-hop) and disambiguate the host (gwresolve) → per DISTINCT config,
// translate to a RouteConfiguration (in-process istiod, translate) and match
// with Envoy's router_check_tool (matchcheck). Identical-content version
// bumps share one translate+check via the memwindow.ConfigSigAt signature
// cache. Destination IPs are REQUIRED (design D6): an IP-less request misses
// with RouteNoIngress before touching the store.
//
// pkg/build MUST NOT import this package (design D1); only cmd/kube-state-graph
// or an opting-in embedder constructs a Resolver and injects it via
// build.Options.RouteResolver. Nothing here constructs a Kubernetes client,
// informer, watch, or kubeconfig (design D0) — istio is a pure library.
package route

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/gwresolve"
	"github.com/akira-core/kube-state-graph/pkg/route/matchcheck"
	"github.com/akira-core/kube-state-graph/pkg/route/memwindow"
	"github.com/akira-core/kube-state-graph/pkg/route/store"
	"github.com/akira-core/kube-state-graph/pkg/route/translate"
)

// Resolver folds store → memwindow → gwresolve → translate → matchcheck into
// the build.RouteResolver contract. Safe for concurrent use: the store is a
// connection pool, the Translator is stateless, and every ResolveRoute call
// builds its own throwaway window.
type Resolver struct {
	st  store.Store
	tr  *translate.Translator
	run matchcheck.Runner
}

var _ build.RouteResolver = (*Resolver)(nil)

// NewResolver wires a Resolver over an opened store and a verified
// router_check_tool runner.
func NewResolver(st store.Store, run matchcheck.Runner) *Resolver {
	return &Resolver{st: st, tr: translate.NewTranslator(), run: run}
}

// outcomeRank orders miss outcomes by pipeline depth so a multi-segment window
// reports the most informative miss: a segment that reached route matching
// (no_route) beats one whose port had servers but none serving the host
// (no_server_for_host), which beats one that stopped at the listener port
// (no_listener_on_port), which beats one where no gateway served the host at
// all.
func outcomeRank(o build.RouteOutcome) int {
	switch o {
	case build.RouteNoRoute:
		return 3
	case build.RouteNoServerForHost:
		return 2
	case build.RouteNoListenerOnPort:
		return 1
	default: // RouteNoGateway
		return 0
	}
}

// segmentResult caches one resolved (config signature → outcome) so segments
// with byte-identical config share a single translate + router_check_tool run.
type segmentResult struct {
	dest    build.RouteDestination
	outcome build.RouteOutcome
}

// ResolveRoute implements build.RouteResolver. The window is loaded once; each
// version segment resolves in memory; every DISTINCT config pays one translate
// + one router_check_tool invocation. When the config changed inside the
// window and more than one segment hits, the LATEST hit wins — the destination
// closest to the request's end time. Misses fold to the deepest pipeline stage
// any segment reached (see outcomeRank).
func (r *Resolver) ResolveRoute(ctx context.Context, req build.RouteRequest) (build.RouteDestination, build.RouteOutcome, error) {
	return r.resolve(ctx, req, r.st.ClustersWithIngressIP)
}

// ingressIPProbe answers "which clusters had an ingress Service carrying ip
// overlapping [t0,t1)?" — the shape of store.ClustersWithIngressIP. resolve
// takes it as a seam so a per-build scope (scopedResolver) can memoise the
// probe without duplicating the pipeline.
type ingressIPProbe func(ctx context.Context, ip string, t0, t1 time.Time) ([]string, error)

// resolve is ResolveRoute's body with the ingress-IP probe injected. The base
// resolver passes r.st.ClustersWithIngressIP directly; the per-build scope
// passes a memoising wrapper. Everything downstream is identical.
func (r *Resolver) resolve(ctx context.Context, req build.RouteRequest, probe ingressIPProbe) (build.RouteDestination, build.RouteOutcome, error) {
	if len(req.IPs) == 0 {
		// Defensive: the prescan never emits an IP-less request (design D6) —
		// without a destination IP no ingress cluster can be selected.
		return build.RouteDestination{}, build.RouteNoIngress, nil
	}

	// Design D10: select the ingress cluster from the destination IPs before
	// touching any window. One probe per IP; the candidate sets stay separate
	// so multi-IP answers must agree — never unioned across clusters.
	perIP := make([][]string, len(req.IPs))
	for i, ip := range req.IPs {
		cands, err := probe(ctx, ip, req.Start, req.End)
		if err != nil {
			return build.RouteDestination{}, build.RouteNoGateway, err
		}
		perIP[i] = cands
	}
	cluster, pickMiss, ok := pickIngressCluster(req.CallerCluster, perIP)
	if !ok {
		return build.RouteDestination{}, pickMiss, nil
	}

	w, err := r.loadWindow(ctx, cluster, req)
	if err != nil {
		return build.RouteDestination{}, build.RouteNoGateway, err
	}
	mw := memwindow.New(w, req.Start, req.End)

	// Per-request signature cache: identical-content version bumps (the common
	// case) collapse to one translate+check. Port, host, and path are constant
	// within a request, so the config signature alone is a sound key.
	cache := map[uint64]segmentResult{}

	var (
		hit     bool
		hitDest build.RouteDestination
		miss    = build.RouteNoGateway
	)
	noteMiss := func(o build.RouteOutcome) {
		if outcomeRank(o) > outcomeRank(miss) {
			miss = o
		}
	}

	for _, seg := range mw.Segments() {
		t := seg.From

		cands := r.candidatesAt(mw, req, t)
		gw, ok := gwresolve.New(candsToGateways(cands)).ResolveAmong(req.Host, candNames(cands))
		if !ok {
			continue // no gateway serves the host in this segment
		}

		sig, sok := mw.ConfigSigAt(gw, t)
		if !sok {
			continue // gateway named by the 3-hop but no live config
		}
		res, cached := cache[sig]
		if !cached {
			res, err = r.resolveConfig(ctx, mw, gw, req, t)
			if err != nil {
				return build.RouteDestination{}, build.RouteNoGateway, err
			}
			cache[sig] = res
		}

		if res.outcome == build.RouteHit {
			// Latest hit wins: segments iterate in ascending time order.
			hit, hitDest = true, res.dest
		} else {
			noteMiss(res.outcome)
		}
	}

	if hit {
		// The destination lives in the selected ingress cluster (design D11):
		// ParseEnvoyCluster cannot know it — the Envoy cluster string carries
		// only (port, subset, service, namespace) — so it is stamped here.
		hitDest.Cluster = cluster
		return hitDest, build.RouteHit, nil
	}
	return build.RouteDestination{}, miss, nil
}

// loadWindow loads the selected ingress cluster's window: one
// store.LoadTrafficWindow per destination IP, rows unioned — multi-A-record
// DNS answers are a union of candidates WITHIN the one selected cluster
// (design §9.2 / D10; cross-cluster unions are forbidden).
func (r *Resolver) loadWindow(ctx context.Context, cluster string, req build.RouteRequest) (store.TrafficWindow, error) {
	var out store.TrafficWindow
	for _, ip := range req.IPs {
		w, err := r.st.LoadTrafficWindow(ctx, cluster, ip, req.Start, req.End)
		if err != nil {
			return store.TrafficWindow{}, err
		}
		out.Services = append(out.Services, w.Services...)
		out.Deploys = append(out.Deploys, w.Deploys...)
		out.Gateways = append(out.Gateways, w.Gateways...)
		out.VSes = append(out.VSes, w.VSes...)
	}
	return out, nil
}

// candidatesAt returns the gateway candidates for one segment: the union of
// the per-IP 3-hop results within the loaded (single-cluster) window.
func (r *Resolver) candidatesAt(mw *memwindow.Window, req build.RouteRequest, t time.Time) []store.GatewayCand {
	var cands []store.GatewayCand
	seen := map[string]bool{}
	for _, ip := range req.IPs {
		for _, c := range mw.ResolveIPToGateways(ip, t) {
			key := c.Namespace + "/" + c.Name
			if !seen[key] {
				seen[key] = true
				cands = append(cands, c)
			}
		}
	}
	return cands
}

// resolveConfig runs the expensive stages for one gateway's config as-of t:
// in-memory ScopedFor → listener check → istiod translate → router_check_tool
// → Envoy cluster-string parse.
func (r *Resolver) resolveConfig(ctx context.Context, mw *memwindow.Window, gw string, req build.RouteRequest, t time.Time) (segmentResult, error) {
	scoped, found, err := mw.ScopedFor(gw, t)
	if err != nil {
		return segmentResult{}, err
	}
	if !found {
		return segmentResult{outcome: build.RouteNoGateway}, nil
	}
	scoped.Port = req.Port
	// The host decides WHICH server on the port owns the RouteConfiguration
	// (each TLS-terminated HTTPS server carries its own).
	scoped.Host = req.Host

	// Listener gate: both misses are decided from config alone — no translate
	// round-trip, no router_check_tool exec. A port with no routable HTTP
	// listener is the D5 mis-guessed-port signature; a port whose servers all
	// serve OTHER hosts is the distinct no-server-for-host miss (translating
	// anyway could only reach RouteNoRoute — istiod builds vhosts from the
	// server-hosts ∩ VS-hosts intersection).
	switch _, st := translate.ListenerFor(scoped); st {
	case translate.ListenerNoneOnPort:
		return segmentResult{outcome: build.RouteNoListenerOnPort}, nil
	case translate.ListenerNoServerForHost:
		return segmentResult{outcome: build.RouteNoServerForHost}, nil
	}

	rc, err := r.tr.Translate(scoped)
	if err != nil {
		return segmentResult{}, err
	}
	clusters, err := r.run.Resolve(ctx, rc, []matchcheck.Query{{Host: req.Host, Path: req.Path}})
	if err != nil {
		return segmentResult{}, err
	}
	dest, ok := ParseEnvoyCluster(clusters[0])
	if !ok {
		// "" (no route matched) or a non-Service cluster string — both a miss.
		return segmentResult{outcome: build.RouteNoRoute}, nil
	}
	return segmentResult{dest: dest, outcome: build.RouteHit}, nil
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
