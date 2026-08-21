package build

import (
	"sort"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// Filtered-build rules (push-request-filters-upstream design D5 / D6).
//
// A filtered build narrows the TOPOLOGY at the source while the three
// traces_service_graph_* series are still read in full. Two rules keep the
// result self-consistent:
//
//	D5 — an endpoint whose pod UID names a pod the request did not load
//	     resolves exactly as if the UID were empty (never a synthesised pod).
//	D6 — a series is kept only if at least one endpoint reached LOADED
//	     topology; a rejected series rolls back every side effect.
//
// Both are inert when the build is unfiltered, which the paired
// "unfiltered still …" assertions pin.

// filteredScopeTopology is the "?namespace=shop" shape: only the `shop` pods of
// cluster-alpha are loaded. `cluster-beta`'s pod and the `db` namespace are the
// out-of-scope estate the service-graph series still reference.
func filteredScopeTopology() Topology {
	client := &graph.PodNode{IDValue: "cluster-alpha/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	pay0 := &graph.PodNode{IDValue: "cluster-alpha/pay0", NameValue: "payments-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	return Topology{
		Pods:      []*graph.PodNode{client, pay0},
		PodsByUID: map[string]*graph.PodNode{"abc": client, "pay0": pay0},
		ServicesByNameNS: map[serviceKey]ServiceObs{
			{"cluster-alpha", "shop", "payments"}: {ClusterIP: "10.0.0.5"},
		},
		EndpointsByService: map[serviceKey][]EndpointObs{
			{"cluster-alpha", "shop", "payments"}: {{Pod: pay0}},
		},
	}
}

func sgSample(m model.Metric, v float64) model.Sample {
	return model.Sample{Metric: m, Value: model.SampleValue(v)}
}

// D5: an unloaded server UID falls to the human label as external/<label>,
// with no synthesised pod anywhere in the result.
func TestFiltered_UnloadedServerUIDBecomesExternal(t *testing.T) {
	vec := sampleVec(sgSample(model.Metric{
		"client":             "checkout",
		"server":             "cart",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "abc",
		"server_k8s_pod_uid": "out-of-scope",
	}, 5))

	res := parseServiceGraphScoped(vec, filteredScopeTopology(), nil, true)

	require.Len(t, res.Edges, 1)
	e := res.Edges[0]
	assert.Equal(t, graph.EdgeTypePodCallsPod, e.Type)
	assert.Equal(t, "cluster-alpha/abc", e.Source)
	assert.Equal(t, graph.ExternalID("cart"), e.Target)
	assert.Equal(t, "cluster-alpha", e.Labels["cluster"], "the client side is a loaded pod, so D9 still applies")
	assert.Nil(t, e.Metrics, "an external endpoint is never measured")

	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "cart", res.ExternalNodes[0].Name())
	assert.Empty(t, res.ExternalNodes[0].Labels())
	assert.Empty(t, res.SynthPods, "a filtered build never synthesises a pod")
}

// D5 counterpart: the SAME series in an unfiltered build still synthesises,
// so the change cannot regress the pre-existing behaviour.
func TestUnfiltered_UnloadedServerUIDStillSynthesises(t *testing.T) {
	vec := sampleVec(sgSample(model.Metric{
		"client":             "checkout",
		"server":             "cart",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "abc",
		"server_k8s_pod_uid": "out-of-scope",
	}, 5))

	res := parseServiceGraph(vec, filteredScopeTopology())

	require.Len(t, res.SynthPods, 1)
	assert.Equal(t, "out-of-scope", res.SynthPods[0].Name())
	assert.Empty(t, res.ExternalNodes)
}

// D5: an unloaded CLIENT UID externalises too — an out-of-scope caller of an
// in-scope pod is an inbound dependency, and the edge then carries no
// labels.cluster (the client side is not a pod).
func TestFiltered_UnloadedClientUIDBecomesInboundExternal(t *testing.T) {
	vec := sampleVec(sgSample(model.Metric{
		"client":             "frontend",
		"server":             "checkout",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "out-of-scope",
		"server_k8s_pod_uid": "abc",
	}, 3))

	res := parseServiceGraphScoped(vec, filteredScopeTopology(), nil, true)

	require.Len(t, res.Edges, 1)
	e := res.Edges[0]
	assert.Equal(t, graph.ExternalID("frontend"), e.Source)
	assert.Equal(t, "cluster-alpha/abc", e.Target)
	assert.NotContains(t, e.Labels, "cluster", "a non-pod client side carries no cluster label")
	assert.Empty(t, res.SynthPods)
}

// D5: a "://" label on an unloaded-UID side still reaches the connection-string
// ladder, so an in-scope Service is resolved rather than externalised — the
// reason the rule routes through resolveEmptyUID instead of emitting an
// external directly.
func TestFiltered_UnloadedUIDWithConnStringResolvesLoadedService(t *testing.T) {
	vec := sampleVec(sgSample(model.Metric{
		"client":             "frontend",
		"server":             "http://payments.shop.svc.cluster.local:8080",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "abc",
		"server_k8s_pod_uid": "out-of-scope",
	}, 4))

	res := parseServiceGraphScoped(vec, filteredScopeTopology(), nil, true)

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, graph.ServiceID("cluster-alpha", "shop", "payments"), res.ServiceNodes[0].ID())
	assert.Empty(t, res.ExternalNodes)
	assert.Empty(t, res.SynthPods)

	calls := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, calls, 1)
	assert.Equal(t, "cluster-alpha/abc", calls[0].Source)
	assert.Len(t, edgesByType(res, graph.EdgeTypeServiceSelectsPod), 1, "the loaded fan-out is emitted")
}

