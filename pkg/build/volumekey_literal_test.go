package build

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// regexToken is the reference derivation: every rule applied as a regexp, in
// order. The literal fast path must agree with it for every rule set.
func regexToken(t *testing.T, rules []VolumeKeyRule, pv string) string {
	t.Helper()
	if pv == "" {
		return ""
	}
	for _, r := range rules {
		pv = regexp.MustCompile(r.Pattern).ReplaceAllString(pv, r.Replacement)
	}
	return pv
}

func TestVolumeKeyRewriter_LiteralFastPathAgreesWithRegexp(t *testing.T) {
	ruleSets := map[string][]VolumeKeyRule{
		"default":              nil,
		"identity":             {},
		"literal multi-byte":   {{Pattern: "pvc-", Replacement: "vol_"}},
		"literal then literal": {{Pattern: "-", Replacement: "_"}, {Pattern: "pvc_", Replacement: ""}},
		"escaped metachar":     {{Pattern: `\.`, Replacement: "_"}},
		"regexp class":         {{Pattern: "[-.]", Replacement: "_"}},
		"anchored":             {{Pattern: "^pvc-", Replacement: "x_"}},
		"capture replacement":  {{Pattern: "(pvc)-", Replacement: "${1}_"}},
		"dollar in literal":    {{Pattern: "-", Replacement: "$0"}},
		"empty pattern":        {{Pattern: "", Replacement: "_"}},
		"case-insensitive":     {{Pattern: "(?i)PVC", Replacement: "v"}},
	}
	pvs := []string{"", "pvc-ab12-cd34", "pvc.ab.12", "PVC-x", "plain", "a-b-c-"}
	for name, rules := range ruleSets {
		rw, err := NewVolumeKeyRewriter(rules)
		require.NoError(t, err, name)
		ref := rules
		if ref == nil {
			ref = DefaultVolumeKeyRules()
		}
		for _, pv := range pvs {
			assert.Equal(t, regexToken(t, ref, pv), rw.token(pv), "%s: %q", name, pv)
		}
	}
}

// The default rule — the one every deployment that configures none runs — is a
// plain literal, so it must take the strings.ReplaceAll path rather than the
// regexp engine.
func TestVolumeKeyRewriter_DefaultRuleIsLiteral(t *testing.T) {
	rw, err := NewVolumeKeyRewriter(nil)
	require.NoError(t, err)
	require.Len(t, rw.rules, 1)
	assert.True(t, rw.rules[0].literal, "the default `-` rule is replaced literally")

	rw, err = NewVolumeKeyRewriter([]VolumeKeyRule{{Pattern: "(pvc)-", Replacement: "${1}_"}})
	require.NoError(t, err)
	assert.False(t, rw.rules[0].literal, "a rule with a capture group keeps the regexp")
}

// BenchmarkVolumeKeyToken derives the tokens of one large estate's claims with
// the default rule set, the cost claimVolumeMatcher pays per scope it builds.
func BenchmarkVolumeKeyToken(b *testing.B) {
	rw, err := NewVolumeKeyRewriter(nil)
	require.NoError(b, err)
	pvs := make([]string, 5000)
	for i := range pvs {
		pvs[i] = fmt.Sprintf("pvc-%08x-1b2c-4d5e-8f90-%012x", i, i*7919)
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, pv := range pvs {
			_ = rw.token(pv)
		}
	}
}
