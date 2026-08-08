package build

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// Sentinel-peer exclusion note (design.md D30): the servicegraph connector's
// virtual peers (client / server ∈ {"user", "unknown"}) are dropped at the
// PromQL QUERY layer via anchored matchers on the QServiceGraphTotal selector
// (see internal/promql/queries.go + queries_test.go), NOT inside
// parseServiceGraph. These tests therefore do not exercise sentinel filtering
// at the parse level — a sentinel label handed directly to parseServiceGraph is
// (correctly) still resolved, because excluded series never reach the parser in
// production. End-to-end exclusion is proven against a real VictoriaMetrics in
// internal/integration (TestSentinelPeersExcludedAtQueryLayer). Do NOT add a
// parse-level sentinel filter here; it belongs upstream in the selector.

func sampleTopology() Topology {
	alphaPod := &graph.PodNode{
		IDValue:     "cluster-alpha/abc",
		NameValue:   "checkout",
		LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"},
	}
	betaPod := &graph.PodNode{
		IDValue:     "cluster-beta/def",
		NameValue:   "payments",
		LabelsValue: map[string]string{"cluster": "cluster-beta", "namespace": "billing"},
	}
	return Topology{
		Pods: []*graph.PodNode{alphaPod, betaPod},
		PodsByUID: map[string]*graph.PodNode{
			"abc": alphaPod,
			"def": betaPod,
		},
	}
}

// sampleTopologyWithServices adds D29 service / endpoint indexes:
//   - ClusterIP service "payments" (ns shop, cluster_ip 10.0.0.5) → pods pay0, pay1
//   - headless service "mongo" (ns db, cluster_ip None) → backing pod mongo0
//   - headless service "redis" (ns db, cluster_ip None) with NO endpointslice
//     entries (a service that resolves to a node but fans out to zero pods)
func sampleTopologyWithServices() Topology {
	clientPod := &graph.PodNode{IDValue: "cluster-alpha/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	pay0 := &graph.PodNode{IDValue: "cluster-alpha/pay0", NameValue: "payments-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	pay1 := &graph.PodNode{IDValue: "cluster-alpha/pay1", NameValue: "payments-1", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	mongo0 := &graph.PodNode{IDValue: "cluster-alpha/m0", NameValue: "mongo-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "db"}}
	return Topology{
		Pods:      []*graph.PodNode{clientPod, pay0, pay1, mongo0},
		PodsByUID: map[string]*graph.PodNode{"abc": clientPod, "pay0": pay0, "pay1": pay1, "m0": mongo0},
		ServicesByNameNS: map[serviceKey]ServiceObs{
			{"cluster-alpha", "shop", "payments"}: {ClusterIP: "10.0.0.5"},
			{"cluster-alpha", "db", "mongo"}:      {ClusterIP: "None"},
			{"cluster-alpha", "db", "redis"}:      {ClusterIP: "None"},
		},
		EndpointsByService: map[serviceKey][]EndpointObs{
			{"cluster-alpha", "shop", "payments"}: {{Pod: pay0}, {Pod: pay1}},
			{"cluster-alpha", "db", "mongo"}:      {{Pod: mongo0}},
			// "redis" deliberately has no backing endpoints → zero fan-out edges.
		},
	}
}

func sampleVec(samples ...model.Sample) model.Vector {
	out := make(model.Vector, len(samples))
	for i := range samples {
		s := samples[i]
		out[i] = &s
	}
	return out
}

// edgesByType partitions a result's edges by edge type.
func edgesByType(res ServiceGraphResult, t graph.EdgeType) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range res.Edges {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func TestParseServiceGraph_DropsZeroRate(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "abc",
		},
		Value: 0,
	})
	res := parseServiceGraph(vec, sampleTopology())
	assert.Empty(t, res.Edges)
}

func TestParseServiceGraph_CrossClusterEdge(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "payments",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "def",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopology())
	require.Len(t, res.Edges, 1)
	e := res.Edges[0]
	assert.Equal(t, "cluster-alpha/abc", e.Source)
	assert.Equal(t, "cluster-beta/def", e.Target, "server-side cluster recovered via UID index")
	assert.Equal(t, "cluster-alpha", e.Labels["cluster"], "edge cluster label = trace source cluster")
	for _, k := range []string{"client_cluster", "server_cluster", "rate", "p99_ms", "error_rate", "cross_cluster", "ghost"} {
		assert.NotContains(t, e.Labels, k, "unexpected label %q in v1 edge labels", k)
	}
}

func TestParseServiceGraph_IntraClusterEdge(t *testing.T) {
	alphaPod1 := &graph.PodNode{IDValue: "cluster-alpha/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	alphaPod2 := &graph.PodNode{IDValue: "cluster-alpha/xyz", NameValue: "cart", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	topo := Topology{
		Pods:      []*graph.PodNode{alphaPod1, alphaPod2},
		PodsByUID: map[string]*graph.PodNode{"abc": alphaPod1, "xyz": alphaPod2},
	}
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "xyz",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, topo)
	require.Len(t, res.Edges, 1)
	assert.Equal(t, "cluster-alpha", res.Edges[0].Labels["cluster"])
}

// ---------------------------------------------------------------------------
// D29: hardcoded "://" connection-string resolution.
// ---------------------------------------------------------------------------

func TestParseServiceGraph_ConnString_ServiceLevelResolvesToServiceNode(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "https://payments.shop.svc.cluster.local/api",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	// Service node materialised with cluster_ip on ipaddress.
	require.Len(t, res.ServiceNodes, 1)
	svc := res.ServiceNodes[0]
	assert.Equal(t, "cluster-alpha/shop/payments", svc.IDValue)
	assert.Equal(t, "payments", svc.NameValue)
	assert.Equal(t, map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}, svc.LabelsValue)
	assert.Equal(t, []string{"10.0.0.5"}, svc.IPAddressValue)

	// The call edge to a resolved service node is now typed pod-calls-service.
	var pcs []*graph.Edge
	for _, e := range res.Edges {
		if e.Type == graph.EdgeTypePodCallsService {
			pcs = append(pcs, e)
		}
	}
	require.Len(t, pcs, 1, "one pod-calls-service edge to the service node")
	assert.Equal(t, "cluster-alpha/abc", pcs[0].Source)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target, "target is the service node")
	assert.Equal(t, "cluster-alpha", pcs[0].Labels["cluster"], "client side is a pod → edge carries cluster")

	// service-selects-pod edges fan out to both backing pods.
	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 2)
	gotTargets := []string{ssp[0].Target, ssp[1].Target}
	assert.ElementsMatch(t, []string{"cluster-alpha/pay0", "cluster-alpha/pay1"}, gotTargets)
	for _, e := range ssp {
		assert.Equal(t, "cluster-alpha/shop/payments", e.Source)
		assert.Equal(t, "shop", e.Labels["namespace"])
	}
}

func TestParseServiceGraph_ConnString_HeadlessResolvesToServiceNode_WithFanout(t *testing.T) {
	// A headless <pod>.<service>.<namespace> string no longer resolves to the
	// specific addressed pod: the pod-hostname is dropped and it resolves to its
	// Service node, fanning out service-selects-pod edges to all backing pods.
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "mongodb://mongo-0.mongo.db.svc.cluster.local:27017",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1, "headless string resolves to its service node")
	assert.Equal(t, "cluster-alpha/db/mongo", res.ServiceNodes[0].IDValue)

	// The call edge to a resolved service node is now typed pod-calls-service.
	var pcs []*graph.Edge
	for _, e := range res.Edges {
		if e.Type == graph.EdgeTypePodCallsService {
			pcs = append(pcs, e)
		}
	}
	require.Len(t, pcs, 1, "one pod-calls-service edge to the service node")
	assert.Equal(t, "cluster-alpha/abc", pcs[0].Source)
	assert.Equal(t, "cluster-alpha/db/mongo", pcs[0].Target, "target is the service node, not a specific pod")
	assert.Equal(t, "cluster-alpha", pcs[0].Labels["cluster"], "client side is a pod → edge carries cluster")

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 1, "mongo fans out to its single backing pod")
	assert.Equal(t, "cluster-alpha/db/mongo", ssp[0].Source)
	assert.Equal(t, "cluster-alpha/m0", ssp[0].Target)
}

