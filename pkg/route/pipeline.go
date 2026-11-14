package route

import (
	"context"
	"strings"
	"time"

	envoyroute "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/gwresolve"
	"github.com/akira-core/kube-state-graph/pkg/route/matchcheck"
	"github.com/akira-core/kube-state-graph/pkg/route/snapshot"
	"github.com/akira-core/kube-state-graph/pkg/route/store"
	"github.com/akira-core/kube-state-graph/pkg/route/translate"
)

// prepared is one request resolved up to the matcher. Either check is nil and
// (dest, outcome) is the final answer — every miss, and the ingress LB
// fallback, is decided before the matcher — or check names the one
// router_check_tool query still to run.
type prepared struct {
	dest    build.RouteDestination
	outcome build.RouteOutcome
	check   *pendingCheck
}

// pendingCheck is a request that reached the matcher: its translated
// RouteConfiguration (shared, read-only), the RouteConfiguration's memo key
// (so a batch can group requests on it), and what finish needs to turn the
// matched Envoy cluster string into the answer.
type pendingCheck struct {
	rcKey   rcKey
	rc      *envoyroute.RouteConfiguration
	query   matchcheck.Query
	cluster string
	snap    *snapshot.Snapshot
	ips     []string
}

// resolve is one request through the whole pipeline: prepare, then the
// single-query matcher run.
//
// When the returned error is non-nil the outcome is meaningless and MUST be the
// empty value: RouteNoGateway is the ingress-LB-fallback gate, so returning it
// alongside an infrastructure failure would let a store outage read as "no
// Istio Gateway serves this host".
func (s *scopedResolver) resolve(ctx context.Context, req build.RouteRequest) (build.RouteDestination, build.RouteOutcome, error) {
	p, err := s.prepare(ctx, req)
	if err != nil {
		return build.RouteDestination{}, "", err
	}
	if p.check == nil {
		return p.dest, p.outcome, nil
	}
	clusters, err := s.r.match(ctx, p.check.rc, []matchcheck.Query{p.check.query})
	if err != nil {
		return build.RouteDestination{}, "", err
	}
	dest, outcome := p.check.finish(clusters[0])
	return dest, outcome, nil
}

// prepare runs everything up to the matcher: ingress-cluster selection, the
// snapshot load, gateway narrowing and host disambiguation, the listener gate,
// the ingress LB fallback, and the (memoised) translation.
func (s *scopedResolver) prepare(ctx context.Context, req build.RouteRequest) (prepared, error) {
	if len(req.IPs) == 0 {
		// Defensive: the prescan never emits an IP-less request —
		// without a destination IP no ingress cluster can be selected.
		return prepared{outcome: build.RouteNoIngress}, nil
	}

	// Select the ingress cluster from the destination IPs before
	// touching any snapshot. One probe per IP; the candidate sets stay separate
	// so multi-IP answers must agree — never unioned across clusters.
	perIP := make([][]string, len(req.IPs))
	for i, ip := range req.IPs {
		cands, err := s.probe(ctx, ip, req.At)
		if err != nil {
			return prepared{}, err
		}
		perIP[i] = cands
	}
	cluster, pickMiss, ok := pickIngressCluster(req.CallerCluster, perIP)
	if !ok {
		return prepared{outcome: pickMiss}, nil
	}

	rows, err := s.loadSnapshot(ctx, cluster, req)
	if err != nil {
		return prepared{}, err
	}
	snap := snapshot.New(rows, req.At)

	p, err := s.prepareConfig(ctx, snap, cluster, req)
	if err != nil || p.check != nil || p.outcome != build.RouteNoGateway {
		return p, err
	}

	// Ingress LB Service fallback, gated
	// on the miss being RouteNoGateway: the nginx-ingress signature is Hop 3
	// finding no Istio Gateway CR, so resolution never got past gateway
	// selection — yet the loaded snapshot may still map the destination IPs to
	// exactly one ingress LB Service. A DEEPER miss (no_listener_on_port /
	// no_server_for_host / no_route) means an Istio Gateway DID serve the host
	// and its diagnostic reason must not be masked by an LB-entry-point edge.
	// A routed hit always wins; a fallback that finds nothing keeps the
	// pipeline's own miss.
	if lbDest, out, ok := resolveIngressLBService(snap, req.IPs); ok {
		if out == build.RouteIngressLBService {
			lbDest.Cluster = cluster // same ingress-cluster stamp as a RouteHit
			return prepared{dest: lbDest, outcome: out}, nil
		}
		return prepared{outcome: out}, nil // ambiguous_ingress_service
	}
	return p, nil
}

