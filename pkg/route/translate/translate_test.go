package translate_test

import (
	"runtime"
	"testing"

	envoyroute "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvk"

	"github.com/akira-core/kube-state-graph/pkg/route/translate"
)

// svc builds a minimal HTTP model.Service for the mem registry.
func svc(fqdn, ns string, port int) *model.Service {
	return &model.Service{
		Hostname:       host.Name(fqdn),
		DefaultAddress: "0.0.0.0",
		Ports: model.PortList{
			&model.Port{Name: "http", Port: port, Protocol: protocol.HTTP},
		},
		Attributes: model.ServiceAttributes{Namespace: ns},
	}
}

func gwConfig(name, ns string, selector map[string]string, hosts []string) config.Config {
	return config.Config{
		Meta: config.Meta{GroupVersionKind: gvk.Gateway, Name: name, Namespace: ns},
		Spec: &networking.Gateway{
			Selector: selector,
			Servers: []*networking.Server{{
				Port:  &networking.Port{Number: 80, Name: "http", Protocol: "HTTP"},
				Hosts: hosts,
			}},
		},
	}
}

func route(uri *networking.StringMatch, destHost string, port uint32) *networking.HTTPRoute {
	r := &networking.HTTPRoute{
		Route: []*networking.HTTPRouteDestination{{
			Destination: &networking.Destination{Host: destHost, Port: &networking.PortSelector{Number: port}},
		}},
	}
	if uri != nil {
		r.Match = []*networking.HTTPMatchRequest{{Uri: uri}}
	}
	return r
}

func matchStr(m *envoyroute.RouteMatch) string {
	switch p := m.GetPathSpecifier().(type) {
	case *envoyroute.RouteMatch_Prefix:
		return "prefix:" + p.Prefix
	case *envoyroute.RouteMatch_Path:
		return "exact:" + p.Path
	case *envoyroute.RouteMatch_SafeRegex:
		return "regex:" + p.SafeRegex.GetRegex()
	default:
		return "<other>"
	}
}

func exact(s string) *networking.StringMatch {
	return &networking.StringMatch{MatchType: &networking.StringMatch_Exact{Exact: s}}
}
func prefix(s string) *networking.StringMatch {
	return &networking.StringMatch{MatchType: &networking.StringMatch_Prefix{Prefix: s}}
}
func regex(s string) *networking.StringMatch {
	return &networking.StringMatch{MatchType: &networking.StringMatch_Regex{Regex: s}}
}

// translateOK runs Translate and fails the test on error (the POC's
// RoutesForScoped helper, folded into the test package so production code
// carries no test-only entry point).
func translateOK(t *testing.T, in translate.ScopedInput) *envoyroute.RouteConfiguration {
	t.Helper()
	rc, err := translate.NewTranslator().Translate(in)
	if err != nil {
		t.Fatalf("scoped translate: %v", err)
	}
	return rc
}

