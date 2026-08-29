package build

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"

	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// ---------------------------------------------------------------------------
// Span-link relation marking (add-span-link-logical-edges).
// Fixture recap (sampleTopologyWithServices): client pod abc (cluster-alpha,
// ns shop), backing pods pay0/pay1 (svc shop/payments), mongo0 = uid m0 (ns
// db, svc db/mongo headless), svc db/redis with no endpoints.
// ---------------------------------------------------------------------------

// linkSample builds one edge_relation="link" sample: producer abc → consumer
// m0, both resolvable, with extra labels merged in (an explicit empty value
// clears a default).
func linkSample(extra model.Metric) model.Sample {
	m := model.Metric{
		"client":             "producer",
		"server":             "consumer",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "abc",
		"server_k8s_pod_uid": "m0",
		"edge_relation":      "link",
	}
	for k, v := range extra {
		m[k] = v
	}
	return model.Sample{Metric: m, Value: 5}
}

// findEdgeST returns the unique edge with the given (source, target), failing
// the test when absent or duplicated.
func findEdgeST(t *testing.T, res ServiceGraphResult, src, tgt string) *graph.Edge {
	t.Helper()
	var found *graph.Edge
	for _, e := range res.Edges {
		if e.Source == src && e.Target == tgt {
			require.Nilf(t, found, "duplicate edge %s -> %s", src, tgt)
			found = e
		}
	}
	require.NotNilf(t, found, "edge %s -> %s not found", src, tgt)
	return found
}

func TestParseServiceGraph_LinkRelation_BothPodsResolve(t *testing.T) {
	res := parseServiceGraph(sampleVec(linkSample(nil)), sampleTopologyWithServices())

	require.Len(t, res.Edges, 1)
	e := res.Edges[0]
	assert.Equal(t, graph.EdgeTypePodCallsPod, e.Type)
	assert.Equal(t, "cluster-alpha/abc", e.Source)
	assert.Equal(t, "cluster-alpha/m0", e.Target)
	assert.Equal(t, "link", e.Labels["relation"])
	assert.Equal(t, "cluster-alpha", e.Labels["cluster"], "ordinary D9 cluster label is unaffected")
}

func TestParseServiceGraph_LinkRelation_LinkWinsOverPlainSeries(t *testing.T) {
	plain := model.Sample{Metric: model.Metric{
		"client": "producer", "server": "consumer", "cluster": "cluster-alpha",
		"client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "m0",
	}, Value: 7}
	link := linkSample(nil)

	// Both ingestion orders (D6): the aggregated single edge is link-marked.
	for name, vec := range map[string]model.Vector{
		"plain_first": sampleVec(plain, link),
		"link_first":  sampleVec(link, plain),
	} {
		t.Run(name, func(t *testing.T) {
			res := parseServiceGraph(vec, sampleTopologyWithServices())
			require.Len(t, res.Edges, 1, "plain and link series aggregate into ONE edge")
			assert.Equal(t, "link", res.Edges[0].Labels["relation"])
		})
	}
}

// The client side's own broker peer address (client_server_address) marks the
// EXISTING pod→broker edge — produced by an ordinary network series — as
// transport; the link edge itself stays link, and the broker's fan-out edges
// are never marked.
func TestParseServiceGraph_LinkRelation_TransportMarksExistingBrokerEdge(t *testing.T) {
	network := model.Sample{Metric: model.Metric{
		"client": "producer", "server": "mongodb://mongo.db.svc.cluster.local:27017",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "",
	}, Value: 3}
	link := linkSample(model.Metric{"client_server_address": "mongo.db"})

	res := parseServiceGraph(sampleVec(network, link), sampleTopologyWithServices())

	broker := findEdgeST(t, res, "cluster-alpha/abc", "cluster-alpha/db/mongo")
	assert.Equal(t, graph.EdgeTypePodCallsService, broker.Type)
	assert.Equal(t, "transport", broker.Labels["relation"])

	logical := findEdgeST(t, res, "cluster-alpha/abc", "cluster-alpha/m0")
	assert.Equal(t, "link", logical.Labels["relation"])

	for _, e := range edgesByType(res, graph.EdgeTypeServiceSelectsPod) {
		assert.NotContains(t, e.Labels, "relation", "fan-out edges are never relation-marked")
	}
}

