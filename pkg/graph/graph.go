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

	// ClusterIdentities maps each cluster IDENTITY present in the graph to the
	// three components it was composed from. A Kubernetes cluster is identified
	// by `<az>-<env>-<cluster>` — the raw `cluster` label alone is not unique
	// across zones and environments — so every cluster-scoped id prefix, every
	// `labels["cluster"]`, and every cluster-keyed index is keyed on the
	// identity (see pkg/build's cluster resolver, which owns the composition
	// rule; this package only stores what the reader decided).
	//
	// Set by the builder after NewGraph; nil for a graph built by hand or by an
	// embedder predating cluster identities, in which case ClusterRawName
	// degrades every value to itself and the request-scoped `cluster` filter
	// compares raw labels exactly as it did before.
	ClusterIdentities map[string]ClusterIdentity
}

// ClusterIdentity holds the three components of a composed cluster identity.
// Name is the RAW upstream `cluster` label value, which is what the
// request-scoped `?cluster=` filter matches on at both the query and the
// projection layer (the composed identity is a response-side value and is
// deliberately NOT a valid filter value).
type ClusterIdentity struct {
	AZ   string
	Env  string
	Name string
}

// ClusterRawName returns the raw `cluster` label component of a cluster
// identity, or the argument verbatim when it names no known identity (an
// unresolved raw name, or any value on a graph with no identity table).
//
// This is the one lookup the projection-level `cluster` filter uses, so that a
// request narrowing on the raw upstream value admits exactly the elements the
// upstream label matcher admitted.
func (g *Graph) ClusterRawName(id string) string {
	if g == nil {
		return id
	}
	if ci, ok := g.ClusterIdentities[id]; ok {
		return ci.Name
	}
	return id
}

// NewGraph builds a Graph from the supplied nodes + edges.
//
// It deliberately keeps NO adjacency index. The Forward / Reverse maps this
// type once carried existed for the withdrawn `?root=&depth=` traversal; every
// surviving consumer — projection, the connectivity prune, serialisation —
// scans Edges once, so an index would be two allocations and 2×|E| appends per
// request that nothing reads.
func NewGraph(nodes []GraphNode, edges []*Edge, builtAt time.Time) *Graph {
	g := &Graph{
		BuiltAt:   builtAt,
		NodesByID: make(map[string]GraphNode, len(nodes)),
		Edges:     edges,
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