// TestRoutesForScopedSpike validates the scoped config-generator core path:
// build one ingress's config typed, translate, and assert each route's cluster
// equals the by-construction expected cluster.
func TestRoutesForScopedSpike(t *testing.T) {
	const ns = "gw-000"
	gw := gwConfig("gw-000", "istio-system", map[string]string{"istio": "gw-000"}, []string{"*.gw000.example.com"})

	// destination hosts (FQDNs) + the by-construction expected clusters.
	cl := func(short string) string {
		return "outbound|8080||" + short + "." + ns + ".svc.cluster.local"
	}
	dh := func(short string) string { return short + "." + ns + ".svc.cluster.local" }

	vs := config.Config{
		Meta: config.Meta{GroupVersionKind: gvk.VirtualService, Name: "svc0", Namespace: ns},
		Spec: &networking.VirtualService{
			Hosts:    []string{"svc0.gw000.example.com"},
			Gateways: []string{"istio-system/gw-000"},
			Http: []*networking.HTTPRoute{
				route(exact("/healthz"), dh("svc-000-0-exact"), 8080),
				route(prefix("/api/v1"), dh("svc-000-0-prefix"), 8080),
				route(regex("^/products/[0-9]+$"), dh("svc-000-0-regex"), 8080),
				route(nil, dh("svc-000-0-default"), 8080), // catch-all
			},
		},
	}

	services := []*model.Service{
		svc(dh("svc-000-0-exact"), ns, 8080),
		svc(dh("svc-000-0-prefix"), ns, 8080),
		svc(dh("svc-000-0-regex"), ns, 8080),
		svc(dh("svc-000-0-default"), ns, 8080),
	}

	rc := translateOK(t, translate.ScopedInput{
		Configs:  []config.Config{gw, vs},
		Services: services,
		Proxy:    translate.GatewayProxy{Name: "gw-000", Namespace: "istio-system", Labels: map[string]string{"istio": "gw-000"}},
	})

	if rc.GetName() != "http.80" {
		t.Fatalf("rc name = %q, want %q", rc.GetName(), "http.80")
	}

	// collect match->cluster
	got := map[string]string{}
	for _, vh := range rc.GetVirtualHosts() {
		for _, rt := range vh.GetRoutes() {
			got[matchStr(rt.GetMatch())] = rt.GetRoute().GetCluster()
		}
	}

	want := map[string]string{
		"exact:/healthz":           cl("svc-000-0-exact"),
		"prefix:/api/v1":           cl("svc-000-0-prefix"),
		"regex:^/products/[0-9]+$": cl("svc-000-0-regex"),
		"prefix:/":                 cl("svc-000-0-default"), // catch-all becomes prefix:/
	}
	for m, wantCluster := range want {
		if got[m] != wantCluster {
			t.Errorf("match %s: cluster = %q, want %q", m, got[m], wantCluster)
		}
	}
}

