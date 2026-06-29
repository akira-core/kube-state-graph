package build

import (
	"math/rand"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// familyTopology models the cluster-family environment for the localised
// connection-string model (D29):
//   - clusters prod-1 and prod-2 (one family: "prod-0") both deploy
//     messaging/nats and data/cache, each with one backing pod of its own;
//   - out-of-family staging-1 ("staging-0") also deploys messaging/nats —
//     it must never be unioned into a prod-* trace's fan-out.
//
// Resolution rule under test: a "://" endpoint resolves to a SINGLE service
// node in the caller's OWN (anchor) cluster, iff that cluster holds the
// service; that node then fans out service-selects-pod edges to the UNION of
// backing pods across every same-family cluster holding the service (so
// service→pod MAY cross clusters, while pod→service never does).
func familyTopology() Topology {
	client := &graph.PodNode{IDValue: "prod-1/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "shop"}}
	natsP1 := &graph.PodNode{IDValue: "prod-1/n1", NameValue: "nats-0", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "messaging"}}
	natsP2 := &graph.PodNode{IDValue: "prod-2/n2", NameValue: "nats-0", LabelsValue: map[string]string{"cluster": "prod-2", "namespace": "messaging"}}
	cacheP1 := &graph.PodNode{IDValue: "prod-1/c1", NameValue: "cache-0", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "data"}}
	cacheP2 := &graph.PodNode{IDValue: "prod-2/c2", NameValue: "cache-0", LabelsValue: map[string]string{"cluster": "prod-2", "namespace": "data"}}
	natsS1 := &graph.PodNode{IDValue: "staging-1/sn", NameValue: "nats-0", LabelsValue: map[string]string{"cluster": "staging-1", "namespace": "messaging"}}
	return Topology{
		Pods:      []*graph.PodNode{client, natsP1, natsP2, cacheP1, cacheP2, natsS1},
		PodsByUID: map[string]*graph.PodNode{"abc": client, "n1": natsP1, "n2": natsP2, "c1": cacheP1, "c2": cacheP2, "sn": natsS1},
		ServicesByNameNS: map[serviceKey]ServiceObs{
			{"prod-1", "messaging", "nats"}:    {ClusterIP: "10.1.0.5"},
			{"prod-2", "messaging", "nats"}:    {ClusterIP: "10.2.0.5"},
			{"staging-1", "messaging", "nats"}: {ClusterIP: "10.9.0.5"},
			{"prod-1", "data", "cache"}:        {ClusterIP: "10.1.0.6"},
			{"prod-2", "data", "cache"}:        {ClusterIP: "10.2.0.6"},
		},
		EndpointsByService: map[serviceKey][]EndpointObs{
			{"prod-1", "messaging", "nats"}:    {{Pod: natsP1}},
			{"prod-2", "messaging", "nats"}:    {{Pod: natsP2}},
			{"staging-1", "messaging", "nats"}: {{Pod: natsS1}},
			{"prod-1", "data", "cache"}:        {{Pod: cacheP1}},
			{"prod-2", "data", "cache"}:        {{Pod: cacheP2}},
		},
		ClustersObserved: []string{"prod-1", "prod-2", "staging-1"},
	}
}

// famSample builds one service-graph sample. An empty cluster omits the
// `cluster` label entirely (exercising the "unknown" bucketing).
func famSample(client, server, cluster, clientUID, serverUID string) model.Sample {
	m := model.Metric{
		"client":             model.LabelValue(client),
		"server":             model.LabelValue(server),
		"client_k8s_pod_uid": model.LabelValue(clientUID),
		"server_k8s_pod_uid": model.LabelValue(serverUID),
	}
	if cluster != "" {
		m["cluster"] = model.LabelValue(cluster)
	}
	return model.Sample{Metric: m, Value: 5}
}

func svcNodeIDs(res ServiceGraphResult) []string {
	ids := make([]string, 0, len(res.ServiceNodes))
	for _, s := range res.ServiceNodes {
		ids = append(ids, s.IDValue)
	}
	return ids
}

func extNodeIDs(res ServiceGraphResult) []string {
	ids := make([]string, 0, len(res.ExternalNodes))
	for _, ext := range res.ExternalNodes {
		ids = append(ids, ext.IDValue)
	}
	return ids
}

