package build

import (
	"cmp"
	"context"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// podSeriesKey is one pod as kube_pod_info and the claim-binding family name it,
// keyed by the cluster IDENTITY — the (az, env, cluster) triple under the
// configured label keys, the same triple claimKey carries — so a cluster name
// reused in two selected zones is two clusters and a same-named pod of each is
// two pods. The cluster is bucketed as the parse buckets it (bucketCluster). In
// a single-zone request every row carries the same (az, env), so no comparison
// changes.
type podSeriesKey struct {
	az, env, cluster, namespace, pod string
}

// podSeriesKeyOf reads a series' pod key. The pod may be empty: a caller that
// needs a pod checks it.
func podSeriesKeyOf(m model.Metric, keys promql.LabelKeys) podSeriesKey {
	return podSeriesKey{
		az:        string(m[model.LabelName(keys.AZ)]),
		env:       string(m[model.LabelName(keys.Env)]),
		cluster:   bucketCluster(string(m["cluster"])),
		namespace: string(m[promql.NamespaceLabel]),
		pod:       string(m[promql.PodLabel]),
	}
}

// comparePodSeriesKeys orders keys by (az, env, cluster, namespace, pod).
func comparePodSeriesKeys(a, b podSeriesKey) int {
	return cmp.Or(
		cmp.Compare(a.az, b.az),
		cmp.Compare(a.env, b.env),
		cmp.Compare(a.cluster, b.cluster),
		cmp.Compare(a.namespace, b.namespace),
		cmp.Compare(a.pod, b.pod),
	)
}

// readNodeSeed is the Kubernetes node root's seed. It reads kube_pod_info
// restricted on the root nodes, then again restricted on the (namespace, pod)
// pairs that read returned, without the node matcher (incarnation completion),
// and keeps a pod only when its newest incarnation still runs on a root node.
// Claim bindings are read for those (namespace, pod) pairs and filtered to the
// kept (cluster, namespace, pod) keys. Bindings are then re-read by claim so
// every mounter of a tracked claim is loaded — the split weight and an
// inherited Application are computed over the same mounters an unrestricted
// read sees.
func readNodeSeed(
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
	onNode, err := instantAll(ctx, q, promql.QPodInfo, end, plan.nodeSeed)
	if err != nil {
		return err
	}
	if len(plan.nodeSeed) > 0 {
		// The seed read the family even when no pod runs on the root. The
		// later pod wave overwrites v.Pod when it loads the kept pods, so the
		// seed's own rows are tallied beside it.
		markScopeIssued(v, scopeMu, promql.QPodInfo)
		addExtraSeries(v, scopeMu, promql.QPodInfo, len(onNode))
	}
	onNodeRefs := podRefsOf(onNode)
	if len(onNodeRefs) == 0 {
		return nil
	}
	// The node read returned each pod's namespace, so incarnation completion
	// re-reads exactly those (namespace, pod) pairs, without the node matcher.
	incarnation, err := queryPodsByNamespace(ctx, q, window, end, opts, sel, v, scopeMu, promql.QPodInfo, onNodeRefs)
	if err != nil {
		return err
	}
	addExtraSeries(v, scopeMu, promql.QPodInfo, len(incarnation))
	lk := opts.LabelKeys.OrDefault()
	onRoot := podsNewestOn(incarnation, plan.nodeRoots, lk)
	if len(onRoot) == 0 {
		return nil
	}
	rootKeys := make([]podSeriesKey, 0, len(onRoot))
	for k := range onRoot {
		rootKeys = append(rootKeys, k)
	}
	byPod, err := queryPodsByNamespace(ctx, q, window, end, opts, sel, v, scopeMu, promql.QPVCBindings, podRefsOfKeys(rootKeys))
	if err != nil {
		return err
	}
	// v.PVC is filled by the by-claim read below, so these rows are tallied
	// beside it.
	addExtraSeries(v, scopeMu, promql.QPVCBindings, len(byPod))
	// Issued even when it matched nothing, so the tally records the family at
	// zero rather than omitting a query that ran.
	markScopeIssued(v, scopeMu, promql.QPVCBindings)
	tracked := keepPodBindings(byPod, onRoot, lk)
	return readMountersOf(ctx, q, window, end, opts, sel, v, scopeMu, tracked)
}

// readMountersOf is mounter completion: every pod mounting a claim the seed's
// bindings name. The binding family is re-read by claim — per namespace, on
// (namespace, persistentvolumeclaim), so a common claim name is never read
// across the estate — and kept only when the row's claim is one the seed
// tracked. No tracked claim issues nothing and keeps the seed's own rows.
func readMountersOf(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	tracked model.Vector,
) error {
	claims := trackedClaimKeys(tracked, opts.LabelKeys.OrDefault())
	if len(claims) == 0 {
		v.PVC = tracked
		return nil
	}
	return issueClaimFamiliesByNamespace(ctx, q, window, end, opts, sel, v, scopeMu, []scopedTarget{{promql.QPVCBindings, &v.PVC}}, claims)
}

// podsNewestOn returns the pods whose newest incarnation runs on a root node.
// Newest is the greatest timestamp, then the lexically-largest UID, matching
// parseTopology's canonical pod. A series with no UID is ignored, as the parse
// ignores it.
//
// kube-state-metrics emits several series per UID while a pod is scheduled
// (the first scrape can carry no node). parseTopology merges a UID's labels
// and keeps the first non-empty node, so the node here is merged the same way:
// an empty-node series of the canonical UID never hides the node another
// series of that UID names.
func podsNewestOn(rows model.Vector, roots []string, keys promql.LabelKeys) map[podSeriesKey]struct{} {
	type best struct {
		ts   model.Time
		uid  string
		node string
	}
	root := make(map[string]struct{}, len(roots))
	for _, r := range roots {
		root[r] = struct{}{}
	}
	canon := map[podSeriesKey]best{}
	for _, s := range rows {
		uid := string(s.Metric["uid"])
		if uid == "" {
			continue
		}
		k := podSeriesKeyOf(s.Metric, keys)
		if k.pod == "" {
			continue
		}
		node := string(s.Metric["node"])
		cur, ok := canon[k]
		switch {
		case !ok || s.Timestamp > cur.ts || (s.Timestamp == cur.ts && uid > cur.uid):
			if ok && uid == cur.uid && node == "" {
				node = cur.node
			}
			canon[k] = best{ts: s.Timestamp, uid: uid, node: node}
		case uid == cur.uid && cur.node == "":
			cur.node = node
			canon[k] = cur
		}
	}
	out := make(map[podSeriesKey]struct{})
	for k, b := range canon {
		if _, ok := root[b.node]; ok {
			out[k] = struct{}{}
		}
	}
	return out
}

func keepPodBindings(rows model.Vector, pods map[podSeriesKey]struct{}, keys promql.LabelKeys) model.Vector {
	var out model.Vector
	for _, s := range rows {
		if _, ok := pods[podSeriesKeyOf(s.Metric, keys)]; ok && bindingClaim(s.Metric) != "" {
			out = append(out, s)
		}
	}
	return out
}

func instantAll(ctx context.Context, q promql.Querier, name promql.Query, end time.Time, queries []string) (model.Vector, error) {
	if len(queries) == 0 {
		return nil, nil
	}
	parts := make([]model.Vector, len(queries))
	wave, wctx := errgroup.WithContext(ctx)
	wave.SetLimit(scopeConcurrency)
	for i, query := range queries {
		wave.Go(func() (err error) {
			defer recoverScopedPanic(wctx, name, &err)
			out, err := q.Instant(wctx, string(name), query, end)
			if err != nil {
				return wrapQueryError(name, err)
			}
			parts[i] = out
			return nil
		})
	}
	if err := wave.Wait(); err != nil {
		return nil, err
	}
	var out model.Vector
	for _, part := range parts {
		out = append(out, part...)
	}
	return out, nil
}
