package memwindow

import (
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/schema/gvk"

	"github.com/akira-core/kube-state-graph/pkg/route/store"
)

var (
	sigT0  = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	sigT1  = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
	sigMid = sigT0.Add(time.Hour)
)

func mustSpec(t *testing.T, m proto.Message) string {
	t.Helper()
	b, err := protojson.Marshal(m)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	return string(b)
}

func gwSpecJSON(t *testing.T, selectorVal string) string {
	return mustSpec(t, &networking.Gateway{
		Selector: map[string]string{"app": selectorVal},
		Servers: []*networking.Server{{
			Hosts: []string{"*.example.com"},
			Port:  &networking.Port{Number: 80, Name: "http", Protocol: "HTTP"},
		}},
	})
}

func vsSpecJSON(t *testing.T, host, dest string) string {
	return mustSpec(t, &networking.VirtualService{
		Hosts:    []string{host},
		Gateways: []string{"ns-a/gw-a"},
		Http: []*networking.HTTPRoute{{
			Route: []*networking.HTTPRouteDestination{{Destination: &networking.Destination{Host: dest}}},
		}},
	})
}

// testWindow builds a minimal one-gateway scoped config (gateway + bound VS +
// backend service) for signature tests.
func testWindow(t *testing.T) store.TrafficWindow {
	return store.TrafficWindow{
		Gateways: []store.GatewayRow{{
			Namespace: "ns-a", Name: "gw-a", ValidFrom: sigT0, ValidTo: sigT1,
			SelectorKV: []string{"app=gw-a"}, ServerHosts: []string{"*.example.com"},
			SpecJSON: gwSpecJSON(t, "gw-a"),
		}},
		VSes: []store.VSRow{{
			Namespace: "ns-a", Name: "vs-1", ValidFrom: sigT0, ValidTo: sigT1,
			BoundGateways: []string{"ns-a/gw-a"},
			SpecJSON:      vsSpecJSON(t, "svc.example.com", "svc-1.ns-a.svc.cluster.local"),
		}},
		Services: []store.ServiceRow{{
			Namespace: "ns-a", Name: "svc-1", ValidFrom: sigT0, ValidTo: sigT1,
			Ports: []store.SvcPort{{Name: "http", Port: 8080, Protocol: "TCP"}},
		}},
	}
}

func cloneWindow(w store.TrafficWindow) store.TrafficWindow {
	return store.TrafficWindow{
		Services: append([]store.ServiceRow(nil), w.Services...),
		Deploys:  append([]store.DeployRow(nil), w.Deploys...),
		Gateways: append([]store.GatewayRow(nil), w.Gateways...),
		VSes:     append([]store.VSRow(nil), w.VSes...),
	}
}

// TestConfigSigCoversScopedInputs is the safety net for signature completeness:
// it takes every resource ScopedFor actually feeds to the translator and asserts
// that perturbing it changes ConfigSigAt. If the signature ignored an input, two
// different configs could share a signature and the range query would dedup to a
// stale cluster.
//
// When you add a new routing input to ScopedFor, the default case below fires
// until you (1) add a perturbation for it here and (2) make ConfigSigAt cover it —
// that is the forget-proofing.
func TestConfigSigCoversScopedInputs(t *testing.T) {
	w := testWindow(t)
	base, ok := New(w, sigT0, sigT1).ConfigSigAt("gw-a", sigMid)
	if !ok {
		t.Fatal("ConfigSigAt: no gateway live")
	}
	in, found, err := New(w, sigT0, sigT1).ScopedFor("gw-a", sigMid)
	if err != nil || !found {
		t.Fatalf("ScopedFor: found=%v err=%v", found, err)
	}

	// Every config ScopedFor emits must move the signature.
	for _, c := range in.Configs {
		w2 := cloneWindow(w)
		perturbConfig(t, &w2, c.GroupVersionKind, c.Namespace, c.Name)
		if s2, _ := New(w2, sigT0, sigT1).ConfigSigAt("gw-a", sigMid); s2 == base {
			t.Errorf("signature ignores %s %s/%s — dedup is unsafe; cover it in ConfigSigAt",
				c.GroupVersionKind.Kind, c.Namespace, c.Name)
		}
	}

	// Every backend Service ScopedFor emits must move the signature.
	for _, svc := range in.Services {
		w2 := cloneWindow(w)
		perturbService(t, &w2, string(svc.Hostname))
		if s2, _ := New(w2, sigT0, sigT1).ConfigSigAt("gw-a", sigMid); s2 == base {
			t.Errorf("signature ignores backend service %s — cover it in ConfigSigAt", svc.Hostname)
		}
	}
}

// perturbConfig changes the spec of one config row in w. The default case forces
// a new resource type to be handled here (and, by the test, in ConfigSigAt).
func perturbConfig(t *testing.T, w *store.TrafficWindow, kind config.GroupVersionKind, ns, name string) {
	switch kind {
	case gvk.Gateway:
		for i := range w.Gateways {
			if w.Gateways[i].Namespace == ns && w.Gateways[i].Name == name {
				w.Gateways[i].SpecJSON = gwSpecJSON(t, "gw-a-CHANGED")
				return
			}
		}
		t.Fatalf("gateway %s/%s not in window", ns, name)
	case gvk.VirtualService:
		for i := range w.VSes {
			if w.VSes[i].Namespace == ns && w.VSes[i].Name == name {
				w.VSes[i].SpecJSON = vsSpecJSON(t, "svc.example.com", "svc-1-CHANGED.ns-a.svc.cluster.local")
				return
			}
		}
		t.Fatalf("virtualservice %s/%s not in window", ns, name)
	default:
		t.Fatalf("TestConfigSigCoversScopedInputs: no perturbation defined for kind %q — "+
			"when you add this resource to ScopedFor, add a perturbation here AND make "+
			"ConfigSigAt hash it", kind.String())
	}
}

