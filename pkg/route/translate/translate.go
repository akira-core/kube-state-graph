// Package translate turns one ingress gateway's scoped Istio config (its single
// Gateway CR + the VirtualServices bound to it + the Services those VS route to)
// into the Envoy RouteConfiguration istiod would push to that gateway for a
// given listener port. It drives istio's config-generator core directly — no
// istiod process, no FakeDiscoveryServer, no whole-cluster load — mirroring what
// the "host+path -> service" query engine loads per gateway. Ported verbatim
// from poc/route2a/internal/translate.
//
// The RouteConfiguration to build is selected by ScopedInput.Port and
// ScopedInput.Host (default port 80): among the servers on the port, the one
// whose hosts most-specifically match the request host (Istio exact/wildcard
// semantics, declaration-order independent) owns the RC — a plain HTTP server
// yields "http.<port>[.<bind>]", a TLS-terminated HTTPS server yields
// "https.<port>.<portName>.<gwName>.<gwNamespace>[.<bind>]" (see
// routeConfigNameFor / rdsRouteName). TLS passthrough servers have no HTTP RDS
// route and resolve to a miss; a port whose servers serve other hosts only is
// the distinct ListenerNoServerForHost miss.
//
// istio's TEST fixture core.NewConfigGenTest is deliberately avoided: it ties
// its stop channel to t.Cleanup and builds a fake Kubernetes client for the
// ServiceEntry registry. Translator assembles the minimal config-generator world
// by hand and tears it down when the call returns, so nothing outlives a
// translation. This is also the design-D0 discipline: no Kubernetes client,
// informer, watch, or kubeconfig is ever constructed here.
//
// The world is no longer entirely static. istio moved VirtualService merging
// into a krt-backed controller that PushContext.InitContext consults, and krt
// collections are populated by event delivery rather than read straight from the
// store — so buildScopedEnv Creates the configs, starts the controllers, WAITS
// for them to sync, and returns a cleanup that reclaims their goroutines.
// Gateways are still listed directly from the store. See buildScopedEnv.
package translate

import (
	"fmt"
	"time"

	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	networking "istio.io/api/networking/v1alpha3"

	configaggregate "istio.io/istio/pilot/pkg/config/aggregate"
	"istio.io/istio/pilot/pkg/config/memory"
	"istio.io/istio/pilot/pkg/model"
	core "istio.io/istio/pilot/pkg/networking/core"
	"istio.io/istio/pilot/pkg/serviceregistry"
	"istio.io/istio/pilot/pkg/serviceregistry/aggregate"
	memregistry "istio.io/istio/pilot/pkg/serviceregistry/memory"
	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	cluster2 "istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/gateway"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"

	"github.com/akira-core/kube-state-graph/pkg/route/gwresolve"
)

// GatewayProxy identifies one ingress-gateway vantage. Labels must equal the
// Gateway resource's spec.selector so istiod attaches that Gateway to the
// synthetic proxy.
type GatewayProxy struct {
	Name      string
	Namespace string
	Labels    map[string]string
}

// ScopedInput is everything needed to translate ONE ingress gateway's
// RouteConfiguration for one listener port: its config (exactly one Gateway CR +
// the VirtualServices bound to it) and the Services those VS route to.
// Proxy.Labels must equal the Gateway's spec.selector so istiod attaches that
// Gateway to the vantage. Port selects which listener's RC to build (0 => 80).
// Host is the request FQDN — the same class of request-derived scalar as Port,
// arriving via the same path — and picks WHICH server on the port owns the RC
// (each TLS-terminated HTTPS server has its own). A caller resolving a real
// request MUST set it; "" is the host-agnostic escape hatch (first server on
// the port wins, the pre-host-aware behaviour).
type ScopedInput struct {
	Configs  []config.Config
	Services []*model.Service
	Proxy    GatewayProxy
	Port     int
	Host     string
}

// Translator is a process-lifetime, concurrency-safe translation core. The only
// state it holds is stateless/reusable (the config generator + default mesh);
// every Translate call builds its own throwaway world, so calls share nothing
// mutable and may run in parallel.
type Translator struct {
	configGen *core.ConfigGeneratorImpl
}

// NewTranslator builds the reusable config generator once. Cheap; safe to keep
// one per process (or one per goroutine — it carries no per-request state).
func NewTranslator() *Translator {
	return &Translator{
		configGen: core.NewConfigGenerator(&model.DisabledCache{}),
	}
}

