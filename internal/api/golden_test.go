package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/cytoscape"
	"github.com/marz32one/kube-state-graph/pkg/graph"
)

var update = flag.Bool("update", false, "update golden files")

// TestGolden_GraphResponses snapshots the Cytoscape responses for several
// canned scenarios so contract drift is caught on PR.
func TestGolden_GraphResponses(t *testing.T) {
	scenarios := map[string]graph.View{
		"single-cluster":       buildSingleCluster(),
		"two-cluster-cross":    buildTwoClusterCross(),
		"with-service":         buildWithService(),
		"family-fanout":        buildFamilyFanout(),
		"with-storageclass":    buildWithStorageClass(),
		"name-filter":          buildNameFilter(),
		"missing-uid-fallback": buildMissingUIDFallback(),
	}

	for name, view := range scenarios {
		t.Run(name+"-cytoscape", func(t *testing.T) {
			g := &graph.Graph{BuiltAt: time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC), NodesByID: map[string]graph.GraphNode{}}
			for _, n := range view.Nodes {
				g.NodesByID[n.ID()] = n
			}
			body := cytoscape.Serialise(g, view)
			compareGolden(t, name+"-cytoscape.json", body)
		})
	}
}

func TestGolden_EdgeTypes(t *testing.T) {
	body := map[string]any{
		"apiVersion": APIVersion,
		"edge_types": graph.EdgeTypes,
	}
	compareGolden(t, "edge-types.json", body)
}

func compareGolden(t *testing.T, file string, body any) {
	t.Helper()
	got, err := json.MarshalIndent(body, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')
	path := filepath.Join("testdata", "golden", file)

	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))
		return
	}

	want, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read golden (run with -update)")
	assert.Truef(t, bytes.Equal(want, got), "golden mismatch for %s\n--- want\n%s\n--- got\n%s", file, want, got)
}

// ----- canned scenarios -----------------------------------------------------

func buildSingleCluster() graph.View {
	pod := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}}
	node := &graph.K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	return graph.View{Nodes: []graph.GraphNode{node, pod}}
}

func buildTwoClusterCross() graph.View {
	a := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	b := &graph.PodNode{IDValue: "cluster-beta/p2", NameValue: "payments", LabelsValue: map[string]string{"cluster": "cluster-beta", "namespace": "billing"}}
	cross := graph.NewEdge(graph.EdgeTypePodCallsPod, a.IDValue, b.IDValue, map[string]string{
		"cluster": "cluster-alpha",
	})
	return graph.View{Nodes: []graph.GraphNode{a, b}, Edges: []*graph.Edge{cross}}
}

// buildWithService snapshots the D29 connection-string service resolution:
// a pod-calls-pod edge whose target is a `type="service"` node (resolved from
// a `<service>.<namespace>.svc...` string, carrying cluster_ip on ipaddress),
// plus a `service-selects-pod` edge fanning out to a backing pod.
func buildWithService() graph.View {
	pod := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	svc := &graph.ServiceNode{IDValue: "cluster-alpha/shop/payments", NameValue: "payments", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}, IPAddressValue: []string{"10.0.0.5"}}
	pay0 := &graph.PodNode{IDValue: "cluster-alpha/pay0", NameValue: "payments-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	edges := []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePodCallsService, pod.IDValue, svc.IDValue, map[string]string{"cluster": "cluster-alpha"}),
		graph.NewEdge(graph.EdgeTypeServiceSelectsPod, svc.IDValue, pay0.IDValue, map[string]string{"namespace": "shop"}),
	}
	return graph.View{Nodes: []graph.GraphNode{pod, svc, pay0}, Edges: edges}
}

