package build

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// A claim root is a seed: the plan tracks by reference from it, reads nothing
// across the zone, and never mistakes it for a Harvest or workload seed.
func TestClaimSeed_PlanShape(t *testing.T) {
	pvc := storagePlan(vlrScope(t, graph.StorageRootPVC, []string{"shop/orders-data", "platform/queue", "shop/cache"}).Roots)
	assert.True(t, pvc.claimSeeded())
	assert.True(t, pvc.tracksByReference())
	assert.True(t, pvc.byReference)
	assert.Equal(t, []graph.ClaimRef{
		{Namespace: "platform", Name: "queue"},
		{Namespace: "shop", Name: "cache"},
		{Namespace: "shop", Name: "orders-data"},
	}, pvc.claimRoots)
	assert.Empty(t, pvc.volumeRoots)
	assert.False(t, pvc.harvestSeed(), "a claim seed reads no volume_labels phase 1")
	assert.False(t, pvc.rootedClaims())
	assert.False(t, pvc.bindingsFromSeed(), "the bindings are read by claim, not from a workload seed")

	pv := storagePlan(vlrScope(t, graph.StorageRootPV, []string{"pvc-b", "pvc-a"}).Roots)
	assert.True(t, pv.claimSeeded())
	assert.True(t, pv.tracksByReference())
	assert.Equal(t, []string{"pvc-a", "pvc-b"}, pv.volumeRoots)
	assert.Empty(t, pv.claimRoots)
	assert.False(t, pv.harvestSeed())

	for name, p := range map[string]topologyPlan{"pvc": pvc, "pv": pv} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, p.issuesFirstWave(promql.QAlerts))
			for _, l := range topologyLegs(&topologyVectors{}) {
				if l.query != promql.QAlerts {
					assert.False(t, p.issuesFirstWave(l.query), "%s is not a first-wave leg", l.query)
				}
				assert.False(t, p.issuesBesideSeed(l.query),
					"a claim root reads nothing across the zone: %s hangs off the seed", l.query)
			}
			assert.False(t, p.flowlessAggrGauges())
			assert.False(t, p.flowlessControllers())
		})
	}

	// Only the claim kinds are claim seeds, and the slices are the decision.
	assert.False(t, fullPlan.claimSeeded())
	assert.False(t, storagePlan(vlrScope(t, graph.StorageRootPod, []string{"shop/orders-0"}).Roots).claimSeeded())
	assert.False(t, storagePlan(vlrScope(t, graph.StorageRootAggr, []string{"aggr1"}).Roots).claimSeeded())
	cleared := pvc
	cleared.claimRoots = nil
	assert.False(t, cleared.claimSeeded(), "clearing the slices reads the inventory across the zone (the parity control)")
	assert.False(t, cleared.tracksByReference())
}

// An embedder fills StorageRoots itself, so a ref with an empty half is dropped
// rather than rendered into a query that can match nothing useful.
func TestClaimSeed_PlanDropsUnusableRefs(t *testing.T) {
	p := storagePlan(graph.StorageRoots{
		Kind:   graph.StorageRootPVC,
		Claims: []graph.ClaimRef{{Namespace: "shop", Name: ""}, {Namespace: "", Name: "x"}},
	})
	assert.Empty(t, p.claimRoots)
	assert.False(t, p.claimSeeded())

	v := storagePlan(graph.StorageRoots{Kind: graph.StorageRootPV, Names: []string{"", "pvc-a", ""}})
	assert.Equal(t, []string{"pvc-a"}, v.volumeRoots)
}

// Spec: "Bounded root sets" — a request-derived scope past the chunk cap is
// rejected before a single query, and never replaced by a read across the zone.
func TestClaimSeed_OverCapRejected(t *testing.T) {
	over := maxRootedVolumeLabelChunks + 1

	t.Run("pvc in one namespace", func(t *testing.T) {
		refs := make([]string, over)
		for i := range refs {
			refs[i] = "shop/claim-" + itoa(i)
		}
		assertClaimSeedRejected(t, graph.StorageRootPVC, refs)
	})

	// The cap counts queries across every namespace: each namespace is its own
	// query group, so many namespaces with a few claims each exceed it as surely
	// as one namespace with many.
	t.Run("pvc across namespaces", func(t *testing.T) {
		var refs []string
		for i := 0; i <= maxRootedVolumeLabelChunks/2; i++ {
			refs = append(refs, "ns-"+itoa(i)+"/a", "ns-"+itoa(i)+"/b")
		}
		require.Greater(t, len(refs), maxRootedVolumeLabelChunks)
		assertClaimSeedRejected(t, graph.StorageRootPVC, refs)
	})

	t.Run("pv", func(t *testing.T) {
		names := make([]string, over)
		for i := range names {
			names[i] = "pvc-" + strings.Repeat("x", 8) + itoa(i)
		}
		assertClaimSeedRejected(t, graph.StorageRootPV, names)
	})
}

