package memwindow

import (
	"testing"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/route/store"
)

// TestResolveIPToIngressServices covers the window-wide Hop-1 accessor of the
// ingress-lb-service-fallback change: every overlapping row carrying the IP is
// returned (no as-of instant, no first-match short-circuit), rows outside the
// window or without the IP are not.
func TestResolveIPToIngressServices(t *testing.T) {
	const ip = "198.51.100.7"
	in := store.ServiceRow{
		Namespace: "ingress-nginx", Name: "lb-a",
		ValidFrom: sigT0, ValidTo: sigT1,
		LoadBalancerIPs: []string{ip},
	}
	inExt := store.ServiceRow{
		Namespace: "other-ns", Name: "lb-b",
		ValidFrom: sigT0, ValidTo: sigT1,
		ExternalIPs: []string{ip},
	}
	otherIP := store.ServiceRow{
		Namespace: "ingress-nginx", Name: "lb-c",
		ValidFrom: sigT0, ValidTo: sigT1,
		LoadBalancerIPs: []string{"203.0.113.1"},
	}
	// Defensive: a row whose validity ends before the queried window opens
	// must not surface even if the store leaked it into the load.
	stale := store.ServiceRow{
		Namespace: "ingress-nginx", Name: "lb-stale",
		ValidFrom: sigT0.Add(-2 * time.Hour), ValidTo: sigT0.Add(-time.Hour),
		LoadBalancerIPs: []string{ip},
	}

	w := New(store.TrafficWindow{Services: []store.ServiceRow{in, inExt, otherIP, stale}}, sigT0, sigT1)

	got := w.ResolveIPToIngressServices(ip)
	if len(got) != 2 {
		t.Fatalf("want 2 rows (LoadBalancerIP + ExternalIP matches), got %d: %+v", len(got), got)
	}
	names := map[string]bool{}
	for _, r := range got {
		names[r.Namespace+"/"+r.Name] = true
	}
	for _, want := range []string{"ingress-nginx/lb-a", "other-ns/lb-b"} {
		if !names[want] {
			t.Errorf("missing row %s in %v", want, names)
		}
	}

	if got := w.ResolveIPToIngressServices("192.0.2.9"); len(got) != 0 {
		t.Errorf("unmatched IP: want no rows, got %+v", got)
	}
}