func TestParseServiceGraph_LocalNode_CrossClusterFanout(t *testing.T) {
	// prod-1 pod calls nats.messaging.svc. The anchor (prod-1) holds the
	// service, so EXACTLY ONE service node materialises — prod-1/messaging/nats
	// — with a single intra-cluster pod-calls-service edge. Its
	// service-selects-pod fan-out unions the prod-0 family's backing pods:
	// prod-1/n1 (intra) AND prod-2/n2 (cross-cluster). The out-of-family
	// staging-1 backing pod is never unioned.
	vec := sampleVec(famSample("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", ""))
	res := parseServiceGraph(vec, familyTopology())

	require.Len(t, res.ServiceNodes, 1, "exactly one service node in the caller's own cluster")
	assert.Equal(t, "prod-1/messaging/nats", res.ServiceNodes[0].IDValue)

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1, "one intra-cluster pod-calls-service edge")
	assert.Equal(t, "prod-1/abc", pcs[0].Source)
	assert.Equal(t, "prod-1/messaging/nats", pcs[0].Target)
	assert.Equal(t, "prod-1", pcs[0].Labels["cluster"], "client side is a pod → trace cluster present")

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 2, "cross-cluster endpoint union over the prod-0 family")
	targets := make([]string, 0, len(ssp))
	for _, e := range ssp {
		assert.Equal(t, "prod-1/messaging/nats", e.Source, "fan-out sources only from the single local node")
		targets = append(targets, e.Target)
	}
	assert.ElementsMatch(t, []string{"prod-1/n1", "prod-2/n2"}, targets,
		"union includes the family sibling's backing pod (cross-cluster), not staging-1")
	assert.Empty(t, res.ExternalNodes, "resolved endpoint must not also produce an external node")
}

func TestParseServiceGraph_ZeroFamilyMatchesFallsBackToExternal(t *testing.T) {
	// data/queue exists in no cluster at all → external fallback, edge stays
	// pod-calls-pod (target is not a service node).
	vec := sampleVec(famSample("checkout", "amqp://queue.data.svc:5672", "prod-1", "abc", ""))
	res := parseServiceGraph(vec, familyTopology())

	assert.Empty(t, res.ServiceNodes)
	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/amqp://queue.data.svc:5672", res.ExternalNodes[0].IDValue)
	require.Len(t, res.Edges, 1)
	assert.Equal(t, graph.EdgeTypePodCallsPod, res.Edges[0].Type)
	assert.Equal(t, "external/amqp://queue.data.svc:5672", res.Edges[0].Target)
}

func TestParseServiceGraph_AnchorLacksService_SiblingHasIt_External(t *testing.T) {
	// messaging/nats is removed from prod-1 but kept in prod-2 (same family).
	// The anchor (prod-1) does NOT hold the service — a family sibling holding
	// it is not enough — so the endpoint is external. No prod-2 service node is
	// materialised by this resolution (a local Service is a mesh precondition).
	topo := familyTopology()
	delete(topo.ServicesByNameNS, serviceKey{"prod-1", "messaging", "nats"})
	vec := sampleVec(famSample("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", ""))
	res := parseServiceGraph(vec, topo)

	assert.Empty(t, res.ServiceNodes, "no service node when the anchor cluster lacks the Service")
	assert.Empty(t, edgesByType(res, graph.EdgeTypeServiceSelectsPod))
	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/nats://nats.messaging.svc:4222", res.ExternalNodes[0].IDValue)
	require.Len(t, res.Edges, 1)
	assert.Equal(t, graph.EdgeTypePodCallsPod, res.Edges[0].Type)
	assert.Equal(t, "prod-1", res.Edges[0].Labels["cluster"], "client side is a pod")
}

