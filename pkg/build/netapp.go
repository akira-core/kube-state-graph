package build

import (
	"log/slog"
	"sort"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// ioFamily is one of the six Harvest QoS workload I/O series.
type ioFamily int

const (
	ioReadOps ioFamily = iota
	ioWriteOps
	ioReadLat
	ioWriteLat
	ioReadData
	ioWriteData
)

// bytesPerMB scales a QoS fixed policy's megabytes-per-second ceiling to bytes
// per second, so max_bytes_per_sec carries the unit of the measured
// read_bytes_per_sec / write_bytes_per_sec and the two compare directly
// (design.md D2 — the ONE value in this resolver not read verbatim).
// Basis: ONTAP expresses QoS throughput limits in binary megabytes.
const bytesPerMB = 1048576

// volumeLabelCandidate is one Harvest volume_labels sample — hop A of the
// storage join (design.md D3). This series is the SOLE source of the storage
// topology; its sample value is discarded and only its labels are consumed.
type volumeLabelCandidate struct {
	ontapCluster string
	node         string
	aggr         string
	svm          string
}

// qosCandidate is one Harvest QoS workload sample — hop B. It carries no
// aggr/node dimension, which is exactly why hop A exists.
type qosCandidate struct {
	ontapCluster string
	svm          string
	policyGroup  string
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
type policyKey struct{ oc, svm, policy string }

// resolveNetAppStorage runs the three-hop storage join of design.md D3:
//
//	hop A  volume_labels             → edge + aggregate + controller + PVC svm
//	hop B  qos_* workload families   → the six measured I/O figures + policy group
//	hop C  qos_policy_fixed_max_*    → the volume's declared throughput ceiling
//
// The hops degrade independently: a hop-B miss leaves a valid measurement-less
// edge rather than erasing the claim's storage topology. Pure except for the
// two aggregated coverage warnings (D8).
func resolveNetAppStorage(
	claims []pvcVolume,
	volumeLabels model.Vector,
	readOps, writeOps, readLat, writeLat, readData, writeData model.Vector,
	policyMaxIOPS, policyMaxMBps model.Vector,
	aggrStatus, aggrUsed, aggrTotal, nodeStatus model.Vector,
) netappResult {
	out := netappResult{svmByPVC: map[string]string{}}

	volIndex := map[string][]volumeLabelCandidate{}
	for _, s := range volumeLabels {
		vn, oc := string(s.Metric["volume_name"]), string(s.Metric["cluster"])
		if vn == "" || oc == "" {
			continue
		}
		volIndex[vn] = append(volIndex[vn], volumeLabelCandidate{
			ontapCluster: oc,
			node:         string(s.Metric["node"]),
			aggr:         string(s.Metric["aggr"]),
			svm:          string(s.Metric["svm"]),
		})
	}

	qosIndex := map[string][]qosCandidate{}
	indexQoSFamily(qosIndex, readOps, ioReadOps)
	indexQoSFamily(qosIndex, writeOps, ioWriteOps)
	indexQoSFamily(qosIndex, readLat, ioReadLat)
	indexQoSFamily(qosIndex, writeLat, ioWriteLat)
	indexQoSFamily(qosIndex, readData, ioReadData)
	indexQoSFamily(qosIndex, writeData, ioWriteData)

	policyIndex := indexPolicyCeilings(policyMaxIOPS, policyMaxMBps)

	// Each coverage signal is gated on ITS OWN family having been read, so a
	// deployment running the volume template without the QoS template gets its
	// topology graph and no spurious I/O warning. Gating on the raw vectors
	// rather than the built indexes is deliberate: a vector whose series carry
	// no volume_name WAS read, and a broken relabel rule is exactly the
	// coverage failure these signals exist to surface.
	topoPresent := len(volumeLabels) > 0
	qosPresent := len(readOps)+len(writeOps)+len(readLat)+len(writeLat)+len(readData)+len(writeData) > 0

	type joinHit struct {
		oc, aggr string
		io       *graph.IOMetrics
	}
	hits := map[string]joinHit{} // pvcID → pick
	topoMisses, qosMisses := 0, 0

	for _, c := range claims {
		cands := volIndex[c.volumeName]
		svm := pickSVM(cands)
		if svm != "" {
			out.svmByPVC[c.id] = svm
		}
		oc, aggr := pickAggr(cands)
		if oc == "" || aggr == "" {
			if topoPresent {
				topoMisses++
			}
			continue
		}
		io := sumQoSIO(qosIndex[c.volumeName], oc, svm)
		if io == nil {
			if qosPresent {
				qosMisses++
			}
		} else {
			// Structural, not defensive: the ceiling is attached ONLY inside
			// this branch, so a ceiling field can never appear on an edge that
			// carries no measurement (design.md D3 hop C).
			applyCeiling(io, policyIndex, pickPolicy(qosIndex[c.volumeName], oc, svm))
		}
		hits[c.id] = joinHit{oc: oc, aggr: aggr, io: io}
	}

	if topoPresent && topoMisses > 0 {
		slog.Warn("netapp_volume_join_miss", "count", topoMisses)
	}
	if qosPresent && qosMisses > 0 {
		slog.Warn("netapp_qos_join_miss", "count", qosMisses)
	}

	// Owner vote is across ALL volume_labels series of the aggregate (takeover
	// in window), not just the joining PVC's series. Status series never votes.
	allByAggr := map[aggrKey][]volumeLabelCandidate{}
	for _, cands := range volIndex {
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
		// pickOwner over allByAggr[k] is a superset of this claim's own
		// candidates for the same (cluster, aggr), so no per-claim fallback is
		// needed — and none may exist: `hits` iterates in map order, so a
		// fallback would make labels.node arrival-order dependent (D6).
		labels := map[string]string{"ontap_cluster": hit.oc}
		// The owner key is set only when it resolves non-empty — same rule as
		// the PVC volumename/svm labels. An owner-less aggregate falls back to
		// the storage-cluster compound group in the serialiser.
		if owner := pickOwner(allByAggr[k], hit.oc, hit.aggr); owner != "" {
			labels["node"] = owner
		}
		n := &graph.NetAppAggrNode{
			IDValue:     graph.NetAppAggrID(hit.oc, hit.aggr),
			NameValue:   hit.aggr,
			LabelsValue: labels,
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

func indexQoSFamily(dst map[string][]qosCandidate, vec model.Vector, fam ioFamily) {
	for _, s := range vec {
		vn := string(s.Metric["volume_name"])
		oc := string(s.Metric["cluster"])
		if vn == "" || oc == "" {
			continue
		}
		dst[vn] = append(dst[vn], qosCandidate{
			ontapCluster: oc,
			svm:          string(s.Metric["svm"]),
			policyGroup:  string(s.Metric["policy_group"]),
			family:       fam,
			value:        float64(s.Value),
		})
	}
}

// indexPolicyCeilings keys the hop-C vectors by (ontap cluster, svm, policy).
// The policy's identity label is read as `name` with a `policy_group` fallback:
// Harvest names it differently across templates, and the spec pins the join
// key's SHAPE, not the label's spelling. Smallest numeric value wins on a
// duplicate, mirroring the usage rule.
func indexPolicyCeilings(maxIOPS, maxMBps model.Vector) map[policyKey]*graph.IOMetrics {
	smallest := func(vec model.Vector) map[policyKey]float64 {
		out := map[policyKey]float64{}
		seen := map[policyKey]bool{}
		for _, s := range vec {
			oc, svm := string(s.Metric["cluster"]), string(s.Metric["svm"])
			policy := string(s.Metric["name"])
			if policy == "" {
				policy = string(s.Metric["policy_group"])
			}
			if oc == "" || policy == "" {
				continue
			}
			k := policyKey{oc, svm, policy}
			v := float64(s.Value)
			if !seen[k] || v < out[k] {
				out[k] = v
				seen[k] = true
			}
		}
		return out
	}
	iops := smallest(maxIOPS)
	mbps := smallest(maxMBps)
	keys := map[policyKey]struct{}{}
	for k := range iops {
		keys[k] = struct{}{}
	}
	for k := range mbps {
		keys[k] = struct{}{}
	}
	out := make(map[policyKey]*graph.IOMetrics, len(keys))
	for k := range keys {
		c := &graph.IOMetrics{}
		if v, ok := iops[k]; ok {
			vv := v
			c.MaxIOPS = &vv
		}
		if v, ok := mbps[k]; ok {
			vv := v * bytesPerMB
			c.MaxBytesPerSec = &vv
		}
		out[k] = c
	}
	return out
}

// applyCeiling copies the resolved ceiling onto io. A zero policyKey (the
// volume is in no QoS policy group) or a policy with no fixed-policy series
// leaves both fields absent — absence means "no declared ceiling" and is never
// rendered as a number.
func applyCeiling(io *graph.IOMetrics, index map[policyKey]*graph.IOMetrics, k policyKey) {
	if k.policy == "" {
		return
	}
	c, ok := index[k]
	if !ok {
		return
	}
	io.MaxIOPS = c.MaxIOPS
	io.MaxBytesPerSec = c.MaxBytesPerSec
}

func pickAggr(cands []volumeLabelCandidate) (oc, aggr string) {
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

func pickOwner(cands []volumeLabelCandidate, oc, aggr string) string {
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

func pickSVM(cands []volumeLabelCandidate) string {
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

// qosInScope keeps a QoS candidate that belongs to the volume the edge was
// drawn for. The ONTAP cluster must match the aggregate's — a volume_name
// colliding across two filers sharing one VictoriaMetrics would otherwise be
// double-counted onto this edge. The svm must match too when BOTH sides
// resolved one; a candidate with no svm label still measures the volume and is
// kept (it simply cannot contribute a policy key).
func qosInScope(c qosCandidate, oc, svm string) bool {
	if c.ontapCluster != oc {
		return false
	}
	return svm == "" || c.svm == "" || c.svm == svm
}

// pickPolicy resolves the hop-C join key: the lexically-smallest non-empty
// (ontap cluster, svm, policy_group) triple across the in-scope QoS
// candidates, so a volume observed under two policy groups inside the window
// collapses deterministically.
func pickPolicy(cands []qosCandidate, oc, svm string) policyKey {
	var best policyKey
	for _, c := range cands {
		if !qosInScope(c, oc, svm) || c.policyGroup == "" || c.svm == "" {
			continue
		}
		k := policyKey{c.ontapCluster, c.svm, c.policyGroup}
		if best.policy == "" || k.svm < best.svm || (k.svm == best.svm && k.policy < best.policy) {
			best = k
		}
	}
	return best
}

// sumQoSIO sums each I/O family over the in-scope QoS candidates for one
// claim. nil means no family matched at all — the edge is still emitted, it
// simply carries no metrics, and the claim counts toward netapp_qos_join_miss.
func sumQoSIO(cands []qosCandidate, oc, svm string) *graph.IOMetrics {
	var readOps, writeOps, readLat, writeLat, readData, writeData []float64
	for _, c := range cands {
		if !qosInScope(c, oc, svm) {
			continue
		}
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
func resolvePVCUsage(used, capacity model.Vector, mc missingClusterCounts) map[pvcKey]*graph.UsageBytes {
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
	for _, s := range capacity {
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
