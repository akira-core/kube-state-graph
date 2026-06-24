package build

import "testing"

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
			if got := clusterFamilyKey(tc.in); got != tc.want {
				t.Errorf("clusterFamilyKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Injectivity spot-checks: distinct families must not share a key.
	if clusterFamilyKey("prod-#") == clusterFamilyKey("prod-1") {
		t.Error("a literal sentinel-lookalike name must not join a numbered family")
	}
	if clusterFamilyKey("a#1") == clusterFamilyKey("a1#") {
		t.Error("digit-run position must stay significant around non-digit bytes")
	}
}
