// Package route is the concrete build.RouteResolver: the Istio route-resolution
// engine that answers "which Kubernetes Service did the ingress config route
// (host, path, port) to at instant `at`?" against a versioned config store
// written by the metadata-exporter.
//
// Pipeline (ported from poc/route2a/internal/rangequery, extended by design
// D10 and simplified to a single instant by
// simplify-route-resolution-to-point-in-time D1): probe the store per
// destination IP for the clusters serving it (store.ClustersWithIngressIP —
// the one cross-cluster read) → select the ingress cluster C
// (pickIngressCluster: family-first, caller tie-break; any unresolvable case
// degrades, never guesses) → load C's snapshot at `at` (store.LoadTrafficAt per
// IP, unioned within C only) → narrow candidate gateways (IP 3-hop) and
// disambiguate the host (gwresolve) → translate that one config to a
// RouteConfiguration (in-process istiod, translate) and match with Envoy's
// router_check_tool (matchcheck). Exactly one configuration is evaluated, so
// exactly one outcome is produced — there is no segment loop and no
// config-signature dedup. Destination IPs are REQUIRED (design D6): an IP-less
// request misses with RouteNoIngress before touching the store.
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
	"github.com/akira-core/kube-state-graph/pkg/route/snapshot"
	"github.com/akira-core/kube-state-graph/pkg/route/store"
	"github.com/akira-core/kube-state-graph/pkg/route/translate"
)

// Resolver folds store → snapshot → gwresolve → translate → matchcheck into
// the build.RouteResolver contract. Safe for concurrent use: the store is a
// connection pool, the Translator is stateless, and every ResolveRoute call
// builds its own throwaway snapshot.
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

// ResolveRoute implements build.RouteResolver. The configuration is evaluated
// at req.At — one snapshot load, one gateway pick, at most one translate + one
// router_check_tool invocation, one outcome.
func (r *Resolver) ResolveRoute(ctx context.Context, req build.RouteRequest) (build.RouteDestination, build.RouteOutcome, error) {
	return r.resolve(ctx, req, r.st.ClustersWithIngressIP)
}

// ingressIPProbe answers "which clusters had an ingress Service carrying ip
// live at `at`?" — the shape of store.ClustersWithIngressIP. resolve takes it
// as a seam so a per-build scope (scopedResolver) can memoise the probe without
// duplicating the pipeline.
type ingressIPProbe func(ctx context.Context, ip string, at time.Time) ([]string, error)

