package build

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// readK8sWorkload is the whole read of a Kubernetes-only build (k8sPlan) for a
// pod or node root. It replaces, under kubernetesOnly, the node or pod seed,
// readWorkloadClaims and the pod, node, controller and application-metadata
// waves, and writes the same topologyVectors slots, so parseTopology, assemble
// and freeze are untouched.
//
// The storage build's chain waits on each wave in full before the next starts,
// which costs a Kubernetes graph of a node seven sequential upstream rounds. Two
// facts make most of that chain unnecessary:
//
//   - Mounter completion never adds a claim. It keeps only rows whose claim is
//     already tracked, so the tracked set is final once the root pods' bindings
//     are filtered.
//   - A mounter shares its claim's namespace and cluster, because a pod can mount
//     only a claim of its own namespace. So the application-metadata scope is
//     fixed by the root pods and the tracked claims alone.
//
// So the rounds follow real dependencies:
//
//	node root                                              pod root
//	r1  seed kube_pod_info{node}                           r1  bindings || kube_pod_info || kube_pod_owner of the roots
//	r2  incarnation || bindings || pod_owner               r2  (= node root r3)
//	r3  mounter completion || claim families
//	    || app_metadata || kube_node_*
//	    || controllers, first stage
//	r4+ only for mounters no earlier read loaded: their pod_info / pod_owner,
//	    then their controllers and nodes, then any Deployment / CronJob the
//	    first stage revealed.
//
// What a pod, a claim or a controller IS comes from the same helpers the storage
// build uses (podsNewestOn, keepPodBindings, trackedClaimKeys, controllerScope,
// deploymentScope, nodeScope, appMetadataScopesOf), so the two endpoints cannot
// disagree about it; only the order of the reads differs.
//
// It closes nothing itself: readTopology wraps it so bindingsDone, pvcInfoDone
// and pvcAnnotationsDone close on every return path, which is what lets the
// scoped QoS read compute an empty scope and return.
func readK8sWorkload(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	plan topologyPlan,
	v *topologyVectors,
	scopeMu *sync.Mutex,
) (err error) {
	defer recoverScopedPanic(ctx, promql.QPodInfo, &err)
	r := &k8sRead{
		ctx: ctx, q: q, window: window, end: end, opts: opts, sel: sel, plan: plan, v: v, mu: scopeMu,
		lk:        opts.LabelKeys.OrDefault(),
		nodesRead: map[string]struct{}{},
	}
	switch plan.kind {
	case graph.StorageRootNode:
		return r.nodeRoot()
	case graph.StorageRootPod:
		return r.podRoot()
	default:
		// BuildK8s admits only pod and node roots. Any other kind reaching here
		// would skip every seed and wave; fail closed rather than return an
		// empty, plausible body.
		return fmt.Errorf("k8s workload read: unsupported root kind %q", plan.kind)
	}
}

// podSet is a set of pods, keyed the way the root kind identifies a pod. A node
// root identifies one by its full (az, env, cluster, namespace, pod), so a
// same-named pod of another selected cluster, running on another node, is a
// different pod. A pod root names (namespace, pod) in EVERY selected cluster, so
// its pods are identified by that pair alone.
type podSet struct {
	keyed bool
	keys  map[podSeriesKey]struct{}
	refs  map[promql.NamePair]struct{}
}

func newPodSet(keyed bool) *podSet {
	return &podSet{keyed: keyed, keys: map[podSeriesKey]struct{}{}, refs: map[promql.NamePair]struct{}{}}
}

func (s *podSet) has(k podSeriesKey) bool {
	if s.keyed {
		_, ok := s.keys[k]
		return ok
	}
	_, ok := s.refs[promql.NamePair{Namespace: k.namespace, Name: k.pod}]
	return ok
}

func (s *podSet) add(k podSeriesKey) {
	if s.keyed {
		s.keys[k] = struct{}{}
		return
	}
	s.refs[promql.NamePair{Namespace: k.namespace, Name: k.pod}] = struct{}{}
}

func (s *podSet) empty() bool { return len(s.keys)+len(s.refs) == 0 }

func (s *podSet) merge(o *podSet) {
	maps.Copy(s.keys, o.keys)
	maps.Copy(s.refs, o.refs)
}

// pairs is the (namespace, pod) of every pod in the set, sorted.
func (s *podSet) pairs() []promql.NamePair {
	out := slices.Collect(maps.Keys(s.refs))
	for k := range s.keys {
		out = append(out, promql.NamePair{Namespace: k.namespace, Name: k.pod})
	}
	return promql.SortedNamePairs(out)
}