// Translate runs istio's config-generator core over one ingress's scoped config
// and returns the RouteConfiguration istiod would push to it for the listener
// port in.Port (default 80). It starts no goroutines: the scoped world is
// assembled statically and discarded when this returns.
func (tr *Translator) Translate(in ScopedInput) (*route.RouteConfiguration, error) {
	port := in.Port
	if port <= 0 {
		port = 80
	}
	if _, ok := gatewayConfig(in.Configs); !ok {
		return nil, fmt.Errorf("translate: scoped input has no Gateway config")
	}
	name, st := ListenerFor(in)
	if st != ListenerFound {
		// No HTTP RDS route for (port, host): no server on the port, a
		// TLS-passthrough server winning the host, or no server on the port
		// serving the host — faithfully a miss (empty RC).
		return &route.RouteConfiguration{Name: fmt.Sprintf("http.%d", port)}, nil
	}

	env, cleanup, err := buildScopedEnv(in)
	if err != nil {
		return nil, err
	}
	// The scoped world is discarded when this returns — including the krt
	// collections' goroutines.
	defer cleanup()
	pc := env.PushContext()

	proxy := setupProxy(env, pc, in.Proxy)

	res, _ := tr.configGen.BuildHTTPRoutes(proxy, &model.PushRequest{Push: pc}, []string{name})
	for _, r := range res {
		rc := &route.RouteConfiguration{}
		if err := r.Resource.UnmarshalTo(rc); err != nil {
			return nil, err
		}
		if rc.GetName() == name {
			return rc, nil
		}
	}
	// No VS matched: an empty (all-miss) RC is the faithful result.
	return &route.RouteConfiguration{Name: name}, nil
}

// ListenerStatus classifies ListenerFor's answer: does the scoped Gateway
// declare a routable HTTP listener for (port, host)?
type ListenerStatus int

const (
	// ListenerFound — a server on the port serves the host and has an HTTP
	// RDS route; Translate would build a real RouteConfiguration.
	ListenerFound ListenerStatus = iota
	// ListenerNoneOnPort — no server listens on the port, or the server that
	// wins the host on it has no HTTP RDS route (TLS passthrough, TCP). The
	// design-D5 mis-guessed-port signature.
	ListenerNoneOnPort
	// ListenerNoServerForHost — servers listen on the port, but none of their
	// hosts match the request host. Distinct from ListenerNoneOnPort so a
	// wrong port guess is not conflated with a host the listener never
	// served; ending here is sound — istiod builds each vhost from the
	// intersection of server hosts and VS hosts (buildGatewayHTTPRouteConfig
	// keeps the more specific of the two), so a host matching no server host
	// can match no vhost either: translating anyway could only reach
	// RouteNoRoute, slower.
	ListenerNoServerForHost
)

// ListenerFor is the ONE RouteConfiguration-selection decision point: it
// returns the RC name the scoped Gateway assigns (in.Port, in.Host) and the
// tri-state listener status. Translate and the resolver's listener gate both
// go through it, so the two can never diverge. Pure config inspection — no
// istiod translate, no subprocess.
func ListenerFor(in ScopedInput) (string, ListenerStatus) {
	gwCfg, ok := gatewayConfig(in.Configs)
	if !ok {
		return "", ListenerNoneOnPort
	}
	return routeConfigNameFor(gwCfg, in.Port, in.Host)
}

// gatewayConfig returns the single Gateway CR from a scoped input's configs.
func gatewayConfig(configs []config.Config) (config.Config, bool) {
	for _, c := range configs {
		if c.GroupVersionKind == gvk.Gateway {
			return c, true
		}
	}
	return config.Config{}, false
}

