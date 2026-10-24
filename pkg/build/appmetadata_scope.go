package build

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// appMetadataRestrictionReserve is the byte cost every chunk of a restricted
// application-metadata read pays once: the fixed matchers (matcherCost), the
// comma that joins them to the restriction when any matcher is present, and
// the `label=~""` wrapper. ChunkScopeWithOverhead counts only the escaped
// values and their separators, so this reserve is what keeps the rendered
// selector inside the byte budget. The regex wrapper is one byte longer than
// a single-value equality, which keeps a one-value chunk inside the budget
// too. label is the configured namespace label or the configured app label.
func appMetadataRestrictionReserve(label string, matcherCost int) int {
	n := matcherCost
	if n > 0 {
		n++
	}
	n += len(label) + len(`=~""`)
	return n
}

// appMetadataRootChunks splits root Applications under the byte budget with
// the app-label wrapper reserved once per chunk. The same split is what
// issueScopedFamilies applies when the seed sets budgetReserve to
// appMetadataRestrictionReserve(src.App), so the bound check and the issued
// queries agree.
func appMetadataRootChunks(src promql.AppMetadataSource, keys promql.LabelKeys, sel promql.Selector, values []string, budget int) [][]string {
	b := max(budget-appMetadataRestrictionReserve(src.App, promql.AppMetadataMatcherCost(src, keys, sel)), 1)
	return promql.ChunkScopeWithOverhead(values, b, 0)
}

// appMetadataSeedActive reports whether an application= build should issue
// the metric seed. An empty app label cannot be restricted and must not fall
// back to an unscoped read. An unserved family is the unset metric.
func appMetadataSeedActive(src *promql.AppMetadataSource, q promql.Querier) bool {
	return src != nil && src.App != "" && appMetadataServed(q)
}

// appMetadataSeedCap rejects an application root whose metric seed would
// exceed maxApplicationRootChunks. An inactive seed does not count: the
// tracking-id cap is a separate check, and a set that fits that cap can still
// exceed this one because the app-label wrapper is wider.
func appMetadataSeedCap(opts Options, sel promql.Selector, roots []string, q promql.Querier) error {
	src := opts.AppMetadata
	if !appMetadataSeedActive(src, q) || len(roots) == 0 {
		return nil
	}
	if len(appMetadataRootChunks(*src, opts.LabelKeys, sel, roots, opts.qosScopeBatchBytes())) > maxApplicationRootChunks {
		return NewError(ReasonInvalidScope, RootScopeCapMessage, nil)
	}
	return nil
}

// rejectWideAppMetadataSeed is appMetadataSeedCap for a storage plan. It is a
// no-op for every other root kind. readTopology returns the error before any
// query is issued.
func rejectWideAppMetadataSeed(plan topologyPlan, opts Options, sel promql.Selector, q promql.Querier) error {
	if plan.kind != graph.StorageRootApplication || len(plan.applicationRoots) == 0 {
		return nil
	}
	return appMetadataSeedCap(opts, sel, plan.applicationRoots, q)
}

// appMetadataServed reports whether this build should issue the metric.
//
// A querier that does not implement FamilyServer is treated as served. The
// storage build then issues whenever a namespace was loaded, and the
// /v1/graph leg is added, since neither has a routing table to consult. An
// explicit Serves(FamilyAppMeta) == false is the unserved table: both builds
// skip the metric.
// The on-demand lookup uses the opposite default and must not
// share this helper.
func appMetadataServed(q promql.Querier) bool {
	fs, ok := q.(promql.FamilyServer)
	return !ok || fs.Serves(promql.FamilyAppMeta)
}

// appMetadataClusterScope is the namespaces one bucketed cluster loaded.
type appMetadataClusterScope struct {
	cluster    string
	namespaces []string
}

// clusterNamespace is one namespace of one bucketed cluster.
type clusterNamespace struct {
	cluster, namespace string
}

// appMetadataScopes groups the namespaces of the loaded pods and claims by
// their bucketed cluster, sorted by cluster with each namespace list sorted
// and de-duplicated. Both labels are kube-state-metrics' (cluster and
// namespace), not the metric's configured namespace label. A namespace is
// only ever paired with a cluster that loaded it, so another cluster's
// same-named namespace is not read. Samples are read only.
func appMetadataScopes(pods, claims model.Vector) []appMetadataClusterScope {
	return appMetadataScopesOf(appendClusterNamespaces(nil, pods, claims))
}

// appendClusterNamespaces appends the bucketed (cluster, namespace) of every
// row of vecs to dst. It is the one place a row's cluster is bucketed for the
// application-metadata scope, so the storage and Kubernetes-only builds cannot
// bucket it differently. Samples are read only.
func appendClusterNamespaces(dst []clusterNamespace, vecs ...model.Vector) []clusterNamespace {
	for _, vec := range vecs {
		for _, s := range vec {
			dst = append(dst, clusterNamespace{
				cluster:   bucketCluster(string(s.Metric["cluster"])),
				namespace: string(s.Metric[promql.NamespaceLabel]),
			})
		}
	}
	return dst
}