// A pair recorded as BOTH link and transport emits link: one link series
// targets the broker service directly (server is a "://" conn string, so the
// pair is a link pair), while another VALID link series' client-side via
// marking hits the same (pod, broker) pair as transport.
func TestParseServiceGraph_LinkRelation_LinkBeatsTransportOnCollision(t *testing.T) {
	linkToBroker := linkSample(model.Metric{
		"server":             "mongodb://mongo.db.svc.cluster.local:27017",
		"server_k8s_pod_uid": "",
	})
	// Valid link series abc→m0 whose client dials the same broker: its via
	// marking records (abc, mongo-svc) as transport.
	viaBroker := linkSample(model.Metric{
		"client_server_address": "mongo.db",
	})

	for name, vec := range map[string]model.Vector{
		"link_first": sampleVec(linkToBroker, viaBroker),
		"via_first":  sampleVec(viaBroker, linkToBroker),
	} {
		t.Run(name, func(t *testing.T) {
			res := parseServiceGraph(vec, sampleTopologyWithServices())
			e := findEdgeST(t, res, "cluster-alpha/abc", "cluster-alpha/db/mongo")
			assert.Equal(t, "link", e.Labels["relation"], "link wins over transport for the same pair")
		})
	}
}

// A link series whose server side is the "unknown" sentinel with no resolvable
// pod recovered NO consumer: the series contributes no markers at all. The
// producer→broker edge it resolves to (via the unknown-server enrichment over
// the CLIENT peer labels) stays an ordinary unmarked network edge — even when
// a plain series produces the same edge — because a transport marker without
// its accompanying link edge would tell the frontend to dash a network
// dependency that backs no rendered logical edge.
func TestParseServiceGraph_LinkRelation_ServerUnknown_ContributesNoMarkers(t *testing.T) {
	link := linkSample(model.Metric{
		"server":                "unknown",
		"server_k8s_pod_uid":    "",
		"client_server_address": "mongo.db",
	})
	network := model.Sample{Metric: model.Metric{
		"client": "producer", "server": "mongodb://mongo.db.svc.cluster.local:27017",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "",
	}, Value: 3}

	// With and without the coinciding plain network series: the merged (or
	// lone) producer→broker edge never carries a relation key.
	for name, vec := range map[string]model.Vector{
		"merged_with_network_series": sampleVec(link, network),
		"link_series_alone":          sampleVec(link),
	} {
		t.Run(name, func(t *testing.T) {
			res := parseServiceGraph(vec, sampleTopologyWithServices())
			e := findEdgeST(t, res, "cluster-alpha/abc", "cluster-alpha/db/mongo")
			assert.NotContains(t, e.Labels, "relation",
				"a no-consumer link series must leave the network edge unmarked")
			for _, edge := range res.Edges {
				assert.NotContains(t, edge.Labels, "relation",
					"a no-consumer link series must contribute no markers anywhere")
			}
		})
	}
}

// Degraded server endpoints keep the logical claim: a missing-UID non-"unknown"
// label (D27 ghost external) and an unknown-to-topology UID (synth pod) both
// stay link-marked.
func TestParseServiceGraph_LinkRelation_ServerGhostExternal_KeepsLink(t *testing.T) {
	res := parseServiceGraph(sampleVec(linkSample(model.Metric{
		"server":             "ext-consumer",
		"server_k8s_pod_uid": "",
	})), sampleTopologyWithServices())

	e := findEdgeST(t, res, "cluster-alpha/abc", "external/ext-consumer")
	assert.Equal(t, graph.EdgeTypePodCallsPod, e.Type)
	assert.Equal(t, "link", e.Labels["relation"])
}

func TestParseServiceGraph_LinkRelation_ServerSynthPod_KeepsLink(t *testing.T) {
	res := parseServiceGraph(sampleVec(linkSample(model.Metric{
		"server_k8s_pod_uid": "zzz",
	})), sampleTopologyWithServices())

	e := findEdgeST(t, res, "cluster-alpha/abc", graph.PodID("", "zzz"))
	assert.Equal(t, "link", e.Labels["relation"])
	require.Len(t, res.SynthPods, 1)
}

