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
// read under the request's selector alone — except ClaimScopedQueries, which a
// hub-mode storage build ALSO withholds.
var ReferenceScopedQueries = slices.Concat(PodScopedQueries, NodeScopedQueries, ControllerScopedQueries)

// VolumeNameLabel is the kube_persistentvolumeclaim_info label naming the
// PersistentVolume a claim is bound to.
const VolumeNameLabel = "volumename"

// ClaimLabel is the label kube-state-metrics identifies a claim by on every
// claim-keyed family, and the one the kubelet volume-stats pair carries.
const ClaimLabel = "persistentvolumeclaim"

// ClaimScopedQueries are the five claim-keyed families a hub-mode
// /v1/storage-graph build reads BY REFERENCE instead of in its first wave
// (read-storage-roots-through-volume-hub): kube_persistentvolumeclaim_info
// restricted on `volumename` to the PersistentVolume names the rooted Harvest
// rows yield, and the other four restricted on `persistentvolumeclaim` to the
// claims that read returned. Outside hub mode they stay first-wave legs, which
// is why they are not part of ReferenceScopedQueries.
var ClaimScopedQueries = []Query{
	QPVCInfo, QPVCBindings, QPVCAnnotations, QKubeletVolumeUsedBytes, QKubeletVolumeCapacityBytes,
}

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

	// The claim-binding family is scoped on `persistentvolumeclaim` only. An
	// exporter labelling it with `claim_name` alone is outside the documented
	// label contract, and a hub-mode build draws no path for it.
	QPVCInfo:                    VolumeNameLabel,
	QPVCBindings:                ClaimLabel,
	QPVCAnnotations:             ClaimLabel,
	QKubeletVolumeUsedBytes:     ClaimLabel,
	QKubeletVolumeCapacityBytes: ClaimLabel,
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
	return RenderOnLabel(q, window, keys, sel, label, values)
}

