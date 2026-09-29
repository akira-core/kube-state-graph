package build

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// A pod is identified by (namespace, pod), never by its name alone: a name is
// unique within a namespace only. Every read a storage build issues for pods it
// already knows — the pod wave, the node seed's incarnation completion, and the
// claim bindings of node-seeded and recovered pods — is therefore keyed by that
// pair, one query per namespace (promql.RenderNamesInNamespace). A same-named
// pod in another namespace is never read, so it cannot widen the node,
// controller or claim reads that are scoped from what these reads return.

// groupPodRefs groups refs by namespace. Namespaces are returned sorted and
// each namespace's pod names sorted and de-duplicated, so the issued queries
// and their merge order are a pure function of the ref set. A ref with no pod
// name is dropped. It does not mutate refs.
func groupPodRefs(refs []graph.PodRef) ([]string, map[string][]string) {
	byNS := make(map[string][]string)
	for _, ref := range refs {
		if ref.Name != "" {
			byNS[ref.Namespace] = append(byNS[ref.Namespace], ref.Name)
		}
	}
	namespaces := make([]string, 0, len(byNS))
	for ns, pods := range byNS {
		slices.Sort(pods)
		byNS[ns] = slices.Compact(pods)
		namespaces = append(namespaces, ns)
	}
	slices.Sort(namespaces)
	return namespaces, byNS
}

// podRefsOf returns the (namespace, pod) of every row naming a pod.
func podRefsOf(rows model.Vector) []graph.PodRef {
	out := make([]graph.PodRef, 0, len(rows))
	for _, s := range rows {
		if pod := string(s.Metric[promql.PodLabel]); pod != "" {
			out = append(out, graph.PodRef{Namespace: string(s.Metric[promql.NamespaceLabel]), Name: pod})
		}
	}
	return out
}

// podRefsOfKeys drops the cluster of each key: a pod-keyed read is issued per
// namespace across the clusters the request selects.
func podRefsOfKeys(keys []podSeriesKey) []graph.PodRef {
	out := make([]graph.PodRef, 0, len(keys))
	for _, k := range keys {
		out = append(out, graph.PodRef{Namespace: k.namespace, Name: k.pod})
	}
	return out
}

// podKeysOf returns the sorted, de-duplicated (cluster, namespace, pod) of
// every row naming a pod.
func podKeysOf(rows model.Vector) []podSeriesKey {
	out := make([]podSeriesKey, 0, len(rows))
	for _, s := range rows {
		k := podSeriesKey{
			cluster:   string(s.Metric["cluster"]),
			namespace: string(s.Metric[promql.NamespaceLabel]),
			pod:       string(s.Metric[promql.PodLabel]),
		}
		if k.pod != "" {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b podSeriesKey) int {
		return cmp.Or(cmp.Compare(a.cluster, b.cluster), cmp.Compare(a.namespace, b.namespace), cmp.Compare(a.pod, b.pod))
	})
	return slices.Compact(out)
}

// issuePodFamiliesByNamespace reads every target family restricted to refs, one
// query per namespace (chunked under the shared byte budget, the namespace
// equality charged once per chunk), all under one bounded wave. Each target's
// vector is the merge of its namespaces' results in namespace order, so it is
// a pure function of refs rather than of upstream timing.
//
// No ref issues nothing and leaves every target untouched: an empty scope is
// neither read nor tallied. A chunk error fails the read, as every
// by-reference read of a storage build does.
func issuePodFamiliesByNamespace(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	targets []scopedTarget,
	refs []graph.PodRef,
) error {
	namespaces, byNS := groupPodRefs(refs)
	if len(namespaces) == 0 {
		return nil
	}
	parts := make([][]model.Vector, len(targets))
	families := make([]scopedFamily, 0, len(targets)*len(namespaces))
	for ti, t := range targets {
		parts[ti] = make([]model.Vector, len(namespaces))
		for ni, ns := range namespaces {
			families = append(families, scopedFamily{
				query: t.query,
				dst:   &parts[ti][ni],
				scope: byNS[ns],
				render: func(chunk []string) (string, bool) {
					return promql.RenderNamesInNamespace(t.query, window, opts.LabelKeys, sel, ns, promql.PodLabel, chunk)
				},
				budgetReserve: promql.NamespaceEqualityCost(ns),
			})
		}
	}
	if err := issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, families); err != nil {
		return err
	}
	for ti, t := range targets {
		var merged model.Vector
		for _, part := range parts[ti] {
			merged = append(merged, part...)
		}
		*t.dst = merged
	}
	return nil
}

// queryPodsByNamespace is issuePodFamiliesByNamespace for one family whose rows
// the caller filters itself rather than landing them in a topology slot.
func queryPodsByNamespace(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	name promql.Query,
	refs []graph.PodRef,
) (model.Vector, error) {
	var out model.Vector
	err := issuePodFamiliesByNamespace(ctx, q, window, end, opts, sel, v, scopeMu,
		[]scopedTarget{{query: name, dst: &out}}, refs)
	return out, err
}
