// Package gwresolve finds the ingress gateway serving a request host by
// matching the host against every gateway's server hosts, using Istio's own
// wildcard/most-specific semantics — exactly like real Istio gateway selection.
// Ported verbatim from poc/route2a/internal/gwresolve.
package gwresolve

import (
	"sort"
	"strings"

	"istio.io/istio/pkg/config/host"
)

// Gateway is one ingress gateway's identity + its server host patterns.
type Gateway struct {
	Name  string
	Hosts []string // exact / "*.suffix" / "*"
}

type pat struct {
	gw      string
	pattern host.Name
	score   int // higher = more specific
	idx     int // owning host-set index for PickHosts; New always stamps 0
}

// Resolver indexes gateway host patterns for host->gateway lookup.
type Resolver struct {
	pats []pat
}

// specificity ranks a pattern: exact always beats any wildcard; among wildcards
// a longer suffix (longer pattern) is more specific; "*" is least specific.
func specificity(p host.Name) int {
	if p.IsWildCarded() {
		return len(p) // "*.gw000.example.com" (19) > "*.example.com" (13) > "*" (1)
	}
	return len(p) + 1_000_000 // exact host wins outright
}

// SanitizeServerHost strips the optional Istio "<ns>/" binding prefix from a
// gateway server host ("prod/*.example.com", "./host", "*/host") so the bare
// pattern can be matched against a client host. The precedent is istio's
// model.GetSNIHostsForServer, which strips the prefix unconditionally before
// SNI matching. Deliberately asymmetric with host.NamesForNamespace (which
// FILTERS): the namespace prefix restricts which VirtualServices may bind to
// the server, never which client hosts the server serves — so this strips,
// it does not filter.
func SanitizeServerHost(h string) string {
	if _, bare, ok := strings.Cut(h, "/"); ok {
		return bare
	}
	return h
}

// newPats builds the pattern rows for one host set, sanitising each host and
// stamping the owning key/idx on every row.
func newPats(key string, idx int, hosts []string) []pat {
	pats := make([]pat, 0, len(hosts))
	for _, h := range hosts {
		p := host.Name(SanitizeServerHost(h))
		pats = append(pats, pat{gw: key, pattern: p, score: specificity(p), idx: idx})
	}
	return pats
}

// sortPats orders patterns most-specific first: score desc, then pattern asc,
// then idx asc — an identical pattern resolves to the smaller (earlier
// declared) host set, mirroring istio's CheckDuplicates first-declared-wins.
// The idx comparison is numeric, never stringified (else 10 would sort
// before 2). Under New all idx are 0, so that branch is a no-op there.
func sortPats(pats []pat) {
	sort.SliceStable(pats, func(a, b int) bool {
		if pats[a].score != pats[b].score {
			return pats[a].score > pats[b].score
		}
		if pats[a].pattern != pats[b].pattern {
			return pats[a].pattern < pats[b].pattern // deterministic tie-break
		}
		return pats[a].idx < pats[b].idx
	})
}

// New builds a resolver over all gateways. Patterns are pre-sorted most-specific
// first so Resolve returns on the first match. Server hosts are sanitised
// (SanitizeServerHost), so a "<ns>/"-prefixed host still matches.
func New(gws []Gateway) *Resolver {
	r := &Resolver{}
	for _, gw := range gws {
		r.pats = append(r.pats, newPats(gw.Name, 0, gw.Hosts)...)
	}
	sortPats(r.pats)
	return r
}

// PickHosts selects, among several server host sets, the one whose hosts
// most-specifically match reqHost — the server-level twin of Resolve, sharing
// the same comparator. It returns the winning set's index (ok=false when no
// set matches). An identical winning pattern in two sets resolves to the
// smaller index (first declared wins).
func PickHosts(hostSets [][]string, reqHost string) (int, bool) {
	var pats []pat
	for i, hosts := range hostSets {
		pats = append(pats, newPats("", i, hosts)...)
	}
	sortPats(pats)
	h := host.Name(reqHost)
	for _, p := range pats {
		if p.pattern.Matches(h) {
			return p.idx, true
		}
	}
	return 0, false
}

// Resolve returns the gateway whose server hosts most-specifically match the
// request host ("", false = no gateway serves it). This is a hot path.
func (r *Resolver) Resolve(reqHost string) (string, bool) {
	h := host.Name(reqHost)
	for _, p := range r.pats {
		// pats are sorted most-specific first; the first match is the answer.
		if p.pattern.Matches(h) {
			return p.gw, true
		}
	}
	return "", false
}

// ResolveAmong is Resolve limited to a candidate gateway set — the IP-narrowed
// candidates from the ClickHouse 3-hop. It scans the same most-specific-first
// patterns but only considers those whose gateway is in candidates, so the first
// match is the most-specific gateway serving reqHost among the candidates. An
// empty candidate set yields ("", false) — a traffic miss.
func (r *Resolver) ResolveAmong(reqHost string, candidates []string) (string, bool) {
	if len(candidates) == 0 {
		return "", false
	}
	allow := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		allow[c] = struct{}{}
	}
	h := host.Name(reqHost)
	for _, p := range r.pats {
		if _, ok := allow[p.gw]; !ok {
			continue
		}
		if p.pattern.Matches(h) {
			return p.gw, true
		}
	}
	return "", false
}
