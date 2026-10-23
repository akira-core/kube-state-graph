package graph

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func k8sMount(pod, pvc string) *Edge { return NewEdge(EdgeTypePodMountsPVC, pod, pvc, nil) }
func k8sRun(pod, node string) *Edge  { return NewEdge(EdgeTypePodToNode, pod, node, nil) }

func k8sScope(clusters, namespaces []string, kind StorageRootKind, values ...string) StorageScope {
	s, err := NewStorageScope(clusters, namespaces, kind, values)
	if err != nil {
		panic(err)
	}
	return s
}

func sortedIDs(v View) []string {
	out := make([]string, 0, len(v.Nodes))
	for _, n := range v.Nodes {
		out = append(out, n.ID())
	}
	sort.Strings(out)
	return out
}

func sortedIDList(ids ...string) []string {
	sort.Strings(ids)
	return ids
}

// k8sEstate is cluster c1:
//
//	worker-1: web-0 (shop, mounts shop/data), api-0 (shop, mounts nothing),
//	          cron-0 (platform, mounts nothing), orders-0 (shop, mounts shop/shared)
//	worker-2: report-0 (shop, mounts shop/shared), db-0 (shop, mounts shop/db-data)
//	worker-9: no pod
//	pending-0 (shop): scheduled nowhere
//	shop/orphan: a claim no pod mounts
func k8sEstate() *Graph {
	return stGraph([]GraphNode{
		stNode("worker-1"), stNode("worker-2"), stNode("worker-9"),
		stPod("shop", "web-0", "uid-web", "worker-1"),
		stPod("shop", "api-0", "uid-api", "worker-1"),
		stPod("platform", "cron-0", "uid-cron", "worker-1"),
		stPod("shop", "orders-0", "uid-ord", "worker-1"),
		stPod("shop", "report-0", "uid-rep", "worker-2"),
		stPod("shop", "db-0", "uid-db", "worker-2"),
		stPod("shop", "pending-0", "uid-pend", ""),
		stPVC("shop", "data"), stPVC("shop", "shared"), stPVC("shop", "db-data"), stPVC("shop", "orphan"),
	}, []*Edge{
		k8sRun(PodID(stC, "uid-web"), K8sNodeID(stC, "worker-1")),
		k8sRun(PodID(stC, "uid-api"), K8sNodeID(stC, "worker-1")),
		k8sRun(PodID(stC, "uid-cron"), K8sNodeID(stC, "worker-1")),
		k8sRun(PodID(stC, "uid-ord"), K8sNodeID(stC, "worker-1")),
		k8sRun(PodID(stC, "uid-rep"), K8sNodeID(stC, "worker-2")),
		k8sRun(PodID(stC, "uid-db"), K8sNodeID(stC, "worker-2")),
		k8sMount(PodID(stC, "uid-web"), PVCID(stC, "shop", "data")),
		k8sMount(PodID(stC, "uid-ord"), PVCID(stC, "shop", "shared")),
		k8sMount(PodID(stC, "uid-rep"), PVCID(stC, "shop", "shared")),
		k8sMount(PodID(stC, "uid-db"), PVCID(stC, "shop", "db-data")),
	})
}