// Per-side independence: an unresolved client contributes no client-side via
// marker, while the resolved server side still marks its own broker hop from
// the mirrored server_* labels.
func TestParseServiceGraph_LinkRelation_ClientUnresolved_NoClientVia_ServerViaStillMarks(t *testing.T) {
	link := linkSample(model.Metric{
		"client":                "ext-producer",
		"client_k8s_pod_uid":    "",
		"client_server_address": "payments.shop", // must be ignored: client did not resolve to a pod
		"server_server_address": "mongo.db",
	})
	// The consumer's own network hop to the broker, from an ordinary series.
	network := model.Sample{Metric: model.Metric{
		"client": "consumer", "server": "mongodb://mongo.db.svc.cluster.local:27017",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": "m0", "server_k8s_pod_uid": "",
	}, Value: 3}

	res := parseServiceGraph(sampleVec(link, network), sampleTopologyWithServices())

	logical := findEdgeST(t, res, "external/ext-producer", "cluster-alpha/m0")
	assert.Equal(t, "link", logical.Labels["relation"], "a degraded client keeps the logical claim")
	assert.NotContains(t, logical.Labels, "cluster", "client side is non-pod")

	broker := findEdgeST(t, res, "cluster-alpha/m0", "cluster-alpha/db/mongo")
	assert.Equal(t, "transport", broker.Labels["relation"], "server-side via marks from the mirrored server_* labels")

	// No pod→payments edge exists, and the client-side peer label must not
	// have marked anything (its pod never resolved).
	for _, e := range res.Edges {
		assert.NotEqual(t, "cluster-alpha/shop/payments", e.Target,
			"the unresolved client side must not produce or mark a payments edge")
	}
}

// Via derivation is lookup-only: a build whose ONLY series are link series
// materialises nothing — no service node, no external node, no fan-out —
// regardless of what the peer labels would classify to. The transport pairs
// are pure markers.
func TestParseServiceGraph_LinkRelation_ViaLookupNeverMaterialises(t *testing.T) {
	res := parseServiceGraph(sampleVec(linkSample(model.Metric{
		"client_server_address": "redis.db",     // classifies to a held Service
		"server_server_address": "203.0.113.50", // classifies to nothing → external ID marker
	})), sampleTopologyWithServices())

	require.Len(t, res.Edges, 1, "only the logical link edge is emitted")
	assert.Equal(t, "link", res.Edges[0].Labels["relation"])
	assert.Empty(t, res.ServiceNodes, "via lookup must not materialise service nodes")
	assert.Empty(t, res.ExternalNodes, "via lookup must not materialise external nodes")
	assert.Empty(t, edgesByType(res, graph.EdgeTypeServiceSelectsPod), "via lookup must not fan out")
}

// A self-loop link series (same resolvable UID on both sides, no "://" label —
// the D33 guard does not fire) emits one link-marked self-loop edge.
func TestParseServiceGraph_LinkRelation_SelfLoopLinkSeries(t *testing.T) {
	res := parseServiceGraph(sampleVec(linkSample(model.Metric{
		"server_k8s_pod_uid": "abc",
	})), sampleTopologyWithServices())

	require.Len(t, res.Edges, 1)
	e := res.Edges[0]
	assert.Equal(t, "cluster-alpha/abc", e.Source)
	assert.Equal(t, "cluster-alpha/abc", e.Target)
	assert.Equal(t, "link", e.Labels["relation"])
}

// A transport pair whose network edge never materialised is a no-op marker:
// nothing is synthesised for it.
func TestParseServiceGraph_LinkRelation_UnmatchedTransportPair_MarkerOnly(t *testing.T) {
	res := parseServiceGraph(sampleVec(linkSample(model.Metric{
		"client_server_address": "redis.db",
	})), sampleTopologyWithServices())

	require.Len(t, res.Edges, 1, "no pod→redis edge is synthesised for the unmatched marker")
	assert.Equal(t, "cluster-alpha/m0", res.Edges[0].Target)
	assert.Equal(t, "link", res.Edges[0].Labels["relation"])
	assert.Empty(t, res.ServiceNodes)
}

// Any edge_relation value other than the exact "link" is ignored.
func TestParseServiceGraph_LinkRelation_NonLinkRelationValueIgnored(t *testing.T) {
	res := parseServiceGraph(sampleVec(linkSample(model.Metric{
		"edge_relation":         "database",
		"client_server_address": "mongo.db",
	})), sampleTopologyWithServices())

	require.Len(t, res.Edges, 1)
	assert.NotContains(t, res.Edges[0].Labels, "relation")
}

