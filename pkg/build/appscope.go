package build

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// maxApplicationRootChunks bounds stage 1 of the application recovery and the
// claim-annotation read beside it. The parser limits each application=
// value's length, never the count, so the client can inflate the restriction;
// past this many chunks the request is rejected before any query. The cap
// matches the volume-label restriction and the wave's own concurrency.
const maxApplicationRootChunks = scopeConcurrency

// applicationRootChunks splits root Applications under the byte budget with
// the tracking-id wrapper reserved once per chunk. The same split is what
// issueScopedFamilies applies when a family sets budgetReserve to
// promql.TrackingIDWrapperCost and budgetOverhead to zero, so the bound
// check and the issued queries agree.
func applicationRootChunks(values []string, budget int) [][]string {
	b := budget - promql.TrackingIDWrapperCost
	if b < 1 {
		b = 1
	}
	return promql.ChunkScopeWithOverhead(values, b, 0)
}

type annotationRecovery struct {
	query promql.Query
	kind  string
	label model.LabelName
}

// annotationRecoveryFamilies is stage 1. Every family fails the build on a
// query error — the history-accumulating two included — because the
// recovery runs only on /v1/storage-graph, which fails closed
// (fail-storage-graph-on-any-leg-error): a lost chunk would silently drop the
// pods of the Applications it carried.
func annotationRecoveryFamilies() []annotationRecovery {
	return []annotationRecovery{
		{promql.QDeploymentAnnotations, "Deployment", "deployment"},
		{promql.QStatefulSetAnnotations, "StatefulSet", "statefulset"},
		{promql.QDaemonSetAnnotations, "DaemonSet", "daemonset"},
		{promql.QCronJobAnnotations, "CronJob", "cronjob"},
		{promql.QReplicaSetAnnotations, "ReplicaSet", "replicaset"},
		{promql.QJobAnnotations, "Job", "job_name"},
	}
}

// readScopedApplications recovers pod names owned by controllers whose
// tracking-id names a root Application. It returns names only: the pods are
// read by the existing by-reference waves, and the projection decides which
// of them are roots. An empty root set returns no names and issues nothing.
func readScopedApplications(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	roots []string,
	v *topologyVectors,
	scopeMu *sync.Mutex,
) ([]string, error) {
	roots = sortedNames(roots)
	if len(roots) == 0 {
		return nil, nil
	}
	keys := opts.LabelKeys
	apps := make(map[string]struct{}, len(roots))
	for _, a := range roots {
		apps[a] = struct{}{}
	}

	if len(applicationRootChunks(roots, opts.qosScopeBatchBytes())) > maxApplicationRootChunks {
		return nil, NewError(ReasonInvalidScope, RootScopeCapMessage, nil)
	}
	families := annotationRecoveryFamilies()
	namesByKind := map[string][]string{}
	dsts := make([]model.Vector, len(families))
	scoped := make([]scopedFamily, len(families), len(families)+1)
	for i, f := range families {
		scoped[i] = scopedFamily{
			query:         f.query,
			dst:           &dsts[i],
			scope:         roots,
			budgetReserve: promql.TrackingIDWrapperCost,
			render: func(chunk []string) (string, bool) {
				return promql.RenderTrackingIDScoped(f.query, window, keys, sel, chunk)
			},
		}
	}
	// Own-annotated claims are discovered here, in parallel with stage 1.
	// The rows land in v.PVCAnnotations; the claim expansion re-reads the
	// family by claim name and the tally keeps both reads.
	scoped = append(scoped, scopedFamily{
		query:         promql.QPVCAnnotations,
		dst:           &v.PVCAnnotations,
		scope:         roots,
		budgetReserve: promql.TrackingIDWrapperCost,
		render: func(chunk []string) (string, bool) {
			return promql.RenderTrackingIDScoped(promql.QPVCAnnotations, window, keys, sel, chunk)
		},
	})
	if err := issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, scoped); err != nil {
		return nil, err
	}
	for i, f := range families {
		addExtraSeries(v, scopeMu, f.query, len(dsts[i]))
		namesByKind[f.kind] = namesFromTracking(dsts[i], f.label, apps)
	}

	var rsVec, jobVec model.Vector
	var stage2 []scopedFamily
	if deps := namesByKind["Deployment"]; len(deps) > 0 {
		stage2 = append(stage2, scopedFamily{
			query: promql.QReplicaSetOwner,
			dst:   &rsVec,
			scope: deps,
			render: func(chunk []string) (string, bool) {
				return promql.RenderOwnerScoped(promql.QReplicaSetOwner, window, keys, sel, "Deployment", chunk)
			},
		})
	}
	if cjs := namesByKind["CronJob"]; len(cjs) > 0 {
		stage2 = append(stage2, scopedFamily{
			query: promql.QJobOwner,
			dst:   &jobVec,
			scope: cjs,
			render: func(chunk []string) (string, bool) {
				return promql.RenderOwnerScoped(promql.QJobOwner, window, keys, sel, "CronJob", chunk)
			},
		})
	}
	if len(stage2) > 0 {
		if err := issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, stage2); err != nil {
			return nil, err
		}
		for _, fam := range stage2 {
			addExtraSeries(v, scopeMu, fam.query, len(*fam.dst))
		}
	}
	rsNames := filteredNames(rsVec, "replicaset", func(m model.Metric) bool {
		return string(m["owner_kind"]) == "Deployment"
	})
	jobNames := filteredNames(jobVec, "job_name", nil)

	kindNames := map[string][]string{
		"ReplicaSet":  sortedNames(append(append([]string{}, namesByKind["ReplicaSet"]...), rsNames...)),
		"Job":         sortedNames(append(append([]string{}, namesByKind["Job"]...), jobNames...)),
		"StatefulSet": namesByKind["StatefulSet"],
		"DaemonSet":   namesByKind["DaemonSet"],
		"Deployment":  namesByKind["Deployment"],
		"CronJob":     namesByKind["CronJob"],
	}
	var stage3 []scopedFamily
	podVecs := make([]*model.Vector, 0, 6)
	for _, kind := range []string{"ReplicaSet", "Job", "StatefulSet", "DaemonSet", "Deployment", "CronJob"} {
		names := kindNames[kind]
		if len(names) == 0 {
			continue
		}
		dst := &model.Vector{}
		podVecs = append(podVecs, dst)
		stage3 = append(stage3, scopedFamily{
			query: promql.QPodOwner,
			dst:   dst,
			scope: names,
			render: func(chunk []string) (string, bool) {
				return promql.RenderOwnerScoped(promql.QPodOwner, window, keys, sel, kind, chunk)
			},
		})
	}
	var pods []string
	if len(stage3) > 0 {
		if err := issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, stage3); err != nil {
			return nil, err
		}
		for _, dst := range podVecs {
			addExtraSeries(v, scopeMu, promql.QPodOwner, len(*dst))
			for _, s := range *dst {
				if p := string(s.Metric[promql.PodLabel]); p != "" {
					pods = append(pods, p)
				}
			}
		}
	}
	pods = sortedNames(pods)
	if err := readApplicationBindings(ctx, q, window, end, opts, sel, pods, roots, v, scopeMu); err != nil {
		return nil, err
	}
	return pods, nil
}