// buildFamilyFanout snapshots the D29 cluster-family fan-out: a client pod in
// prod-1 dials a `<service>.<namespace>.svc` connection string for a Service
// deployed in BOTH prod-1 and prod-2 (one family: digit runs normalise to
// "prod-0"). Each family cluster's service node materialises, the single
// upstream series yields one pod-calls-service edge per match — the prod-2
// edge is cross-cluster (source labels.cluster=prod-1, target
// labels.cluster=prod-2) — and each service fans out service-selects-pod only
// to its own cluster's backing pod. Both call edges carry
// labels.cluster=prod-1 (the client side is a pod).
func buildFamilyFanout() graph.View {
	pod := &graph.PodNode{IDValue: "prod-1/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "shop"}}
	svc1 := &graph.ServiceNode{IDValue: "prod-1/messaging/nats", NameValue: "nats", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "messaging"}, IPAddressValue: []string{"10.1.0.5"}}
	svc2 := &graph.ServiceNode{IDValue: "prod-2/messaging/nats", NameValue: "nats", LabelsValue: map[string]string{"cluster": "prod-2", "namespace": "messaging"}, IPAddressValue: []string{"10.2.0.5"}}
	nats1 := &graph.PodNode{IDValue: "prod-1/n1", NameValue: "nats-0", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "messaging"}}
	nats2 := &graph.PodNode{IDValue: "prod-2/n2", NameValue: "nats-0", LabelsValue: map[string]string{"cluster": "prod-2", "namespace": "messaging"}}
	edges := []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePodCallsService, pod.IDValue, svc1.IDValue, map[string]string{"cluster": "prod-1"}),
		graph.NewEdge(graph.EdgeTypePodCallsService, pod.IDValue, svc2.IDValue, map[string]string{"cluster": "prod-1"}),
		graph.NewEdge(graph.EdgeTypeServiceSelectsPod, svc1.IDValue, nats1.IDValue, map[string]string{"namespace": "messaging"}),
		graph.NewEdge(graph.EdgeTypeServiceSelectsPod, svc2.IDValue, nats2.IDValue, map[string]string{"namespace": "messaging"}),
	}
	return graph.View{Nodes: []graph.GraphNode{pod, svc1, svc2, nats1, nats2}, Edges: edges}
}

// buildWithStorageClass snapshots the StorageClass compound grouping: a PVC
// with a resolved StorageClass nests under a synthetic `type="storageclass"`
// group node (cluster > storageclass > pvc), while a PVC with no StorageClass
// falls back to its cluster group (cluster > pvc). The pod nests under its node
// (cluster > node > pod) and mounts both PVCs via pod-mounts-pvc edges. The
// StorageClass is reflected ONLY via the group node + data.parent — never as a
// PVC attribute or label.
func buildWithStorageClass() graph.View {
	pod := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "mongo-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "db", "node": "cluster-alpha/worker-0"}}
	node := &graph.K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	pvcGP3 := &graph.PVCNode{IDValue: "cluster-alpha/db/data-mongo-0", NameValue: "data-mongo-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "db", "volume": "data"}, StorageClassValue: "gp3"}
	pvcNone := &graph.PVCNode{IDValue: "cluster-alpha/db/legacy", NameValue: "legacy", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "db"}}
	edges := []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePodMountsPVC, pod.IDValue, pvcGP3.IDValue, map[string]string{"claim_name": "data-mongo-0"}),
		graph.NewEdge(graph.EdgeTypePodMountsPVC, pod.IDValue, pvcNone.IDValue, map[string]string{"claim_name": "legacy"}),
	}
	return graph.View{Nodes: []graph.GraphNode{node, pod, pvcGP3, pvcNone}, Edges: edges}
}

// buildMissingUIDFallback snapshots the D27 fallback shape: a service-graph
// series whose client_k8s_pod_uid is empty surfaces as `external/<label>`
// with an empty labels map (distinguishable from the pattern-matched
// external case, which carries labels.pattern). The edge omits
// labels.cluster because the client side is external.
func buildMissingUIDFallback() graph.View {
	pod := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	ext := &graph.ExternalNode{IDValue: "external/admin", NameValue: "admin", LabelsValue: map[string]string{}}
	edge := graph.NewEdge(graph.EdgeTypePodCallsPod, ext.IDValue, pod.IDValue, map[string]string{})
	return graph.View{Nodes: []graph.GraphNode{pod, ext}, Edges: []*graph.Edge{edge}}
}

// buildNameFilter snapshots the projection of a two-cluster graph through
// `?name=checkout`. The matching pod (cluster-alpha/p1) is the anchor; the
// cross-cluster partner pod (cluster-beta/p2) is re-added via the unified
// edge-endpoint partner rule on the pod-calls-pod edge. The host K8s nodes
// carry no edges (pod→node is compound nesting via labels.node only), so a
// name-filtered view does not pull them in.
func buildNameFilter() graph.View {
	a := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}}
	b := &graph.PodNode{IDValue: "cluster-beta/p2", NameValue: "payments", LabelsValue: map[string]string{"cluster": "cluster-beta", "namespace": "billing", "node": "cluster-beta/worker-0"}}
	nodeA := &graph.K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	nodeB := &graph.K8sNode{IDValue: "cluster-beta/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-beta"}}
	edges := []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePodCallsPod, a.IDValue, b.IDValue, map[string]string{"cluster": "cluster-alpha"}),
	}
	g := graph.NewGraph([]graph.GraphNode{a, b, nodeA, nodeB}, edges, time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC))
	return graph.Project(g, graph.Scope{Names: map[string]struct{}{"checkout": {}}})
}