// The lookup-only routeNodeID must derive the SAME node ID the materialising
// route path produces: a plain unknown-server series materialises the routed
// backend service, and a link series carrying the same broker FQDN marks that
// very edge transport.
func TestParseServiceGraphRoutes_LinkViaAlignsWithMaterialisedRoute(t *testing.T) {
	routes := routeIndex{
		{callerCluster: "cluster-alpha", host: "broker.example.com", path: "/", port: 443, ips: testDNSAnswer}: {
			dest:    RouteDestination{Cluster: "cluster-alpha", Namespace: "shop", Service: "payments", Port: 8080},
			outcome: RouteHit,
		},
	}
	network := unknownPeerSample("broker.example.com", nil)
	link := linkSample(model.Metric{
		"client_server_address": "broker.example.com",
		"client_dns_answers":    testDNSAnswer,
	})

	res := parseServiceGraphRoutes(sampleVec(network, link), sampleTopologyWithServices(), routes)

	broker := findEdgeST(t, res, "cluster-alpha/abc", "cluster-alpha/shop/payments")
	assert.Equal(t, "transport", broker.Labels["relation"],
		"routeNodeID must align with the ID the materialising route path produced")
	logical := findEdgeST(t, res, "cluster-alpha/abc", "cluster-alpha/m0")
	assert.Equal(t, "link", logical.Labels["relation"])
	assert.Empty(t, res.ExternalNodes)
}

// Without a matching index entry (engine off / truncated key) the via marker
// degrades to the external ID — aligned with the materialising path's external
// fallback, so a plain series' external edge is marked.
func TestParseServiceGraph_LinkRelation_ViaExternalFallbackAligns(t *testing.T) {
	network := unknownPeerSample("broker.example.com", nil)
	link := linkSample(model.Metric{
		"client_server_address": "broker.example.com",
		"client_dns_answers":    testDNSAnswer,
	})

	res := parseServiceGraph(sampleVec(network, link), sampleTopologyWithServices())

	e := findEdgeST(t, res, "cluster-alpha/abc", "external/broker.example.com")
	assert.Equal(t, "transport", e.Labels["relation"])
}

// ---------------------------------------------------------------------------
// Prescan: link series via-key collection (routeprescan.go).
// ---------------------------------------------------------------------------

// A link series emits one via key per side whose own pod resolves, anchored on
// that side's own cluster.
func TestCollectRouteQueries_LinkSeries_EmitsBothViaKeys(t *testing.T) {
	vec := sampleVec(linkSample(model.Metric{
		"server_k8s_pod_uid":    "bpay0", // cluster-beta pod
		"client_server_address": "cbroker.example.com",
		"client_dns_answers":    testDNSAnswer,
		"server_server_address": "sbroker.example.com",
		"server_dns_answers":    "198.51.100.9",
	}))

	keys := collectRouteQueries(vec, sampleTopologyTwoClusters())

	require.Len(t, keys, 2)
	assert.Equal(t, "cluster-alpha", keys[0].callerCluster, "client via key anchors on the client pod's cluster")
	assert.Equal(t, "cbroker.example.com", keys[0].host)
	assert.Equal(t, testDNSAnswer, keys[0].ips)
	assert.Equal(t, "cluster-beta", keys[1].callerCluster, "server via key anchors on the SERVER pod's own cluster")
	assert.Equal(t, "sbroker.example.com", keys[1].host)
	assert.Equal(t, "198.51.100.9", keys[1].ips)
}

// The viaRouteKey skip chain mirrors the parse: in-cluster-resolvable,
// Pod-IP-resolvable, IP-less, and valueless endpoints are never collected.
func TestCollectRouteQueries_LinkSeries_SkipChain(t *testing.T) {
	cases := []struct {
		name  string
		extra model.Metric
	}{
		{"no_peer_value", model.Metric{}},
		{"classified_and_anchor_holds", model.Metric{
			"client_server_address": "payments.shop",
			"client_dns_answers":    testDNSAnswer,
		}},
		{"pod_ip_resolvable", model.Metric{
			"client_server_address": "10.244.1.9",
			"client_dns_answers":    testDNSAnswer,
		}},
		{"no_dns_answers", model.Metric{
			"client_server_address": "cbroker.example.com",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			keys := collectRouteQueries(sampleVec(linkSample(c.extra)), sampleTopologyPodIP())
			assert.Empty(t, keys)
		})
	}
}

// A link series' client-side via key and an ordinary unknown-server series'
// key for the same broker are structurally identical and dedupe to ONE key.
func TestCollectRouteQueries_LinkSeries_DedupsWithPlainNetworkSeriesKey(t *testing.T) {
	vec := sampleVec(
		unknownPeerSample("broker.example.com", nil),
		linkSample(model.Metric{
			"client_server_address": "broker.example.com",
			"client_dns_answers":    testDNSAnswer,
		}),
	)
	keys := collectRouteQueries(vec, sampleTopologyWithServices())
	require.Len(t, keys, 1)
	assert.Equal(t, "broker.example.com", keys[0].host)
}