// keepRows keeps the rows of pods in the set, in input order.
func (s *podSet) keepRows(rows model.Vector, keys promql.LabelKeys) model.Vector {
	out := make(model.Vector, 0, len(rows))
	for _, row := range rows {
		if s.has(podSeriesKeyOf(row.Metric, keys)) {
			out = append(out, row)
		}
	}
	return out
}

// namePairsOf is the (namespace, label) of every row naming one.
func namePairsOf(rows model.Vector, label string) []promql.NamePair {
	out := make([]promql.NamePair, 0, len(rows))
	for _, s := range rows {
		if name := string(s.Metric[model.LabelName(label)]); name != "" {
			out = append(out, promql.NamePair{Namespace: string(s.Metric[promql.NamespaceLabel]), Name: name})
		}
	}
	return out
}

// nodeRoot is r1 and r2 of a node root: the seed, then incarnation completion,
// the claim bindings and the pod owners of the seed's pods, issued together. The
// bindings and owners are read on the seed's pairs rather than on the pods that
// turn out to still run on the root, so they do not wait for incarnation
// completion; the rows of a pod whose newest incarnation moved are dropped in
// Go, exactly as the incarnation filter drops its kube_pod_info rows.
func (r *k8sRead) nodeRoot() error {
	onNode, err := instantAll(r.ctx, r.q, promql.QPodInfo, r.end, r.plan.nodeSeed)
	if err != nil {
		return err
	}
	if len(r.plan.nodeSeed) > 0 {
		// The seed read the family even when no pod runs on the root. The pod
		// rows that land in v.Pod are the incarnation completion's, so the
		// seed's own are tallied beside them.
		markScopeIssued(r.v, r.mu, promql.QPodInfo)
		addExtraSeries(r.v, r.mu, promql.QPodInfo, len(onNode))
	}

	loaded := newPodSet(true)
	var rootInfo, rootOwner, bindings model.Vector
	if seed := namePairsOf(onNode, promql.PodLabel); len(seed) > 0 {
		reads := []pairRead{
			{query: promql.QPodInfo, label: promql.PodLabel, pairs: seed},
			{query: promql.QPVCBindings, label: promql.PodLabel, pairs: seed},
			{query: promql.QPodOwner, label: promql.PodLabel, pairs: seed},
		}
		if err := r.issuePairReads(r.ctx, reads); err != nil {
			return err
		}
		incarnation, byPod, owners := reads[0].rows, reads[1].rows, reads[2].rows
		onRoot := podsNewestOn(incarnation, r.plan.nodeRoots, r.lk)
		for k := range onRoot {
			loaded.add(k)
		}
		rootInfo = keepPodRows(incarnation, onRoot, r.lk)
		r.tallyDropped(promql.QPodInfo, len(incarnation)-len(rootInfo))
		rootOwner = keepPodRows(owners, onRoot, r.lk)
		r.tallyDropped(promql.QPodOwner, len(owners)-len(rootOwner))
		// v.PVC is filled by the by-claim read, so every row of this one is
		// tallied beside it.
		r.tallyDropped(promql.QPVCBindings, len(byPod))
		bindings = keepPodBindings(byPod, onRoot, r.lk)
	}
	return r.fromRoots(rootInfo, rootOwner, bindings, loaded)
}

// podRoot is r1 of a pod root: the claim-binding seed and the roots' own
// kube_pod_info and kube_pod_owner, in the seed's chunk split. The split is the
// one preparePodSeed capped, so a root set the cap accepted is never rejected
// by a read issued after a query. All three are restricted on independent
// namespace and name alternations, so a cross pair is dropped in Go; and, as
// ever, a pod root names its pod in every selected cluster.
func (r *k8sRead) podRoot() error {
	chunks := r.plan.podSeedChunks
	reads := []pairRead{
		{query: promql.QPVCBindings, label: promql.PodLabel, chunks: chunks},
		{query: promql.QPodInfo, label: promql.PodLabel, chunks: chunks},
		{query: promql.QPodOwner, label: promql.PodLabel, chunks: chunks},
	}
	if err := r.issuePairReads(r.ctx, reads); err != nil {
		return err
	}
	roots := make([]promql.NamePair, 0, len(r.plan.pods))
	loaded := newPodSet(false)
	for _, ref := range r.plan.pods {
		roots = append(roots, promql.NamePair{Namespace: ref.Namespace, Name: ref.Name})
		loaded.refs[promql.NamePair{Namespace: ref.Namespace, Name: ref.Name}] = struct{}{}
	}
	// v.PVC is filled by the by-claim read, so every row of the seed is tallied
	// beside it.
	r.tallyDropped(promql.QPVCBindings, len(reads[0].rows))
	bindings := keepRootBindings(reads[0].rows, r.plan.pods)
	rootInfo := r.keepRequestedPairs(reads[1], roots)
	rootOwner := r.keepRequestedPairs(reads[2], roots)
	return r.fromRoots(rootInfo, rootOwner, bindings, loaded)
}