func TestParseServiceGraph_ConnString_HeadlessServiceWithNoEndpoints_StillResolvesToServiceNode(t *testing.T) {
	// "redis" is a known headless service with NO backing endpoints. It still
	// resolves to a service node (not others), with zero service-selects-pod edges.
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "redis://redis-0.redis.db.svc.cluster.local:6379",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "cluster-alpha/db/redis", res.ServiceNodes[0].IDValue)
	assert.Empty(t, edgesByType(res, graph.EdgeTypeServiceSelectsPod), "no backing pods → no fan-out edges")

	// The call edge to a resolved service node is now typed pod-calls-service.
	var pcs []*graph.Edge
	for _, e := range res.Edges {
		if e.Type == graph.EdgeTypePodCallsService {
			pcs = append(pcs, e)
		}
	}
	require.Len(t, pcs, 1, "one pod-calls-service edge to the service node")
	assert.Equal(t, "cluster-alpha/db/redis", pcs[0].Target, "target is the service node")
}

func TestParseServiceGraph_ConnString_ClientHeadlessResolvesToServiceNode_OmitsCluster(t *testing.T) {
	// A client-side headless connection string now resolves to a service node
	// (never a pod), so the edge OMITS labels.cluster — consistent with every
	// other non-pod client side (service / others / external).
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "mongodb://mongo-0.mongo.db.svc.cluster.local:27017",
			"server":             "checkout",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "",
			"server_k8s_pod_uid": "abc",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())
	pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pcp, 1)
	assert.Equal(t, "cluster-alpha/db/mongo", pcp[0].Source, "client headless string → service node")
	assert.Equal(t, "cluster-alpha/abc", pcp[0].Target)
	assert.NotContains(t, pcp[0].Labels, "cluster", "client resolved to a service node → edge omits cluster")
}

func TestParseServiceGraph_ConnString_UnresolvableExternalURL_BecomesExternal(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "https://payments.partner.example/api",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ExternalNodes, 1)
	ext := res.ExternalNodes[0]
	assert.Equal(t, "external/https://payments.partner.example/api", ext.IDValue)
	assert.Equal(t, "https://payments.partner.example/api", ext.NameValue)
	assert.Empty(t, ext.LabelsValue, "external node carries empty labels")

	pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pcp, 1)
	assert.Equal(t, "external/https://payments.partner.example/api", pcp[0].Target)
	assert.Equal(t, "cluster-alpha", pcp[0].Labels["cluster"], "client side is a pod")
}

func TestParseServiceGraph_ConnString_EmptyUIDWithURL_BothExternal(t *testing.T) {
	// Both endpoints have empty UID; both labels are "://" URLs. Both are
	// unresolvable → both fall back to external nodes (not others).
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "https://a.partner.example",
			"server":             "https://b.partner.example",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "",
			"server_k8s_pod_uid": "",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())
	assert.Len(t, res.ExternalNodes, 2, `unresolvable "://" labels now fall back to external`)
}

func TestParseServiceGraph_ConnString_NonK8sHostBecomesExternal(t *testing.T) {
	// A "://" connection string whose host is not a 2/3-label k8s .svc name —
	// e.g. an IP:port or a bare single-label host — is not classifiable as a
	// service record and falls back to an external node.
	for _, server := range []string{
		"grpc://10.0.0.5:50051",          // IP host → 4 dotted labels → unclassifiable
		"redis://my-redis:6379",          // single-label host → unclassifiable
		"amqp://broker.a.b.c.d.svc:5672", // >3 service-relative labels → unclassifiable
	} {
		t.Run(server, func(t *testing.T) {
			vec := sampleVec(model.Sample{
				Metric: model.Metric{
					"client": "checkout", "server": model.LabelValue(server),
					"cluster": "cluster-alpha", "client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "",
				},
				Value: 5,
			})
			res := parseServiceGraph(vec, sampleTopologyWithServices())
			require.Len(t, res.ExternalNodes, 1)
			assert.Equal(t, graph.ExternalID(server), res.ExternalNodes[0].IDValue)
			assert.Empty(t, res.ServiceNodes)
		})
	}
}

func TestParseServiceGraph_ConnString_UnknownServiceBecomesExternal(t *testing.T) {
	// A 2-label service-level connection string whose service is absent from the
	// trace cluster's topology resolves to an external node (labels={}).
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client": "checkout", "server": "https://ghost-svc.ghost-ns.svc.cluster.local/x",
			"cluster": "cluster-alpha", "client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())
	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/https://ghost-svc.ghost-ns.svc.cluster.local/x", res.ExternalNodes[0].IDValue)
	assert.Empty(t, res.ExternalNodes[0].LabelsValue)
	assert.Empty(t, res.ServiceNodes, "unknown service must not materialise a service node")
}

func TestParseServiceGraph_ConnString_ServiceMaterialisedOnceAcrossSeries(t *testing.T) {
	// Two distinct clients call the same ClusterIP service. The service node and
	// its service-selects-pod edges materialise exactly once (deduped), while
	// two pod-calls-pod edges are produced.
	topo := sampleTopologyWithServices()
	mk := func(client, clientUID string) model.Sample {
		return model.Sample{
			Metric: model.Metric{
				"client": model.LabelValue(client), "server": "https://payments.shop.svc.cluster.local/api",
				"cluster": "cluster-alpha", "client_k8s_pod_uid": model.LabelValue(clientUID), "server_k8s_pod_uid": "",
			},
			Value: 5,
		}
	}
	vec := sampleVec(mk("checkout", "abc"), mk("payments-0", "pay0"))
	res := parseServiceGraph(vec, topo)

	require.Len(t, res.ServiceNodes, 1, "service node materialised once despite two referencing series")
	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 2, "payments has two backing pods; each service-selects-pod edge deduped to one")
	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 2, "two distinct clients → two pod-calls-service edges to the service")
}

func TestParseServiceGraph_UIDPresentSkipsConnStringResolution(t *testing.T) {
	// A client label containing "://" but with a NON-empty UID resolves by pod
	// UID; Stage 0 does not run.
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "http://api.example.com",
			"server":             "payments",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "def",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopology())
	assert.Empty(t, res.ExternalNodes)
	require.Len(t, res.Edges, 1)
	assert.Equal(t, "cluster-alpha/abc", res.Edges[0].Source)
}

// ---------------------------------------------------------------------------
// D33: self-loop UID guard — an exporter that stamps the SAME pod UID on both
// client and server for a "://" peer must not collapse the URL side onto the
// caller's own pod. The "://" side's UID is treated as bogus so it falls
// through to D29 Stage 0 (connection-string resolution).
// ---------------------------------------------------------------------------