func assertClaimSeedRejected(t *testing.T, kind graph.StorageRootKind, values []string) {
	t.Helper()
	q := promqlfake.New(nil)
	_, err := New(q, Options{QoSScopeBatchBytes: 1}, nil, nil).BuildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel, vlrScope(t, kind, values).Roots)
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Contains(t, err.Error(), RootScopeCapMessage)
	assert.Empty(t, q.Issued(), "rejected before any upstream query")
}

// A root set exactly at the cap is served.
func TestClaimSeed_AtCapIsServed(t *testing.T) {
	refs := make([]string, maxRootedVolumeLabelChunks)
	for i := range refs {
		refs[i] = "shop/claim-" + itoa(i)
	}
	q := promqlfake.New(nil)
	_, err := New(q, Options{QoSScopeBatchBytes: 1}, nil, nil).BuildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel, vlrScope(t, graph.StorageRootPVC, refs).Roots)
	require.NoError(t, err)
}

// claimSeedRun drives readClaimSeed alone against q and returns the vectors it
// wrote, so a test states only the seed's own behaviour.
func claimSeedRun(t *testing.T, q promql.Querier, kind graph.StorageRootKind, values []string) (*topologyVectors, error) {
	t.Helper()
	plan := storagePlan(vlrScope(t, kind, values).Roots)
	var v topologyVectors
	var mu sync.Mutex
	err := readClaimSeed(t.Context(), q, time.Minute, vlrEnd, Options{}, vlrSel, plan, &v, &mu)
	return &v, err
}

func claimInfoRow(ns, claim, volume string) *model.Sample {
	return planKSM("namespace", ns, "persistentvolumeclaim", claim, "volumename", volume, "storageclass", "netapp-nas")
}

// Spec: "A claim root reads its claim by reference" — one query per namespace,
// the namespace as an equality and its claim names as the alternation, beside
// the request matchers, and no other family.
func TestClaimSeed_PVCReadsClaimInfoPerNamespace(t *testing.T) {
	q := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCInfo: {
			claimInfoRow("shop", "orders-data", "pvc-1"),
			claimInfoRow("shop", "cache", "pvc-2"),
			claimInfoRow("platform", "queue", "pvc-3"),
			claimInfoRow("platform", "cache", "pvc-4"),
		},
	})
	v, err := claimSeedRun(t, q, graph.StorageRootPVC, []string{"shop/orders-data", "shop/cache", "platform/queue"})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{
		`last_over_time(kube_persistentvolumeclaim_info{az="zone-a",env="prod",namespace="platform",persistentvolumeclaim="queue"}[1m])`,
		`last_over_time(kube_persistentvolumeclaim_info{az="zone-a",env="prod",namespace="shop",persistentvolumeclaim=~"cache|orders-data"}[1m])`,
	}, q.QueriesFor(promql.QPVCInfo))
	assert.Len(t, q.Issued(), 2, "the seed issues the claim-info family and nothing else — no volume_labels precedes it")

	assert.ElementsMatch(t, []string{"pvc-1", "pvc-2", "pvc-3"}, volumeNamesOf(v.PVCInfo),
		"platform/cache is a claim no root names: it is never read")
}

// Spec: "A volume root reads its claim by volume name" — restricted on volumename
// verbatim, sorted, beside the request matchers.
func TestClaimSeed_PVReadsClaimInfoByVolumeName(t *testing.T) {
	q := promqlfake.New(map[promql.Query]model.Vector{
		promql.QPVCInfo: {
			claimInfoRow("shop", "orders-data", "pvc-ab12-cd34"),
			claimInfoRow("db", "mongo-data", "mongo-data-01"),
			claimInfoRow("shop", "other", "pvc-zzzz"),
		},
	})
	v, err := claimSeedRun(t, q, graph.StorageRootPV, []string{"pvc-ab12-cd34", "mongo-data-01"})
	require.NoError(t, err)

	assert.Equal(t, []string{
		`last_over_time(kube_persistentvolumeclaim_info{az="zone-a",env="prod",volumename=~"mongo-data-01|pvc-ab12-cd34"}[1m])`,
	}, q.QueriesFor(promql.QPVCInfo))
	assert.Len(t, q.Issued(), 1)
	assert.ElementsMatch(t, []string{"pvc-ab12-cd34", "mongo-data-01"}, volumeNamesOf(v.PVCInfo))
}

