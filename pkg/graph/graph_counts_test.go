package graph

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Cross-cluster classification must reflect the connection-string + route-
// engine model: pod-calls-service is registry-declared MayCrossCluster=true
// (a route-engine-resolved endpoint anchors on the selected ingress cluster,
// which may be a family sibling), so its edges bucket per-edge by the D9
// labels.cluster comparison — a D29 connection-string edge (local Service
// node) still buckets "false", a route-engine cross-cluster edge buckets
// "true". service-selects-pod fans out across same-family clusters and MAY
// cross (registry MayCrossCluster=true). The buckets are registry-driven,
// never a hardcoded type gate.
func TestEdgeCountByType_CrossClusterBuckets(t *testing.T) {
	clientPod := &PodNode{IDValue: "prod-1/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "prod-1"}}
	remotePod := &PodNode{IDValue: "prod-2/def", NameValue: "payments", LabelsValue: map[string]string{"cluster": "prod-2"}}
	localSvc := &ServiceNode{IDValue: "prod-1/messaging/nats", NameValue: "nats", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "messaging"}}
	remoteSvc := &ServiceNode{IDValue: "prod-2/shop/payments", NameValue: "payments", LabelsValue: map[string]string{"cluster": "prod-2", "namespace": "shop"}}
	backingRemote := &PodNode{IDValue: "prod-2/n2", NameValue: "nats-0", LabelsValue: map[string]string{"cluster": "prod-2"}}
	backingLocal := &PodNode{IDValue: "prod-1/n1", NameValue: "nats-1", LabelsValue: map[string]string{"cluster": "prod-1"}}
	ext := &ExternalNode{IDValue: "external/admin", NameValue: "admin", LabelsValue: map[string]string{}}

	edges := []*Edge{
		// Cross-cluster pod-calls-pod (server pod recovered via UID index).
		NewEdge(EdgeTypePodCallsPod, clientPod.IDValue, remotePod.IDValue, map[string]string{"cluster": "prod-1"}),
		// Intra-cluster pod-calls-service: the D29 connection-string path always
		// materialises the Service node in the caller's own cluster.
		NewEdge(EdgeTypePodCallsService, clientPod.IDValue, localSvc.IDValue, map[string]string{"cluster": "prod-1"}),
		// Cross-cluster pod-calls-service: a route-engine hit anchored on the
		// selected ingress cluster prod-2 while the caller lives in prod-1.
		NewEdge(EdgeTypePodCallsService, clientPod.IDValue, remoteSvc.IDValue, map[string]string{"cluster": "prod-1"}),
		// Cross-cluster service-selects-pod: local prod-1 Service node selecting a
		// backing pod in family sibling prod-2 (endpoint-union fan-out).
		NewEdge(EdgeTypeServiceSelectsPod, localSvc.IDValue, backingRemote.IDValue, map[string]string{"namespace": "messaging"}),
		// Intra-cluster service-selects-pod: local Service node → local pod.
		NewEdge(EdgeTypeServiceSelectsPod, localSvc.IDValue, backingLocal.IDValue, map[string]string{"namespace": "messaging"}),
		// External endpoints can never prove a cluster boundary → "false".
		NewEdge(EdgeTypePodCallsPod, ext.IDValue, clientPod.IDValue, map[string]string{}),
	}
	g := NewGraph([]GraphNode{clientPod, remotePod, localSvc, remoteSvc, backingRemote, backingLocal, ext}, edges, time.Unix(0, 0).UTC())

	counts := g.EdgeCountByType()
	assert.Equal(t, 1, counts[[2]string{string(EdgeTypePodCallsPod), "true"}])
	assert.Equal(t, 1, counts[[2]string{string(EdgeTypePodCallsPod), "false"}], "external endpoint buckets as false")
	assert.Equal(t, 1, counts[[2]string{string(EdgeTypePodCallsService), "false"}], "connection-string pod-calls-service stays intra-cluster")
	assert.Equal(t, 1, counts[[2]string{string(EdgeTypePodCallsService), "true"}], "route-engine cross-cluster pod-calls-service must bucket as true")
	assert.Equal(t, 1, counts[[2]string{string(EdgeTypeServiceSelectsPod), "true"}], "cross-cluster service-selects-pod must bucket as true")
	assert.Equal(t, 1, counts[[2]string{string(EdgeTypeServiceSelectsPod), "false"}])

	// The cross-cluster total is the sum of the "true" buckets — the single
	// EdgeCountByType scan is the one source for both metrics and the log line.
	cross := 0
	for k, n := range counts {
		if k[1] == "true" {
			cross += n
		}
	}
	assert.Equal(t, 3, cross, "one cross-cluster pod edge + one cross-cluster pod-calls-service + one cross-cluster service-selects-pod")
}

// TestCounts_ClusterIdentitiesAreDistinctClusters: the self-metric gauges key on
// labels.cluster, which now carries the composed identity, so two clusters
// sharing a raw name count separately and an edge between them is cross-cluster.
// Nothing in the counting code changes — the identity arrives on the labels.
func TestCounts_ClusterIdentitiesAreDistinctClusters(t *testing.T) {
	us := &PodNode{IDValue: "us-dev-c1/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "us-dev-c1"}}
	eu := &PodNode{IDValue: "eu-prod-c1/p2", NameValue: "payments", LabelsValue: map[string]string{"cluster": "eu-prod-c1"}}
	edge := NewEdge(EdgeTypePodCallsPod, us.IDValue, eu.IDValue, map[string]string{"cluster": "us-dev-c1"})

	g := NewGraph([]GraphNode{us, eu}, []*Edge{edge}, time.Now())
	g.ClusterIdentities = map[string]ClusterIdentity{
		"us-dev-c1":  {AZ: "us", Env: "dev", Name: "c1"},
		"eu-prod-c1": {AZ: "eu", Env: "prod", Name: "c1"},
	}

	assert.Equal(t, []string{"eu-prod-c1", "us-dev-c1"}, g.ClusterNames())

	nodes := g.NodeCountByKind()
	assert.Equal(t, 1, nodes[[2]string{"us-dev-c1", string(NodeTypePod)}])
	assert.Equal(t, 1, nodes[[2]string{"eu-prod-c1", string(NodeTypePod)}])
	assert.Zero(t, nodes[[2]string{"c1", string(NodeTypePod)}], "the raw name is not a metric dimension")

	assert.Equal(t, 1, g.EdgeCountByType()[[2]string{string(EdgeTypePodCallsPod), "true"}],
		"two identities behind one raw name make the edge cross-cluster")
}
