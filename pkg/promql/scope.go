package promql

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// PodLabel is the label kube-state-metrics identifies a pod by on every
// pod-keyed family.
const PodLabel = "pod"

// PodScopedQueries are the two kube-state-metrics families the storage build
// reads BY REFERENCE: restricted to the pods its body can draw. Every other
// topology leg is read under the request's selector alone.
var PodScopedQueries = []Query{QPodInfo, QPodOwner}

// scopedLabel names, per scopeable query, the label a data-derived scope
// restricts. The label is part of the table rather than a caller argument so a
// family can only be scoped on the key its reader actually joins on.
var scopedLabel = map[Query]string{
	QPodInfo:  PodLabel,
	QPodOwner: PodLabel,
}

// RenderScoped renders q restricted to a known set of label values, composed
// with — never replacing — the request's own matchers:
//
//	last_over_time(kube_pod_info{az="zone-a",env="prod",pod=~"orders-0|web-0"}[5m])
//
// The order is the query's fixed selector, then the request matchers, then the
// scope, so the rendered string is Render's output with one matcher appended.
// The values are sorted, de-duplicated and QuoteMeta-escaped exactly as a
// request dimension's are, so a value containing a regex metacharacter matches
// itself and nothing else, and the string is a pure function of the value set.
//
// Like the QoS volume scope, the restriction is derived from upstream data (and,
// for the storage build, from the request's pod roots), not from a
// selector-level dimension: queryDims is unchanged and Selector.Reaches does not
// see it.
//
// ok is false when values holds no non-empty value or q is not scopeable. The
// caller MUST then skip the query rather than fall back to an unscoped read.
func RenderScoped(q Query, window time.Duration, keys LabelKeys, sel Selector, values []string) (string, bool) {
	label, scopeable := scopedLabel[q]
	if !scopeable {
		return "", false
	}
	vals := normaliseValues(values)
	if len(vals) == 0 {
		return "", false
	}
	var matchers []string
	if req := sel.render(queryDims[q], keys); req != "" {
		matchers = append(matchers, req)
	}
	matchers = appendMatcher(matchers, label, vals)
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		q, strings.Join(matchers, ","), FormatDuration(window)), true
}

// ChunkScope splits a sorted, de-duplicated scope into chunks whose rendered
// alternation fits the byte budget, so a large scope is read through several
// narrow queries instead of one query the upstream would reject for length
// (`-search.maxQueryLen`).
//
// The split is a pure function of the input slice and the budget, so which
// values share a chunk is deterministic across rebuilds.
//
// A single value longer than the budget still gets its own chunk rather than
// being dropped: a silently missing value is a silently missing node or
// measurement, and an over-length query the upstream rejects fails visibly.
func ChunkScope(values []string, budget int) [][]string {
	if len(values) == 0 {
		return nil
	}
	if budget <= 0 {
		return [][]string{values}
	}
	var (
		out  [][]string
		cur  []string
		used int
	)
	for _, v := range values {
		// The rendered cost of this value: its escaped form plus the `|`
		// separator it needs once it is not first in the chunk.
		cost := len(escapeLiteral(regexp.QuoteMeta(v)))
		if len(cur) > 0 {
			cost++
		}
		if len(cur) > 0 && used+cost > budget {
			out = append(out, cur)
			cur, used = nil, 0
			cost = len(escapeLiteral(regexp.QuoteMeta(v)))
		}
		cur = append(cur, v)
		used += cost
	}
	return append(out, cur)
}
