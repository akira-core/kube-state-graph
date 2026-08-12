package build

import (
	"math"
	"sort"
	"strconv"
)

// bucket is one classic-histogram cumulative boundary used by classicQuantile.
// le is the upper bound of the bucket (Prometheus "le" label); count is the
// cumulative sample count up to and including that bound.
type bucket struct {
	le    float64
	count float64
}

// classicQuantile implements the classic cumulative-le histogram quantile
// with in-bucket linear interpolation (design D5), matching the semantics of
// PromQL histogram_quantile(q, ...).
//
// Contract (ok=false when any of these fail):
//   - fewer than two distinct finite-or-Inf boundaries
//   - missing +Inf bucket
//   - zero total count
//   - non-numeric / unparsable le (callers must parse before calling)
//
// When the quantile lands in the +Inf bucket, the result is clamped to the
// highest finite le boundary. Result is in the same unit as le (seconds for
// the service-graph server histogram).
func classicQuantile(q float64, buckets []bucket) (float64, bool) {
	if len(buckets) < 2 {
		return 0, false
	}
	// Work on a sorted copy so input order cannot affect the result (D6).
	bs := make([]bucket, len(buckets))
	copy(bs, buckets)
	sort.SliceStable(bs, func(i, j int) bool {
		return bs[i].le < bs[j].le
	})

	// Require a +Inf terminal bucket and a non-zero total count.
	if !math.IsInf(bs[len(bs)-1].le, 1) {
		return 0, false
	}
	total := bs[len(bs)-1].count
	if total <= 0 || math.IsNaN(total) {
		return 0, false
	}

	// Highest finite le for the +Inf clamp.
	highestFinite := math.NaN()
	for i := len(bs) - 2; i >= 0; i-- {
		if !math.IsInf(bs[i].le, 0) {
			highestFinite = bs[i].le
			break
		}
	}
	if math.IsNaN(highestFinite) {
		// Only +Inf present (or all Inf) — not enough structure.
		return 0, false
	}

	// Defensive clamp of q into [0, 1].
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	rank := q * total

	// Walk cumulative buckets; the first whose count >= rank holds the quantile.
	// Lower bound of bucket i is the previous finite le (0 for the first).
	var prevLe, prevCount float64
	for _, b := range bs {
		if b.count >= rank {
			if math.IsInf(b.le, 1) {
				// Quantile lands in +Inf → clamp to highest finite le.
				return highestFinite, true
			}
			// Linear interpolation inside [prevLe, b.le].
			width := b.le - prevLe
			delta := b.count - prevCount
			if delta <= 0 {
				// Flat cumulative: return the upper bound.
				return b.le, true
			}
			frac := (rank - prevCount) / delta
			return prevLe + frac*width, true
		}
		if !math.IsInf(b.le, 0) {
			prevLe = b.le
		}
		prevCount = b.count
	}
	// Exhausted without matching — should be unreachable given +Inf present.
	return highestFinite, true
}

// parseLe parses a Prometheus classic-histogram "le" label value into a float64.
// Accepts "+Inf" / "Inf" / "-Inf" and ordinary decimal forms. Returns ok=false
// for empty or unparsable values (native-histogram / vmrange case).
//
// NaN is rejected explicitly even though strconv accepts it: a NaN boundary
// would be used as a map key by the bucket accumulator, and since NaN != NaN
// every occurrence would allocate a fresh entry — unbounded growth driven by
// an untrusted upstream label, for a value no quantile can use anyway.
// strconv.ParseFloat already accepts every Prometheus infinity spelling
// ("+Inf" / "Inf" / "inf" / "-Inf"), case-insensitively, so no special-casing
// is needed ahead of it.
func parseLe(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) {
		return 0, false
	}
	return v, true
}
