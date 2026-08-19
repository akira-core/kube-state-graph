package build

import (
	"log/slog"
	"sort"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// ioFamily is one of the six Harvest volume I/O series.
type ioFamily int

const (
	ioReadOps ioFamily = iota
	ioWriteOps
	ioReadLat
	ioWriteLat
	ioReadData
	ioWriteData
)

// volumeCandidate is one Harvest volume-object sample used by the storage join.
type volumeCandidate struct {
	ontapCluster string
	node         string
	aggr         string
	svm          string
	family       ioFamily
	value        float64
}

// pvcVolume is a PVC that carries a non-empty volumename (the join key).
type pvcVolume struct {
	id, volumeName string
}

// netappResult is the demand-driven output of resolveNetAppStorage.
type netappResult struct {
	svmByPVC map[string]string
	aggrs    []*graph.NetAppAggrNode
	nodes    []*graph.NetAppNode
	edges    []*graph.Edge
}

type aggrKey struct{ oc, aggr string }
type nodeKey struct{ oc, node string }

// resolveNetAppStorage joins PVC PV names to Harvest volume series and
// materialises NetApp aggregate/controller nodes plus pvc-to-netapp-aggr
// edges. Pure except for the aggregated coverage warning (D8).
func resolveNetAppStorage(
	claims []pvcVolume,
	readOps, writeOps, readLat, writeLat, readData, writeData model.Vector,
	aggrStatus, aggrUsed, aggrTotal, nodeStatus model.Vector,
) netappResult {
	out := netappResult{svmByPVC: map[string]string{}}
	index := map[string][]volumeCandidate{}
	indexFamily(index, readOps, ioReadOps)
	indexFamily(index, writeOps, ioWriteOps)
	indexFamily(index, readLat, ioReadLat)
	indexFamily(index, writeLat, ioWriteLat)
	indexFamily(index, readData, ioReadData)
	indexFamily(index, writeData, ioWriteData)
	harvestPresent := len(index) > 0

	type joinHit struct {
		oc, aggr, node string
		io             *graph.IOMetrics
	}
	hits := map[string]joinHit{} // pvcID → pick
	misses := 0

	for _, c := range claims {
		cands := index[c.volumeName]
		if svm := pickSVM(cands); svm != "" {
			out.svmByPVC[c.id] = svm
		}
		oc, aggr := pickAggr(cands)
		if oc == "" || aggr == "" {
			if harvestPresent {
				misses++
			}
			continue
		}
		hits[c.id] = joinHit{
			oc: oc, aggr: aggr,
			node: pickOwner(cands, oc, aggr),
			io:   sumIO(cands),
		}
	}

	if harvestPresent && misses > 0 {
		slog.Warn("netapp_volume_join_miss", "count", misses)
	}

	// Owner vote is across ALL volume series of the aggregate (takeover in
	// window), not just the joining PVC's series. Status series never votes.
	allByAggr := map[aggrKey][]volumeCandidate{}
	for _, cands := range index {
		for _, c := range cands {
			if c.aggr == "" || c.ontapCluster == "" {
				continue
			}
			k := aggrKey{c.ontapCluster, c.aggr}
			allByAggr[k] = append(allByAggr[k], c)
		}
	}

	aggrHealth := healthByAggr(aggrStatus)
	aggrUsage := usageByAggr(aggrUsed, aggrTotal)
	nodeHealth := healthByNode(nodeStatus)

	aggrSeen := map[aggrKey]*graph.NetAppAggrNode{}
	for _, hit := range hits {
		k := aggrKey{hit.oc, hit.aggr}
		if _, ok := aggrSeen[k]; ok {
			continue
		}
		owner := pickOwner(allByAggr[k], hit.oc, hit.aggr)
		if owner == "" {
			owner = hit.node
		}
		n := &graph.NetAppAggrNode{
			IDValue:     graph.NetAppAggrID(hit.oc, hit.aggr),
			NameValue:   hit.aggr,
			LabelsValue: map[string]string{"ontap_cluster": hit.oc, "node": owner},
			HealthValue: aggrHealth[k],
			UsageValue:  aggrUsage[k],
		}
		aggrSeen[k] = n
	}

	nodeSeen := map[nodeKey]*graph.NetAppNode{}
	for _, a := range aggrSeen {
		oc, node := a.LabelsValue["ontap_cluster"], a.LabelsValue["node"]
		if oc == "" || node == "" {
			continue
		}
		nk := nodeKey{oc, node}
		if _, ok := nodeSeen[nk]; ok {
			continue
		}
		nodeSeen[nk] = &graph.NetAppNode{
			IDValue:     graph.NetAppNodeID(oc, node),
			NameValue:   node,
			LabelsValue: map[string]string{"ontap_cluster": oc},
			HealthValue: nodeHealth[nk],
		}
	}

	out.aggrs = make([]*graph.NetAppAggrNode, 0, len(aggrSeen))
	for _, a := range aggrSeen {
		out.aggrs = append(out.aggrs, a)
	}
	sort.Slice(out.aggrs, func(i, j int) bool { return out.aggrs[i].ID() < out.aggrs[j].ID() })

	out.nodes = make([]*graph.NetAppNode, 0, len(nodeSeen))
	for _, n := range nodeSeen {
		out.nodes = append(out.nodes, n)
	}
	sort.Slice(out.nodes, func(i, j int) bool { return out.nodes[i].ID() < out.nodes[j].ID() })

	seenEdge := map[[2]string]bool{}
	for _, c := range claims {
		hit, ok := hits[c.id]
		if !ok {
			continue
		}
		tgt := graph.NetAppAggrID(hit.oc, hit.aggr)
		key := [2]string{c.id, tgt}
		if seenEdge[key] {
			continue
		}
		seenEdge[key] = true
		e := graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, c.id, tgt, nil)
		if hit.io != nil {
			e = e.WithIO(*hit.io)
		}
		out.edges = append(out.edges, e)
	}
	graph.SortEdges(out.edges)
	return out
}

