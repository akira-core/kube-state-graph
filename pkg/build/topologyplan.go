package build

import (
	"cmp"
	"maps"
	"slices"
	"time"

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

// topologyLegs is the first wave of the topology fan-out under fullPlan. The
// QoS workload six are always a second wave (readScopedQoS); under a
// by-reference plan (storagePlan) the two pod families, the four kube_node_*
// families and the eight controller-owner / controller-annotation families
// are ALSO withdrawn from this first-wave list and become three more second
// waves (readScopedPods, readScopedNodes, readScopedControllers) — none of
// the fourteen are listed here in that case. issuesFirstWave is what decides,
// per plan, which of the legs below actually launch; a family a plan does not
// launch here has no topologyVectors slot written until its own second wave
// runs (or stays nil if its scope came out empty).
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
// first-wave legs it issues at all, and whether it reads the pod / node /
// controller families by reference. /v1/graph reads everything (fullPlan);
// /v1/storage-graph reads only what its body can draw (storagePlan).
type topologyPlan struct {
	// kind and pods are the storage request's one root. fullPlan leaves them
	// zero. pods is the pod kind's refs, sorted and de-duplicated; every other
	// kind's values live in the field the seed that reads them consults (the
	// volume*, nodeRoots, claimRoots and applicationRoots fields below).
	kind graph.StorageRootKind
	pods []graph.PodRef
	// skip names first-wave legs this read never issues. A skipped leg is
	// neither launched nor tallied, and its topologyVectors slot stays nil —
	// the state parseTopology already handles for a degraded optional leg.
	skip map[promql.Query]bool
	// byReference reads every promql.ReferenceScopedQueries family BY
	// REFERENCE instead of in the first wave: kube_pod_info / kube_pod_owner
	// restricted to the pods a claim-binding series names plus the pod roots
	// (readScopedPods); the four kube_node_* families restricted to those
	// pods' nodes plus nodeRoots (readScopedNodes); and the eight
	// controller-owner / controller-annotation families restricted to those
	// pods' resolved owners (readScopedControllers). One bit governs all
	// three waves together — pods, nodes and controllers are by-reference or
	// not as a unit, since a plan that scoped controllers over an
	// unrestricted pod read would derive a scope from the whole estate.
	// False reads every one of them unscoped, in the first wave.
	byReference bool
	// nodeRoots are the request's node=<name> roots, sorted. A node root
	// naming a Kubernetes node no loaded pod runs on is drawable only if the
	// node families are read for it, so the roots must reach the node scope
	// exactly as the pod roots reach the pod scope.
	nodeRoots []string
	// volumeClusters, volumeAggrs and volumeSVMs are the request's
	// ontap_cluster=, aggr= and svm= roots, sorted and de-duplicated. harvestSeed
	// is the decision; these slices are the values phase 1 renders.
	volumeClusters []string
	volumeAggrs    []string
	volumeSVMs     []string
	// volumeAggrPairs and volumeSVMPairs are the QUALIFIED `aggr=` / `svm=`
	// values (`<ontap_cluster>/<name>`), keyed by ONTAP cluster with each name set
	// sorted and de-duplicated. volumeAggrs / volumeSVMs hold the bare values
	// (that name on every filer); the two forms are OR-combined.
	volumeAggrPairs map[string][]string
	volumeSVMPairs  map[string][]string
	// volumeNodes are an ontap_node root's controller names. Phase 1 restricts
	// volume_labels on `node`; the claim source is then the aggregates whose
	// owner vote lands on one of these names.
	volumeNodes []string
	// claimRoots are the request's pvc=<namespace>/<claim> roots, sorted, and
	// volumeRoots its pv=<name> roots, sorted and de-duplicated. Either non-empty
	// makes the plan claim-seeded (claimSeeded): the seed IS the claim-info read,
	// restricted on them.
	claimRoots  []graph.ClaimRef
	volumeRoots []string
	// applicationRoots are the request's application=<name> values, sorted.
	// Non-empty launches the recovery wave and narrows the pod scope to the
	// pods related to those Applications (see appscope.go). The volume-label
	// restriction ignores them: an application root is workload-side, and the
	// projection ANDs it with the storage roots.
	applicationRoots []string
	// failClosed makes a query error of every family but ALERTS fail the
	// build, whatever error class /v1/graph gives that family
	// (fail-storage-graph-on-any-leg-error). The storage body's subject IS the
	// Harvest and kubelet data /v1/graph treats as optional decoration, so a
	// silently missing family would render as a smaller, plausible, wrong
	// estate. It governs the legs both plans share — the first wave and the
	// scoped QoS read; every wave only a by-reference plan issues (pods,
	// nodes, controllers, the application recovery, the rooted volume-label
	// read) fails closed unconditionally.
	failClosed bool

	// phaseOne is the Harvest seed's phase-1 read, rendered, in (group, chunk)
	// order. Non-empty means this build reads volume_labels from the root and
	// the claim families from those rows. It is filled by prepareHarvestSeed
	// before the fan-out launches: the render is a pure function of the roots
	// and the byte budget, so a seed past the cap is rejected before any query.
	phaseOne []rootedVolumeLabelsQuery
	resolved bool
	// nodeSeed is the rendered kube_pod_info{node} queries of a node root.
	// Empty unless prepareNodeSeed rendered a seed that fits the cap.
	nodeSeed     []string
	nodePrepared bool
	// podSeed is the rendered bindings{namespace,pod} queries of a pod root.
	// Empty unless preparePodSeed rendered a seed that fits the cap.
	podSeed          []string
	podPrepared      bool
	appPrepared      bool
	flowlessPrepared bool
	claimPrepared    bool
}

// failsClosed reports whether a query error of q must fail this build even
// though its family's default class would degrade. ALERTS is the one
// exception: it only feeds data.alerts and the status fold, and coupling the
// storage view to the alert store's availability would buy nothing.
func (p topologyPlan) failsClosed(q promql.Query) bool {
	return p.failClosed && q != promql.QAlerts
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

// storagePlan is the /v1/storage-graph read for one request's root kind.
//
// It maps the one kind onto the plan fields the read consults. ontap_cluster,
// aggr, svm and ontap_node seed volume_labels; node and pod seed their claim
// bindings; pvc and pv seed the claim-info read itself (claimSeeded); application still reads the claim side from the zone until its
// seed replaces that.
func storagePlan(roots graph.StorageRoots) topologyPlan {
	plan := topologyPlan{
		kind:        roots.Kind,
		pods:        slices.Clone(roots.Pods),
		skip:        storageSkippedLegs,
		byReference: true,
		failClosed:  true,
	}
	// sortedNames drops empty values, which is what keeps harvestSeed and the
	// renderer in agreement: the renderer normalises too, so a plan carrying
	// only empty values would answer "seeded" to a query that cannot be
	// rendered. graph.NewStorageScope already drops them for an HTTP caller,
	// but pkg/build is an importable engine and StorageRoots is an exported
	// value an embedder fills itself.
	switch roots.Kind {
	case graph.StorageRootONTAPCluster:
		plan.volumeClusters = sortedNames(roots.Names)
	case graph.StorageRootAggr:
		plan.volumeAggrs = sortedNames(roots.Names)
		plan.volumeAggrPairs = qualifiedPairs(roots.Qualified)
	case graph.StorageRootSVM:
		plan.volumeSVMs = sortedNames(roots.Names)
		plan.volumeSVMPairs = qualifiedPairs(roots.Qualified)
	case graph.StorageRootONTAPNode:
		plan.volumeNodes = sortedNames(roots.Names)
	case graph.StorageRootNode:
		plan.nodeRoots = sortedNames(roots.Names)
	case graph.StorageRootPod:
		// plan.pods carries the (namespace, pod) refs the pod seed and the pod
		// wave read; nothing more to derive.
	case graph.StorageRootPVC:
		plan.claimRoots = usableClaimRefs(roots.Claims)
	case graph.StorageRootPV:
		plan.volumeRoots = sortedNames(roots.Names)
	case graph.StorageRootApplication:
		plan.applicationRoots = sortedNames(roots.Names)
	}
	return plan
}

// qualifiedPairs groups qualified `aggr=` / `svm=` refs by ONTAP cluster, each
// name set sorted and de-duplicated. A ref with an empty half can match nothing
// a query could name, and an embedder fills StorageRoots itself, so it is
// dropped here rather than left for the renderer. Nil when nothing is left.
func qualifiedPairs(refs []graph.ONTAPRef) map[string][]string {
	out := map[string][]string{}
	for _, r := range refs {
		if r.ONTAPCluster != "" && r.Name != "" {
			out[r.ONTAPCluster] = append(out[r.ONTAPCluster], r.Name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return compactPairs(out)
}

// usableClaimRefs is the sorted claim refs that name both a namespace and a
// claim. A ref with an empty half can match nothing a query could name, and an
// embedder fills StorageRoots itself, so the plan drops it instead of asking
// the renderer to.
func usableClaimRefs(refs []graph.ClaimRef) []graph.ClaimRef {
	out := make([]graph.ClaimRef, 0, len(refs))
	for _, r := range refs {
		if r.Namespace != "" && r.Name != "" {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b graph.ClaimRef) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return slices.Compact(out)
}

// harvestSeed reports whether this storage build's volume_labels read is the
// root's own phase 1. storagePlan sets the volume slices for an ontap_cluster,
// aggr, or svm root and for nothing else, so fullPlan and a workload root
// answer false. The slices are the decision, not the kind alone: a caller can
// clear them to read the family whole while keeping the same root for projection.
func (p topologyPlan) harvestSeed() bool {
	return p.byReference &&
		len(p.volumeClusters)+len(p.volumeAggrs)+len(p.volumeSVMs)+len(p.volumeNodes)+
			len(p.volumeAggrPairs)+len(p.volumeSVMPairs) > 0
}

// aggrRoots reports whether the request roots any aggregate, by bare name or by
// qualified (ONTAP cluster, aggregate) pair.
func (p topologyPlan) aggrRoots() bool {
	return len(p.volumeAggrs)+len(p.volumeAggrPairs) > 0
}

// svmRoots reports whether the request roots any SVM, bare or qualified.
func (p topologyPlan) svmRoots() bool {
	return len(p.volumeSVMs)+len(p.volumeSVMPairs) > 0
}

// claimSeeded reports whether this storage build's seed is the claim-info read
// itself: a pvc or pv root. Like harvestSeed, the slices are the decision rather
// than the kind alone, so a caller can clear them to read the inventory across
// the zone while keeping the same root for projection.
func (p topologyPlan) claimSeeded() bool {
	return p.byReference && len(p.claimRoots)+len(p.volumeRoots) > 0
}

// rootedClaims reports whether phase 1 was rendered, so the claim families are
// read from those rows rather than across the zone.
func (p topologyPlan) rootedClaims() bool {
	return len(p.phaseOne) > 0
}

// prepareHarvestSeed renders the Harvest seed's phase 1 under the request's
// az / env matchers. It is a pure function of the plan, the window, the byte
// budget and the request, and it is idempotent: a resolved plan is returned
// as is. A seed that would take more queries than the cap returns
// ReasonInvalidScope and leaves phase 1 empty — the build issues nothing.
func (p topologyPlan) prepareHarvestSeed(window time.Duration, budget int, keys promql.LabelKeys, sel promql.Selector) (topologyPlan, error) {
	if p.resolved {
		return p, nil
	}
	p.resolved = true
	if !p.harvestSeed() {
		return p, nil
	}
	// Every chunk repeats the request matchers, so they come off the budget
	// the way the repeated cluster matcher does inside rootedVolumeLabelsChunks.
	budget -= promql.RequestMatcherCost(promql.QVolumeLabels, keys, sel)
	var queries []rootedVolumeLabelsQuery
	var ok bool
	if len(p.volumeNodes) > 0 {
		queries, ok = rootedNodeLabelChunks(p.volumeNodes, budget)
	} else {
		queries, ok = rootedVolumeLabelsChunks(p.volumeClusters, p.volumeAggrs, p.volumeSVMs, p.volumeAggrPairs, p.volumeSVMPairs, budget)
	}
	for i := range queries {
		if !ok {
			break
		}
		queries[i].rendered, ok = queries[i].render(window, keys, sel)
	}
	if !ok {
		return p, NewError(ReasonInvalidScope, RootScopeCapMessage, nil)
	}
	p.phaseOne = queries
	return p, nil
}

// prepareNodeSeed renders a node root's first read, kube_pod_info restricted
// on node. A seed past the chunk cap returns ReasonInvalidScope before any
// query. Idempotent, like prepareHarvestSeed.
func (p topologyPlan) prepareNodeSeed(window time.Duration, budget int, keys promql.LabelKeys, sel promql.Selector) (topologyPlan, error) {
	if p.nodePrepared {
		return p, nil
	}
	p.nodePrepared = true
	if p.kind != graph.StorageRootNode || len(p.nodeRoots) == 0 {
		return p, nil
	}
	budget -= promql.RequestMatcherCost(promql.QPodInfo, keys, sel)
	if budget < 1 {
		budget = 1
	}
	chunks := promql.ChunkScope(p.nodeRoots, budget)
	if len(chunks) > maxRootedVolumeLabelChunks {
		return p, NewError(ReasonInvalidScope, RootScopeCapMessage, nil)
	}
	for _, chunk := range chunks {
		rendered, ok := promql.RenderPodInfoByNode(window, keys, sel, chunk)
		if !ok {
			continue
		}
		p.nodeSeed = append(p.nodeSeed, rendered)
	}
	return p, nil
}

// prepare resolves every seed of the plan, in the one order both buildStorage
// and readTopology need: each step is idempotent, so the builder can reject a
// capped request before binding a querier and readTopology can re-run it for a
// plan that arrives unresolved. The first rejection is returned as is.
func (p topologyPlan) prepare(window time.Duration, budget int, keys promql.LabelKeys, sel promql.Selector) (topologyPlan, error) {
	steps := []func(topologyPlan) (topologyPlan, error){
		func(p topologyPlan) (topologyPlan, error) { return p.prepareHarvestSeed(window, budget, keys, sel) },
		func(p topologyPlan) (topologyPlan, error) { return p.prepareNodeSeed(window, budget, keys, sel) },
		func(p topologyPlan) (topologyPlan, error) { return p.preparePodSeed(window, budget, keys, sel) },
		func(p topologyPlan) (topologyPlan, error) { return p.prepareClaimSeed(budget, keys, sel) },
		func(p topologyPlan) (topologyPlan, error) { return p.prepareApplicationSeed(budget) },
		func(p topologyPlan) (topologyPlan, error) { return p.prepareFlowless(budget, keys, sel) },
	}
	for _, step := range steps {
		var err error
		if p, err = step(p); err != nil {
			return p, err
		}
	}
	return p, nil
}

// prepareClaimSeed rejects a claim or volume root whose first read would take
// more queries than the cap, before any query. The count comes from the same
// groups and the same per-group byte reserve the read chunks with
// (claimSeedGroups), so the check and the read cannot disagree. Idempotent, like
// prepareNodeSeed.
func (p topologyPlan) prepareClaimSeed(budget int, keys promql.LabelKeys, sel promql.Selector) (topologyPlan, error) {
	if p.claimPrepared {
		return p, nil
	}
	p.claimPrepared = true
	if !p.claimSeeded() {
		return p, nil
	}
	chunks := 0
	for _, g := range p.claimSeedGroups() {
		chunks += len(promql.ChunkScope(g.names, g.budget(budget, keys, sel)))
	}
	if chunks > maxRootedVolumeLabelChunks {
		return p, NewError(ReasonInvalidScope, RootScopeCapMessage, nil)
	}
	return p, nil
}

// preparePodSeed renders a pod root's first read: the claim-binding family
// restricted on namespace and pod. A seed past the chunk cap returns
// ReasonInvalidScope before any query. Idempotent, like prepareNodeSeed.
//
// The namespace matcher is charged whole, once, because every chunk repeats
// it. Pod names are what the byte budget then splits. A chunk renders only
// the namespaces of the refs whose name it carries; the independent
// alternations can still match a cross pair, and the seed drops those rows.
func (p topologyPlan) preparePodSeed(window time.Duration, budget int, keys promql.LabelKeys, sel promql.Selector) (topologyPlan, error) {
	if p.podPrepared {
		return p, nil
	}
	p.podPrepared = true
	if p.kind != graph.StorageRootPod || len(p.pods) == 0 {
		return p, nil
	}
	budget -= promql.RequestMatcherCost(promql.QPVCBindings, keys, sel)
	if nsCost := promql.MatcherCost(promql.NamespaceLabel, podRefNamespaces(p.pods)); nsCost > 0 {
		budget -= nsCost + 1 // the comma before the pod alternation
	}
	if budget < 1 {
		budget = 1
	}
	names := podRefNames(p.pods)
	chunks := promql.ChunkScope(names, budget)
	if len(chunks) > maxRootedVolumeLabelChunks {
		return p, NewError(ReasonInvalidScope, RootScopeCapMessage, nil)
	}
	for _, chunk := range chunks {
		rendered, ok := promql.RenderClaimBindingsByPod(window, keys, sel, namespacesForNames(p.pods, chunk), chunk)
		if !ok {
			continue
		}
		p.podSeed = append(p.podSeed, rendered)
	}
	return p, nil
}

func podRefNames(refs []graph.PodRef) []string {
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref.Name != "" {
			seen[ref.Name] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func podRefNamespaces(refs []graph.PodRef) []string {
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref.Namespace != "" {
			seen[ref.Namespace] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func namespacesForNames(refs []graph.PodRef, names []string) []string {
	want := make(map[string]struct{}, len(names))
	for _, name := range names {
		want[name] = struct{}{}
	}
	seen := make(map[string]struct{})
	for _, ref := range refs {
		if _, ok := want[ref.Name]; ok && ref.Namespace != "" {
			seen[ref.Namespace] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// issuesFirstWave reports whether the plan launches q in the first wave.
//
// A storage plan's first wave is ALERTS alone. Every other family it reads
// hangs off the seed or runs beside it (issuesBesideSeed). fullPlan launches
// every leg it does not skip.
func (p topologyPlan) issuesFirstWave(q promql.Query) bool {
	if p.skip[q] {
		return false
	}
	if p.byReference {
		return q == promql.QAlerts
	}
	return true
}

// tracksByReference reports whether this storage build reads every family but
// ALERTS from the root's reach. A Harvest seed and a workload seed do. A plan
// whose seed slices were cleared does not: that is the parity control, which
// keeps the storage plan's skip set and fail-closed rule while reading the
// inventory across the zone.
func (p topologyPlan) tracksByReference() bool {
	if !p.byReference {
		return false
	}
	if p.harvestSeed() || p.rootedClaims() || p.claimSeeded() {
		return true
	}
	switch p.kind {
	case graph.StorageRootNode:
		return len(p.nodeRoots) > 0
	case graph.StorageRootPod:
		return len(p.pods) > 0
	case graph.StorageRootApplication:
		return len(p.applicationRoots) > 0
	default:
		return false
	}
}

// issuesBesideSeed reports whether a storage build launches q concurrently
// with ALERTS, without waiting on the seed. A tracking build launches only
// the Harvest seed's phase-1 volume_labels read here; everything else hangs
// off the seed or the expansion. A non-tracking storage plan (no root, or the
// parity control with its seed slices cleared) still launches the inventory
// it does not scope. fullPlan launches nothing here.
func (p topologyPlan) issuesBesideSeed(q promql.Query) bool {
	if !p.byReference || p.skip[q] || q == promql.QAlerts {
		return false
	}
	if q == promql.QVolumeLabels && p.harvestSeed() {
		return true
	}
	if p.tracksByReference() {
		return false
	}
	return !slices.Contains(promql.ReferenceScopedQueries, q)
}

// flowlessAggrGauges reports whether aggregate gauges are read for the root
// instead of across the zone. The slices are the decision, so a caller can
// clear them and keep the zone-wide read.
func (p topologyPlan) flowlessAggrGauges() bool {
	if !p.byReference {
		return false
	}
	switch p.kind {
	case graph.StorageRootAggr:
		return p.aggrRoots()
	case graph.StorageRootONTAPCluster:
		return len(p.volumeClusters) > 0
	default:
		return false
	}
}

// flowlessControllers reports whether controller families are read for the
// root, or for the owners of an aggr root's gauges, instead of across the zone.
func (p topologyPlan) flowlessControllers() bool {
	if !p.byReference {
		return false
	}
	switch p.kind {
	case graph.StorageRootAggr:
		return p.aggrRoots()
	case graph.StorageRootONTAPNode:
		return len(p.volumeNodes) > 0
	case graph.StorageRootONTAPCluster:
		return len(p.volumeClusters) > 0
	default:
		return false
	}
}

// prepareFlowless rejects a flowless gauge scope that would take more queries
// than the cap. Idempotent. The check uses the same chunk split the read uses.
func (p topologyPlan) prepareFlowless(budget int, keys promql.LabelKeys, sel promql.Selector) (topologyPlan, error) {
	if p.flowlessPrepared {
		return p, nil
	}
	p.flowlessPrepared = true
	var names []string
	var pairs map[string][]string
	switch {
	case p.kind == graph.StorageRootAggr && p.aggrRoots():
		names, pairs = p.volumeAggrs, p.volumeAggrPairs
	case p.kind == graph.StorageRootONTAPNode && len(p.volumeNodes) > 0:
		names = p.volumeNodes
	case p.kind == graph.StorageRootONTAPCluster && len(p.volumeClusters) > 0:
		names = p.volumeClusters
	default:
		return p, nil
	}
	// The qualified aggregates are read one query per ONTAP cluster beside the
	// bare names, so the cap counts both.
	chunks := len(promql.ChunkHarvestPairs(promql.QAggrStatus, keys, sel, pairs, budget))
	budget -= promql.RequestMatcherCost(promql.QAggrStatus, keys, sel)
	if budget < 1 {
		budget = 1
	}
	if len(names) > 0 {
		chunks += len(promql.ChunkScope(names, budget))
	}
	if chunks > maxRootedVolumeLabelChunks {
		return p, NewError(ReasonInvalidScope, RootScopeCapMessage, nil)
	}
	return p, nil
}

// bindingsFromSeed reports whether this plan's claim bindings come from a
// root seed rather than a beside-seed read of the whole zone.
func (p topologyPlan) bindingsFromSeed() bool {
	switch p.kind {
	case graph.StorageRootNode:
		return len(p.nodeRoots) > 0
	case graph.StorageRootPod:
		return len(p.pods) > 0
	case graph.StorageRootApplication:
		return len(p.applicationRoots) > 0
	default:
		return false
	}
}

// prepareApplicationSeed rejects an application root whose stage-1 tracking-id
// read would exceed the chunk cap. The claim-annotation read uses the same
// split, so one check covers both. Idempotent. The rejection happens before
// any query when the build calls it ahead of the fan-out.
func (p topologyPlan) prepareApplicationSeed(budget int) (topologyPlan, error) {
	if p.appPrepared {
		return p, nil
	}
	p.appPrepared = true
	if p.kind != graph.StorageRootApplication || len(p.applicationRoots) == 0 {
		return p, nil
	}
	if len(applicationRootChunks(p.applicationRoots, budget)) > maxApplicationRootChunks {
		return p, NewError(ReasonInvalidScope, RootScopeCapMessage, nil)
	}
	return p, nil
}

// tallySeries is RawSeriesCount for one read: one entry per family the read
// ISSUED, and none for a family it did not. A second wave whose scope came out
// empty issued nothing, so its families are absent too — 0 means "read,
// matched nothing", and a family never read has no count to report.
func tallySeries(legs []topologyLeg, plan topologyPlan, v *topologyVectors) map[string]int {
	raw := make(map[string]int, len(legs)+len(promql.QoSWorkloadQueries)+len(promql.ReferenceScopedQueries))
	for _, l := range legs {
		// A flowless gauge read is issued beside the first wave but lands
		// through the scoped writer, so it is tallied from ScopeIssued rather
		// than from the beside-seed bit.
		if plan.issuesFirstWave(l.query) || plan.issuesBesideSeed(l.query) || v.ScopeIssued[l.query] {
			raw[string(l.query)] = len(*l.dst)
		}
	}
	for _, targets := range [][]scopedTarget{qosTargets(v), podTargets(v), nodeTargets(v), controllerTargets(v), claimTargets(v)} {
		for _, t := range targets {
			extra, hasExtra := v.ExtraSeriesCount[t.query]
			if !v.ScopeIssued[t.query] && !hasExtra {
				continue
			}
			n := extra
			if v.ScopeIssued[t.query] {
				n += len(*t.dst)
			}
			raw[string(t.query)] = n
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
