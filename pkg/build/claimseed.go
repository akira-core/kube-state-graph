package build

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// The claim root's seed (add-storage-graph-pv-pvc-roots).
//
// A pvc or pv root names the claim directly, so its seed is one read of
// kube_persistentvolumeclaim_info restricted on the roots — the same family a
// storage-side root reads second, minus the volume_labels phase 1 and the
// candidate derivation in front of it. The claims that read returns are the
// tracked set; everything after it (claim families, mounter completion,
// candidate completion, owner completion, the by-reference waves) is the shared
// expansion, unchanged.

// claimSeedGroup is one restricted query group of the seed. A pvc root reads one
// group per namespace — a claim is identified by (namespace, name), and two
// independent alternations would read every cross pair — while a pv root is one
// group over its bare, cluster-scoped names.
type claimSeedGroup struct {
	// namespace is the group's equality for a pvc root; empty for a pv root.
	namespace string
	// label is the identity label names restrict: ClaimLabel or VolumeNameLabel.
	label string
	names []string
}

// claimSeedGroups is the seed's query groups, in a fixed order: namespaces
// ascending for pvc roots, the one volume group for pv roots. A pure function of
// the plan, so the cap check and the read see the same groups.
func (p topologyPlan) claimSeedGroups() []claimSeedGroup {
	switch {
	case len(p.claimRoots) > 0:
		byNS := make(map[string][]string)
		for _, ref := range p.claimRoots {
			byNS[ref.Namespace] = append(byNS[ref.Namespace], ref.Name)
		}
		out := make([]claimSeedGroup, 0, len(byNS))
		for _, ns := range slices.Sorted(maps.Keys(byNS)) {
			out = append(out, claimSeedGroup{
				namespace: ns,
				label:     promql.ClaimLabel,
				names:     sortedNames(byNS[ns]),
			})
		}
		return out
	case len(p.volumeRoots) > 0:
		return []claimSeedGroup{{label: promql.VolumeNameLabel, names: p.volumeRoots}}
	default:
		return nil
	}
}

// reserve is what every chunk of the group repeats beyond its names: the
// request's own matchers and, for a pvc root, the namespace equality. It comes
// off the byte budget once per chunk, so the budget bounds the whole selector
// the seed sends.
func (g claimSeedGroup) reserve(keys promql.LabelKeys, sel promql.Selector) int {
	n := promql.RequestMatcherCost(promql.QPVCInfo, keys, sel)
	if g.namespace != "" {
		n += promql.NamespaceEqualityCost(g.namespace)
	}
	return n
}

// budget is the byte budget one chunk of the group's names may spend — the
// figure the cap check chunks with and issueScopedFamilies chunks with after
// taking reserve off the same total.
func (g claimSeedGroup) budget(total int, keys promql.LabelKeys, sel promql.Selector) int {
	return max(1, total-g.reserve(keys, sel))
}

// render renders one chunk of the group's names. A pvc group is
// RenderNamesInNamespace; a pv group is the family restricted on volumename.
func (g claimSeedGroup) render(window time.Duration, keys promql.LabelKeys, sel promql.Selector) func(chunk []string) (string, bool) {
	if g.namespace != "" {
		return func(chunk []string) (string, bool) {
			return promql.RenderNamesInNamespace(promql.QPVCInfo, window, keys, sel, g.namespace, g.label, chunk)
		}
	}
	return func(chunk []string) (string, bool) {
		return promql.RenderOnLabel(promql.QPVCInfo, window, keys, sel, g.label, chunk)
	}
}

// keep is the reader-side statement of which rows are roots. The restriction
// only narrows the query: a request namespace= matcher composes ahead of it, an
// exporter may return more than was asked, and a claim name is unique per
// namespace only — so the rows are filtered here, before any claim is tracked.
// A pvc row must name the group's namespace and one of its claims; a pv row
// must carry one of the root volume names.
func (g claimSeedGroup) keep() func(model.Metric) bool {
	want := make(map[string]struct{}, len(g.names))
	for _, n := range g.names {
		want[n] = struct{}{}
	}
	return func(m model.Metric) bool {
		if g.namespace != "" && string(m[promql.NamespaceLabel]) != g.namespace {
			return false
		}
		_, ok := want[string(m[model.LabelName(g.label)])]
		return ok
	}
}

// readClaimSeed is the seed of a pvc or pv root: the claim-info read,
// restricted on the roots. The claims it returns are the tracked set, and the
// vector it lands in (v.PVCInfo) is the very slot the storage-side seed's
// second step fills, so everything downstream reads it unchanged.
//
// A pvc root issues one query per namespace and a pv root one query group over
// the volume names; each is chunked under the shared byte budget however large
// it is, and never replaced by a read across the zone. Rows are merged in
// (namespace, chunk) order, so the result is a pure function of the roots and
// the upstream data.
//
// A root naming nothing loads nothing and is not an error: the build then draws
// an empty body, exactly as an unknown root of any other kind does.
func readClaimSeed(
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
	defer recoverScopedPanic(ctx, promql.QPVCInfo, &err)
	groups := plan.claimSeedGroups()
	if len(groups) == 0 {
		return nil
	}
	parts := make([]model.Vector, len(groups))
	fams := make([]claimFamily, len(groups))
	for i, g := range groups {
		fams[i] = claimFamily{
			query:         promql.QPVCInfo,
			dst:           &parts[i],
			scope:         g.names,
			keep:          g.keep(),
			render:        g.render(window, opts.LabelKeys, sel),
			budgetReserve: g.reserve(opts.LabelKeys, sel),
		}
	}
	if err := issueClaimKeyed(ctx, q, window, end, opts, sel, v, scopeMu, fams); err != nil {
		return err
	}
	var merged model.Vector
	for _, part := range parts {
		merged = append(merged, part...)
	}
	v.PVCInfo = merged
	return nil
}