func TestParseServiceGraph_OutOfFamilyOnlyServiceFallsBackToExternal(t *testing.T) {
	// messaging/nats exists ONLY in staging-1; the trace comes from prod-1.
	// staging-0 is not prod-0's family → the prod-0 candidate set is empty →
	// external.
	topo := familyTopology()
	delete(topo.ServicesByNameNS, serviceKey{"prod-1", "messaging", "nats"})
	delete(topo.ServicesByNameNS, serviceKey{"prod-2", "messaging", "nats"})
	vec := sampleVec(famSample("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", ""))
	res := parseServiceGraph(vec, topo)

	assert.Empty(t, res.ServiceNodes, "out-of-family staging-1 service must not be matched")
	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/nats://nats.messaging.svc:4222", res.ExternalNodes[0].IDValue)
}

func TestParseServiceGraph_BothSidesConnString_SingleIntraEdge(t *testing.T) {
	// Both sides are "://" and neither is a pod, so both anchor on the trace
	// cluster prod-1: the client resolves to prod-1/messaging/nats and the
	// server to prod-1/data/cache — ONE intra-cluster pod-calls-service edge in
	// prod-1 (no cross product). Each service node still fans out its own
	// cross-cluster service-selects-pod union.
	vec := sampleVec(famSample("nats://nats.messaging.svc:4222", "redis://cache.data.svc:6379", "prod-1", "", ""))
	res := parseServiceGraph(vec, familyTopology())

	assert.ElementsMatch(t, []string{"prod-1/messaging/nats", "prod-1/data/cache"}, svcNodeIDs(res),
		"both sides resolve to a single node in the anchor cluster")

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1, "single intra-cluster edge, no cross product")
	assert.Equal(t, "prod-1/messaging/nats", pcs[0].Source)
	assert.Equal(t, "prod-1/data/cache", pcs[0].Target)
	assert.NotContains(t, pcs[0].Labels, "cluster", "client side is non-pod → cluster key omitted")

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 4, "each local service node fans out across the prod-0 family")
	fanout := map[string][]string{}
	for _, e := range ssp {
		fanout[e.Source] = append(fanout[e.Source], e.Target)
	}
	assert.ElementsMatch(t, []string{"prod-1/n1", "prod-2/n2"}, fanout["prod-1/messaging/nats"])
	assert.ElementsMatch(t, []string{"prod-1/c1", "prod-2/c2"}, fanout["prod-1/data/cache"])
}