// TestTranslatorNoGoroutineLeak asserts repeated translation does not accumulate
// goroutines: the resolver translates once per distinct config per build, so a
// leak here would grow unbounded in a long-lived API server.
func TestTranslatorNoGoroutineLeak(t *testing.T) {
	const ns = "gw-000"
	dh := "svc-000-0-exact." + ns + ".svc.cluster.local"
	in := translate.ScopedInput{
		Configs: []config.Config{
			gwConfig("gw-000", "istio-system", map[string]string{"istio": "gw-000"}, []string{"*.gw000.example.com"}),
			{
				Meta: config.Meta{GroupVersionKind: gvk.VirtualService, Name: "svc0", Namespace: ns},
				Spec: &networking.VirtualService{
					Hosts:    []string{"svc0.gw000.example.com"},
					Gateways: []string{"istio-system/gw-000"},
					Http:     []*networking.HTTPRoute{route(exact("/healthz"), dh, 8080)},
				},
			},
		},
		Services: []*model.Service{svc(dh, ns, 8080)},
		Proxy:    translate.GatewayProxy{Name: "gw-000", Namespace: "istio-system", Labels: map[string]string{"istio": "gw-000"}},
	}
	tr := translate.NewTranslator()

	if _, err := tr.Translate(in); err != nil { // warm up
		t.Fatal(err)
	}
	runtime.GC()
	before := runtime.NumGoroutine()

	for i := range 200 {
		if _, err := tr.Translate(in); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	runtime.GC()
	after := runtime.NumGoroutine()

	if after > before+10 {
		t.Fatalf("goroutine growth after 200 translations: before=%d after=%d", before, after)
	}
}

// TestMultiPortBackendServiceClusters proves a single backend Service carrying
// MULTIPLE ports (the shape spec_json stores — one row/one model.Service with a
// multi-port PortList) is usable on every port: routing to each port resolves
// to that port's own outbound|<port>||<fqdn> cluster.
func TestMultiPortBackendServiceClusters(t *testing.T) {
	const ns = "gw-000"
	dh := "svc-multi." + ns + ".svc.cluster.local"

	gw := gwConfig("gw-000", "istio-system", map[string]string{"istio": "gw-000"}, []string{"*.gw000.example.com"})
	vs := config.Config{
		Meta: config.Meta{GroupVersionKind: gvk.VirtualService, Name: "vs-multi", Namespace: ns},
		Spec: &networking.VirtualService{
			Hosts:    []string{"svc0.gw000.example.com"},
			Gateways: []string{"istio-system/gw-000"},
			Http: []*networking.HTTPRoute{
				route(exact("/p8080"), dh, 8080),
				route(exact("/p8443"), dh, 8443),
			},
		},
	}
	// One Service, both ports.
	multi := &model.Service{
		Hostname:       host.Name(dh),
		DefaultAddress: "0.0.0.0",
		Ports: model.PortList{
			&model.Port{Name: "http", Port: 8080, Protocol: protocol.HTTP},
			&model.Port{Name: "http-alt", Port: 8443, Protocol: protocol.HTTP},
		},
		Attributes: model.ServiceAttributes{Namespace: ns},
	}

	rc := translateOK(t, translate.ScopedInput{
		Configs:  []config.Config{gw, vs},
		Services: []*model.Service{multi},
		Proxy:    translate.GatewayProxy{Name: "gw-000", Namespace: "istio-system", Labels: map[string]string{"istio": "gw-000"}},
	})

	got := map[string]string{}
	for _, vh := range rc.GetVirtualHosts() {
		for _, rt := range vh.GetRoutes() {
			got[matchStr(rt.GetMatch())] = rt.GetRoute().GetCluster()
		}
	}
	want := map[string]string{
		"exact:/p8080": "outbound|8080||" + dh,
		"exact:/p8443": "outbound|8443||" + dh,
	}
	for m, wantCluster := range want {
		if got[m] != wantCluster {
			t.Errorf("match %s: cluster = %q, want %q (multi-port service must expose both ports)", m, got[m], wantCluster)
		}
	}
}

// gwConfigHTTPSAndHTTP builds a gateway with BOTH a plain HTTP :80 server and a
// TLS-terminated HTTPS :443 server, sharing the same host pattern. tls controls
// the 443 server's TLS mode.
func gwConfigHTTPSAndHTTP(name, ns string, selector map[string]string, hosts []string, tls networking.ServerTLSSettings_TLSmode) config.Config {
	return config.Config{
		Meta: config.Meta{GroupVersionKind: gvk.Gateway, Name: name, Namespace: ns},
		Spec: &networking.Gateway{
			Selector: selector,
			Servers: []*networking.Server{
				{Port: &networking.Port{Number: 80, Name: "http", Protocol: "HTTP"}, Hosts: hosts},
				{
					Port:  &networking.Port{Number: 443, Name: "https", Protocol: "HTTPS"},
					Hosts: hosts,
					Tls:   &networking.ServerTLSSettings{Mode: tls, CredentialName: "cred-000"},
				},
			},
		},
	}
}

// TestTranslatePortSelectsRouteConfig proves ScopedInput.Port selects the RC:
// :80 -> "http.80", TLS-terminated :443 -> "https.443.https.<gw>.<ns>" (same
// clusters as :80, since VS routing is port-independent), and an unserved port
// (:8080) or a passthrough :443 server -> an empty RC (a miss). This is the
// design-D5 behaviour the route_engine_no_listener_on_port reason keys off.
func TestTranslatePortSelectsRouteConfig(t *testing.T) {
	const ns = "gw-000"
	dh := "svc-000-0-exact." + ns + ".svc.cluster.local"
	wantCluster := "outbound|8080||" + dh

	vs := config.Config{
		Meta: config.Meta{GroupVersionKind: gvk.VirtualService, Name: "svc0", Namespace: ns},
		Spec: &networking.VirtualService{
			Hosts:    []string{"svc0.gw000.example.com"},
			Gateways: []string{"istio-system/gw-000"},
			Http:     []*networking.HTTPRoute{route(exact("/healthz"), dh, 8080)},
		},
	}
	services := []*model.Service{svc(dh, ns, 8080)}
	proxy := translate.GatewayProxy{Name: "gw-000", Namespace: "istio-system", Labels: map[string]string{"istio": "gw-000"}}

	clusterFor := func(rc *envoyroute.RouteConfiguration) string {
		for _, vh := range rc.GetVirtualHosts() {
			for _, rt := range vh.GetRoutes() {
				if matchStr(rt.GetMatch()) == "exact:/healthz" {
					return rt.GetRoute().GetCluster()
				}
			}
		}
		return ""
	}

	tr := translate.NewTranslator()

	// Host stays empty throughout: the host-agnostic path must keep today's
	// byte-for-byte behaviour.
	cases := []struct {
		name       string
		port       int
		tls        networking.ServerTLSSettings_TLSmode
		wantName   string
		wantHit    bool // true => the /healthz route resolves to wantCluster
		wantStatus translate.ListenerStatus
	}{
		{"http_80", 80, networking.ServerTLSSettings_SIMPLE, "http.80", true, translate.ListenerFound},
		{"https_443_terminated", 443, networking.ServerTLSSettings_SIMPLE, "https.443.https.gw-000.istio-system", true, translate.ListenerFound},
		{"unserved_8080_miss", 8080, networking.ServerTLSSettings_SIMPLE, "http.8080", false, translate.ListenerNoneOnPort},
		{"passthrough_443_miss", 443, networking.ServerTLSSettings_PASSTHROUGH, "http.443", false, translate.ListenerNoneOnPort},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gw := gwConfigHTTPSAndHTTP("gw-000", "istio-system", map[string]string{"istio": "gw-000"}, []string{"*.gw000.example.com"}, c.tls)
			in := translate.ScopedInput{
				Configs:  []config.Config{gw, vs},
				Services: services,
				Proxy:    proxy,
				Port:     c.port,
			}
			rc, err := tr.Translate(in)
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			if rc.GetName() != c.wantName {
				t.Errorf("rc name = %q, want %q", rc.GetName(), c.wantName)
			}
			got := clusterFor(rc)
			if c.wantHit {
				if got != wantCluster {
					t.Errorf("/healthz cluster = %q, want %q", got, wantCluster)
				}
			} else if got != "" {
				t.Errorf("/healthz cluster = %q, want miss (empty RC)", got)
			}
			if _, st := translate.ListenerFor(in); st != c.wantStatus {
				t.Errorf("ListenerFor(port=%d) status = %v, want %v", c.port, st, c.wantStatus)
			}
		})
	}
}

