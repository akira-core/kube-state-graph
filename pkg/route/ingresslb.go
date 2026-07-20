package route

import (
	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/memwindow"
)

// resolveIngressLBService is the post-Istio-miss fallback of the
// ingress-lb-service-fallback change: when the Gateway + VirtualService
// pipeline produced no hit over the loaded (selected-cluster) window — the
// caller additionally gates on the folded miss being RouteNoGateway, so a
// deeper Istio miss keeps its diagnostic reason — does the destination IP
// set map to exactly ONE ingress LB Service identity in that window? Semantics are deliberately coarse — "which ingress LB Service
// owns this IP" — host, path, and port play no part (design D4).
//
// Per IP the candidate identities are the distinct (namespace, name) of every
// window-overlapping service row carrying the IP (window-wide dedup, design
// D6 — no per-instant evaluation, so normal version churn of one identity
// still counts once while an identity change inside the window yields two).
// Merge precedence over IPs is order-free (design D7): any IP with more than
// one identity → RouteAmbiguousIngressService; otherwise any IP with none →
// no fallback (ok=false — the caller keeps the Istio pipeline's own miss);
// otherwise all singleton candidates must agree on one identity → that
// identity with RouteIngressLBService, and a disagreement is ambiguous.
// Never a lexicographic tie-break: ambiguity degrades rather than guesses.
//
// The returned destination carries Namespace/Service only; the caller stamps
// Cluster with the locked ingress cluster (design D11 — same as a RouteHit),
// and Port/Subset stay zero (discarded in v1).
func resolveIngressLBService(mw *memwindow.Window, ips []string) (build.RouteDestination, build.RouteOutcome, bool) {
	type identity struct{ ns, name string }
	var (
		agreed   identity
		found    bool
		anyEmpty bool
		disagree bool
	)
	for _, ip := range ips {
		seen := map[identity]bool{}
		for _, r := range mw.ResolveIPToIngressServices(ip) {
			seen[identity{ns: r.Namespace, name: r.Name}] = true
		}
		// Rule 1 has top precedence over every other IP's state, so a same-IP
		// collision may return immediately.
		if len(seen) > 1 {
			return build.RouteDestination{}, build.RouteAmbiguousIngressService, true
		}
		if len(seen) == 0 {
			anyEmpty = true
			continue
		}
		for id := range seen {
			if found && id != agreed {
				// Rule 3 disagreement ranks BELOW rule 2 (an empty IP anywhere
				// keeps the Istio miss), so only record it here.
				disagree = true
			}
			agreed, found = id, true
		}
	}
	if anyEmpty || !found {
		return build.RouteDestination{}, "", false
	}
	if disagree {
		return build.RouteDestination{}, build.RouteAmbiguousIngressService, true
	}
	return build.RouteDestination{Namespace: agreed.ns, Service: agreed.name}, build.RouteIngressLBService, true
}
