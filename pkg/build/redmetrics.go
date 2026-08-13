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
//
// A nil Err with an EMPTY vector means the metric does not exist upstream. The
// spec puts that on the same footing as a query error: `error_rate` is OMITTED
// (never 0) and `p90_server_ms` is absent. `error_rate == 0` is reserved for a
// counter that WAS read (non-empty result) but carried no series matching that
// particular edge.
//
// Consequence for the zero value: `redInputs{}` reads as "neither OPTIONAL
// metric exists", i.e. rate-only metrics — the safe interpretation for the
// route-index-free test entry points that pass it.
type redInputs struct {
	Failed      model.Vector
	FailedErr   error
	Duration    model.Vector
	DurationErr error
}

// pairKey identifies a resolved (src, tgt) edge during the service-graph parse.
// Shared by the resolution loop and the RED attach pass.
type pairKey struct{ src, tgt string }

// redPairAcc accumulates in-scope RED contributions for one pairKey.
// Contributions are stored as slices and summed in ascending order at attach
// time so the result is a pure function of the multiset (design D9) — that
// holds for the histogram buckets too, since float addition is not
// associative and several contributing series may feed one le boundary.
type redPairAcc struct {
	rates   []float64
	fails   []float64
	buckets map[float64][]float64 // le → cumulative-count contributions
	// ineligible is sticky: set when the pair failed a structural condition
	// of the attachment rule (an endpoint that is neither a pod nor a
	// service node, or the RouteHit chain's caller→ingress entry hop —
	// design D1). Sticky rather than per-series so the outcome is a pure
	// function of the data, independent of vector arrival order (D6).
	ineligible bool
	// noUsableLe is set when duration series matched this pair but none
	// carried a parseable classic "le" label (native-histogram / vmrange).
	noUsableLe bool
}

// redJoin holds the series→pair map built during the resolution loop, plus the
// per-pair accumulators.
//
// joinFailures / joinDuration mirror whether the corresponding OPTIONAL query
// returned anything to join against. When neither did — the common case for a
// deployment that exposes neither metric — the join map is never allocated and
// its key is never computed, so the parse pays nothing for an absent metric.
// Both vectors join through the SAME map: the failure counter by full series
// identity, the histogram by that identity minus `le` (design D4).
type redJoin struct {
	joinFailures bool
	joinDuration bool
	// seriesToPairs maps a canonical series identity (all labels except
	// __name__, sorted) to every pair that series contributed to. Normally
	// one pair; multi-target resolution can produce more.
	seriesToPairs map[string][]pairKey
	// acc is the per-pair accumulator; only pairs with at least one
	// in-scope contribution or a structural rejection appear.
	acc map[pairKey]*redPairAcc
}

func newRedJoin(capHint int, joinFailures, joinDuration bool) *redJoin {
	j := &redJoin{
		joinFailures: joinFailures,
		joinDuration: joinDuration,
		acc:          make(map[pairKey]*redPairAcc, capHint),
	}
	if joinFailures || joinDuration {
		j.seriesToPairs = make(map[string][]pairKey, capHint)
	}
	return j
}

// joinKeyed reports whether either companion vector is present, i.e. whether
// the resolution loop must compute a series key at all.
func (j *redJoin) joinKeyed() bool { return j.joinFailures || j.joinDuration }

func (j *redJoin) pairAcc(k pairKey) *redPairAcc {
	a, ok := j.acc[k]
	if !ok {
		a = &redPairAcc{}
		j.acc[k] = a
	}
	return a
}

