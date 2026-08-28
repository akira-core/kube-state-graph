package build

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClusterFamilyKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"digit run collapses", "prod-03", "prod-0"},
		{"different run same family", "prod-12", "prod-0"},
		{"single digit equals padded run", "prod-3", "prod-0"},
		{"staging family differs from prod", "staging-1", "staging-0"},
		{"bare number", "1", "0"},
		{"bare multi-digit number", "42", "0"},
		{"digit-free name maps to itself", "production", "production"},
		{"unknown bucket maps to itself", "unknown", "unknown"},
		{"empty name", "", ""},
		{"multiple runs", "edge12east3", "edge0east0"},
		{"leading and trailing digits", "1prod2", "0prod0"},
		// The sentinel is a digit, so a literal non-digit sentinel-lookalike
		// in a cluster name cannot collide with a numbered family.
		{"literal hash is not a digit run", "prod-#", "prod-#"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClusterFamilyKey(tc.in); got != tc.want {
				t.Errorf("ClusterFamilyKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Injectivity spot-checks: distinct families must not share a key.
	if ClusterFamilyKey("prod-#") == ClusterFamilyKey("prod-1") {
		t.Error("a literal sentinel-lookalike name must not join a numbered family")
	}
	if ClusterFamilyKey("a#1") == ClusterFamilyKey("a1#") {
		t.Error("digit-run position must stay significant around non-digit bytes")
	}
}

// TestClusterFamilyKey_OverClusterIdentities pins the family rule as it applies
// to composed cluster identities (design D4): the unchanged digit-run rule,
// evaluated over `<az>-<env>-<cluster>`, scopes a family to one zone AND one
// environment.
func TestClusterFamilyKey_OverClusterIdentities(t *testing.T) {
	same := func(a, b string) bool { return ClusterFamilyKey(a) == ClusterFamilyKey(b) }

	assert.True(t, same("us-dev-c1", "us-dev-c2"), "same zone and environment: one family")
	assert.False(t, same("us-dev-c1", "eu-prod-c1"), "a different zone and environment is a different family")
	assert.False(t, same("us-dev-c1", "us-prod-c1"), "the environment alone separates families")
	assert.False(t, same("us-dev-c1", "eu-dev-c1"), "the zone alone separates families")

	// Documented caveat (design D4): the rule normalises digit runs ANYWHERE in
	// the string, so digits inside a zone value widen the family. Pinned so a
	// future struct-aware key is a deliberate change to this expectation, not a
	// silent one.
	assert.True(t, same("us-east-1-prod-c1", "us-east-2-prod-c1"),
		"digits inside the zone value normalise too — the known widening")
}
