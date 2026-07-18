package route

import (
	"testing"

	"github.com/akira-core/kube-state-graph/pkg/build"
)

// TestPickIngressCluster drives the full design-D10 decision table plus the
// multi-IP agreement merge (the plan's T-matrix). caller defaults to
// "prod-01"; family membership follows build.ClusterFamilyKey (digit-run →
// '0' sentinel), so prod-01/prod-02/prod-03 are one family and staging-01 is
// another.
func TestPickIngressCluster(t *testing.T) {
	cases := []struct {
		name   string
		caller string
		perIP  [][]string
		want   string
		miss   build.RouteOutcome
		wantOK bool
	}{
		// T1: single same-family candidate that is not the caller.
		{name: "family singleton wins", caller: "prod-01",
			perIP: [][]string{{"prod-02"}}, want: "prod-02", wantOK: true},
		// T2: same-family collision including the caller → caller tie-break.
		{name: "caller breaks family collision", caller: "prod-01",
			perIP: [][]string{{"prod-01", "prod-02"}}, want: "prod-01", wantOK: true},
		// T3: family converges before the global set is consulted.
		{name: "family beats cross-family candidate", caller: "prod-01",
			perIP: [][]string{{"prod-01", "staging-01"}}, want: "prod-01", wantOK: true},
		{name: "family sibling beats cross-family candidate", caller: "prod-01",
			perIP: [][]string{{"prod-02", "staging-01"}}, want: "prod-02", wantOK: true},
		// T4: no family candidate, exactly one global → shared ingress.
		{name: "unique global candidate outside family", caller: "prod-01",
			perIP: [][]string{{"staging-01"}}, want: "staging-01", wantOK: true},
		// T5a: several family candidates, caller not among them.
		{name: "family collision without caller is ambiguous", caller: "prod-01",
			perIP: [][]string{{"prod-02", "prod-03"}}, miss: build.RouteAmbiguousIngress},
		// T5b: no family candidate, several global, caller not among them.
		{name: "global collision without caller is ambiguous", caller: "edge",
			perIP: [][]string{{"staging-01", "staging-02"}}, miss: build.RouteAmbiguousIngress},
		// |G|==0 for the only IP → no ingress.
		{name: "no candidate anywhere is no-ingress", caller: "prod-01",
			perIP: [][]string{{}}, miss: build.RouteNoIngress},
		{name: "nil candidate set is no-ingress", caller: "prod-01",
			perIP: [][]string{nil}, miss: build.RouteNoIngress},
		// Defensive: the prescan never emits an IP-less request.
		{name: "no IPs at all is no-ingress", caller: "prod-01",
			perIP: nil, miss: build.RouteNoIngress},
		// Digit-free caller forms an exact-name singleton family; being a
		// candidate itself it converges at the family stage (rule 1).
		{name: "digit-free caller in global set converges via own family", caller: "edge",
			perIP: [][]string{{"edge", "staging-01"}}, want: "edge", wantOK: true},
		// T8: multi-IP, both agree.
		{name: "multi-IP agreement", caller: "prod-01",
			perIP: [][]string{{"prod-02"}, {"prod-02", "staging-01"}}, want: "prod-02", wantOK: true},
		// T9: multi-IP selections disagree.
		{name: "multi-IP disagreement is ambiguous", caller: "prod-01",
			perIP: [][]string{{"prod-02"}, {"staging-01"}}, miss: build.RouteAmbiguousIngress},
		// T9b: mix of no-ingress and a selection.
		{name: "mixed no-ingress and pick is ambiguous", caller: "prod-01",
			perIP: [][]string{{}, {"prod-02"}}, miss: build.RouteAmbiguousIngress},
		{name: "pick then no-ingress is ambiguous", caller: "prod-01",
			perIP: [][]string{{"prod-02"}, {}}, miss: build.RouteAmbiguousIngress},
		// Every IP no-ingress → no-ingress (not ambiguous).
		{name: "all IPs no-ingress stays no-ingress", caller: "prod-01",
			perIP: [][]string{{}, {}}, miss: build.RouteNoIngress},
		// Any ambiguous IP poisons the merge even if another IP agrees.
		{name: "one ambiguous IP is ambiguous overall", caller: "prod-01",
			perIP: [][]string{{"prod-02"}, {"prod-02", "prod-03"}}, miss: build.RouteAmbiguousIngress},
		// Duplicate candidates within one IP's set dedupe before counting.
		{name: "duplicate candidates count once", caller: "prod-01",
			perIP: [][]string{{"prod-02", "prod-02"}}, want: "prod-02", wantOK: true},
		// Selection is order-free within a candidate set.
		{name: "candidate order does not matter", caller: "prod-01",
			perIP: [][]string{{"staging-01", "prod-01"}}, want: "prod-01", wantOK: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, miss, ok := pickIngressCluster(tc.caller, tc.perIP)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got=%q miss=%q)", ok, tc.wantOK, got, miss)
			}
			if tc.wantOK {
				if got != tc.want {
					t.Fatalf("cluster = %q, want %q", got, tc.want)
				}
				if miss != "" {
					t.Fatalf("miss = %q, want empty on a successful pick", miss)
				}
				return
			}
			if miss != tc.miss {
				t.Fatalf("miss = %q, want %q", miss, tc.miss)
			}
			if got != "" {
				t.Fatalf("cluster = %q, want empty on a miss", got)
			}
		})
	}
}
