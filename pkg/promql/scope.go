package promql

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// PodLabel is the label kube-state-metrics identifies a pod by on every
// pod-keyed family.
const PodLabel = "pod"

// NodeLabel is the label kube-state-metrics identifies a Kubernetes node by
// on every node-keyed family.
const NodeLabel = "node"

// PodScopedQueries are the two kube-state-metrics families the storage build
// reads BY REFERENCE: restricted to the pods its body can draw.
var PodScopedQueries = []Query{QPodInfo, QPodOwner}

// NodeScopedQueries are the four kube-state-metrics families the storage
// build reads BY REFERENCE: restricted to the Kubernetes nodes the loaded pods
// are scheduled on, plus the request's `node=` roots.
var NodeScopedQueries = []Query{QNodeInfo, QNodeAddresses, QNodeLabels, QNodeStatusCondition}

// ControllerScopedQueries are the eight kube-state-metrics owner /
// controller-annotation families the storage build reads BY REFERENCE:
// restricted to the controller names the loaded pods' resolved owners name
// (scope-controller-legs-by-reference).
var ControllerScopedQueries = []Query{
	QReplicaSetOwner, QReplicaSetAnnotations,
	QJobOwner, QJobAnnotations,
	QDeploymentAnnotations, QStatefulSetAnnotations, QDaemonSetAnnotations, QCronJobAnnotations,
}

// ReferenceScopedQueries is the complete set of families a by-reference
// topology plan withholds from its first wave: pods, the Kubernetes nodes
// they run on, and their resolved controllers. Every other topology leg is
// read under the request's selector alone.
var ReferenceScopedQueries = slices.Concat(PodScopedQueries, NodeScopedQueries, ControllerScopedQueries)

// scopedLabel names, per scopeable query, the label a data-derived scope
// restricts. The label is part of the table rather than a caller argument so a
// family can only be scoped on the key its reader actually joins on.
var scopedLabel = map[Query]string{
	QPodInfo:  PodLabel,
	QPodOwner: PodLabel,

	QNodeInfo:            NodeLabel,
	QNodeAddresses:       NodeLabel,
	QNodeLabels:          NodeLabel,
	QNodeStatusCondition: NodeLabel,

	QReplicaSetOwner:       "replicaset",
	QReplicaSetAnnotations: "replicaset",
	QJobOwner:              "job_name",
	QJobAnnotations:        "job_name",

	QDeploymentAnnotations:  "deployment",
	QStatefulSetAnnotations: "statefulset",
	QDaemonSetAnnotations:   "daemonset",
	QCronJobAnnotations:     "cronjob",
}

// RenderScoped renders q restricted to a known set of label values, composed
// with — never replacing — the request's own matchers:
//
//	last_over_time(kube_pod_info{az="zone-a",env="prod",pod=~"orders-0|web-0"}[5m])
//
// The order is the query's fixed selector, then the request matchers, then the
// scope, so the rendered string is Render's output with one matcher appended
// (scope-controller-legs-by-reference D2: fixedSelector, defined in
// queries.go, is the ONE table both Render and RenderScoped read, so a scoped
// rendering of a family can never drop the fixed selector its unscoped
// rendering carries — e.g. a scoped kube_job_owner query still carries
// `owner_kind="CronJob",owner_is_controller="true"`). The values are sorted,
// de-duplicated and QuoteMeta-escaped exactly as a request dimension's are, so
// a value containing a regex metacharacter matches itself and nothing else,
// and the string is a pure function of the value set.
//
// Like the QoS volume scope, the restriction is derived from upstream data (and,
// for the storage build, from the request's pod / node roots), not from a
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
	if fixed := fixedSelector[q]; fixed != "" {
		matchers = append(matchers, fixed)
	}
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
	return ChunkScopeWithOverhead(values, budget, 0)
}

// ChunkScopeWithOverhead is ChunkScope for an alternation whose every branch
// carries a fixed extra cost beyond the escaped value — the two bytes of the
// `.*` prefix a suffix-mode token branch renders with. Ignoring it would let a
// chunk overrun the budget by that much per value, which at the default budget
// and a typical token length is a few percent of the headroom the budget leaves
// under `-search.maxQueryLen`; accounting for it keeps the budget a real bound.
//
// overhead is per value and never negative; a negative value is treated as zero.
func ChunkScopeWithOverhead(values []string, budget, overhead int) [][]string {
	if len(values) == 0 {
		return nil
	}
	if budget <= 0 {
		return [][]string{values}
	}
	overhead = max(overhead, 0)
	var (
		out  [][]string
		cur  []string
		used int
	)
	for _, v := range values {
		// The rendered cost of this value: its escaped form, its fixed branch
		// overhead, plus the `|` separator it needs once it is not first in
		// the chunk.
		base := len(escapeLiteral(regexp.QuoteMeta(v))) + overhead
		cost := base
		if len(cur) > 0 {
			cost++
		}
		if len(cur) > 0 && used+cost > budget {
			out = append(out, cur)
			cur, used = nil, 0
			cost = base
		}
		cur = append(cur, v)
		used += cost
	}
	return append(out, cur)
}
