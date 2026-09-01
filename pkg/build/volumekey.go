package build

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// VolumeMatchMode selects how a claim's derived match token is compared against
// the stock `volume` label of a Harvest series (the ONTAP FlexVol name).
//
// ONTAP volume names admit only letters, digits and `_`, so a `volume` value
// can never equal a `pvc-<uuid>` PersistentVolume name and the two are never
// compared for equality directly: the PV name is first rewritten into a match
// token (see VolumeKeyRewriter), and the token is compared under one of these
// modes.
type VolumeMatchMode string

const (
	// VolumeMatchExact requires the token to equal the whole `volume` value.
	VolumeMatchExact VolumeMatchMode = "exact"
	// VolumeMatchSuffix requires `volume` to END with the token. This is the
	// default: a provisioner names a FlexVol by PREFIXING the transformed PV
	// name, so a suffix match resolves it without the deployment declaring the
	// prefix, while still rejecting a derived volume whose name extends past
	// the PV name (`trident_pvc_x_clone`), which VolumeMatchContains accepts.
	VolumeMatchSuffix VolumeMatchMode = "suffix"
	// VolumeMatchContains requires `volume` to contain the token anywhere.
	VolumeMatchContains VolumeMatchMode = "contains"
	// VolumeMatchRegex compiles the token itself as a regular expression and
	// matches it against `volume`.
	VolumeMatchRegex VolumeMatchMode = "regex"
)

// DefaultVolumeMatchMode is the mode used when the operator configures none.
const DefaultVolumeMatchMode = VolumeMatchSuffix

// VolumeMatchModes lists every accepted mode, in the order the operator
// reference documents them.
var VolumeMatchModes = []VolumeMatchMode{
	VolumeMatchExact, VolumeMatchSuffix, VolumeMatchContains, VolumeMatchRegex,
}

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
// per-backend configurable in the provisioner, and DefaultVolumeMatchMode does
// not need to know it.
func DefaultVolumeKeyRules() []VolumeKeyRule {
	return []VolumeKeyRule{{Pattern: "-", Replacement: "_"}}
}

// VolumeKeyRewriter derives a claim's match token from its bound PV name and
// answers whether that token matches a Harvest `volume` value. It is immutable
// after construction and safe for concurrent use.
type VolumeKeyRewriter struct {
	rules []compiledVolumeKeyRule
	mode  VolumeMatchMode
}

type compiledVolumeKeyRule struct {
	re          *regexp.Regexp
	replacement string
}

