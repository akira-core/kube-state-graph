package graph

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genK8sEstate is a random Kubernetes estate over two cluster identities that
// share a raw name: three nodes and up to eight pods per cluster, pods
// scheduled or not, each mounting zero to two of the cluster's claims.
func genK8sEstate(r *rand.Rand) ([]GraphNode, []*Edge) {
	nodes := make([]GraphNode, 0, 48)
	var edges []*Edge
	for _, cluster := range []string{"a-prod-c1", "b-prod-c1"} {
		nodeIDs, claimIDs := make([]string, 0, 3), make([]string, 0, 4)
		for i := range 3 {
			n := &K8sNode{IDValue: K8sNodeID(cluster, fmt.Sprintf("worker-%d", i)), NameValue: fmt.Sprintf("worker-%d", i),
				LabelsValue: map[string]string{"cluster": cluster}}
			nodes = append(nodes, n)
			nodeIDs = append(nodeIDs, n.ID())
		}
		for i := range 4 {
			ns := fmt.Sprintf("ns-%d", i%2)
			c := &PVCNode{IDValue: PVCID(cluster, ns, fmt.Sprintf("claim-%d", i)), NameValue: fmt.Sprintf("claim-%d", i),
				LabelsValue: map[string]string{"cluster": cluster, "namespace": ns}}
			nodes = append(nodes, c)
			claimIDs = append(claimIDs, c.ID())
		}
		for i := range 1 + r.Intn(8) {
			ns := fmt.Sprintf("ns-%d", r.Intn(2))
			p := &PodNode{IDValue: PodID(cluster, fmt.Sprintf("uid-%d", i)), NameValue: fmt.Sprintf("pod-%d", i),
				LabelsValue: map[string]string{"cluster": cluster, "namespace": ns}}
			nodes = append(nodes, p)
			if r.Intn(5) > 0 {
				edges = append(edges, k8sRun(p.ID(), nodeIDs[r.Intn(len(nodeIDs))]))
			}
			for range r.Intn(3) {
				c := claimIDs[r.Intn(len(claimIDs))]
				// A binding is same-namespace by construction in the real build.
				if cn, ok := lookup(nodes, c); ok && cn.Labels()["namespace"] == ns {
					edges = append(edges, k8sMount(p.ID(), c))
				}
			}
		}
	}
	return nodes, edges
}

func lookup(nodes []GraphNode, id string) (GraphNode, bool) {
	for _, n := range nodes {
		if n.ID() == id {
			return n, true
		}
	}
	return nil, false
}

func genK8sScope(r *rand.Rand) StorageScope {
	var clusters, namespaces []string
	if r.Intn(3) == 0 {
		clusters = []string{"c1"}
	}
	if r.Intn(3) == 0 {
		namespaces = []string{fmt.Sprintf("ns-%d", r.Intn(2))}
	}
	if r.Intn(2) == 0 {
		vals := make([]string, 1+r.Intn(2))
		for i := range vals {
			vals[i] = fmt.Sprintf("worker-%d", r.Intn(4))
		}
		return k8sScope(clusters, namespaces, StorageRootNode, vals...)
	}
	vals := make([]string, 1+r.Intn(3))
	for i := range vals {
		vals[i] = fmt.Sprintf("ns-%d/pod-%d", r.Intn(2), r.Intn(8))
	}
	return k8sScope(clusters, namespaces, StorageRootPod, vals...)
}

func shuffled[T any](r *rand.Rand, in []T) []T {
	out := append([]T(nil), in...)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestProperty_K8sProjection: random estates × random roots and filters. Every
// kept edge's endpoints are kept, every kept pod is a root or runs on a root
// node, every kept claim or node hangs off a kept pod (or is a root node), and
// the view is a pure function of (g, scope) — shuffling the build inputs does
// not change it.
func TestProperty_K8sProjection(t *testing.T) {
	for seed := int64(1); seed <= 1000; seed++ {
		r := rand.New(rand.NewSource(seed))
		nodes, edges := genK8sEstate(r)
		g := stGraph(nodes, edges)
		g.ClusterIdentities = map[string]ClusterIdentity{
			"a-prod-c1": {Name: "c1"}, "b-prod-c1": {Name: "c1"},
		}
		scope := genK8sScope(r)
		v := ProjectK8s(g, scope)
		kept := viewIDs(v)

		nodeRoot, podRoot := resolveK8sRoots(g, scope)
		runsOn := map[string]string{}
		for _, e := range g.Edges {
			if e.Type == EdgeTypePodToNode {
				runsOn[e.Source] = e.Target
			}
		}
		mounted := map[string]bool{}
		hosted := map[string]bool{}
		for _, e := range v.Edges {
			require.Containsf(t, []EdgeType{EdgeTypePodToNode, EdgeTypePodMountsPVC}, e.Type, "seed=%d", seed)
			require.Truef(t, kept[e.Source] && kept[e.Target], "seed=%d: edge %s has an endpoint outside the view", seed, e.ID)
			require.Equalf(t, NodeTypePod, g.NodesByID[e.Source].Type(), "seed=%d", seed)
			if e.Type == EdgeTypePodMountsPVC {
				mounted[e.Target] = true
			} else {
				hosted[e.Target] = true
			}
		}
		for _, n := range v.Nodes {
			id := n.ID()
			switch n.Type() {
			case NodeTypePod:
				_, isRoot := podRoot[id]
				_, onRoot := nodeRoot[runsOn[id]]
				assert.Truef(t, isRoot || onRoot, "seed=%d: pod %s is neither a root nor on a root node", seed, id)
			case NodeTypePVC:
				assert.Truef(t, mounted[id], "seed=%d: claim %s is not mounted by a kept pod", seed, id)
			case NodeTypeK8sNode:
				_, isRoot := nodeRoot[id]
				assert.Truef(t, isRoot || hosted[id], "seed=%d: node %s is neither a root nor hosts a kept pod", seed, id)
			default:
				t.Fatalf("seed=%d: unexpected node type %s", seed, n.Type())
			}
		}
		for id := range nodeRoot {
			assert.Truef(t, kept[id], "seed=%d: node root %s is always drawn", seed, id)
		}
		for id := range podRoot {
			assert.Truef(t, kept[id], "seed=%d: pod root %s is always drawn", seed, id)
		}

		g2 := stGraph(shuffled(r, nodes), shuffled(r, edges))
		g2.ClusterIdentities = g.ClusterIdentities
		v2 := ProjectK8s(g2, scope)
		ids := func(v View) []string {
			out := make([]string, 0, len(v.Edges))
			for _, e := range v.Edges {
				out = append(out, e.ID)
			}
			sort.Strings(out)
			return out
		}
		require.Equalf(t, sortedIDs(v), sortedIDs(v2), "seed=%d: input order changed the nodes", seed)
		require.Equalf(t, ids(v), ids(v2), "seed=%d: input order changed the edges", seed)
	}
}
