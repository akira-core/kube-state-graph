package build

import (
	"math"
	"sort"
	"strings"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// redInputs carries the two OPTIONAL RED query results into the parse.
// A non-nil *Err means that query failed and the corresponding field must be
// omitted (not zeroed) on every edge — design D3 / graceful degradation.
// A nil Err with an empty vector means the query succeeded and found nothing
// (error_rate → 0; p90_server_ms → absent).
type redInputs struct {
	Failed      model.Vector
	FailedErr   error
	Duration    model.Vector
	DurationErr error
}

// pairKey identifies a resolved (src, tgt) edge during the service-graph parse.
// Shared by the resolution loop and the RED attach pass.
type pairKey struct{ src, tgt string }

// redKey is the histogram join key: the pod-pair identity the duration query
// groups by (cluster, client_k8s_pod_uid, server_k8s_pod_uid). Several redKeys
// may map to one pairKey (e.g. missing-cluster bucketed to "unknown" alongside
// a sibling carrying the real cluster); their bucket sets are summed (D4/D5).
type redKey struct{ cluster, clientUID, serverUID string }

// redPairAcc accumulates in-scope RED contributions for one pairKey.
// Contributions are stored as slices and summed in ascending order at attach
// time so the result is a pure function of the multiset (design D9) — that
// holds for the histogram buckets too, since float addition is not
// associative and several redKeys may feed one le boundary.
type redPairAcc struct {
	rates   []float64
	fails   []float64
	buckets map[float64][]float64 // le → cumulative-count contributions
	// hasInScope / hasOutOfScope record whether the pair was fed by
	// UID-resolved and/or peer-resolved series (design D1).
	hasInScope    bool
	hasOutOfScope bool
	// noUsableLe is set when duration series matched this pair but none
	// carried a parseable classic "le" label (native-histogram / vmrange).
	noUsableLe bool
}

// redJoin holds the series→pair and redKey→pair maps built during the
// resolution loop, plus the per-pair accumulators.
//
// joinFailures / joinDuration mirror whether the corresponding OPTIONAL query
// returned anything to join against. When it did not — the common case for a
// deployment that exposes neither metric — the join map is never allocated and
// its key is never computed, so the parse pays nothing for an absent metric.
type redJoin struct {
	joinFailures bool
	joinDuration bool
	// seriesToPairs maps a canonical series identity (all labels except
	// __name__, sorted) to every pair that series contributed to. Normally
	// one pair; multi-target resolution can produce more.
	seriesToPairs map[string][]pairKey
	// redToPairs maps the pod-pair triple to every pairKey it resolved to.
	redToPairs map[redKey][]pairKey
	// acc is the per-pair accumulator; only pairs with at least one
	// contribution (in- or out-of-scope) appear.
	acc map[pairKey]*redPairAcc
}

func newRedJoin(capHint int, joinFailures, joinDuration bool) *redJoin {
	j := &redJoin{
		joinFailures: joinFailures,
		joinDuration: joinDuration,
		acc:          make(map[pairKey]*redPairAcc, capHint),
	}
	if joinFailures {
		j.seriesToPairs = make(map[string][]pairKey, capHint)
	}
	if joinDuration {
		j.redToPairs = make(map[redKey][]pairKey, capHint)
	}
	return j
}

func (j *redJoin) pairAcc(k pairKey) *redPairAcc {
	a, ok := j.acc[k]
	if !ok {
		a = &redPairAcc{}
		j.acc[k] = a
	}
	return a
}

// recordSeries is called once per upstream total series for each (src,tgt)
// pair it produced. inScope is true when both raw pod UIDs are non-empty
// (design D1 attachment rule). rate is the series' rate value. sk / rk are
// only read when the corresponding join is active.
func (j *redJoin) recordSeries(k pairKey, inScope bool, rate float64, sk string, rk redKey) {
	a := j.pairAcc(k)
	if !inScope {
		a.hasOutOfScope = true
		return
	}
	a.hasInScope = true
	a.rates = append(a.rates, rate)
	if j.joinFailures {
		j.seriesToPairs[sk] = appendUniquePair(j.seriesToPairs[sk], k)
	}
	if j.joinDuration {
		j.redToPairs[rk] = appendUniquePair(j.redToPairs[rk], k)
	}
}

func appendUniquePair(s []pairKey, k pairKey) []pairKey {
	for _, p := range s {
		if p == k {
			return s
		}
	}
	return append(s, k)
}

// accumulateFailures walks the failure vector and adds matched rates onto the
// pairs recorded during the resolution loop.
//
// The join is by EXACT series identity, so a label-set drift between
// _total and _failed_total silently yields error_rate=0 on every edge — the
// absent-vs-zero conflation the contract forbids. The unmatched tally is
// returned so the caller can surface that case instead of letting it read as a
// clean zero.
func (j *redJoin) accumulateFailures(failed model.Vector) (matched, unmatched int) {
	for _, s := range failed {
		v := float64(s.Value)
		// Drop NaN / non-positive and +Inf: a non-finite contribution would
		// poison the error_rate numerator.
		if !(v > 0) || math.IsInf(v, 0) {
			continue
		}
		pairs := j.seriesToPairs[seriesKeyOf(s.Metric)]
		if len(pairs) == 0 {
			unmatched++
			continue
		}
		matched++
		for _, k := range pairs {
			a := j.pairAcc(k)
			a.fails = append(a.fails, v)
		}
	}
	return matched, unmatched
}

// accumulateDuration walks the pre-aggregated duration vector and sums bucket
// counts per le boundary onto each mapped pair. Several redKeys mapping to one
// pair sum together (design D4/D5).
//
// Returns true when at least one series was seen without a usable "le" label
// (logged as a distinct degradation reason).
func (j *redJoin) accumulateDuration(duration model.Vector) (sawNoLe bool) {
	for _, s := range duration {
		v := float64(s.Value)
		// Skip NaN, negative and infinite counts — any of them would make the
		// cumulative shape (and therefore the quantile rank) meaningless.
		// Zero-count buckets ARE kept: they are part of a complete histogram.
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		rk := redKey{
			cluster:   bucketCluster(string(s.Metric["cluster"])),
			clientUID: string(s.Metric["client_k8s_pod_uid"]),
			serverUID: string(s.Metric["server_k8s_pod_uid"]),
		}
		pairs := j.redToPairs[rk]
		if len(pairs) == 0 {
			continue
		}
		le, ok := parseLe(string(s.Metric["le"]))
		if !ok {
			sawNoLe = true
			for _, k := range pairs {
				j.pairAcc(k).noUsableLe = true
			}
			continue
		}
		for _, k := range pairs {
			a := j.pairAcc(k)
			if a.buckets == nil {
				a.buckets = map[float64][]float64{}
			}
			a.buckets[le] = append(a.buckets[le], v)
		}
	}
	return sawNoLe
}

// eligible reports whether the pair qualifies for a metrics object: every
// contributing series was in-scope (both UIDs non-empty) and at least one
// contributed a positive rate.
func (a *redPairAcc) eligible() bool {
	return a != nil && a.hasInScope && !a.hasOutOfScope && len(a.rates) > 0
}

// attachMetrics builds graph.EdgeMetrics for an eligible pair, or returns
// ok=false when the pair does not qualify. failedOK / durationOK reflect
// whether those queries succeeded (nil error); a false value omits the
// corresponding field rather than zeroing it.
func (a *redPairAcc) attachMetrics(failedOK, durationOK bool) (graph.EdgeMetrics, bool) {
	if !a.eligible() {
		return graph.EdgeMetrics{}, false
	}
	rate := sumAscending(a.rates)
	// A non-finite rate must never reach the serialiser: encoding/json rejects
	// Inf/NaN, so ONE poisoned upstream sample would fail the whole /v1/graph
	// response with a 500. Drop the metrics object for that edge instead.
	if !(rate > 0) || math.IsInf(rate, 0) {
		return graph.EdgeMetrics{}, false
	}
	m := graph.EdgeMetrics{Rate: rate}
	if failedOK {
		er := sumAscending(a.fails) / rate
		switch {
		case math.IsNaN(er), er < 0:
			er = 0
		case er > 1:
			er = 1
		}
		m.ErrorRate = &er
	}
	if durationOK && len(a.buckets) > 0 && !a.noUsableLe {
		bs := make([]bucket, 0, len(a.buckets))
		for le, cs := range a.buckets {
			bs = append(bs, bucket{le: le, count: sumAscending(cs)})
		}
		if q, ok := classicQuantile(0.90, bs); ok {
			if ms := q * 1000; !math.IsInf(ms, 0) && !math.IsNaN(ms) {
				m.P90ServerMs = &ms
			}
		}
	}
	return m, true
}

// sumAscending returns the sum of xs after sorting ascending, so the result is
// independent of contribution arrival order (design D9).
func sumAscending(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)
	var sum float64
	for _, v := range cp {
		sum += v
	}
	return sum
}

// seriesKeyOf builds a deterministic identity for a sample's full label set
// minus __name__, used to join the failure counter to the total series (D4).
func seriesKeyOf(m model.Metric) string {
	if len(m) == 0 {
		return ""
	}
	// Collect and sort label names for determinism.
	names := make([]string, 0, len(m))
	for k := range m {
		if k == model.MetricNameLabel {
			continue
		}
		names = append(names, string(k))
	}
	sort.Strings(names)
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteByte(0) // NUL separator — cannot appear in Prometheus labels
		}
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(string(m[model.LabelName(n)]))
	}
	return b.String()
}

// redInScope reports whether a series' raw pod UIDs satisfy the D1 attachment
// rule (both non-empty). Evaluated on the pre-normalisation UID labels so it
// matches the query-layer pod-pair selector exactly.
func redInScope(clientUID, serverUID string) bool {
	return clientUID != "" && serverUID != ""
}
