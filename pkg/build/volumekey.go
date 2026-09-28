package build

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// VolumeKeyRule is one ordered rewrite step turning a PersistentVolume name
// into the match token. Every match of Pattern is replaced by Replacement.
type VolumeKeyRule struct {
	Pattern     string
	Replacement string
}

// DefaultVolumeKeyRules is the rewrite applied when the operator configures
// none: replace `-` with `_`, which is exactly the transformation a
// CSI provisioner performs to make a `pvc-<uuid>` PV name a legal ONTAP volume
// name. It deliberately does NOT prepend a storage prefix — the prefix is
// per-backend configurable in the provisioner, and a suffix comparison does
// not need to know it.
func DefaultVolumeKeyRules() []VolumeKeyRule {
	return []VolumeKeyRule{{Pattern: "-", Replacement: "_"}}
}

// VolumeKeyRewriter derives a claim's match token from its bound PV name and
// answers whether a Harvest `volume` value ends with that token. The join is
// suffix-only: a provisioner names a FlexVol by prefixing the transformed PV
// name, so the comparison resolves it without the deployment declaring the
// prefix, rejects a derived volume whose name extends past the PV name
// (`trident_pvc_x_clone`), and is the one comparison a storage build can both
// render as an anchored alternation branch and invert from a FlexVol name
// back to candidate PersistentVolume names. It is immutable after construction
// and safe for concurrent use.
type VolumeKeyRewriter struct {
	rules []compiledVolumeKeyRule
}

type compiledVolumeKeyRule struct {
	re          *regexp.Regexp
	replacement string
}

// NewVolumeKeyRewriter compiles the ordered rewrite rules. An uncompilable
// pattern is an error, never a silent fallback to the defaults: a typo would
// otherwise resolve a different estate than the operator declared.
//
// A NIL rules slice means "the operator configured none" and adopts
// DefaultVolumeKeyRules; a non-nil EMPTY slice is an explicit identity rewrite
// (the token is the PV name verbatim).
func NewVolumeKeyRewriter(rules []VolumeKeyRule) (*VolumeKeyRewriter, error) {
	if rules == nil {
		rules = DefaultVolumeKeyRules()
	}
	out := &VolumeKeyRewriter{rules: make([]compiledVolumeKeyRule, 0, len(rules))}
	for i, r := range rules {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("volume key rewrite rule %d: pattern %q does not compile: %w",
				i+1, r.Pattern, err)
		}
		out.rules = append(out.rules, compiledVolumeKeyRule{re: re, replacement: r.Replacement})
	}
	return out, nil
}

// defaultVolumeKeyRewriter is the rewriter a zero build.Options resolves to.
// It cannot fail — the defaults are compiled from constants.
func defaultVolumeKeyRewriter() *VolumeKeyRewriter {
	rw, err := NewVolumeKeyRewriter(nil)
	if err != nil {
		panic("build: default volume key rewriter does not compile: " + err.Error())
	}
	return rw
}

// DefaultQoSScopeBatchBytes bounds one scoped QoS query's rendered `volume`
// alternation when the operator configures no budget. It sits comfortably under
// the common VictoriaMetrics `-search.maxQueryLen` default, leaving room for the
// metric name and the surrounding function call.
const DefaultQoSScopeBatchBytes = 8192

// volumeKey resolves the rewriter this build uses, adopting the defaults for a
// zero Options.
func (o Options) volumeKey() *VolumeKeyRewriter {
	if o.VolumeKey != nil {
		return o.VolumeKey
	}
	return defaultVolumeKeyRewriter()
}

// qosScopeBatchBytes resolves the chunk budget, adopting the default for a
// zero or negative Options value.
func (o Options) qosScopeBatchBytes() int {
	if o.QoSScopeBatchBytes > 0 {
		return o.QoSScopeBatchBytes
	}
	return DefaultQoSScopeBatchBytes
}

// token derives the match token from a PersistentVolume name by applying every
// rule in declaration order. An empty PV name derives no token.
func (r *VolumeKeyRewriter) token(pvName string) string {
	if pvName == "" {
		return ""
	}
	for _, rule := range r.rules {
		pvName = rule.re.ReplaceAllString(pvName, rule.replacement)
	}
	return pvName
}

// matches reports whether a derived token is a suffix of a Harvest `volume`
// value. A FlexVol named exactly the token matches, because a value ends with
// itself. It is the single-pair form of the predicate the volumeMatcher index
// answers in bulk; the two MUST agree.
func (r *VolumeKeyRewriter) matches(token, volume string) bool {
	if token == "" {
		return false
	}
	return strings.HasSuffix(volume, token)
}