// rdsRouteName is a line-for-line port of istio's (unexported)
// gatewayRDSRouteName (pilot/pkg/model/gateway.go), INCLUDING the bind suffix:
// "http.<port>[.<bind>]" for a plain HTTP server,
// "https.<port>.<portName>.<gwName>.<gwNamespace>[.<bind>]" for a
// TLS-terminated HTTPS server, "" otherwise (passthrough/TCP — no HTTP RDS
// route). Two upstream facts to preserve on any future edit: (a) upstream uses
// the RESOLVED portNumber for HTTP but s.Port.Number for HTTPS — here the two
// coincide because every caller pre-filters servers on s.Port.Number == port;
// (b) do NOT swap the HTTPS condition for gateway.IsHTTPSServerWithTLSTermination
// — it adds a Tls != nil guard, while IsPassThroughServer returns false for
// Tls == nil, so the two disagree on an HTTPS server with tls: nil. We mirror
// the RC NAME rule, not the filter-chain branch.
func rdsRouteName(s *networking.Server, port int, gwCfg config.Config) string {
	p := protocol.Parse(s.Port.Protocol)
	bind := ""
	if s.Bind != "" {
		bind = "." + s.Bind
	}
	if p.IsHTTP() {
		return fmt.Sprintf("http.%d", port) + bind
	}
	if p == protocol.HTTPS && !gateway.IsPassThroughServer(s) {
		return fmt.Sprintf("https.%d.%s.%s.%s", s.Port.Number, s.Port.Name, gwCfg.Name, gwCfg.Namespace) + bind
	}
	return ""
}

// routeConfigNameFor computes the Envoy RouteConfiguration name istiod assigns
// for (port, reqHost): among the servers on the port, the one whose hosts
// most-specifically match reqHost (Istio exact/wildcard semantics via
// gwresolve.PickHosts, declaration-order independent) owns the RC — each
// TLS-terminated HTTPS server carries its OWN RC (one filter chain per
// cert/SNI), plain HTTP servers share "http.<port>[.<bind>]". reqHost == "" is
// the host-agnostic escape hatch (first server on the port). port<=0 => 80.
//
// The on-port server set is deliberately NOT protocol-filtered before the host
// pick: if a TLS-passthrough server matches the host more specifically, istio
// hands it the filter chain — it must win the selection and then report the
// miss (no HTTP RDS route); filtering it out would invent a route Envoy never
// serves.
func routeConfigNameFor(gwCfg config.Config, port int, reqHost string) (string, ListenerStatus) {
	if port <= 0 {
		port = 80
	}
	gw, _ := gwCfg.Spec.(*networking.Gateway)
	if gw == nil {
		return "", ListenerNoneOnPort
	}
	var onPort []*networking.Server
	for _, s := range gw.Servers {
		if s.GetPort() != nil && int(s.Port.Number) == port {
			onPort = append(onPort, s)
		}
	}
	if len(onPort) == 0 {
		return "", ListenerNoneOnPort
	}
	winner := onPort[0]
	if reqHost != "" {
		hostSets := make([][]string, len(onPort))
		for i, s := range onPort {
			hostSets[i] = s.Hosts
		}
		idx, ok := gwresolve.PickHosts(hostSets, reqHost)
		if !ok {
			return "", ListenerNoServerForHost
		}
		winner = onPort[idx]
	}
	name := rdsRouteName(winner, port, gwCfg)
	if name == "" {
		// The winning server has no HTTP RDS route (passthrough/TCP).
		return "", ListenerNoneOnPort
	}
	return name, ListenerFound
}

