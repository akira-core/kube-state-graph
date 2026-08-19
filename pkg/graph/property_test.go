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

func TestProperty_TraversalDepthRespected(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		g := genGraph(seed, 3, 5, 12)
		var root string
		for id := range g.NodesByID {
			root = id
			break
		}
		for d := range 4 {
			v := Project(g, Scope{Root: root, Depth: d, Direction: DirectionBoth})
			dist := map[string]int{root: 0}
			frontier := []string{root}
			for hop := 0; hop < d && len(frontier) > 0; hop++ {
				next := []string{}
				for _, id := range frontier {
					for _, e := range g.Forward[id] {
						if _, seen := dist[e.Target]; !seen {
							dist[e.Target] = hop + 1
							next = append(next, e.Target)
						}
					}
					for _, e := range g.Reverse[id] {
						if _, seen := dist[e.Source]; !seen {
							dist[e.Source] = hop + 1
							next = append(next, e.Source)
						}
					}
				}
				frontier = next
			}
			for _, n := range v.Nodes {
				if n.Type() == NodeTypeNetAppNode {
					// Compound parent of an admitted aggregate — pulled in
					// even when it has no edge (so BFS never reaches it).
					continue
				}
				dd, ok := dist[n.ID()]
				assert.Truef(t, ok, "seed=%d depth=%d: node %s in view but not reachable from root", seed, d, n.ID())
				assert.LessOrEqualf(t, dd, d, "seed=%d depth=%d: node %s at distance %d > depth", seed, d, n.ID(), dd)
			}
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

func TestProperty_NameFilterEveryNodeMatchesOrIsRehydratedPartner(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		g := genGraph(seed, 3, 5, 12)
		// Pick the first pod's name as the in-scope set. Random but deterministic.
		var anchor string
		for _, n := range g.NodesByID {
			if n.Type() == NodeTypePod {
				anchor = n.Name()
				break
			}
		}
		require.NotEmpty(t, anchor)
		v := Project(g, Scope{Names: map[string]struct{}{anchor: {}}})

		// Build the set of node IDs whose name matches the filter (the
		// "named-match" set) and the set of all returned node IDs.
		named := map[string]bool{}
		returned := map[string]bool{}
		for _, n := range v.Nodes {
			returned[n.ID()] = true
			if n.Name() == anchor {
				named[n.ID()] = true
			}
		}
		// Every returned node either matches the name OR is incident on at
		// least one retained edge whose other endpoint matches the name set.
		incident := map[string]bool{}
		for _, e := range v.Edges {
			if named[e.Source] {
				incident[e.Target] = true
			}
			if named[e.Target] {
				incident[e.Source] = true
			}
		}
		for id := range returned {
			if named[id] || incident[id] {
				continue
			}
			if n := g.NodesByID[id]; n != nil && n.Type() == NodeTypeNetAppNode {
				// Compound parent of an admitted aggregate.
				continue
			}
			assert.Failf(t, "unexpected node in name-filtered view",
				"seed=%d: node %s in result but neither matches name nor is incident on a retained edge to a named match", seed, id)
		}
	}
}

func TestProperty_ServiceSelectsPodEdgesWellFormed(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		g := genGraph(seed, 3, 5, 12)
		count := 0
		for _, e := range g.Edges {
			if e.Type != EdgeTypeServiceSelectsPod {
				continue
			}
			count++
			src, srcOK := g.NodesByID[e.Source]
			tgt, tgtOK := g.NodesByID[e.Target]
			require.Truef(t, srcOK, "seed=%d: service-selects-pod source %s unresolved", seed, e.Source)
			require.Truef(t, tgtOK, "seed=%d: service-selects-pod target %s unresolved", seed, e.Target)
			assert.Equalf(t, NodeTypeService, src.Type(), "seed=%d: source must be a service node", seed)
			assert.Equalf(t, NodeTypePod, tgt.Type(), "seed=%d: target must be a pod node", seed)
		}
		assert.Positivef(t, count, "seed=%d: expected at least one service-selects-pod edge", seed)
	}
}

func TestProperty_EmittedAggrImpliesOwningNode(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		g := genGraph(seed, 3, 5, 12)
		for _, scope := range []Scope{
			{},
			{Clusters: map[string]struct{}{"cluster-0": {}}},
			{Namespaces: map[string]struct{}{"ns-0": {}}},
		} {
			v := Project(g, scope)
			ids := map[string]bool{}
			for _, n := range v.Nodes {
				ids[n.ID()] = true
			}
			for _, n := range v.Nodes {
				if n.Type() != NodeTypeNetAppAggr {
					continue
				}
				oc, node := n.Labels()["ontap_cluster"], n.Labels()["node"]
				parent := NetAppNodeID(oc, node)
				assert.Truef(t, ids[parent],
					"seed=%d: aggregate %s emitted without owning node %s", seed, n.ID(), parent)
			}
		}
	}
}

func TestProperty_EdgeIDsUniquePerTuple(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		g := genGraph(seed, 3, 5, 12)
		ids := map[string]string{}
		for _, e := range g.Edges {
			tuple := string(e.Type) + "|" + e.Source + "|" + e.Target
			if existing, ok := ids[e.ID]; ok {
				require.Equalf(t, existing, tuple, "seed=%d: edge id %s shared by %q and %q", seed, e.ID, existing, tuple)
			}
			ids[e.ID] = tuple
		}
	}
}
