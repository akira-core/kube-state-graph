package build

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// readWorkloadClaims is the claim side of a workload root (node, pod,
// application). The seed already named the claims, so every claim family but
// the bindings — those the seed re-read by claim, which is the mounter
// completion — is read per namespace on (namespace, persistentvolumeclaim)
// and kept only when the row's (az, env, cluster, namespace, claim) is
// tracked. An empty set issues nothing.
func readWorkloadClaims(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	plan topologyPlan,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	bindingsDone, appDone <-chan struct{},
) (err error) {
	defer recoverScopedPanic(ctx, promql.QPVCInfo, &err)
	if !waitSignal(ctx, bindingsDone) {
		return nil
	}
	if len(plan.applicationRoots) > 0 && !waitSignal(ctx, appDone) {
		return nil
	}
	keys := opts.LabelKeys.OrDefault()
	tracked := trackedClaimKeys(v.PVC, keys)
	for k := range annotatedClaimKeys(v.PVCAnnotations, plan.applicationRoots, keys) {
		tracked[k] = struct{}{}
	}
	if len(tracked) == 0 {
		return nil
	}
	// The application seed already landed tracking-id rows in this vector.
	// The claim-name read replaces it; the tally keeps both reads.
	if n := len(v.PVCAnnotations); n > 0 {
		addExtraSeries(v, scopeMu, promql.QPVCAnnotations, n)
	}
	return issueClaimFamiliesByNamespace(ctx, q, window, end, opts, sel, v, scopeMu, []scopedTarget{
		{promql.QPVCInfo, &v.PVCInfo},
		{promql.QPVCAnnotations, &v.PVCAnnotations},
		{promql.QKubeletVolumeUsedBytes, &v.KubeletVolumeUsed},
		{promql.QKubeletVolumeCapacityBytes, &v.KubeletVolumeCapacity},
	}, tracked)
}

// readWorkloadVolumeLabels is candidate completion for a workload root, plus
// owner completion of every aggregate those rows name. Nothing was read whole,
// so every aggregate the token read touches is completed. Completion rows are
// merged after the claim read took its names, and they are not a claim source.
func readWorkloadVolumeLabels(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	plan topologyPlan,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	pvcInfoDone <-chan struct{},
) (err error) {
	defer recoverScopedPanic(ctx, promql.QVolumeLabels, &err)
	if !waitSignal(ctx, pvcInfoDone) {
		return nil
	}
	rw := v.VolumeKey
	if rw == nil {
		rw = defaultVolumeKeyRewriter()
	}
	tokens := tokensOfClaims(v.PVCInfo, rw)
	if len(tokens) == 0 {
		return nil
	}
	rows, err := issueTokenVolumeLabels(ctx, q, window, end, opts, sel, tokens, nil)
	if err != nil {
		return err
	}
	completed, err := readOwnerCompletion(ctx, q, window, end, opts, sel, ownerCompletionTargets(rows, plan))
	if err != nil {
		return err
	}
	merged := mergeVolumeLabels(rows, completed)
	if len(merged) == 0 {
		return nil
	}
	v.VolumeLabels = merged
	markScopeIssued(v, scopeMu, promql.QVolumeLabels)
	return nil
}