// D6 (a): a series touching NO loaded topology is dropped and leaves the
// resolver byte-identical — no external node, no service node, no fan-out
// edge, no evidence counter.
func TestFiltered_UnanchoredSeriesDroppedWithoutResidue(t *testing.T) {
	vec := sampleVec(sgSample(model.Metric{
		"client":             "frontend",
		"server":             "cart",
		"cluster":            "cluster-beta",
		"client_k8s_pod_uid": "out-1",
		"server_k8s_pod_uid": "out-2",
	}, 7))

	res := parseServiceGraphScoped(vec, filteredScopeTopology(), nil, true)

	assert.Empty(t, res.Edges)
	assert.Empty(t, res.ExternalNodes, "no external↔external web from the out-of-scope estate")
	assert.Empty(t, res.ServiceNodes)
	assert.Empty(t, res.SynthPods)
}

// resolverSnapshot captures every piece of resolver state a series can mutate,
// in a comparable form, so a rejected series can be proven to leave NOTHING
// behind — the property the whole journal exists for.
type resolverSnapshot struct {
	externals  []string
	services   []string
	roles      map[string]string
	svcEdges   []string
	chainEdges []string
	extReasons map[string]int
}

func snapshotResolver(r *sgResolver) resolverSnapshot {
	out := resolverSnapshot{roles: map[string]string{}, extReasons: map[string]int{}}
	for id, sv := range r.services {
		out.services = append(out.services, id)
		out.roles[id] = sv.LabelsValue["role"]
	}
	for id := range r.externals {
		out.externals = append(out.externals, id)
	}
	for k := range r.svcEdges {
		out.svcEdges = append(out.svcEdges, k)
	}
	for k := range r.routeChainEdges {
		out.chainEdges = append(out.chainEdges, k)
	}
	for k, v := range r.extReasons {
		out.extReasons[k] = v
	}
	sort.Strings(out.externals)
	sort.Strings(out.services)
	sort.Strings(out.svcEdges)
	sort.Strings(out.chainEdges)
	return out
}

// D6 (a) at the resolver level: a rejected series must leave the resolver's
// state byte-identical, so nothing it touched can be found by a later series.
func TestFiltered_RollbackLeavesResolverStateUnchanged(t *testing.T) {
	res := newSGResolver(filteredScopeTopology(), true)

	// An out-of-scope caller of a LOADED service IS anchored: the service node
	// and its fan-out are materialised and committed.
	anchored := sampleVec(sgSample(model.Metric{
		"client":             "frontend",
		"server":             "http://payments.shop.svc.cluster.local",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "out-of-scope",
	}, 2))
	kept := parseWithResolver(anchored, res, nil, redInputs{})
	require.Len(t, kept.ServiceNodes, 1, "an out-of-scope caller of a LOADED service is admitted")

	before := snapshotResolver(res)
	require.NotEmpty(t, before.services, "the committed state is non-trivial")

	// Same shape, but the addressed service is NOT loaded: both sides fall to
	// external nodes, admission rejects, rollback undoes everything.
	rejected := sampleVec(sgSample(model.Metric{
		"client":             "someone-else",
		"server":             "http://ledger.finance.svc.cluster.local",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "also-out-of-scope",
	}, 2))
	parseWithResolver(rejected, res, nil, redInputs{})

	assert.Equal(t, before, snapshotResolver(res), "a rejected series must leave no residue")
	assert.Empty(t, res.journal, "the journal is drained after every series")
}