// --- host-aware server selection fixtures -----------------------------------

// tlsServer is a TLS-terminated HTTPS :443 server.
func tlsServer(portName string, hosts []string, bind string) *networking.Server {
	return &networking.Server{
		Port:  &networking.Port{Number: 443, Name: portName, Protocol: "HTTPS"},
		Hosts: hosts,
		Bind:  bind,
		Tls:   &networking.ServerTLSSettings{Mode: networking.ServerTLSSettings_SIMPLE, CredentialName: "cred-" + portName},
	}
}

// passthroughServer is an HTTPS :443 passthrough server (no HTTP RDS route).
// Note the zero value of ServerTLSSettings_TLSmode IS passthrough; it is
// spelled out for the reader.
func passthroughServer(portName string, hosts []string) *networking.Server {
	return &networking.Server{
		Port:  &networking.Port{Number: 443, Name: portName, Protocol: "HTTPS"},
		Hosts: hosts,
		Tls:   &networking.ServerTLSSettings{Mode: networking.ServerTLSSettings_PASSTHROUGH},
	}
}

// httpServer is a plain HTTP server — Tls MUST stay nil (a non-nil Tls with
// the zero mode would silently mean passthrough).
func httpServer(portName string, port uint32, hosts []string, bind string) *networking.Server {
	return &networking.Server{
		Port:  &networking.Port{Number: port, Name: portName, Protocol: "HTTP"},
		Hosts: hosts,
		Bind:  bind,
	}
}

func gwWithServers(servers ...*networking.Server) config.Config {
	return config.Config{
		Meta: config.Meta{GroupVersionKind: gvk.Gateway, Name: "gw-000", Namespace: "istio-system"},
		Spec: &networking.Gateway{Selector: map[string]string{"istio": "gw-000"}, Servers: servers},
	}
}

func reversed(servers []*networking.Server) []*networking.Server {
	out := make([]*networking.Server, len(servers))
	for i, s := range servers {
		out[len(servers)-1-i] = s
	}
	return out
}