// readReachedHarvest reads the aggregate gauges, controller families and
// fixed-policy families for the components the merged volume-label rows name,
// plus whatever a flowless root read did not already cover. It waits until
// that root read has finished writing the same vectors.
//
// It waits on pvcInfoDone itself rather than trusting volumeLabelsFinal to
// imply it. The tail that closes volumeLabelsFinal waits for the claim read
// only on its success path: a failed owner completion returns at once, and
// signalWhenDone closes the channel before the errgroup cancels the context,
// so the claim read can still be writing v.PVCInfo when this reader starts.
func readReachedHarvest(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	plan topologyPlan,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	pvcInfoDone, volumeLabelsFinal, flowlessDone <-chan struct{},
) (err error) {
	defer recoverScopedPanic(ctx, promql.QAggrStatus, &err)
	if !waitSignal(ctx, pvcInfoDone) || !waitSignal(ctx, volumeLabelsFinal) || !waitSignal(ctx, flowlessDone) {
		return nil
	}
	alreadyControllers := controllersAlreadyRead(plan, v)
	rw := v.VolumeKey
	if rw == nil {
		rw = defaultVolumeKeyRewriter()
	}
	// The fixed-policy read depends only on the claims and the volume-label
	// rows, so it runs beside the aggregate → controller chain instead of
	// behind it. The two write disjoint vectors.
	policyPairs := svmPairsOfMatched(v.VolumeLabels, v.PVCInfo, rw)
	aggrPairs := uncoveredAggrPairs(v.VolumeLabels, plan)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		defer recoverScopedPanic(gctx, promql.QQoSPolicyFixedMaxIOPS, &err)
		return issueHarvestPairMap(gctx, q, window, end, opts, sel, v, scopeMu,
			promql.PolicyPairQueries, policyPairs)
	})
	g.Go(func() (err error) {
		defer recoverScopedPanic(gctx, promql.QAggrStatus, &err)
		if err := issueHarvestPairMap(gctx, q, window, end, opts, sel, v, scopeMu,
			promql.AggrPairQueries, aggrPairs); err != nil {
			return err
		}
		controllerRows := []model.Vector{
			v.VolumeLabels, v.AggrStatus, v.AggrSpaceUsed, v.AggrSpaceTotal,
		}
		return issueHarvestPairMap(gctx, q, window, end, opts, sel, v, scopeMu,
			promql.NetAppNodePairQueries, uncoveredControllerPairs(controllerRows, plan, alreadyControllers))
	})
	return g.Wait()
}

func trackedClaimKeys(rows model.Vector, keys promql.LabelKeys) map[claimKey]struct{} {
	out := make(map[claimKey]struct{})
	for _, s := range rows {
		claim := bindingClaim(s.Metric)
		if claim == "" {
			continue
		}
		out[claimKeyOf(s.Metric, keys, claim)] = struct{}{}
	}
	return out
}

func annotatedClaimKeys(rows model.Vector, roots []string, keys promql.LabelKeys) map[claimKey]struct{} {
	if len(roots) == 0 || len(rows) == 0 {
		return nil
	}
	apps := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if root != "" {
			apps[root] = struct{}{}
		}
	}
	out := make(map[claimKey]struct{})
	for _, s := range rows {
		app := argoAppName(string(s.Metric[argoTrackingIDLabel]))
		if app == "" {
			continue
		}
		if _, ok := apps[app]; !ok {
			continue
		}
		claim := string(s.Metric[promql.ClaimLabel])
		if claim == "" {
			continue
		}
		out[claimKeyOf(s.Metric, keys, claim)] = struct{}{}
	}
	return out
}

// controllersAlreadyRead is the set of controller names a flowless root read
// already fetched by name, so the reached-component read does not fetch them
// again. An ontap_cluster root covered whole filers; that is expressed by
// skipping the cluster, not by this set.
func controllersAlreadyRead(plan topologyPlan, v *topologyVectors) map[string]struct{} {
	if !plan.flowlessControllers() {
		return nil
	}
	var names []string
	switch plan.kind {
	case graph.StorageRootONTAPNode:
		names = plan.volumeNodes
	case graph.StorageRootAggr:
		names = controllerNames(v.AggrStatus, v.AggrSpaceUsed, v.AggrSpaceTotal, v.VolumeLabels)
	default:
		return nil
	}
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		if n != "" {
			out[n] = struct{}{}
		}
	}
	return out
}

func uncoveredAggrPairs(rows model.Vector, plan topologyPlan) map[string][]string {
	out := map[string][]string{}
	for _, s := range rows {
		cluster := string(s.Metric[promql.VolumeLabelsClusterLabel])
		aggr := string(s.Metric["aggr"])
		if cluster == "" || aggr == "" {
			continue
		}
		if slices.Contains(plan.volumeClusters, cluster) || slices.Contains(plan.volumeAggrs, aggr) ||
			slices.Contains(plan.volumeAggrPairs[cluster], aggr) {
			continue
		}
		out[cluster] = append(out[cluster], aggr)
	}
	return compactPairs(out)
}

