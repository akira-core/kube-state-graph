package build

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// maxApplicationRootChunks bounds stage 1 of the application recovery. The
// parser limits each application= value's length, never the count, so the
// client can inflate the restriction; past this many chunks the family is
// read unrestricted and filtered in the reader. The cap matches the
// volume-label restriction and the wave's own concurrency.
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

	families := annotationRecoveryFamilies()
	namesByKind := map[string][]string{}
	chunks := applicationRootChunks(roots, opts.qosScopeBatchBytes())
	if len(chunks) > maxApplicationRootChunks {
		for _, f := range families {
			slog.WarnContext(ctx, "application_root_restriction_unbounded",
				"query", string(f.query),
				"chunks", len(chunks),
			)
			vec, err := issueUnrestricted(ctx, q, f.query, window, end, keys, sel)
			if err != nil {
				return nil, err
			}
			addExtraSeries(v, scopeMu, f.query, len(vec))
			namesByKind[f.kind] = namesFromTracking(vec, f.label, apps)
		}
	} else {
		dsts := make([]model.Vector, len(families))
		scoped := make([]scopedFamily, len(families))
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
		if err := issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, scoped); err != nil {
			return nil, err
		}
		for i, f := range families {
			addExtraSeries(v, scopeMu, f.query, len(dsts[i]))
			namesByKind[f.kind] = namesFromTracking(dsts[i], f.label, apps)
		}
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
	if len(stage3) == 0 {
		return nil, nil
	}
	if err := issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, stage3); err != nil {
		return nil, err
	}
	var pods []string
	for _, dst := range podVecs {
		addExtraSeries(v, scopeMu, promql.QPodOwner, len(*dst))
		for _, s := range *dst {
			if p := string(s.Metric[promql.PodLabel]); p != "" {
				pods = append(pods, p)
			}
		}
	}
	return sortedNames(pods), nil
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

// issueUnrestricted reads one family with its fixed selector and the request
// matchers only — the /v1/graph shape. It is the stage-1 fallback when the
// root set does not yield a bounded restriction, and like every recovery read
// it fails the build on a query error.
func issueUnrestricted(
	ctx context.Context,
	q promql.Querier,
	name promql.Query,
	window time.Duration,
	end time.Time,
	keys promql.LabelKeys,
	sel promql.Selector,
) (out model.Vector, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(ctx, "panic in unrestricted topology query",
				"query", string(name),
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			out, err = nil, fmt.Errorf("panic in %s query: %v", name, rec)
		}
	}()
	res, qerr := q.Instant(ctx, string(name), promql.Render(name, window, keys, sel), end)
	if qerr != nil {
		return nil, wrapQueryError(name, qerr)
	}
	return res, nil
}

// storageClaimKey is one claim as the binding and annotation readers name it.
// Namespace is part of the key: a same-named claim in another namespace is a
// different object.
type storageClaimKey struct {
	cluster, namespace, claim string
}

func bindingClaim(m model.Metric) string {
	if c := string(m["persistentvolumeclaim"]); c != "" {
		return c
	}
	return string(m["claim_name"])
}

// podScopeUnderApp is the pod scope under an application root. The keep set
// is the recovered names plus the request's pod= roots. A claim is related
// when it is own-annotated with a root Application or mounted by a pod in the
// keep set; every mounter of a related claim joins the scope, so a shared
// claim's split weight and inherited Application are computed over the same
// mounters as an un-narrowed read.
func podScopeUnderApp(bindings, pvcAnnotations model.Vector, recovered, podRoots, apps []string) []string {
	appSet := make(map[string]struct{}, len(apps))
	for _, a := range apps {
		if a != "" {
			appSet[a] = struct{}{}
		}
	}
	keep := map[string]struct{}{}
	for _, n := range recovered {
		if n != "" {
			keep[n] = struct{}{}
		}
	}
	for _, n := range podRoots {
		if n != "" {
			keep[n] = struct{}{}
		}
	}
	related := map[storageClaimKey]struct{}{}
	for _, s := range pvcAnnotations {
		app := argoAppName(string(s.Metric[argoTrackingIDLabel]))
		if app == "" {
			continue
		}
		if _, ok := appSet[app]; !ok {
			continue
		}
		claim := string(s.Metric["persistentvolumeclaim"])
		if claim == "" {
			continue
		}
		related[storageClaimKey{string(s.Metric["cluster"]), string(s.Metric["namespace"]), claim}] = struct{}{}
	}
	claimOf := func(s *model.Sample) (storageClaimKey, bool) {
		claim := bindingClaim(s.Metric)
		if claim == "" {
			return storageClaimKey{}, false
		}
		return storageClaimKey{string(s.Metric["cluster"]), string(s.Metric["namespace"]), claim}, true
	}
	for _, s := range bindings {
		pod := string(s.Metric[promql.PodLabel])
		if _, ok := keep[pod]; !ok || pod == "" {
			continue
		}
		if key, ok := claimOf(s); ok {
			related[key] = struct{}{}
		}
	}
	names := make([]string, 0, len(keep)+len(bindings))
	for n := range keep {
		names = append(names, n)
	}
	for _, s := range bindings {
		pod := string(s.Metric[promql.PodLabel])
		if pod == "" {
			continue
		}
		key, ok := claimOf(s)
		if !ok {
			continue
		}
		if _, relatedClaim := related[key]; relatedClaim {
			names = append(names, pod)
		}
	}
	return sortedNames(names)
}
