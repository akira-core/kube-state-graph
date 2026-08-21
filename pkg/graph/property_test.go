package graph

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genGraph generates a deterministic random graph from rand.New(seed) for
// property-based testing.
func genGraph(seed int64, clusters, podsPerCluster, extraEdges int) *Graph {
	r := rand.New(rand.NewSource(seed))
	all := make([]GraphNode, 0, clusters*(1+podsPerCluster))
	clusterNames := make([]string, clusters)
	for i := range clusters {
		clusterNames[i] = fmt.Sprintf("cluster-%d", i)
		nodeID := K8sNodeID(clusterNames[i], "worker-0")
		all = append(all, &K8sNode{IDValue: nodeID, NameValue: "worker-0", LabelsValue: map[string]string{"cluster": clusterNames[i]}})
		for j := range podsPerCluster {
			id := PodID(clusterNames[i], fmt.Sprintf("uid-%d-%d", i, j))
			all = append(all, &PodNode{
				IDValue:   id,
				NameValue: fmt.Sprintf("pod-%d-%d", i, j),
				LabelsValue: map[string]string{
					"cluster":   clusterNames[i],
					"namespace": fmt.Sprintf("ns-%d", j%2),
					"node":      nodeID,
				},
			})
		}
	}

	// Shared NetApp filer: one aggregate + controller referenced by every
	// cluster's first pod via a PVC, so the default projection retains them
	// whenever that pod is connectivity-connected.
	aggrID := NetAppAggrID("ontap-prod", "aggr1")
	ctrlID := NetAppNodeID("ontap-prod", "n1")
	all = append(all,
		&NetAppAggrNode{IDValue: aggrID, NameValue: "aggr1", LabelsValue: map[string]string{"ontap_cluster": "ontap-prod", "node": "n1"}},
		&NetAppNode{IDValue: ctrlID, NameValue: "n1", LabelsValue: map[string]string{"ontap_cluster": "ontap-prod"}},
	)

	// One Service per cluster (D29). Added before the edge loop so podsOnly()
	// still sees only pods.
	for i := range clusters {
		all = append(all, &ServiceNode{
			IDValue:     ServiceID(clusterNames[i], "ns-0", "svc"),
			NameValue:   "svc",
			LabelsValue: map[string]string{"cluster": clusterNames[i], "namespace": "ns-0"},
		})
	}

	edges := []*Edge{}
	pods := podsOnly(all)
	for i := range clusters {
		for _, p := range pods {
			if p.Labels()["cluster"] != clusterNames[i] {
				continue
			}
			pvcID := PVCID(clusterNames[i], "ns-0", "data")
			all = append(all, &PVCNode{
				IDValue: pvcID, NameValue: "data",
				LabelsValue: map[string]string{"cluster": clusterNames[i], "namespace": "ns-0"},
			})
			edges = append(edges, NewEdge(EdgeTypePodMountsPVC, p.ID(), pvcID, nil))
			edges = append(edges, NewEdge(EdgeTypePVCToNetAppAggr, pvcID, aggrID, nil))
			break
		}
	}
	// service-selects-pod edge from each cluster's Service to a backing pod.
	for i := range clusters {
		svcID := ServiceID(clusterNames[i], "ns-0", "svc")
		for _, p := range pods {
			if p.Labels()["cluster"] == clusterNames[i] {
				edges = append(edges, NewEdge(EdgeTypeServiceSelectsPod, svcID, p.ID(), map[string]string{"namespace": "ns-0"}))
				break
			}
		}
	}
	for i := 0; i < extraEdges && len(pods) >= 2; i++ {
		a := pods[r.Intn(len(pods))]
		b := pods[r.Intn(len(pods))]
		if a.ID() == b.ID() {
			continue
		}
		edges = append(edges, NewEdge(EdgeTypePodCallsPod, a.ID(), b.ID(), map[string]string{
			"cluster": a.Labels()["cluster"],
		}))
	}
	return NewGraph(all, edges, time.Now())
}

func podsOnly(nodes []GraphNode) []*PodNode {
	out := []*PodNode{}
	for _, n := range nodes {
		if p, ok := n.(*PodNode); ok {
			out = append(out, p)
		}
	}
	return out
}

func TestProperty_EveryEdgeEndpointResolves(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		g := genGraph(seed, 3, 5, 12)
		for _, e := range g.Edges {
			_, srcOK := g.NodesByID[e.Source]
			_, tgtOK := g.NodesByID[e.Target]
			require.Truef(t, srcOK, "seed=%d: edge %s has unresolved source %s", seed, e.ID, e.Source)
			require.Truef(t, tgtOK, "seed=%d: edge %s has unresolved target %s", seed, e.ID, e.Target)
		}
	}
}

func TestProperty_FilteredSubsetUnfiltered(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		g := genGraph(seed, 3, 5, 12)
		full := Project(g, Scope{})
		filtered := Project(g, Scope{Clusters: map[string]struct{}{"cluster-0": {}}})
		fullIDs := map[string]bool{}
		for _, n := range full.Nodes {
			fullIDs[n.ID()] = true
		}
		for _, n := range filtered.Nodes {
			require.Truef(t, fullIDs[n.ID()], "seed=%d: filtered contains node %s not in unfiltered", seed, n.ID())
		}
	}
}

func TestProperty_CrossClusterEdgesHaveDistinctClusterEndpoints(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		g := genGraph(seed, 3, 5, 12)
		for _, e := range g.Edges {
			if e.Type != EdgeTypePodCallsPod {
				continue
			}
			// Cross-cluster status is derived from the resolved endpoint
			// nodes' `cluster` labels (the edge itself only carries the
			// trace-source / client-side cluster).
			src, srcOK := g.NodesByID[e.Source]
			tgt, tgtOK := g.NodesByID[e.Target]
			require.True(t, srcOK)
			require.True(t, tgtOK)
			srcCluster := src.Labels()["cluster"]
			tgtCluster := tgt.Labels()["cluster"]
			if srcCluster != tgtCluster {
				assert.NotEmpty(t, srcCluster)
				assert.NotEmpty(t, tgtCluster)
				assert.Equal(t, srcCluster, e.Labels["cluster"], "edge cluster label = source-side cluster")
			}
		}
	}
}

// TestProperty_PrunedViewSubsetOfInventory: the default (pruned) view can only
// ever be a subset of the same request with prune=false — turning the prune off
// adds elements, never removes or rewrites them.
func TestProperty_PrunedViewSubsetOfInventory(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		g := genGraph(seed, 3, 5, 12)
		pruned := Project(g, Scope{})
		inventory := Project(g, Scope{Inventory: true})

		invNodes := map[string]bool{}
		for _, n := range inventory.Nodes {
			invNodes[n.ID()] = true
		}
		for _, n := range pruned.Nodes {
			require.Truef(t, invNodes[n.ID()], "seed=%d: pruned view has node %s absent from the inventory view", seed, n.ID())
		}
		invEdges := map[string]bool{}
		for _, e := range inventory.Edges {
			invEdges[e.ID] = true
		}
		for _, e := range pruned.Edges {
			require.Truef(t, invEdges[e.ID], "seed=%d: pruned view has edge %s absent from the inventory view", seed, e.ID)
		}
	}
}