// appMetadataScopesOf is appMetadataScopes over (cluster, namespace) pairs the
// caller already holds, for a build that knows its scope before the rows are
// loaded. The clusters must already be bucketed.
func appMetadataScopesOf(pairs []clusterNamespace) []appMetadataClusterScope {
	byCluster := make(map[string][]string)
	for _, p := range pairs {
		byCluster[p.cluster] = append(byCluster[p.cluster], p.namespace)
	}
	out := make([]appMetadataClusterScope, 0, len(byCluster))
	for _, c := range slices.Sorted(maps.Keys(byCluster)) {
		if names := sortedNames(byCluster[c]); len(names) > 0 {
			out = append(out, appMetadataClusterScope{cluster: c, namespaces: names})
		}
	}
	return out
}

// appMetadataActive reports whether a by-reference build reads the
// namespace-restricted metric at all: the metric is configured with a
// namespace label and the family is served.
func appMetadataActive(src *promql.AppMetadataSource, q promql.Querier) bool {
	return src != nil && src.Namespace != "" && appMetadataServed(q)
}

// readScopedAppMetadata issues the application-metadata metric restricted to
// the (cluster, namespace) pairs of the loaded pods and claims:
// one restriction per cluster, each carrying only that cluster's namespaces.
//
// It waits on both waves so the two vectors are complete, then runs beside
// readScopedControllers. An unset metric, a querier that does not serve
// appmeta, an empty namespace label, or a build that loaded no namespace
// issues nothing. The unscoped first-wave leg stays skipped by
// storageSkippedLegs; this function does not consult that skip set. A chunk
// error fails the build. The scope is data-derived: chunked under the shared
// byte budget, never widened, never rejected. The per-cluster vectors are
// concatenated in cluster order into v.AppMetadata.
func readScopedAppMetadata(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	podsDone, pvcInfoDone <-chan struct{},
) error {
	if !appMetadataActive(opts.AppMetadata, q) {
		return nil
	}
	if !waitSignal(ctx, podsDone) || !waitSignal(ctx, pvcInfoDone) {
		return nil
	}
	scopes := appMetadataScopes(v.Pod, v.PVCInfo)
	if len(scopes) == 0 {
		return nil
	}
	rows, err := issueAppMetadata(ctx, q, window, end, opts, sel, v, scopeMu, scopes)
	if err != nil {
		return err
	}
	v.AppMetadata = rows
	return nil
}

// issueAppMetadata reads the metric once per cluster scope, each restriction
// carrying that cluster's matcher and only its namespaces, chunked under the
// shared byte budget, and returns the per-cluster vectors concatenated in
// cluster order. It marks the family issued when a chunk reached the upstream.
func issueAppMetadata(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	scopes []appMetadataClusterScope,
) (model.Vector, error) {
	src := opts.AppMetadata
	parts := make([]model.Vector, len(scopes))
	families := make([]scopedFamily, len(scopes))
	for i, sc := range scopes {
		families[i] = scopedFamily{
			query: promql.QAppMetadata,
			dst:   &parts[i],
			scope: sc.namespaces,
			budgetReserve: appMetadataRestrictionReserve(src.Namespace,
				promql.AppMetadataClusterMatcherCost(*src, opts.LabelKeys, sel, sc.cluster)),
			render: func(chunk []string) (string, bool) {
				return promql.RenderAppMetadataByNamespace(*src, window, opts.LabelKeys, sel, sc.cluster, chunk)
			},
		}
	}
	if err := issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, families); err != nil {
		return nil, err
	}
	return slices.Concat(parts...), nil
}

// appMetadataPodKeys reads the (az, env, cluster, namespace, pod) of every
// seeded row whose app is an exact root value. Namespace and pod come from
// the configured labels, not the kube-state-metrics names; az, env and the
// bucketed cluster label match podSeriesKeyOf so a seeded pod joins the
// binding rows of the same pod. An empty pod is skipped. Samples are read
// only. The result is sorted and de-duplicated.
func appMetadataPodKeys(rows model.Vector, src promql.AppMetadataSource, roots map[string]struct{}, keys promql.LabelKeys) []podSeriesKey {
	keys = keys.OrDefault()
	nsLabel := model.LabelName(src.Namespace)
	podLabel := model.LabelName(src.Pod)
	appLabel := model.LabelName(src.App)
	out := make([]podSeriesKey, 0, len(rows))
	for _, s := range rows {
		if s == nil {
			continue
		}
		app := string(s.Metric[appLabel])
		if _, ok := roots[app]; !ok {
			continue
		}
		pod := string(s.Metric[podLabel])
		if pod == "" {
			continue
		}
		out = append(out, podSeriesKey{
			az:        string(s.Metric[model.LabelName(keys.AZ)]),
			env:       string(s.Metric[model.LabelName(keys.Env)]),
			cluster:   bucketCluster(string(s.Metric["cluster"])),
			namespace: string(s.Metric[nsLabel]),
			pod:       pod,
		})
	}
	slices.SortFunc(out, comparePodSeriesKeys)
	return slices.Compact(out)
}

// unionPodSeriesKeys returns the sorted, de-duplicated union of two pod-key
// sets. Neither input is mutated.
func unionPodSeriesKeys(a, b []podSeriesKey) []podSeriesKey {
	out := slices.Concat(a, b)
	slices.SortFunc(out, comparePodSeriesKeys)
	return slices.Compact(out)
}
