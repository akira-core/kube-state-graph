package build

import (
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
// pair. The pairs are read GROUPED, one query per chunk carrying a namespace
// alternation beside a name alternation (promql.RenderNamesAcrossNamespaces,
// chunked by promql.ChunkNamePairs), as the Kubernetes-only build reads them,
// rather than one query per namespace. The two alternations are independent,
// so a chunk can match a cross pair — a name requested in one namespace that
// also exists in another — and the read drops those rows (tallying them) before
// the node, controller or claim reads are scoped from what it returns.

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

// podRefsOfKeys drops the zone and cluster of each key: a pod-keyed read is
// issued on (namespace, pod) pairs across the clusters and zones the request
// selects, and the key filters the rows it returns.
func podRefsOfKeys(keys []podSeriesKey) []graph.PodRef {
	out := make([]graph.PodRef, 0, len(keys))
	for _, k := range keys {
		out = append(out, graph.PodRef{Namespace: k.namespace, Name: k.pod})
	}
	return out
}

// podKeysOf returns the sorted, de-duplicated (az, env, cluster, namespace, pod)
// of every row naming a pod.
func podKeysOf(rows model.Vector, keys promql.LabelKeys) []podSeriesKey {
	out := make([]podSeriesKey, 0, len(rows))
	for _, s := range rows {
		if k := podSeriesKeyOf(s.Metric, keys); k.pod != "" {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, comparePodSeriesKeys)
	return slices.Compact(out)
}

// podNamePairs is the (namespace, pod) pair of every ref. A ref with no
// namespace or no pod name names no pod and is dropped.
func podNamePairs(refs []graph.PodRef) []promql.NamePair {
	out := make([]promql.NamePair, 0, len(refs))
	for _, ref := range refs {
		out = append(out, promql.NamePair{Namespace: ref.Namespace, Name: ref.Name})
	}
	return promql.SortedNamePairs(out)
}

// issuePodFamiliesByPair reads every target family restricted to the
// (namespace, pod) pairs of refs, grouped across namespaces and chunked under
// the shared byte budget and promql.MaxAlternationValues per alternation, all
// under one bounded wave. Each target keeps only the rows of a requested pair,
// in chunk order, so its vector is a pure function of refs rather than of
// upstream timing; a cross pair the independent alternations matched is
// dropped and tallied as read.
//
// No ref issues nothing and leaves every target untouched: an empty scope is
// neither read nor tallied. A chunk error fails the read, as every
// by-reference read of a storage build does.
func issuePodFamiliesByPair(
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
	pairs := podNamePairs(refs)
	if len(pairs) == 0 {
		return nil
	}
	reads := make([]pairRead, len(targets))
	for i, t := range targets {
		reads[i] = pairRead{query: t.query, label: promql.PodLabel, pairs: pairs}
	}
	if err := issuePairReads(ctx, q, window, end, opts, sel, v, scopeMu, reads); err != nil {
		return err
	}
	for i, t := range targets {
		kept := promql.KeepNamePairs(reads[i].rows, promql.PodLabel, pairs)
		if dropped := len(reads[i].rows) - len(kept); dropped > 0 {
			addExtraSeries(v, scopeMu, t.query, dropped)
		}
		*t.dst = kept
	}
	return nil
}

// queryPodsByPair is issuePodFamiliesByPair for one family whose rows the
// caller filters itself rather than landing them in a topology slot.
func queryPodsByPair(
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
	err := issuePodFamiliesByPair(ctx, q, window, end, opts, sel, v, scopeMu,
		[]scopedTarget{{query: name, dst: &out}}, refs)
	return out, err
}
