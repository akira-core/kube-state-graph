package build

import (
	"context"
	"time"
)

// RouteRequest is one route-resolution question posed to a RouteResolver: which
// Kubernetes Service did the Istio ingress config route (Host, Path, Port) to
// during [Start, End]? The ingress cluster is not an input — the resolver
// selects it from the destination IPs (design D10), with CallerCluster feeding
// only the family key and the collision tie-break.
//
// It is assembled by the collectRouteQueries prescan in ReadServiceGraph for
// every server="unknown" endpoint that would otherwise fall back to an
// external node (the translate-global-fqdn-to-k8s-service change, design D2/D3).
type RouteRequest struct {
	// CallerCluster is the already-resolved client pod's own cluster. It is
	// used ONLY to derive the cluster-family key and to break candidate ties
	// during ingress-cluster selection (design D10/D11) — it never scopes a
	// route-store window load by itself.
	CallerCluster string
	// Host is the peer FQDN with any trailing ":<port>" already split off.
	Host string
	// Path is the HTTP path to match. The service-graph metric carries no
	// path dimension, so this is fixed to "/" today.
	Path string
	// Port is the ingress listener port; it selects WHICH RouteConfiguration
	// (http.<port> / https.<port>...) the gateway config is translated to.
	// Derived per design D5: peer-address ":<port>" → optional
	// client_server_port / client_net_peer_port label → default 443.
	Port int
	// IPs are the destination IPs from the client_dns_answers dimension.
	// They are REQUIRED (design D6): they select the ingress cluster before
	// any window is loaded, and the prescan never emits an IP-less request —
	// an endpoint with no parseable IP falls external without consulting the
	// engine at all.
	IPs []string
	// Start / End are the build's own time window, passed through verbatim.
	Start, End time.Time
}

// RouteDestination is a resolved route target: the Kubernetes Service an
// Envoy cluster string (outbound|<port>|<subset>|<svc>.<ns>.svc.cluster.local)
// names, in the engine-selected ingress cluster. Cluster, Namespace and
// Service feed the graph — the triple is handed to the same
// resolveServiceLevel used by every other service resolution path, anchored on
// Cluster (design D11). Port and Subset are parsed but unused in v1 (labels
// stay strict typological metadata; a typed attribute would be a separate
// change).
type RouteDestination struct {
	// Cluster is the ingress cluster the engine selected from the destination
	// IPs (design D10) — the cluster whose Gateway + VirtualService config
	// produced this destination, and the cluster the parse anchors on. It may
	// differ from the caller's cluster.
	Cluster   string
	Namespace string
	Service   string
	Port      uint32
	Subset    string
}

// RouteOutcome classifies one ResolveRoute answer. Every non-hit degrades to
// the caller's existing external-node fallback; the outcome only picks WHICH
// diagnostic reason is recorded, so a mis-derived listener port
// (RouteNoListenerOnPort, design D5) is distinguishable in the logs from a
// host no gateway serves, a path no route matches, or an ingress-cluster
// selection that found nothing / could not converge (design D10).
type RouteOutcome string

const (
	// RouteHit — the config routed (host, path, port) to a Service.
	RouteHit RouteOutcome = "hit"
	// RouteNoGateway — no gateway in the selected ingress cluster is
	// reachable from the supplied destination IPs and serves the host in the
	// window.
	RouteNoGateway RouteOutcome = "no_gateway"
	// RouteNoListenerOnPort — a gateway serves the host, but declares no
	// routable HTTP listener on the derived port (unserved port, TLS
	// passthrough, TCP). The design-D5 signature of a mis-guessed port.
	RouteNoListenerOnPort RouteOutcome = "no_listener_on_port"
	// RouteNoRoute — the listener exists but no route matched the path (or
	// the matched cluster string was not a parseable in-cluster Service).
	RouteNoRoute RouteOutcome = "no_route"
	// RouteNoIngress — no cluster had an ingress Service carrying any of the
	// destination IPs during the window (design D10). Occurs before any
	// window is loaded.
	RouteNoIngress RouteOutcome = "no_ingress"
	// RouteAmbiguousIngress — the ingress-cluster candidates could not be
	// reduced to one cluster by the family-first selection (an unresolvable
	// tie, or multi-IP selections that disagree — design D10). Occurs before
	// any window is loaded. Deliberately degrades rather than guessing:
	// never a wrong cluster.
	RouteAmbiguousIngress RouteOutcome = "ambiguous_ingress_cluster"
)

// RouteResolver answers RouteRequests against a versioned Istio-config store.
// A nil RouteResolver means the feature is OFF: the prescan collects nothing
// and the service-graph reader behaves byte-for-byte as it did before route
// resolution existed. This is the default and the regression safety net.
//
// The concrete implementation lives in pkg/route, which pkg/build MUST NOT
// import (design D1 — dependency hygiene: pkg/route links istio.io/istio,
// clickhouse-go, and envoy protos, and an embedder of pkg/kubegraph must not
// inherit them). Only cmd/kube-state-graph (or an embedder that opts in)
// constructs one and injects it via Options.RouteResolver.
//
// ResolveRoute returns (destination, RouteHit, nil) on a hit; (zero, <miss
// outcome>, nil) when the config yields no destination; and a non-nil error
// only for infrastructure failures (store unreachable, tool failure, timeout).
// Every non-hit — miss or error — degrades to the caller's existing
// external-node fallback; route resolution can never fail a build.
type RouteResolver interface {
	ResolveRoute(ctx context.Context, req RouteRequest) (RouteDestination, RouteOutcome, error)
}