// TestListenerForSelectsServerByHost proves RC selection is host-aware and
// order-independent: every case runs in both declaration orders and must pick
// the same server (Istio exact/wildcard specificity, not first-on-port).
func TestListenerForSelectsServerByHost(t *testing.T) {
	proxy := translate.GatewayProxy{Name: "gw-000", Namespace: "istio-system", Labels: map[string]string{"istio": "gw-000"}}

	cases := []struct {
		name       string
		servers    []*networking.Server
		port       int
		host       string
		wantName   string
		wantStatus translate.ListenerStatus
	}{
		{
			name:       "https_two_servers_admin",
			servers:    []*networking.Server{tlsServer("api", []string{"api.example.com"}, ""), tlsServer("admin", []string{"admin.example.com"}, "")},
			port:       443,
			host:       "admin.example.com",
			wantName:   "https.443.admin.gw-000.istio-system",
			wantStatus: translate.ListenerFound,
		},
		{
			name:       "https_two_servers_api",
			servers:    []*networking.Server{tlsServer("api", []string{"api.example.com"}, ""), tlsServer("admin", []string{"admin.example.com"}, "")},
			port:       443,
			host:       "api.example.com",
			wantName:   "https.443.api.gw-000.istio-system",
			wantStatus: translate.ListenerFound,
		},
		{
			name:       "exact_beats_wildcard",
			servers:    []*networking.Server{tlsServer("wild", []string{"*.example.com"}, ""), tlsServer("api", []string{"api.example.com"}, "")},
			port:       443,
			host:       "api.example.com",
			wantName:   "https.443.api.gw-000.istio-system",
			wantStatus: translate.ListenerFound,
		},
		{
			name:       "more_specific_wildcard_wins",
			servers:    []*networking.Server{tlsServer("broad", []string{"*.example.com"}, ""), tlsServer("narrow", []string{"*.api.example.com"}, "")},
			port:       443,
			host:       "v1.api.example.com",
			wantName:   "https.443.narrow.gw-000.istio-system",
			wantStatus: translate.ListenerFound,
		},
		{
			name:       "http_bind_in_name",
			servers:    []*networking.Server{httpServer("http", 80, []string{"*"}, "10.0.0.2")},
			port:       80,
			host:       "api.example.com",
			wantName:   "http.80.10.0.0.2",
			wantStatus: translate.ListenerFound,
		},
		{
			name:       "https_bind_in_name",
			servers:    []*networking.Server{tlsServer("api", []string{"api.example.com"}, "10.0.0.1")},
			port:       443,
			host:       "api.example.com",
			wantName:   "https.443.api.gw-000.istio-system.10.0.0.1",
			wantStatus: translate.ListenerFound,
		},
		{
			name:       "ns_prefixed_server_host",
			servers:    []*networking.Server{tlsServer("api", []string{"prod/api.example.com"}, "")},
			port:       443,
			host:       "api.example.com",
			wantName:   "https.443.api.gw-000.istio-system",
			wantStatus: translate.ListenerFound,
		},
		{
			name:       "https_no_server_for_host",
			servers:    []*networking.Server{tlsServer("api", []string{"api.example.com"}, ""), tlsServer("admin", []string{"admin.example.com"}, "")},
			port:       443,
			host:       "other.example.com",
			wantName:   "",
			wantStatus: translate.ListenerNoServerForHost,
		},
		{
			name:       "http_no_server_for_host",
			servers:    []*networking.Server{httpServer("http", 80, []string{"api.example.com"}, "")},
			port:       80,
			host:       "other.example.com",
			wantName:   "",
			wantStatus: translate.ListenerNoServerForHost,
		},
		{
			name:       "http_servers_share_rc",
			servers:    []*networking.Server{httpServer("http-a", 80, []string{"api.example.com"}, ""), httpServer("http-b", 80, []string{"admin.example.com"}, "")},
			port:       80,
			host:       "admin.example.com",
			wantName:   "http.80",
			wantStatus: translate.ListenerFound,
		},
		{
			name:       "passthrough_wins_sni",
			servers:    []*networking.Server{passthroughServer("pass", []string{"api.example.com"}), tlsServer("wild", []string{"*.example.com"}, "")},
			port:       443,
			host:       "api.example.com",
			wantName:   "",
			wantStatus: translate.ListenerNoneOnPort,
		},
		{
			name:       "no_listener_on_port",
			servers:    []*networking.Server{tlsServer("api", []string{"api.example.com"}, "")},
			port:       8080,
			host:       "api.example.com",
			wantName:   "",
			wantStatus: translate.ListenerNoneOnPort,
		},
		{
			name:       "star_catch_all",
			servers:    []*networking.Server{tlsServer("all", []string{"*"}, ""), tlsServer("api", []string{"api.example.com"}, "")},
			port:       443,
			host:       "random.host.net",
			wantName:   "https.443.all.gw-000.istio-system",
			wantStatus: translate.ListenerFound,
		},
	}
	for _, c := range cases {
		orders := []struct {
			name    string
			servers []*networking.Server
		}{
			{"declared", c.servers},
			{"reversed", reversed(c.servers)},
		}
		for _, o := range orders {
			t.Run(c.name+"/"+o.name, func(t *testing.T) {
				in := translate.ScopedInput{
					Configs: []config.Config{gwWithServers(o.servers...)},
					Proxy:   proxy,
					Port:    c.port,
					Host:    c.host,
				}
				name, st := translate.ListenerFor(in)
				if st != c.wantStatus {
					t.Errorf("ListenerFor status = %v, want %v", st, c.wantStatus)
				}
				if st == translate.ListenerFound && name != c.wantName {
					t.Errorf("ListenerFor name = %q, want %q", name, c.wantName)
				}
			})
		}
	}
}