// fromRoots is everything after the root pods are known, shared by both root
// kinds: the round that needs the roots alone, then the co-mounters.
func (r *k8sRead) fromRoots(rootInfo, rootOwner, bindings model.Vector, loaded *podSet) error {
	r.v.Pod, r.v.PodOwner = rootInfo, rootOwner
	if err := r.fanOut(trackedClaimKeys(bindings, r.lk), rootInfo, rootOwner); err != nil {
		return err
	}
	return r.completeMounters(loaded)
}

// claimTarget is a claim-keyed family and the slot its tracked rows land in.
type claimTarget struct {
	query promql.Query
	slot  *model.Vector
}

// fanOut is the round that depends on the root pods alone: mounter completion
// and the claim families by the tracked claims, the application-metadata metric
// by the root pods' and the claims' namespaces, the Kubernetes nodes the root
// pods run on, and the first stage of their controllers. Nothing in it waits on
// another member, so they are one round.
func (r *k8sRead) fanOut(tracked map[claimKey]struct{}, rootInfo, rootOwner model.Vector) error {
	targets := []claimTarget{
		{promql.QPVCBindings, &r.v.PVC}, // mounter completion
		{promql.QPVCInfo, &r.v.PVCInfo},
		{promql.QPVCAnnotations, &r.v.PVCAnnotations},
		{promql.QKubeletVolumeUsedBytes, &r.v.KubeletVolumeUsed},
		{promql.QKubeletVolumeCapacityBytes, &r.v.KubeletVolumeCapacity},
	}
	var claimPairs []promql.NamePair
	for k := range tracked {
		claimPairs = append(claimPairs, promql.NamePair{Namespace: k.namespace, Name: k.claim})
	}
	claimPairs = promql.SortedNamePairs(claimPairs)
	var claimReads []pairRead
	if len(claimPairs) > 0 {
		for _, t := range targets {
			claimReads = append(claimReads, pairRead{query: t.query, label: promql.ClaimLabel, pairs: claimPairs})
		}
	}

	scopes := r.appMetadataScopes(rootInfo, tracked)
	nodes := r.nodeReads(rootInfo, r.plan.nodeRoots)
	controllers := r.ctrl.firstStage(r.v, rootOwner)

	var appRows model.Vector
	err := r.parallel(promql.QPodInfo,
		func(ctx context.Context) error { return r.issuePairReads(ctx, claimReads) },
		func(ctx context.Context) (err error) {
			if len(scopes) == 0 {
				return nil
			}
			appRows, err = issueAppMetadata(ctx, r.q, r.window, r.end, r.opts, r.sel, r.v, r.mu, scopes)
			return err
		},
		func(ctx context.Context) error { return r.issueNameReads(ctx, nodes) },
		func(ctx context.Context) error { return r.issueNameReads(ctx, controllers) },
	)
	if err != nil {
		return err
	}

	// A tracked claim is identified by its full key; a same-named claim of
	// another cluster is a cross pair and dropped.
	isTracked := func(m model.Metric) bool {
		_, ok := tracked[claimKeyOf(m, r.lk, string(m[promql.ClaimLabel]))]
		return ok
	}
	for i, read := range claimReads {
		kept := keepRows(read.rows, isTracked)
		r.tallyDropped(read.query, len(read.rows)-len(kept))
		*targets[i].slot = kept
	}
	if len(scopes) > 0 {
		r.v.AppMetadata = appRows
	}
	commitNameReads(nodes)
	commitNameReads(controllers)
	return nil
}