// D6 (d): the emitted set is a pure function of the series SET — swapping the
// vector order must not change the outcome.
func TestFiltered_OutcomeIsOrderFree(t *testing.T) {
	anchored := sgSample(model.Metric{
		"client":             "checkout",
		"server":             "cart",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "abc",
		"server_k8s_pod_uid": "out-of-scope",
	}, 5)
	unanchored := sgSample(model.Metric{
		"client":             "frontend",
		"server":             "cart",
		"cluster":            "cluster-beta",
		"client_k8s_pod_uid": "out-1",
		"server_k8s_pod_uid": "out-2",
	}, 7)

	summary := func(res ServiceGraphResult) []string {
		out := make([]string, 0, len(res.Edges)+len(res.ExternalNodes))
		for _, e := range res.Edges {
			out = append(out, string(e.Type)+" "+e.Source+"→"+e.Target)
		}
		for _, n := range res.ExternalNodes {
			out = append(out, "ext "+n.ID())
		}
		return out
	}

	a := summary(parseServiceGraphScoped(sampleVec(anchored, unanchored), filteredScopeTopology(), nil, true))
	b := summary(parseServiceGraphScoped(sampleVec(unanchored, anchored), filteredScopeTopology(), nil, true))
	assert.ElementsMatch(t, a, b)
	assert.Len(t, a, 2, "one edge to external/cart plus the external node itself")
}

// D6 (e): a rejected series contributes nothing to the RED join, so its
// companion samples cannot leak onto an admitted edge.
func TestFiltered_RejectedSeriesContributesNoRED(t *testing.T) {
	m := model.Metric{
		"client":             "frontend",
		"server":             "cart",
		"cluster":            "cluster-beta",
		"client_k8s_pod_uid": "out-1",
		"server_k8s_pod_uid": "out-2",
	}
	res := parseWithResolver(sampleVec(sgSample(m, 7)), newSGResolver(filteredScopeTopology(), true), nil, redInputs{
		Failed: sampleVec(sgSample(m, 1)),
	})
	assert.Empty(t, res.Edges)
}

// A cluster filter renders the out-of-cluster partner as an external, and the
// family fan-out reaches loaded clusters only.
func TestFiltered_CrossClusterPartnerBecomesExternal(t *testing.T) {
	// sampleTopology holds pods in BOTH clusters; the filtered analogue loads
	// only cluster-alpha's.
	alpha := &graph.PodNode{IDValue: "cluster-alpha/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	topo := Topology{Pods: []*graph.PodNode{alpha}, PodsByUID: map[string]*graph.PodNode{"abc": alpha}}

	vec := sampleVec(sgSample(model.Metric{
		"client":             "checkout",
		"server":             "cart",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "abc",
		"server_k8s_pod_uid": "def", // a cluster-beta pod, not loaded
	}, 5))

	res := parseServiceGraphScoped(vec, topo, nil, true)
	require.Len(t, res.Edges, 1)
	assert.Equal(t, graph.ExternalID("cart"), res.Edges[0].Target)
	assert.Equal(t, "cluster-alpha", res.Edges[0].Labels["cluster"])
	assert.Empty(t, res.SynthPods)
}

// The prescan already skips endpoints whose client pod is not loaded, so a
// filtered build never asks the route engine about out-of-scope traffic.
func TestFiltered_PrescanSkipsOutOfScopeClients(t *testing.T) {
	vec := sampleVec(sgSample(model.Metric{
		"client":                "frontend",
		"server":                "unknown",
		"cluster":               "cluster-alpha",
		"client_k8s_pod_uid":    "out-of-scope",
		"client_server_address": "api.example.com",
		"client_dns_answers":    "203.0.113.10",
	}, 2))

	assert.Empty(t, collectRouteQueriesWith(vec, newSGResolver(filteredScopeTopology(), true)),
		"an unloaded client pod yields no route key")
}

// An out-of-scope caller whose server is the "unknown" sentinel is dropped
// outright: the peer ladder requires a real client pod, so nothing resolves —
// byte-identical to the pre-change blanket exclusion.
func TestFiltered_UnknownServerFromOutOfScopeClientIsDropped(t *testing.T) {
	vec := sampleVec(sgSample(model.Metric{
		"client":                "frontend",
		"server":                "unknown",
		"cluster":               "cluster-alpha",
		"client_k8s_pod_uid":    "out-of-scope",
		"client_server_address": "payments.shop.svc.cluster.local",
	}, 2))

	res := parseServiceGraphScoped(vec, filteredScopeTopology(), nil, true)
	assert.Empty(t, res.Edges)
	assert.Empty(t, res.ExternalNodes)
	assert.Empty(t, res.ServiceNodes)
}

// A side with an unloaded UID AND an empty label is wholly unresolvable; the
// series is dropped rather than emitting a half edge.
func TestFiltered_UnloadedUIDWithEmptyLabelDropsSeries(t *testing.T) {
	vec := sampleVec(sgSample(model.Metric{
		"client":             "checkout",
		"server":             "",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "abc",
		"server_k8s_pod_uid": "out-of-scope",
	}, 5))

	res := parseServiceGraphScoped(vec, filteredScopeTopology(), nil, true)
	assert.Empty(t, res.Edges)
	assert.Empty(t, res.ExternalNodes)
	assert.Empty(t, res.SynthPods)
}
