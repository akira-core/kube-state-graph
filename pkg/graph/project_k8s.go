package graph

// ProjectK8s returns a View of the Kubernetes-only graph served by
// GET /v1/k8s-graph, constrained by scope. It does not mutate g.
//
// The algorithm is a pure function of (g, scope), reachability from the roots
// over pod-to-node and pod-mounts-pvc:
//
//  1. Resolve the roots. A node root is a K8sNode whose name is a root value; a
//     pod root is a PodNode whose (namespace, name) is a root ref. Both must
//     pass the cluster filter, and a pod root the namespace filter too.
//  2. Keep a pod iff it is a pod root, or runs on a node root (its pod-to-node
//     edge targets one), and passes the cluster / namespace filters.
//  3. Keep a claim iff a kept pod has a pod-mounts-pvc edge to it, a node iff a
//     kept pod has a pod-to-node edge to it or it is a node root. A claim no
//     kept pod mounts is never drawn: no root kind reaches one.
//  4. Edges are exactly the pod-to-node and pod-mounts-pvc edges whose source
//     is a kept pod and whose target is kept. SortNodes / SortEdges.
//
// Roots show even when they reach nothing else (an unscheduled root pod, a
// podless root node); a root the graph does not hold is simply absent. The
// other pods that mount a shared claim are in g — they drive the claim's
// inherited Application — but no rule here keeps them. Only the pod and node
// root kinds are read; any other kind resolves nothing and yields an empty
// View, as ProjectStorage treats a kind that resolved to nothing.
func ProjectK8s(g *Graph, scope StorageScope) View {
	if g == nil {
		return View{}
	}
	nodeRoots, podRoots := resolveK8sRoots(g, scope)
	if len(nodeRoots)+len(podRoots) == 0 {
		return View{}
	}
	rawName := g.ClusterRawName

	// The edges a kept pod owns, keyed by source, so one scan of g.Edges serves
	// both the pod-by-node rule and the reference rules.
	var podEdges []*Edge
	runsOnRoot := map[string]struct{}{}
	for _, e := range g.Edges {
		switch e.Type {
		case EdgeTypePodToNode:
			podEdges = append(podEdges, e)
			if _, ok := nodeRoots[e.Target]; ok {
				runsOnRoot[e.Source] = struct{}{}
			}
		case EdgeTypePodMountsPVC:
			podEdges = append(podEdges, e)
		default:
		}
	}

	pods := map[string]GraphNode{}
	for id := range podRoots {
		pods[id] = g.NodesByID[id]
	}
	for id := range runsOnRoot {
		n, ok := g.NodesByID[id]
		if ok && n.Type() == NodeTypePod && workloadPassesFilters(n, scope, rawName) {
			pods[id] = n
		}
	}

	nodes := make(map[string]GraphNode, len(pods))
	for id, n := range pods {
		nodes[id] = n
	}
	var edges []*Edge
	for _, e := range podEdges {
		if _, ok := pods[e.Source]; !ok {
			continue
		}
		target, ok := g.NodesByID[e.Target]
		if !ok || !workloadPassesFilters(target, scope, rawName) {
			continue
		}
		nodes[e.Target] = target
		edges = append(edges, e)
	}
	// A node root shows even when no kept pod runs on it.
	for id := range nodeRoots {
		nodes[id] = g.NodesByID[id]
	}

	out := View{Nodes: make([]GraphNode, 0, len(nodes)), Edges: edges}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, n)
	}
	SortNodes(out.Nodes)
	SortEdges(out.Edges)
	return out
}

// resolveK8sRoots resolves scope's pod and node roots to node-id sets, already
// past the cluster filter (and, for a pod, the namespace filter). The match
// itself is resolveStorageRoots' own, so a `node=` or `pod=` value resolves to
// the same nodes on /v1/storage-graph and here. A kind other than pod and node
// resolves nothing: resolveStorageRoots would resolve a claim root to PVCs,
// which this view never roots on.
func resolveK8sRoots(g *Graph, scope StorageScope) (nodeRoots, podRoots map[string]struct{}) {
	nodeRoots = map[string]struct{}{}
	podRoots = map[string]struct{}{}
	switch scope.Roots.Kind {
	case StorageRootNode, StorageRootPod:
	default:
		return nodeRoots, podRoots
	}
	_, workload, _, _ := resolveStorageRoots(g, scope)
	for id := range workload {
		n := g.NodesByID[id]
		if !workloadPassesFilters(n, scope, g.ClusterRawName) {
			continue
		}
		switch n.Type() {
		case NodeTypeK8sNode:
			nodeRoots[id] = struct{}{}
		case NodeTypePod:
			podRoots[id] = struct{}{}
		default:
		}
	}
	return nodeRoots, podRoots
}
