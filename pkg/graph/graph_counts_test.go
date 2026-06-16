package graph

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Cross-cluster classification must reflect the localised connection-string
// model: pod-calls-service resolves to a Service node in the caller's OWN
// cluster, so it is ALWAYS intra-cluster (registry MayCrossCluster=false), while
// service-selects-pod fans out across same-family clusters and MAY cross
// (registry MayCrossCluster=true). The buckets are registry-driven, never a
// hardcoded type gate.
func TestEdgeCountByType_CrossClusterBuckets(t *testing.T) {
	clientPod := &PodNode{IDValue: "prod-1/abc", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "prod-1"}}
	remotePod := &PodNode{IDValue: "prod-2/def", NameValue: "payments", LabelsValue: map[string]string{"cluster": "prod-2"}}
	localSvc := &ServiceNode{IDValue: "prod-1/messaging/nats", NameValue: "nats", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "messaging"}}
	backingRemote := &PodNode{IDValue: "prod-2/n2", NameValue: "nats-0", LabelsValue: map[string]string{"cluster": "prod-2"}}
	backingLocal := &PodNode{IDValue: "prod-1/n1", NameValue: "nats-1", LabelsValue: map[string]string{"cluster": "prod-1"}}
	ext := &ExternalNode{IDValue: "external/admin", NameValue: "admin", LabelsValue: map[string]string{}}

	edges := []*Edge{
		// Cross-cluster pod-calls-pod (server pod recovered via UID index).
		NewEdge(EdgeTypePodCallsPod, clientPod.IDValue, remotePod.IDValue, map[string]string{"cluster": "prod-1"}),
		// pod-calls-service is ALWAYS intra-cluster (local Service node) — the
		// registry declares it never-cross, so it buckets "false" without a lookup.
		NewEdge(EdgeTypePodCallsService, clientPod.IDValue, localSvc.IDValue, map[string]string{"cluster": "prod-1"}),
		// Cross-cluster service-selects-pod: local prod-1 Service node selecting a
		// backing pod in family sibling prod-2 (endpoint-union fan-out).
		NewEdge(EdgeTypeServiceSelectsPod, localSvc.IDValue, backingRemote.IDValue, map[string]string{"namespace": "messaging"}),
		// Intra-cluster service-selects-pod: local Service node → local pod.
		NewEdge(EdgeTypeServiceSelectsPod, localSvc.IDValue, backingLocal.IDValue, map[string]string{"namespace": "messaging"}),
		// External endpoints can never prove a cluster boundary → "false".
		NewEdge(EdgeTypePodCallsPod, ext.IDValue, clientPod.IDValue, map[string]string{}),
	}
	g := NewGraph([]GraphNode{clientPod, remotePod, localSvc, backingRemote, backingLocal, ext}, edges, time.Unix(0, 0).UTC())

	counts := g.EdgeCountByType()
	assert.Equal(t, 1, counts[[2]string{string(EdgeTypePodCallsPod), "true"}])
	assert.Equal(t, 1, counts[[2]string{string(EdgeTypePodCallsPod), "false"}], "external endpoint buckets as false")
	assert.Equal(t, 1, counts[[2]string{string(EdgeTypePodCallsService), "false"}], "pod-calls-service is always intra-cluster")
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
	assert.Equal(t, 2, cross, "one cross-cluster pod edge + one cross-cluster service-selects-pod edge")
}
