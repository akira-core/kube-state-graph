package graph

import (
	"fmt"
	"math/rand"
	"strconv"
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

// TestProperty_StorageProjection: random storage-flow estates × random root
// sets. For every projection: retained ⊆ built, interior-node flow conserves
// to 6 significant digits, and shuffling the input slices yields a
// byte-identical view. 1000 iterations — the task pin.
func TestProperty_StorageProjection(t *testing.T) {
	for seed := int64(1); seed <= 1000; seed++ {
		r := rand.New(rand.NewSource(seed))
		g := genStorageEstate(r)
		scope := genStorageScope(r, g)

		v := ProjectStorage(g, scope)
		assertStorageRetainedSubset(t, seed, g, v)
		assertStorageConservation(t, seed, v)

		shuffled := shuffleStorageGraph(r, g)
		v2 := ProjectStorage(shuffled, scope)
		require.Equalf(t, nodeIDList(v), nodeIDList(v2), "seed=%d: shuffle changed nodes", seed)
		require.Equalf(t, edgeFingerprint(v), edgeFingerprint(v2), "seed=%d: shuffle changed edges", seed)
	}
}

func genStorageEstate(r *rand.Rand) *Graph {
	const oc, cluster = "ontap", "c1"
	nCtrl := 1 + r.Intn(3)
	nAggr := 1 + r.Intn(4)
	nSVM := 1 + r.Intn(3)
	nK8s := 1 + r.Intn(3)
	nClaims := 2 + r.Intn(7)

	nodes := make([]GraphNode, 0, 64)
	ctrls := make([]string, nCtrl)
	for i := range nCtrl {
		name := fmt.Sprintf("ctrl-%d", i)
		ctrls[i] = name
		nodes = append(nodes, &NetAppNode{
			IDValue: NetAppNodeID(oc, name), NameValue: name,
			LabelsValue: map[string]string{"ontap_cluster": oc},
		})
	}
	aggrs := make([]string, nAggr)
	for i := range nAggr {
		name := fmt.Sprintf("aggr-%d", i)
		aggrs[i] = name
		owner := ctrls[r.Intn(len(ctrls))]
		nodes = append(nodes, &NetAppAggrNode{
			IDValue: NetAppAggrID(oc, name), NameValue: name,
			LabelsValue: map[string]string{"ontap_cluster": oc, "node": owner},
		})
	}
	svms := make([]string, nSVM)
	for i := range nSVM {
		name := fmt.Sprintf("svm-%d", i)
		svms[i] = name
		nodes = append(nodes, &NetAppSVMNode{
			IDValue: NetAppSVMID(oc, name), NameValue: name,
			LabelsValue: map[string]string{"ontap_cluster": oc},
		})
	}
	k8s := make([]string, nK8s)
	for i := range nK8s {
		name := fmt.Sprintf("worker-%d", i)
		k8s[i] = name
		nodes = append(nodes, &K8sNode{
			IDValue: K8sNodeID(cluster, name), NameValue: name,
			LabelsValue: map[string]string{"cluster": cluster},
		})
	}

	seen := map[[2]string]bool{}
	var edges []*Edge
	emit := func(e *Edge) {
		key := [2]string{e.Source, e.Target}
		if seen[key] {
			return
		}
		seen[key] = true
		edges = append(edges, e)
	}

	podSeq := 0
	for c := range nClaims {
		pvc := &PVCNode{
			IDValue:     PVCID(cluster, "ns", fmt.Sprintf("claim-%d", c)),
			NameValue:   fmt.Sprintf("claim-%d", c),
			LabelsValue: map[string]string{"cluster": cluster, "namespace": "ns"},
		}
		nodes = append(nodes, pvc)
		nMounters := r.Intn(4) // 0–3; 0 = unmounted
		flex := r.Intn(5) == 0
		var io *IOMetrics
		if r.Intn(2) == 0 {
			ops := float64(50 * (1 + r.Intn(8)))
			io = &IOMetrics{
				ReadOps: &ops, WriteOps: ptrFloat(ops / 2),
				ReadLatencyUs: ptrFloat(100), MaxIOPS: ptrFloat(1000),
			}
		}
		svmID := NetAppSVMID(oc, svms[r.Intn(len(svms))])
		var aggrID, ctrlID string
		if !flex {
			aggrName := aggrs[r.Intn(len(aggrs))]
			aggrID = NetAppAggrID(oc, aggrName)
			for _, n := range nodes {
				if n.ID() == aggrID {
					ctrlID = NetAppNodeID(oc, n.Labels()["node"])
					break
				}
			}
		}
		if nMounters == 0 {
			continue
		}
		claimLabels := map[string]string{"tier": StorageTierSVMPVC}
		if aggrID != "" {
			claimLabels[ClaimAggrLabel] = aggrID
		}
		claim := NewEdge(EdgeTypeStorageFlow, svmID, pvc.ID(), claimLabels)
		if io != nil {
			claim = claim.WithIO(*io)
		}
		emit(claim)
		if aggrID != "" {
			emit(NewEdge(EdgeTypeStorageFlow, aggrID, svmID, map[string]string{"tier": StorageTierAggrSVM}))
			if ctrlID != "" {
				emit(NewEdge(EdgeTypeStorageFlow, ctrlID, aggrID, map[string]string{"tier": StorageTierNodeAggr}))
			}
		}
		podLabels := map[string]string{"tier": StorageTierPVCPod}
		if nMounters > 1 {
			podLabels["attribution"] = AttributionSplit
		}
		for range nMounters {
			uid := fmt.Sprintf("uid-%d", podSeq)
			podSeq++
			name := fmt.Sprintf("pod-%d", podSeq)
			pl := map[string]string{"cluster": cluster, "namespace": "ns"}
			var nodeID string
			if r.Intn(10) != 0 {
				wn := k8s[r.Intn(len(k8s))]
				nodeID = K8sNodeID(cluster, wn)
				pl["node"] = nodeID
			}
			pod := &PodNode{IDValue: PodID(cluster, uid), NameValue: name, LabelsValue: pl}
			nodes = append(nodes, pod)
			emit(NewEdge(EdgeTypeStorageFlow, pvc.ID(), pod.ID(), mapsClone(podLabels)))
			if nodeID != "" {
				emit(NewEdge(EdgeTypeStorageFlow, pod.ID(), nodeID, map[string]string{"tier": StorageTierPodNode}))
			}
		}
	}
	return NewGraph(nodes, edges, time.Time{})
}

func ptrFloat(v float64) *float64 { return &v }

func mapsClone(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func genStorageScope(r *rand.Rand, g *Graph) StorageScope {
	// 0 = no root (whole estate); 1 = storage; 2 = workload; 3 = mixed; 4 = typo.
	switch r.Intn(5) {
	case 0:
		return StorageScope{}
	case 1:
		var aggrs, svms []string
		for _, n := range g.NodesByID {
			switch n.Type() {
			case NodeTypeNetAppAggr:
				aggrs = append(aggrs, n.Name())
			case NodeTypeNetAppSVM:
				svms = append(svms, n.Name())
			default:
				// other kinds are not storage roots
			}
		}
		if len(aggrs) > 0 && r.Intn(2) == 0 {
			s, _ := NewStorageScope(nil, nil, nil, nil, []string{aggrs[r.Intn(len(aggrs))]}, nil, nil)
			return s
		}
		if len(svms) > 0 {
			s, _ := NewStorageScope(nil, nil, nil, nil, nil, []string{svms[r.Intn(len(svms))]}, nil)
			return s
		}
		return StorageScope{}
	case 2:
		var pods []string
		var nodes []string
		for _, n := range g.NodesByID {
			switch n.Type() {
			case NodeTypePod:
				pods = append(pods, n.Labels()["namespace"]+"/"+n.Name())
			case NodeTypeK8sNode:
				nodes = append(nodes, n.Name())
			default:
				// other kinds are not workload roots
			}
		}
		if len(pods) > 0 && r.Intn(2) == 0 {
			s, _ := NewStorageScope(nil, nil, nil, nil, nil, nil, []string{pods[r.Intn(len(pods))]})
			return s
		}
		if len(nodes) > 0 {
			s, _ := NewStorageScope(nil, nil, nil, []string{nodes[r.Intn(len(nodes))]}, nil, nil, nil)
			return s
		}
		return StorageScope{}
	case 3:
		var aggrs, pods []string
		for _, n := range g.NodesByID {
			switch n.Type() {
			case NodeTypeNetAppAggr:
				aggrs = append(aggrs, n.Name())
			case NodeTypePod:
				pods = append(pods, n.Labels()["namespace"]+"/"+n.Name())
			default:
				// mixed-root picker only needs aggrs and pods
			}
		}
		if len(aggrs) == 0 || len(pods) == 0 {
			return StorageScope{}
		}
		s, _ := NewStorageScope(nil, nil, nil, nil, []string{aggrs[r.Intn(len(aggrs))]}, nil, []string{pods[r.Intn(len(pods))]})
		return s
	default:
		s, _ := NewStorageScope(nil, nil, nil, nil, []string{"typo-aggr"}, nil, nil)
		return s
	}
}

func shuffleStorageGraph(r *rand.Rand, g *Graph) *Graph {
	nodes := make([]GraphNode, 0, len(g.NodesByID))
	for _, n := range g.NodesByID {
		nodes = append(nodes, n)
	}
	r.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
	edges := append([]*Edge(nil), g.Edges...)
	r.Shuffle(len(edges), func(i, j int) { edges[i], edges[j] = edges[j], edges[i] })
	out := NewGraph(nodes, edges, g.BuiltAt)
	out.ClusterIdentities = g.ClusterIdentities
	return out
}

func assertStorageRetainedSubset(t *testing.T, seed int64, g *Graph, v View) {
	t.Helper()
	for _, n := range v.Nodes {
		_, ok := g.NodesByID[n.ID()]
		require.Truef(t, ok, "seed=%d: retained node %s not in built graph", seed, n.ID())
	}
	builtEdges := map[string]bool{}
	for _, e := range g.Edges {
		builtEdges[e.ID] = true
	}
	for _, e := range v.Edges {
		require.Truef(t, builtEdges[e.ID], "seed=%d: retained edge %s not in built graph", seed, e.ID)
		_, srcOK := g.NodesByID[e.Source]
		_, tgtOK := g.NodesByID[e.Target]
		require.Truef(t, srcOK && tgtOK, "seed=%d: edge %s has unresolved endpoint", seed, e.ID)
	}
}

func assertStorageConservation(t *testing.T, seed int64, v View) {
	t.Helper()
	type bucket struct {
		in, out       [4]float64
		hasIn, hasOut bool
	}
	by := map[string]*bucket{}
	ensure := func(id string) *bucket {
		b, ok := by[id]
		if !ok {
			b = &bucket{}
			by[id] = b
		}
		return b
	}
	add := func(dst *[4]float64, io *IOMetrics) {
		if io == nil {
			return
		}
		if io.ReadOps != nil {
			dst[0] += *io.ReadOps
		}
		if io.WriteOps != nil {
			dst[1] += *io.WriteOps
		}
		if io.ReadBytesPerSec != nil {
			dst[2] += *io.ReadBytesPerSec
		}
		if io.WriteBytesPerSec != nil {
			dst[3] += *io.WriteBytesPerSec
		}
	}
	for _, e := range v.Edges {
		if e.Type != EdgeTypeStorageFlow {
			continue
		}
		s, t := ensure(e.Source), ensure(e.Target)
		s.hasOut = true
		t.hasIn = true
		add(&s.out, e.IO)
		add(&t.in, e.IO)
	}
	for id, b := range by {
		if !b.hasIn || !b.hasOut {
			continue
		}
		n, ok := nodeByID(v, id)
		if !ok {
			continue
		}
		switch n.Type() {
		case NodeTypePVC, NodeTypePod, NodeTypeNetAppAggr:
		case NodeTypeNetAppSVM, NodeTypeNetAppNode, NodeTypeK8sNode, NodeTypeService, NodeTypeExternal:
			continue
		default:
			continue
		}
		for i, name := range []string{"read_ops", "write_ops", "read_bytes", "write_bytes"} {
			require.InDeltaf(t, round6prop(b.in[i]), round6prop(b.out[i]), 1e-9,
				"seed=%d node=%s type=%s %s in=%g out=%g", seed, id, n.Type(), name, b.in[i], b.out[i])
		}
	}
}

func nodeByID(v View, id string) (GraphNode, bool) {
	for _, n := range v.Nodes {
		if n.ID() == id {
			return n, true
		}
	}
	return nil, false
}

func round6prop(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 6, 64), 64)
	return r
}

func edgeFingerprint(v View) []string {
	out := make([]string, len(v.Edges))
	for i, e := range v.Edges {
		fp := e.ID + "|" + e.Labels["tier"]
		if e.IO != nil {
			fp += fmt.Sprintf("|%v|%v|%v|%v",
				fmtPtr(e.IO.ReadOps), fmtPtr(e.IO.WriteOps),
				fmtPtr(e.IO.ReadLatencyUs), fmtPtr(e.IO.MaxIOPS))
		}
		out[i] = fp
	}
	return out
}

func fmtPtr(p *float64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatFloat(*p, 'g', -1, 64)
}
