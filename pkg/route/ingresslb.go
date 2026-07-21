package route

import (
	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/memwindow"
)

// ingressIdentity is one (namespace, name) ingress LB Service identity found
// in the loaded window.
type ingressIdentity struct{ ns, name string }

// ingressIdentityStatus classifies the window-wide identity dedup of the
// destination IP set (route-hit-ingress-chain D1).
type ingressIdentityStatus int

const (
	// identityUnique: every IP's candidate set is a singleton and all agree.
	identityUnique ingressIdentityStatus = iota
	// identityAmbiguous: a same-IP collision (>1 identity for one IP), or
	// disagreeing singletons across IPs with no empty IP.
	identityAmbiguous
	// identityIncomplete: some IP matched no ingress Service row at all (or
	// there were no IPs) — the evidence is incomplete, not conflicting.
	identityIncomplete
)

// ingressServiceIdentity resolves the destination IP set to the ingress LB
// Service identity it uniquely maps to inside the loaded (selected-cluster)
// window. It is the identity-dedup core shared by the ingress LB Service
// fallback (resolveIngressLBService) and the RouteHit ingress-chain stamping
// (route-hit-ingress-chain D1), operating purely on rows already loaded by
// LoadTrafficWindow — no store read.
//
// Per IP the candidate identities are the distinct (namespace, name) of every
// window-overlapping service row carrying the IP (window-wide dedup, sibling
// design D6 — no per-instant evaluation, so normal version churn of one
// identity still counts once while an identity change inside the window
// yields two). Merge precedence over IPs is order-free (sibling design D7):
// any IP with more than one identity → identityAmbiguous; otherwise any IP
// with none → identityIncomplete; otherwise all singleton candidates must
// agree on one identity → identityUnique, and a disagreement is ambiguous.
// Never a lexicographic tie-break: ambiguity degrades rather than guesses.
func ingressServiceIdentity(mw *memwindow.Window, ips []string) (ingressIdentity, ingressIdentityStatus) {
	var (
		agreed   ingressIdentity
		found    bool
		anyEmpty bool
		disagree bool
	)
	for _, ip := range ips {
		seen := map[ingressIdentity]bool{}
		for _, r := range mw.ResolveIPToIngressServices(ip) {
			seen[ingressIdentity{ns: r.Namespace, name: r.Name}] = true
		}
		// Rule 1 has top precedence over every other IP's state, so a same-IP
		// collision may return immediately.
		if len(seen) > 1 {
			return ingressIdentity{}, identityAmbiguous
		}
		if len(seen) == 0 {
			anyEmpty = true
			continue
		}
		for id := range seen {
			if found && id != agreed {
				// Rule 3 disagreement ranks BELOW rule 2 (an empty IP anywhere
				// wins), so only record it here.
				disagree = true
			}
			agreed, found = id, true
		}
	}
	if anyEmpty || !found {
		return ingressIdentity{}, identityIncomplete
	}
	if disagree {
		return ingressIdentity{}, identityAmbiguous
	}
	return agreed, identityUnique
}

// resolveIngressLBService is the post-Istio-miss fallback of the
// ingress-lb-service-fallback change: when the Gateway + VirtualService
// pipeline produced no hit over the loaded (selected-cluster) window — the
// caller additionally gates on the folded miss being RouteNoGateway, so a
// deeper Istio miss keeps its diagnostic reason — does the destination IP
// set map to exactly ONE ingress LB Service identity in that window?
// Semantics are deliberately coarse — "which ingress LB Service owns this
// IP" — host, path, and port play no part (design D4). The identity dedup
// itself lives in ingressServiceIdentity (shared with the RouteHit
// ingress-chain stamping).
//
// The returned destination carries Namespace/Service (mirrored into
// IngressNamespace/IngressService — the destination IS the ingress entry
// point); the caller stamps Cluster with the locked ingress cluster (design
// D11 — same as a RouteHit), and Port/Subset stay zero (discarded in v1).
func resolveIngressLBService(mw *memwindow.Window, ips []string) (build.RouteDestination, build.RouteOutcome, bool) {
	id, status := ingressServiceIdentity(mw, ips)
	switch status {
	case identityAmbiguous:
		return build.RouteDestination{}, build.RouteAmbiguousIngressService, true
	case identityIncomplete:
		return build.RouteDestination{}, "", false
	case identityUnique:
	}
	return build.RouteDestination{
		Namespace:        id.ns,
		Service:          id.name,
		IngressNamespace: id.ns,
		IngressService:   id.name,
	}, build.RouteIngressLBService, true
}
