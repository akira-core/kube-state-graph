package promql

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// volumeLabelsClusterLabel and volumeLabelsAggrLabel are the STOCK Harvest labels
// of the volume-object family a storage-side root restricts. `cluster` is the
// ONTAP cluster name — never a Kubernetes cluster — and `aggr` the containing
// aggregate. They are the request's `ontap_cluster=` and `aggr=` values verbatim:
// restricting by them is an identity mapping, not a derivation.
const (
	// VolumeLabelsClusterLabel is exported because a caller chunking the rooted
	// restriction has to measure the rendered cost of this exact matcher.
	VolumeLabelsClusterLabel = "cluster"
	volumeLabelsAggrLabel    = "aggr"
	volumeLabelsSVMLabel     = "svm"
)

// RenderVolumeLabelsRooted renders the volume-label topology family restricted
// to the ONTAP clusters and aggregates a /v1/storage-graph request roots at
// (storage-graph-api, "Storage-side roots narrow the Harvest topology read"):
//
//	last_over_time(volume_labels{cluster="ontap-prod",aggr=~"aggr1|aggr2"}[5m])
//
// The two matchers are AND-combined in ONE selector, in that order, and that is
// the projection's own rule rather than a simplification of it: an `aggr=` root
// names an aggregate only WITHIN the `ontap_cluster=` values, so an aggregate of
// that name on another filer is not a root and its volumes need not be read. A
// caller holding only one value set passes nil for the other.
//
// Like RenderQoSVolumeScoped and RenderScoped the restriction is derived from
// data the request carries rather than from a selector-level dimension:
// queryDims is unchanged and Selector.Reaches does not see it. The request's
// own matchers — az and env, the only ones queryDims grants Harvest — render
// FIRST, exactly as on the unrestricted read, so the restriction narrows the
// request's zone rather than replacing it. Values are sorted, de-duplicated
// and QuoteMeta-escaped, so the string is a pure function of the value SETS.
//
// ok is false when both sets are empty after normalisation. The caller MUST then
// issue the unrestricted read instead — an empty restriction is "no root", never
// "match nothing".
func RenderVolumeLabelsRooted(window time.Duration, keys LabelKeys, sel Selector, clusters, aggrs []string) (string, bool) {
	var restriction []string
	restriction = appendMatcher(restriction, VolumeLabelsClusterLabel, clusters)
	restriction = appendMatcher(restriction, volumeLabelsAggrLabel, aggrs)
	if len(restriction) == 0 {
		return "", false
	}
	matchers := append(requestMatchers(QVolumeLabels, keys, sel), restriction...)
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		QVolumeLabels, strings.Join(matchers, ","), FormatDuration(window)), true
}

// RenderVolumeLabelsSVMRooted renders the volume-label topology family
// restricted to the SVMs a /v1/storage-graph request roots at, narrowed by its
// ONTAP-cluster roots when it carries any:
//
//	last_over_time(volume_labels{cluster="ontap-prod",svm=~"svm_a|svm_b"}[5m])
//
// It is the SVM group of phase 1, issued beside RenderVolumeLabelsRooted's
// aggregate group when a request carries both roots: the projection UNIONS
// `aggr=` with `svm=` and narrows both by `ontap_cluster=`, so the two groups
// mirror it exactly. An SVM restriction returns only that SVM's volumes on each
// aggregate it touches, which is why the build follows it with an
// owner-completion read (RenderVolumeLabelsOwnerCompletion).
//
// ok is false when svms holds no non-empty value — an ONTAP-cluster set alone
// is RenderVolumeLabelsRooted's shape, never this one's.
func RenderVolumeLabelsSVMRooted(window time.Duration, keys LabelKeys, sel Selector, clusters, svms []string) (string, bool) {
	if len(normaliseValues(svms)) == 0 {
		return "", false
	}
	matchers := requestMatchers(QVolumeLabels, keys, sel)
	matchers = appendMatcher(matchers, VolumeLabelsClusterLabel, clusters)
	matchers = appendMatcher(matchers, volumeLabelsSVMLabel, svms)
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		QVolumeLabels, strings.Join(matchers, ","), FormatDuration(window)), true
}

// RenderVolumeLabelsOwnerCompletion renders the volume-label family restricted
// to a set of aggregates of ONE ONTAP cluster:
//
//	last_over_time(volume_labels{cluster="ontap-prod",aggr=~"aggr03|aggr07"}[5m])
//
// It is the owner-completion read. The owning controller of an aggregate is a
// vote over EVERY volume-label series of that aggregate, and an SVM-restricted
// phase-1 query returns only the rooted SVM's share of each aggregate it
// touches; this re-reads those aggregates whole so the vote runs over the same
// population an unrestricted read gives it.
//
// The cluster is always rendered as an exact equality — even an empty one,
// which matches a series carrying no `cluster` label — because an aggregate
// name is unique only within its filer, and dropping the matcher would read
// every filer's aggregate of that name.
//
// ok is false when aggrs holds no non-empty value.
func RenderVolumeLabelsOwnerCompletion(window time.Duration, keys LabelKeys, sel Selector, cluster string, aggrs []string) (string, bool) {
	vals := normaliseValues(aggrs)
	if len(vals) == 0 {
		return "", false
	}
	matchers := append(requestMatchers(QVolumeLabels, keys, sel),
		VolumeLabelsClusterLabel+`="`+escapeLiteral(cluster)+`"`)
	matchers = appendMatcher(matchers, volumeLabelsAggrLabel, vals)
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		QVolumeLabels, strings.Join(matchers, ","), FormatDuration(window)), true
}