func TestProjectK8s(t *testing.T) {
	pod := func(uid string) string { return PodID(stC, uid) }
	node := func(n string) string { return K8sNodeID(stC, n) }
	pvc := func(ns, c string) string { return PVCID(stC, ns, c) }

	cases := []struct {
		name  string
		scope StorageScope
		ids   []string
		edges int
	}{
		{"pod root with a claim", k8sScope(nil, nil, StorageRootPod, "shop/web-0"),
			sortedIDList(pod("uid-web"), node("worker-1"), pvc("shop", "data")), 2},
		{"node root draws every pod on it", k8sScope(nil, nil, StorageRootNode, "worker-1"),
			sortedIDList(node("worker-1"), pod("uid-web"), pod("uid-api"), pod("uid-cron"), pod("uid-ord"),
				pvc("shop", "data"), pvc("shop", "shared")), 6},
		{"shared claim does not pull in its other mounter", k8sScope(nil, nil, StorageRootPod, "shop/orders-0"),
			sortedIDList(pod("uid-ord"), node("worker-1"), pvc("shop", "shared")), 2},
		{"two pod roots on two nodes", k8sScope(nil, nil, StorageRootPod, "shop/web-0", "shop/db-0"),
			sortedIDList(pod("uid-web"), pod("uid-db"), node("worker-1"), node("worker-2"),
				pvc("shop", "data"), pvc("shop", "db-data")), 4},
		{"podless node root", k8sScope(nil, nil, StorageRootNode, "worker-9"),
			sortedIDList(node("worker-9")), 0},
		{"unscheduled pod root", k8sScope(nil, nil, StorageRootPod, "shop/pending-0"),
			sortedIDList(pod("uid-pend")), 0},
		{"unknown pod root", k8sScope(nil, nil, StorageRootPod, "shop/nope"), nil, 0},
		{"unknown node root", k8sScope(nil, nil, StorageRootNode, "nope"), nil, 0},
		{"namespace filter on a node root", k8sScope(nil, []string{"shop"}, StorageRootNode, "worker-1"),
			sortedIDList(node("worker-1"), pod("uid-web"), pod("uid-api"), pod("uid-ord"),
				pvc("shop", "data"), pvc("shop", "shared")), 5},
		{"namespace filter drops a pod root", k8sScope(nil, []string{"platform"}, StorageRootPod, "shop/web-0"), nil, 0},
		{"cluster filter keeps its cluster", k8sScope([]string{stC}, nil, StorageRootPod, "shop/web-0"),
			sortedIDList(pod("uid-web"), node("worker-1"), pvc("shop", "data")), 2},
		{"cluster filter excludes another", k8sScope([]string{"other"}, nil, StorageRootPod, "shop/web-0"), nil, 0},
		{"a storage-side kind resolves nothing", k8sScope(nil, nil, StorageRootAggr, "aggr1"), nil, 0},
		{"a claim kind resolves nothing", k8sScope(nil, nil, StorageRootPVC, "shop/data"), nil, 0},
		{"no root", StorageScope{}, nil, 0},
	}
	g := k8sEstate()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ProjectK8s(g, tc.scope)
			assert.ElementsMatch(t, tc.ids, sortedIDs(v))
			assert.Len(t, v.Edges, tc.edges)
			for _, e := range v.Edges {
				ids := viewIDs(v)
				assert.True(t, ids[e.Source] && ids[e.Target], "edge endpoints are drawn")
			}
		})
	}
}

// A node name reused by two clusters is two nodes: a root names both, a cluster
// filter picks one, and a pod is kept only through the node of its own cluster.
func TestProjectK8s_ClusterFilterAcrossTwoIdentities(t *testing.T) {
	mk := func(cluster string) ([]GraphNode, []*Edge) {
		n := &K8sNode{IDValue: K8sNodeID(cluster, "worker-1"), NameValue: "worker-1", LabelsValue: map[string]string{"cluster": cluster}}
		p := &PodNode{IDValue: PodID(cluster, "uid"), NameValue: "web-0", LabelsValue: map[string]string{"cluster": cluster, "namespace": "shop"}}
		return []GraphNode{n, p}, []*Edge{k8sRun(p.ID(), n.ID())}
	}
	n1, e1 := mk("a-prod-c1")
	n2, e2 := mk("b-prod-c1")
	g := stGraph(append(n1, n2...), append(e1, e2...))
	g.ClusterIdentities = map[string]ClusterIdentity{
		"a-prod-c1": {Name: "c1"}, "b-prod-c1": {Name: "c1"},
	}

	both := ProjectK8s(g, k8sScope([]string{"c1"}, nil, StorageRootNode, "worker-1"))
	assert.Len(t, both.Nodes, 4, "the raw name c1 selects both identities")
	assert.Len(t, both.Edges, 2)

	none := ProjectK8s(g, k8sScope([]string{"a-prod-c1"}, nil, StorageRootNode, "worker-1"))
	assert.Empty(t, none.Nodes, "an identity sent as the cluster selects nothing")
}

func TestProjectK8s_NilGraphAndDeterminism(t *testing.T) {
	assert.Equal(t, View{}, ProjectK8s(nil, k8sScope(nil, nil, StorageRootNode, "worker-1")))
	g := k8sEstate()
	scope := k8sScope(nil, nil, StorageRootNode, "worker-1", "worker-2")
	first := ProjectK8s(g, scope)
	for range 5 {
		again := ProjectK8s(g, scope)
		require.Equal(t, sortedIDs(first), sortedIDs(again))
		require.Len(t, again.Edges, len(first.Edges))
	}
	resorted := append([]GraphNode(nil), first.Nodes...)
	SortNodes(resorted)
	assert.Equal(t, resorted, first.Nodes, "nodes leave the projection in the shared order")
	reEdges := append([]*Edge(nil), first.Edges...)
	SortEdges(reEdges)
	assert.Equal(t, reEdges, first.Edges, "edges leave the projection in the shared order")
}