// readApplicationBindings reads claim bindings for the recovered pods, unions
// the claims an annotation row names with a root Application, and re-reads
// bindings by claim so every mounter of that set is loaded. An empty set
// issues no binding query.
func readApplicationBindings(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	pods, roots []string,
	v *topologyVectors,
	scopeMu *sync.Mutex,
) error {
	ann := annotatedClaimKeys(v.PVCAnnotations, roots)
	var tracked model.Vector
	if len(pods) > 0 {
		byPod, err := queryRendered(ctx, q, end, opts, promql.QPVCBindings, pods, func(chunk []string) (string, bool) {
			return promql.RenderClaimBindingsByPodName(window, opts.LabelKeys, sel, chunk)
		})
		if err != nil {
			return err
		}
		markScopeIssued(v, scopeMu, promql.QPVCBindings)
		tracked = keepNamedPodBindings(byPod, pods)
	}
	keys := claimKeysOf(tracked)
	for k := range ann {
		keys[k] = struct{}{}
	}
	claims := claimNamesFromKeys(keys)
	if len(claims) == 0 {
		v.PVC = tracked
		return nil
	}
	if err := issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, []scopedFamily{{
		query: promql.QPVCBindings,
		dst:   &v.PVC,
		scope: claims,
	}}); err != nil {
		return err
	}
	v.PVC = keepClaimBindings(v.PVC, keys)
	return nil
}

func annotatedClaimKeys(rows model.Vector, roots []string) map[claimSeriesKey]struct{} {
	apps := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if root != "" {
			apps[root] = struct{}{}
		}
	}
	out := make(map[claimSeriesKey]struct{})
	for _, s := range rows {
		app := argoAppName(string(s.Metric[argoTrackingIDLabel]))
		if _, ok := apps[app]; !ok || app == "" {
			continue
		}
		claim := string(s.Metric["persistentvolumeclaim"])
		if claim == "" {
			continue
		}
		out[claimSeriesKey{
			cluster:   string(s.Metric["cluster"]),
			namespace: string(s.Metric["namespace"]),
			claim:     claim,
		}] = struct{}{}
	}
	return out
}

func keepNamedPodBindings(rows model.Vector, pods []string) model.Vector {
	keep := make(map[string]struct{}, len(pods))
	for _, pod := range pods {
		if pod != "" {
			keep[pod] = struct{}{}
		}
	}
	var out model.Vector
	for _, s := range rows {
		if _, ok := keep[string(s.Metric[promql.PodLabel])]; ok && bindingClaim(s.Metric) != "" {
			out = append(out, s)
		}
	}
	return out
}

func claimNamesFromKeys(keys map[claimSeriesKey]struct{}) []string {
	names := make([]string, 0, len(keys))
	for k := range keys {
		if k.claim != "" {
			names = append(names, k.claim)
		}
	}
	return sortedNames(names)
}

func namesFromTracking(vec model.Vector, label model.LabelName, apps map[string]struct{}) []string {
	var out []string
	for _, s := range vec {
		app := argoAppName(string(s.Metric[argoTrackingIDLabel]))
		if app == "" {
			continue
		}
		if _, ok := apps[app]; !ok {
			continue
		}
		if name := string(s.Metric[label]); name != "" {
			out = append(out, name)
		}
	}
	return sortedNames(out)
}

func filteredNames(vec model.Vector, label model.LabelName, keep func(model.Metric) bool) []string {
	var out []string
	for _, s := range vec {
		if keep != nil && !keep(s.Metric) {
			continue
		}
		if n := string(s.Metric[label]); n != "" {
			out = append(out, n)
		}
	}
	return sortedNames(out)
}

func bindingClaim(m model.Metric) string {
	if c := string(m["persistentvolumeclaim"]); c != "" {
		return c
	}
	return string(m["claim_name"])
}
