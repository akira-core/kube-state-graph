package build

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// podTargets are the two pod families a pod-scoping plan reads by reference,
// with the slot each lands in. They are exactly promql.PodScopedQueries — the
// families promql.RenderScoped accepts.
func podTargets(v *topologyVectors) []scopedTarget {
	return []scopedTarget{
		{promql.QPodInfo, &v.Pod},
		{promql.QPodOwner, &v.PodOwner},
	}
}

// podScope is the sorted, de-duplicated set of pod names a storage body can
// draw: every pod a loaded claim-binding series binds, plus the request's pod
// roots. It mirrors the binding reader's own discard — parseTopology skips a
// series naming no claim — so a pod enters the scope iff it could be bound, or
// is a root, which must be drawable even when it mounts nothing.
func podScope(bindings model.Vector, roots []string) []string {
	names := make([]string, 0, len(bindings)+len(roots))
	for _, s := range bindings {
		if s.Metric["persistentvolumeclaim"] == "" && s.Metric["claim_name"] == "" {
			continue
		}
		names = append(names, string(s.Metric[promql.PodLabel]))
	}
	names = append(names, roots...)
	names = slices.DeleteFunc(names, func(n string) bool { return n == "" })
	slices.Sort(names)
	return slices.Compact(names)
}

// readScopedPods issues kube_pod_info and kube_pod_owner restricted to the pods
// a storage body can draw (podScope), composed with the request's own matchers.
// Pods that mount no claim — in a large estate, nearly all of them — are never
// fetched.
//
// It waits on the claim-binding family, and — when the request carries an
// application root — on the application recovery and on
// kube_persistentvolumeclaim_annotations. The scope is then podScope, or
// podScopeUnderApp when an application root narrowed the binding half.
//
// An empty scope issues no query at all and leaves both families unread: no pod
// could be bound or is a root, so no pod could be drawn. That mirrors the QoS
// read's empty-scope rule.
//
// Unlike a QoS chunk, a pod chunk FAILS CLOSED (legRequired). A pod is
// topology, not a measurement: a missing chunk would be a smaller, plausible,
// wrong body with no signal — the partial fan-out that backend routing D6
// forbids — so the first chunk error fails the build exactly as an unscoped
// kube_pod_info error does. Results merge in CHUNK ORDER (issueScopedFamilies),
// so the vectors and everything parsed from them are a pure function of the
// scope rather than of upstream timing.
//
// A pod name is unique within a namespace only, so the scope MAY admit a
// same-named pod from another namespace. Such a pod lies on no drawn path and is
// not a root, so the storage projection drops it; the body is unchanged.
func readScopedPods(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	podRoots []string,
	applicationRoots []string,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	bindingsDone <-chan struct{},
	appDone <-chan struct{},
	pvcAnnotationsDone <-chan struct{},
	recovered *[]string,
) error {
	// A sibling leg failed (or the caller went away). The group already
	// carries that error; adding another would only mask it.
	wait := func(done <-chan struct{}) bool {
		select {
		case <-done:
			return true
		case <-ctx.Done():
			return false
		}
	}
	if !wait(bindingsDone) || !wait(appDone) {
		return nil
	}
	if len(applicationRoots) > 0 && !wait(pvcAnnotationsDone) {
		return nil
	}

	var scope []string
	if len(applicationRoots) > 0 {
		var names []string
		if recovered != nil {
			names = *recovered
		}
		scope = podScopeUnderApp(v.PVC, v.PVCAnnotations, names, podRoots, applicationRoots)
	} else {
		scope = podScope(v.PVC, podRoots)
	}
	if len(scope) == 0 {
		return nil
	}
	families := make([]scopedFamily, 0, len(promql.PodScopedQueries))
	for _, t := range podTargets(v) {
		families = append(families, scopedFamily{query: t.query, dst: t.dst, scope: scope, mode: legRequired})
	}
	return issueScopedFamilies(ctx, ctx, q, window, end, opts, sel, v, scopeMu, families)
}