func TestParseServiceGraph_SelfLoopUID_ServerConnString_ResolvesToService(t *testing.T) {
	// Exporter bug: client_k8s_pod_uid == server_k8s_pod_uid ("abc") while the
	// real server is the "://" label. Without the guard, the server collapses to
	// pod "abc" (a self-loop pod-calls-pod) and no service node materialises.
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "https://payments.shop.svc.cluster.local/api",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "abc",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1, "bogus self-loop UID cleared → server resolves to its service node")
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1, "one pod-calls-service edge from the caller pod to the service")
	assert.Equal(t, "cluster-alpha/abc", pcs[0].Source, "client keeps its real UID")
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)
	assert.Equal(t, "cluster-alpha", pcs[0].Labels["cluster"], "client side is still a pod → edge carries cluster")

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 2, "service fans out to its two backing pods")

	for _, e := range edgesByType(res, graph.EdgeTypePodCallsPod) {
		assert.NotEqual(t, e.Source, e.Target, "no self-loop pod-calls-pod edge survives")
	}
}

func TestParseServiceGraph_SelfLoopUID_ClientConnString_ResolvesToService(t *testing.T) {
	// Symmetric: the "://" is on the CLIENT side, UIDs collide. The client UID is
	// the bogus one → client resolves to the service node, server keeps the pod.
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "https://payments.shop.svc.cluster.local/api",
			"server":             "checkout",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "abc",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)

	pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pcp, 1, "client → service, server → pod: one pod-calls-pod from service to pod")
	assert.Equal(t, "cluster-alpha/shop/payments", pcp[0].Source, "client '://' side → service node")
	assert.Equal(t, "cluster-alpha/abc", pcp[0].Target, "server keeps its real UID")
	assert.NotContains(t, pcp[0].Labels, "cluster", "client resolved to a non-pod → edge omits cluster")
}

func TestParseServiceGraph_SelfLoopUID_NoConnString_StaysPodSelfLoop(t *testing.T) {
	// Guard boundary: UIDs collide but NEITHER label is a "://" string. The guard
	// must NOT fire — a legitimate in-process self-call stays a pod-calls-pod
	// self-loop. Documents that the guard keys on "://", not on the collision alone.
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "checkout",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "abc",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())
	assert.Empty(t, res.ServiceNodes, "no '://' label → guard does not fire")
	pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pcp, 1)
	assert.Equal(t, "cluster-alpha/abc", pcp[0].Source)
	assert.Equal(t, "cluster-alpha/abc", pcp[0].Target, "stays a pod self-loop")
}

// ---------------------------------------------------------------------------
// resolve-unknown-server-peer-labels: the D30 server-side matcher is narrowed
// to server!~"user" (queries_test.go), so a literal server="unknown" now
// reaches parseServiceGraph. resolveUnknownServerPeer resolves it via the
// client-recorded client_net_peer_name / client_server_address labels when
// the client side is a REAL (non-synthesised) topology pod; every other case
// still drops the endpoint exactly as under the old blanket exclusion.
// ---------------------------------------------------------------------------

func TestParseServiceGraph_UnknownServerPeerLabel_NetPeerNameResolvesService(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":               "checkout",
			"server":               "unknown",
			"cluster":              "cluster-alpha",
			"client_k8s_pod_uid":   "abc",
			"server_k8s_pod_uid":   "",
			"client_net_peer_name": "payments.shop.svc.cluster.local",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/abc", pcs[0].Source)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)
	assert.Equal(t, "cluster-alpha", pcs[0].Labels["cluster"], "client side is a pod → edge carries cluster")

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 2, "payments fans out to its two backing pods")
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_ServerAddressFallback(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                "checkout",
			"server":                "unknown",
			"cluster":               "cluster-alpha",
			"client_k8s_pod_uid":    "abc",
			"server_k8s_pod_uid":    "",
			"client_net_peer_name":  "",
			"client_server_address": "payments.shop.svc.cluster.local:8080",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1, "port suffix stripped before classification")
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)
}

func TestParseServiceGraph_UnknownServerPeerLabel_BareShortName(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":               "checkout",
			"server":               "unknown",
			"cluster":              "cluster-alpha",
			"client_k8s_pod_uid":   "abc",
			"server_k8s_pod_uid":   "",
			"client_net_peer_name": "payments",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1, "bare short name resolves within the client pod's own namespace (shop)")
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)
}

func TestParseServiceGraph_UnknownServerPeerLabel_ExternalAddress(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":               "checkout",
			"server":               "unknown",
			"cluster":              "cluster-alpha",
			"client_k8s_pod_uid":   "abc",
			"server_k8s_pod_uid":   "",
			"client_net_peer_name": "payments.partner.example",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ExternalNodes, 1)
	ext := res.ExternalNodes[0]
	assert.Equal(t, "external/payments.partner.example", ext.IDValue)
	assert.Equal(t, "payments.partner.example", ext.NameValue)
	assert.Empty(t, ext.LabelsValue)
	assert.Empty(t, res.ServiceNodes)

	pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pcp, 1)
	assert.Equal(t, "cluster-alpha/abc", pcp[0].Source)
	assert.Equal(t, "external/payments.partner.example", pcp[0].Target)
	assert.Equal(t, "cluster-alpha", pcp[0].Labels["cluster"])
}

func TestParseServiceGraph_UnknownServerPeerLabel_AnchorLacksService(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":               "checkout",
			"server":               "unknown",
			"cluster":              "cluster-alpha",
			"client_k8s_pod_uid":   "abc",
			"server_k8s_pod_uid":   "",
			"client_net_peer_name": "web.shop.svc.cluster.local",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ExternalNodes, 1, "anchor cluster does not hold 'web' → external, not dropped")
	assert.Equal(t, "external/web.shop.svc.cluster.local", res.ExternalNodes[0].IDValue)
	assert.Empty(t, res.ServiceNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_NeitherLabelPresent_Dropped(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "unknown",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	assert.Empty(t, res.Edges, "no edge produced when neither peer label is present")
	assert.Empty(t, res.ExternalNodes)
	assert.Empty(t, res.ServiceNodes)
	assert.Empty(t, res.SynthPods)
}

func TestParseServiceGraph_UnknownServerPeerLabel_ClientUnresolved_Dropped(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":               "admin",
			"server":               "unknown",
			"cluster":              "cluster-alpha",
			"client_k8s_pod_uid":   "",
			"server_k8s_pod_uid":   "",
			"client_net_peer_name": "payments.shop.svc.cluster.local",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	// The client side ("admin", empty UID) still resolves via the pre-existing,
	// unrelated D27 fallback regardless of this change. But because that side is
	// NOT a real topology pod, resolveUnknownServerPeer's trigger condition is
	// unmet, so the server side never resolves and no edge touches it — the
	// presence of a peer label does not by itself cause enrichment.
	assert.Empty(t, res.Edges, "no edge: enrichment does not apply when the client is unresolved")
	assert.Empty(t, res.ServiceNodes, "no service node materialised — enrichment never ran")
}

func TestParseServiceGraph_UnknownServerPeerLabel_ServerUIDPresentButUnresolved(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":               "checkout",
			"server":               "unknown",
			"cluster":              "cluster-alpha",
			"client_k8s_pod_uid":   "abc",
			"server_k8s_pod_uid":   "stale-uid",
			"client_net_peer_name": "payments.shop.svc.cluster.local",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1, "enrichment applies even with a stale, topology-unknown server UID present")
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)
	assert.Empty(t, res.SynthPods, "no synth pod minted for the stale UID — enrichment wins over the synth-pod fallback")
}

