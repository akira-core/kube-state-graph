package store

import "testing"

// ParseBackendHost's input is an in-cluster Service FQDN — either a
// VirtualService destination.host already normalised by ResolveDestinationHost,
// or the host segment of an istiod-generated Envoy cluster string. Anything else
// names no Service this engine can report.
func TestParseBackendHost(t *testing.T) {
	cases := []struct {
		host           string
		wantName, want string
		wantOK         bool
	}{
		{"reviews.prod.svc.cluster.local", "reviews", "prod", true},
		// Three leading labels is the headless per-pod form. It is NOT the
		// Service reviews.prod: parsing it by prefix would report a Service that
		// never received the traffic.
		{"mysql-0.mysql.db.svc.cluster.local", "", "", false},
		// Relative forms: istiod leaves a dotted destination.host verbatim, so
		// these reach here unexpanded and name nothing in the registry.
		{"checkout.shop.svc", "", "", false},
		{"checkout.shop", "", "", false},
		{"checkout", "", "", false},
		{"api.example.com", "", "", false},
		{".prod.svc.cluster.local", "", "", false},
		{"reviews..svc.cluster.local", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			name, ns, ok := ParseBackendHost(c.host)
			if ok != c.wantOK || name != c.wantName || ns != c.want {
				t.Errorf("ParseBackendHost(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.host, name, ns, ok, c.wantName, c.want, c.wantOK)
			}
		})
	}
}

// ResolveDestinationHost mirrors istiod's ResolveShortnameToFQDN: ONLY a
// dot-free name is expanded, using the owning VirtualService's namespace.
// Anything containing a dot is already-qualified as far as istiod is concerned
// and is returned verbatim — including the relative forms a user may have
// intended as short names.
func TestResolveDestinationHost(t *testing.T) {
	cases := []struct {
		host, ns, want string
	}{
		{"checkout", "shop", "checkout.shop.svc.cluster.local"},
		{"checkout.shop", "shop", "checkout.shop"},
		{"checkout.shop.svc", "shop", "checkout.shop.svc"},
		{"checkout.shop.svc.cluster.local", "shop", "checkout.shop.svc.cluster.local"},
		{"api.example.com", "shop", "api.example.com"},
		{"*", "shop", "*"},
		{"*.example.com", "shop", "*.example.com"},
		{"10.0.0.1", "shop", "10.0.0.1"},
		{"2001:db8::1", "shop", "2001:db8::1"},
		{"checkout", "", "checkout"}, // no namespace to qualify with
		{"", "shop", ""},
	}
	for _, c := range cases {
		t.Run(c.host+"@"+c.ns, func(t *testing.T) {
			if got := ResolveDestinationHost(c.host, c.ns); got != c.want {
				t.Errorf("ResolveDestinationHost(%q, %q) = %q, want %q", c.host, c.ns, got, c.want)
			}
		})
	}
}
