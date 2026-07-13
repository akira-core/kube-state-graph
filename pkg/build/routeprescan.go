package build

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// defaultListenerPort is the design-D5 fallback when neither the peer-address
// value nor the optional port dimensions carry a listener port. 443 rather
// than 80: a Gateway declaring only :443 (or an httpsRedirect :80 stub) is the
// common global-FQDN ingress shape, and a wrong guess fails as "no listener"
// (→ external), never as a wrong destination.
const defaultListenerPort = 443

// peerLabels bundles the client-recorded peer dimensions of one service-graph
// sample that feed the unknown-server enrichment: the two peer-address labels
// the D2 precedence already reads, plus the two OPTIONAL dimensions added by
// translate-global-fqdn-to-k8s-service (destination IPs and listener port).
type peerLabels struct {
	netPeerName   string // client_net_peer_name (checked first)
	serverAddress string // client_server_address (checked second)
	dnsAnswers    string // client_dns_answers — raw label, parsed lazily
	serverPort    string // client_server_port
	netPeerPort   string // client_net_peer_port
}

// peerLabelsOf extracts the bundle from one sample's metric.
func peerLabelsOf(m model.Metric) peerLabels {
	return peerLabels{
		netPeerName:   string(m["client_net_peer_name"]),
		serverAddress: string(m["client_server_address"]),
		dnsAnswers:    string(m["client_dns_answers"]),
		serverPort:    string(m["client_server_port"]),
		netPeerPort:   string(m["client_net_peer_port"]),
	}
}

// value returns the peer-address value under the existing D2 precedence:
// client_net_peer_name first, client_server_address second, "" when neither
// is present.
func (p peerLabels) value() string {
	if p.netPeerName != "" {
		return p.netPeerName
	}
	return p.serverAddress
}

// splitPeerAddressPort best-effort splits a trailing ":<port>" suffix off a
// client_net_peer_name / client_server_address value. net.SplitHostPort fails
// on a plain hostname/IP with no port (no colon) or on an unbracketed IPv6
// literal (ambiguous colon) — either failure yields the raw, unsplit value as
// the host and "" as the port. This is the pre-change stripPeerAddressPort
// with the port RETURNED instead of discarded (design D5 step 1); every
// classification caller keeps using the host alone, so their behaviour is
// unchanged.
func splitPeerAddressPort(value string) (host, port string) {
	h, p, err := net.SplitHostPort(value)
	if err != nil {
		return value, ""
	}
	return h, p
}

// derivePort implements the design-D5 listener-port precedence: (1) the
// ":<port>" split off the peer-address value, (2) the optional
// client_server_port then client_net_peer_port dimension, (3) 443. A value
// that does not parse as a port number falls through to the next step.
func derivePort(hostPort string, p peerLabels) int {
	for _, cand := range []string{hostPort, p.serverPort, p.netPeerPort} {
		if cand == "" {
			continue
		}
		if n, err := strconv.Atoi(cand); err == nil && n > 0 && n <= 65535 {
			return n
		}
	}
	return defaultListenerPort
}