// prepareConfig runs the in-memory chain and the translation for the request:
// 3-hop candidates → host disambiguation → in-memory ScopedFor → listener check
// → istiod translate (memoised per RouteConfiguration). Every miss it returns is
// final; a request reaching the matcher comes back with check set.
func (s *scopedResolver) prepareConfig(ctx context.Context, snap *snapshot.Snapshot, cluster string, req build.RouteRequest) (prepared, error) {
	cands := candidates(snap, req.IPs)
	gwName, ok := gwresolve.New(candsToGateways(cands)).ResolveAmong(req.Host, candNames(cands))
	if !ok {
		return prepared{outcome: build.RouteNoGateway}, nil // no gateway serves the host
	}
	gw, ok := pickCandidate(cands, gwName)
	if !ok {
		// Two candidates share a bare name across namespaces — only reachable
		// when a multi-IP request's per-IP hop 1 landed in different namespaces.
		// Neither is more correct than the other, so degrade instead of
		// guessing; the caller's LB fallback still applies.
		return prepared{outcome: build.RouteNoGateway}, nil
	}

	scoped, found, err := snap.ScopedFor(gw.Namespace, gw.Name)
	if err != nil {
		return prepared{}, err
	}
	if !found {
		// Named by the 3-hop but no live config at the instant.
		return prepared{outcome: build.RouteNoGateway}, nil
	}
	scoped.Port = req.Port
	// The host decides WHICH server on the port owns the RouteConfiguration
	// (each TLS-terminated HTTPS server carries its own).
	scoped.Host = req.Host

	// Listener gate: both misses are decided from config alone — no translate
	// round-trip, no router_check_tool exec. A port with no routable HTTP
	// listener is the mis-guessed-port signature; a port whose servers all
	// serve OTHER hosts is the distinct no-server-for-host miss (translating
	// anyway could only reach RouteNoRoute — istiod builds vhosts from the
	// server-hosts ∩ VS-hosts intersection).
	rcName, st := translate.ListenerFor(scoped)
	switch st {
	case translate.ListenerNoneOnPort:
		return prepared{outcome: build.RouteNoListenerOnPort}, nil
	case translate.ListenerNoServerForHost:
		return prepared{outcome: build.RouteNoServerForHost}, nil
	case translate.ListenerFound:
		// fall through to the translate + router_check_tool stages
	}

	key := rcKey{cluster: cluster, ips: strings.Join(req.IPs, ","), gwNS: gw.Namespace, gwName: gw.Name, rcName: rcName}
	rc, err := s.translate(ctx, key, scoped)
	if err != nil {
		return prepared{}, err
	}
	return prepared{check: &pendingCheck{
		rcKey:   key,
		rc:      rc,
		query:   matchcheck.Query{Host: req.Host, Path: req.Path},
		cluster: cluster,
		snap:    snap,
		ips:     req.IPs,
	}}, nil
}

// finish turns the matched Envoy cluster string into the answer: a parseable
// in-cluster Service is a RouteHit in the selected ingress cluster, carrying
// the ingress LB identity when the snapshot pins exactly one; anything else
// ("" — no route matched — or a non-Service cluster) is RouteNoRoute, which
// does not reach the LB fallback.
func (p *pendingCheck) finish(envoyCluster string) (build.RouteDestination, build.RouteOutcome) {
	dest, ok := ParseEnvoyCluster(envoyCluster)
	if !ok {
		return build.RouteDestination{}, build.RouteNoRoute
	}
	// The destination lives in the selected ingress cluster:
	// ParseEnvoyCluster cannot know it — the Envoy cluster string carries
	// only (port, subset, service, namespace) — so it is stamped here.
	dest.Cluster = p.cluster
	// Ingress chain: recover the ingress LB Service the
	// destination IPs uniquely map to in the already-loaded snapshot (same
	// identity dedup as the LB fallback, zero new store reads) so the parse
	// can emit the full caller → ingress → backend chain. Ambiguous or
	// incomplete identity leaves the fields empty — the chain degrades to
	// the direct edge; a hit is NEVER demoted.
	if id, status := ingressServiceIdentity(p.snap, p.ips); status == identityUnique {
		dest.IngressNamespace, dest.IngressService = id.ns, id.name
	}
	return dest, build.RouteHit
}

// loadSnapshot loads the selected ingress cluster's configuration state at
// req.At: one (memoised) store.LoadTrafficAt per destination IP, rows unioned —
// multi-A-record DNS answers are a union of candidates WITHIN the one selected
// cluster (cross-cluster unions are forbidden).
//
// The union is deduplicated by resource-version identity. Two destination IPs
// published by the SAME ingress Service (a dual-stack Service carrying an A and
// an AAAA address, or several addresses of one load balancer) make each per-IP
// load return the same gateway/VS rows, and the store's own dedup only spans a
// single call. Without this, ScopedFor would hand istiod the same
// VirtualService twice and its config store would reject it ("item already
// exists"), failing the whole resolution.
//
// The per-IP rows are shared with every other key of the build (the load
// memo); the union copies them into fresh slices and nothing downstream
// mutates a row.
func (s *scopedResolver) loadSnapshot(ctx context.Context, cluster string, req build.RouteRequest) (store.TrafficSnapshot, error) {
	var out store.TrafficSnapshot
	svcSeen, depSeen := map[versionKey]bool{}, map[versionKey]bool{}
	gwSeen, vsSeen := map[versionKey]bool{}, map[versionKey]bool{}
	for _, ip := range req.IPs {
		w, err := s.loadTraffic(ctx, cluster, ip, req.At)
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