// buildScopedEnv assembles the minimal static Environment for one gateway's
// scoped config, mirroring core.NewConfigGenTest. The returned cleanup MUST be
// called: it stops the controllers this starts.
//
// The service registry is the in-memory one ALONE. istiod's own test harness
// also wires a ServiceEntry registry, but this translator never receives a
// ServiceEntry: ScopedInput.Configs carries Gateway + VirtualService only, and
// every backend Service arrives through in.Services. Constructing one now
// requires a *multicluster.Controller, which would mean building Kubernetes
// clients — forbidden here (design D0) — so the registry that was never
// consulted is gone rather than faked.
func buildScopedEnv(in ScopedInput) (*model.Environment, func(), error) {
	// NewController, not the removed NewSyncController: the memory store is
	// built on krt now and no longer distinguishes a synchronous monitor.
	cc := memory.NewController(memory.MakeSkipValidation(collections.PilotGatewayAPI()))
	configController, _ := configaggregate.MakeWriteableCache([]model.ConfigStoreController{cc}, cc)

	// Populate the store FIRST. Ordering is now load-bearing: VirtualService
	// merging moved out of PushContext into a krt-backed controller whose
	// derived collections take their initial contents from the source at
	// CONSTRUCTION. Building it over an empty store and creating configs
	// afterwards leaves MergedVirtualServices empty at InitContext — every route
	// resolves to no cluster. (InitContext still lists Gateways straight from
	// the store, so that half is unchanged.)
	for _, cfg := range in.Configs {
		if _, err := configController.Create(cfg); err != nil {
			return nil, nil, err
		}
	}

	m := mesh.DefaultMeshConfig()
	env := model.NewEnvironment()
	env.Watcher = meshwatcher.NewTestWatcher(m)

	xdsUpdater := model.NewEndpointIndexUpdater(env.EndpointIndex)

	// InitContext dereferences VirtualServiceController unconditionally, so it
	// must exist. Its krt collections own goroutines that live until the
	// controller's internal stop channel closes, and Run(stop) is precisely the
	// shutdown hook that closes it — hence the returned cleanup. Without it each
	// translation would leak its collections' goroutines.
	vsController := model.NewVirtualServiceController(
		configController,
		model.VSControllerOptions{XDSUpdater: xdsUpdater},
		env.Watcher,
	)
	done := make(chan struct{})
	go configController.Run(done)
	go vsController.Run(done)
	cleanup := func() { close(done) }
	if err := waitSynced(configController.HasSynced, vsController.HasSynced); err != nil {
		cleanup()
		return nil, nil, err
	}

	serviceDiscovery := aggregate.NewController(aggregate.Options{})

	msd := memregistry.NewServiceDiscovery(in.Services...)
	msd.XdsUpdater = xdsUpdater
	msd.ClusterID = cluster2.ID(provider.Mock)
	serviceDiscovery.AddRegistry(serviceregistry.Simple{
		ClusterID:           cluster2.ID(provider.Mock),
		ProviderID:          provider.Mock,
		DiscoveryController: msd,
	})

	env.ServiceDiscovery = serviceDiscovery
	env.ConfigStore = configController
	env.VirtualServiceController = vsController
	env.NetworksWatcher = meshwatcher.NewFixedNetworksWatcher(nil)
	env.Init()

	if err := env.InitNetworksManager(xdsUpdater); err != nil {
		cleanup()
		return nil, nil, err
	}
	env.PushContext().InitContext(env, nil, nil)
	return env, cleanup, nil
}

// waitSynced blocks until every krt-backed controller reports synced, or gives
// up. The collections are populated by event delivery, so InitContext must not
// run before they have caught up — an unsynced MergedVirtualServices lists
// nothing and every route resolves to no cluster.
func waitSynced(checks ...func() bool) error {
	deadline := time.Now().Add(syncTimeout)
	for {
		synced := true
		for _, ok := range checks {
			if !ok() {
				synced = false
				break
			}
		}
		if synced {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("translate: scoped config controllers did not sync within %s", syncTimeout)
		}
		time.Sleep(syncPoll)
	}
}

const (
	syncTimeout = 5 * time.Second
	syncPoll    = 100 * time.Microsecond
)

// setupProxy fills the same proxy defaults core.ConfigGenTest.SetupProxy does
// for a gateway vantage, then wires it to the freshly built PushContext.
func setupProxy(env *model.Environment, pc *model.PushContext, gw GatewayProxy) *model.Proxy {
	p := &model.Proxy{
		Type:            model.Router,
		ConfigNamespace: gw.Namespace,
		Labels:          gw.Labels,
		Metadata:        &model.NodeMetadata{Namespace: gw.Namespace, Labels: gw.Labels},
	}
	if p.Metadata.IstioVersion == "" {
		p.Metadata.IstioVersion = "1.23.0"
	}
	p.IstioVersion = model.ParseIstioVersion(p.Metadata.IstioVersion)
	if p.ConfigNamespace == "" {
		p.ConfigNamespace = "default"
	}
	if p.Metadata.Namespace == "" {
		p.Metadata.Namespace = p.ConfigNamespace
	}
	if p.ID == "" {
		p.ID = "app.test"
	}
	if p.DNSDomain == "" {
		p.DNSDomain = p.ConfigNamespace + ".svc.cluster.local"
	}
	if len(p.IPAddresses) == 0 {
		p.IPAddresses = []string{"1.1.1.1"}
	}

	p.SetSidecarScope(pc)
	p.SetServiceTargets(env.ServiceDiscovery)
	p.SetGatewaysForProxy(pc)
	p.DiscoverIPMode()
	return p
}