func indexFamily(dst map[string][]volumeCandidate, vec model.Vector, fam ioFamily) {
	for _, s := range vec {
		vn := string(s.Metric["volume_name"])
		oc := string(s.Metric["cluster"])
		if vn == "" || oc == "" {
			continue
		}
		dst[vn] = append(dst[vn], volumeCandidate{
			ontapCluster: oc,
			node:         string(s.Metric["node"]),
			aggr:         string(s.Metric["aggr"]),
			svm:          string(s.Metric["svm"]),
			family:       fam,
			value:        float64(s.Value),
		})
	}
}

func pickAggr(cands []volumeCandidate) (oc, aggr string) {
	for _, c := range cands {
		if c.aggr == "" || c.ontapCluster == "" {
			continue
		}
		if oc == "" || c.ontapCluster < oc || (c.ontapCluster == oc && c.aggr < aggr) {
			oc, aggr = c.ontapCluster, c.aggr
		}
	}
	return oc, aggr
}

func pickOwner(cands []volumeCandidate, oc, aggr string) string {
	var node string
	for _, c := range cands {
		if c.ontapCluster != oc || c.aggr != aggr || c.node == "" {
			continue
		}
		if node == "" || c.node < node {
			node = c.node
		}
	}
	return node
}

func pickSVM(cands []volumeCandidate) string {
	var svm string
	for _, c := range cands {
		if c.svm == "" {
			continue
		}
		if svm == "" || c.svm < svm {
			svm = c.svm
		}
	}
	return svm
}

func sumIO(cands []volumeCandidate) *graph.IOMetrics {
	var readOps, writeOps, readLat, writeLat, readData, writeData []float64
	for _, c := range cands {
		switch c.family {
		case ioReadOps:
			readOps = append(readOps, c.value)
		case ioWriteOps:
			writeOps = append(writeOps, c.value)
		case ioReadLat:
			readLat = append(readLat, c.value)
		case ioWriteLat:
			writeLat = append(writeLat, c.value)
		case ioReadData:
			readData = append(readData, c.value)
		case ioWriteData:
			writeData = append(writeData, c.value)
		}
	}
	if len(readOps)+len(writeOps)+len(readLat)+len(writeLat)+len(readData)+len(writeData) == 0 {
		return nil
	}
	io := &graph.IOMetrics{}
	if v, ok := sumIOFamily(readOps); ok {
		io.ReadOps = &v
	}
	if v, ok := sumIOFamily(writeOps); ok {
		io.WriteOps = &v
	}
	if v, ok := sumIOFamily(readLat); ok {
		io.ReadLatencyUs = &v
	}
	if v, ok := sumIOFamily(writeLat); ok {
		io.WriteLatencyUs = &v
	}
	if v, ok := sumIOFamily(readData); ok {
		io.ReadBytesPerSec = &v
	}
	if v, ok := sumIOFamily(writeData); ok {
		io.WriteBytesPerSec = &v
	}
	return io
}

func sumIOFamily(vals []float64) (float64, bool) {
	if len(vals) == 0 {
		return 0, false
	}
	sort.Float64s(vals)
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum, true
}