func uncoveredControllerPairs(rows []model.Vector, plan topologyPlan, alreadyByName map[string]struct{}) map[string][]string {
	out := map[string][]string{}
	for _, vec := range rows {
		for _, s := range vec {
			// volume_labels and the aggr_* gauges name the ONTAP cluster with
			// the same label.
			cluster := string(s.Metric[promql.VolumeLabelsClusterLabel])
			node := string(s.Metric["node"])
			if cluster == "" || node == "" {
				continue
			}
			if slices.Contains(plan.volumeClusters, cluster) {
				continue
			}
			if _, ok := alreadyByName[node]; ok {
				continue
			}
			out[cluster] = append(out[cluster], node)
		}
	}
	return compactPairs(out)
}

// svmPairsOfMatched is the (ONTAP cluster, SVM) set of volume-label rows that
// suffix-match a tracked claim. Owner-completion rows of other volumes on the
// same aggregate are not included: the fixed-policy read keys on the claim's
// SVM, and a volume that does not match the token cannot be that SVM.
func svmPairsOfMatched(volumeLabels, pvcInfo model.Vector, rw *VolumeKeyRewriter) map[string][]string {
	if len(volumeLabels) == 0 || len(pvcInfo) == 0 || rw == nil {
		return nil
	}
	_, matcher := claimVolumeMatcher(pvcInfo, rw)
	if matcher == nil {
		return nil
	}
	out := map[string][]string{}
	for _, s := range volumeLabels {
		if !matcher.any(string(s.Metric[promql.HarvestVolumeLabel])) {
			continue
		}
		cluster := string(s.Metric[promql.VolumeLabelsClusterLabel])
		svm := string(s.Metric["svm"])
		if cluster == "" || svm == "" {
			continue
		}
		out[cluster] = append(out[cluster], svm)
	}
	return compactPairs(out)
}

func compactPairs(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for cluster, names := range in {
		names = sortedNames(names)
		if len(names) > 0 {
			out[cluster] = names
		}
	}
	return out
}

// issueHarvestPairMap reads each family one query per ONTAP cluster, chunked,
// and merges the rows into the vector a flowless read may already have
// written. An empty map issues nothing.
func issueHarvestPairMap(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	families []promql.Query,
	byCluster map[string][]string,
) error {
	if len(byCluster) == 0 {
		return nil
	}
	wave, wctx := errgroup.WithContext(ctx)
	wave.SetLimit(scopeConcurrency)
	for _, fam := range families {
		dst := harvestDst(v, fam)
		if dst == nil {
			continue
		}
		chunks := promql.ChunkHarvestPairs(fam, opts.LabelKeys, sel, byCluster, opts.qosScopeBatchBytes())
		if len(chunks) == 0 {
			continue
		}
		wave.Go(func() (err error) {
			defer recoverScopedPanic(wctx, fam, &err)
			parts := make([]model.Vector, len(chunks))
			partWave, pctx := errgroup.WithContext(wctx)
			partWave.SetLimit(scopeConcurrency)
			for i, chunk := range chunks {
				partWave.Go(func() (err error) {
					defer recoverScopedPanic(pctx, fam, &err)
					query, ok := promql.RenderHarvestPair(fam, window, opts.LabelKeys, sel, chunk.Cluster, chunk.Names)
					if !ok {
						return nil
					}
					out, err := q.Instant(pctx, string(fam), query, end)
					if err != nil {
						return wrapQueryError(fam, err)
					}
					parts[i] = out
					return nil
				})
			}
			if err := partWave.Wait(); err != nil {
				return err
			}
			var merged model.Vector
			for _, part := range parts {
				merged = mergeVolumeLabels(merged, part)
			}
			scopeMu.Lock()
			*dst = mergeVolumeLabels(*dst, merged)
			scopeMu.Unlock()
			markScopeIssued(v, scopeMu, fam)
			return nil
		})
	}
	return wave.Wait()
}
