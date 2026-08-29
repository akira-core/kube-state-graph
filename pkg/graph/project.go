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
//  1. Unless scope.Inventory is set, compute the connectivity prune set.
//  2. Apply cluster / namespace filters to nodes; admit infrastructure nodes
//     by reference (or unconditionally under Inventory).
//  3. Apply the edge-type filter and drop edges whose endpoints are absent,
//     re-adding a single missing partner where the filters allow it.
func Project(g *Graph, scope Scope) View {
	if g == nil {
		return View{}
	}

	// Default-projection connectivity prune: every response carries only pods
	// that sit on a connectivity edge (pod-calls-pod / pod-calls-service /
	// service-selects-pod) and the infra that hangs off them — an edgeless pod,
	// the node hosting only edgeless pods, a PVC mounted only by edgeless pods,
	// and the NetApp aggregate (then controller) serving only such PVCs are
	// dropped. The exclusion set is a pure function of the graph
	// (scope-independent), so it is computed once and consulted in both
	// filterNodes (skip) and filterEdges (no partner re-add).
	//
	// `prune=false` (scope.Inventory) is the single escape hatch that surfaces
	// connectivity-disconnected elements; the former ?name= / ?root= hatches
	// are withdrawn with those parameters.
	var excluded map[string]struct{}
	if !scope.Inventory {
		excluded = connectivityExcluded(g)
	}

	nodes := filterNodes(g, scope, excluded)
	edges := filterEdges(g, scope, nodes, excluded)
	// The single parent sweep, deliberately after filterEdges: an aggregate
	// admitted as an edge partner (via a pvc-to-netapp-aggr edge) still needs
	// its owning controller, because the compound parent must exist. No edge
	// type has a netapp-node endpoint, so running it here rather than inside
	// filterNodes cannot change which edges survive.
	pullNetAppParents(g, nodes)

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

// connectivityExcluded returns the set of pod and PVC node IDs that the
// default projection drops because they sit on no connectivity edge:
//   - a pod is excluded iff it is not an endpoint of any pod-calls-pod /
//     pod-calls-service / service-selects-pod edge;
//   - a PVC is excluded iff none of the pods that mount it (pod-mounts-pvc) is
//     itself connectivity-connected.
//
// It is a pure function of g (independent of any Scope), so the result is stable
// across requests and reusable by a future cache. K8s nodes and the NetApp types
// are NOT listed here — they are already reference-gated by
// infraNodePassesFilters / netappInfraPassesFilters (a node hosting only excluded
// pods, or an aggregate serving only excluded PVCs — and then its controller —
// falls out for free once those pods/PVCs are gone).
func connectivityExcluded(g *Graph) map[string]struct{} {
	connectedPods := make(map[string]struct{})
	pvcMounters := make(map[string][]string)
	for _, e := range g.Edges {
		switch e.Type {
		case EdgeTypePodCallsPod, EdgeTypePodCallsService, EdgeTypeServiceSelectsPod:
			// Endpoints of a connectivity edge are connected. A non-pod endpoint
			// (a service) may land in the set too — harmless, since the set is
			// only ever queried for pod and pod-mounter IDs.
			connectedPods[e.Source] = struct{}{}
			connectedPods[e.Target] = struct{}{}
		case EdgeTypePodMountsPVC:
			pvcMounters[e.Target] = append(pvcMounters[e.Target], e.Source)
		case EdgeTypePodToNode, EdgeTypePVCToNetAppAggr:
			// Infra edges (pod→node, pvc→netapp-aggr) are NOT connectivity edges:
			// they never make a pod or PVC connectivity-connected. Their endpoints
			// (K8s nodes / NetApp aggregates) are reference-gated by
			// infraNodePassesFilters, not by this set. Listed explicitly to keep
			// the switch exhaustive over graph.EdgeType.
		}
	}

	excluded := make(map[string]struct{})
	for id, n := range g.NodesByID {
		switch n.Type() {
		case NodeTypePod:
			if _, ok := connectedPods[id]; !ok {
				excluded[id] = struct{}{}
			}
		case NodeTypePVC:
			kept := false
			for _, podID := range pvcMounters[id] {
				if _, ok := connectedPods[podID]; ok {
					kept = true
					break
				}
			}
			if !kept {
				excluded[id] = struct{}{}
			}
		default:
			// node / netapp-aggr / netapp-node / service / external are not
			// connectivity-pruned (NetApp types are reference-gated).
		}
	}
	return excluded
}

func filterNodes(g *Graph, scope Scope, excluded map[string]struct{}) map[string]GraphNode {
	out := make(map[string]GraphNode, len(g.NodesByID))
	// The projection-level `cluster` filter compares the RAW upstream cluster
	// name, not the composed identity a node's labels carry, so that it admits
	// exactly what the upstream label matcher admitted (?cluster=c1 selects
	// every zone's c1; ?az=&env=&cluster= pins one). Resolved through the
	// graph's identity table, which degrades to identity when absent.
	rawName := g.ClusterRawName
	// Infra admission is deferred: K8s nodes, NetApp aggregates, and NetApp
	// controllers carry no namespace (NetApp types also carry no cluster), so
	// each is retained iff referenced by an in-scope element (or admitted
	// unconditionally under Inventory). Wave 1: netapp-aggr iff an admitted PVC has a
	// pvc-to-netapp-aggr edge to it. Wave 2: netapp-node iff an admitted
	// aggregate names it in labels.node. See design.md D6.
	var deferredK8s, deferredAggr, deferredNetAppNode []GraphNode
	hostNodes := map[string]struct{}{}
	for id, n := range g.NodesByID {
		if excluded != nil {
			if _, ex := excluded[id]; ex {
				continue
			}
		}
		switch n.Type() {
		case NodeTypeK8sNode:
			deferredK8s = append(deferredK8s, n)
			continue
		case NodeTypeNetAppAggr:
			deferredAggr = append(deferredAggr, n)
			continue
		case NodeTypeNetAppNode:
			deferredNetAppNode = append(deferredNetAppNode, n)
			continue
		default:
			// pod / pvc / service / external are admitted directly below.
		}
		if !nodePassesFilters(n, scope, rawName) {
			continue
		}
		out[id] = n
		if n.Type() == NodeTypePod {
			if hn := n.Labels()["node"]; hn != "" {
				hostNodes[hn] = struct{}{}
			}
		}
	}

	referencedAggr := map[string]struct{}{}
	for _, e := range g.Edges {
		if e.Type != EdgeTypePVCToNetAppAggr {
			continue
		}
		if _, ok := out[e.Source]; ok {
			referencedAggr[e.Target] = struct{}{}
		}
	}

	for _, n := range deferredK8s {
		if !infraNodePassesFilters(n, scope, hostNodes, rawName) {
			continue
		}
		out[n.ID()] = n
	}
	for _, n := range deferredAggr {
		if !netappInfraPassesFilters(n, scope, referencedAggr) {
			continue
		}
		out[n.ID()] = n
	}
	referencedCtrl := map[string]struct{}{}
	for _, n := range out {
		if n.Type() != NodeTypeNetAppAggr {
			continue
		}
		oc, node := n.Labels()["ontap_cluster"], n.Labels()["node"]
		if oc != "" && node != "" {
			referencedCtrl[NetAppNodeID(oc, node)] = struct{}{}
		}
	}
	for _, n := range deferredNetAppNode {
		if !netappInfraPassesFilters(n, scope, referencedCtrl) {
			continue
		}
		out[n.ID()] = n
	}
	// No pullNetAppParents here: referencedCtrl above already names the owning
	// netapp-node of every aggregate admitted by this pass, so the sweep would
	// find nothing. Project runs it once AFTER filterEdges, which is the only
	// remaining way an aggregate can enter the view without having passed this
	// loop.
	return out
}

// pullNetAppParents admits the owning netapp-node of every admitted
// netapp-aggr so the real-node compound parent cannot dangle. Called exactly
// once per projection, from Project, after both filter passes.
func pullNetAppParents(g *Graph, nodes map[string]GraphNode) {
	for _, n := range nodes {
		if n.Type() != NodeTypeNetAppAggr {
			continue
		}
		oc, node := n.Labels()["ontap_cluster"], n.Labels()["node"]
		if oc == "" || node == "" {
			continue
		}
		id := NetAppNodeID(oc, node)
		if _, ok := nodes[id]; ok {
			continue
		}
		parent, ok := g.NodesByID[id]
		if ok {
			nodes[id] = parent
		}
	}
}

func nodePassesFilters(n GraphNode, scope Scope, rawName func(string) string) bool {
	labels := n.Labels()
	if len(scope.Clusters) > 0 {
		if n.Type() == NodeTypeExternal {
			// External nodes have no cluster; exclude when caller scoped to clusters.
			return false
		}
		if n.Type() == NodeTypeNetAppAggr || n.Type() == NodeTypeNetAppNode {
			// NetApp types belong to no Kubernetes cluster; they pass the
			// cluster check and are gated purely by reference (see
			// netappInfraPassesFilters).
		} else if _, ok := scope.Clusters[rawName(labels["cluster"])]; !ok {
			return false
		}
	}
	if len(scope.Namespaces) > 0 {
		// ExternalNode is cluster-unscoped (no namespace label) and only ever
		// enters a view as the re-added partner of a pod-calls-pod edge, so it
		// is exempt from the namespace match. K8sNode and NetApp types are
		// also namespace-less but are admitted separately by the infra
		// predicates (referenced-by-in-scope rule), so they never reach this
		// predicate. Every other node type must match the requested namespace.
		switch n.Type() {
		case NodeTypeExternal:
			// pass-through
		default:
			if _, ok := scope.Namespaces[labels["namespace"]]; !ok {
				return false
			}
		}
	}
	return true
}

// infraNodePassesFilters decides whether a cluster-scoped infrastructure node
// (today: a K8sNode) is admitted to a view. It carries no namespace label, so
// admission is reference-driven: the node is kept iff `referenced` contains its
// id — i.e. some in-scope element references it (a pod scheduled on it via
// labels.node). This holds for EVERY request shape (no filter, cluster filter,
// namespace filter), so the response only ever carries infra nodes connected to
// in-scope workload — a node hosting no in-scope pod is dropped, not surfaced as
// an orphan. The cluster-less NetApp types use netappInfraPassesFilters instead.
//
// The one exception is `prune=false` (scope.Inventory), which admits an
// unreferenced infra node — but only when no ACTIVE filter could have excluded
// it by its own labels (see the guard below). That is what makes `?prune=false`
// alone the full inventory while `?namespace=x&prune=false` stays the
// namespace's storage topology.
//
// The cluster filter applies first and exactly as for other node types (the
// node's own labels carry cluster). See design.md D6.
func infraNodePassesFilters(n GraphNode, scope Scope, referenced map[string]struct{}, rawName func(string) string) bool {
	labels := n.Labels()
	if len(scope.Clusters) > 0 {
		if _, ok := scope.Clusters[rawName(labels["cluster"])]; !ok {
			return false
		}
	}
	// A K8s node carries `cluster` (applied above) but no `namespace`, so under
	// a namespace filter its only meaningful admission stays "some in-scope pod
	// is scheduled on it" — lifting there would emit every node of the loaded
	// clusters into a namespace view.
	if scope.Inventory && len(scope.Namespaces) == 0 {
		return true
	}
	// Default: admit iff referenced by an in-scope element.
	_, ok := referenced[n.ID()]
	return ok
}

// netappInfraPassesFilters is the NetApp-type twin of infraNodePassesFilters.
// The cluster filter is skipped (the NetApp types carry no Kubernetes cluster
// label, and their Harvest series receive neither a cluster nor a namespace
// matcher), so admission is reference-driven. The Inventory lift requires that
// NEITHER a cluster NOR a namespace filter is active, since both reach these
// nodes only through the claims that join them.
func netappInfraPassesFilters(n GraphNode, scope Scope, referenced map[string]struct{}) bool {
	if scope.Inventory && len(scope.Clusters) == 0 && len(scope.Namespaces) == 0 {
		return true
	}
	_, ok := referenced[n.ID()]
	return ok
}

func filterEdges(g *Graph, scope Scope, nodes map[string]GraphNode, excluded map[string]struct{}) []*Edge {
	out := make([]*Edge, 0, len(g.Edges))
	// Snapshot the in-scope set at entry. Re-adds during this pass MUST NOT
	// promote a re-added partner into a new in-scope anchor, otherwise a
	// cluster anchor would cascade through the graph indefinitely.
	primary := make(map[string]struct{}, len(nodes))
	for id := range nodes {
		primary[id] = struct{}{}
	}
	for _, e := range g.Edges {
		if !scope.edgeTypeAllowed(e.Type) {
			continue
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
		// This single rule covers (a) non-pod endpoints incident on in-scope
		// pods — the pod-to-node host, the pvc-to-netapp-aggr aggregate, and
		// the `external` partner a filtered build materialises for an
		// out-of-scope peer — and (b) in an UNFILTERED build, a cross-cluster
		// pod-calls-pod partner outside a projection-level cluster filter.
		// (Under a selector-level cluster filter that partner's topology was
		// never loaded, so the edge already points at an external node.)
		if !readdEdgePartners(g, e, nodes, srcOK, tgtOK, scope, excluded, nodePassesNonClusterFilters) {
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
	excluded map[string]struct{},
	pred func(GraphNode, Scope) bool,
) bool {
	// A connectivity-excluded endpoint must stay excluded: re-adding it here
	// (e.g. the pruned pod of a pod-to-node edge whose node survived via another
	// pod) would resurrect what the default prune dropped. A partner missing only
	// because of a cluster/namespace filter is NOT in excluded, so legitimate
	// cross-cluster partner preservation is unaffected.
	if !srcOK {
		if _, ex := excluded[e.Source]; ex {
			return false
		}
		partner, ok := g.NodesByID[e.Source]
		if !ok || !pred(partner, scope) {
			return false
		}
		nodes[e.Source] = partner
	}
	if !tgtOK {
		if _, ex := excluded[e.Target]; ex {
			return false
		}
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
		case NodeTypeExternal, NodeTypeK8sNode, NodeTypeNetAppAggr, NodeTypeNetAppNode:
			// pass-through; these types carry no namespace.
		default:
			if _, ok := scope.Namespaces[labels["namespace"]]; !ok {
				return false
			}
		}
	}
	return true
}