func healthFromSamples(vals []float64) string {
	if len(vals) == 0 {
		return ""
	}
	for _, v := range vals {
		if v != 1 {
			return graph.HealthDegraded
		}
	}
	return graph.HealthOnline
}

func healthByAggr(vec model.Vector) map[aggrKey]string {
	raw := map[aggrKey][]float64{}
	for _, s := range vec {
		oc, aggr := string(s.Metric["cluster"]), string(s.Metric["aggr"])
		if oc == "" || aggr == "" {
			continue
		}
		k := aggrKey{oc, aggr}
		raw[k] = append(raw[k], float64(s.Value))
	}
	out := make(map[aggrKey]string, len(raw))
	for k, vals := range raw {
		out[k] = healthFromSamples(vals)
	}
	return out
}

func healthByNode(vec model.Vector) map[nodeKey]string {
	raw := map[nodeKey][]float64{}
	for _, s := range vec {
		oc, node := string(s.Metric["cluster"]), string(s.Metric["node"])
		if oc == "" || node == "" {
			continue
		}
		k := nodeKey{oc, node}
		raw[k] = append(raw[k], float64(s.Value))
	}
	out := make(map[nodeKey]string, len(raw))
	for k, vals := range raw {
		out[k] = healthFromSamples(vals)
	}
	return out
}

func usageByAggr(used, total model.Vector) map[aggrKey]*graph.UsageBytes {
	smallest := func(vec model.Vector) map[aggrKey]float64 {
		out := map[aggrKey]float64{}
		seen := map[aggrKey]bool{}
		for _, s := range vec {
			oc, aggr := string(s.Metric["cluster"]), string(s.Metric["aggr"])
			if oc == "" || aggr == "" {
				continue
			}
			k := aggrKey{oc, aggr}
			v := float64(s.Value)
			if !seen[k] || v < out[k] {
				out[k] = v
				seen[k] = true
			}
		}
		return out
	}
	u := smallest(used)
	c := smallest(total)
	keys := map[aggrKey]struct{}{}
	for k := range u {
		keys[k] = struct{}{}
	}
	for k := range c {
		keys[k] = struct{}{}
	}
	out := make(map[aggrKey]*graph.UsageBytes, len(keys))
	for k := range keys {
		ub := &graph.UsageBytes{}
		if v, ok := u[k]; ok {
			vv := v
			ub.UsedBytes = &vv
		}
		if v, ok := c[k]; ok {
			vv := v
			ub.CapacityBytes = &vv
		}
		out[k] = ub
	}
	return out
}

// resolvePVCUsage joins kubelet volume-stats onto (cluster, ns, claim).
// Per-field independent; smallest numeric value on duplicates.
func resolvePVCUsage(used, cap model.Vector, mc missingClusterCounts) map[pvcKey]*graph.UsageBytes {
	usedBy := map[pvcKey]float64{}
	usedSeen := map[pvcKey]bool{}
	for _, s := range used {
		cluster := mc.bucket(promql.QKubeletVolumeUsedBytes, string(s.Metric["cluster"]))
		ns := string(s.Metric["namespace"])
		claim := string(s.Metric["persistentvolumeclaim"])
		if claim == "" {
			continue
		}
		k := pvcKey{cluster, ns, claim}
		v := float64(s.Value)
		if !usedSeen[k] || v < usedBy[k] {
			usedBy[k] = v
			usedSeen[k] = true
		}
	}
	capBy := map[pvcKey]float64{}
	capSeen := map[pvcKey]bool{}
	for _, s := range cap {
		cluster := mc.bucket(promql.QKubeletVolumeCapacityBytes, string(s.Metric["cluster"]))
		ns := string(s.Metric["namespace"])
		claim := string(s.Metric["persistentvolumeclaim"])
		if claim == "" {
			continue
		}
		k := pvcKey{cluster, ns, claim}
		v := float64(s.Value)
		if !capSeen[k] || v < capBy[k] {
			capBy[k] = v
			capSeen[k] = true
		}
	}
	keys := map[pvcKey]struct{}{}
	for k := range usedBy {
		keys[k] = struct{}{}
	}
	for k := range capBy {
		keys[k] = struct{}{}
	}
	out := make(map[pvcKey]*graph.UsageBytes, len(keys))
	for k := range keys {
		ub := &graph.UsageBytes{}
		if v, ok := usedBy[k]; ok {
			vv := v
			ub.UsedBytes = &vv
		}
		if v, ok := capBy[k]; ok {
			vv := v
			ub.CapacityBytes = &vv
		}
		out[k] = ub
	}
	return out
}
