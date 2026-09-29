package build

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// readPodSeed is the pod root's seed. It reads the claim-binding family
// restricted on the roots' namespaces and pod names, and keeps a row only
// when its (namespace, pod) is a root ref — the two alternations are
// independent, so a cross pair matches the query and must be dropped before
// any claim is tracked. Bindings are then re-read by claim so every mounter
// of a tracked claim is loaded: the split weight and an inherited Application
// are computed over the same mounters an unrestricted read sees.
//
// kube_pod_info for the roots is the pod wave that follows. readScopedPods
// adds the pod roots to its (namespace, pod) scope, so a claimless root is
// still read, and that read is the flowless one.
func readPodSeed(
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
	defer recoverScopedPanic(ctx, promql.QPVCBindings, &err)
	rows, err := instantAll(ctx, q, promql.QPVCBindings, end, plan.podSeed)
	if err != nil {
		return err
	}
	if len(plan.podSeed) > 0 {
		// Issued even when no root mounts a claim, so the tally records the
		// family at zero rather than omitting a query that ran. v.PVC is
		// filled by the by-claim read below, so these rows are tallied beside
		// it.
		markScopeIssued(v, scopeMu, promql.QPVCBindings)
		addExtraSeries(v, scopeMu, promql.QPVCBindings, len(rows))
	}
	return readMountersOf(ctx, q, window, end, opts, sel, v, scopeMu, keepRootBindings(rows, plan.pods))
}

// keepRootBindings keeps binding rows whose (namespace, pod) is a root ref.
// Cluster is not part of the ref: a pod root names a pod in every cluster of
// the selected estate, and the request's cluster matcher already narrowed
// the query.
func keepRootBindings(rows model.Vector, roots []graph.PodRef) model.Vector {
	root := make(map[graph.PodRef]struct{}, len(roots))
	for _, ref := range roots {
		root[ref] = struct{}{}
	}
	var out model.Vector
	for _, s := range rows {
		ref := graph.PodRef{
			Namespace: string(s.Metric["namespace"]),
			Name:      string(s.Metric[promql.PodLabel]),
		}
		if _, ok := root[ref]; ok && bindingClaim(s.Metric) != "" {
			out = append(out, s)
		}
	}
	return out
}