// The restriction is composed with the request's namespace matcher, not
// replaced by it, so the reader is the one statement of which rows are roots.
func TestClaimSeed_RequestNamespaceMatcherComposes(t *testing.T) {
	q := promqlfake.New(nil)
	sel := vlrSel
	sel.Namespace = []string{"shop", "platform"}
	plan := storagePlan(vlrScope(t, graph.StorageRootPVC, []string{"shop/orders-data"}).Roots)
	var v topologyVectors
	var mu sync.Mutex
	require.NoError(t, readClaimSeed(t.Context(), q, time.Minute, vlrEnd, Options{}, sel, plan, &v, &mu))
	assert.Equal(t, []string{
		`last_over_time(kube_persistentvolumeclaim_info{az="zone-a",env="prod",namespace=~"platform|shop",namespace="shop",persistentvolumeclaim="orders-data"}[1m])`,
	}, q.QueriesFor(promql.QPVCInfo))
}

// stubQuerier answers every query with the same rows, ignoring the query's
// matchers. It stands in for an upstream returning more than the restriction
// asked for, which is what the reader-side filter exists to survive.
type stubQuerier struct {
	rows model.Vector
	mu   sync.Mutex
	n    int
}

func (s *stubQuerier) Instant(_ context.Context, _, _ string, _ time.Time) (model.Vector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return s.rows, nil
}

// Spec: "A same-named claim in another namespace is filtered out" — a row the
// query returned that no root names is dropped before any claim is tracked.
func TestClaimSeed_PVCDropsRowsNoRootNames(t *testing.T) {
	q := &stubQuerier{rows: model.Vector{
		claimInfoRow("shop", "data", "pvc-shop"),
		claimInfoRow("platform", "data", "pvc-platform"),
		claimInfoRow("shop", "other", "pvc-other"),
	}}
	v, err := claimSeedRun(t, q, graph.StorageRootPVC, []string{"shop/data"})
	require.NoError(t, err)
	assert.Equal(t, []string{"pvc-shop"}, volumeNamesOf(v.PVCInfo),
		"only (shop, data) is a root: same name in another namespace and another name in the root namespace are dropped")
}

// A pvc root names a claim in every Kubernetes cluster of the estate: cluster is
// not part of the ref, so a same-named claim in two clusters is two roots.
func TestClaimSeed_PVCMatchesEveryCluster(t *testing.T) {
	c1 := claimInfoRow("shop", "data", "pvc-c1")
	c2 := claimInfoRow("shop", "data", "pvc-c2")
	c2.Metric["cluster"] = "c2"
	q := &stubQuerier{rows: model.Vector{c1, c2}}
	v, err := claimSeedRun(t, q, graph.StorageRootPVC, []string{"shop/data"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"pvc-c1", "pvc-c2"}, volumeNamesOf(v.PVCInfo))
}

// Spec: "Unknown claim root is not drawn" — a pv naming no claim loads nothing,
// and neither does a row whose volumename is not a root.
func TestClaimSeed_PVNamingNoClaimLoadsNothing(t *testing.T) {
	v, err := claimSeedRun(t, promqlfake.New(nil), graph.StorageRootPV, []string{"pvc-typo"})
	require.NoError(t, err)
	assert.Empty(t, v.PVCInfo)

	stub := &stubQuerier{rows: model.Vector{claimInfoRow("shop", "data", "pvc-other")}}
	v, err = claimSeedRun(t, stub, graph.StorageRootPV, []string{"pvc-typo"})
	require.NoError(t, err)
	assert.Empty(t, v.PVCInfo, "a row whose volumename is not a root is dropped by the reader")
}