// resolve is ResolveRoute's body with the ingress-IP probe injected. The base
// resolver passes r.st.ClustersWithIngressIP directly; the per-build scope
// passes a memoising wrapper. Everything downstream is identical.
//
// When the returned error is non-nil the outcome is meaningless and MUST be the
// empty value: RouteNoGateway is the ingress-LB-fallback gate below, so
// returning it alongside an infrastructure failure would let a store outage read
// as "no Istio Gateway serves this host" (design D6).
func (r *Resolver) resolve(ctx context.Context, req build.RouteRequest, probe ingressIPProbe) (build.RouteDestination, build.RouteOutcome, error) {
	if len(req.IPs) == 0 {
		// Defensive: the prescan never emits an IP-less request (design D6) —
		// without a destination IP no ingress cluster can be selected.
		return build.RouteDestination{}, build.RouteNoIngress, nil
	}

	// Design D10: select the ingress cluster from the destination IPs before
	// touching any snapshot. One probe per IP; the candidate sets stay separate
	// so multi-IP answers must agree — never unioned across clusters.
	perIP := make([][]string, len(req.IPs))
	for i, ip := range req.IPs {
		cands, err := probe(ctx, ip, req.At)
		if err != nil {
			return build.RouteDestination{}, "", err
		}
		perIP[i] = cands
	}
	cluster, pickMiss, ok := pickIngressCluster(req.CallerCluster, perIP)
	if !ok {
		return build.RouteDestination{}, pickMiss, nil
	}

	rows, err := r.loadSnapshot(ctx, cluster, req)
	if err != nil {
		return build.RouteDestination{}, "", err
	}
	snap := snapshot.New(rows, req.At)

	dest, outcome, err := r.resolveConfig(ctx, snap, req)
	if err != nil {
		return build.RouteDestination{}, "", err
	}

	if outcome == build.RouteHit {
		// The destination lives in the selected ingress cluster (design D11):
		// ParseEnvoyCluster cannot know it — the Envoy cluster string carries
		// only (port, subset, service, namespace) — so it is stamped here.
		dest.Cluster = cluster
		// route-hit-ingress-chain D1: recover the ingress LB Service the
		// destination IPs uniquely map to in the already-loaded snapshot (same
		// identity dedup as the LB fallback, zero new store reads) so the parse
		// can emit the full caller → ingress → backend chain. Ambiguous or
		// incomplete identity leaves the fields empty — the chain degrades to
		// the direct edge; a hit is NEVER demoted.
		if id, status := ingressServiceIdentity(snap, req.IPs); status == identityUnique {
			dest.IngressNamespace, dest.IngressService = id.ns, id.name
		}
		return dest, build.RouteHit, nil
	}

	// Ingress LB Service fallback (ingress-lb-service-fallback change), gated
	// on the miss being RouteNoGateway: the nginx-ingress signature is Hop 3
	// finding no Istio Gateway CR, so resolution never got past gateway
	// selection — yet the loaded snapshot may still map the destination IPs to
	// exactly one ingress LB Service. A DEEPER miss (no_listener_on_port /
	// no_server_for_host / no_route) means an Istio Gateway DID serve the host
	// and its diagnostic reason must not be masked by an LB-entry-point edge.
	// A routed hit above always wins; a fallback that finds nothing keeps the
	// pipeline's own miss.
	if outcome == build.RouteNoGateway {
		if lbDest, out, ok := resolveIngressLBService(snap, req.IPs); ok {
			if out == build.RouteIngressLBService {
				lbDest.Cluster = cluster // same D11 stamp as a RouteHit
				return lbDest, out, nil
			}
			return build.RouteDestination{}, out, nil // ambiguous_ingress_service
		}
	}
	return build.RouteDestination{}, outcome, nil
}

// loadSnapshot loads the selected ingress cluster's configuration state at
// req.At: one store.LoadTrafficAt per destination IP, rows unioned —
// multi-A-record DNS answers are a union of candidates WITHIN the one selected
// cluster (design §9.2 / D10; cross-cluster unions are forbidden).
//
// The union is deduplicated by resource-version identity. Two destination IPs
// published by the SAME ingress Service (a dual-stack Service carrying an A and
// an AAAA address, or several addresses of one load balancer) make each per-IP
// load return the same gateway/VS rows, and the store's own dedup only spans a
// single call. Without this, ScopedFor would hand istiod the same
// VirtualService twice and its config store would reject it ("item already
// exists"), failing the whole resolution — see design D1.
func (r *Resolver) loadSnapshot(ctx context.Context, cluster string, req build.RouteRequest) (store.TrafficSnapshot, error) {
	var out store.TrafficSnapshot
	svcSeen, depSeen := map[versionKey]bool{}, map[versionKey]bool{}
	gwSeen, vsSeen := map[versionKey]bool{}, map[versionKey]bool{}
	for _, ip := range req.IPs {
		w, err := r.st.LoadTrafficAt(ctx, cluster, ip, req.At)
		if err != nil {
			return store.TrafficSnapshot{}, err
		}
		for _, row := range w.Services {
			appendUnseen(&out.Services, svcSeen, keyOf(row.Cluster, row.Namespace, row.Name, row.ValidFrom), row)
		}
		for _, row := range w.Deploys {
			appendUnseen(&out.Deploys, depSeen, keyOf(row.Cluster, row.Namespace, row.Name, row.ValidFrom), row)
		}
		for _, row := range w.Gateways {
			appendUnseen(&out.Gateways, gwSeen, keyOf(row.Cluster, row.Namespace, row.Name, row.ValidFrom), row)
		}
		for _, row := range w.VSes {
			appendUnseen(&out.VSes, vsSeen, keyOf(row.Cluster, row.Namespace, row.Name, row.ValidFrom), row)
		}
	}
	return out, nil
}

