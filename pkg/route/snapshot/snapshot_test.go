package snapshot

import (
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	networking "istio.io/api/networking/v1alpha3"

	"github.com/akira-core/kube-state-graph/pkg/route/store"
	"github.com/akira-core/kube-state-graph/pkg/route/translate"
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

// testSnapshot builds a minimal one-gateway scoped config (gateway + bound VS +
// backend service), every version live across [sigT0, sigT1).
func testSnapshot(t *testing.T) store.TrafficSnapshot {
	return store.TrafficSnapshot{
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
	w := testSnapshot(t)
	// Rebind the VS by bare name; move it into the gateway's namespace so the
	// shorthand is legal.
	w.VSes[0].Namespace = "ns-a"
	w.VSes[0].BoundGateways = []string{"gw-a"}

	in, found, err := New(w, sigMid).ScopedFor("gw-a")
	if err != nil || !found {
		t.Fatalf("ScopedFor: found=%v err=%v", found, err)
	}
	if len(in.Configs) != 2 {
		t.Fatalf("configs = %d, want 2 (gateway + bare-ref-bound VS)", len(in.Configs))
	}
	if len(in.Services) == 0 {
		t.Fatal("bare-ref VS's backend services missing from translate input")
	}
}

// TestScopedForToleratesUnknownSpecFields proves production spec_json — the
// API server's CR JSON, whose field set follows the CLUSTER's CRD version —
// parses even when it carries fields this binary's istio.io/api doesn't know.
func TestScopedForToleratesUnknownSpecFields(t *testing.T) {
	w := testSnapshot(t)
	// Splice an unknown field into both specs, as a newer CRD would.
	w.Gateways[0].SpecJSON = `{"futureField":{"x":1},` + w.Gateways[0].SpecJSON[1:]
	w.VSes[0].SpecJSON = `{"anotherFutureField":"y",` + w.VSes[0].SpecJSON[1:]

	in, found, err := New(w, sigMid).ScopedFor("gw-a")
	if err != nil {
		t.Fatalf("ScopedFor must tolerate unknown spec fields (DiscardUnknown): %v", err)
	}
	if !found || len(in.Configs) != 2 {
		t.Fatalf("found=%v configs=%d, want found with 2 configs", found, len(in.Configs))
	}
}

// TestScopedForUsesVersionLiveAtInstant is the point-in-time core
// (simplify-route-resolution-to-point-in-time D1/D2): with two Gateway versions
// and two VS versions in the loaded rows, ONLY the pair live at the snapshot's
// instant reaches the translate input — the superseded ones are invisible even
// though the store handed them over.
func TestScopedForUsesVersionLiveAtInstant(t *testing.T) {
	boundary := sigT0.Add(48 * time.Hour)
	w := testSnapshot(t)
	// Close the original versions at the boundary and add newer ones.
	w.Gateways[0].ValidTo = boundary
	w.Gateways = append(w.Gateways, store.GatewayRow{
		Namespace: "ns-a", Name: "gw-a", ValidFrom: boundary, ValidTo: sigT1, IngestSeq: 9,
		SelectorKV: []string{"app=gw-a"}, ServerHosts: []string{"*.example.com"},
		SpecJSON: gwSpecJSON(t, "gw-a-NEW"),
	})
	w.VSes[0].ValidTo = boundary
	w.VSes = append(w.VSes, store.VSRow{
		Namespace: "ns-a", Name: "vs-1", ValidFrom: boundary, ValidTo: sigT1, IngestSeq: 9,
		BoundGateways: []string{"ns-a/gw-a"},
		SpecJSON:      vsSpecJSON(t, "svc.example.com", "svc-2.ns-a.svc.cluster.local"),
	})
	// Two backend Services, each live only for its own VS version.
	w.Services[0].ValidTo = boundary
	w.Services = append(w.Services, store.ServiceRow{
		Namespace: "ns-a", Name: "svc-2", ValidFrom: boundary, ValidTo: sigT1,
		Ports: []store.SvcPort{{Name: "http", Port: 8080, Protocol: "TCP"}},
	})

	for _, c := range []struct {
		name     string
		at       time.Time
		wantHost string
		wantSvc  string
	}{
		{"before_boundary", sigMid, "gw-a", "svc-1.ns-a.svc.cluster.local"},
		{"after_boundary", boundary.Add(time.Hour), "gw-a-NEW", "svc-2.ns-a.svc.cluster.local"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in, found, err := New(w, c.at).ScopedFor("gw-a")
			if err != nil || !found {
				t.Fatalf("ScopedFor: found=%v err=%v", found, err)
			}
			if len(in.Configs) != 2 {
				t.Fatalf("configs = %d, want exactly 2 (one live gateway + one live VS)", len(in.Configs))
			}
			if got := in.Proxy.Labels["app"]; got != c.wantHost {
				t.Errorf("gateway selector = %q, want the version live at the instant (%q)", got, c.wantHost)
			}
			if len(in.Services) != 1 || string(in.Services[0].Hostname) != c.wantSvc {
				t.Fatalf("backend services = %v, want only %s", in.Services, c.wantSvc)
			}
		})
	}
}

// TestScopedForDedupsDuplicateRows is the multi-IP union guard: a request whose
// destination IPs are served by the same ingress Service loads the SAME gateway,
// VS and backend rows once per IP, and Resolver.loadSnapshot concatenates them.
// istiod's in-memory config store rejects a duplicate ("item already exists"),
// so an undeduped translate input fails the whole resolution and the endpoint
// falls back to an external node — the dual-stack ingress shape.
//
// The final Translate is the point of the test: asserting only on the config
// count would not prove istiod accepts the input.
func TestScopedForDedupsDuplicateRows(t *testing.T) {
	w := testSnapshot(t)
	// Exactly what two per-IP LoadTrafficAt calls against one dual-stack ingress
	// Service return: byte-identical rows, appended twice.
	w.Gateways = append(w.Gateways, w.Gateways[0])
	w.VSes = append(w.VSes, w.VSes[0])
	w.Services = append(w.Services, w.Services[0])

	in, found, err := New(w, sigMid).ScopedFor("gw-a")
	if err != nil || !found {
		t.Fatalf("ScopedFor: found=%v err=%v", found, err)
	}
	if len(in.Configs) != 2 {
		t.Fatalf("configs = %d, want 2 (one gateway + one VS) — duplicates must collapse", len(in.Configs))
	}
	if len(in.Services) != 1 {
		t.Fatalf("backend services = %d, want 1 — duplicates must collapse", len(in.Services))
	}
	if _, err := translate.NewTranslator().Translate(in); err != nil {
		t.Fatalf("translate rejected the deduped input: %v", err)
	}
}

// A gateway with no version live at the instant is not resolvable at all.
func TestScopedForGatewayNotLiveAtInstant(t *testing.T) {
	w := testSnapshot(t)
	w.Gateways[0].ValidTo = sigMid // ends before the queried instant

	_, found, err := New(w, sigMid.Add(time.Hour)).ScopedFor("gw-a")
	if err != nil {
		t.Fatalf("ScopedFor: %v", err)
	}
	if found {
		t.Error("gateway whose only version ended before the instant must not resolve")
	}
}

// The 3-hop join is evaluated as of the instant too: an ingress Service version
// that ended before it cannot select a gateway.
func TestResolveIPToGatewaysUsesLiveVersions(t *testing.T) {
	const ip = "198.51.100.7"
	boundary := sigT0.Add(time.Hour)
	w := store.TrafficSnapshot{
		Services: []store.ServiceRow{{
			Namespace: "istio-system", Name: "ingress-lb",
			ValidFrom: sigT0, ValidTo: boundary, // dead after the boundary
			LoadBalancerIPs: []string{ip}, Selector: []string{"app=gw-a"},
		}},
		Deploys: []store.DeployRow{{
			Namespace: "istio-system", Name: "ingress", ValidFrom: sigT0, ValidTo: sigT1,
			PodLabels: []string{"app=gw-a"},
		}},
		Gateways: []store.GatewayRow{{
			// Same namespace as the ingress Service — hop 3 is namespace-scoped.
			Namespace: "istio-system", Name: "gw-a", ValidFrom: sigT0, ValidTo: sigT1,
			SelectorKV: []string{"app=gw-a"}, ServerHosts: []string{"*.example.com"},
		}},
	}

	if got := New(w, sigT0).ResolveIPToGateways(ip); len(got) != 1 || got[0].Name != "gw-a" {
		t.Fatalf("while the ingress Service is live: got %+v, want gw-a", got)
	}
	if got := New(w, boundary).ResolveIPToGateways(ip); len(got) != 0 {
		t.Errorf("after the ingress Service version ended: got %+v, want no candidates", got)
	}
}

// Hop 3 is namespace-scoped (scope-gateway-candidates-to-ingress-namespace):
// a candidate Gateway must live in the ingress Service's own namespace. A
// same-named gateway in another namespace whose selector also matches the
// ingress pod labels is NOT a candidate — cross-namespace attachment is
// deliberately out of scope, so the bare-name identity downstream can never
// collide.
func TestResolveIPToGatewaysScopedToIngressNamespace(t *testing.T) {
	const ip = "198.51.100.7"
	w := store.TrafficSnapshot{
		Services: []store.ServiceRow{{
			Namespace: "istio-system", Name: "ingress-lb", ValidFrom: sigT0, ValidTo: sigT1,
			LoadBalancerIPs: []string{ip}, Selector: []string{"istio=ingress"},
		}},
		Deploys: []store.DeployRow{{
			Namespace: "istio-system", Name: "ingress", ValidFrom: sigT0, ValidTo: sigT1,
			PodLabels: []string{"istio=ingress"},
		}},
		Gateways: []store.GatewayRow{
			{ // cross-namespace, listed FIRST: must be skipped, not merely outranked
				Namespace: "team-b", Name: "gw-a", ValidFrom: sigT0, ValidTo: sigT1,
				SelectorKV: []string{"istio=ingress"}, ServerHosts: []string{"b.example.com"},
			},
			{
				Namespace: "istio-system", Name: "gw-a", ValidFrom: sigT0, ValidTo: sigT1,
				SelectorKV: []string{"istio=ingress"}, ServerHosts: []string{"a.example.com"},
			},
		},
	}

	got := New(w, sigMid).ResolveIPToGateways(ip)
	if len(got) != 1 {
		t.Fatalf("candidates = %+v, want exactly the ingress-namespace gateway", got)
	}
	if got[0].Namespace != "istio-system" || got[0].Name != "gw-a" {
		t.Errorf("candidate = %s/%s, want istio-system/gw-a", got[0].Namespace, got[0].Name)
	}
}
