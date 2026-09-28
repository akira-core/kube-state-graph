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
// restricted on the root nodes, then again restricted on pod name without the
// node matcher (incarnation completion), and keeps a pod only when its newest
// incarnation still runs on a root node. Claim bindings are read for those pod
// names and filtered to the kept (cluster, namespace, pod) keys. Bindings are
// then re-read by claim so every mounter of a tracked claim is loaded — the
// split weight and an inherited Application are computed over the same mounters
// an unrestricted read sees.
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
) error {
	onNode, err := instantAll(ctx, q, promql.QPodInfo, end, plan.nodeSeed)
	if err != nil {
		return err
	}
	if len(plan.nodeSeed) > 0 {
		// The seed read the family even when no pod runs on the root. The
		// later pod wave overwrites v.Pod when it loads the kept pods.
		markScopeIssued(v, scopeMu, promql.QPodInfo)
	}
	names := podNamesOf(onNode)
	if len(names) == 0 {
		return nil
	}
	incarnation, err := queryRendered(ctx, q, end, opts, promql.QPodInfo, names, func(chunk []string) (string, bool) {
		return promql.RenderScoped(promql.QPodInfo, window, opts.LabelKeys, sel, chunk)
	})
	if err != nil {
		return err
	}
	onRoot := podsNewestOn(incarnation, plan.nodeRoots)
	if len(onRoot) == 0 {
		return nil
	}
	podScope := make([]string, 0, len(onRoot))
	for k := range onRoot {
		podScope = append(podScope, k.pod)
	}
	slices.Sort(podScope)
	podScope = slices.Compact(podScope)
	byPod, err := queryRendered(ctx, q, end, opts, promql.QPVCBindings, podScope, func(chunk []string) (string, bool) {
		return promql.RenderClaimBindingsByPodName(window, opts.LabelKeys, sel, chunk)
	})
	if err != nil {
		return err
	}
	tracked := keepPodBindings(byPod, onRoot)
	claims := claimNamesOf(tracked)
	if len(claims) == 0 {
		v.PVC = tracked
		if len(byPod) > 0 || len(tracked) > 0 {
			markScopeIssued(v, scopeMu, promql.QPVCBindings)
		}
		return nil
	}
	if err := issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, []scopedFamily{{
		query: promql.QPVCBindings,
		dst:   &v.PVC,
		scope: claims,
	}}); err != nil {
		return err
	}
	v.PVC = keepClaimBindings(v.PVC, claimKeysOf(tracked))
	return nil
}

func podNamesOf(rows model.Vector) []string {
	names := make([]string, 0, len(rows))
	for _, s := range rows {
		if name := string(s.Metric[promql.PodLabel]); name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// podsNewestOn returns the pods whose newest incarnation runs on a root node.
// Newest is the greatest timestamp, then the lexically-largest UID, matching
// parseTopology's canonical pod. A series with no UID is ignored, as the parse
// ignores it.
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
		cur, ok := canon[k]
		if !ok || s.Timestamp > cur.ts || (s.Timestamp == cur.ts && uid > cur.uid) {
			canon[k] = best{ts: s.Timestamp, uid: uid, node: string(s.Metric["node"])}
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

type claimSeriesKey struct {
	cluster, namespace, claim string
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

func claimKeysOf(rows model.Vector) map[claimSeriesKey]struct{} {
	out := make(map[claimSeriesKey]struct{})
	for _, s := range rows {
		claim := bindingClaim(s.Metric)
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

func keepClaimBindings(rows model.Vector, claims map[claimSeriesKey]struct{}) model.Vector {
	var out model.Vector
	for _, s := range rows {
		k := claimSeriesKey{
			cluster:   string(s.Metric["cluster"]),
			namespace: string(s.Metric["namespace"]),
			claim:     bindingClaim(s.Metric),
		}
		if _, ok := claims[k]; ok {
			out = append(out, s)
		}
	}
	return out
}

// queryRendered chunks values and issues one rendered query per chunk, merged
// in chunk order. It does not mark the family issued: the caller decides what
// lands in the topology vectors.
func queryRendered(
	ctx context.Context,
	q promql.Querier,
	end time.Time,
	opts Options,
	name promql.Query,
	values []string,
	render func(chunk []string) (string, bool),
) (model.Vector, error) {
	budget := opts.qosScopeBatchBytes()
	if budget < 1 {
		budget = 1
	}
	var rendered []string
	for _, chunk := range promql.ChunkScope(values, budget) {
		query, ok := render(chunk)
		if !ok {
			continue
		}
		rendered = append(rendered, query)
	}
	return instantAll(ctx, q, name, end, rendered)
}

func instantAll(ctx context.Context, q promql.Querier, name promql.Query, end time.Time, queries []string) (model.Vector, error) {
	if len(queries) == 0 {
		return nil, nil
	}
	parts := make([]model.Vector, len(queries))
	wave, wctx := errgroup.WithContext(ctx)
	wave.SetLimit(scopeConcurrency)
	for i, query := range queries {
		wave.Go(func() error {
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
