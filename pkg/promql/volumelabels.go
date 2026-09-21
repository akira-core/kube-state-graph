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
// queryDims is unchanged, Selector.Reaches does not see it, and Harvest still
// takes no request-scoped matcher. Values are sorted, de-duplicated and
// QuoteMeta-escaped, so the string is a pure function of the two value SETS.
//
// ok is false when both sets are empty after normalisation. The caller MUST then
// issue the unrestricted read instead — an empty restriction is "no root", never
// "match nothing".
func RenderVolumeLabelsRooted(window time.Duration, clusters, aggrs []string) (string, bool) {
	var matchers []string
	matchers = appendMatcher(matchers, VolumeLabelsClusterLabel, clusters)
	matchers = appendMatcher(matchers, volumeLabelsAggrLabel, aggrs)
	if len(matchers) == 0 {
		return "", false
	}
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		QVolumeLabels, strings.Join(matchers, ","), FormatDuration(window)), true
}

// VolumeTokenBranchOverhead is the fixed per-token cost, in rendered bytes, of a
// suffix-mode branch: the `.*` RenderVolumeLabelsTokenScoped prefixes. Pass it to
// ChunkScopeWithOverhead so a token chunk stays inside the byte budget.
const VolumeTokenBranchOverhead = len(".*")

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
func RenderVolumeLabelsTokenScoped(window time.Duration, tokens []string, suffix bool, excludeClusters []string) (string, bool) {
	vals := normaliseValues(tokens)
	if len(vals) == 0 {
		return "", false
	}
	var matchers []string
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
