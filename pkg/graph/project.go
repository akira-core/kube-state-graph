package graph

// View is a read-only projection of a Graph after a Scope has been applied.
// It holds slices of pointers into the underlying Graph; callers MUST NOT
// mutate the returned slices' elements.
type View struct {
	Nodes []GraphNode
	Edges []*Edge
}

// Project returns a View of g constrained by scope. It does not mutate g.
//
// Order of operations:
//  1. If scope.Root is set, run a bounded BFS to determine the reachable
//     node set; otherwise consider all nodes.
//  2. Apply edge-type filter to edges among the reachable set.
//  3. Apply cluster / namespace / name filters to nodes.
//  4. Drop edges whose endpoints are no longer present.
func Project(g *Graph, scope Scope) View {
	if g == nil {
		return View{}
	}

	reachable := traverse(g, scope)
	nodes := filterNodes(g, scope, reachable)
	edges := filterEdges(g, scope, nodes, reachable)

	out := View{
		Nodes: make([]GraphNode, 0, len(nodes)),
		Edges: edges,
	}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, n)
	}
	SortNodes(out.Nodes)
	SortEdges(out.Edges)
	return out
}

func traverse(g *Graph, scope Scope) map[string]struct{} {
	if scope.Root == "" {
		return nil // sentinel: no traversal restriction
	}
	if _, ok := g.NodesByID[scope.Root]; !ok {
		return map[string]struct{}{} // empty: unknown root
	}

	// A Scope built directly (not via NewScope) may leave Direction unset; a
	// Root-anchored traversal then matched none of the branches below and
	// silently collapsed to the root alone. Default an empty Direction to
	// "both", matching the HTTP path's NewScope behaviour (D32 reusable engine).
	dir := scope.Direction
	if dir == "" {
		dir = DirectionBoth
	}

	visited := map[string]struct{}{scope.Root: {}}
	frontier := []string{scope.Root}
	for depth := 0; depth < scope.Depth && len(frontier) > 0; depth++ {
		next := make([]string, 0, len(frontier))
		for _, id := range frontier {
			if dir == DirectionOut || dir == DirectionBoth {
				for _, e := range g.Forward[id] {
					// Only cross in-scope edge types so a node reachable solely
					// via a filtered-out edge never enters the view as an orphan.
					if !scope.edgeTypeAllowed(e.Type) {
						continue
					}
					if _, seen := visited[e.Target]; !seen {
						visited[e.Target] = struct{}{}
						next = append(next, e.Target)
					}
				}
			}
			if dir == DirectionIn || dir == DirectionBoth {
				for _, e := range g.Reverse[id] {
					if !scope.edgeTypeAllowed(e.Type) {
						continue
					}
					if _, seen := visited[e.Source]; !seen {
						visited[e.Source] = struct{}{}
						next = append(next, e.Source)
					}
				}
			}
		}
		frontier = next
	}
	return visited
}

func filterNodes(g *Graph, scope Scope, reachable map[string]struct{}) map[string]GraphNode {
	out := make(map[string]GraphNode, len(g.NodesByID))
	// K8sNode and StorageClassNode admission is deferred: neither carries a
	// namespace label, so each is retained iff referenced by an in-scope element
	// — a K8s node hosts an in-scope pod (labels.node), a StorageClass backs an
	// in-scope PVC (its StorageClass()) — or it is explicitly matched by a name
	// filter. We first resolve every other node — recording the referenced ids —
	// then admit the deferred infra nodes per infraNodePassesFilters. The
	// reference sets are built for EVERY request shape (no filter, cluster,
	// namespace) so the response only ever carries infra nodes connected to
	// in-scope workload; an explicit ?name=<infra-node> is the one exception.
	// See design.md D6 (generalises the namespace-only rule to all requests).
	var deferred []GraphNode
	hostNodes := map[string]struct{}{}
	referencedSC := map[string]struct{}{}
	for id, n := range g.NodesByID {
		if reachable != nil {
			if _, ok := reachable[id]; !ok {
				continue
			}
		}
		switch n.Type() {
		case NodeTypeK8sNode, NodeTypeStorageClass:
			deferred = append(deferred, n)
			continue
		default:
			// pod / pvc / service / external are admitted directly below.
		}
		if !nodePassesFilters(n, scope) {
			continue
		}
		out[id] = n
		switch n.Type() {
		case NodeTypePod:
			if hn := n.Labels()["node"]; hn != "" {
				hostNodes[hn] = struct{}{}
			}
		case NodeTypePVC:
			if sc := n.StorageClass(); sc != "" {
				referencedSC[StorageClassID(n.Labels()["cluster"], sc)] = struct{}{}
			}
		default:
			// other in-scope types reference no deferred infra node.
		}
	}
	for _, n := range deferred {
		referenced := hostNodes
		if n.Type() == NodeTypeStorageClass {
			referenced = referencedSC
		}
		if !infraNodePassesFilters(n, scope, referenced) {
			continue
		}
		out[n.ID()] = n
	}
	return out
}

