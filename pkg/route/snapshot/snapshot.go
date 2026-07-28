// Package snapshot resolves a store.TrafficSnapshot in memory. Given the
// resource versions loaded once by the store for one instant, it answers the
// same 3-hop IP->Gateway and per-gateway ScopedFor a point-in-time store query
// would — but against the already-loaded rows, with no further DB round-trips.
//
// Route resolution evaluates the ingress configuration at ONE instant — the
// build window's end (simplify-route-resolution-to-point-in-time D1) — so this
// package has no notion of segments, version boundaries, or config-signature
// dedup: every accessor answers as of the snapshot's own `at`.
//
// The set/liveness logic mirrors the ClickHouse reader exactly: has -> contains,
// hasAll -> containsAll, and the materialized valid_to gives
// live(at) = ValidFrom <= at < ValidTo.
package snapshot

import (
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvk"

	"github.com/akira-core/kube-state-graph/pkg/route/store"
	"github.com/akira-core/kube-state-graph/pkg/route/translate"
)

// pjUnmarshal tolerates unknown fields — production spec_json is the API
// server's CR JSON verbatim, so a newer cluster CRD's extra fields must not
// fail the parse. Kept in sync with the store reader's copy.
var pjUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// Snapshot is a loaded TrafficSnapshot pinned to the instant it answers for.
type Snapshot struct {
	w  store.TrafficSnapshot
	at time.Time
}

// New wraps loaded rows as the configuration state at `at`.
func New(w store.TrafficSnapshot, at time.Time) *Snapshot {
	return &Snapshot{w: w, at: at}
}

// ResolveIPToGateways runs the 3-hop selector join for a destination IP as of
// the snapshot's instant: IP -> ingress Service (selector) -> ingress Deployment
// pod labels L -> gateways whose selector ⊆ L AND whose namespace is the
// ingress Service's own. Hop 3 is namespace-scoped
// (scope-gateway-candidates-to-ingress-namespace): Istio's cross-namespace
// gateway attachment is deliberately out of scope, so within a resolution the
// candidate set can never hold two same-named Gateways (K8s per-namespace name
// uniqueness) and the bare-name identity downstream is unambiguous. Empty
// result => traffic miss.
func (s *Snapshot) ResolveIPToGateways(ip string) []store.GatewayCand {
	// Hop 1: IP -> ingress Service (its namespace + selector).
	var svcNS string
	var svcSel []string
	found := false
	for i := range s.w.Services {
		r := &s.w.Services[i]
		if s.live(r.ValidFrom, r.ValidTo) && r.HasIngressIP(ip) {
			svcNS, svcSel, found = r.Namespace, r.Selector, true
			break
		}
	}
	if !found {
		return nil
	}

	// Hop 2: Service selector -> ingress Deployment pod labels L (svc.selector ⊆ L).
	var podLabels []string
	found = false
	for i := range s.w.Deploys {
		r := &s.w.Deploys[i]
		if s.live(r.ValidFrom, r.ValidTo) && r.Namespace == svcNS && containsAll(r.PodLabels, svcSel) {
			podLabels, found = r.PodLabels, true
			break
		}
	}
	if !found {
		return nil
	}

	// Hop 3: L -> candidate gateways (gateway.selector ⊆ L, same namespace as
	// the ingress Service).
	var cands []store.GatewayCand
	for i := range s.w.Gateways {
		r := &s.w.Gateways[i]
		if s.live(r.ValidFrom, r.ValidTo) && r.Namespace == svcNS && containsAll(podLabels, r.SelectorKV) {
			cands = append(cands, store.GatewayCand{Namespace: r.Namespace, Name: r.Name, ServerHosts: r.ServerHosts})
		}
	}
	return cands
}

// ResolveIPToIngressServices returns every service row live at the snapshot's
// instant whose ingress IPs (external or load-balancer) contain ip — the
// single-cluster analogue of store.ClustersWithIngressIP's SQL, returning the
// rows themselves instead of cluster names. Uniqueness of the (Namespace, Name)
// identity set is the caller's concern (pkg/route's ingressServiceIdentity).
//
// Because it is evaluated as of one instant, an identity that was superseded
// earlier is simply not returned — only the owner(s) live at `at` count, so
// more than one of those is a genuine collision rather than version churn.
func (s *Snapshot) ResolveIPToIngressServices(ip string) []store.ServiceRow {
	var out []store.ServiceRow
	for i := range s.w.Services {
		r := &s.w.Services[i]
		if s.live(r.ValidFrom, r.ValidTo) && r.HasIngressIP(ip) {
			out = append(out, *r)
		}
	}
	return out
}

