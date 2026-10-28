package promql

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// NamePair is one namespaced object — a pod or a claim — named by its
// namespace and its name. A name is unique only within a namespace, so a read of
// known objects is keyed by the pair.
type NamePair struct {
	Namespace, Name string
}

// RenderNamesAcrossNamespaces renders a family restricted on a namespace
// alternation AND a name alternation, composed with the family's fixed selector
// and the request matchers, in that order:
//
//	last_over_time(kube_pod_info{az="zone-a",namespace=~"platform|shop",pod=~"orders-0|redis-0"}[5m])
//
// The two alternations are independent, so a row whose (namespace, name) is not
// one of the caller's pairs can still match — shop/redis-0 above. The caller
// drops those rows (KeepNamePairs) before any later read's restriction is
// computed from them. A single value renders as an equality, several as one
// anchored regex.
//
// label names the identity label and is checked against the family exactly as
// RenderNamesInNamespace checks it: PodLabel for the pod-keyed families,
// ClaimLabel for the claim-keyed ones. ok is false when (q, label) is not such a
// pair or either set holds no non-empty value; restricting only one side would
// read objects the caller did not name. The caller MUST skip the query rather
// than fall back to an unscoped read.
func RenderNamesAcrossNamespaces(q Query, window time.Duration, keys LabelKeys, sel Selector, label string, namespaces, names []string) (string, bool) {
	if !namespaceNameLabel(q, label) {
		return "", false
	}
	ns := normaliseValues(namespaces)
	ps := normaliseValues(names)
	if len(ns) == 0 || len(ps) == 0 {
		return "", false
	}
	var matchers []string
	if fixed := fixedSelector[q]; fixed != "" {
		matchers = append(matchers, fixed)
	}
	if req := sel.render(queryDims[q], keys); req != "" {
		matchers = append(matchers, req)
	}
	matchers = appendMatcher(matchers, NamespaceLabel, ns)
	matchers = appendMatcher(matchers, label, ps)
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		q, strings.Join(matchers, ","), FormatDuration(window)), true
}

// SortedNamePairs is the canonical form of a pair set: the usable pairs — both
// halves non-empty — sorted by (namespace, name) and de-duplicated. A caller that
// compares or chunks pair sets goes through it so the result is a pure function
// of the set. It does not mutate pairs.
func SortedNamePairs(pairs []NamePair) []NamePair {
	out := make([]NamePair, 0, len(pairs))
	for _, p := range pairs {
		if p.Namespace != "" && p.Name != "" {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b NamePair) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return slices.Compact(out)
}

// ChunkNamePairs splits a pair set into chunks whose two rendered alternations —
// namespaces and names, the matcher wrappers and the comma joining them
// included — each fit the byte budget, and neither of which holds more than
// MaxAlternationValues values. The value bound counts DISTINCT namespaces and
// DISTINCT names separately, because the upstream judges each label filter on
// its own; it never counts pairs. A caller that also renders request
// matchers takes RequestMatcherCost off the budget first, as it does for every
// other restriction.
//
// Pairs are sorted by (namespace, name) and packed greedily, so a chunk holds a
// run of neighbouring namespaces and its namespace alternation stays small,
// which also keeps the cross pairs its independent alternations can match few.
// Each chunk is charged for ITS OWN namespaces and names, not the whole set's.
// The split is a pure function of the set and the budget. A non-positive
// budget imposes no byte bound, but the value bound still holds.
//
// The cost is measured on the regex form, which is one byte longer than the
// single-value equality, so a one-pair chunk never reads as smaller than it
// renders. A single pair longer than the budget still gets its own chunk: a
// silently missing pair is a silently missing node, and an over-length query the
// upstream rejects fails visibly. label is the identity label the chunk will be
// rendered with; its wrapper is part of the cost.
func ChunkNamePairs(pairs []NamePair, label string, budget int) [][]NamePair {
	sorted := SortedNamePairs(pairs)
	if len(sorted) == 0 {
		return nil
	}
	// The wrappers and the comma are paid once per chunk: `namespace=~"…",label=~"…"`.
	wrapper := len(NamespaceLabel) + len(`=~""`) + 1 + len(label) + len(`=~""`)
	var (
		out        [][]NamePair
		cur        []NamePair
		namespaces = map[string]struct{}{}
		names      = map[string]struct{}{}
		used       int
	)
	reset := func() {
		cur, used = nil, wrapper
		clear(namespaces)
		clear(names)
	}
	reset()
	// valueCost is the rendered cost of adding v to an alternation that already
	// holds n values: its escaped form plus the `|` separating it from the rest.
	valueCost := func(v string, n int) int {
		c := len(escapeLiteral(regexp.QuoteMeta(v)))
		if n > 0 {
			c++
		}
		return c
	}
	for _, p := range sorted {
		add := 0
		if _, ok := namespaces[p.Namespace]; !ok {
			add += valueCost(p.Namespace, len(namespaces))
		}
		if _, ok := names[p.Name]; !ok {
			add += valueCost(p.Name, len(names))
		}
		_, knownNS := namespaces[p.Namespace]
		_, knownName := names[p.Name]
		overCount := (!knownNS && len(namespaces) >= MaxAlternationValues) ||
			(!knownName && len(names) >= MaxAlternationValues)
		if len(cur) > 0 && ((budget > 0 && used+add > budget) || overCount) {
			out = append(out, cur)
			reset()
			add = valueCost(p.Namespace, 0) + valueCost(p.Name, 0)
		}
		cur = append(cur, p)
		namespaces[p.Namespace] = struct{}{}
		names[p.Name] = struct{}{}
		used += add
	}
	return append(out, cur)
}

// KeepNamePairs keeps the rows whose (namespace, label) is one of pairs, in
// input order — the filter that drops the cross pairs a
// RenderNamesAcrossNamespaces restriction can match. label is the row's identity
// label (PodLabel or ClaimLabel). It allocates a new vector and shares the
// samples, which a reader must not mutate.
func KeepNamePairs(rows model.Vector, label string, pairs []NamePair) model.Vector {
	want := make(map[NamePair]struct{}, len(pairs))
	for _, p := range pairs {
		want[p] = struct{}{}
	}
	out := make(model.Vector, 0, len(rows))
	for _, s := range rows {
		k := NamePair{
			Namespace: string(s.Metric[NamespaceLabel]),
			Name:      string(s.Metric[model.LabelName(label)]),
		}
		if _, ok := want[k]; ok {
			out = append(out, s)
		}
	}
	return out
}