// TestTranslateHTTPSServerByHostEndToEnd is the case port-only selection
// silently misses: two TLS-terminated :443 servers, each with its own VS —
// asking for one host must yield THAT server's RC and THAT host's backend
// cluster, in either declaration order.
func TestTranslateHTTPSServerByHostEndToEnd(t *testing.T) {
	const ns = "gw-000"
	apiDH := "api-backend." + ns + ".svc.cluster.local"
	adminDH := "admin-backend." + ns + ".svc.cluster.local"

	vsFor := func(name, vhost, destHost string) config.Config {
		return config.Config{
			Meta: config.Meta{GroupVersionKind: gvk.VirtualService, Name: name, Namespace: ns},
			Spec: &networking.VirtualService{
				Hosts:    []string{vhost},
				Gateways: []string{"istio-system/gw-000"},
				Http:     []*networking.HTTPRoute{route(nil, destHost, 8080)},
			},
		}
	}
	vsAPI := vsFor("vs-api", "api.example.com", apiDH)
	vsAdmin := vsFor("vs-admin", "admin.example.com", adminDH)
	services := []*model.Service{svc(apiDH, ns, 8080), svc(adminDH, ns, 8080)}
	proxy := translate.GatewayProxy{Name: "gw-000", Namespace: "istio-system", Labels: map[string]string{"istio": "gw-000"}}
	servers := []*networking.Server{
		tlsServer("api", []string{"api.example.com"}, ""),
		tlsServer("admin", []string{"admin.example.com"}, ""),
	}

	cases := []struct {
		host, wantName, wantCluster, otherCluster string
	}{
		{"api.example.com", "https.443.api.gw-000.istio-system", "outbound|8080||" + apiDH, "outbound|8080||" + adminDH},
		{"admin.example.com", "https.443.admin.gw-000.istio-system", "outbound|8080||" + adminDH, "outbound|8080||" + apiDH},
	}
	for _, c := range cases {
		for _, o := range []struct {
			name    string
			servers []*networking.Server
		}{
			{"declared", servers},
			{"reversed", reversed(servers)},
		} {
			t.Run(c.host+"/"+o.name, func(t *testing.T) {
				rc := translateOK(t, translate.ScopedInput{
					Configs:  []config.Config{gwWithServers(o.servers...), vsAPI, vsAdmin},
					Services: services,
					Proxy:    proxy,
					Port:     443,
					Host:     c.host,
				})
				if rc.GetName() != c.wantName {
					t.Fatalf("rc name = %q, want %q", rc.GetName(), c.wantName)
				}
				clusters := map[string]bool{}
				for _, vh := range rc.GetVirtualHosts() {
					for _, rt := range vh.GetRoutes() {
						clusters[rt.GetRoute().GetCluster()] = true
					}
				}
				if !clusters[c.wantCluster] {
					t.Errorf("RC %s lacks the host's own backend cluster %q (got %v)", rc.GetName(), c.wantCluster, clusters)
				}
				if clusters[c.otherCluster] {
					t.Errorf("RC %s leaked the OTHER server's backend cluster %q", rc.GetName(), c.otherCluster)
				}
			})
		}
	}
}