// parseDNSAnswers parses the optional client_dns_answers dimension into the
// destination-IP list. Exporters flatten the OTel string-array attribute in
// several shapes ("10.0.0.1", "10.0.0.1,10.0.0.2", "[10.0.0.1 10.0.0.2]"), so
// the parse is permissive: brackets stripped, split on commas/semicolons/
// whitespace, non-IP tokens dropped, order preserved, duplicates removed.
// Order matters only for determinism of the joined key — the resolver unions
// the per-IP candidate sets, which is order-free.
func parseDNSAnswers(raw string) []string {
	raw = strings.Trim(raw, "[]")
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t'
	})
	var out []string
	seen := map[string]bool{}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] || net.ParseIP(f) == nil {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// routeKey identifies one distinct route-resolution question within a build.
// Start/End are deliberately absent — they are constant per build — so
// multiple samples naming the same (cluster, host, port, IPs) share one
// engine invocation. ips is the parsed answer list joined by "," (order
// preserved from the label, so the key is a pure function of the sample).
type routeKey struct {
	cluster string
	host    string
	path    string
	port    int
	ips     string
}

// routeEntry is one resolved routeKey: the outcome (and destination on a hit)
// or the fact that the resolver errored. Misses are recorded too, so the
// parse can log the precise route_engine_* reason without re-consulting the
// resolver (the parse stays I/O-free).
type routeEntry struct {
	dest    RouteDestination
	outcome RouteOutcome
	failed  bool // resolver returned an error — logged as route_engine_error
}

// routeIndex is the prefetched answer set handed into parseServiceGraphRoutes.
// A nil index means route resolution is off (no resolver, or nothing to ask).
type routeIndex map[routeKey]routeEntry

// peerRouteKey derives the routeKey for one unknown-server endpoint from the
// already-resolved client pod and the sample's peer labels. ok=false when the
// sample carries no peer-address value (nothing to resolve — the endpoint
// drops exactly as before). Shared by the collectRouteQueries prescan and by
// resolveUnknownServerPeer so the two can never derive different keys for the
// same sample (design D2's anti-drift requirement).
func peerRouteKey(anchorCluster string, peer peerLabels) (routeKey, bool) {
	value := peer.value()
	if value == "" {
		return routeKey{}, false
	}
	host, portStr := splitPeerAddressPort(value)
	return routeKey{
		cluster: anchorCluster,
		host:    host,
		path:    "/", // the metric carries no HTTP path dimension
		port:    derivePort(portStr, peer),
		ips:     strings.Join(parseDNSAnswers(peer.dnsAnswers), ","),
	}, true
}

// request expands a routeKey back into the RouteRequest posed to the engine.
func (k routeKey) request(start, end time.Time) RouteRequest {
	var ips []string
	if k.ips != "" {
		ips = strings.Split(k.ips, ",")
	}
	return RouteRequest{
		Cluster: k.cluster,
		Host:    k.host,
		Path:    k.path,
		Port:    k.port,
		IPs:     ips,
		Start:   start,
		End:     end,
	}
}

// collectRouteQueries is the pure prescan over the service-graph vector: it
// returns the deduped routeKey set for every endpoint that WOULD fall to an
// external node in resolveUnknownServerPeer — i.e. every endpoint route
// resolution could still save. It mirrors the parse loop's per-sample
// preconditions (zero-rate drop, self-loop UID guard, wholly-empty-side drop,
// the unknown-server trigger) and the classification ladder via the same
// sgResolver methods the parse uses (lookupClientPod, classifyPeerHost,
// anchorHolds), so the two cannot drift. It materialises nothing: only the
// resolver's immutable topology indexes are consulted.
//
// Collection deliberately covers ALL THREE external branches (design D3): a
// host no grammar matches, an IP literal with no ClusterIP hit, AND a
// classified (namespace, service) the anchor cluster does not hold — the
// branch a global FQDN like api.example.com actually takes.
func collectRouteQueries(vec model.Vector, topology Topology) []routeKey {
	if len(vec) == 0 {
		return nil
	}
	r := newSGResolver(topology)

	var keys []routeKey
	seen := map[routeKey]bool{}
	for _, s := range vec {
		if !(s.Value > 0) {
			continue
		}
		serverLabel := string(s.Metric["server"])
		if serverLabel != sentinelUnknown {
			continue
		}
		clientLabel := string(s.Metric["client"])
		traceCluster := string(s.Metric["cluster"])
		if traceCluster == "" {
			traceCluster = "unknown" // mirrors missingClusterCounts.bucket
		}
		clientUID := string(s.Metric["client_k8s_pod_uid"])
		serverUID := string(s.Metric["server_k8s_pod_uid"])
		clientUID, serverUID = normalizeSelfLoopUIDs(clientUID, serverUID, clientLabel, serverLabel)
		if (clientUID == "" && clientLabel == "") || (serverUID == "" && serverLabel == "") {
			continue // wholly-empty side: the parse drops the series pre-resolution
		}
		if serverUID != "" {
			if _, known := r.podByUID[serverUID]; known {
				continue // server resolves to a real pod — enrichment never fires
			}
		}
		clientPod := r.lookupClientPod(traceCluster, clientUID)
		if clientPod == nil {
			continue // trigger requires a REAL topology client pod
		}

		peer := peerLabelsOf(s.Metric)
		anchorCluster := clientPod.Labels()["cluster"]
		key, ok := peerRouteKey(anchorCluster, peer)
		if !ok {
			continue // no peer value — the endpoint drops, nothing to resolve
		}
		// Skip endpoints the existing in-cluster ladder already resolves: the
		// route engine is consulted ONLY where the parse would fall external.
		if ns, svc, classified := r.classifyPeerHost(key.host, clientPod.Labels()["namespace"], anchorCluster); classified && r.anchorHolds(anchorCluster, ns, svc) {
			continue
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	// Deterministic resolution order (D6): sorted, not vector-arrival order.
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.cluster != b.cluster {
			return a.cluster < b.cluster
		}
		if a.host != b.host {
			return a.host < b.host
		}
		if a.port != b.port {
			return a.port < b.port
		}
		return a.ips < b.ips
	})
	return keys
}

// resolveRouteQueries answers the prescan's keys against the injected
// RouteResolver, serially, each call bounded by perCallTimeout (zero = inherit
// ctx only). Errors and misses are recorded in the index — never returned —
// so route resolution can never fail a build (design D9); the parse logs the
// per-endpoint reason off the recorded entry.
func resolveRouteQueries(ctx context.Context, resolver RouteResolver, perCallTimeout time.Duration, keys []routeKey, start, end time.Time) routeIndex {
	if resolver == nil || len(keys) == 0 {
		return nil
	}
	idx := make(routeIndex, len(keys))
	for _, k := range keys {
		callCtx, cancel := ctx, context.CancelFunc(func() {})
		if perCallTimeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, perCallTimeout)
		}
		dest, outcome, err := resolver.ResolveRoute(callCtx, k.request(start, end))
		cancel()
		if err != nil {
			slog.Debug("route-engine resolution errored (endpoint degrades to external)",
				"cluster", k.cluster, "host", k.host, "port", k.port, "ips", k.ips, "error", err)
			idx[k] = routeEntry{failed: true}
			continue
		}
		idx[k] = routeEntry{dest: dest, outcome: outcome}
		if ctx.Err() != nil {
			// Build deadline hit: stop asking; uncollected keys simply have no
			// entry and fall back to their pre-change external reasons.
			break
		}
	}
	return idx
}