func nodePassesFilters(n GraphNode, scope Scope) bool {
	labels := n.Labels()
	if len(scope.Clusters) > 0 {
		if n.Type() == NodeTypeExternal {
			// External nodes have no cluster; exclude when caller scoped to clusters.
			return false
		}
		if _, ok := scope.Clusters[labels["cluster"]]; !ok {
			return false
		}
	}
	if len(scope.Namespaces) > 0 {
		// ExternalNode is cluster-unscoped (no namespace label) and only ever
		// enters a view as the re-added partner of a pod-calls-pod edge, so it
		// is exempt from the namespace match. K8sNode and StorageClassNode are
		// also namespace-less but are admitted separately by
		// infraNodePassesFilters (referenced-by-in-scope rule), so they never
		// reach this predicate. Every other node type must match the requested
		// namespace.
		switch n.Type() {
		case NodeTypeExternal:
			// pass-through
		default:
			if _, ok := scope.Namespaces[labels["namespace"]]; !ok {
				return false
			}
		}
	}
	if scope.NameFilterActive() {
		if _, ok := scope.Names[n.Name()]; !ok {
			return false
		}
	}
	return true
}

// infraNodePassesFilters decides whether a cluster-scoped infrastructure node
// (a K8sNode or a StorageClassNode) is admitted to a view. Neither carries a
// namespace label, so admission is reference-driven: the node is kept iff
// `referenced` contains its id — i.e. some in-scope element references it (a pod
// scheduled on a K8s node via labels.node, or a PVC backed by a StorageClass via
// its StorageClass()). This holds for EVERY request shape (no filter, cluster
// filter, namespace filter), so the response only ever carries infra nodes
// connected to in-scope workload — a node hosting no in-scope pod (and a
// StorageClass backing no in-scope PVC) is dropped, not surfaced as an orphan.
//
// The ONE exception is an explicit name filter: ?name=<infra-node> matches the
// node by Name() and admits it regardless of whether it is referenced (a node
// with zero pods, or a StorageClass with zero PVCs, is still directly
// queryable). A name filter that does not name this node drops it here; if it is
// instead the host of a named pod (or backs a named PVC), it re-enters the view
// as that edge's re-added partner in filterEdges, not via this predicate.
//
// The cluster filter applies first and exactly as for other node types (the
// node's own labels carry cluster). See design.md D6.
func infraNodePassesFilters(n GraphNode, scope Scope, referenced map[string]struct{}) bool {
	labels := n.Labels()
	if len(scope.Clusters) > 0 {
		if _, ok := scope.Clusters[labels["cluster"]]; !ok {
			return false
		}
	}
	// Explicit name match is the exception — a named infra node surfaces even
	// when referenced by no in-scope element.
	if scope.NameFilterActive() {
		_, named := scope.Names[n.Name()]
		return named
	}
	// Default (no name filter): admit iff referenced by an in-scope element.
	_, ok := referenced[n.ID()]
	return ok
}

func filterEdges(g *Graph, scope Scope, nodes map[string]GraphNode, reachable map[string]struct{}) []*Edge {
	out := make([]*Edge, 0, len(g.Edges))
	// Snapshot the in-scope set at entry. Re-adds during this pass MUST NOT
	// promote a re-added partner into a new in-scope anchor, otherwise name
	// or cluster anchors would cascade through the graph indefinitely.
	primary := make(map[string]struct{}, len(nodes))
	for id := range nodes {
		primary[id] = struct{}{}
	}
	for _, e := range g.Edges {
		if len(scope.EdgeTypes) > 0 {
			if _, ok := scope.EdgeTypes[e.Type]; !ok {
				continue
			}
		}
		_, srcOK := primary[e.Source]
		_, tgtOK := primary[e.Target]
		if srcOK && tgtOK {
			out = append(out, e)
			continue
		}
		if !srcOK && !tgtOK {
			continue
		}
		// Unified partner re-add: exactly one endpoint is in scope, re-add the
		// other from g.NodesByID provided it passes the non-cluster filters.
		// This single rule covers (a) cross-cluster pod-calls-pod partner
		// preservation, (b) non-pod endpoints incident on in-scope pods, and
		// (c) name-anchored views that need to render incident edges with
		// their partner endpoints. When traversal is active, the partner must
		// also lie within the reachable set so the depth bound is respected.
		if reachable != nil {
			missing := e.Target
			if !srcOK {
				missing = e.Source
			}
			if _, ok := reachable[missing]; !ok {
				continue
			}
		}
		if !readdEdgePartners(g, e, nodes, srcOK, tgtOK, scope, nodePassesNonClusterFilters) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// readdEdgePartners brings in the missing endpoint(s) of e via g.NodesByID,
// gated by pred (must accept the partner under scope). Returns false if any
// missing endpoint cannot be re-added.
func readdEdgePartners(
	g *Graph,
	e *Edge,
	nodes map[string]GraphNode,
	srcOK, tgtOK bool,
	scope Scope,
	pred func(GraphNode, Scope) bool,
) bool {
	if !srcOK {
		partner, ok := g.NodesByID[e.Source]
		if !ok || !pred(partner, scope) {
			return false
		}
		nodes[e.Source] = partner
	}
	if !tgtOK {
		partner, ok := g.NodesByID[e.Target]
		if !ok || !pred(partner, scope) {
			return false
		}
		nodes[e.Target] = partner
	}
	return true
}

func nodePassesNonClusterFilters(n GraphNode, scope Scope) bool {
	labels := n.Labels()
	if len(scope.Namespaces) > 0 {
		switch n.Type() {
		case NodeTypeExternal:
			// pass-through; external endpoints carry no namespace.
		default:
			if _, ok := scope.Namespaces[labels["namespace"]]; !ok {
				return false
			}
		}
	}
	return true
}
