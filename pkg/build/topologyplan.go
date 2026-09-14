package build

import (
	"slices"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// topologyLeg is one first-wave query of the topology fan-out: the family, the
// slot its vector lands in, and whether a query error degrades or fails the
// build. One table drives both the launch and the RawSeriesCount tally, so a
// leg cannot be issued without being counted, or counted without being issued.
type topologyLeg struct {
	query promql.Query
	dst   *model.Vector
	// optional legs log a query error and continue with an empty vector;
	// required legs fail the build. Caller cancellation fails either.
	optional bool
	// degraded, when non-nil, is set iff an optional leg's query error was
	// swallowed — for the one reader that infers something from a family's
	// absence (see topologyVectors.JobAnnotationsDegraded).
	degraded *bool
}

// topologyLegs is the first wave of the topology fan-out. The QoS workload
// families, and the pod families under a pod-scoping plan, are second waves
// and are not listed here (readScopedQoS, readScopedPods).
func topologyLegs(v *topologyVectors) []topologyLeg {
	return []topologyLeg{
		{query: promql.QPodInfo, dst: &v.Pod},
		{query: promql.QNodeInfo, dst: &v.Node},
		{query: promql.QNodeAddresses, dst: &v.Addr},
		{query: promql.QPVCBindings, dst: &v.PVC},
		{query: promql.QNodeLabels, dst: &v.NodeLabels},
		{query: promql.QServiceInfo, dst: &v.Service},
		{query: promql.QEndpointSliceEndpoints, dst: &v.EpEndpoints},
		{query: promql.QEndpointSliceLabels, dst: &v.EpLabels},
		{query: promql.QPodOwner, dst: &v.PodOwner},
		{query: promql.QReplicaSetOwner, dst: &v.ReplicaSetOwner},
		{query: promql.QPVCInfo, dst: &v.PVCInfo},
		{query: promql.QVolumeLabels, dst: &v.VolumeLabels, optional: true},
		// kube_pod_container_info degrades rather than fails
		// (harden-topology-read-cardinality D1). Its cardinality MULTIPLIES with
		// the live object count — one series per container per image variant,
		// and, read over the whole window, one per pod that existed at any
		// instant of it — so it is the largest kube-state-metrics family in any
		// estate and the first to meet a memory-derived upstream series limit.
		// What it feeds, data.containers, is presentation; losing it is
		// subtractive.
		{query: promql.QPodContainerInfo, dst: &v.PodContainerInfo, optional: true},
		{query: promql.QNodeStatusCondition, dst: &v.NodeStatus},
		{query: promql.QServiceAnnotations, dst: &v.ServiceAnnotations},
		{query: promql.QPVCAnnotations, dst: &v.PVCAnnotations},
		// The four live-object-count controller-annotation families and
		// kube_job_owner fail the build on a query error: an upstream fault is
		// rare and fail-fast is the right response. kube_replicaset_annotations
		// and kube_job_annotations degrade — their cardinality accumulates with
		// history (revisionHistoryLimit / Job history limits) and can exceed an
		// upstream series limit in an otherwise ordinary estate; losing an
		// `application` string is never worth failing the whole graph
		// (harden-controller-annotation-legs D3).
		{query: promql.QJobOwner, dst: &v.JobOwner},
		{query: promql.QDeploymentAnnotations, dst: &v.DeploymentAnnotations},
		{query: promql.QStatefulSetAnnotations, dst: &v.StatefulSetAnnotations},
		{query: promql.QDaemonSetAnnotations, dst: &v.DaemonSetAnnotations},
		{query: promql.QReplicaSetAnnotations, dst: &v.ReplicaSetAnnotations, optional: true},
		{query: promql.QJobAnnotations, dst: &v.JobAnnotations, optional: true, degraded: &v.JobAnnotationsDegraded},
		{query: promql.QCronJobAnnotations, dst: &v.CronJobAnnotations},
		// NetApp Harvest: OPTIONAL, so a non-NetApp deployment builds cleanly.
		{query: promql.QQoSPolicyFixedMaxIOPS, dst: &v.QoSPolicyMaxIOPS, optional: true},
		{query: promql.QQoSPolicyFixedMaxMBps, dst: &v.QoSPolicyMaxMBps, optional: true},
		{query: promql.QAggrStatus, dst: &v.AggrStatus, optional: true},
		{query: promql.QAggrSpaceUsed, dst: &v.AggrSpaceUsed, optional: true},
		{query: promql.QAggrSpaceTotal, dst: &v.AggrSpaceTotal, optional: true},
		{query: promql.QNetAppNodeStatus, dst: &v.NetAppNodeStatus, optional: true},
		{query: promql.QNetAppNodeLabels, dst: &v.NetAppNodeLabels, optional: true},
		{query: promql.QNetAppNodeCPUBusy, dst: &v.NetAppNodeCPUBusy, optional: true},
		{query: promql.QNetAppNodeTotalOps, dst: &v.NetAppNodeTotalOps, optional: true},
		{query: promql.QNetAppNodeTotalLatency, dst: &v.NetAppNodeTotalLatency, optional: true},
		{query: promql.QNetAppNodeTotalData, dst: &v.NetAppNodeTotalData, optional: true},
		// The alert overlay. Routed through FamilyAlerts, so a table serving that
		// family on no backend issues nothing and every node stays alert-less —
		// the documented normal state, not a degrade.
		{query: promql.QAlerts, dst: &v.Alerts, optional: true},
		{query: promql.QKubeletVolumeUsedBytes, dst: &v.KubeletVolumeUsed, optional: true},
		{query: promql.QKubeletVolumeCapacityBytes, dst: &v.KubeletVolumeCapacity, optional: true},
	}
}

// topologyPlan is the endpoint-dependent part of a topology read: which
// first-wave legs it issues at all, and whether it reads the pod families by
// reference. /v1/graph reads everything (fullPlan); /v1/storage-graph reads
// only what its body can draw (storagePlan).
type topologyPlan struct {
	// skip names first-wave legs this read never issues. A skipped leg is
	// neither launched nor tallied, and its topologyVectors slot stays nil —
	// the state parseTopology already handles for a degraded optional leg.
	skip map[promql.Query]bool
	// scopePods reads kube_pod_info and kube_pod_owner BY REFERENCE,
	// restricted to the pods a claim-binding series names plus podRoots
	// (readScopedPods). False reads both unscoped, in the first wave.
	scopePods bool
	// podRoots are the pod-name segments of the request's pod=<ns>/<name>
	// roots, sorted. A pod root mounting no claim is drawable only if its pod
	// is read, so the roots must reach the scope.
	podRoots []string
}

// fullPlan is the /v1/graph read: every leg, every pod.
var fullPlan = topologyPlan{}

// storageSkippedLegs are the families a storage-flow body cannot carry. The
// four service-side families feed only the service-graph resolver, which the
// storage build never runs; the container family feeds only data.containers,
// which the storage body does not carry. Skipping them is output-preserving for
// everything the body DOES carry — pinned by
// TestBuildStorage_PlanIsOutputPreserving.
var storageSkippedLegs = map[promql.Query]bool{
	promql.QPodContainerInfo:       true,
	promql.QServiceInfo:            true,
	promql.QEndpointSliceEndpoints: true,
	promql.QEndpointSliceLabels:    true,
	promql.QServiceAnnotations:     true,
}

// storagePlan is the /v1/storage-graph read for one request's roots.
func storagePlan(roots graph.StorageRoots) topologyPlan {
	names := make([]string, 0, len(roots.Pods))
	for ref := range roots.Pods {
		names = append(names, ref.Name)
	}
	slices.Sort(names) // map order must not reach the scope
	return topologyPlan{skip: storageSkippedLegs, scopePods: true, podRoots: names}
}

// issuesFirstWave reports whether the plan launches q in the first wave.
func (p topologyPlan) issuesFirstWave(q promql.Query) bool {
	if p.skip[q] {
		return false
	}
	return !p.scopePods || !slices.Contains(promql.PodScopedQueries, q)
}

// tallySeries is RawSeriesCount for one read: one entry per family the read
// ISSUED, and none for a family it did not. A second wave whose scope came out
// empty issued nothing, so its families are absent too — 0 means "read,
// matched nothing", and a family never read has no count to report.
func tallySeries(legs []topologyLeg, plan topologyPlan, v *topologyVectors) map[string]int {
	raw := make(map[string]int, len(legs)+len(promql.QoSWorkloadQueries))
	for _, l := range legs {
		if plan.issuesFirstWave(l.query) {
			raw[string(l.query)] = len(*l.dst)
		}
	}
	if v.QoSScopeIssued {
		for _, t := range qosTargets(v) {
			raw[string(t.query)] = len(*t.dst)
		}
	}
	if v.PodScopeIssued {
		for _, t := range podTargets(v) {
			raw[string(t.query)] = len(*t.dst)
		}
	}
	return raw
}

// largestLeg names the family that returned the most series in one read — the
// one an upstream series limit will reject first. Ties break on the
// lexically-smallest name, so the log line it feeds is deterministic.
func largestLeg(counts map[string]int) (string, int) {
	name, n := "", 0
	for k, c := range counts {
		if name == "" || c > n || (c == n && k < name) {
			name, n = k, c
		}
	}
	return name, n
}