// appMetadataScopes is the namespace restriction of the application-metadata
// read, fixed by the root pods and the tracked claims: a mounter
// shares its claim's cluster and namespace, so no later round can add one.
func (r *k8sRead) appMetadataScopes(rootInfo model.Vector, tracked map[claimKey]struct{}) []appMetadataClusterScope {
	if !appMetadataActive(r.opts.AppMetadata, r.q) {
		return nil
	}
	pairs := appendClusterNamespaces(make([]clusterNamespace, 0, len(rootInfo)+len(tracked)), rootInfo)
	// claimKeyOf already bucketed each tracked claim's cluster.
	for k := range tracked {
		pairs = append(pairs, clusterNamespace{cluster: k.cluster, namespace: k.namespace})
	}
	return appMetadataScopesOf(pairs)
}

// nodeReads is the four kube_node_* families restricted to the Kubernetes nodes
// the pods run on, plus the request's node roots, that no earlier round read.
func (r *k8sRead) nodeReads(pods model.Vector, roots []string) []nameRead {
	var scope []string
	for _, n := range nodeScope(pods, roots) {
		if _, done := r.nodesRead[n]; !done {
			r.nodesRead[n] = struct{}{}
			scope = append(scope, n)
		}
	}
	if len(scope) == 0 {
		return nil
	}
	targets := nodeTargets(r.v)
	reads := make([]nameRead, 0, len(targets))
	for _, t := range targets {
		reads = append(reads, nameRead{query: t.query, slot: t.dst, scope: scope})
	}
	return reads
}

// freshMounters is the pods mounting a tracked claim that no earlier read
// loaded, from the mounter-completion rows in v.PVC.
func (r *k8sRead) freshMounters(loaded *podSet) *podSet {
	fresh := newPodSet(loaded.keyed)
	for _, s := range r.v.PVC {
		if bindingClaim(s.Metric) == "" {
			continue
		}
		if k := podSeriesKeyOf(s.Metric, r.lk); k.pod != "" && !loaded.has(k) {
			fresh.add(k)
		}
	}
	return fresh
}

// completeMounters loads the co-mounters of the tracked claims, and only them.
// A claim mounted by pods the root already loaded costs nothing here: no pod,
// node or controller read is issued for a mounter that is not new, and a name
// an earlier round covered is never read again.
//
//	r4  the new mounters' kube_pod_info / kube_pod_owner
//	    || the second controller stage of the roots' owners
//	r5  the new mounters' controllers, first stage || their nodes
//	r6  the second stage of the new mounters' owners
//
// The slots are appended to in round order. The parse is keyed, so the order of
// rows of different keys cannot change the body (TestParseTopology_IsOrderInsensitiveAcrossKeys).
func (r *k8sRead) completeMounters(loaded *podSet) error {
	fresh := r.freshMounters(loaded)
	second := r.ctrl.secondStage(r.v)
	if fresh.empty() && len(second) == 0 {
		return nil
	}

	pairs := fresh.pairs()
	podReads := []pairRead{
		{query: promql.QPodInfo, label: promql.PodLabel, pairs: pairs},
		{query: promql.QPodOwner, label: promql.PodLabel, pairs: pairs},
	}
	err := r.parallel(promql.QPodInfo,
		func(ctx context.Context) error { return r.issuePairReads(ctx, podReads) },
		func(ctx context.Context) error { return r.issueNameReads(ctx, second) },
	)
	if err != nil {
		return err
	}
	commitNameReads(second)
	if fresh.empty() {
		return nil
	}

	info := fresh.keepRows(podReads[0].rows, r.lk)
	r.tallyDropped(promql.QPodInfo, len(podReads[0].rows)-len(info))
	owner := fresh.keepRows(podReads[1].rows, r.lk)
	r.tallyDropped(promql.QPodOwner, len(podReads[1].rows)-len(owner))
	r.v.Pod = append(r.v.Pod, info...)
	r.v.PodOwner = append(r.v.PodOwner, owner...)
	loaded.merge(fresh)

	nodes := r.nodeReads(info, nil)
	controllers := r.ctrl.firstStage(r.v, owner)
	err = r.parallel(promql.QPodInfo,
		func(ctx context.Context) error { return r.issueNameReads(ctx, nodes) },
		func(ctx context.Context) error { return r.issueNameReads(ctx, controllers) },
	)
	if err != nil {
		return err
	}
	commitNameReads(nodes)
	commitNameReads(controllers)

	tail := r.ctrl.secondStage(r.v)
	if err := r.issueNameReads(r.ctx, tail); err != nil {
		return err
	}
	commitNameReads(tail)
	return nil
}