// routeIndexResolve is the parse-side consult: given the entry for one
// endpoint's key, it resolves a hit through the same resolveServiceLevel every
// other path uses, and otherwise records the precise route_engine_* reason
// before the caller falls back to the external node. ids is nil when the
// endpoint stays external; noted reports whether a route_engine_* reason was
// recorded (so the caller does not double-count the endpoint under its
// classify reason too — the classify reason survives as the classify_reason
// attribute).
func (r *sgResolver) routeIndexResolve(key routeKey, value, origReason string, t sgTrace) (ids []string, noted bool) {
	entry, ok := r.routes[key]
	if !ok {
		return nil, false // engine off or endpoint not collected: pre-change behaviour
	}
	switch {
	case entry.failed:
		r.noteExternal("route_engine_error", t, "host", key.host, "port", key.port,
			"peer_address", value, "anchor_cluster", key.cluster, "classify_reason", origReason)
	case entry.outcome == RouteHit:
		if ids := r.resolveServiceLevel(key.cluster, entry.dest.Namespace, entry.dest.Service); len(ids) > 0 {
			slog.Debug("service-graph unknown-server peer resolved via route engine",
				"side", t.side, "peer_address", value, "host", key.host, "port", key.port,
				"service", entry.dest.Service, "namespace", entry.dest.Namespace,
				"anchor_cluster", key.cluster, "service_id", ids[0],
				"client", t.clientLabel, "server", t.serverLabel)
			return ids, true
		}
		// The engine's destination is not a Service the anchor cluster holds in
		// topology (config store and kube_service_info disagree) — external.
		r.noteExternal("route_engine_dest_anchor_lacks_service", t,
			"service", entry.dest.Service, "namespace", entry.dest.Namespace,
			"host", key.host, "port", key.port, "anchor_cluster", key.cluster)
	case entry.outcome == RouteNoListenerOnPort:
		// The design-D5 mis-derived-port signature, kept distinct from an
		// ordinary route miss so operators can spot a bad port guess.
		r.noteExternal("route_engine_no_listener_on_port", t, "host", key.host,
			"port", key.port, "peer_address", value, "anchor_cluster", key.cluster)
	default: // RouteNoGateway / RouteNoRoute
		r.noteExternal("route_engine_miss", t, "host", key.host, "port", key.port,
			"outcome", string(entry.outcome), "peer_address", value,
			"anchor_cluster", key.cluster, "classify_reason", origReason)
	}
	return nil, true
}