// sampleTopologyIPFamily gives two family-sibling clusters (prod-1, prod-2,
// family "prod-0") each deploying their own "payments" service in namespace
// "shop", at DIFFERENT ClusterIPs — resolve-unknown-server-ip-peer's
// anchor-cluster-only IP lookup must never cross this family boundary.
func sampleTopologyIPFamily() Topology {
	clientPod := &graph.PodNode{IDValue: "prod-1/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "shop"}}
	p1a := &graph.PodNode{IDValue: "prod-1/p1a", NameValue: "payments-0", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "shop"}}
	p2a := &graph.PodNode{IDValue: "prod-2/p2a", NameValue: "payments-0", LabelsValue: map[string]string{"cluster": "prod-2", "namespace": "shop"}}
	return Topology{
		Pods:      []*graph.PodNode{clientPod, p1a, p2a},
		PodsByUID: map[string]*graph.PodNode{"abc": clientPod},
		ServicesByNameNS: map[serviceKey]ServiceObs{
			{"prod-1", "shop", "payments"}: {ClusterIP: "10.1.0.5"},
			{"prod-2", "shop", "payments"}: {ClusterIP: "10.2.0.5"},
		},
		EndpointsByService: map[serviceKey][]EndpointObs{
			{"prod-1", "shop", "payments"}: {{Pod: p1a}},
			{"prod-2", "shop", "payments"}: {{Pod: p2a}},
		},
	}
}

// sampleTopologyIPDuplicate is a defensive fixture: two Services in the SAME
// cluster share one ClusterIP (an anomaly Kubernetes itself prevents), used
// to assert the lexically-smaller (namespace, service) wins deterministically.
func sampleTopologyIPDuplicate() Topology {
	clientPod := &graph.PodNode{IDValue: "cluster-alpha/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	alphaPod := &graph.PodNode{IDValue: "cluster-alpha/a1", NameValue: "alpha-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "ops"}}
	zetaPod := &graph.PodNode{IDValue: "cluster-alpha/z1", NameValue: "zeta-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "ops"}}
	return Topology{
		Pods:      []*graph.PodNode{clientPod, alphaPod, zetaPod},
		PodsByUID: map[string]*graph.PodNode{"abc": clientPod},
		ServicesByNameNS: map[serviceKey]ServiceObs{
			{"cluster-alpha", "ops", "zeta"}:  {ClusterIP: "172.20.10.5"},
			{"cluster-alpha", "ops", "alpha"}: {ClusterIP: "172.20.10.5"},
		},
		EndpointsByService: map[serviceKey][]EndpointObs{
			{"cluster-alpha", "ops", "zeta"}:  {{Pod: zetaPod}},
			{"cluster-alpha", "ops", "alpha"}: {{Pod: alphaPod}},
		},
	}
}

func TestParseServiceGraph_UnknownServerPeerLabel_IPLiteralResolvesService(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                "checkout",
			"server":                "unknown",
			"cluster":               "cluster-alpha",
			"client_k8s_pod_uid":    "abc",
			"server_k8s_pod_uid":    "",
			"client_server_address": "10.0.0.5",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1, "bare IP literal matches the anchor cluster's own ClusterIP")
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 2, "normal service-selects-pod fan-out still applies once the Service is identified")
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_IPLiteralWithPort(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":               "checkout",
			"server":               "unknown",
			"cluster":              "cluster-alpha",
			"client_k8s_pod_uid":   "abc",
			"server_k8s_pod_uid":   "",
			"client_net_peer_name": "10.0.0.5:8080",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1, "port suffix stripped before IP-literal matching")
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)
}

func TestParseServiceGraph_UnknownServerPeerLabel_IPLiteralFamilySiblingNotMatched(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                "checkout",
			"server":                "unknown",
			"cluster":               "prod-1",
			"client_k8s_pod_uid":    "abc",
			"server_k8s_pod_uid":    "",
			"client_server_address": "10.2.0.5", // prod-2's ClusterIP, not prod-1's
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyIPFamily())

	require.Len(t, res.ExternalNodes, 1, "IP lookup is anchor-cluster-only — a family sibling's ClusterIP does not match")
	assert.Equal(t, "external/10.2.0.5", res.ExternalNodes[0].IDValue)
	assert.Empty(t, res.ServiceNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_IPLiteralNoMatch(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":               "checkout",
			"server":               "unknown",
			"cluster":              "cluster-alpha",
			"client_k8s_pod_uid":   "abc",
			"server_k8s_pod_uid":   "",
			"client_net_peer_name": "192.0.2.55",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/192.0.2.55", res.ExternalNodes[0].IDValue)
	assert.Empty(t, res.ServiceNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_IPLiteralDuplicateClusterIP(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                "checkout",
			"server":                "unknown",
			"cluster":               "cluster-alpha",
			"client_k8s_pod_uid":    "abc",
			"server_k8s_pod_uid":    "",
			"client_server_address": "172.20.10.5",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyIPDuplicate())

	require.Len(t, res.ServiceNodes, 1, "duplicate ClusterIP resolves deterministically")
	assert.Equal(t, "cluster-alpha/ops/alpha", res.ServiceNodes[0].IDValue, "lexically-smaller (namespace, service) wins: alpha < zeta")
}

// sampleTopologyPrecedence gives one client pod three DISTINCT, independently
// resolvable Services in its own namespace/cluster — "primary", "secondary",
// "tertiary" — so a precedence test can prove resolution targets exactly one
// of them regardless of which other (also-resolvable) labels are present.
func sampleTopologyPrecedence() Topology {
	clientPod := &graph.PodNode{IDValue: "cluster-alpha/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	prim0 := &graph.PodNode{IDValue: "cluster-alpha/prim0", NameValue: "primary-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	sec0 := &graph.PodNode{IDValue: "cluster-alpha/sec0", NameValue: "secondary-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	tert0 := &graph.PodNode{IDValue: "cluster-alpha/tert0", NameValue: "tertiary-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	return Topology{
		Pods:      []*graph.PodNode{clientPod, prim0, sec0, tert0},
		PodsByUID: map[string]*graph.PodNode{"abc": clientPod},
		ServicesByNameNS: map[serviceKey]ServiceObs{
			{"cluster-alpha", "shop", "primary"}:   {ClusterIP: "10.9.9.1"},
			{"cluster-alpha", "shop", "secondary"}: {ClusterIP: "10.9.9.2"},
			{"cluster-alpha", "shop", "tertiary"}:  {ClusterIP: "10.9.9.3"},
		},
		EndpointsByService: map[serviceKey][]EndpointObs{
			{"cluster-alpha", "shop", "primary"}:   {{Pod: prim0}},
			{"cluster-alpha", "shop", "secondary"}: {{Pod: sec0}},
			{"cluster-alpha", "shop", "tertiary"}:  {{Pod: tert0}},
		},
	}
}

func TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressResolvesService(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                      "checkout",
			"server":                      "unknown",
			"cluster":                     "cluster-alpha",
			"client_k8s_pod_uid":          "abc",
			"server_k8s_pod_uid":          "",
			"client_network_peer_address": "payments.shop.svc.cluster.local",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)

	ssp := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, ssp, 2, "payments fans out to its two backing pods")
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_ServerAddressWinsPrecedence(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                      "checkout",
			"server":                      "unknown",
			"cluster":                     "cluster-alpha",
			"client_k8s_pod_uid":          "abc",
			"server_k8s_pod_uid":          "",
			"client_server_address":       "primary.shop.svc.cluster.local",
			"client_network_peer_address": "secondary.shop.svc.cluster.local",
			"client_net_peer_name":        "tertiary.shop.svc.cluster.local",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyPrecedence())

	require.Len(t, res.ServiceNodes, 1, "only client_server_address's target is materialised")
	assert.Equal(t, "cluster-alpha/shop/primary", res.ServiceNodes[0].IDValue)

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/shop/primary", pcs[0].Target)
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressBeatsNetPeerName(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                      "checkout",
			"server":                      "unknown",
			"cluster":                     "cluster-alpha",
			"client_k8s_pod_uid":          "abc",
			"server_k8s_pod_uid":          "",
			"client_server_address":       "",
			"client_network_peer_address": "secondary.shop.svc.cluster.local",
			"client_net_peer_name":        "tertiary.shop.svc.cluster.local",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyPrecedence())

	require.Len(t, res.ServiceNodes, 1, "middle slot wins when client_server_address is absent")
	assert.Equal(t, "cluster-alpha/shop/secondary", res.ServiceNodes[0].IDValue)
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_NoFallThroughOnNonClassifying(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                      "checkout",
			"server":                      "unknown",
			"cluster":                     "cluster-alpha",
			"client_k8s_pod_uid":          "abc",
			"server_k8s_pod_uid":          "",
			"client_server_address":       "payments.partner.example",         // multi-label, not a .svc name — anchor lacks the addressed service
			"client_network_peer_address": "secondary.shop.svc.cluster.local", // WOULD resolve, must not be tried
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyPrecedence())

	require.Len(t, res.ExternalNodes, 1, "first non-empty label wins outright — no fall-through on classification failure")
	assert.Equal(t, "external/payments.partner.example", res.ExternalNodes[0].IDValue)
	assert.Empty(t, res.ServiceNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressBracketSuffixResolves(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                      "checkout",
			"server":                      "unknown",
			"cluster":                     "cluster-alpha",
			"client_k8s_pod_uid":          "abc",
			"server_k8s_pod_uid":          "",
			"client_network_peer_address": "payments.shop.svc.cluster.local:27017[-181]",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1, "bracket suffix truncated, then port stripped, before classification")
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)
	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressBracketSuffixExternalKeepsRaw(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                      "checkout",
			"server":                      "unknown",
			"cluster":                     "cluster-alpha",
			"client_k8s_pod_uid":          "abc",
			"server_k8s_pod_uid":          "",
			"client_network_peer_address": "mongo.com:27017[-181]",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ExternalNodes, 1)
	ext := res.ExternalNodes[0]
	assert.Equal(t, "external/mongo.com:27017[-181]", ext.IDValue, "raw value verbatim — bracket suffix AND port kept")
	assert.Equal(t, "mongo.com:27017[-181]", ext.NameValue)
	assert.Empty(t, res.ServiceNodes)

	pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pcp, 1)
	assert.Equal(t, "external/mongo.com:27017[-181]", pcp[0].Target)
}

func TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressBracketDistinctExternals(t *testing.T) {
	vec := sampleVec(
		model.Sample{
			Metric: model.Metric{
				"client":                      "checkout",
				"server":                      "unknown",
				"cluster":                     "cluster-alpha",
				"client_k8s_pod_uid":          "abc",
				"server_k8s_pod_uid":          "",
				"client_network_peer_address": "mongo.com:27017[-181]",
			},
			Value: 5,
		},
		model.Sample{
			Metric: model.Metric{
				"client":                      "checkout",
				"server":                      "unknown",
				"cluster":                     "cluster-alpha",
				"client_k8s_pod_uid":          "abc",
				"server_k8s_pod_uid":          "",
				"client_network_peer_address": "mongo.com:27017[-182]",
			},
			Value: 5,
		},
	)
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ExternalNodes, 2, "raw-value naming is not deduplicated by host")
	ids := []string{res.ExternalNodes[0].IDValue, res.ExternalNodes[1].IDValue}
	assert.ElementsMatch(t, []string{"external/mongo.com:27017[-181]", "external/mongo.com:27017[-182]"}, ids)
}

func TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressBracketIPLiteral(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                      "checkout",
			"server":                      "unknown",
			"cluster":                     "cluster-alpha",
			"client_k8s_pod_uid":          "abc",
			"server_k8s_pod_uid":          "",
			"client_network_peer_address": "10.0.0.5:27017[-9]",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1, "bracket then port stripped, ClusterIP lookup hits")
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_BracketSuffixOnNetPeerName(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":               "checkout",
			"server":               "unknown",
			"cluster":              "cluster-alpha",
			"client_k8s_pod_uid":   "abc",
			"server_k8s_pod_uid":   "",
			"client_net_peer_name": "payments.shop.svc.cluster.local:27017[-181]",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1, "truncation is uniform across all three labels, including the deprecated one")
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)
}

func TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerPortIgnored(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                   "checkout",
			"server":                   "unknown",
			"cluster":                  "cluster-alpha",
			"client_k8s_pod_uid":       "abc",
			"server_k8s_pod_uid":       "",
			"client_network_peer_port": "27017",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	assert.Empty(t, res.Edges, "port label is never read as a peer address")
	assert.Empty(t, res.ExternalNodes)
	assert.Empty(t, res.ServiceNodes)
	assert.Empty(t, res.SynthPods)
}

// sampleTopologyIPv6 gives a Service with an IPv6 ClusterIP so bracket-vs-IPv6
// interaction tests (2.10 / 2.11) have a real dual-stack address to match
// against — sampleTopologyWithServices only carries IPv4 ClusterIPs.
func sampleTopologyIPv6() Topology {
	clientPod := &graph.PodNode{IDValue: "cluster-alpha/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	pay0 := &graph.PodNode{IDValue: "cluster-alpha/pay0", NameValue: "payments-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	return Topology{
		Pods:      []*graph.PodNode{clientPod, pay0},
		PodsByUID: map[string]*graph.PodNode{"abc": clientPod},
		ServicesByNameNS: map[serviceKey]ServiceObs{
			{"cluster-alpha", "shop", "payments"}: {ClusterIP: "fd00:10:96::a"},
		},
		EndpointsByService: map[serviceKey][]EndpointObs{
			{"cluster-alpha", "shop", "payments"}: {{Pod: pay0}},
		},
	}
}

func TestParseServiceGraph_UnknownServerPeerLabel_BracketedIPv6NotTruncated(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                      "checkout",
			"server":                      "unknown",
			"cluster":                     "cluster-alpha",
			"client_k8s_pod_uid":          "abc",
			"server_k8s_pod_uid":          "",
			"client_network_peer_address": "[fd00:10:96::a]:8080",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyIPv6())

	require.Len(t, res.ServiceNodes, 1, "leading '[' is index 0 — not truncated; host/port split strips brackets and resolves by ClusterIP")
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_BracketedIPv6WithIdentifierExternal(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                      "checkout",
			"server":                      "unknown",
			"cluster":                     "cluster-alpha",
			"client_k8s_pod_uid":          "abc",
			"server_k8s_pod_uid":          "",
			"client_network_peer_address": "[fd00:10:96::a]:8080[-181]",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyIPv6())

	require.Len(t, res.ExternalNodes, 1, "index-0 guard leaves it untruncated; host/port split fails on the trailing '['; bare-short-name stage matches but no Service answers")
	ext := res.ExternalNodes[0]
	assert.Equal(t, "external/[fd00:10:96::a]:8080[-181]", ext.IDValue)
	assert.Equal(t, "[fd00:10:96::a]:8080[-181]", ext.NameValue)
	assert.Empty(t, res.ServiceNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_TruncationPromotesToBareShortName(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                      "checkout",
			"server":                      "unknown",
			"cluster":                     "cluster-alpha",
			"client_k8s_pod_uid":          "abc",
			"server_k8s_pod_uid":          "",
			"client_network_peer_address": "payments:8080[-181]",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ServiceNodes, 1, "truncation reduces the value to the bare short name 'payments', resolved in the client's own namespace (shop)")
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_UnknownServerPeerLabel_TruncationPromotesToBareShortName_NoMatchStaysExternal(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":                      "checkout",
			"server":                      "unknown",
			"cluster":                     "cluster-alpha",
			"client_k8s_pod_uid":          "abc",
			"server_k8s_pod_uid":          "",
			"client_network_peer_address": "nosuchsvc:8080[-181]",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopologyWithServices())

	require.Len(t, res.ExternalNodes, 1)
	ext := res.ExternalNodes[0]
	assert.Equal(t, "external/nosuchsvc:8080[-181]", ext.IDValue, "raw value verbatim")
	assert.Equal(t, "nosuchsvc:8080[-181]", ext.NameValue)
	assert.Empty(t, res.ServiceNodes)
}

// ---------------------------------------------------------------------------
// resolve-unknown-server-pod-ip-peer: an IP-literal peer that matches no
// Service ClusterIP is looked up against the anchor cluster's Pod IP set.

// sampleTopologyPodIP extends the service fixture with pod-level addresses:
//   - "cluster-alpha/def" holds pod_ip 10.244.1.9 — a pod dialled directly,
//     behind no Service
//   - "cluster-alpha/col" holds pod_ip 10.0.0.5 — the SAME address as the
//     shop/payments ClusterIP, so it can only be reached if the ClusterIP step
//     were (wrongly) skipped
//   - "cluster-alpha/nip" carries no address at all and must never be indexed
func sampleTopologyPodIP() Topology {
	topo := sampleTopologyWithServices()
	direct := &graph.PodNode{
		IDValue: "cluster-alpha/def", NameValue: "backend",
		LabelsValue:    map[string]string{"cluster": "cluster-alpha", "namespace": "shop"},
		IPAddressValue: []string{"10.244.1.9"},
	}
	collide := &graph.PodNode{
		IDValue: "cluster-alpha/col", NameValue: "collider",
		LabelsValue:    map[string]string{"cluster": "cluster-alpha", "namespace": "shop"},
		IPAddressValue: []string{"10.0.0.5"}, // == shop/payments ClusterIP
	}
	noIP := &graph.PodNode{
		IDValue: "cluster-alpha/nip", NameValue: "addressless",
		LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"},
	}
	topo.Pods = append(topo.Pods, direct, collide, noIP)
	return topo
}

// podIPPod is a terse constructor for the family fixtures below.
func podIPPod(cluster, uid, ip string) *graph.PodNode {
	return &graph.PodNode{
		IDValue: cluster + "/" + uid, NameValue: "backend-" + uid,
		LabelsValue:    map[string]string{"cluster": cluster, "namespace": "shop"},
		IPAddressValue: []string{ip},
	}
}

// sampleTopologyPodIPFamily seeds the cross-cluster cases on top of the
// prod-1 / prod-2 family fixture (family "prod-0"), plus a staging-1 pod that
// must stay unreachable (different family). Callers pass the extra pods they
// want; the prod-1 client pod "abc" is always present.
func sampleTopologyPodIPFamily(extra ...*graph.PodNode) Topology {
	topo := sampleTopologyIPFamily()
	topo.Pods = append(topo.Pods, extra...)
	return topo
}

// reversePods returns the topology with topology.Pods in reverse order — used
// to prove the index build and the candidate pick are order-free (D6).
func reversePods(topo Topology) Topology {
	out := make([]*graph.PodNode, len(topo.Pods))
	for i, p := range topo.Pods {
		out[len(topo.Pods)-1-i] = p
	}
	topo.Pods = out
	return topo
}

// sampleTopologyPodIPDuplicate models the hostNetwork case: two pods in ONE
// cluster reporting their shared node address. reverse flips the load order to
// prove the pick does not depend on topology.Pods ordering.
func sampleTopologyPodIPDuplicate(reverse bool) Topology {
	clientPod := &graph.PodNode{IDValue: "cluster-alpha/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	zzz := &graph.PodNode{
		IDValue: "cluster-alpha/zzz", NameValue: "host-z",
		LabelsValue:    map[string]string{"cluster": "cluster-alpha", "namespace": "infra"},
		IPAddressValue: []string{"10.244.1.9"},
	}
	aaa := &graph.PodNode{
		IDValue: "cluster-alpha/aaa", NameValue: "host-a",
		LabelsValue:    map[string]string{"cluster": "cluster-alpha", "namespace": "infra"},
		IPAddressValue: []string{"10.244.1.9"},
	}
	pods := []*graph.PodNode{clientPod, zzz, aaa}
	if reverse {
		pods = []*graph.PodNode{clientPod, aaa, zzz}
	}
	return Topology{Pods: pods, PodsByUID: map[string]*graph.PodNode{"abc": clientPod}}
}

func podIPPeerSample(peer string) model.Sample {
	return model.Sample{
		Metric: model.Metric{
			"client":                "checkout",
			"server":                "unknown",
			"cluster":               "cluster-alpha",
			"client_k8s_pod_uid":    "abc",
			"server_k8s_pod_uid":    "",
			"client_server_address": model.LabelValue(peer),
		},
		Value: 5,
	}
}

func TestParseServiceGraph_UnknownServerPeerLabel_PodIPResolvesPod(t *testing.T) {
	res := parseServiceGraph(sampleVec(podIPPeerSample("10.244.1.9")), sampleTopologyPodIP())

	pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pcp, 1)
	assert.Equal(t, "cluster-alpha/abc", pcp[0].Source)
	assert.Equal(t, "cluster-alpha/def", pcp[0].Target, "peer IP matched the backend pod's own pod_ip")
	assert.Equal(t, "cluster-alpha", pcp[0].Labels["cluster"])

	assert.Empty(t, res.ExternalNodes)
	assert.Empty(t, res.SynthPods)
	assert.Empty(t, res.ServiceNodes, "a direct Pod IP call materialises no service node")
	assert.Empty(t, edgesByType(res, graph.EdgeTypeServiceSelectsPod), "and no fan-out")
}

