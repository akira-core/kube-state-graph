package route

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/memwindow"
	"github.com/akira-core/kube-state-graph/pkg/route/store"
)

var (
	lbT0 = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	lbT1 = lbT0.Add(time.Hour)
)

// lbRow builds one window-spanning ingress LB service row (LoadBalancer IP +
// selector, no ports — the ingress-LB shape store.ServiceRow documents).
func lbRow(ns, name, ip string) store.ServiceRow {
	return store.ServiceRow{
		Namespace: ns, Name: name,
		ValidFrom: lbT0.Add(-time.Hour), ValidTo: lbT1.Add(time.Hour),
		LoadBalancerIPs: []string{ip},
		Selector:        []string{"app=" + name},
	}
}

func lbWindow(rows ...store.ServiceRow) *memwindow.Window {
	return memwindow.New(store.TrafficWindow{Services: rows}, lbT0, lbT1)
}

func TestResolveIngressLBService_SingleIPSingleService(t *testing.T) {
	mw := lbWindow(lbRow("ingress-nginx", "ingress-nginx-controller", "198.51.100.7"))

	dest, outcome, ok := resolveIngressLBService(mw, []string{"198.51.100.7"})

	require.True(t, ok)
	assert.Equal(t, build.RouteIngressLBService, outcome)
	assert.Equal(t, build.RouteDestination{
		Namespace: "ingress-nginx", Service: "ingress-nginx-controller",
		IngressNamespace: "ingress-nginx", IngressService: "ingress-nginx-controller",
	}, dest)
}

// ingressServiceIdentity is the shared identity-dedup core (also stamped onto
// RouteHit destinations for the ingress chain); the tri-state table below pins
// it directly, complementing the wrapper tests around resolveIngressLBService.
func TestIngressServiceIdentity(t *testing.T) {
	sameIdentityV1 := lbRow("ingress-nginx", "ctl", "198.51.100.7")
	sameIdentityV1.ValidTo = lbT0.Add(30 * time.Minute)
	sameIdentityV2 := lbRow("ingress-nginx", "ctl", "198.51.100.7")
	sameIdentityV2.ValidFrom = lbT0.Add(30 * time.Minute)

	renamedOld := lbRow("ingress-nginx", "lb-old", "198.51.100.7")
	renamedOld.ValidTo = lbT0.Add(30 * time.Minute)
	renamedNew := lbRow("ingress-nginx", "lb-new", "198.51.100.7")
	renamedNew.ValidFrom = lbT0.Add(30 * time.Minute)

	dualIP := lbRow("ingress-nginx", "ctl", "198.51.100.7")
	dualIP.ExternalIPs = []string{"198.51.100.8"}

	cases := []struct {
		name   string
		rows   []store.ServiceRow
		ips    []string
		want   ingressIdentity
		status ingressIdentityStatus
	}{
		{
			name: "unique single IP",
			rows: []store.ServiceRow{lbRow("ingress-nginx", "ctl", "198.51.100.7")},
			ips:  []string{"198.51.100.7"},
			want: ingressIdentity{ns: "ingress-nginx", name: "ctl"}, status: identityUnique,
		},
		{
			name: "unique across two IPs of one row",
			rows: []store.ServiceRow{dualIP},
			ips:  []string{"198.51.100.7", "198.51.100.8"},
			want: ingressIdentity{ns: "ingress-nginx", name: "ctl"}, status: identityUnique,
		},
		{
			name: "version churn of one identity stays unique",
			rows: []store.ServiceRow{sameIdentityV1, sameIdentityV2},
			ips:  []string{"198.51.100.7"},
			want: ingressIdentity{ns: "ingress-nginx", name: "ctl"}, status: identityUnique,
		},
		{
			name: "same-IP collision ambiguous",
			rows: []store.ServiceRow{
				lbRow("ingress-nginx", "lb-a", "198.51.100.7"),
				lbRow("other-ns", "lb-b", "198.51.100.7"),
			},
			ips:    []string{"198.51.100.7"},
			status: identityAmbiguous,
		},
		{
			name: "identity change within window ambiguous",
			rows: []store.ServiceRow{renamedOld, renamedNew},
			ips:  []string{"198.51.100.7"},

			status: identityAmbiguous,
		},
		{
			name: "disagreeing singletons ambiguous",
			rows: []store.ServiceRow{
				lbRow("ingress-nginx", "lb-a", "198.51.100.7"),
				lbRow("ingress-nginx", "lb-b", "198.51.100.8"),
			},
			ips:    []string{"198.51.100.7", "198.51.100.8"},
			status: identityAmbiguous,
		},
		{
			name:   "no rows incomplete",
			rows:   nil,
			ips:    []string{"198.51.100.7"},
			status: identityIncomplete,
		},
		{
			name: "partial empty IP incomplete",
			rows: []store.ServiceRow{lbRow("ingress-nginx", "ctl", "198.51.100.7")},
			ips:  []string{"198.51.100.7", "203.0.113.1"},

			status: identityIncomplete,
		},
		{
			name: "empty outranks disagreement",
			rows: []store.ServiceRow{
				lbRow("ingress-nginx", "lb-a", "198.51.100.7"),
				lbRow("ingress-nginx", "lb-b", "198.51.100.8"),
			},
			ips:    []string{"198.51.100.7", "203.0.113.1", "198.51.100.8"},
			status: identityIncomplete,
		},
		{
			name: "collision outranks empty",
			rows: []store.ServiceRow{
				lbRow("ingress-nginx", "lb-a", "198.51.100.7"),
				lbRow("other-ns", "lb-b", "198.51.100.7"),
			},
			ips:    []string{"203.0.113.1", "198.51.100.7"},
			status: identityAmbiguous,
		},
		{
			name:   "no IPs incomplete",
			rows:   []store.ServiceRow{lbRow("ingress-nginx", "ctl", "198.51.100.7")},
			ips:    nil,
			status: identityIncomplete,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, status := ingressServiceIdentity(lbWindow(tc.rows...), tc.ips)
			assert.Equal(t, tc.status, status)
			assert.Equal(t, tc.want, id)
		})
	}
}