func TestParseServiceGraph_MissingClusterLabelRecoversAnchorFromClientPod(t *testing.T) {
	// The series is missing its cluster label (bucketed to "unknown"), but the
	// client UID resolves to the prod-1 pod via the global UID index — the
	// server-side "://" anchor is recovered as prod-1, so resolution localises
	// there and fans out across the prod-0 family. The edge's labels.cluster
	// stays the raw trace label ("unknown", per D9).
	vec := sampleVec(famSample("checkout", "nats://nats.messaging.svc:4222", "", "abc", ""))
	res := parseServiceGraph(vec, familyTopology())

	require.Len(t, res.ServiceNodes, 1, "anchor recovered from the UID-resolved client pod")
	assert.Equal(t, "prod-1/messaging/nats", res.ServiceNodes[0].IDValue)
	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "prod-1/abc", pcs[0].Source)
	assert.Equal(t, "unknown", pcs[0].Labels["cluster"], "edge cluster label stays the raw trace label (D9)")
	assert.ElementsMatch(t, []string{"prod-1/n1", "prod-2/n2"},
		[]string{edgesByType(res, graph.EdgeTypeServiceSelectsPod)[0].Target, edgesByType(res, graph.EdgeTypeServiceSelectsPod)[1].Target})
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_WrongClusterLabelRecoversAnchorFromClientPod(t *testing.T) {
	// The trace label disagrees with topology ("legacy-7" is no family member),
	// but the client UID resolves to the prod-1 pod — the anchor follows the
	// pod's authoritative cluster, not the label.
	vec := sampleVec(famSample("checkout", "nats://nats.messaging.svc:4222", "legacy-7", "abc", ""))
	res := parseServiceGraph(vec, familyTopology())

	require.Len(t, res.ServiceNodes, 1, "anchor recovered from the UID-resolved client pod, not the wrong label")
	assert.Equal(t, "prod-1/messaging/nats", res.ServiceNodes[0].IDValue)
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_UnknownAnchorNonPodClientFallsBackToExternal(t *testing.T) {
	// Missing cluster label AND the client side is not a pod (non-URL human
	// label, no UID): the anchor is the "unknown" bucket, whose family-of-one
	// holds no nats (no "unknown"-bucketed entry), and there is NO cross-family
	// fallback — so the "://" server stays external.
	vec := sampleVec(famSample("admin", "nats://nats.messaging.svc:4222", "", "", ""))
	res := parseServiceGraph(vec, familyTopology())

	assert.Empty(t, res.ServiceNodes, "an unanchorable non-pod caller cannot resolve a local service")
	assert.ElementsMatch(t, []string{"external/admin", "external/nats://nats.messaging.svc:4222"}, extNodeIDs(res))
}

func TestParseServiceGraph_AnchorHoldsServiceWholeFamilyEndpointless(t *testing.T) {
	// The anchor (prod-1) holds the nats Service object but NEITHER prod cluster
	// has backing pods (no allowlisted endpointslice join anywhere). The single
	// local node still materialises with zero fan-out — an operator signal.
	topo := familyTopology()
	delete(topo.EndpointsByService, serviceKey{"prod-1", "messaging", "nats"})
	delete(topo.EndpointsByService, serviceKey{"prod-2", "messaging", "nats"})
	vec := sampleVec(famSample("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", ""))
	res := parseServiceGraph(vec, topo)

	require.Len(t, res.ServiceNodes, 1, "anchor's local node materialises on Service-object presence")
	assert.Equal(t, "prod-1/messaging/nats", res.ServiceNodes[0].IDValue)
	require.Len(t, edgesByType(res, graph.EdgeTypePodCallsService), 1)
	assert.Empty(t, edgesByType(res, graph.EdgeTypeServiceSelectsPod), "no backing pods → no fan-out edges")
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_EndpointlessAnchorStillFansOutToSibling(t *testing.T) {
	// The anchor (prod-1) holds the nats Service object but has no local backing
	// pods; prod-2 (same family) does. The local prod-1 service node still
	// materialises and its cross-cluster fan-out reaches prod-2's backing pod.
	topo := familyTopology()
	delete(topo.EndpointsByService, serviceKey{"prod-1", "messaging", "nats"})
	vec := sampleVec(famSample("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", ""))
	res := parseServiceGraph(vec, topo)

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "prod-1/messaging/nats", res.ServiceNodes[0].IDValue)
	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 1, "fan-out reaches only the sibling's backing pod")
	assert.Equal(t, "prod-1/messaging/nats", ssp[0].Source)
	assert.Equal(t, "prod-2/n2", ssp[0].Target, "cross-cluster service-selects-pod into prod-2")
}

func TestParseServiceGraph_FullyUnlabelledDeploymentResolvesWithinUnknown(t *testing.T) {
	// A deployment whose KSM series carry no cluster label at all: everything
	// (pods, services, endpoints) is bucketed to "unknown". clusterFamilyKey
	// ("unknown") == "unknown" is a family-of-one, so an "unknown"-anchored
	// caller IS a holder of its "unknown"-bucketed Service and resolution stays
	// inside the pseudo-cluster.
	cachePod := &graph.PodNode{IDValue: "unknown/u1", NameValue: "cache-0", LabelsValue: map[string]string{"cluster": "unknown", "namespace": "data"}}
	topo := Topology{
		Pods:      []*graph.PodNode{cachePod},
		PodsByUID: map[string]*graph.PodNode{"u1": cachePod},
		ServicesByNameNS: map[serviceKey]ServiceObs{
			{"unknown", "data", "cache"}: {ClusterIP: "10.0.0.2"},
		},
		EndpointsByService: map[serviceKey][]EndpointObs{
			{"unknown", "data", "cache"}: {{Pod: cachePod}},
		},
	}
	vec := sampleVec(famSample("admin", "redis://cache.data.svc:6379", "", "", ""))
	res := parseServiceGraph(vec, topo)

	require.Len(t, res.ServiceNodes, 1, "fully-unlabelled deployment keeps conn-string resolution")
	assert.Equal(t, "unknown/data/cache", res.ServiceNodes[0].IDValue)
	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 1)
	assert.Equal(t, "unknown/u1", ssp[0].Target)
}