func TestParseServiceGraph_UnknownServerPeerLabel_PodIPWithPort(t *testing.T) {
	res := parseServiceGraph(sampleVec(podIPPeerSample("10.244.1.9:8080")), sampleTopologyPodIP())

	pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pcp, 1)
	assert.Equal(t, "cluster-alpha/def", pcp[0].Target, "port suffix stripped before the Pod IP lookup")
	assert.Empty(t, res.ExternalNodes)
}

// The ClusterIP step runs inside classifyPeerHost, before the Pod IP step is
// reached — so a colliding pod address can never shadow a real Service.
func TestParseServiceGraph_UnknownServerPeerLabel_ClusterIPBeatsPodIP(t *testing.T) {
	res := parseServiceGraph(sampleVec(podIPPeerSample("10.0.0.5")), sampleTopologyPodIP())

	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", res.ServiceNodes[0].IDValue)

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)
	for _, e := range edgesByType(res, graph.EdgeTypePodCallsPod) {
		assert.NotEqual(t, "cluster-alpha/col", e.Target, "the colliding pod must not be reached")
	}
}

// familyPodIPSample is podIPPeerSample anchored in prod-1 (family "prod-0").
func familyPodIPSample(peer string) model.Sample {
	s := podIPPeerSample(peer)
	s.Metric["cluster"] = "prod-1"
	return s
}