// ScopedFor rebuilds one gateway's translate input as of the snapshot's instant:
// its Gateway CR + the VirtualServices bound to it + the backend Services those
// VS route to. ok=false if the gateway has no version live at that instant.
func (s *Snapshot) ScopedFor(gwName string) (translate.ScopedInput, bool, error) {
	var gw *store.GatewayRow
	for i := range s.w.Gateways {
		r := &s.w.Gateways[i]
		if r.Name == gwName && s.live(r.ValidFrom, r.ValidTo) {
			gw = r
			break
		}
	}
	if gw == nil {
		return translate.ScopedInput{}, false, nil
	}
	var gwSpec networking.Gateway
	if err := pjUnmarshal.Unmarshal([]byte(gw.SpecJSON), &gwSpec); err != nil {
		return translate.ScopedInput{}, false, err
	}
	// Domain is what production's config store stamps on every config it serves,
	// and istiod needs it to finish resolving a short destination.host: without
	// it ResolveShortnameToFQDN appends only the namespace, yielding `<svc>.<ns>`
	// — neither a short name nor an FQDN, and unparseable downstream (design D2).
	cfgs := []config.Config{{
		Meta: config.Meta{
			GroupVersionKind: gvk.Gateway,
			Name:             gwName,
			Namespace:        gw.Namespace,
			Domain:           store.ClusterDomain,
		},
		Spec: &gwSpec,
	}}

	// Configs are keyed by resource identity: istiod's in-memory config store
	// rejects a repeat ("item already exists"), which would fail the whole
	// translation. Resolver.loadSnapshot already dedups its multi-IP union, but
	// store.Store is an interface and this package's contract is to answer
	// exactly what a point-in-time query would — so the invariant is enforced
	// here too (design D1).
	seen := map[string]bool{gvk.Gateway.String() + "|" + gw.Namespace + "|" + gwName: true}

	var destHosts []string
	for i := range s.w.VSes {
		r := &s.w.VSes[i]
		if !s.live(r.ValidFrom, r.ValidTo) || !boundTo(r.Namespace, r.BoundGateways, gw.Namespace, gwName) {
			continue
		}
		key := gvk.VirtualService.String() + "|" + r.Namespace + "|" + r.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		var vsSpec networking.VirtualService
		if err := pjUnmarshal.Unmarshal([]byte(r.SpecJSON), &vsSpec); err != nil {
			return translate.ScopedInput{}, false, err
		}
		destHosts = append(destHosts, store.VSDestHosts(&vsSpec, r.Namespace)...)
		cfgs = append(cfgs, config.Config{
			Meta: config.Meta{
				GroupVersionKind: gvk.VirtualService,
				Name:             r.Name,
				Namespace:        r.Namespace,
				Domain:           store.ClusterDomain,
			},
			Spec: &vsSpec,
		})
	}

	return translate.ScopedInput{
		Configs:  cfgs,
		Services: s.backendServices(destHosts),
		Proxy:    translate.GatewayProxy{Name: gwName, Namespace: gw.Namespace, Labels: gwSpec.GetSelector()},
	}, true, nil
}

// backendServices rebuilds the destination Services matching hosts as of the
// snapshot's instant. Identity is the (namespace, name) parsed from each
// destination.host FQDN — not scoped by the gateway namespace, so it's portable
// to production where backend Service ns == VS ns != gateway ns. Each matched
// row is one Service carrying all its ports, and each identity contributes at
// most one registry entry (same duplicate-row reasoning as ScopedFor).
func (s *Snapshot) backendServices(hosts []string) []*model.Service {
	if len(hosts) == 0 {
		return nil
	}
	want := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if name, ns, ok := store.ParseBackendHost(h); ok {
			want[ns+"/"+name] = true
		}
	}
	emitted := make(map[string]bool, len(want))
	var out []*model.Service
	for i := range s.w.Services {
		r := &s.w.Services[i]
		id := r.Namespace + "/" + r.Name
		if len(r.Ports) == 0 || !want[id] || emitted[id] || !s.live(r.ValidFrom, r.ValidTo) {
			continue
		}
		emitted[id] = true
		pl := make(model.PortList, 0, len(r.Ports))
		for _, p := range r.Ports {
			pl = append(pl, &model.Port{Name: p.Name, Port: int(p.Port), Protocol: protocol.Parse(p.Name)})
		}
		out = append(out, &model.Service{
			Hostname:       host.Name(store.BackendFQDN(r.Name, r.Namespace)),
			DefaultAddress: "0.0.0.0",
			Ports:          pl,
			Attributes:     model.ServiceAttributes{Namespace: r.Namespace},
		})
	}
	return out
}

// live reports whether the version [vf,vt) is live at the snapshot's instant:
// vf <= at < vt (the reader's liveAt, applied in memory).
func (s *Snapshot) live(vf, vt time.Time) bool {
	return !vf.After(s.at) && s.at.Before(vt)
}

// boundTo reports whether a VirtualService in vsNS whose spec.gateways carries
// `bound` binds the gateway (gwNS, gwName). Istio accepts two forms: the
// qualified "<ns>/<name>", and the bare "<name>" — shorthand for a gateway in
// the VS's OWN namespace. The exporter stores spec.gateways[*] verbatim
// (unnormalized), so the reader must match both; matching only the qualified
// form silently drops every bare-bound VS's routes.
func boundTo(vsNS string, bound []string, gwNS, gwName string) bool {
	if contains(bound, gwNS+"/"+gwName) {
		return true
	}
	return vsNS == gwNS && contains(bound, gwName)
}

// contains reports whether set has x (mirrors ClickHouse has()).
func contains(set []string, x string) bool {
	for _, s := range set {
		if s == x {
			return true
		}
	}
	return false
}

// containsAll reports whether sub ⊆ super (mirrors ClickHouse hasAll(super, sub)).
func containsAll(super, sub []string) bool {
	for _, x := range sub {
		if !contains(super, x) {
			return false
		}
	}
	return true
}

// (destination-host collection lives in pkg/route/store as VSDestHosts — one
// implementation shared with the ClickHouse reader, so the backend key list and
// the translate registry cannot disagree on a Service identity.)
