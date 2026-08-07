package matchcheck

import "testing"

// parseActuals scrapes router_check_tool's --details text. Its marker rule
// (any all-digits line re-points the current index) cannot tell a test name from
// a stray numeric line, so an incomplete parse must be an error rather than a
// partially filled result set — otherwise a batch silently returns one query's
// cluster under another query's index (design D7).
func TestParseActuals(t *testing.T) {
	cases := []struct {
		name string
		out  string
		n    int
		want []string
		ok   bool
	}{
		{
			name: "single_query",
			out:  "0\n  actual: [outbound|8080||reviews.prod.svc.cluster.local]\n",
			n:    1,
			want: []string{"outbound|8080||reviews.prod.svc.cluster.local"},
			ok:   true,
		},
		{
			name: "route_miss_is_an_empty_cluster_not_an_error",
			out:  "0\n  actual: []\n",
			n:    1,
			want: []string{""},
			ok:   true,
		},
		{
			name: "batch",
			out:  "0\n  actual: [a]\n1\n  actual: [b]\n",
			n:    2,
			want: []string{"a", "b"},
			ok:   true,
		},
		{
			// The misalignment this guard exists for: a stray numeric line
			// re-points cur, so query 1's result lands under index 2 and query 1
			// is never filled. Pre-change this returned ["a", "", "b"].
			name: "stray_numeric_line_misaligns_a_batch",
			out:  "0\n  actual: [a]\n1\n2\n  actual: [b]\n",
			n:    3,
			ok:   false,
		},
		{
			name: "tool_failed_before_running_tests",
			out:  "error initializing configuration\n",
			n:    1,
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseActuals([]byte(c.out), c.n)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
			if !c.ok {
				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("result[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}