// A pod IP held by exactly one family sibling resolves across the cluster
// boundary: cross-cluster pod-to-pod dialling over a flat network is real
// traffic, and being the lone family holder is direct evidence that the
// family's pod CIDRs do not overlap at this address.
func TestParseServiceGraph_UnknownServerPeerLabel_PodIPFamilySiblingResolves(t *testing.T) {
	topo := sampleTopologyPodIPFamily(podIPPod("prod-2", "sib", "10.244.1.9"))

	for _, reverse := range []bool{false, true} {
		fixture := topo
		if reverse {
			fixture = reversePods(topo)
		}
		res := parseServiceGraph(sampleVec(familyPodIPSample("10.244.1.9")), fixture)

		pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
		require.Len(t, pcp, 1, "reverse=%v", reverse)
		assert.Equal(t, "prod-1/abc", pcp[0].Source)
		assert.Equal(t, "prod-2/sib", pcp[0].Target, "lone family holder resolves across clusters (reverse=%v)", reverse)
		assert.Equal(t, "prod-1", pcp[0].Labels["cluster"], "edge cluster stays the client side (D9)")
		assert.Empty(t, res.ExternalNodes)
		assert.Empty(t, res.ServiceNodes)
	}
}

// The anchor cluster's own holder wins even when a family sibling carries the
// same address — a caller most plausibly reached a pod in its own cluster, and
// this path is byte-for-byte the pre-widening behaviour.
func TestParseServiceGraph_UnknownServerPeerLabel_PodIPAnchorBeatsSibling(t *testing.T) {
	topo := sampleTopologyPodIPFamily(
		podIPPod("prod-1", "own", "10.244.1.9"),
		podIPPod("prod-2", "sib", "10.244.1.9"),
	)

	for _, reverse := range []bool{false, true} {
		fixture := topo
		if reverse {
			fixture = reversePods(topo)
		}
		res := parseServiceGraph(sampleVec(familyPodIPSample("10.244.1.9")), fixture)

		pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
		require.Len(t, pcp, 1, "reverse=%v", reverse)
		assert.Equal(t, "prod-1/own", pcp[0].Target, "anchor cluster wins (reverse=%v)", reverse)
		assert.Empty(t, res.ExternalNodes)
	}
}

// Two family siblings holding the address means the family's pod CIDRs overlap
// here; picking one would fabricate a dependency, so the endpoint degrades.
func TestParseServiceGraph_UnknownServerPeerLabel_PodIPFamilyAmbiguousDegrades(t *testing.T) {
	topo := sampleTopologyPodIPFamily(
		podIPPod("prod-2", "sib2", "10.244.1.9"),
		podIPPod("prod-3", "sib3", "10.244.1.9"),
	)

	for _, reverse := range []bool{false, true} {
		fixture := topo
		if reverse {
			fixture = reversePods(topo)
		}
		res := parseServiceGraph(sampleVec(familyPodIPSample("10.244.1.9")), fixture)

		require.Len(t, res.ExternalNodes, 1, "reverse=%v", reverse)
		assert.Equal(t, "external/10.244.1.9", res.ExternalNodes[0].IDValue)
		for _, e := range edgesByType(res, graph.EdgeTypePodCallsPod) {
			assert.NotEqual(t, "prod-2/sib2", e.Target, "reverse=%v", reverse)
			assert.NotEqual(t, "prod-3/sib3", e.Target, "reverse=%v", reverse)
		}
	}
}

// The family boundary still holds: staging-1 is family "staging-0", not
// "prod-0", so its pod is never a candidate for a prod-1 caller.
func TestParseServiceGraph_UnknownServerPeerLabel_PodIPOtherFamilyNotMatched(t *testing.T) {
	topo := sampleTopologyPodIPFamily(podIPPod("staging-1", "stg", "10.244.1.9"))
	res := parseServiceGraph(sampleVec(familyPodIPSample("10.244.1.9")), topo)

	require.Len(t, res.ExternalNodes, 1, "a different cluster family is never consulted")
	assert.Equal(t, "external/10.244.1.9", res.ExternalNodes[0].IDValue)
	for _, e := range edgesByType(res, graph.EdgeTypePodCallsPod) {
		assert.NotEqual(t, "staging-1/stg", e.Target)
	}
}

func TestParseServiceGraph_UnknownServerPeerLabel_PodIPDuplicateDeterministic(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		res := parseServiceGraph(sampleVec(podIPPeerSample("10.244.1.9")), sampleTopologyPodIPDuplicate(reverse))

		pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
		require.Len(t, pcp, 1, "reverse=%v", reverse)
		assert.Equal(t, "cluster-alpha/aaa", pcp[0].Target,
			"lexically-smallest pod id wins regardless of load order (reverse=%v)", reverse)
	}
}

func TestParseServiceGraph_UnknownServerPeerLabel_PodWithoutIPNotIndexed(t *testing.T) {
	// 198.51.100.4 is held by no Service and by no pod — the addressless pod in
	// the fixture must not be reachable by any IP.
	res := parseServiceGraph(sampleVec(podIPPeerSample("198.51.100.4")), sampleTopologyPodIP())

	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/198.51.100.4", res.ExternalNodes[0].IDValue)
	for _, e := range edgesByType(res, graph.EdgeTypePodCallsPod) {
		assert.NotEqual(t, "cluster-alpha/nip", e.Target)
	}
}

func TestParseServiceGraph_GhostFallback_ServerUIDUnknown(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "missing",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "missing-uid",
		},
		Value: 1,
	})
	res := parseServiceGraph(vec, sampleTopology())
	require.Len(t, res.SynthPods, 1)
	sp := res.SynthPods[0]
	assert.Equal(t, "/missing-uid", sp.IDValue, "synth pod ID has empty cluster prefix when server cluster unknown")
	assert.Empty(t, sp.LabelsValue["cluster"], "server-side synth pod has empty cluster label")
	assert.NotContains(t, sp.LabelsValue, "ghost", "ghost label must NOT be set in v1")
}

func TestParseServiceGraph_EmptyVectorIsNotAnError(t *testing.T) {
	res := parseServiceGraph(nil, sampleTopology())
	assert.Empty(t, res.Edges)
}

func TestParseServiceGraph_DedupSamePair(t *testing.T) {
	vec := sampleVec(
		model.Sample{
			Metric: model.Metric{
				"client": "checkout", "server": "payments", "cluster": "cluster-alpha",
				"client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "def", "connection_type": "virtual_node",
			},
			Value: 5,
		},
		model.Sample{
			Metric: model.Metric{
				"client": "checkout", "server": "payments", "cluster": "cluster-alpha",
				"client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "def", "connection_type": "messaging_system",
			},
			Value: 3,
		},
	)
	res := parseServiceGraph(vec, sampleTopology())
	require.Len(t, res.Edges, 1, "duplicate (src,tgt) series must collapse into one edge")
	ids := map[string]int{}
	for _, e := range res.Edges {
		ids[e.ID]++
	}
	for id, n := range ids {
		assert.Equal(t, 1, n, "edge id %s appeared %d times", id, n)
	}
}

// ---------------------------------------------------------------------------
// D27: Missing pod-UID human-label fallback (non-URL labels only).
// ---------------------------------------------------------------------------

func TestParseServiceGraph_MissingClientUID_PromotesToExternal(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client": "admin", "server": "checkout", "cluster": "cluster-alpha",
			"client_k8s_pod_uid": "", "server_k8s_pod_uid": "abc",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopology())

	require.Len(t, res.Edges, 1)
	e := res.Edges[0]
	assert.Equal(t, "external/admin", e.Source)
	assert.Equal(t, "cluster-alpha/abc", e.Target)
	assert.NotContains(t, e.Labels, "cluster",
		"edge cluster label MUST be omitted when client side is external (missing-UID fallback)")

	require.Len(t, res.ExternalNodes, 1)
	ext := res.ExternalNodes[0]
	assert.Equal(t, "external/admin", ext.IDValue)
	assert.Equal(t, "admin", ext.NameValue)
	assert.Empty(t, ext.LabelsValue)
}