// RenderOnLabel renders q with its fixed selector and the request matchers,
// then one restriction on label. It is RenderScoped for a label other than the
// family's default scope key — kube_persistentvolumeclaim_info is scoped on
// volumename by the storage seed and on persistentvolumeclaim by the claim
// expansion. ok is false when label or values is empty. The caller MUST then
// skip the query rather than fall back to an unscoped read.
func RenderOnLabel(q Query, window time.Duration, keys LabelKeys, sel Selector, label string, values []string) (string, bool) {
	if label == "" {
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

// NamespaceLabel is the label kube-state-metrics identifies a namespace by.
const NamespaceLabel = "namespace"

// RenderPodInfoByNode renders kube_pod_info restricted on the Kubernetes node
// the pod is scheduled on, composed with the family's fixed selector and the
// request matchers:
//
//	last_over_time(kube_pod_info{az="zone-a",env="prod",node=~"worker-1|worker-2"}[5m])
//
// This is a different key from RenderScoped(QPodInfo), which restricts `pod`.
// A node seed reads pods by the node they run on; the incarnation completion
// that follows is keyed by (namespace, pod) — see RenderNamesInNamespace.
//
// ok is false when nodes holds no non-empty value. The caller MUST skip the
// query rather than fall back to an unscoped read.
func RenderPodInfoByNode(window time.Duration, keys LabelKeys, sel Selector, nodes []string) (string, bool) {
	return RenderOnLabel(QPodInfo, window, keys, sel, NodeLabel, nodes)
}

// PodNamespaceScopedQueries are the pod-keyed families RenderNamesInNamespace
// accepts under PodLabel: every family a storage build reads for a set of known
// pods.
var PodNamespaceScopedQueries = []Query{QPodInfo, QPodOwner, QPVCBindings}

// ClaimNamespaceScopedQueries are the claim-keyed families
// RenderNamesInNamespace accepts under ClaimLabel: the one family a claim root
// seeds from, read one namespace at a time.
var ClaimNamespaceScopedQueries = []Query{QPVCInfo}

// namespaceNameLabel reports whether q is keyed by (namespace, label) — the
// pairs RenderNamesInNamespace can render.
func namespaceNameLabel(q Query, label string) bool {
	switch label {
	case PodLabel:
		return slices.Contains(PodNamespaceScopedQueries, q)
	case ClaimLabel:
		return slices.Contains(ClaimNamespaceScopedQueries, q)
	}
	return false
}

// RenderNamesInNamespace renders a family restricted to objects of ONE
// namespace, named by the family's own identity label, composed with the
// family's fixed selector and the request matchers:
//
//	last_over_time(kube_pod_info{az="zone-a",namespace="shop",pod=~"orders-0|web-0"}[5m])
//	last_over_time(kube_persistentvolumeclaim_info{namespace="shop",persistentvolumeclaim=~"cache|orders-data"}[5m])
//
// An object is identified by its namespace and name, never by its name alone,
// so a caller holding (namespace, name) pairs issues one query per namespace.
// The query then matches exactly those pairs: a same-named object in another
// namespace is never read. Two independent alternations would read every cross
// pair.
//
// The namespace equality is always rendered, including when namespace is
// empty, so a query never spans namespaces. A request namespace matcher is
// composed ahead of it, never replaced.
//
// label names the identity label and is checked against the family — PodLabel
// for the pod-keyed families, ClaimLabel for claim info — so a family can only
// be restricted on the key its reader joins on. ok is false when (q, label) is
// not such a pair or names holds no non-empty value. The caller MUST skip the
// query rather than fall back to an unscoped read.
func RenderNamesInNamespace(q Query, window time.Duration, keys LabelKeys, sel Selector, namespace, label string, names []string) (string, bool) {
	if !namespaceNameLabel(q, label) {
		return "", false
	}
	ns := normaliseValues(names)
	if len(ns) == 0 {
		return "", false
	}
	var matchers []string
	if fixed := fixedSelector[q]; fixed != "" {
		matchers = append(matchers, fixed)
	}
	if req := sel.render(queryDims[q], keys); req != "" {
		matchers = append(matchers, req)
	}
	matchers = append(matchers, namespaceEquality(namespace))
	matchers = appendMatcher(matchers, label, ns)
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		q, strings.Join(matchers, ","), FormatDuration(window)), true
}

// NamespaceEqualityCost is the rendered length of the namespace equality
// RenderNamesInNamespace repeats in every chunk of one namespace's names, plus
// its separator. A caller chunking the name alternation takes it off the byte
// budget so the budget bounds the whole selector it sends.
func NamespaceEqualityCost(namespace string) int {
	return len(namespaceEquality(namespace)) + 1
}

func namespaceEquality(namespace string) string {
	return NamespaceLabel + `="` + escapeLiteral(namespace) + `"`
}

// RenderClaimBindingsByPod renders the claim-binding family restricted on
// both namespace and pod:
//
//	last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info{namespace=~"platform|shop",pod=~"orders-0|redis-0"}[5m])
//
// The two alternations are independent, so a row whose (namespace, pod) is
// not one of the caller's refs can still match; the caller drops those rows.
// A request namespace matcher is composed ahead of the scope, never replaced.
//
// ok is false when either set holds no non-empty value. Restricting only one
// side would read bindings the caller did not name. The caller MUST skip the
// query rather than fall back to an unscoped read.
func RenderClaimBindingsByPod(window time.Duration, keys LabelKeys, sel Selector, namespaces, pods []string) (string, bool) {
	ns := normaliseValues(namespaces)
	ps := normaliseValues(pods)
	if len(ns) == 0 || len(ps) == 0 {
		return "", false
	}
	var matchers []string
	if fixed := fixedSelector[QPVCBindings]; fixed != "" {
		matchers = append(matchers, fixed)
	}
	if req := sel.render(queryDims[QPVCBindings], keys); req != "" {
		matchers = append(matchers, req)
	}
	matchers = appendMatcher(matchers, NamespaceLabel, ns)
	matchers = appendMatcher(matchers, PodLabel, ps)
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		QPVCBindings, strings.Join(matchers, ","), FormatDuration(window)), true
}
