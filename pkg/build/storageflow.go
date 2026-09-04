package build

import (
	"sort"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// storageChain is one claim's resolved position on the fixed tier chain:
// the aggregate it landed on, that aggregate's owning controller, and the SVM
// its FlexVol lives in.
//
// A FlexGroup claim resolves an SVM but no aggregate, so aggrID / ctrlID stay
// empty and the claim enters the chain at the svm-pvc tier. A claim that
// resolved no SVM at all contributes NO path: the tier chain is fixed and an
// aggr → pvc shortcut is not permitted.
type storageChain struct {
	pvcID  string
	svmID  string
	aggrID string
	ctrlID string
	io     *graph.IOMetrics
}

// assembleStorageFlow builds the storage-flow graph: every NetApp entity the
// Harvest read named, the loaded Kubernetes workload, and one directed path per
// joined-and-mounted claim.
//
// It is the storage counterpart of assemble and, like it, a pure function of
// the topology. It emits NO service, external, or non-storage-flow edge — the
// service-graph read does not run for this build at all.
//
// Materialisation is deliberately WIDER than the /v1/graph join: every
// inventory controller, aggregate and SVM is emitted whether or not a claim
// reaches it, because a root must be drawable even when nothing flows through
// it. Everything flowless is dropped by ProjectStorage unless it IS a root, so
// the width costs nothing in the response.
//
// The build bakes exactly ONE weight: the claim's own IOMetrics on its svm-pvc
// edge. Every other tier is emitted weightless and is summed at projection over
// the RETAINED flow units — weights baked over the full estate would fail to
// conserve the moment a filter or a root removed a unit.
func assembleStorageFlow(topology Topology) ([]graph.GraphNode, []*graph.Edge) {
	nodes := storageFlowNodes(topology)
	chains := storageChains(topology)
	edges := storageFlowEdges(chains, topology.PodPVCs, podNodeIDs(topology))

	graph.SortNodes(nodes)
	graph.SortEdges(edges)
	return nodes, edges
}

// storageFlowNodes materialises the node set: the whole NetApp inventory plus
// the loaded pods, Kubernetes nodes and PVCs.
//
// Append order mirrors assemble's rule — the authoritative topology nodes go in
// before anything that could mint a colliding id — but nothing here can
// collide: NetApp ids live under the "netapp/" prefix and the Kubernetes ids
// are the same cluster-scoped ids /v1/graph uses.
func storageFlowNodes(topology Topology) []graph.GraphNode {
	inv := topology.NetAppInventory
	out := make([]graph.GraphNode, 0,
		len(topology.Pods)+len(topology.Nodes)+len(topology.PVCs)+
			len(inv.Nodes)+len(inv.Aggrs)+len(inv.SVMs))

	for _, p := range topology.Pods {
		out = append(out, p)
	}
	for _, n := range topology.Nodes {
		out = append(out, n)
	}
	for _, c := range topology.PVCs {
		out = append(out, c)
	}
	for _, n := range inv.Nodes {
		out = append(out, n)
	}
	for _, a := range inv.Aggrs {
		out = append(out, a)
	}
	for _, s := range inv.SVMs {
		out = append(out, s)
	}
	return out
}

// storageChains resolves each joined claim's position on the tier chain.
//
// The aggregate and its I/O come from the pvc-to-netapp-aggr edges the storage
// join already produced — the same edges /v1/graph serialises — so the two
// endpoints cannot disagree about which aggregate serves a claim or how much it
// is pushing. The SVM comes from SVMByPVC, which is the ONLY source for a
// FlexGroup claim (it has no aggregate edge at all).
//
// Chains are returned in ascending PVC-id order so every downstream loop is
// order-free.
func storageChains(topology Topology) []storageChain {
	type aggrHit struct {
		aggrID string
		io     *graph.IOMetrics
	}
	byPVC := make(map[string]aggrHit, len(topology.StorageEdges))
	for _, e := range topology.StorageEdges {
		if e.Type != graph.EdgeTypePVCToNetAppAggr {
			continue
		}
		byPVC[e.Source] = aggrHit{aggrID: e.Target, io: e.IO}
	}

	// The aggregate's current owner, read off the materialised aggregate rather
	// than re-derived: the node-aggr tier must name the same controller the
	// aggregate's own data.parent does, or the Sankey path and the compound
	// hierarchy would disagree after an HA takeover.
	ownerOf := make(map[string]string, len(topology.NetAppInventory.Aggrs))
	for _, a := range topology.NetAppInventory.Aggrs {
		labels := a.Labels()
		if oc, node := labels["ontap_cluster"], labels["node"]; oc != "" && node != "" {
			ownerOf[a.ID()] = graph.NetAppNodeID(oc, node)
		}
	}

	out := make([]storageChain, 0, len(topology.SVMByPVC))
	for pvcID, ref := range topology.SVMByPVC {
		// No SVM, no path. The chain is fixed: a claim that resolved an
		// aggregate but no SVM would need an aggr → pvc shortcut, which the
		// tier contract does not permit, so it is counted like a topology miss
		// for this graph and simply draws nothing.
		if ref.SVM == "" || ref.ONTAPCluster == "" {
			continue
		}
		c := storageChain{
			pvcID: pvcID,
			svmID: graph.NetAppSVMID(ref.ONTAPCluster, ref.SVM),
		}
		if hit, ok := byPVC[pvcID]; ok {
			c.aggrID = hit.aggrID
			c.ctrlID = ownerOf[hit.aggrID]
			c.io = hit.io
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pvcID < out[j].pvcID })
	return out
}

// podNodeIDs maps each loaded pod's id to the Kubernetes node it is scheduled
// on. A pod carries its node as a ready-made cluster-scoped id in labels.node
// (set at parse time), and an unscheduled pod carries no such label at all —
// which is exactly how its path ends at the pvc-pod tier.
func podNodeIDs(topology Topology) map[string]string {
	out := make(map[string]string, len(topology.Pods))
	for _, p := range topology.Pods {
		if nodeID := p.Labels()["node"]; nodeID != "" {
			out[p.ID()] = nodeID
		}
	}
	return out
}

// storageFlowEdges emits the tier edges of every chain.
//
// Each (source, target) pair is emitted at most ONCE however many claims flow
// through it: two claims on the same aggregate share one node-aggr edge, and
// the projection then sums their weights onto it. The de-duplication is an
// insert-only set keyed on the pair, so it is order-free (D6) — and it has to
// be, because the same pair can be reached from chains in any order.
//
// mountersOf is derived from the pod-mounts-pvc bindings /v1/graph already
// computes, so "which pods mount this claim" means the same thing on both
// endpoints.
func storageFlowEdges(chains []storageChain, bindings []PodPVCBinding, nodeOf map[string]string) []*graph.Edge {
	mountersOf := make(map[string][]string)
	for _, b := range bindings {
		mountersOf[b.PVCID] = append(mountersOf[b.PVCID], b.PodID)
	}
	for pvcID := range mountersOf {
		sort.Strings(mountersOf[pvcID])
	}

	var out []*graph.Edge
	seen := make(map[[2]string]bool)
	// emit appends one tier edge, skipping a pair already emitted and a hop
	// with a missing endpoint. `io` is non-nil for exactly one tier — see the
	// svm-pvc call below — and is applied before the edge is appended, so an
	// edge is never mutated after construction.
	emit := func(source, target, tier string, labels map[string]string, io *graph.IOMetrics) {
		if source == "" || target == "" {
			return
		}
		key := [2]string{source, target}
		if seen[key] {
			return
		}
		seen[key] = true
		l := map[string]string{"tier": tier}
		for k, v := range labels {
			l[k] = v
		}
		e := graph.NewEdge(graph.EdgeTypeStorageFlow, source, target, l)
		if io != nil {
			e = e.WithIO(*io)
		}
		out = append(out, e)
	}

	for _, c := range chains {
		mounters := mountersOf[c.pvcID]
		// An UNMOUNTED claim draws no path at all: with no pod there is no
		// Sankey flow to render, and ProjectStorage would drop every node on
		// the path anyway. Emitting it would put a dangling stub on the
		// storage side of the diagram.
		if len(mounters) == 0 {
			continue
		}

		emit(c.ctrlID, c.aggrID, graph.StorageTierNodeAggr, nil, nil)
		emit(c.aggrID, c.svmID, graph.StorageTierAggrSVM, nil, nil)

		// The ONE weight the build bakes. It rides the claim-level edge because
		// that is the tier the measurement actually describes: the QoS workload
		// series measure the FlexVol, not the controller and not the pod. Every
		// other tier is weightless here and is summed at projection over the
		// RETAINED units.
		//
		// The pair is unique by construction — a claim resolves at most one SVM
		// — so no other claim's measurement can land on this edge.
		//
		// claim_aggr recovers this claim's aggregate after aggr-svm hops are
		// deduplicated across an SVM that spans aggregates. ProjectStorage
		// strips it; it is not a wire label.
		var claimLabels map[string]string
		if c.aggrID != "" {
			claimLabels = map[string]string{graph.ClaimAggrLabel: c.aggrID}
		}
		emit(c.svmID, c.pvcID, graph.StorageTierSVMPVC, claimLabels, c.io)

		// A claim mounted by several pods (RWX) has no observable per-pod
		// share, so the projection splits its weight equally across them. The
		// label is what lets a consumer tell an attributed share from a
		// measurement; a singly-mounted claim carries no attribution key at
		// all.
		var podLabels map[string]string
		if len(mounters) > 1 {
			podLabels = map[string]string{"attribution": graph.AttributionSplit}
		}
		for _, podID := range mounters {
			emit(c.pvcID, podID, graph.StorageTierPVCPod, podLabels, nil)
			// An unscheduled pod ends its path here — nodeOf has no entry, so
			// emit's empty-endpoint guard drops the hop.
			emit(podID, nodeOf[podID], graph.StorageTierPodNode, nil, nil)
		}
	}
	return out
}
