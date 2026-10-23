package build

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// podTargets are the two pod families a pod-scoping plan reads by reference,
// with the slot each lands in. They are exactly promql.PodScopedQueries.
func podTargets(v *topologyVectors) []scopedTarget {
	return []scopedTarget{
		{promql.QPodInfo, &v.Pod},
		{promql.QPodOwner, &v.PodOwner},
	}
}

// bindingPodRefs is the (namespace, pod) of every pod a loaded claim-binding
// series binds. It mirrors the binding reader's own discard — parseTopology
// skips a series naming no claim — so a pod enters the scope iff it could be
// bound. The namespace is the binding row's own, which is also the claim's:
// a pod can only mount a claim of its own namespace.
func bindingPodRefs(bindings model.Vector) []graph.PodRef {
	out := make([]graph.PodRef, 0, len(bindings))
	for _, s := range bindings {
		if bindingClaim(s.Metric) == "" {
			continue
		}
		if pod := string(s.Metric[promql.PodLabel]); pod != "" {
			out = append(out, graph.PodRef{Namespace: string(s.Metric[promql.NamespaceLabel]), Name: pod})
		}
	}
	return out
}

// readScopedPods issues kube_pod_info and kube_pod_owner restricted to the pods
// a storage or Kubernetes-only body can draw, composed with the request's own
// matchers. Pods that mount no claim — in a large estate, nearly all of them —
// are never fetched, except a Kubernetes-only node root's (readRootNodePods).
//
// It waits on the claim-binding family, and — when the request carries an
// application root — on the application recovery and on
// kube_persistentvolumeclaim_annotations. The scope is the (namespace, pod) of
// every pod the kept bindings bind (bindingPodRefs), the pod roots and the
// recovered pods, read one query per namespace (issuePodFamiliesByNamespace).
//
// An empty scope issues no query at all and leaves both families unread: no pod
// could be bound or is a root, so no pod could be drawn. That mirrors the QoS
// read's empty-scope rule.
//
// Unlike a /v1/graph QoS chunk, a pod chunk FAILS CLOSED. A pod is
// topology, not a measurement: a missing chunk would be a smaller, plausible,
// wrong body with no signal — the partial fan-out that backend routing D6
// forbids — so the first chunk error fails the build exactly as an unscoped
// kube_pod_info error does. Results merge in CHUNK ORDER (issueScopedFamilies),
// so the vectors and everything parsed from them are a pure function of the
// scope rather than of upstream timing.
//
// The scope is keyed by (namespace, pod), so a same-named pod in another
// namespace is never read, and neither are its node and its controllers, which
// the node and controller waves scope from what this read returns.
func readScopedPods(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	podRoots []graph.PodRef,
	applicationRoots []string,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	bindingsDone <-chan struct{},
	appDone <-chan struct{},
	pvcAnnotationsDone <-chan struct{},
	recovered *[]podSeriesKey,
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

	if len(v.RootNodePods) > 0 {
		// Only a Kubernetes-only node root sets RootNodePods, and that plan has
		// no pod root, application root or recovered pod to add.
		return readRootNodePods(ctx, q, window, end, opts, sel, v, scopeMu)
	}
	refs := append(bindingPodRefs(v.PVC), podRoots...)
	if recovered != nil {
		refs = append(refs, podRefsOfKeys(*recovered)...)
	}
	return issuePodFamiliesByNamespace(ctx, q, window, end, opts, sel, v, scopeMu, podTargets(v), refs)
}

// readRootNodePods is the pod wave of a Kubernetes-only node root. Its scope is
// keyed by the full (az, env, cluster, namespace, pod), not by the
// (namespace, pod) a read is issued on: the pods running on a root node
// (v.RootNodePods) and every pod a kept binding names (the root pods' own and
// their claims' other mounters). A read per namespace still returns a
// same-named pod of every other selected cluster or zone; those rows are
// dropped, so the node, controller and application-metadata waves never scope
// from a pod this body cannot draw.
//
// kube_pod_info is not re-read for the root-node pods: the node seed's
// incarnation completion already read exactly those (namespace, pod) pairs
// under the same selector and kept their rows in v.RootNodePodInfo. The rows
// dropped by the key filter are tallied beside the kept ones, so the tally
// still counts what the upstream returned.
func readRootNodePods(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
) error {
	lk := opts.LabelKeys.OrDefault()
	onRoot := make(map[podSeriesKey]struct{}, len(v.RootNodePods))
	for _, k := range v.RootNodePods {
		onRoot[k] = struct{}{}
	}
	wanted := maps.Clone(onRoot)
	mounters := map[podSeriesKey]struct{}{}
	for _, s := range v.PVC {
		if bindingClaim(s.Metric) == "" {
			continue
		}
		k := podSeriesKeyOf(s.Metric, lk)
		if k.pod == "" {
			continue
		}
		wanted[k] = struct{}{}
		if _, ok := onRoot[k]; !ok {
			mounters[k] = struct{}{}
		}
	}

	var info, owner model.Vector
	wave, wctx := errgroup.WithContext(ctx)
	wave.Go(func() (err error) {
		info, err = queryPodsByNamespace(wctx, q, window, end, opts, sel, v, scopeMu, promql.QPodInfo,
			podRefsOfKeys(sortedPodKeys(mounters)))
		return err
	})
	wave.Go(func() (err error) {
		owner, err = queryPodsByNamespace(wctx, q, window, end, opts, sel, v, scopeMu, promql.QPodOwner,
			podRefsOfKeys(sortedPodKeys(wanted)))
		return err
	})
	if err := wave.Wait(); err != nil {
		return err
	}

	keptInfo := keepPodRows(info, mounters, lk)
	keptOwner := keepPodRows(owner, wanted, lk)
	addExtraSeries(v, scopeMu, promql.QPodInfo, len(info)-len(keptInfo))
	addExtraSeries(v, scopeMu, promql.QPodOwner, len(owner)-len(keptOwner))
	v.Pod = append(slices.Clone(v.RootNodePodInfo), keptInfo...)
	v.PodOwner = keptOwner
	return nil
}

// sortedPodKeys returns the keys of set in comparePodSeriesKeys order.
func sortedPodKeys(set map[podSeriesKey]struct{}) []podSeriesKey {
	return slices.SortedFunc(maps.Keys(set), comparePodSeriesKeys)
}
