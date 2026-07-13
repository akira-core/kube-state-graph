//go:build oracle

// The by-construction oracle cross-check (task 9.1), ported from
// poc/route2a/internal/scalegen + matchcheck_scale_test.go: generate the same
// deterministic corpus (gw-NNN gateways, vs-NNN-JJ VirtualServices with
// exact/prefix/regex/catch-all routes, svc-NNN-JJ-<rule> backend Services),
// resolve every case through the PORTED gwresolve → translate →
// router_check_tool pipeline, and require zero mismatches against the
// expected clusters — which are computed purely by construction, independent
// of istiod, so the pipeline is validated against something it did not itself
// produce.
//
// Build-tagged `oracle` (long-running; the full POC scale is 600×100 = 60k VS
// and takes tens of minutes with the native tool):
//
//	go test -tags oracle ./pkg/route/ -run TestOracle -v -timeout 60m
//
// Scale is env-tunable (KSG_ORACLE_GATEWAYS / KSG_ORACLE_VS, defaults 600/100)
// so a reduced sweep can validate the machinery quickly. Requires a native
// router_check_tool (KSG_ROUTER_CHECK_BIN or PATH); skips otherwise.
package route

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvk"

	"github.com/akira-core/kube-state-graph/pkg/route/gwresolve"
	"github.com/akira-core/kube-state-graph/pkg/route/matchcheck"
	"github.com/akira-core/kube-state-graph/pkg/route/translate"
)

const (
	oracleSvcPort = 8080
	oracleGwNS    = "istio-system"
	oracleRegex   = "^/products/[0-9]+$"
)

func pad3(i int) string { return fmt.Sprintf("%03d", i) }
func pad2(j int) string { return fmt.Sprintf("%02d", j) }

func oGwName(i int) string    { return "gw-" + pad3(i) }
func oGwHostPat(i int) string { return "*.gw" + pad3(i) + ".example.com" }
func oVsHost(i, j int) string { return "svc" + pad2(j) + ".gw" + pad3(i) + ".example.com" }
func oVsNS(i int) string      { return oGwName(i) }

func oDestFQDN(i, j int, rule string) string {
	return "svc-" + pad3(i) + "-" + pad2(j) + "-" + rule + "." + oVsNS(i) + ".svc.cluster.local"
}
func oClusterOf(i, j int, rule string) string {
	return fmt.Sprintf("outbound|%d||%s", oracleSvcPort, oDestFQDN(i, j, rule))
}

func oExact(s string) *networking.StringMatch {
	return &networking.StringMatch{MatchType: &networking.StringMatch_Exact{Exact: s}}
}
func oPrefix(s string) *networking.StringMatch {
	return &networking.StringMatch{MatchType: &networking.StringMatch_Prefix{Prefix: s}}
}
func oRegex(s string) *networking.StringMatch {
	return &networking.StringMatch{MatchType: &networking.StringMatch_Regex{Regex: s}}
}

func oRoute(uri *networking.StringMatch, destHost string) *networking.HTTPRoute {
	r := &networking.HTTPRoute{
		Route: []*networking.HTTPRouteDestination{{
			Destination: &networking.Destination{Host: destHost, Port: &networking.PortSelector{Number: oracleSvcPort}},
		}},
	}
	if uri != nil {
		r.Match = []*networking.HTTPMatchRequest{{Uri: uri}}
	}
	return r
}

func oSvc(fqdn, ns string) *model.Service {
	return &model.Service{
		Hostname:       host.Name(fqdn),
		DefaultAddress: "0.0.0.0",
		Ports:          model.PortList{&model.Port{Name: "http", Port: oracleSvcPort, Protocol: protocol.HTTP}},
		Attributes:     model.ServiceAttributes{Namespace: ns},
	}
}

// oScopedFor builds gateway i's scoped translate input: its Gateway CR + vsPerGW
// VirtualServices + the 4*vsPerGW backend Services those VS route to.
func oScopedFor(i, vsPerGW int) translate.ScopedInput {
	cfgs := []config.Config{{
		Meta: config.Meta{GroupVersionKind: gvk.Gateway, Name: oGwName(i), Namespace: oracleGwNS},
		Spec: &networking.Gateway{
			Selector: map[string]string{"istio": oGwName(i)},
			Servers: []*networking.Server{{
				Port:  &networking.Port{Number: 80, Name: "http", Protocol: "HTTP"},
				Hosts: []string{oGwHostPat(i)},
			}},
		},
	}}
	var svcs []*model.Service
	for j := 0; j < vsPerGW; j++ {
		cfgs = append(cfgs, config.Config{
			Meta: config.Meta{GroupVersionKind: gvk.VirtualService, Name: "vs-" + pad3(i) + "-" + pad2(j), Namespace: oVsNS(i)},
			Spec: &networking.VirtualService{
				Hosts:    []string{oVsHost(i, j)},
				Gateways: []string{oracleGwNS + "/" + oGwName(i)},
				Http: []*networking.HTTPRoute{
					oRoute(oExact("/healthz"), oDestFQDN(i, j, "exact")),
					oRoute(oPrefix("/api/v1"), oDestFQDN(i, j, "prefix")),
					oRoute(oRegex(oracleRegex), oDestFQDN(i, j, "regex")),
					oRoute(nil, oDestFQDN(i, j, "default")),
				},
			},
		})
		for _, rule := range []string{"exact", "prefix", "regex", "default"} {
			svcs = append(svcs, oSvc(oDestFQDN(i, j, rule), oVsNS(i)))
		}
	}
	return translate.ScopedInput{
		Configs:  cfgs,
		Services: svcs,
		Proxy:    translate.GatewayProxy{Name: oGwName(i), Namespace: oracleGwNS, Labels: map[string]string{"istio": oGwName(i)}},
	}
}