// volumeMatcher answers, for one Harvest `volume` value, which of a build's
// claims match it. It is built once per build and iterated series-first, so the
// cost of the whole join is one pass over the Harvest vector rather than one
// pass per claim.
//
// Suffix resolves through a hash index and costs O(volumes): tokens are
// bucketed by byte length, and for each series only the trailing len(token)
// bytes are looked up per distinct length (one length in practice, since every
// `pvc-<uuid>` derives the same length).
type volumeMatcher struct {
	tokens []string // per claim index; "" for a claim that derived none

	byToken map[string][]int
	lengths []int // distinct token byte lengths, ascending
}

// newVolumeMatcher indexes the claims by their derived tokens.
func newVolumeMatcher(rw *VolumeKeyRewriter, claims []pvcVolume) *volumeMatcher {
	m := &volumeMatcher{
		tokens:  make([]string, len(claims)),
		byToken: make(map[string][]int, len(claims)),
	}
	seenLen := map[int]bool{}
	for i, c := range claims {
		t := rw.token(c.volumeName)
		m.tokens[i] = t
		if t == "" {
			continue
		}
		m.byToken[t] = append(m.byToken[t], i)
		if !seenLen[len(t)] {
			seenLen[len(t)] = true
			m.lengths = append(m.lengths, len(t))
		}
	}
	slices.Sort(m.lengths)
	return m
}

// match appends the indexes of every claim whose token is a suffix of this
// `volume` value to dst and returns it. dst is reused across series to keep
// the pass allocation-free.
func (m *volumeMatcher) match(volume string, dst []int) []int {
	dst = dst[:0]
	if volume == "" {
		return dst
	}
	for _, l := range m.lengths {
		if l > len(volume) {
			break // lengths ascend; no longer token can be a suffix
		}
		dst = append(dst, m.byToken[volume[len(volume)-l:]]...)
	}
	return dst
}

// any reports whether ANY claim's token is a suffix of this `volume` value.
// It is the scope-computation form: it needs no claim identity and can stop
// at the first hit.
func (m *volumeMatcher) any(volume string) bool {
	if volume == "" {
		return false
	}
	for _, l := range m.lengths {
		if l > len(volume) {
			break
		}
		if len(m.byToken[volume[len(volume)-l:]]) > 0 {
			return true
		}
	}
	return false
}

// qosVolumeScope is the set of Harvest `volume` values worth issuing a QoS
// workload query for: exactly those the loaded claims' derived tokens match.
// The result is sorted and de-duplicated, so the chunking that consumes it — and
// therefore the merged QoS vector — is a pure function of the upstream data.
//
// PV names are read straight off the raw kube_persistentvolumeclaim_info vector
// rather than off resolved PVC entities. That is a deliberate SUPERSET of the
// entity claim list (an unbound PVC contributes a name no claim will later
// join): the scope decides only what is FETCHED, never what joins, and the
// authoritative join stays in resolveNetAppStorage.
// claimVolumeMatcher turns a kube_persistentvolumeclaim_info vector into the
// claim list and the matcher every Harvest read joins through: one pvcVolume
// per DISTINCT bound PersistentVolume name, and a volumeMatcher indexing their
// derived tokens.
//
// It is shared by qosVolumeScope and by the rooted volume-label read's
// phase-2 scope so the two cannot disagree about what a claim is or what token
// it derives. They ask different questions of the result — which FlexVol names
// matched, versus which CLAIMS matched — but a claim admitted by one and not
// the other would mean a volume fetched for and then not joined, or the
// reverse. Keeping the step in one place is what makes that structural rather
// than a promise in two comments.
//
// The claim set is deliberately every bound claim the read loaded, which is a
// SUPERSET of the claims the parse binds to a pod: an unmounted claim costs a
// token and is dropped at projection exactly as it always has been.
func claimVolumeMatcher(pvcInfo model.Vector, rw *VolumeKeyRewriter) ([]pvcVolume, *volumeMatcher) {
	claims := make([]pvcVolume, 0, len(pvcInfo))
	seenPV := make(map[string]bool, len(pvcInfo))
	for _, s := range pvcInfo {
		vn := string(s.Metric["volumename"])
		if vn == "" || seenPV[vn] {
			continue
		}
		seenPV[vn] = true
		claims = append(claims, pvcVolume{volumeName: vn})
	}
	if len(claims) == 0 {
		return nil, nil
	}
	return claims, newVolumeMatcher(rw, claims)
}

func qosVolumeScope(pvcInfo, volumeLabels model.Vector, rw *VolumeKeyRewriter) []string {
	if len(pvcInfo) == 0 || len(volumeLabels) == 0 {
		return nil
	}
	_, m := claimVolumeMatcher(pvcInfo, rw)
	if m == nil {
		return nil
	}
	seenVol := make(map[string]bool, len(volumeLabels))
	out := make([]string, 0, len(volumeLabels))
	for _, s := range volumeLabels {
		v := string(s.Metric[promql.HarvestVolumeLabel])
		if v == "" || seenVol[v] || !m.any(v) {
			continue
		}
		seenVol[v] = true
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}