func TestResolveIngressLBService_SingleIPTwoServicesAmbiguous(t *testing.T) {
	mw := lbWindow(
		lbRow("ingress-nginx", "ingress-nginx-controller", "198.51.100.7"),
		lbRow("other-ns", "some-other-lb", "198.51.100.7"),
	)

	dest, outcome, ok := resolveIngressLBService(mw, []string{"198.51.100.7"})

	require.True(t, ok)
	assert.Equal(t, build.RouteAmbiguousIngressService, outcome)
	assert.Zero(t, dest)
}

func TestResolveIngressLBService_TwoIPsDisagreeingIdentitiesAmbiguous(t *testing.T) {
	mw := lbWindow(
		lbRow("ingress-nginx", "lb-a", "198.51.100.7"),
		lbRow("ingress-nginx", "lb-b", "198.51.100.8"),
	)

	_, outcome, ok := resolveIngressLBService(mw, []string{"198.51.100.7", "198.51.100.8"})

	require.True(t, ok)
	assert.Equal(t, build.RouteAmbiguousIngressService, outcome)
}

func TestResolveIngressLBService_TwoIPsSameIdentityHit(t *testing.T) {
	row := lbRow("ingress-nginx", "ingress-nginx-controller", "198.51.100.7")
	row.ExternalIPs = []string{"198.51.100.8"}
	mw := lbWindow(row)

	dest, outcome, ok := resolveIngressLBService(mw, []string{"198.51.100.7", "198.51.100.8"})

	require.True(t, ok)
	assert.Equal(t, build.RouteIngressLBService, outcome)
	assert.Equal(t, "ingress-nginx-controller", dest.Service)
}

func TestResolveIngressLBService_NoMatchKeepsMiss(t *testing.T) {
	mw := lbWindow(lbRow("ingress-nginx", "ingress-nginx-controller", "198.51.100.7"))

	_, outcome, ok := resolveIngressLBService(mw, []string{"203.0.113.1"})

	assert.False(t, ok)
	assert.Empty(t, outcome)
}

