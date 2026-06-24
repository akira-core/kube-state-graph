package graph

import (
	"log/slog"
	"sort"
	"time"
)

// Graph is the immutable, in-memory multi-cluster graph for one time window.
// Once constructed, all callers MUST treat it as read-only — projection
// returns views over it without mutation.
type Graph struct {
	BuiltAt time.Time

	NodesByID map[string]GraphNode
	Edges     []*Edge

	// Forward[id] = edges where Source == id.
	// Reverse[id] = edges where Target == id.
	Forward map[string][]*Edge
	Reverse map[string][]*Edge
}

// NewGraph builds a Graph from the supplied nodes + edges and pre-computes
// forward / reverse adjacency maps.
func NewGraph(nodes []GraphNode, edges []*Edge, builtAt time.Time) *Graph {
	g := &Graph{
		BuiltAt:   builtAt,
		NodesByID: make(map[string]GraphNode, len(nodes)),
		Edges:     edges,
		Forward:   make(map[string][]*Edge, len(nodes)),
		Reverse:   make(map[string][]*Edge, len(nodes)),
	}
	for _, n := range nodes {
		// ServiceID mirrors PVCID keying, so a Service and a PVC sharing
		// (cluster, namespace, name) mint byte-identical IDs. Changing the ID
		// grammar is a v2 wire-format break, so dedupe here instead. Known
		// consequence: edges minted against the dropped node's ID (e.g.
		// pod-calls-service edges whose service node lost to a same-ID PVC)
		// resolve to the surviving node's type, violating the catalogue's
		// source/target-type contract for that edge. The D29 cluster-family
		// fan-out widens the collision window from the trace-source cluster
		// to every family cluster — tracked as a v2 ID-grammar fix.
		// Deterministic dedupe: the FIRST node wins — the input slice order is
		// deterministic (build's assemble appends authoritative topology nodes
		// before on-demand service-graph nodes), so the winner is a pure
		// function of the input, independent of map-iteration order (D6
		// determinism). NodesByID is the sole node collection — projection and
		// serialisation derive their node sets from it — so keep-first here
		// guarantees the dropped node can never be emitted.
		if existing, dup := g.NodesByID[n.ID()]; dup {
			slog.Warn("duplicate node ID; keeping first",
				"id", n.ID(),
				"kept_type", string(existing.Type()),
				"dropped_type", string(n.Type()),
			)
			continue
		}
		g.NodesByID[n.ID()] = n
	}
	for _, e := range edges {
		g.Forward[e.Source] = append(g.Forward[e.Source], e)
		g.Reverse[e.Target] = append(g.Reverse[e.Target], e)
	}
	return g
}

// ClusterNames returns the sorted unique set of cluster values present on any
// pod / node / PVC node. External nodes are excluded.
func (g *Graph) ClusterNames() []string {
	seen := map[string]struct{}{}
	for _, n := range g.NodesByID {
		if n.Type() == NodeTypeExternal {
			continue
		}
		if c := n.Labels()["cluster"]; c != "" {
			seen[c] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// NodeCountByKind returns counts grouped by `cluster` (empty for externals)
// and node `kind` for self-metric exposition.
func (g *Graph) NodeCountByKind() map[[2]string]int {
	out := map[[2]string]int{}
	for _, n := range g.NodesByID {
		cluster := n.Labels()["cluster"]
		out[[2]string{cluster, string(n.Type())}]++
	}
	return out
}

// neverCrossCluster is derived from the EdgeTypes registry at init: edge
// types declared MayCrossCluster=false are intra-cluster by construction, so
// the per-edge node lookups in isCrossCluster can be skipped for them.
// Registry-driven (not type literals) so a future may-cross type can never be
// silently mis-bucketed by a stale gate; unregistered types are NOT in the
// set and are still evaluated.
var neverCrossCluster = func() map[EdgeType]struct{} {
	out := make(map[EdgeType]struct{}, len(EdgeTypes))
	for _, def := range EdgeTypes {
		if !def.MayCrossCluster {
			out[def.Type] = struct{}{}
		}
	}
	return out
}()

// EdgeCountByType returns counts grouped by edge type and a "true"|"false"
// cross-cluster bucket. Cross-cluster status is derived by comparing the
// resolved source-node and target-node `cluster` labels (the edge itself only
// carries the trace-source / client-side cluster) — this covers both
// pod-calls-pod (server pod recovered via the UID index) and pod-calls-service
// (service resolved via the D29 cluster-family fan-out). Types the registry
// declares always-intra-cluster bucket as "false" without the per-edge node
// lookups; edges whose endpoints are missing or external are bucketed as
// "false".
func (g *Graph) EdgeCountByType() map[[2]string]int {
	out := map[[2]string]int{}
	for _, e := range g.Edges {
		cross := "false"
		if _, never := neverCrossCluster[e.Type]; !never && g.isCrossCluster(e) {
			cross = "true"
		}
		out[[2]string{string(e.Type), cross}]++
	}
	return out
}

// isCrossCluster returns true when both endpoints of the edge are non-external
// nodes that resolve in g.NodesByID and whose `cluster` labels are non-empty
// and differ. External endpoints, missing nodes, or empty cluster labels all
// count as not-cross-cluster (we cannot prove the cluster boundary in those
// cases).
func (g *Graph) isCrossCluster(e *Edge) bool {
	src, srcOK := g.NodesByID[e.Source]
	tgt, tgtOK := g.NodesByID[e.Target]
	if !srcOK || !tgtOK {
		return false
	}
	if src.Type() == NodeTypeExternal || tgt.Type() == NodeTypeExternal {
		return false
	}
	srcCluster := src.Labels()["cluster"]
	tgtCluster := tgt.Labels()["cluster"]
	if srcCluster == "" || tgtCluster == "" {
		return false
	}
	return srcCluster != tgtCluster
}