func TestParseServiceGraph_MissingServerUID_PromotesToExternal(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client": "checkout", "server": "payments", "cluster": "cluster-alpha",
			"client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopology())

	require.Len(t, res.Edges, 1)
	e := res.Edges[0]
	assert.Equal(t, "cluster-alpha/abc", e.Source)
	assert.Equal(t, "external/payments", e.Target)
	assert.Equal(t, "cluster-alpha", e.Labels["cluster"],
		"edge keeps labels.cluster when client side is still a pod")

	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/payments", res.ExternalNodes[0].IDValue)
	assert.Empty(t, res.ExternalNodes[0].LabelsValue)
}

func TestParseServiceGraph_BothUIDsMissing_BothLabelsPresent(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client": "admin", "server": "payments", "cluster": "cluster-alpha",
			"client_k8s_pod_uid": "", "server_k8s_pod_uid": "",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopology())

	require.Len(t, res.Edges, 1)
	e := res.Edges[0]
	assert.Equal(t, "external/admin", e.Source)
	assert.Equal(t, "external/payments", e.Target)
	assert.NotContains(t, e.Labels, "cluster")

	require.Len(t, res.ExternalNodes, 2)
	gotIDs := map[string]bool{}
	for _, ext := range res.ExternalNodes {
		gotIDs[ext.IDValue] = true
	}
	assert.True(t, gotIDs["external/admin"])
	assert.True(t, gotIDs["external/payments"])
}

func TestParseServiceGraph_UIDAndLabelBothEmpty_EdgeDropped_ClientSide(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client": "", "server": "checkout", "cluster": "cluster-alpha",
			"client_k8s_pod_uid": "", "server_k8s_pod_uid": "abc",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopology())
	assert.Empty(t, res.Edges, "edge MUST be dropped when both client UID and label are empty")
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_UIDAndLabelBothEmpty_EdgeDropped_ServerSide(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client": "checkout", "server": "", "cluster": "cluster-alpha",
			"client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, sampleTopology())
	assert.Empty(t, res.Edges, "edge MUST be dropped when both server UID and label are empty")
	assert.Empty(t, res.ExternalNodes)
}

func TestParseServiceGraph_ConnStringWinsOverMissingUIDFallback(t *testing.T) {
	// A "://" client label with empty UID is handled by connection-string
	// resolution (here unresolvable → external, labels={}); the missing-UID
	// external fallback path is the same destination but connection-string
	// resolution still runs first (Stage 0 wins).
	alphaPod := &graph.PodNode{IDValue: "cluster-alpha/abc", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	topo := Topology{Pods: []*graph.PodNode{alphaPod}, PodsByUID: map[string]*graph.PodNode{"abc": alphaPod}}
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client": "http://api.example.com", "server": "checkout", "cluster": "cluster-alpha",
			"client_k8s_pod_uid": "", "server_k8s_pod_uid": "abc",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, topo)

	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "external/http://api.example.com", res.ExternalNodes[0].IDValue)
	assert.Empty(t, res.ExternalNodes[0].LabelsValue, "external node carries empty labels")
}

func TestParseServiceGraph_ConnStringAndMissingUIDBothExternal(t *testing.T) {
	// One parse can produce TWO distinct external nodes: one from an unresolvable
	// "://" string (connection-string resolution falls back to external) and one
	// from a non-URL missing-UID label (D27 fallback). Both produce external nodes
	// with distinct IDs (the verbatim labels differ); no others nodes remain.
	betaPod := &graph.PodNode{IDValue: "cluster-beta/def", LabelsValue: map[string]string{"cluster": "cluster-beta"}}
	topo := Topology{Pods: []*graph.PodNode{betaPod}, PodsByUID: map[string]*graph.PodNode{"def": betaPod}}
	vec := sampleVec(
		model.Sample{
			Metric: model.Metric{
				"client": "https://ext.partner.example/x", "server": "payments", "cluster": "cluster-alpha",
				"client_k8s_pod_uid": "", "server_k8s_pod_uid": "def",
			},
			Value: 5,
		},
		model.Sample{
			Metric: model.Metric{
				"client": "stray-caller", "server": "payments", "cluster": "cluster-alpha",
				"client_k8s_pod_uid": "", "server_k8s_pod_uid": "def",
			},
			Value: 3,
		},
	)
	res := parseServiceGraph(vec, topo)

	require.Len(t, res.ExternalNodes, 2, `both "://" fallback and missing-UID fallback produce external nodes`)
	gotIDs := map[string]bool{}
	for _, ext := range res.ExternalNodes {
		gotIDs[ext.IDValue] = true
		assert.Empty(t, ext.LabelsValue)
	}
	assert.True(t, gotIDs["external/https://ext.partner.example/x"], `unresolvable "://" string → external/<verbatim label>`)
	assert.True(t, gotIDs["external/stray-caller"], "non-URL missing-UID label → external/<verbatim label>")
}

func TestProperty_ParseServiceGraph_EveryEdgeHasNonEmptyEndpoints(t *testing.T) {
	topo := sampleTopology()
	for seed := int64(1); seed <= 25; seed++ {
		r := rand.New(rand.NewSource(seed))
		samples := make([]model.Sample, 0, 20)
		for i := range 20 {
			clientUID := pickUID(r)
			serverUID := pickUID(r)
			clientLabel := pickLabel(r, "client", i)
			serverLabel := pickLabel(r, "server", i)
			if clientUID == "" && clientLabel == "" {
				continue
			}
			if serverUID == "" && serverLabel == "" {
				continue
			}
			samples = append(samples, model.Sample{
				Metric: model.Metric{
					"client":             model.LabelValue(clientLabel),
					"server":             model.LabelValue(serverLabel),
					"cluster":            "cluster-alpha",
					"client_k8s_pod_uid": model.LabelValue(clientUID),
					"server_k8s_pod_uid": model.LabelValue(serverUID),
				},
				Value: 5,
			})
		}
		res := parseServiceGraph(sampleVec(samples...), topo)
		for _, e := range res.Edges {
			require.NotEmptyf(t, e.Source, "seed=%d: edge has empty source id", seed)
			require.NotEmptyf(t, e.Target, "seed=%d: edge has empty target id", seed)
		}
	}
}

func pickUID(r *rand.Rand) string {
	switch r.Intn(4) {
	case 0:
		return "abc"
	case 1:
		return "def"
	case 2:
		return "ghost-uid"
	default:
		return ""
	}
}

func pickLabel(r *rand.Rand, side string, i int) string {
	if r.Intn(4) == 0 {
		return ""
	}
	return fmt.Sprintf("%s-%d", side, i)
}

func TestParseServiceGraph_NoForbiddenNumericLabels(t *testing.T) {
	alphaPod1 := &graph.PodNode{IDValue: "cluster-alpha/abc", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	alphaPod2 := &graph.PodNode{IDValue: "cluster-alpha/def", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"cluster": "cluster-alpha", "client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "def",
		},
		Value: 5,
	})
	res := parseServiceGraph(vec, Topology{
		Pods:      []*graph.PodNode{alphaPod1, alphaPod2},
		PodsByUID: map[string]*graph.PodNode{"abc": alphaPod1, "def": alphaPod2},
	})
	for _, e := range res.Edges {
		for _, k := range []string{"rate", "p99_ms", "error_rate"} {
			assert.NotContains(t, e.Labels, k)
		}
	}
}