// A claim is identified by (namespace, name): a pv-root row that names no
// namespace is dropped by the reader instead of becoming a `<cluster>//<claim>`
// node.
func TestClaimSeed_PVDropsRowsWithNoNamespace(t *testing.T) {
	q := &stubQuerier{rows: model.Vector{
		claimInfoRow("", "data", "pvc-x"),
		claimInfoRow("shop", "data", "pvc-x"),
	}}
	v, err := claimSeedRun(t, q, graph.StorageRootPV, []string{"pvc-x"})
	require.NoError(t, err)
	require.Len(t, v.PVCInfo, 1)
	assert.Equal(t, "shop", string(v.PVCInfo[0].Metric[promql.NamespaceLabel]))
}

// The tally records the family when the seed issued it — including at zero
// rows, so an issued query is never reported as one that never ran.
func TestClaimSeed_MarksTheFamilyIssued(t *testing.T) {
	v, err := claimSeedRun(t, promqlfake.New(nil), graph.StorageRootPVC, []string{"shop/nope"})
	require.NoError(t, err)
	assert.True(t, v.ScopeIssued[promql.QPVCInfo])
	assert.Empty(t, v.PVCInfo)
}

// A failed seed read fails the build: /v1/storage-graph fails closed, and a
// missing claim read would render as a smaller, plausible, wrong estate.
func TestClaimSeed_QueryErrorFails(t *testing.T) {
	q := promqlfake.New(nil)
	q.Fail = func(name, _ string) error {
		if name == string(promql.QPVCInfo) {
			return assert.AnError
		}
		return nil
	}
	_, err := claimSeedRun(t, q, graph.StorageRootPV, []string{"pvc-a"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(promql.QPVCInfo))
}

// A data-derived root scope is chunked however large it is: names past one
// chunk are all read, never dropped.
func TestClaimSeed_ChunksUnderTheByteBudget(t *testing.T) {
	names := []string{"pvc-aaaaaaaa", "pvc-bbbbbbbb", "pvc-cccccccc"}
	q := promqlfake.New(nil)
	plan := storagePlan(vlrScope(t, graph.StorageRootPV, names).Roots)
	var v topologyVectors
	var mu sync.Mutex
	require.NoError(t, readClaimSeed(t.Context(), q, time.Minute, vlrEnd, Options{QoSScopeBatchBytes: 1}, vlrSel, plan, &v, &mu))
	assert.Len(t, q.QueriesFor(promql.QPVCInfo), len(names), "one name per chunk under a one-byte budget")
	assert.ElementsMatch(t, names, flattenScope(q.ScopeValues(promql.QPVCInfo, promql.VolumeNameLabel)))
}

func volumeNamesOf(rows model.Vector) []string {
	out := make([]string, 0, len(rows))
	for _, s := range rows {
		out = append(out, string(s.Metric[promql.VolumeNameLabel]))
	}
	return out
}

func flattenScope(groups [][]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// claimSeedEstate is the takeover corpus: claim shop/orders-data (PV pvc-orders,
// mounted by shop/orders-0 and shop/orders-1) joins ontap-prod / aggr1 /
// svm_shop, and the estate holds claims, pods and filers no claim root names.
func claimSeedEstate() map[promql.Query]model.Vector {
	return vlrEstate(vlrHarvest([]vlrVol{
		{"trident_pvc_orders", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
		{"trident_pvc_redis", "ontap-prod", "ontap-prod-02", "aggr2", "svm_platform", 100},
	}))
}

func buildClaimSeed(t *testing.T, fx map[promql.Query]model.Vector, kind graph.StorageRootKind, values []string) (*graph.Graph, *promqlfake.Querier) {
	t.Helper()
	q := promqlfake.New(fx)
	g, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, kind, values).Roots)
	require.NoError(t, err)
	return g, q
}

// Spec: "Claim root finds its storage and its consumers" — the build loads the
// root claim, every mounter of it, and its storage chain, and nothing else.
func TestClaimSeed_BuildLoadsTheRootClaimAndItsChain(t *testing.T) {
	for name, tc := range map[string]struct {
		kind   graph.StorageRootKind
		values []string
	}{
		"pvc": {graph.StorageRootPVC, []string{"shop/orders-data"}},
		"pv":  {graph.StorageRootPV, []string{"pvc-orders"}},
	} {
		t.Run(name, func(t *testing.T) {
			g, q := buildClaimSeed(t, claimSeedEstate(), tc.kind, tc.values)

			for _, id := range []string{
				"zone-a-prod-c1/shop/orders-data",
				"zone-a-prod-c1/uid-o0", "zone-a-prod-c1/uid-o1", // both mounters: mounter completion
				"zone-a-prod-c1/worker-1", "zone-a-prod-c1/worker-2",
				"netapp/ontap-prod/aggr/aggr1", "netapp/ontap-prod/ontap-prod-01",
			} {
				assert.Contains(t, g.NodesByID, id)
			}
			for _, id := range []string{
				"zone-a-prod-c1/platform/redis-data", // a claim no root names
				"zone-a-prod-c1/uid-w0",              // a pod that mounts nothing of the root
				"netapp/ontap-prod/aggr/aggr2",       // a filer component the root does not reach
			} {
				assert.NotContains(t, g.NodesByID, id, "%s is not reached from the root", id)
			}

			// The seed IS the claim-info read: issued once, before any volume_labels
			// read, and not issued again by an expansion step.
			assert.Len(t, q.QueriesFor(promql.QPVCInfo), 1)
			firstInfo, firstVolumes := -1, -1
			for i, is := range q.Issued() {
				switch is.Name {
				case string(promql.QPVCInfo):
					if firstInfo < 0 {
						firstInfo = i
					}
				case string(promql.QVolumeLabels):
					if firstVolumes < 0 {
						firstVolumes = i
					}
				}
			}
			require.GreaterOrEqual(t, firstInfo, 0)
			require.GreaterOrEqual(t, firstVolumes, 0, "candidate completion reads volume_labels by the claim's token")
			assert.Less(t, firstInfo, firstVolumes, "no volume_labels query precedes the claim read")

			// The expansion reads the claim families by (namespace, claim),
			// mounters included.
			assert.Contains(t, q.QueriesFor(promql.QPVCBindings),
				`last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info{az="zone-a",env="prod",namespace="shop",persistentvolumeclaim="orders-data"}[1m])`)
		})
	}
}

// Spec: "Unknown claim root is not drawn" and "Candidates naming no claim are
// reported": a root naming nothing issues no binding, pod, node or controller
// query, and the build is an empty graph — never a read across the zone.
func TestClaimSeed_UnknownRootReadsNothingElse(t *testing.T) {
	for name, tc := range map[string]struct {
		kind   graph.StorageRootKind
		values []string
	}{
		"pvc": {graph.StorageRootPVC, []string{"shop/typo"}},
		"pv":  {graph.StorageRootPV, []string{"pvc-typo"}},
	} {
		t.Run(name, func(t *testing.T) {
			g, q := buildClaimSeed(t, claimSeedEstate(), tc.kind, tc.values)
			assert.Empty(t, g.NodesByID)
			assert.Empty(t, g.Edges)
			for _, is := range q.Issued() {
				assert.Contains(t, []string{string(promql.QAlerts), string(promql.QPVCInfo)}, is.Name,
					"%s must not be read for a root that names no claim", is.Name)
			}
		})
	}
}

// A failed seed fails the build with the family's name; it must not leave a
// downstream wave waiting on a channel nobody closes. Run under -race -count.
func TestClaimSeed_FailingSeedReturnsInsteadOfBlocking(t *testing.T) {
	for name, kind := range map[string]graph.StorageRootKind{"pvc": graph.StorageRootPVC, "pv": graph.StorageRootPV} {
		t.Run(name, func(t *testing.T) {
			q := promqlfake.New(claimSeedEstate())
			q.Fail = func(name, _ string) error {
				if name == string(promql.QPVCInfo) {
					return assert.AnError
				}
				return nil
			}
			values := []string{"shop/orders-data"}
			if kind == graph.StorageRootPV {
				values = []string{"pvc-orders"}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, err := New(q, Options{}, nil, nil).BuildStorage(ctx, time.Minute, vlrEnd, vlrSel, vlrScope(t, kind, values).Roots)
			require.Error(t, err)
			require.NoError(t, ctx.Err(), "the build returned on the seed's error instead of running into the deadline")
			assert.Contains(t, err.Error(), string(promql.QPVCInfo))
		})
	}
}

// A failure in the claim families that follow the seed also returns.
func TestClaimSeed_FailingClaimFamilyReturnsInsteadOfBlocking(t *testing.T) {
	q := promqlfake.New(claimSeedEstate())
	q.Fail = func(name, _ string) error {
		if name == string(promql.QPVCBindings) {
			return assert.AnError
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := New(q, Options{}, nil, nil).BuildStorage(ctx, time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootPVC, []string{"shop/orders-data"}).Roots)
	require.Error(t, err)
	require.NoError(t, ctx.Err())
}

// claimSeedEstateWithOrphan adds claim shop/orphan-data — bound to PV pvc-orphan,
// on ontap-prod / aggr1 / svm_shop, mounted by no pod.
func claimSeedEstateWithOrphan() map[promql.Query]model.Vector {
	fx := vlrEstate(vlrHarvest([]vlrVol{
		{"trident_pvc_orders", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
		{"trident_pvc_orphan", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 40},
	}))
	fx[promql.QPVCInfo] = append(fx[promql.QPVCInfo],
		planKSM("namespace", "shop", "persistentvolumeclaim", "orphan-data", "volumename", "pvc-orphan", "storageclass", "netapp-nas"))
	return fx
}

func claimTierEdges(g *graph.Graph, tier string) []string {
	var out []string
	for _, e := range g.Edges {
		if e.Type == graph.EdgeTypeStorageFlow && e.Labels["tier"] == tier {
			out = append(out, e.Source+" -> "+e.Target)
		}
	}
	slices.Sort(out)
	return out
}

// Spec: "Unmounted root claim keeps its storage-side path" — only a claim-seeded
// build draws the chain of an unmounted claim, so the built graph of every other
// root kind is what it was.
func TestClaimSeed_UnmountedChainIsDrawnOnlyUnderAClaimSeed(t *testing.T) {
	const svm = "netapp/ontap-prod/svm/svm_shop"
	orphan := svm + " -> zone-a-prod-c1/shop/orphan-data"

	pvc, _ := buildClaimSeed(t, claimSeedEstateWithOrphan(), graph.StorageRootPVC, []string{"shop/orphan-data"})
	assert.Equal(t, []string{orphan}, claimTierEdges(pvc, graph.StorageTierSVMPVC))
	assert.Equal(t, []string{"netapp/ontap-prod/aggr/aggr1 -> " + svm}, claimTierEdges(pvc, graph.StorageTierAggrSVM))
	assert.Empty(t, claimTierEdges(pvc, graph.StorageTierPVCPod))

	pv, _ := buildClaimSeed(t, claimSeedEstateWithOrphan(), graph.StorageRootPV, []string{"pvc-orphan"})
	assert.Equal(t, claimTierEdges(pvc, graph.StorageTierSVMPVC), claimTierEdges(pv, graph.StorageTierSVMPVC))

	// The aggregate root reads the same claim (a candidate of its FlexVol), but
	// an unmounted claim is no node outside a claim seed, so only the mounted
	// claim's chain is drawn.
	aggr, _ := buildClaimSeed(t, claimSeedEstateWithOrphan(), graph.StorageRootAggr, []string{"aggr1"})
	assert.NotContains(t, aggr.NodesByID, "zone-a-prod-c1/shop/orphan-data")
	assert.Equal(t, []string{svm + " -> zone-a-prod-c1/shop/orders-data"}, claimTierEdges(aggr, graph.StorageTierSVMPVC),
		"an unmounted claim's chain is not drawn under an aggregate root")
}

// A root claim that no pod mounts has no binding to materialise it, so a
// claim-seeded parse builds its PVC node from the claim-info row. Every other
// parse still materialises a PVC only from a binding: /v1/graph and the other
// root kinds draw no unmounted claim.
func TestParseTopology_MaterialisesUnboundClaimsOnlyWhenClaimSeeded(t *testing.T) {
	unbound := func(seeded bool) Topology {
		v := topologyVectors{
			PVCInfo: model.Vector{
				planKSM("namespace", "shop", "persistentvolumeclaim", "orphan-b", "volumename", "pvc-b", "storageclass", "netapp-nas"),
				planKSM("namespace", "shop", "persistentvolumeclaim", "orphan-a", "volumename", "pvc-a", "storageclass", "netapp-nas"),
			},
			VolumeLabels: sampleVec(
				volLabelSample("pvc-a", "oc", "n1", "a1", "svm-a"),
				volLabelSample("pvc-b", "oc", "n1", "", "svm-b"),
			),
			KubeletVolumeUsed:        model.Vector{withValue(planKSM("namespace", "shop", "persistentvolumeclaim", "orphan-a"), 50)},
			MaterialiseUnboundClaims: seeded,
		}
		return parseTopology(v, promql.LabelKeys{})
	}

	assert.Empty(t, unbound(false).PVCs, "outside a claim seed an unmounted claim is not a node")
	assert.False(t, unbound(false).ClaimSeeded)

	tp := unbound(true)
	assert.True(t, tp.ClaimSeeded, "the parse carries the fact to assembleStorageFlow")
	require.Len(t, tp.PVCs, 2)
	assert.Equal(t, "zone-a-prod-c1/shop/orphan-a", tp.PVCs[0].ID(), "sorted, so the result never depends on vector order")
	assert.Equal(t, "zone-a-prod-c1/shop/orphan-b", tp.PVCs[1].ID())
	a := tp.PVCs[0]
	assert.Equal(t, "pvc-a", a.Labels()["volumename"])
	assert.Equal(t, "svm-a", a.Labels()["svm"], "the Harvest join runs over the unbound claim")
	assert.Equal(t, "netapp/oc/aggr/a1", a.Labels()["aggr"])
	assert.Equal(t, "netapp-nas", a.StorageClass())
	require.NotNil(t, a.Usage())
	assert.Contains(t, tp.SVMByPVC, a.ID())
	assert.Contains(t, tp.ClustersObserved, "zone-a-prod-c1")
	assert.Empty(t, tp.PodPVCs, "no pod mounts either claim")
	require.Len(t, tp.StorageEdges, 1, "orphan-a joins an aggregate; the FlexGroup-shaped orphan-b has no edge")
	assert.Equal(t, a.ID(), tp.StorageEdges[0].Source)

	// A mounted claim is unchanged, and is not duplicated by its claim-info row.
	v := pvcBindingVectors("c1", "shop", "orders-0", "orders-data", "pvc-x")
	v.MaterialiseUnboundClaims = true
	mounted := parseTopology(v, promql.LabelKeys{})
	require.Len(t, mounted.PVCs, 1)
	require.Len(t, mounted.PodPVCs, 1)
}

func claimSeedBody(t *testing.T, fx map[promql.Query]model.Vector, kind graph.StorageRootKind, values []string) cytoscape.Body {
	t.Helper()
	scope := vlrScope(t, kind, values)
	g, _ := buildClaimSeed(t, fx, kind, values)
	return cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
}

// Spec: "Claim root finds its storage and its consumers" / "Volume root resolves
// to its claim". For a claim with one mounter the `?pvc=` body draws exactly the
// paths a `?pod=` root on that mounter draws for the claim, and `?pv=` draws the
// body `?pvc=` does.
func TestClaimSeed_BodyParity(t *testing.T) {
	fx := claimSeedEstate()

	pvc := claimSeedBody(t, fx, graph.StorageRootPVC, []string{"platform/redis-data"})
	require.NotEmpty(t, pvc.Elements.Nodes, "a vacuous body would prove nothing")
	require.NotEmpty(t, pvc.Elements.Edges)

	pod := claimSeedBody(t, fx, graph.StorageRootPod, []string{"platform/redis-0"})
	assert.JSONEq(t, planBodyJSON(t, pod), planBodyJSON(t, pvc), "?pvc= equals ?pod= for a singly-mounted claim")

	pv := claimSeedBody(t, fx, graph.StorageRootPV, []string{"pvc-redis"})
	assert.JSONEq(t, planBodyJSON(t, pvc), planBodyJSON(t, pv), "?pv= equals ?pvc=")

	// A claim with two mounters: the claim root draws BOTH paths, each pod root
	// draws its own share, and every pod-root path is a subset of the claim's.
	both := claimSeedBody(t, fx, graph.StorageRootPVC, []string{"shop/orders-data"})
	for _, pod := range []string{"shop/orders-0", "shop/orders-1"} {
		one := claimSeedBody(t, fx, graph.StorageRootPod, []string{pod})
		for _, e := range one.Elements.Edges {
			assert.Contains(t, edgeIDsOf(both), e.Data.ID, "%s: %s -> %s", pod, e.Data.Source, e.Data.Target)
		}
	}
}

func edgeIDsOf(b cytoscape.Body) []string {
	out := make([]string, 0, len(b.Elements.Edges))
	for _, e := range b.Elements.Edges {
		out = append(out, e.Data.ID)
	}
	return out
}