// An IP with one candidate plus an IP with none: rule 2 (incomplete evidence →
// keep the Istio miss) applies — a partial match must not promote a fallback
// hit the other IP cannot corroborate.
func TestResolveIngressLBService_PartialEmptyIPKeepsMiss(t *testing.T) {
	mw := lbWindow(lbRow("ingress-nginx", "ingress-nginx-controller", "198.51.100.7"))

	_, _, ok := resolveIngressLBService(mw, []string{"198.51.100.7", "203.0.113.1"})

	assert.False(t, ok)
}

// One IP with two candidates and another with none: the same-IP collision
// (rule 1) outranks the incomplete-evidence rule regardless of IP order.
func TestResolveIngressLBService_CollisionOutranksEmptyEitherOrder(t *testing.T) {
	mw := lbWindow(
		lbRow("ingress-nginx", "lb-a", "198.51.100.7"),
		lbRow("other-ns", "lb-b", "198.51.100.7"),
	)

	for _, ips := range [][]string{
		{"198.51.100.7", "203.0.113.1"},
		{"203.0.113.1", "198.51.100.7"},
	} {
		_, outcome, ok := resolveIngressLBService(mw, ips)
		require.True(t, ok, "ips=%v", ips)
		assert.Equal(t, build.RouteAmbiguousIngressService, outcome, "ips=%v", ips)
	}
}

// Disagreeing singletons rank BELOW an empty IP: with both present the Istio
// miss is kept (rule 2 before rule 3), regardless of IP order.
func TestResolveIngressLBService_EmptyOutranksDisagreementEitherOrder(t *testing.T) {
	mw := lbWindow(
		lbRow("ingress-nginx", "lb-a", "198.51.100.7"),
		lbRow("ingress-nginx", "lb-b", "198.51.100.8"),
	)

	for _, ips := range [][]string{
		{"198.51.100.7", "203.0.113.1", "198.51.100.8"},
		{"198.51.100.7", "198.51.100.8", "203.0.113.1"},
	} {
		_, _, ok := resolveIngressLBService(mw, ips)
		assert.False(t, ok, "ips=%v", ips)
	}
}

// Several versions (rows) of the SAME identity dedup to one candidate — normal
// version churn must not read as a collision.
func TestResolveIngressLBService_SameIdentityMultiVersionHit(t *testing.T) {
	v1 := lbRow("ingress-nginx", "ingress-nginx-controller", "198.51.100.7")
	v1.ValidTo = lbT0.Add(30 * time.Minute)
	v2 := lbRow("ingress-nginx", "ingress-nginx-controller", "198.51.100.7")
	v2.ValidFrom = lbT0.Add(30 * time.Minute)
	mw := lbWindow(v1, v2)

	dest, outcome, ok := resolveIngressLBService(mw, []string{"198.51.100.7"})

	require.True(t, ok)
	assert.Equal(t, build.RouteIngressLBService, outcome)
	assert.Equal(t, "ingress-nginx-controller", dest.Service)
}

// An identity CHANGE inside the window (svc-a deleted, svc-b created on the
// same IP) yields two identities — ambiguous, per the window-wide dedup rule
// (design D6: don't guess which one the traffic hit).
func TestResolveIngressLBService_IdentityChangeWithinWindowAmbiguous(t *testing.T) {
	old := lbRow("ingress-nginx", "lb-old", "198.51.100.7")
	old.ValidTo = lbT0.Add(30 * time.Minute)
	renamed := lbRow("ingress-nginx", "lb-new", "198.51.100.7")
	renamed.ValidFrom = lbT0.Add(30 * time.Minute)
	mw := lbWindow(old, renamed)

	_, outcome, ok := resolveIngressLBService(mw, []string{"198.51.100.7"})

	require.True(t, ok)
	assert.Equal(t, build.RouteAmbiguousIngressService, outcome)
}