func TestParseServiceGraph_BogusAnchorUnknownOnlyHolderStaysExternal(t *testing.T) {
	// data/queue is known ONLY from unlabelled ("unknown"-bucketed) series. A
	// bogus-label anchor ("legacy-7", family "legacy-0") cannot hit the
	// "unknown"-keyed entries (its own family-of-one holds nothing) → external.
	topo := familyTopology()
	topo.ServicesByNameNS[serviceKey{"unknown", "data", "queue"}] = ServiceObs{ClusterIP: "10.0.0.3"}
	vec := sampleVec(famSample("admin", "amqp://queue.data.svc:5672", "legacy-7", "", ""))
	res := parseServiceGraph(vec, topo)

	assert.Empty(t, res.ServiceNodes, "an identity-less holder must not satisfy a bogus-label anchor")
	assert.ElementsMatch(t, []string{"external/admin", "external/amqp://queue.data.svc:5672"}, extNodeIDs(res))
}

func TestParseServiceGraph_AnchorFamilyLacksServiceFallsBackToExternal(t *testing.T) {
	// The anchor (staging-1) holds nats but NOT cache. The "://" addresses
	// data/cache; staging-0's family has no cache candidate and there is no
	// cross-family fallback — so the endpoint stays external even though the
	// prod-0 family holds the service.
	vec := sampleVec(famSample("admin", "redis://cache.data.svc:6379", "staging-1", "", ""))
	res := parseServiceGraph(vec, familyTopology())

	assert.Empty(t, res.ServiceNodes, "loaded family lacking the service must not fall back cross-family")
	assert.ElementsMatch(t, []string{"external/admin", "external/redis://cache.data.svc:6379"}, extNodeIDs(res))
}

func TestParseServiceGraph_ClientSideConnString_LocalSourceNode(t *testing.T) {
	// A CLIENT-side "://" (resolveClient's empty-UID path anchors on the raw
	// trace label "prod-1"). data/cache resolves to a single local source node
	// prod-1/data/cache; the server is a pod (via UID). Edge type is
	// pod-calls-pod (target is a pod), labels.cluster omitted (non-pod client).
	vec := sampleVec(famSample("redis://cache.data.svc:6379", "checkout", "prod-1", "", "abc"))
	res := parseServiceGraph(vec, familyTopology())

	require.Len(t, res.ServiceNodes, 1, "client-side conn string resolves to a single local source node")
	assert.Equal(t, "prod-1/data/cache", res.ServiceNodes[0].IDValue)
	pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pcp, 1)
	assert.Equal(t, "prod-1/data/cache", pcp[0].Source)
	assert.Equal(t, "prod-1/abc", pcp[0].Target, "server side resolves via the UID index")
	assert.NotContains(t, pcp[0].Labels, "cluster", "client resolved to a service node → cluster key omitted")
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_SameLabelDifferentAnchorsResolveIndependently(t *testing.T) {
	// One vector resolves the SAME "://" label under two different anchors: a
	// prod-1 client localises to prod-1/messaging/nats, a staging-1 client to
	// staging-1/messaging/nats. Each anchor materialises only its OWN local
	// node (prod-2's node is never materialised — prod-2 is reached only as a
	// fan-out endpoint), and resolution is independent of arrival order.
	vec := sampleVec(
		famSample("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", ""),
		famSample("nats-0", "nats://nats.messaging.svc:4222", "staging-1", "sn", ""),
	)
	res := parseServiceGraph(vec, familyTopology())

	assert.ElementsMatch(t, []string{"prod-1/messaging/nats", "staging-1/messaging/nats"}, svcNodeIDs(res),
		"each anchor materialises only its own local service node")

	targetsBySrc := map[string][]string{}
	for _, e := range edgesByType(res, graph.EdgeTypePodCallsService) {
		targetsBySrc[e.Source] = append(targetsBySrc[e.Source], e.Target)
	}
	assert.ElementsMatch(t, []string{"prod-1/messaging/nats"}, targetsBySrc["prod-1/abc"],
		"prod client resolves to its own local node")
	assert.ElementsMatch(t, []string{"staging-1/messaging/nats"}, targetsBySrc["staging-1/sn"],
		"staging client resolves to its own local node, independently")
}

