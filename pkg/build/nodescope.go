package build

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// nodeTargets are the four kube-state-metrics node families a by-reference
// plan reads by reference, with the slot each lands in. They are exactly
// promql.NodeScopedQueries.
func nodeTargets(v *topologyVectors) []scopedTarget {
	return []scopedTarget{
		{promql.QNodeInfo, &v.Node},
		{promql.QNodeAddresses, &v.Addr},
		{promql.QNodeLabels, &v.NodeLabels},
		{promql.QNodeStatusCondition, &v.NodeStatus},
	}
}

// nodeScope is the sorted, de-duplicated set of Kubernetes node names a
// storage body can draw the node families for: every node a loaded pod is
// scheduled on, plus the request's `node=` roots. It mirrors the pod scope's
// own rule — a root must be drawable even when no pod names it.
func nodeScope(pods model.Vector, roots []string) []string {
	names := make([]string, 0, len(pods)+len(roots))
	for _, s := range pods {
		if n := string(s.Metric["node"]); n != "" {
			names = append(names, n)
		}
	}
	names = append(names, roots...)
	names = slices.DeleteFunc(names, func(n string) bool { return n == "" })
	slices.Sort(names)
	return slices.Compact(names)
}

// readScopedNodes issues the four kube_node_* families restricted to the
// Kubernetes nodes the loaded pods are scheduled on, plus the request's
// `node=` roots (scope-controller-legs-by-reference).
//
// It waits on the pod wave alone (podsDone): the scope is computed from the
// MERGED v.Pod the pod wave wrote, so it must not read v.Pod before that
// write happens-before this read — which podsDone's close (after
// readScopedPods returns) establishes.
//
// An empty scope issues no query at all: no pod is scheduled and no node
// root was given, so no Kubernetes-node entity could be drawn. Every family
// fails closed — a K8s node is topology, not a measurement, so a missing
// chunk would be a smaller, plausible, wrong body with no signal, exactly
// like a pod chunk.
func readScopedNodes(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	roots []string,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	podsDone <-chan struct{},
) error {
	select {
	case <-podsDone:
	case <-ctx.Done():
		// A sibling leg failed (or the caller went away). The group already
		// carries that error; adding another would only mask it.
		return nil
	}

	scope := nodeScope(v.Pod, roots)
	if len(scope) == 0 {
		return nil
	}
	families := make([]scopedFamily, 0, len(promql.NodeScopedQueries))
	for _, t := range nodeTargets(v) {
		families = append(families, scopedFamily{query: t.query, dst: t.dst, scope: scope})
	}
	return issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, families)
}