// versionKey identifies ONE resource version across per-IP loads. ValidFrom is
// part of the identity so two genuine versions of one resource both survive;
// IngestSeq deliberately is NOT — a rewrite twin and its closing row share a
// version slot and the store collapses them before the union sees either.
// ValidFrom is stored as UnixNano because a time.Time (monotonic reading +
// *Location pointer) is not a sound map key.
type versionKey struct {
	cluster, namespace, name string
	validFrom                int64
}

func keyOf(cluster, namespace, name string, validFrom time.Time) versionKey {
	return versionKey{cluster: cluster, namespace: namespace, name: name, validFrom: validFrom.UnixNano()}
}

// appendUnseen appends row to dst the first time its key is seen.
func appendUnseen[T any](dst *[]T, seen map[versionKey]bool, k versionKey, row T) {
	if seen[k] {
		return
	}
	seen[k] = true
	*dst = append(*dst, row)
}

// candidates returns the gateway candidates for the request: the union of the
// per-IP 3-hop results within the loaded (single-cluster) snapshot.
func candidates(snap *snapshot.Snapshot, ips []string) []store.GatewayCand {
	var cands []store.GatewayCand
	seen := map[string]bool{}
	for _, ip := range ips {
		for _, c := range snap.ResolveIPToGateways(ip) {
			key := c.Namespace + "/" + c.Name
			if !seen[key] {
				seen[key] = true
				cands = append(cands, c)
			}
		}
	}
	return cands
}

// resolveConfig runs the whole in-memory + expensive chain for the request:
// 3-hop candidates → host disambiguation → in-memory ScopedFor → listener check
// → istiod translate → router_check_tool → Envoy cluster-string parse.
func (r *Resolver) resolveConfig(ctx context.Context, snap *snapshot.Snapshot, req build.RouteRequest) (build.RouteDestination, build.RouteOutcome, error) {
	cands := candidates(snap, req.IPs)
	gwName, ok := gwresolve.New(candsToGateways(cands)).ResolveAmong(req.Host, candNames(cands))
	if !ok {
		return build.RouteDestination{}, build.RouteNoGateway, nil // no gateway serves the host
	}
	gw, ok := pickCandidate(cands, gwName)
	if !ok {
		// Two candidates share a bare name across namespaces — only reachable
		// when a multi-IP request's per-IP hop 1 landed in different namespaces.
		// Neither is more correct than the other, so degrade instead of guessing
		// (design D3); the caller's LB fallback still applies.
		return build.RouteDestination{}, build.RouteNoGateway, nil
	}

	scoped, found, err := snap.ScopedFor(gw.Namespace, gw.Name)
	if err != nil {
		return build.RouteDestination{}, "", err
	}
	if !found {
		// Named by the 3-hop but no live config at the instant.
		return build.RouteDestination{}, build.RouteNoGateway, nil
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
		return build.RouteDestination{}, build.RouteNoListenerOnPort, nil
	case translate.ListenerNoServerForHost:
		return build.RouteDestination{}, build.RouteNoServerForHost, nil
	case translate.ListenerFound:
		// fall through to the translate + router_check_tool stages
	}

	rc, err := r.tr.Translate(scoped)
	if err != nil {
		return build.RouteDestination{}, "", err
	}
	clusters, err := r.run.Resolve(ctx, rc, []matchcheck.Query{{Host: req.Host, Path: req.Path}})
	if err != nil {
		return build.RouteDestination{}, "", err
	}
	dest, ok := ParseEnvoyCluster(clusters[0])
	if !ok {
		// "" (no route matched) or a non-Service cluster string — both a miss.
		return build.RouteDestination{}, build.RouteNoRoute, nil
	}
	return dest, build.RouteHit, nil
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