// oracleCase is one by-construction query for gateway i.
type oracleCase struct {
	host, path, expected string
}

// oCases yields ≥1 case per VS, rotating the rule so every match kind is
// exercised (mirrors scalegen.Cases without the shuffle — batching is
// per-gateway anyway).
func oCases(i, vsPerGW int) []oracleCase {
	rules := []string{"exact", "prefix", "regex", "default"}
	paths := map[string]func(j int) string{
		"exact":   func(int) string { return "/healthz" },
		"prefix":  func(j int) string { return fmt.Sprintf("/api/v1/users/%d", j) },
		"regex":   func(j int) string { return fmt.Sprintf("/products/%d", j+1) },
		"default": func(j int) string { return fmt.Sprintf("/misc/%d", j) },
	}
	out := make([]oracleCase, 0, vsPerGW)
	for j := 0; j < vsPerGW; j++ {
		rule := rules[(i+j)%len(rules)]
		out = append(out, oracleCase{
			host:     oVsHost(i, j),
			path:     paths[rule](j),
			expected: oClusterOf(i, j, rule),
		})
	}
	return out
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func TestOracle(t *testing.T) {
	runner, err := matchcheck.NewRunner(os.Getenv("KSG_ROUTER_CHECK_BIN"))
	if err != nil {
		t.Skipf("router_check_tool unavailable: %v", err)
	}
	numGW := envInt("KSG_ORACLE_GATEWAYS", 600)
	vsPerGW := envInt("KSG_ORACLE_VS", 100)
	t.Logf("oracle sweep: %d gateways × %d VS", numGW, vsPerGW)

	// gwresolve over the whole corpus + a broad overlapping wildcard, so
	// most-specific disambiguation is exercised on every case.
	gws := make([]gwresolve.Gateway, 0, numGW+1)
	for i := 0; i < numGW; i++ {
		gws = append(gws, gwresolve.Gateway{Name: oGwName(i), Hosts: []string{oGwHostPat(i)}})
	}
	gws = append(gws, gwresolve.Gateway{Name: "gw-broad-all", Hosts: []string{"*.example.com"}})
	resolver := gwresolve.New(gws)

	tr := translate.NewTranslator()
	ctx := context.Background()
	mismatches := 0
	for i := 0; i < numGW; i++ {
		cases := oCases(i, vsPerGW)

		// Every case's host must resolve to ITS gateway (not the broad one).
		queries := make([]matchcheck.Query, len(cases))
		for k, c := range cases {
			gw, ok := resolver.Resolve(c.host)
			if !ok || gw != oGwName(i) {
				t.Fatalf("gwresolve(%s) = (%q,%v), want %q", c.host, gw, ok, oGwName(i))
			}
			queries[k] = matchcheck.Query{Host: c.host, Path: c.path}
		}

		scoped := oScopedFor(i, vsPerGW)
		scoped.Port = 80
		rc, err := tr.Translate(scoped)
		if err != nil {
			t.Fatalf("translate gw %d: %v", i, err)
		}
		got, err := runner.Resolve(ctx, rc, queries)
		if err != nil {
			t.Fatalf("router_check_tool gw %d: %v", i, err)
		}
		for k, c := range cases {
			if got[k] != c.expected {
				mismatches++
				if mismatches <= 10 {
					t.Errorf("gw %d case %d (%s %s): got %q, want %q", i, k, c.host, c.path, got[k], c.expected)
				}
			}
		}
		if i%50 == 0 {
			t.Logf("gateway %d/%d done (mismatches so far: %d)", i, numGW, mismatches)
		}
	}
	if mismatches != 0 {
		t.Fatalf("%d mismatches — the ported engine disagrees with the by-construction oracle", mismatches)
	}
	// Miss coverage: a broad-gateway host resolves to the broad gateway whose
	// scoped RC is empty; an unknown host resolves to no gateway at all.
	if gw, ok := resolver.Resolve("direct-1.example.com"); !ok || gw != "gw-broad-all" {
		t.Fatalf("broad host resolved to (%q,%v), want gw-broad-all", gw, ok)
	}
	if _, ok := resolver.Resolve("nope.unknown.invalid"); ok {
		t.Fatal("unknown host must resolve to no gateway")
	}
}
