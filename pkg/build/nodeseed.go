package build

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// podSeriesKey is one pod as kube_pod_info and the claim-binding family name it.
type podSeriesKey struct {
	cluster, namespace, pod string
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
	onRoot := podsNewestOn(incarnation, plan.nodeRoots)
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
	tracked := keepPodBindings(byPod, onRoot)
	claims := claimNamesOf(tracked)
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
	lk := opts.LabelKeys.OrDefault()
	v.PVC = keepClaimBindings(v.PVC, trackedClaimKeys(tracked, lk), lk)
	return nil
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
func podsNewestOn(rows model.Vector, roots []string) map[podSeriesKey]struct{} {
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
		k := podSeriesKey{
			cluster:   string(s.Metric["cluster"]),
			namespace: string(s.Metric["namespace"]),
			pod:       string(s.Metric[promql.PodLabel]),
		}
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

func keepPodBindings(rows model.Vector, pods map[podSeriesKey]struct{}) model.Vector {
	var out model.Vector
	for _, s := range rows {
		k := podSeriesKey{
			cluster:   string(s.Metric["cluster"]),
			namespace: string(s.Metric["namespace"]),
			pod:       string(s.Metric[promql.PodLabel]),
		}
		if _, ok := pods[k]; ok && bindingClaim(s.Metric) != "" {
			out = append(out, s)
		}
	}
	return out
}

func claimNamesOf(rows model.Vector) []string {
	names := make([]string, 0, len(rows))
	for _, s := range rows {
		if claim := bindingClaim(s.Metric); claim != "" {
			names = append(names, claim)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// keepClaimBindings keeps the binding rows whose claim is one of claims, keyed
// exactly as the parse keys a claim (claimKeyOf).
func keepClaimBindings(rows model.Vector, claims map[claimKey]struct{}, keys promql.LabelKeys) model.Vector {
	var out model.Vector
	for _, s := range rows {
		if _, ok := claims[claimKeyOf(s.Metric, keys, bindingClaim(s.Metric))]; ok {
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