func perturbService(t *testing.T, w *store.TrafficWindow, hostname string) {
	name, ns, ok := store.ParseBackendHost(hostname)
	if !ok {
		t.Fatalf("service host %q is not an in-cluster FQDN", hostname)
	}
	for i := range w.Services {
		if w.Services[i].Namespace == ns && w.Services[i].Name == name {
			// any change to the port set the signature must notice (append
			// reallocates, so the cloned window's row isn't shared with base)
			w.Services[i].Ports = append(w.Services[i].Ports, store.SvcPort{Name: "perturb", Port: 1})
			return
		}
	}
	t.Fatalf("service %s not in window", hostname)
}

// TestConfigSigContentNotVersion checks the dedup premise: two versions with
// identical spec content (different valid_from) produce the same signature, so
// identical-spec version bumps collapse.
func TestConfigSigContentNotVersion(t *testing.T) {
	w := testWindow(t)
	// Add a second gateway version with identical spec but a later valid_from and
	// a different ingest_seq; split the timeline at the boundary.
	boundary := sigT0.Add(48 * time.Hour)
	w.Gateways[0].ValidTo = boundary
	w.Gateways = append(w.Gateways, store.GatewayRow{
		Namespace: "ns-a", Name: "gw-a", ValidFrom: boundary, ValidTo: sigT1, IngestSeq: 999,
		SelectorKV: w.Gateways[0].SelectorKV, ServerHosts: w.Gateways[0].ServerHosts,
		SpecJSON: w.Gateways[0].SpecJSON, // identical content
	})

	mw := New(w, sigT0, sigT1)
	sigV0, ok0 := mw.ConfigSigAt("gw-a", sigT0.Add(time.Hour))    // first version live
	sigV1, ok1 := mw.ConfigSigAt("gw-a", boundary.Add(time.Hour)) // second version live
	if !ok0 || !ok1 {
		t.Fatalf("ConfigSigAt: ok0=%v ok1=%v", ok0, ok1)
	}
	if sigV0 != sigV1 {
		t.Errorf("identical-content versions produced different signatures: dedup would miss them")
	}
}

// TestBoundTo covers the two legal Istio gateway-binding forms the exporter
// stores verbatim: qualified "<ns>/<name>" from anywhere, and bare "<name>"
// only when the VS lives in the gateway's own namespace.
func TestBoundTo(t *testing.T) {
	cases := []struct {
		name  string
		vsNS  string
		bound []string
		want  bool
	}{
		{"qualified_cross_ns", "shop", []string{"istio-system/public-gw"}, true},
		{"qualified_same_ns", "istio-system", []string{"istio-system/public-gw"}, true},
		{"bare_same_ns", "istio-system", []string{"public-gw"}, true},
		{"bare_cross_ns_does_not_bind", "shop", []string{"public-gw"}, false},
		{"other_gateway", "istio-system", []string{"istio-system/other-gw"}, false},
		{"mesh_only", "istio-system", []string{"mesh"}, false},
		{"empty", "istio-system", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := boundTo(c.vsNS, c.bound, "istio-system", "public-gw"); got != c.want {
				t.Errorf("boundTo(%q, %v) = %v, want %v", c.vsNS, c.bound, got, c.want)
			}
		})
	}
}

// TestScopedForBareGatewayRef proves a bare-name-bound VS (exporter stores
// spec.gateways verbatim) reaches the translate input, and that its route
// contributes backend services.
func TestScopedForBareGatewayRef(t *testing.T) {
	w := testWindow(t)
	// Rebind the VS by bare name; move it into the gateway's namespace so the
	// shorthand is legal.
	w.VSes[0].Namespace = "ns-a"
	w.VSes[0].BoundGateways = []string{"gw-a"}

	mw := New(w, sigT0, sigT1)
	in, found, err := mw.ScopedFor("gw-a", sigMid)
	if err != nil || !found {
		t.Fatalf("ScopedFor: found=%v err=%v", found, err)
	}
	if len(in.Configs) != 2 {
		t.Fatalf("configs = %d, want 2 (gateway + bare-ref-bound VS)", len(in.Configs))
	}
	if len(in.Services) == 0 {
		t.Fatal("bare-ref VS's backend services missing from translate input")
	}
	// The signature must see the bare-bound VS too (superset invariant).
	if _, ok := mw.ConfigSigAt("gw-a", sigMid); !ok {
		t.Fatal("ConfigSigAt: no gateway live")
	}
}

// TestScopedForToleratesUnknownSpecFields proves production spec_json — the
// API server's CR JSON, whose field set follows the CLUSTER's CRD version —
// parses even when it carries fields this binary's istio.io/api doesn't know.
func TestScopedForToleratesUnknownSpecFields(t *testing.T) {
	w := testWindow(t)
	// Splice an unknown field into both specs, as a newer CRD would.
	w.Gateways[0].SpecJSON = `{"futureField":{"x":1},` + w.Gateways[0].SpecJSON[1:]
	w.VSes[0].SpecJSON = `{"anotherFutureField":"y",` + w.VSes[0].SpecJSON[1:]

	mw := New(w, sigT0, sigT1)
	in, found, err := mw.ScopedFor("gw-a", sigMid)
	if err != nil {
		t.Fatalf("ScopedFor must tolerate unknown spec fields (DiscardUnknown): %v", err)
	}
	if !found || len(in.Configs) != 2 {
		t.Fatalf("found=%v configs=%d, want found with 2 configs", found, len(in.Configs))
	}
}
