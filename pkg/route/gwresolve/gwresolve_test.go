package gwresolve

import (
	"fmt"
	"testing"
)

func TestResolveMostSpecific(t *testing.T) {
	r := New([]Gateway{
		{Name: "gw-000", Hosts: []string{"*.gw000.example.com"}},
		{Name: "gw-001", Hosts: []string{"*.gw001.example.com"}},
		{Name: "gw-broad-all", Hosts: []string{"*.example.com"}},
		{Name: "gw-exact", Hosts: []string{"exact.host.example.com"}},
		// Istio allows a "<ns>/" binding prefix on server hosts; New must strip
		// it before matching or the pattern silently matches nothing.
		{Name: "gw-scoped", Hosts: []string{"prod/*.scoped.example.com"}},
	})

	cases := []struct {
		host   string
		wantGW string
		wantOK bool
	}{
		// matches both *.gw000.example.com and *.example.com -> most specific wins
		{"svc07.gw000.example.com", "gw-000", true},
		{"svc99.gw001.example.com", "gw-001", true},
		// only matches the broad wildcard
		{"direct-3.example.com", "gw-broad-all", true},
		// exact host beats the broad wildcard that also matches it
		{"exact.host.example.com", "gw-exact", true},
		// "prod/*.scoped.example.com" serves this once the ns prefix is stripped
		{"api.scoped.example.com", "gw-scoped", true},
		// no gateway serves this
		{"nope-1.unknown.invalid", "", false},
	}
	for _, c := range cases {
		gw, ok := r.Resolve(c.host)
		if ok != c.wantOK || gw != c.wantGW {
			t.Errorf("Resolve(%q) = (%q,%v), want (%q,%v)", c.host, gw, ok, c.wantGW, c.wantOK)
		}
	}
}

func TestSanitizeServerHost(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"prod/*.example.com", "*.example.com"},
		{"./api.example.com", "api.example.com"},
		{"*/api.example.com", "api.example.com"},
		{"api.example.com", "api.example.com"},
		{"*", "*"},
	}
	for _, c := range cases {
		if got := SanitizeServerHost(c.in); got != c.want {
			t.Errorf("SanitizeServerHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPickHosts(t *testing.T) {
	cases := []struct {
		name    string
		sets    [][]string
		reqHost string
		wantIdx int
		wantOK  bool
	}{
		{
			name:    "exact beats wildcard",
			sets:    [][]string{{"*.example.com"}, {"api.example.com"}},
			reqHost: "api.example.com",
			wantIdx: 1, wantOK: true,
		},
		{
			name:    "exact beats wildcard, reversed declaration order",
			sets:    [][]string{{"api.example.com"}, {"*.example.com"}},
			reqHost: "api.example.com",
			wantIdx: 0, wantOK: true,
		},
		{
			name:    "more specific wildcard wins",
			sets:    [][]string{{"*.example.com"}, {"*.api.example.com"}},
			reqHost: "v1.api.example.com",
			wantIdx: 1, wantOK: true,
		},
		{
			name:    "more specific wildcard wins, reversed declaration order",
			sets:    [][]string{{"*.api.example.com"}, {"*.example.com"}},
			reqHost: "v1.api.example.com",
			wantIdx: 0, wantOK: true,
		},
		{
			name:    "star is least specific",
			sets:    [][]string{{"*"}, {"*.example.com"}},
			reqHost: "api.example.com",
			wantIdx: 1, wantOK: true,
		},
		{
			name:    "star still matches when nothing else does",
			sets:    [][]string{{"*"}, {"*.example.com"}},
			reqHost: "api.other.net",
			wantIdx: 0, wantOK: true,
		},
		{
			name:    "no match",
			sets:    [][]string{{"api.example.com"}, {"*.example.org"}},
			reqHost: "nope.example.net",
			wantOK:  false,
		},
		{
			name:    "empty host set never matches",
			sets:    [][]string{{}, {"api.example.com"}},
			reqHost: "api.example.com",
			wantIdx: 1, wantOK: true,
		},
		{
			name:    "no sets",
			sets:    nil,
			reqHost: "api.example.com",
			wantOK:  false,
		},
		{
			name:    "ns prefix is stripped before matching",
			sets:    [][]string{{"prod/api.example.com"}},
			reqHost: "api.example.com",
			wantIdx: 0, wantOK: true,
		},
		{
			name:    "multi-host set contributes its best pattern",
			sets:    [][]string{{"*", "api.example.com"}, {"*.example.com"}},
			reqHost: "api.example.com",
			wantIdx: 0, wantOK: true,
		},
		{
			name:    "identical pattern resolves to the smaller index",
			sets:    [][]string{{"api.example.com"}, {"api.example.com"}},
			reqHost: "api.example.com",
			wantIdx: 0, wantOK: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			idx, ok := PickHosts(c.sets, c.reqHost)
			if ok != c.wantOK || (ok && idx != c.wantIdx) {
				t.Errorf("PickHosts(%v, %q) = (%d,%v), want (%d,%v)", c.sets, c.reqHost, idx, ok, c.wantIdx, c.wantOK)
			}
		})
	}
}

// TestPickHostsIndexNotLexicographic pins that the winning index is compared
// numerically, never as a string: with 12 identical-pattern sets, index 10
// must NOT beat index 2 the way "10" < "2" would lexicographically.
func TestPickHostsIndexNotLexicographic(t *testing.T) {
	sets := make([][]string, 12)
	for i := range sets {
		sets[i] = []string{"api.example.com"}
	}
	// Give indices 0 and 1 non-matching hosts so the smallest matching index
	// is 2 — a lexicographic comparison would pick 10 ("10" < "2").
	sets[0] = []string{"other.example.com"}
	sets[1] = []string{"other.example.com"}
	idx, ok := PickHosts(sets, "api.example.com")
	if !ok || idx != 2 {
		t.Fatalf("PickHosts = (%d,%v), want (2,true)", idx, ok)
	}
	if fmt.Sprintf("%d", idx) == "10" {
		t.Fatal("index compared lexicographically")
	}
}