// OwnerCompletionClusterCost is the rendered byte length of the cluster
// equality RenderVolumeLabelsOwnerCompletion repeats in every chunk of one
// cluster's aggregate set, the separating comma included. A caller chunking the
// aggregates takes it off the budget first, exactly as the phase-1 read charges
// its repeated cluster alternation.
func OwnerCompletionClusterCost(cluster string) int {
	return len(VolumeLabelsClusterLabel+`="`+escapeLiteral(cluster)+`"`) + 1
}

// VolumeTokenBranchOverhead is the fixed per-token cost, in rendered bytes, of a
// suffix-mode branch: the `.*` RenderVolumeLabelsTokenScoped prefixes. Pass it to
// ChunkScopeWithOverhead so a token chunk stays inside the byte budget.
const VolumeTokenBranchOverhead = len(".*")

// RequestMatcherCost is the rendered byte length of the request matchers query
// q carries under sel — the fragment every chunk of a restricted read repeats —
// the separating comma included. A caller chunking a restriction takes it off
// its byte budget, exactly as it takes off a repeated root matcher. Zero when
// sel renders nothing on q.
func RequestMatcherCost(q Query, keys LabelKeys, sel Selector) int {
	m := sel.render(queryDims[q], keys)
	if m == "" {
		return 0
	}
	return len(m) + 1
}

// requestMatchers is the request's own matcher fragment for q, as the leading
// matchers of a restricted selector — nil when sel renders nothing on q.
func requestMatchers(q Query, keys LabelKeys, sel Selector) []string {
	if m := sel.render(queryDims[q], keys); m != "" {
		return []string{m}
	}
	return nil
}

// MatcherCost is the rendered byte length of the matcher appendMatcher produces
// for key and values — escaping and the `=~"…"` wrapper included.
//
// A caller chunking ONE alternation of a query that also carries a second,
// repeated matcher must take this off its budget, or the budget bounds only
// part of what it renders. Measuring the rendered form rather than the raw
// values is what makes it a bound rather than an estimate: `ontap.prod`
// becomes `ontap\\.prod` on the wire, three bytes longer per metacharacter.
//
// An empty value set costs nothing, because appendMatcher renders nothing.
func MatcherCost(key string, values []string) int {
	m := appendMatcher(nil, key, values)
	if len(m) == 0 {
		return 0
	}
	return len(m[0])
}

// RenderVolumeLabelsTokenScoped renders the volume-label family restricted on
// `volume` to a set of derived match tokens — phase 2 of the rooted read, which
// recovers each phase-1-matched claim's WHOLE candidate set:
//
//	last_over_time(volume_labels{volume=~".*pvc_a|.*pvc_b"}[5m])   // suffix
//	last_over_time(volume_labels{volume="pvc_a"}[5m])              // exact
//
// The direction is FORWARD. A token is what the volume-key derivation computes
// from a PersistentVolume name the build already holds, and the branch expresses
// the comparison the Go-side matcher performs; nothing here recovers a PV name
// from a FlexVol name, which is the inversion the storage-join design rules out.
// The `.*` a suffix branch renders is exactly the provisioner prefix that
// derivation cannot know — PromQL anchors `=~` as ^(?:...)$, so `.*tok` matches
// precisely the volumes that END with tok.
//
// suffix selects the branch shape: true renders `.*<token>` (always the regex
// form, since the prefix needs one), false renders the token alone as an exact
// match. Tokens are QuoteMeta-escaped, so a metacharacter matches itself; the
// `.*` is deliberately NOT escaped.
//
// excludeClusters, when non-empty, renders a NEGATIVE `cluster` matcher beside
// the token alternation. It is for the one shape where phase 1 already returned
// every candidate on the clusters it names — a restriction by ONTAP cluster
// alone, which reads those filers whole — so the only candidates left to
// recover are the ones somewhere else. Without it that request re-scans every
// volume it has already read, and a `.*`-prefixed regex cannot use a prefix
// index, so the re-scan is the expensive kind. It MUST NOT be passed when the
// restriction names an aggregate: phase 1 then read only part of its cluster,
// and a candidate on another aggregate of the SAME cluster still has to be
// recovered.
//
// ok is false when tokens holds no non-empty value.
func RenderVolumeLabelsTokenScoped(window time.Duration, keys LabelKeys, sel Selector, tokens []string, suffix bool, excludeClusters []string) (string, bool) {
	vals := normaliseValues(tokens)
	if len(vals) == 0 {
		return "", false
	}
	matchers := requestMatchers(QVolumeLabels, keys, sel)
	if excl := normaliseValues(excludeClusters); len(excl) > 0 {
		alts := make([]string, len(excl))
		for i, v := range excl {
			alts[i] = escapeLiteral(regexp.QuoteMeta(v))
		}
		matchers = append(matchers, VolumeLabelsClusterLabel+`!~"`+strings.Join(alts, "|")+`"`)
	}
	if suffix {
		alts := make([]string, len(vals))
		for i, v := range vals {
			alts[i] = ".*" + escapeLiteral(regexp.QuoteMeta(v))
		}
		matchers = append(matchers, HarvestVolumeLabel+`=~"`+strings.Join(alts, "|")+`"`)
	} else {
		matchers = appendMatcher(matchers, HarvestVolumeLabel, vals)
	}
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		QVolumeLabels, strings.Join(matchers, ","), FormatDuration(window)), true
}
