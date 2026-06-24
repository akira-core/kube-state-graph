package build

import "strings"

// clusterFamilyKey normalises a cluster name to its family key by replacing
// every maximal ASCII digit run with a single '0' sentinel: "prod-03" and
// "prod-12" both yield "prod-0" (same family), while "staging-1" yields
// "staging-0" (a different family). A digit-free name normalises to itself,
// so its family is exact-name match — the pre-fan-out status quo. Two
// clusters are in the same family iff their keys are equal.
//
// The sentinel is itself a digit, which makes the mapping collision-free
// without escaping: every '0' in a key came from a digit run (any literal
// digit is part of a run and is collapsed), and a non-digit byte can never
// equal the sentinel. A non-digit sentinel such as '#' would collide with a
// cluster name literally containing it ("prod-#" vs "prod-1").
//
// The connection-string resolver (D29 Stage 0) uses the family to widen its
// service lookup beyond the trace-source cluster: a service mesh routes the
// same Kubernetes Service DNS name to any cluster of the caller's numbered
// family, never across families. The rule is a hardcoded pure string
// function — no flag, env var, or config field — so resolution stays a pure
// function of (series labels, topology) and determinism is preserved.
func clusterFamilyKey(name string) string {
	var b strings.Builder
	inDigits := false
	for i := range len(name) {
		c := name[i]
		if c >= '0' && c <= '9' {
			if !inDigits {
				b.WriteByte('0')
				inDigits = true
			}
			continue
		}
		inDigits = false
		b.WriteByte(c)
	}
	return b.String()
}