// NewVolumeKeyRewriter compiles the ordered rewrite rules and validates the
// match mode. An uncompilable pattern or an unrecognised mode is an error, never
// a silent fallback to the defaults: a typo would otherwise resolve a different
// estate than the operator declared.
//
// A NIL rules slice means "the operator configured none" and adopts
// DefaultVolumeKeyRules; a non-nil EMPTY slice is an explicit identity rewrite
// (the token is the PV name verbatim). An empty mode adopts
// DefaultVolumeMatchMode.
func NewVolumeKeyRewriter(rules []VolumeKeyRule, mode VolumeMatchMode) (*VolumeKeyRewriter, error) {
	if rules == nil {
		rules = DefaultVolumeKeyRules()
	}
	if mode == "" {
		mode = DefaultVolumeMatchMode
	}
	if !validVolumeMatchMode(mode) {
		return nil, fmt.Errorf("unknown volume match mode %q (want one of %s)",
			mode, joinVolumeMatchModes())
	}
	out := &VolumeKeyRewriter{mode: mode, rules: make([]compiledVolumeKeyRule, 0, len(rules))}
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

func validVolumeMatchMode(m VolumeMatchMode) bool {
	return slices.Contains(VolumeMatchModes, m)
}

func joinVolumeMatchModes() string {
	s := make([]string, len(VolumeMatchModes))
	for i, m := range VolumeMatchModes {
		s[i] = string(m)
	}
	return strings.Join(s, ", ")
}

// defaultVolumeKeyRewriter is the rewriter a zero build.Options resolves to.
// It cannot fail — the defaults are compiled from constants.
func defaultVolumeKeyRewriter() *VolumeKeyRewriter {
	rw, err := NewVolumeKeyRewriter(nil, "")
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

// matches reports whether a derived token matches a Harvest `volume` value
// under the configured mode. It is the single-pair form of the predicate the
// volumeMatcher index answers in bulk; the two MUST agree.
func (r *VolumeKeyRewriter) matches(token, volume string) bool {
	if token == "" {
		return false
	}
	switch r.mode {
	case VolumeMatchExact:
		return token == volume
	case VolumeMatchSuffix:
		return strings.HasSuffix(volume, token)
	case VolumeMatchContains:
		return strings.Contains(volume, token)
	case VolumeMatchRegex:
		re, err := regexp.Compile(token)
		if err != nil {
			return false
		}
		return re.MatchString(volume)
	}
	return false
}

// volumeMatcher answers, for one Harvest `volume` value, which of a build's
// claims match it. It is built once per build and iterated series-first, so the
// cost of the whole join is one pass over the Harvest vector rather than one
// pass per claim.
//
// `exact` and `suffix` resolve through a hash index and cost O(volumes):
// tokens are bucketed by byte length, and for each series only the trailing
// len(token) bytes are looked up per distinct length (one length in practice,
// since every `pvc-<uuid>` derives the same length). `contains` and `regex`
// have no such reduction and scan every claim per series — that cost is the
// documented price of opting into them.
type volumeMatcher struct {
	rw *VolumeKeyRewriter

	tokens []string // per claim index; "" for a claim that derived none

	// exact / suffix
	byToken map[string][]int
	lengths []int // distinct token byte lengths, ascending

	// regex
	res []*regexp.Regexp // per claim index; nil when the token does not compile
}

// newVolumeMatcher indexes the claims. A claim whose token fails to compile in
// `regex` mode simply never matches: the token comes from upstream data, not
// from configuration, so it degrades (and is then counted by
// netapp_volume_join_miss) rather than failing the build.
func newVolumeMatcher(rw *VolumeKeyRewriter, claims []pvcVolume) *volumeMatcher {
	m := &volumeMatcher{rw: rw, tokens: make([]string, len(claims))}
	for i, c := range claims {
		m.tokens[i] = rw.token(c.volumeName)
	}
	switch rw.mode {
	case VolumeMatchExact, VolumeMatchSuffix:
		m.byToken = make(map[string][]int, len(claims))
		seenLen := map[int]bool{}
		for i, t := range m.tokens {
			if t == "" {
				continue
			}
			m.byToken[t] = append(m.byToken[t], i)
			if !seenLen[len(t)] {
				seenLen[len(t)] = true
				m.lengths = append(m.lengths, len(t))
			}
		}
		sort.Ints(m.lengths)
	case VolumeMatchRegex:
		m.res = make([]*regexp.Regexp, len(claims))
		for i, t := range m.tokens {
			if t == "" {
				continue
			}
			if re, err := regexp.Compile(t); err == nil {
				m.res[i] = re
			}
		}
	case VolumeMatchContains:
		// No index: substring search has no hash-lookup form. match scans the
		// token slice, which newVolumeMatcher has already filled.
	}
	return m
}

// match appends the indexes of every claim matching this `volume` value to dst
// and returns it. dst is reused across series to keep the pass allocation-free.
func (m *volumeMatcher) match(volume string, dst []int) []int {
	dst = dst[:0]
	if volume == "" {
		return dst
	}
	switch m.rw.mode {
	case VolumeMatchExact:
		return append(dst, m.byToken[volume]...)
	case VolumeMatchSuffix:
		for _, l := range m.lengths {
			if l > len(volume) {
				break // lengths ascend; no longer token can be a suffix
			}
			dst = append(dst, m.byToken[volume[len(volume)-l:]]...)
		}
		return dst
	case VolumeMatchContains:
		for i, t := range m.tokens {
			if t != "" && strings.Contains(volume, t) {
				dst = append(dst, i)
			}
		}
		return dst
	case VolumeMatchRegex:
		for i, re := range m.res {
			if re != nil && re.MatchString(volume) {
				dst = append(dst, i)
			}
		}
		return dst
	}
	return dst
}

// any reports whether ANY claim matches this `volume` value. It is the
// scope-computation form: it needs no claim identity and can stop at the first
// hit.
func (m *volumeMatcher) any(volume string) bool {
	if volume == "" {
		return false
	}
	switch m.rw.mode {
	case VolumeMatchExact:
		return len(m.byToken[volume]) > 0
	case VolumeMatchSuffix:
		for _, l := range m.lengths {
			if l > len(volume) {
				break
			}
			if len(m.byToken[volume[len(volume)-l:]]) > 0 {
				return true
			}
		}
		return false
	case VolumeMatchContains:
		for _, t := range m.tokens {
			if t != "" && strings.Contains(volume, t) {
				return true
			}
		}
		return false
	case VolumeMatchRegex:
		for _, re := range m.res {
			if re != nil && re.MatchString(volume) {
				return true
			}
		}
		return false
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
func qosVolumeScope(pvcInfo, volumeLabels model.Vector, rw *VolumeKeyRewriter) []string {
	if len(pvcInfo) == 0 || len(volumeLabels) == 0 {
		return nil
	}
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
		return nil
	}

	m := newVolumeMatcher(rw, claims)
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
	sort.Strings(out)
	return out
}