func TestParseServiceGraph_SelfLoopUID_ConnStringSide_LocalNode(t *testing.T) {
	// D33 guard: both UIDs equal and the server label is a "://" string — the
	// server UID is cleared and that side resolves as a connection string,
	// localised to the anchor (prod-1, the client pod). One intra-cluster
	// pod-calls-service edge, no self-loop pod edge.
	vec := sampleVec(famSample("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", "abc"))
	res := parseServiceGraph(vec, familyTopology())

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1, "cleared '://' side resolves to the local service node")
	assert.Equal(t, "prod-1/abc", pcs[0].Source, "non-'://' side keeps the shared UID and resolves to its pod")
	assert.Equal(t, "prod-1/messaging/nats", pcs[0].Target)
	assert.Empty(t, edgesByType(res, graph.EdgeTypePodCallsPod), "no self-loop pod edge")
}

func TestParseServiceGraph_EmptySideDropsSeriesWithoutMaterialisation(t *testing.T) {
	// A series with a wholly empty side (no UID, no label) is dropped BEFORE
	// resolution: the other side's "://" label must not leak service nodes or
	// fan-out edges as an orphan subgraph.
	t.Run("empty client side", func(t *testing.T) {
		vec := sampleVec(famSample("", "nats://nats.messaging.svc:4222", "prod-1", "", ""))
		res := parseServiceGraph(vec, familyTopology())
		assert.Empty(t, res.Edges)
		assert.Empty(t, res.ServiceNodes, "server-side fan-out must not materialise for a dropped series")
		assert.Empty(t, res.ExternalNodes)
	})
	t.Run("empty server side", func(t *testing.T) {
		vec := sampleVec(famSample("nats://nats.messaging.svc:4222", "", "prod-1", "", ""))
		res := parseServiceGraph(vec, familyTopology())
		assert.Empty(t, res.Edges)
		assert.Empty(t, res.ServiceNodes, "client-side fan-out must not materialise for a dropped series")
		assert.Empty(t, res.ExternalNodes)
	})
}

func TestParseServiceGraph_LocalisedResolution_Deterministic(t *testing.T) {
	// Same fixture in two shuffled arrival orders → identical node and edge
	// SETS (IDs, UUIDv5 edge identities, multiplicity). Output slice order is
	// legitimately unspecified — the serialiser's graph.SortNodes/SortEdges
	// owns ordering — so the comparison is content-based (ElementsMatch), not
	// positional (D6 determinism of content, not of emission order). The
	// fixture exercises the local-node path, the cross-cluster fan-out, the
	// both-"://" intra edge, and an unanchorable-external series.
	mkVec := func(seed int64) model.Vector {
		samples := []model.Sample{
			famSample("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", ""),
			famSample("nats://nats.messaging.svc:4222", "redis://cache.data.svc:6379", "prod-2", "", ""),
			famSample("checkout", "amqp://queue.data.svc:5672", "prod-1", "abc", ""),
			// Unanchorable series (missing label, non-pod client) → external.
			famSample("admin", "redis://cache.data.svc:6379", "", "", ""),
		}
		rng := rand.New(rand.NewSource(seed))
		rng.Shuffle(len(samples), func(i, j int) { samples[i], samples[j] = samples[j], samples[i] })
		return sampleVec(samples...)
	}

	summarise := func(res ServiceGraphResult) (nodes []string, edges []string) {
		for _, s := range res.ServiceNodes {
			nodes = append(nodes, s.IDValue)
		}
		for _, ext := range res.ExternalNodes {
			nodes = append(nodes, ext.IDValue)
		}
		for _, e := range res.Edges {
			edges = append(edges, string(e.Type)+"|"+e.Source+"|"+e.Target+"|"+e.ID)
		}
		return nodes, edges
	}

	n1, e1 := summarise(parseServiceGraph(mkVec(1), familyTopology()))
	n2, e2 := summarise(parseServiceGraph(mkVec(99), familyTopology()))
	assert.ElementsMatch(t, n1, n2, "node set must be arrival-order independent")
	assert.ElementsMatch(t, e1, e2, "edge set (incl. UUIDv5 IDs) must be arrival-order independent")
}
