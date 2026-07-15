package route

import (
	"github.com/akira-core/kube-state-graph/pkg/build"
)

// pickIngressCluster implements the IP + family-first ingress-cluster
// selection (design D10). caller is the client pod's own cluster — it feeds
// ONLY the family key and the collision tie-break, never a default answer.
// perIP holds, per destination IP, the candidate clusters that had an ingress
// Service carrying that IP overlapping the window (the store probe's output;
// order does not matter — selection is order-free).
//
// Per IP, with G the candidate set and F its same-family subset
// (build.ClusterFamilyKey(g) == build.ClusterFamilyKey(caller)):
//
//  1. |F| == 1              → that cluster
//  2. |F| >  1              → caller if caller ∈ F, else ambiguous
//  3. |F| == 0 && |G| == 1  → that cluster (shared ingress, cross-family name)
//  4. |F| == 0 && |G| >  1  → caller if caller ∈ G, else ambiguous
//  5. |G| == 0              → no ingress
//
// Multi-IP merge: every IP selects independently and ALL must agree on one
// cluster. All IPs "no ingress" → RouteNoIngress; any IP ambiguous, any
// disagreement between selected clusters, or a mix of "no ingress" and a
// selection → RouteAmbiguousIngress. Candidate sets are never unioned across
// IPs — a wrong-cluster answer is the failure class this function exists to
// prevent, so every unresolvable case degrades (the caller falls external)
// rather than guessing.
//
// Pure function: no I/O, no state — the store's whole job upstream is
// answering "ip → []cluster". ok=false ⇒ miss carries the RouteOutcome to
// report (RouteNoIngress or RouteAmbiguousIngress).
func pickIngressCluster(caller string, perIP [][]string) (cluster string, miss build.RouteOutcome, ok bool) {
	if len(perIP) == 0 {
		// Defensive: the prescan never emits an IP-less request (design D6).
		return "", build.RouteNoIngress, false
	}

	callerFamily := build.ClusterFamilyKey(caller)
	picked := ""    // the agreed cluster so far
	anyPick := false // at least one IP selected a cluster
	anyNone := false // at least one IP had no candidate

	for _, cands := range perIP {
		c, outcome, selOK := pickOneIP(caller, callerFamily, cands)
		switch {
		case !selOK && outcome == build.RouteAmbiguousIngress:
			return "", build.RouteAmbiguousIngress, false
		case !selOK: // no ingress for this IP
			anyNone = true
		case anyPick && c != picked: // disagreement between IPs
			return "", build.RouteAmbiguousIngress, false
		default:
			picked, anyPick = c, true
		}
	}

	switch {
	case !anyPick:
		return "", build.RouteNoIngress, false
	case anyNone:
		// A mix of "no ingress" and a selection: some answer IPs are off-mesh
		// (or stale) — degrade rather than trust the partial agreement.
		return "", build.RouteAmbiguousIngress, false
	default:
		return picked, "", true
	}
}

// pickOneIP applies the D10 selection table to one IP's candidate set.
func pickOneIP(caller, callerFamily string, cands []string) (string, build.RouteOutcome, bool) {
	seen := map[string]bool{}
	var family []string // F: same-family candidates
	callerInG := false
	callerInF := false
	total := 0 // |G| after dedup
	var only string

	for _, c := range cands {
		if seen[c] {
			continue
		}
		seen[c] = true
		total++
		only = c
		if c == caller {
			callerInG = true
		}
		if build.ClusterFamilyKey(c) == callerFamily {
			family = append(family, c)
			if c == caller {
				callerInF = true
			}
		}
	}

	switch {
	case total == 0:
		return "", build.RouteNoIngress, false
	case len(family) == 1:
		return family[0], "", true
	case len(family) > 1:
		if callerInF {
			return caller, "", true
		}
		return "", build.RouteAmbiguousIngress, false
	case total == 1:
		return only, "", true
	case callerInG:
		// Defensive mirror of the D10 table's rule 4 tie-break. Unreachable
		// in practice: caller ∈ G implies caller ∈ F (a cluster is always in
		// its own family), which the |F| >= 1 cases above already consumed.
		return caller, "", true
	default:
		return "", build.RouteAmbiguousIngress, false
	}
}