// recordSeries is called once per upstream total series for each (src,tgt)
// pair it produced.
//
// eligible carries the pair's STRUCTURAL verdict (design D1 conditions 2–3:
// both endpoints resolved to a pod or a service node, and this is not the
// ingress chain's entry hop); a false value rejects the pair permanently.
// inScope carries the SERIES verdict (design D1b: not a span-link series); a
// false value skips this series' measurement while leaving the pair — and every
// other series feeding it — untouched, so a mixed pair is measured over its
// non-link subset. sk is read only when a companion vector is present.
func (j *redJoin) recordSeries(k pairKey, eligible, inScope bool, rate float64, sk string) {
	if !eligible {
		j.pairAcc(k).ineligible = true
		return
	}
	if !inScope {
		return
	}
	a := j.pairAcc(k)
	a.rates = append(a.rates, rate)
	if j.joinKeyed() {
		j.seriesToPairs[sk] = appendUniquePair(j.seriesToPairs[sk], k)
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
		// Tally the JOIN outcome before filtering on value, mirroring
		// accumulateDuration: a healthy deployment's failure series are all 0,
		// and counting those as "not matched" would raise the label-set-drift
		// warning on exactly the population it is meant to exonerate.
		pairs := j.seriesToPairs[seriesKeyOf(s.Metric)]
		if len(pairs) == 0 {
			unmatched++
			continue
		}
		matched++
		v := float64(s.Value)
		// Drop NaN / non-positive and +Inf: a non-finite contribution would
		// poison the error_rate numerator. A zero contributes nothing either.
		if !(v > 0) || math.IsInf(v, 0) {
			continue
		}
		for _, k := range pairs {
			a := j.pairAcc(k)
			a.fails = append(a.fails, v)
		}
	}
	return matched, unmatched
}

// accumulateDuration walks the RAW duration vector and sums bucket counts per
// le boundary onto each mapped pair. Several contributing series mapping to one
// pair sum together, per le (design D4/D5).
//
// The join mirrors accumulateFailures exactly, minus the `le` label: a bucket
// series carries the same dimension set as its request-total series plus `le`,
// so identity-minus-le is that series' key. The unmatched tally is returned for
// the same reason as the failure one — a label-set drift between _total and
// _bucket joins nothing and omits p90_server_ms everywhere, which reads as "the
// producer emits no histogram" unless it is surfaced.
//
// sawNoLe reports that at least one MATCHED series carried no usable "le"
// label (logged as its own degradation reason).
func (j *redJoin) accumulateDuration(duration model.Vector) (matched, unmatched int, sawNoLe bool) {
	for _, s := range duration {
		v := float64(s.Value)
		// Skip NaN, negative and infinite counts — any of them would make the
		// cumulative shape (and therefore the quantile rank) meaningless.
		// Zero-count buckets ARE kept: they are part of a complete histogram.
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		pairs := j.seriesToPairs[bucketSeriesKeyOf(s.Metric)]
		if len(pairs) == 0 {
			unmatched++
			continue
		}
		matched++
		le, ok := parseLe(string(s.Metric[model.BucketLabel]))
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
	return matched, unmatched, sawNoLe
}

// eligible reports whether the pair qualifies for a metrics object: it was
// never structurally rejected and at least one in-scope contributing series fed
// it a positive rate. A pair whose contributing series are ALL span-link series
// has no rates and therefore falls out here — no special case needed (D1b).
func (a *redPairAcc) eligible() bool {
	return a != nil && !a.ineligible && len(a.rates) > 0
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
	return seriesKeyExcluding(m, "")
}

// bucketSeriesKeyOf is seriesKeyOf minus the `le` label: a duration-histogram
// series carries its request-total series' dimension set plus the bucket
// boundary, so dropping `le` yields that series' identity (D4).
func bucketSeriesKeyOf(m model.Metric) string {
	return seriesKeyExcluding(m, model.BucketLabel)
}

func seriesKeyExcluding(m model.Metric, skip model.LabelName) string {
	if len(m) == 0 {
		return ""
	}
	// Collect and sort label names for determinism.
	names := make([]string, 0, len(m))
	for k := range m {
		if k == model.MetricNameLabel || (skip != "" && k == skip) {
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

const (
	// labelEdgeRelation is the operator-configured servicegraph dimension that
	// marks how the connector derived an edge, and edgeRelationLink is the
	// value it carries for an edge materialised from a SPAN LINK. A fixed,
	// case-sensitive contract with no knob — same class as the D30 sentinel
	// values (design D1b).
	labelEdgeRelation = "edge_relation"
	edgeRelationLink  = "link"
)

// redSeriesInScope reports whether a contributing series measures the edge
// (design D1b). A span-link series does not: its two spans belong to different
// trace contexts and the interaction crosses a queue or a database, so its
// rate, failures and durations describe something that is not a
// request-response call. The edge is still emitted; only the measurement is
// skipped. Mirrors promql.serviceGraphLinkExclusionSelector, which drops the
// same series from the two companion vectors upstream.
func redSeriesInScope(m model.Metric) bool {
	return string(m[labelEdgeRelation]) != edgeRelationLink
}