// A link series with server="unknown" reaches BOTH the link branch and the
// unknown-server branch; the two derive the same key, so exactly one is
// collected.
func TestCollectRouteQueries_ServerUnknownLinkSeries_SingleKey(t *testing.T) {
	vec := sampleVec(linkSample(model.Metric{
		"server":                "unknown",
		"server_k8s_pod_uid":    "",
		"client_server_address": "broker.example.com",
		"client_dns_answers":    testDNSAnswer,
	}))
	keys := collectRouteQueries(vec, sampleTopologyWithServices())
	require.Len(t, keys, 1)
	assert.Equal(t, "broker.example.com", keys[0].host)
	assert.Equal(t, "cluster-alpha", keys[0].callerCluster)
}

// Many series — several client pods, ordinary unknown-server series and link
// series mixed — carrying ONE broker FQDN within one anchor cluster collapse
// to a single routeKey (the prescan `seen` map).
func TestCollectRouteQueries_SameFQDN_MultipleSeries_SingleRouteKey(t *testing.T) {
	vec := sampleVec(
		unknownPeerSample("broker.example.com", nil), // caller abc
		unknownPeerSample("broker.example.com", model.Metric{"client_k8s_pod_uid": "pay0"}),
		linkSample(model.Metric{ // link client via, caller m0
			"client_k8s_pod_uid":    "m0",
			"client_server_address": "broker.example.com",
			"client_dns_answers":    testDNSAnswer,
		}),
		linkSample(model.Metric{ // link server via, server pay1 (same cluster)
			"server_k8s_pod_uid":    "pay1",
			"server_server_address": "broker.example.com",
			"server_dns_answers":    testDNSAnswer,
		}),
	)

	keys := collectRouteQueries(vec, sampleTopologyWithServices())

	require.Len(t, keys, 1, "one broker FQDN in one anchor cluster = one route key")
	assert.Equal(t, "broker.example.com", keys[0].host)
	assert.Equal(t, "cluster-alpha", keys[0].callerCluster)
}

// End-to-end cache assertion: N same-FQDN series (link + plain mixed) through
// ReadServiceGraph cost exactly ONE route-store read, and every dependent edge
// resolves to the same destination node off the prefetched index.
func TestReadServiceGraph_SameFQDNResolvedOnce(t *testing.T) {
	vec := sampleVec(
		unknownPeerSample("broker.example.com", nil),
		unknownPeerSample("broker.example.com", model.Metric{"client_k8s_pod_uid": "pay0"}),
		linkSample(model.Metric{
			"client_server_address": "broker.example.com",
			"client_dns_answers":    testDNSAnswer,
		}),
		linkSample(model.Metric{
			"server_k8s_pod_uid":    "pay1",
			"server_server_address": "broker.example.com",
			"server_dns_answers":    testDNSAnswer,
		}),
	)
	end := time.Unix(1_700_000_000, 0)

	q := promqlmocks.NewMockQuerier(t)
	expectServiceGraphQueries(q, end, vec)

	resolver := &fakeRouteResolver{fn: func(RouteRequest) (RouteDestination, RouteOutcome, error) {
		return RouteDestination{Cluster: "cluster-alpha", Namespace: "shop", Service: "payments", Port: 8080}, RouteHit, nil
	}}

	res, err := ReadServiceGraph(context.Background(), q,
		5*time.Minute, end, sampleTopologyWithServices(), resolver, time.Second, false)
	require.NoError(t, err)
	require.Len(t, resolver.requests(), 1,
		"one broker FQDN in one anchor cluster = ONE route-store read, link and plain series included")

	// Both plain unknown-server callers resolved to the routed backend off the
	// single prefetched answer…
	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)
	abcEdge := findEdgeST(t, res, "cluster-alpha/abc", "cluster-alpha/shop/payments")
	findEdgeST(t, res, "cluster-alpha/pay0", "cluster-alpha/shop/payments")
	// …and the link series' via marker aligned with the same node ID.
	assert.Equal(t, "transport", abcEdge.Labels["relation"],
		"the link series' client via must mark the materialised broker edge")
	assert.Empty(t, res.ExternalNodes)
}
