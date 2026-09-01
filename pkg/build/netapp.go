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
// aggr/node dimension, which is exactly why hop A exists. It DOES carry the
// policy group: `volume_labels` states no policy identity, so this is the only
// upstream statement of which QoS policy governs this FlexVol, and hop C's key
// takes that one component from here (design.md D9).
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
//	hop B  qos_* workload families   → the six measured I/O figures
//	hop C  qos_policy_fixed_max_*    → the volume's declared throughput ceiling
//
// The hops degrade independently: a hop-B miss leaves a valid measurement-less
// edge rather than erasing the claim's storage topology. Pure except for the
// two aggregated coverage warnings (D8).
func resolveNetAppStorage(
	claims []pvcVolume,
	rw *VolumeKeyRewriter,
	volumeLabels model.Vector,
	readOps, writeOps, readLat, writeLat, readData, writeData model.Vector,
	policyMaxIOPS, policyMaxMBps model.Vector,
	aggrStatus, aggrUsed, aggrTotal, nodeStatus model.Vector,
) netappResult {
	out := netappResult{svmByPVC: map[string]string{}}

	// One pass over the Harvest vector resolves every claim. The join is
	// derive-then-match, not equality: ONTAP volume names admit no `-`, so a
	// `volume` value can never equal a `pvc-<uuid>` PV name. matcher answers
	// which claims a given FlexVol name matches (hash-indexed for the default
	// suffix mode — see volumeMatcher).
	matcher := newVolumeMatcher(rw, claims)
	volIndex := map[string][]volumeLabelCandidate{}
	candsByClaim := make([][]volumeLabelCandidate, len(claims))
	volsByClaim := make([][]string, len(claims))
	volSeenByClaim := make([]map[string]bool, len(claims))
	var matched []int
	for _, s := range volumeLabels {
		vol, oc := string(s.Metric[promql.HarvestVolumeLabel]), string(s.Metric["cluster"])
		if vol == "" || oc == "" {
			continue
		}
		cand := volumeLabelCandidate{
			ontapCluster: oc,
			node:         string(s.Metric["node"]),
			aggr:         string(s.Metric["aggr"]),
			svm:          string(s.Metric["svm"]),
		}
		volIndex[vol] = append(volIndex[vol], cand)
		matched = matcher.match(vol, matched)
		for _, ci := range matched {
			candsByClaim[ci] = append(candsByClaim[ci], cand)
			if volSeenByClaim[ci] == nil {
				volSeenByClaim[ci] = map[string]bool{}
			}
			if !volSeenByClaim[ci][vol] {
				volSeenByClaim[ci][vol] = true
				volsByClaim[ci] = append(volsByClaim[ci], vol)
			}
		}
	}
	// A claim matching several FlexVol names gathers its QoS candidates in
	// sorted name order, so the float summation order — and therefore the last
	// bits of every I/O figure — is a pure function of the matched set rather
	// than of upstream vector order (D6).
	for i := range volsByClaim {
		sort.Strings(volsByClaim[i])
	}

	// Keyed by the same stock `volume` label the topology family carries, so a
	// claim looks its QoS candidates up through the FlexVol names it matched.
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
	// rather than the built indexes is deliberate: a vector whose series match
	// no claim WAS read, and a derivation that does not fit the estate's
	// FlexVol naming is exactly the coverage failure these signals exist to
	// surface.
	topoPresent := len(volumeLabels) > 0
	// Under the scoped read this is exactly "at least one issued chunk of at
	// least one QoS family returned series": a build whose scope was empty
	// issued no QoS query at all, so every vector is empty and no I/O-coverage
	// warning fires — which is right, since an empty scope means hop A drew no
	// edge for hop B to measure.
	qosPresent := len(readOps)+len(writeOps)+len(readLat)+len(writeLat)+len(readData)+len(writeData) > 0

	type joinHit struct {
		oc, aggr string
		io       *graph.IOMetrics
	}
	hits := map[string]joinHit{} // pvcID → pick
	topoMisses, qosMisses := 0, 0

	for i, c := range claims {
		cands := candsByClaim[i]
		oc, aggr := pickAggr(cands)
		// The aggregate pick runs FIRST so the SVM pick can be scoped to the
		// filer it landed on: both the QoS scope test and the hop-C ceiling key
		// pair the two, and a cross-filer mismatch would silently attach a
		// foreign tenant's ceiling.
		svm := pickSVM(cands, oc)
		if svm != "" {
			out.svmByPVC[c.id] = svm
		}
		if oc == "" || aggr == "" {
			if topoPresent {
				topoMisses++
			}
			continue
		}
		qcands := qosCandidatesFor(volsByClaim[i], qosIndex)
		io := sumQoSIO(qcands, oc, svm)
		if io == nil {
			if qosPresent {
				qosMisses++
			}
		} else {
			// The ceiling key is assembled from BOTH topology hops: hop A owns
			// the filer and the SVM (so it follows the aggregate this edge
			// points at), hop B owns the policy group (the only upstream
			// statement of which policy governs this FlexVol). Attaching it
			// only inside this branch is therefore belt-and-braces: the policy
			// rides on a matched workload series, so a ceiling cannot exist
			// without a measurement in the first place (design.md D9).
			applyCeiling(io, policyIndex, policyKey{oc, svm, pickPolicy(qcands, oc, svm)})
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

// qosCandidatesFor gathers the QoS candidates of every FlexVol name a claim
// matched, concatenated in the caller's (sorted) name order so the summation
// downstream is order-free.
func qosCandidatesFor(volumes []string, index map[string][]qosCandidate) []qosCandidate {
	if len(volumes) == 1 {
		return index[volumes[0]]
	}
	var out []qosCandidate
	for _, v := range volumes {
		out = append(out, index[v]...)
	}
	return out
}

func indexQoSFamily(dst map[string][]qosCandidate, vec model.Vector, fam ioFamily) {
	for _, s := range vec {
		vol := string(s.Metric[promql.HarvestVolumeLabel])
		oc := string(s.Metric["cluster"])
		if vol == "" || oc == "" {
			continue
		}
		dst[vol] = append(dst[vol], qosCandidate{
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
			if oc == "" || svm == "" || policy == "" {
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

// applyCeiling copies the resolved ceiling onto io. An incomplete key (hop A
// resolved no serving SVM, or no in-scope workload carried a policy group) or a
// triple with no fixed-policy series leaves both fields absent — absence means
// "no declared ceiling" and is never rendered as a number.
//
// The floats are copied, not aliased: one index entry now serves EVERY claim in
// the SVM (D9 widened the key from a policy group to the pair), so handing out
// the index's own pointers would make all of them one shared mutable cell.
func applyCeiling(io *graph.IOMetrics, index map[policyKey]*graph.IOMetrics, k policyKey) {
	// An incomplete key is IGNORED, never widened: no hop-A svm, or no policy
	// group on any in-scope workload, leaves the ceiling absent rather than
	// borrowing another policy group's figure from the same SVM (design.md D9).
	if k.svm == "" || k.policy == "" {
		return
	}
	c, ok := index[k]
	if !ok {
		return
	}
	if c.MaxIOPS != nil {
		v := *c.MaxIOPS
		io.MaxIOPS = &v
	}
	if c.MaxBytesPerSec != nil {
		v := *c.MaxBytesPerSec
		io.MaxBytesPerSec = &v
	}
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

// pickSVM resolves the claim's serving SVM: the lexically-smallest non-empty
// `svm` among the matched volume_labels candidates, scoped to the ONTAP cluster
// the aggregate pick landed on.
//
// The scope is load-bearing. A derived token can match same-named FlexVols on
// two filers sharing one VictoriaMetrics (the fifth blind spot in
// docs/netapp-harvest-preconditions.md), and the resolved SVM is paired with
// the picked aggregate's ONTAP cluster twice over: qosInScope rejects every
// workload whose svm differs, and applyCeiling keys the ceiling on the
// (ontap cluster, svm) pair. An unscoped pick could hand the other filer's SVM
// to both — losing the edge's I/O outright, and attaching an unrelated tenant's
// ceiling if the picked filer happens to host a same-named SVM.
//
// An empty oc means no aggregate resolved (the FlexGroup shape, or no candidate
// carrying both labels): there is then neither an edge nor a ceiling, so there
// is no filer to scope to and the pick stays over every candidate — a FlexGroup
// claim still gains its `svm` label.
func pickSVM(cands []volumeLabelCandidate, oc string) string {
	var svm string
	for _, c := range cands {
		if c.svm == "" || (oc != "" && c.ontapCluster != oc) {
			continue
		}
		if svm == "" || c.svm < svm {
			svm = c.svm
		}
	}
	return svm
}

// pickPolicy resolves the hop-B component of the ceiling key: the
// lexically-smallest non-empty `policy_group` across the claim's in-scope QoS
// candidates, so a volume observed under two policy groups inside the window
// resolves deterministically. Unlike hop A's cluster and svm, the policy group
// exists ONLY on these series — volume_labels carries no policy identity.
//
// A candidate with no `svm` label of its own still contributes: the key's svm
// comes from hop A, so such a series measures the edge AND reaches hop C
// (design.md D9). An empty result leaves the key incomplete, and applyCeiling
// then attaches nothing.
func pickPolicy(cands []qosCandidate, oc, svm string) string {
	var best string
	for _, c := range cands {
		if !qosInScope(c, oc, svm) || c.policyGroup == "" {
			continue
		}
		if best == "" || c.policyGroup < best {
			best = c.policyGroup
		}
	}
	return best
}

// qosInScope keeps a QoS candidate that belongs to the volume the edge was
// drawn for. The ONTAP cluster must match the aggregate's — a FlexVol name
// colliding across two filers sharing one VictoriaMetrics would otherwise be
// double-counted onto this edge. The svm must match too when BOTH sides
// resolved one; a candidate with no svm label still measures the volume and is
// kept — and, since D9 takes the ceiling key's svm from hop A, such a candidate
// can still contribute its policy group.
func qosInScope(c qosCandidate, oc, svm string) bool {
	if c.ontapCluster != oc {
		return false
	}
	return svm == "" || c.svm == "" || c.svm == svm
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
func resolvePVCUsage(used, capacity model.Vector, mc *clusterResolver) map[pvcKey]*graph.UsageBytes {
	usedBy := map[pvcKey]float64{}
	usedSeen := map[pvcKey]bool{}
	for _, s := range used {
		cluster := mc.bucket(promql.QKubeletVolumeUsedBytes, s.Metric)
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
		cluster := mc.bucket(promql.QKubeletVolumeCapacityBytes, s.Metric)
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
