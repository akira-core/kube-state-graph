package build

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// ---------------------------------------------------------------- 2. mode gate

func TestVolumeModeTokenScope_EveryModeClassified(t *testing.T) {
	// A mode added to VolumeMatchModes must be classified deliberately, so a new
	// mode can never silently default into (or out of) the rooted read.
	for _, m := range VolumeMatchModes {
		_, ok := volumeModeTokenScope[m]
		assert.True(t, ok, "match mode %q has no token-scope classification", m)
	}
	assert.Len(t, volumeModeTokenScope, len(VolumeMatchModes),
		"the table names no mode the enum does not")
}

func TestVolumeKeyRewriter_TokenScope(t *testing.T) {
	want := map[VolumeMatchMode]tokenScopeKind{
		VolumeMatchExact:    tokenScopeExact,
		VolumeMatchSuffix:   tokenScopeSuffix,
		VolumeMatchContains: tokenScopeNone,
		VolumeMatchRegex:    tokenScopeNone,
	}
	for mode, kind := range want {
		rw, err := NewVolumeKeyRewriter(nil, mode)
		require.NoError(t, err)
		assert.Equal(t, kind, rw.tokenScope(), string(mode))
	}
	assert.Equal(t, tokenScopeSuffix, defaultVolumeKeyRewriter().tokenScope(),
		"the default mode is suffix, which is scopeable")
	assert.Equal(t, tokenScopeNone, (*VolumeKeyRewriter)(nil).tokenScope())
	assert.Equal(t, tokenScopeNone, (&VolumeKeyRewriter{}).tokenScope(),
		"a zero-value rewriter has no mode and must read unrestricted")
}

func TestMatchedClaimTokens(t *testing.T) {
	rw := defaultVolumeKeyRewriter()
	pvc := func(volumeName string) *model.Sample {
		return &model.Sample{Metric: model.Metric{"volumename": model.LabelValue(volumeName)}, Value: 1}
	}
	vol := func(name string) *model.Sample {
		return &model.Sample{Metric: model.Metric{"volume": model.LabelValue(name)}, Value: 1}
	}

	t.Run("only a claim phase 1 matched contributes its token", func(t *testing.T) {
		got := matchedClaimTokens(
			model.Vector{pvc("pvc-aaaa"), pvc("pvc-bbbb")},
			model.Vector{vol("trident_pvc_aaaa")}, rw)
		assert.Equal(t, []string{"pvc_aaaa"}, got, "pvc-bbbb matched nothing, so it needs no candidate recovery")
	})

	t.Run("a claim matching several series contributes one token", func(t *testing.T) {
		got := matchedClaimTokens(
			model.Vector{pvc("pvc-aaaa")},
			model.Vector{vol("trident_pvc_aaaa"), vol("snap_trident_pvc_aaaa"), vol("clone_pvc_aaaa")}, rw)
		assert.Equal(t, []string{"pvc_aaaa"}, got)
	})

	t.Run("tokens are sorted and de-duplicated across claims", func(t *testing.T) {
		got := matchedClaimTokens(
			model.Vector{pvc("pvc-bbbb"), pvc("pvc-aaaa"), pvc("pvc-aaaa")},
			model.Vector{vol("x_pvc_aaaa"), vol("y_pvc_bbbb")}, rw)
		assert.Equal(t, []string{"pvc_aaaa", "pvc_bbbb"}, got)
	})

	t.Run("the token is the matcher's own derivation, not the PV name", func(t *testing.T) {
		// A custom rewrite makes the token differ from anything derivable by eye.
		// Phase 2 must fetch for the token the parse joins under, or a claim
		// could be fetched for and then not joined.
		custom, err := NewVolumeKeyRewriter(
			[]VolumeKeyRule{{Pattern: `^pvc-`, Replacement: "vol_"}, {Pattern: `-`, Replacement: "_"}},
			VolumeMatchSuffix)
		require.NoError(t, err)
		claims := []pvcVolume{{volumeName: "pvc-9f3a-11d0"}}
		indexed := newVolumeMatcher(custom, claims).tokens[0]
		got := matchedClaimTokens(
			model.Vector{pvc("pvc-9f3a-11d0")},
			model.Vector{vol("prefix_vol_9f3a_11d0")}, custom)
		assert.Equal(t, []string{indexed}, got)
		assert.Equal(t, "vol_9f3a_11d0", indexed)
	})

	t.Run("nothing to recover for", func(t *testing.T) {
		assert.Nil(t, matchedClaimTokens(nil, model.Vector{vol("v")}, rw))
		assert.Nil(t, matchedClaimTokens(model.Vector{pvc("pvc-a")}, nil, rw))
		assert.Nil(t, matchedClaimTokens(model.Vector{pvc("")}, model.Vector{vol("v")}, rw),
			"a claim with no bound volume derives no token")
	})
}

func TestMergeVolumeLabels_DeduplicatesByLabelSet(t *testing.T) {
	series := func(vol, aggr string) *model.Sample {
		return &model.Sample{Metric: model.Metric{"volume": model.LabelValue(vol), "aggr": model.LabelValue(aggr)}, Value: 1}
	}
	base := model.Vector{series("v1", "a1"), series("v2", "a1")}
	extra := model.Vector{series("v2", "a1"), series("v3", "a0")}

	got := mergeVolumeLabels(base, extra)
	require.Len(t, got, 3, "the series both phases return is kept once")
	assert.Equal(t, "v1", string(got[0].Metric["volume"]), "base order is preserved")
	assert.Equal(t, "v3", string(got[2].Metric["volume"]))
	assert.Len(t, base, 2, "the base vector is never mutated")

	assert.Equal(t, base, mergeVolumeLabels(base, nil))
}

// ----------------------------------------------------------------- 3. the plan

func vlrRoots(t *testing.T, ontap, nodes, aggrs, svms, pods []string) graph.StorageRoots {
	t.Helper()
	scope, err := graph.NewStorageScope(nil, nil, ontap, nodes, aggrs, svms, pods)
	require.NoError(t, err)
	return scope.Roots
}

func TestStoragePlan_CarriesTheVolumeRoots(t *testing.T) {
	plan := storagePlan(vlrRoots(t,
		[]string{"ontap-b", "ontap-a", "ontap-a", ""},
		nil,
		[]string{"aggr2", "aggr1", ""},
		[]string{"svm_x"}, nil))
	assert.Equal(t, []string{"ontap-a", "ontap-b"}, plan.volumeClusters,
		"sorted, de-duplicated, empties dropped — map order must never reach the plan")
	assert.Equal(t, []string{"aggr1", "aggr2"}, plan.volumeAggrs)
	assert.True(t, plan.svmRoot)

	assert.False(t, storagePlan(graph.StorageRoots{}).svmRoot)
	assert.Empty(t, storagePlan(graph.StorageRoots{}).volumeAggrs)
}

func TestTopologyPlan_RestrictsVolumeLabels(t *testing.T) {
	suffix := defaultVolumeKeyRewriter()
	mode := func(m VolumeMatchMode) *VolumeKeyRewriter {
		rw, err := NewVolumeKeyRewriter(nil, m)
		require.NoError(t, err)
		return rw
	}
	cases := []struct {
		name string
		plan topologyPlan
		rw   *VolumeKeyRewriter
		want bool
	}{
		{"no roots at all", storagePlan(vlrRoots(t, nil, nil, nil, nil, nil)), suffix, false},
		{"pod root only", storagePlan(vlrRoots(t, nil, nil, nil, nil, []string{"shop/a"})), suffix, false},
		{"svm only", storagePlan(vlrRoots(t, nil, nil, nil, []string{"s"}, nil)), suffix, false},
		{"node only", storagePlan(vlrRoots(t, nil, []string{"n"}, nil, nil, nil)), suffix, false},
		{"aggr plus svm: the projection unions them", storagePlan(vlrRoots(t, nil, nil, []string{"a"}, []string{"s"}, nil)), suffix, false},
		{"aggr plus node: a node root is admitted regardless of flow", storagePlan(vlrRoots(t, nil, []string{"n"}, []string{"a"}, nil, nil)), suffix, false},
		{"cluster plus svm", storagePlan(vlrRoots(t, []string{"o"}, nil, nil, []string{"s"}, nil)), suffix, false},
		{"aggr, contains mode", storagePlan(vlrRoots(t, nil, nil, []string{"a"}, nil, nil)), mode(VolumeMatchContains), false},
		{"aggr, regex mode", storagePlan(vlrRoots(t, nil, nil, []string{"a"}, nil, nil)), mode(VolumeMatchRegex), false},
		{"aggr", storagePlan(vlrRoots(t, nil, nil, []string{"a"}, nil, nil)), suffix, true},
		{"ontap_cluster", storagePlan(vlrRoots(t, []string{"o"}, nil, nil, nil, nil)), suffix, true},
		{"ontap_cluster plus aggr", storagePlan(vlrRoots(t, []string{"o"}, nil, []string{"a"}, nil, nil)), suffix, true},
		{"aggr plus pod composes", storagePlan(vlrRoots(t, nil, nil, []string{"a"}, nil, []string{"shop/a"})), suffix, true},
		{"aggr, exact mode", storagePlan(vlrRoots(t, nil, nil, []string{"a"}, nil, nil)), mode(VolumeMatchExact), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.plan.restrictsVolumeLabels(tc.rw))
		})
	}

	t.Run("fullPlan can never restrict, structurally", func(t *testing.T) {
		assert.False(t, fullPlan.restrictsVolumeLabels(suffix))
		// Even a plan that somehow carried roots stays unrestricted unless it
		// is by-reference: only /v1/storage-graph has roots to restrict by.
		forged := topologyPlan{volumeAggrs: []string{"a"}}
		assert.False(t, forged.restrictsVolumeLabels(suffix))
	})
}

// ------------------------------------------------------------------- 4. chunks

func TestRootedVolumeLabelsChunks(t *testing.T) {
	t.Run("aggregates are chunked and the cluster set repeats in every chunk", func(t *testing.T) {
		got, ok := rootedVolumeLabelsChunks([]string{"ontap-prod"}, []string{"aggr1", "aggr2", "aggr3"}, 30)
		require.True(t, ok)
		require.Greater(t, len(got), 1, "a tight budget must split the aggregate set")
		var union []string
		for _, c := range got {
			assert.Equal(t, []string{"ontap-prod"}, c.clusters, "the AND partner must be in every chunk")
			union = append(union, c.aggrs...)
		}
		assert.Equal(t, []string{"aggr1", "aggr2", "aggr3"}, union, "chunks are disjoint and complete")
	})

	t.Run("with no aggregate the cluster set is chunked", func(t *testing.T) {
		got, ok := rootedVolumeLabelsChunks([]string{"ontap-a", "ontap-b", "ontap-c"}, nil, 12)
		require.True(t, ok)
		require.Greater(t, len(got), 1)
		for _, c := range got {
			assert.Empty(t, c.aggrs)
		}
	})

	t.Run("a roomy budget is one query", func(t *testing.T) {
		got, ok := rootedVolumeLabelsChunks([]string{"o"}, []string{"a1", "a2"}, 8192)
		require.True(t, ok)
		require.Len(t, got, 1)
		assert.Equal(t, []string{"a1", "a2"}, got[0].aggrs)
	})

	t.Run("the repeated cluster matcher is charged at its RENDERED length", func(t *testing.T) {
		// Two filer names carrying a metacharacter render as an anchored
		// alternation: QuoteMeta escapes each dot, escapeLiteral escapes the
		// backslash it added, and the `cluster=~"…"` wrapper costs eleven more.
		// The budget bounds the CHUNKED alternation (as it does for every other
		// ChunkScope caller), so what the repeated matcher must not do is eat
		// into it uncounted.
		dotted := []string{"ontap.a", "ontap.b"}
		rendered := promql.MatcherCost(promql.VolumeLabelsClusterLabel, dotted)
		naive := len(strings.Join(dotted, "|"))
		assert.Equal(t, 30, rendered)
		assert.Equal(t, 15, naive, "what a raw join would have charged")

		const budget = 40
		remaining := budget - rendered - 1 // -1 for the comma joining the two matchers
		got, ok := rootedVolumeLabelsChunks(dotted, []string{"a1", "a2", "a3", "a4"}, budget)
		require.True(t, ok)
		require.Greater(t, len(got), 1, "only %d bytes are left for the aggregates", remaining)

		for _, c := range got {
			q, rok := promql.RenderVolumeLabelsRooted(time.Minute, c.clusters, c.aggrs)
			require.True(t, rok)
			sel := q[strings.Index(q, "{")+1 : strings.LastIndex(q, "}")]
			alt := sel[strings.LastIndex(sel, `aggr=`):]
			alt = alt[strings.Index(alt, `"`)+1 : len(alt)-1]
			assert.LessOrEqual(t, len(alt), remaining,
				"the chunked alternation must fit what the repeated matcher leaves")
		}
		assert.Greater(t, budget-naive-1, remaining,
			"charging the raw join would have handed each chunk bytes the rendered query does not have")
	})

	t.Run("a single over-budget value still gets its own chunk", func(t *testing.T) {
		long := strings.Repeat("a", 100)
		got, ok := rootedVolumeLabelsChunks(nil, []string{"a", long, "b"}, 10)
		require.True(t, ok)
		require.Len(t, got, 3)
		assert.Equal(t, []string{long}, got[1].aggrs)
	})

	t.Run("more chunks than the cap reads unrestricted instead", func(t *testing.T) {
		// `?aggr=` is repeatable and the request parser caps each value's
		// LENGTH, never the count, so this is the one scope a client can
		// inflate. Past the cap the leg reads as it did before the
		// restriction existed: one query, same body, bounded fan-out.
		_, ok := rootedVolumeLabelsChunks(nil, manyAggrRoots(5000, 240), DefaultQoSScopeBatchBytes)
		assert.False(t, ok, "5000 near-maximum-length values")

		// A cluster set that eats the whole budget collapses the per-chunk
		// budget to the floor, which would otherwise put every aggregate in a
		// query of its own. The same cap catches it.
		_, ok = rootedVolumeLabelsChunks([]string{strings.Repeat("c", 9000)}, manyAggrRoots(9, 0), DefaultQoSScopeBatchBytes)
		assert.False(t, ok, "a budget collapsed to the floor")

		// What a real request looks like is nowhere near it: a filer has tens
		// of aggregates, and even 5000 ordinary names fit in six chunks.
		_, ok = rootedVolumeLabelsChunks([]string{"ontap-prod"}, []string{"aggr1", "aggr2", "aggr3"}, DefaultQoSScopeBatchBytes)
		assert.True(t, ok)
		got, ok := rootedVolumeLabelsChunks(nil, manyAggrRoots(5000, 0), DefaultQoSScopeBatchBytes)
		assert.True(t, ok)
		assert.LessOrEqual(t, len(got), maxRootedVolumeLabelChunks)
	})
}

// manyAggrRoots builds n aggregate-root names, each padded to about pad extra
// bytes so a test can choose whether the set fits the chunk budget.
func manyAggrRoots(n, pad int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("aggr%04d%s", i, strings.Repeat("x", pad)))
	}
	return out
}

// An uncapped `?aggr=` must not turn one query into thousands. Past the cap the
// build reads the family whole — the pre-change behaviour — and says so.
func TestRootedVolumeLabels_UnboundedRootSetReadsUnrestricted(t *testing.T) {
	scope := vlrScope(t, nil, nil, append(manyAggrRoots(5000, 240), "aggr1"), nil, nil)
	q := promqlfake.New(vlrMultiFiler())
	g, err := New(q, Options{}, nil, nil).buildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel, storagePlan(scope.Roots))
	require.NoError(t, err)

	assert.Equal(t, []string{vlrBare}, q.QueriesFor(promql.QVolumeLabels),
		"one unrestricted query, not thousands of chunks — and no phase 2 after the fallback")

	// And the body is still right: aggr1 is among the 5000, so its claim draws.
	rooted := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)
	gu, _ := vlrBuild(t, vlrMultiFiler(), rooted.Roots, Options{}, false)
	assert.JSONEq(t, planBodyJSON(t, vlrBody(t, gu, rooted)), planBodyJSON(t, vlrBody(t, g, rooted)))
}

// A value set that normalises away makes the predicate true and the renderer
// false. graph.NewStorageScope drops empties for an HTTP caller, but
// pkg/build is an importable engine and StorageRoots is an exported map.
func TestRootedVolumeLabels_EmptyRootValueDoesNotFailAnOptionalLeg(t *testing.T) {
	plan := storagePlan(graph.StorageRoots{ONTAPClusters: map[string]struct{}{"": {}}})
	assert.Empty(t, plan.volumeClusters, "the plan normalises, so the predicate and the renderer agree")
	assert.False(t, plan.restrictsVolumeLabels(defaultVolumeKeyRewriter()))

	q := promqlfake.New(vlrMultiFiler())
	g, err := New(q, Options{}, nil, nil).buildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, plan)
	require.NoError(t, err, "an OPTIONAL family must never fail the build")
	assert.Equal(t, []string{vlrBare}, q.QueriesFor(promql.QVolumeLabels))
	assert.NotEmpty(t, g.NodesByID)
}

// ------------------------------------------------------------- estate fixtures

// vlrVol is one FlexVol of a synthetic filer: the volume_labels series and the
// QoS read-ops workload that measures it.
type vlrVol struct {
	name, oc, node, aggr, svm string
	ops                       float64
}

// vlrHarvest renders the whole Harvest side of an estate from its volumes: the
// volume_labels series, one qos_read_ops series each, and the aggregate and
// controller gauges the inventory needs. extraAggrs names aggregates that carry
// no volume at all — present only in the unrestricted gauge families.
func vlrHarvest(vols []vlrVol, extraAggrs ...[3]string) map[promql.Query]model.Vector {
	out := map[promql.Query]model.Vector{}
	aggrSeen, nodeSeen := map[string]bool{}, map[string]bool{}
	gauges := func(oc, node, aggr string) {
		if k := oc + "/" + aggr; !aggrSeen[k] {
			aggrSeen[k] = true
			out[promql.QAggrStatus] = append(out[promql.QAggrStatus], planHarvest("cluster", oc, "node", node, "aggr", aggr))
		}
		if k := oc + "/" + node; !nodeSeen[k] {
			nodeSeen[k] = true
			out[promql.QNetAppNodeStatus] = append(out[promql.QNetAppNodeStatus], planHarvest("cluster", oc, "node", node))
		}
	}
	for _, v := range vols {
		out[promql.QVolumeLabels] = append(out[promql.QVolumeLabels],
			planHarvest("cluster", v.oc, "node", v.node, "aggr", v.aggr, "svm", v.svm, "volume", v.name))
		out[promql.QQoSReadOps] = append(out[promql.QQoSReadOps],
			withValue(planHarvest("cluster", v.oc, "svm", v.svm, "volume", v.name), v.ops))
		gauges(v.oc, v.node, v.aggr)
	}
	for _, a := range extraAggrs { // {oc, node, aggr}
		gauges(a[0], a[1], a[2])
	}
	return out
}

// vlrEstate is planEstate's Kubernetes side over the given Harvest side.
func vlrEstate(harvest map[promql.Query]model.Vector) map[promql.Query]model.Vector {
	e := planEstate()
	for _, q := range []promql.Query{
		promql.QVolumeLabels, promql.QQoSReadOps, promql.QQoSWriteOps, promql.QQoSReadLatency,
		promql.QQoSWriteLatency, promql.QQoSReadData, promql.QQoSWriteData,
		promql.QQoSPolicyFixedMaxIOPS, promql.QQoSPolicyFixedMaxMBps,
		promql.QAggrStatus, promql.QAggrSpaceUsed, promql.QAggrSpaceTotal,
		promql.QNetAppNodeStatus, promql.QNetAppNodeLabels, promql.QNetAppNodeCPUBusy,
		promql.QNetAppNodeTotalOps, promql.QNetAppNodeTotalLatency, promql.QNetAppNodeTotalData,
	} {
		delete(e, q)
	}
	for q, v := range harvest {
		e[q] = v
	}
	return e
}

// vlrMultiFiler: two claims on two aggregates of ontap-prod, a second filer
// whose aggr1 shares a name with ontap-prod's, and extra volumes that no claim
// matches — on the rooted aggregate (so the owner vote has a population) and on
// a third one.
func vlrMultiFiler() map[promql.Query]model.Vector {
	return vlrEstate(vlrHarvest([]vlrVol{
		{"trident_pvc_orders", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
		{"trident_pvc_redis", "ontap-prod", "ontap-prod-02", "aggr2", "svm_platform", 100},
		{"lab_unrelated_1", "ontap-lab", "ontap-lab-01", "aggr1", "svm_lab", 5},
		{"prod_unrelated_1", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 7},
		{"prod_unrelated_2", "ontap-prod", "ontap-prod-03", "aggr3", "svm_shop", 7},
	}))
}

var (
	vlrSel = promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}
	vlrEnd = time.Unix(1, 0).UTC()
)

// vlrBuild builds the storage graph for roots over fx, restricted or not. The
// "unrestricted" build is the SAME plan with the two volume root sets cleared,
// so it differs in exactly one thing: the volume-label read. Everything else —
// the by-reference pod, node and controller waves, the QoS wave — is identical.
func vlrBuild(t *testing.T, fx map[promql.Query]model.Vector, roots graph.StorageRoots, opts Options, restricted bool) (*graph.Graph, *promqlfake.Querier) {
	t.Helper()
	plan := storagePlan(roots)
	if !restricted {
		plan.volumeClusters, plan.volumeAggrs = nil, nil
	}
	q := promqlfake.New(fx)
	g, err := New(q, opts, nil, nil).buildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, plan)
	require.NoError(t, err)
	return g, q
}

func vlrBody(t *testing.T, g *graph.Graph, scope graph.StorageScope) cytoscape.Body {
	t.Helper()
	return cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
}

func vlrIDs(body cytoscape.Body) map[string]bool {
	out := map[string]bool{}
	for _, n := range body.Elements.Nodes {
		out[n.Data.ID] = true
	}
	return out
}

// vlrParity builds the estate restricted and unrestricted, asserts the two
// projected bodies are identical, and returns the two queriers so the caller
// can assert the restricted read really was different.
func vlrParity(t *testing.T, fx map[promql.Query]model.Vector, scope graph.StorageScope, opts Options) (restricted, unrestricted *promqlfake.Querier, body cytoscape.Body) {
	t.Helper()
	gr, qr := vlrBuild(t, fx, scope.Roots, opts, true)
	gu, qu := vlrBuild(t, fx, scope.Roots, opts, false)
	br, bu := vlrBody(t, gr, scope), vlrBody(t, gu, scope)
	require.NotEmpty(t, br.Elements.Nodes, "a vacuous body would prove nothing")
	assert.JSONEq(t, planBodyJSON(t, bu), planBodyJSON(t, br),
		"the restricted read must draw exactly what the whole-filer read draws")
	return qr, qu, br
}

func vlrScope(t *testing.T, ontap, nodes, aggrs, svms, pods []string) graph.StorageScope {
	t.Helper()
	scope, err := graph.NewStorageScope(nil, nil, ontap, nodes, aggrs, svms, pods)
	require.NoError(t, err)
	return scope
}

const vlrBare = `last_over_time(volume_labels[1m])`

// -------------------------------------------------- 7. output preservation

func TestRootedVolumeLabels_ParityAcrossRootShapes(t *testing.T) {
	cases := map[string]struct {
		scope graph.StorageScope
		// the volume_labels queries the restricted build must have issued in
		// its first phase, as substrings each must carry
		phase1 []string
	}{
		"aggregate": {
			vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil),
			[]string{`aggr="aggr1"`},
		},
		"ontap cluster": {
			vlrScope(t, []string{"ontap-prod"}, nil, nil, nil, nil),
			[]string{`cluster="ontap-prod"`},
		},
		"cluster and aggregate narrow one query": {
			vlrScope(t, []string{"ontap-prod"}, nil, []string{"aggr1"}, nil, nil),
			[]string{`cluster="ontap-prod",aggr="aggr1"`},
		},
		"two aggregates": {
			vlrScope(t, nil, nil, []string{"aggr1", "aggr2"}, nil, nil),
			[]string{`aggr=~"aggr1|aggr2"`},
		},
		"aggregate composed with a pod root": {
			vlrScope(t, nil, nil, []string{"aggr1"}, nil, []string{"shop/orders-0"}),
			[]string{`aggr="aggr1"`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			restricted, unrestricted, _ := vlrParity(t, vlrMultiFiler(), tc.scope, Options{})

			assert.Equal(t, []string{vlrBare}, unrestricted.QueriesFor(promql.QVolumeLabels),
				"the control build reads the whole filer in one bare query")
			phase1 := restricted.QueriesFor(promql.QVolumeLabels)
			require.NotEmpty(t, phase1)
			assert.NotContains(t, phase1, vlrBare, "a rooted request never issues the bare query")
			assert.Contains(t, phase1[0], tc.phase1[0])
		})
	}
}

func TestRootedVolumeLabels_ClusterAndAggregateAreAnded(t *testing.T) {
	// ontap-lab also has an aggr1. `ontap_cluster=ontap-prod&aggr=aggr1` roots
	// ONLY ontap-prod's, so its body must not draw the lab aggregate, while
	// `aggr=aggr1` alone roots both.
	fx := vlrMultiFiler()
	labAggr := "netapp/ontap-lab/aggr/aggr1"
	prodAggr := "netapp/ontap-prod/aggr/aggr1"

	_, _, both := vlrParity(t, fx, vlrScope(t, []string{"ontap-prod"}, nil, []string{"aggr1"}, nil, nil), Options{})
	ids := vlrIDs(both)
	assert.True(t, ids[prodAggr])
	assert.False(t, ids[labAggr], "an aggr= root names an aggregate only WITHIN the ontap_cluster= values")

	_, _, only := vlrParity(t, fx, vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil), Options{})
	ids = vlrIDs(only)
	assert.True(t, ids[prodAggr])
	assert.True(t, ids[labAggr], "with no ontap_cluster= both filers' aggr1 are roots")

	// And the query shape says the same thing: one selector, both matchers.
	_, q := vlrBuild(t, fx, vlrScope(t, []string{"ontap-prod"}, nil, []string{"aggr1"}, nil, nil).Roots, Options{}, true)
	assert.Equal(t, `last_over_time(volume_labels{cluster="ontap-prod",aggr="aggr1"}[1m])`,
		q.QueriesFor(promql.QVolumeLabels)[0])
}

// A Trident clone whose FlexVol name also ends with the claim's token sits on a
// lexically-smaller aggregate. pickAggr is smallest over the WHOLE candidate
// set, so a phase-1-only read rooted at the larger aggregate would place the
// claim there and an unrestricted read would not.
func vlrClone() map[promql.Query]model.Vector {
	return vlrEstate(vlrHarvest([]vlrVol{
		{"trident_pvc_orders", "ontap-prod", "ontap-prod-09", "aggr9", "svm_shop", 300},
		{"snap_trident_pvc_orders", "ontap-prod", "ontap-prod-00", "aggr0", "svm_shop", 50},
		{"trident_pvc_redis", "ontap-prod", "ontap-prod-02", "aggr2", "svm_platform", 100},
	}))
}

func TestRootedVolumeLabels_CloneOnLexicallySmallerAggregateKeepsItsPick(t *testing.T) {
	const ordersPod = "zone-a-prod-c1/uid-o0"

	t.Run("rooted at the larger aggregate the claim is not retained", func(t *testing.T) {
		scope := vlrScope(t, nil, nil, []string{"aggr9"}, nil, nil)
		restricted, _, body := vlrParity(t, vlrClone(), scope, Options{})
		ids := vlrIDs(body)
		assert.True(t, ids["netapp/ontap-prod/aggr/aggr9"], "the root is drawn")
		assert.False(t, ids[ordersPod], "the pick is aggr0, so the claim's pod is not on a rooted path")

		// Phase 2 ran, and reached the clone that phase 1 (aggr9 only) never saw.
		var tokenQuery string
		for _, q := range restricted.QueriesFor(promql.QVolumeLabels) {
			if strings.Contains(q, `volume=~".*pvc_orders"`) {
				tokenQuery = q
			}
		}
		assert.NotEmpty(t, tokenQuery, "phase 2 must restrict on the matched claim's token")
		assert.NotContains(t, tokenQuery, "aggr", "phase 2 must reach volumes on ANY aggregate")
	})

	t.Run("rooted at the smaller aggregate the I/O sums both volumes in both builds", func(t *testing.T) {
		scope := vlrScope(t, nil, nil, []string{"aggr0"}, nil, nil)
		restricted, _, body := vlrParity(t, vlrClone(), scope, Options{})
		assert.True(t, vlrIDs(body)[ordersPod], "the pick is aggr0, so the claim IS retained")

		// The QoS scope is computed over the MERGED result, so it names the
		// volume phase 2 alone recovered — and the claim's I/O sums both. The
		// redis claim is off the rooted aggregate, so its volume is (rightly)
		// absent: the restriction narrows the QoS read too.
		scopes := restricted.ScopeValues(promql.QQoSReadOps, "volume")
		require.NotEmpty(t, scopes)
		assert.ElementsMatch(t, []string{"snap_trident_pvc_orders", "trident_pvc_orders"}, flattenStrings(scopes),
			"trident_pvc_orders is the volume only phase 2 returned")
		assert.NotContains(t, flattenStrings(scopes), "trident_pvc_redis")
	})

	t.Run("phase 2 is load-bearing: without it the pick is different", func(t *testing.T) {
		// Prove the hazard is real at the resolver, independent of any wiring.
		claim := []pvcVolume{{id: "c/shop/orders-data", volumeName: "pvc-orders"}}
		mk := func(vols ...vlrVol) model.Vector {
			v := make(model.Vector, 0, len(vols))
			for _, x := range vols {
				v = append(v, planHarvest("cluster", x.oc, "node", x.node, "aggr", x.aggr, "svm", x.svm, "volume", x.name))
			}
			return v
		}
		phase1Only := netappFixture{claims: claim, vol: mk(
			vlrVol{name: "trident_pvc_orders", oc: "ontap-prod", node: "ontap-prod-09", aggr: "aggr9", svm: "svm_shop"},
		)}.run()
		merged := netappFixture{claims: claim, vol: mk(
			vlrVol{name: "trident_pvc_orders", oc: "ontap-prod", node: "ontap-prod-09", aggr: "aggr9", svm: "svm_shop"},
			vlrVol{name: "snap_trident_pvc_orders", oc: "ontap-prod", node: "ontap-prod-00", aggr: "aggr0", svm: "svm_shop"},
		)}.run()
		require.Len(t, phase1Only.edges, 1)
		require.Len(t, merged.edges, 1)
		assert.Equal(t, graph.NetAppAggrID("ontap-prod", "aggr9"), phase1Only.edges[0].Target)
		assert.Equal(t, graph.NetAppAggrID("ontap-prod", "aggr0"), merged.edges[0].Target,
			"the full candidate set picks the lexically-smaller aggregate")
	})
}

func flattenStrings(in [][]string) []string {
	var out []string
	for _, s := range in {
		out = append(out, s...)
	}
	return out
}

func TestRootedVolumeLabels_CrossFilerNameCollisionKeepsItsPick(t *testing.T) {
	// One FlexVol name on two filers. ontap-lab sorts before ontap-prod, so the
	// unrestricted pick is the lab filer and the claim is NOT under the prod root.
	fx := vlrEstate(vlrHarvest([]vlrVol{
		{"trident_pvc_orders", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
		{"trident_pvc_orders", "ontap-lab", "ontap-lab-07", "aggr7", "svm_lab", 90},
		{"trident_pvc_redis", "ontap-prod", "ontap-prod-02", "aggr2", "svm_platform", 100},
	}))
	scope := vlrScope(t, []string{"ontap-prod"}, nil, []string{"aggr1"}, nil, nil)
	restricted, _, body := vlrParity(t, fx, scope, Options{})

	assert.False(t, vlrIDs(body)["zone-a-prod-c1/uid-o0"],
		"the lexically-smallest (ontap_cluster, aggr) is on the lab filer")
	assert.True(t, vlrIDs(body)["netapp/ontap-prod/aggr/aggr1"])
	var sawTokens bool
	for _, q := range restricted.QueriesFor(promql.QVolumeLabels) {
		sawTokens = sawTokens || strings.Contains(q, `volume=~".*pvc_orders"`)
	}
	assert.True(t, sawTokens, "phase 1 was cluster-scoped, so only phase 2 could see the lab filer's series")
}

func TestRootedVolumeLabels_TakeoverOwnershipSurvives(t *testing.T) {
	// The claim's own series names ontap-prod-02, but another volume of the same
	// aggregate names ontap-prod-01 — a takeover inside the window. The owner is
	// the lexically-smallest node across ALL of the aggregate's series.
	fx := vlrEstate(vlrHarvest([]vlrVol{
		{"trident_pvc_orders", "ontap-prod", "ontap-prod-02", "aggr1", "svm_shop", 300},
		{"prod_unrelated_1", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 7},
		{"trident_pvc_redis", "ontap-prod", "ontap-prod-02", "aggr2", "svm_platform", 100},
	}))
	scope := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)
	gr, _ := vlrBuild(t, fx, scope.Roots, Options{}, true)
	_, _, body := vlrParity(t, fx, scope, Options{})

	aggr, ok := gr.NodesByID["netapp/ontap-prod/aggr/aggr1"]
	require.True(t, ok)
	assert.Equal(t, "ontap-prod-01", aggr.Labels()["node"],
		"phase 1 kept every series of the rooted aggregate, so the vote is unchanged")
	assert.True(t, vlrIDs(body)["netapp/ontap-prod/ontap-prod-01"])
}

func TestRootedVolumeLabels_RootedAggregateWithNoClaimIsStillDrawn(t *testing.T) {
	// aggr3 exists only in the unrestricted gauge families; no volume names it.
	fx := vlrEstate(vlrHarvest([]vlrVol{
		{"trident_pvc_orders", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
	}, [3]string{"ontap-prod", "ontap-prod-03", "aggr3"}))
	scope := vlrScope(t, nil, nil, []string{"aggr3"}, nil, nil)
	restricted, _, body := vlrParity(t, fx, scope, Options{})

	ids := vlrIDs(body)
	assert.True(t, ids["netapp/ontap-prod/aggr/aggr3"], "the root is drawn from the gauge families")
	assert.True(t, ids["netapp/ontap-prod/ontap-prod-03"], "with its owning controller")
	assert.Len(t, restricted.QueriesFor(promql.QVolumeLabels), 1,
		"phase 1 matched nothing, so phase 2 is not issued at all")
}

func TestRootedVolumeLabels_OptOutsReadTheWholeFiler(t *testing.T) {
	contains, err := NewVolumeKeyRewriter(nil, VolumeMatchContains)
	require.NoError(t, err)
	regex, err := NewVolumeKeyRewriter(nil, VolumeMatchRegex)
	require.NoError(t, err)

	cases := map[string]struct {
		scope graph.StorageScope
		opts  Options
	}{
		"svm only":                 {vlrScope(t, nil, nil, nil, []string{"svm_shop"}, nil), Options{}},
		"node only":                {vlrScope(t, nil, []string{"ontap-prod-01"}, nil, nil, nil), Options{}},
		"aggregate plus svm":       {vlrScope(t, nil, nil, []string{"aggr1"}, []string{"svm_platform"}, nil), Options{}},
		"aggregate plus node":      {vlrScope(t, nil, []string{"ontap-prod-01"}, []string{"aggr1"}, nil, nil), Options{}},
		"contains mode":            {vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil), Options{VolumeKey: contains}},
		"regex mode":               {vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil), Options{VolumeKey: regex}},
		"no storage-side root":     {vlrScope(t, nil, nil, nil, nil, []string{"shop/orders-0"}), Options{}},
		"cluster, contains mode":   {vlrScope(t, []string{"ontap-prod"}, nil, nil, nil, nil), Options{VolumeKey: contains}},
		"aggregate, svm, and node": {vlrScope(t, nil, []string{"ontap-prod-01"}, []string{"aggr1"}, []string{"svm_shop"}, nil), Options{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g, q := vlrBuild(t, vlrMultiFiler(), tc.scope.Roots, tc.opts, true)
			assert.Equal(t, []string{vlrBare}, q.QueriesFor(promql.QVolumeLabels),
				"exactly one bare unrestricted query and no second phase")
			require.NotNil(t, g)
		})
	}

	t.Run("an SVM root keeps a claim that only the SVM reaches", func(t *testing.T) {
		// redis is on aggr2 / svm_platform. Rooting aggr1 AND svm_platform must
		// retain it through the SVM — which a restriction by aggr alone would drop.
		scope := vlrScope(t, nil, nil, []string{"aggr1"}, []string{"svm_platform"}, nil)
		_, _, body := vlrParity(t, vlrMultiFiler(), scope, Options{})
		ids := vlrIDs(body)
		assert.True(t, ids["zone-a-prod-c1/uid-r0"], "platform/redis-0 is reached through svm_platform")
		assert.True(t, ids["netapp/ontap-prod/aggr/aggr2"])
	})
}

// ---------------------------------------------------------------- phases + tally

func TestRootedVolumeLabels_PhaseTwoWaitsForPhaseOne(t *testing.T) {
	scope := vlrScope(t, nil, nil, []string{"aggr1", "aggr2"}, nil, nil)
	q := promqlfake.New(vlrMultiFiler())
	// A tiny budget forces several phase-1 chunks AND several phase-2 chunks.
	_, err := New(q, Options{QoSScopeBatchBytes: 6}, nil, nil).buildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel, storagePlan(scope.Roots))
	require.NoError(t, err)

	lastRooted, firstToken := -1, -1
	for i, is := range q.Issued() {
		if is.Name != string(promql.QVolumeLabels) {
			continue
		}
		if strings.Contains(is.Query, `volume=~`) || strings.Contains(is.Query, `volume=`) {
			if firstToken < 0 {
				firstToken = i
			}
		} else {
			lastRooted = i
		}
	}
	require.GreaterOrEqual(t, lastRooted, 0)
	require.GreaterOrEqual(t, firstToken, 0, "phase 2 must have run")
	assert.Less(t, lastRooted, firstToken, "every phase-1 chunk lands before phase 2 issues")

	rooted := 0
	for _, s := range q.QueriesFor(promql.QVolumeLabels) {
		if strings.Contains(s, "aggr=") {
			rooted++
			assert.True(t, strings.Contains(s, `aggr="aggr1"`) || strings.Contains(s, `aggr="aggr2"`),
				"a 6-byte budget puts each aggregate in its own chunk: %s", s)
		}
	}
	assert.Equal(t, 2, rooted)
}

func TestRootedVolumeLabels_SeriesTallyIsTheMergedCount(t *testing.T) {
	// Roots aggr1 (3 series: orders, lab_unrelated_1, prod_unrelated_1). Phase 2
	// re-reads `.*pvc_orders`, which returns trident_pvc_orders AGAIN. The tally
	// is the merged, de-duplicated count under the one family name.
	scope := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)
	q := promqlfake.New(vlrMultiFiler())
	topo, err := readTopology(t.Context(), q, time.Minute, vlrEnd, Options{}, vlrSel, storagePlan(scope.Roots))
	require.NoError(t, err)
	assert.Equal(t, 3, topo.RawSeriesCount[string(promql.QVolumeLabels)],
		"the series both phases return counts once")

	unrestricted := storagePlan(scope.Roots)
	unrestricted.volumeAggrs = nil
	topo, err = readTopology(t.Context(), promqlfake.New(vlrMultiFiler()), time.Minute, vlrEnd, Options{}, vlrSel, unrestricted)
	require.NoError(t, err)
	assert.Equal(t, 5, topo.RawSeriesCount[string(promql.QVolumeLabels)], "the control reads the whole filer")
}

// ------------------------------------------------------------ degrade + signal

func TestRootedVolumeLabels_ChunkFailuresDegrade(t *testing.T) {
	scope := vlrScope(t, nil, nil, []string{"aggr1", "aggr2"}, nil, nil)
	boom := errors.New("upstream said no")

	t.Run("a failed phase-1 chunk costs only the claims it carried", func(t *testing.T) {
		q := promqlfake.New(vlrMultiFiler())
		q.Fail = func(name, query string) error {
			if name == string(promql.QVolumeLabels) && strings.Contains(query, `aggr="aggr2"`) {
				return boom
			}
			return nil
		}
		g, err := New(q, Options{QoSScopeBatchBytes: 6}, nil, nil).buildStorage(
			t.Context(), time.Minute, vlrEnd, vlrSel, storagePlan(scope.Roots))
		require.NoError(t, err, "the family is OPTIONAL: a failed chunk never fails the build")

		ids := vlrIDs(vlrBody(t, g, scope))
		assert.True(t, ids["zone-a-prod-c1/uid-o0"], "the orders claim's chunk succeeded")
		assert.False(t, ids["zone-a-prod-c1/uid-r0"], "the redis claim lived in the failed chunk")
	})

	t.Run("a failed phase-2 chunk degrades to the phase-1 candidate set", func(t *testing.T) {
		q := promqlfake.New(vlrMultiFiler())
		q.Fail = func(name, query string) error {
			if name == string(promql.QVolumeLabels) && strings.Contains(query, `volume=`) {
				return boom
			}
			return nil
		}
		g, err := New(q, Options{}, nil, nil).buildStorage(
			t.Context(), time.Minute, vlrEnd, vlrSel, storagePlan(scope.Roots))
		require.NoError(t, err)

		// No claim here has a candidate off the rooted aggregates, so the phase-1
		// set is already complete and the body matches the unrestricted one.
		gu, _ := vlrBuild(t, vlrMultiFiler(), scope.Roots, Options{}, false)
		assert.JSONEq(t, planBodyJSON(t, vlrBody(t, gu, scope)), planBodyJSON(t, vlrBody(t, g, scope)))
	})

	t.Run("the caller going away still fails the build", func(t *testing.T) {
		// Degrading is for UPSTREAM errors. When the caller itself is gone —
		// a build timeout, a disconnected client — nobody is waiting for a
		// result, and optionalQueryFatal must let that through.
		ctx, cancel := context.WithCancel(t.Context())
		q := promqlfake.New(vlrMultiFiler())
		q.Fail = func(name, _ string) error {
			if name == string(promql.QVolumeLabels) {
				cancel()
				return context.Canceled
			}
			return nil
		}
		_, err := New(q, Options{}, nil, nil).buildStorage(ctx, time.Minute, vlrEnd, vlrSel, storagePlan(scope.Roots))
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestRootedVolumeLabels_JoinMissSignalUnderARestriction(t *testing.T) {
	// Three claims: one joins, one matches a FlexVol whose `aggr` is empty (a
	// FlexGroup — a genuine coverage miss under either read), and one matches
	// nothing at all (off the rooted components under a restriction; a
	// derivation that does not fit the estate under an unrestricted one).
	claims := []pvcVolume{
		{id: "c/shop/orders-data", volumeName: "pvc-orders"},
		{id: "c/shop/fg-data", volumeName: "pvc-fg"},
		{id: "c/p/elsewhere", volumeName: "pvc-elsewhere"},
	}
	f := netappFixture{
		claims: claims,
		vol: sampleVec(
			volLabelSample("pvc-orders", "oc", "n1", "aggr1", "svm"),
			volLabelSample("pvc-fg", "oc", "n1", "", "svm-fg"),
		),
	}
	missCount := func(restricted bool) (recs []map[string]any) {
		return captureDebugRecords(t, func() {
			v := f.vectors()
			v.VolumeLabelsRestricted = restricted
			resolveNetAppStorage(f.claims, v)
		})
	}
	countOf := func(recs []map[string]any) float64 {
		for _, r := range recs {
			if r["msg"] == "netapp_volume_join_miss" {
				if n, ok := r["count"].(float64); ok {
					return n
				}
			}
		}
		return 0
	}

	assert.InDelta(t, 2.0, countOf(missCount(false)), 1e-9,
		"unrestricted: the FlexGroup claim AND the unmatched claim are both coverage misses")
	assert.InDelta(t, 1.0, countOf(missCount(true)), 1e-9,
		"restricted: the FlexGroup claim still is — only the claim that matched nothing is silenced, "+
			"because a restriction cannot tell that from a claim the request did not ask about")

	t.Run("end to end through a build", func(t *testing.T) {
		// cache-data is a claim no FlexVol backs anywhere. Unrooted it is a
		// coverage miss; under ?aggr=aggr1 it matched nothing, so it is silent.
		scope := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)
		restricted := captureDebugRecords(t, func() {
			vlrBuild(t, vlrMultiFiler(), scope.Roots, Options{}, true)
		})
		unrooted := captureDebugRecords(t, func() {
			vlrBuild(t, vlrMultiFiler(), graph.StorageRoots{}, Options{}, true)
		})
		assert.False(t, hasMsg(restricted, "netapp_volume_join_miss"))
		assert.True(t, hasMsg(unrooted, "netapp_volume_join_miss"))
	})
}

// An aggregate a claim reaches ONLY through phase 2 votes for its owning
// controller over the one or two volumes that claim's token matched, not over
// all of its own. It is never drawn — it is not a root and the unit that
// reached it does not intersect the rooted ids — and that is what keeps the
// partial vote out of the body. The invariant lives in pkg/graph, so pin it
// here: widening resolveStorageRoots would start drawing a controller chosen
// from a partial vote.
func TestRootedVolumeLabels_PhaseTwoOnlyAggregateIsNeverDrawn(t *testing.T) {
	// The claim's token matches trident_pvc_orders on aggr9 (rooted) and the
	// clone on aggr0, whose OTHER volume names a lexically-smaller controller
	// that only an unrestricted read sees.
	fx := vlrEstate(vlrHarvest([]vlrVol{
		{"trident_pvc_orders", "ontap-prod", "ontap-prod-09", "aggr9", "svm_shop", 300},
		{"snap_trident_pvc_orders", "ontap-prod", "ontap-prod-05", "aggr0", "svm_shop", 50},
		{"prod_unrelated_1", "ontap-prod", "ontap-prod-01", "aggr0", "svm_shop", 7},
	}))
	scope := vlrScope(t, nil, nil, []string{"aggr9"}, nil, nil)

	gr, _ := vlrBuild(t, fx, scope.Roots, Options{}, true)
	gu, _ := vlrBuild(t, fx, scope.Roots, Options{}, false)

	// The partial vote really does differ in the BUILT graph...
	assert.Equal(t, "ontap-prod-05", gr.NodesByID["netapp/ontap-prod/aggr/aggr0"].Labels()["node"],
		"phase 2 saw only the clone, so aggr0's owner vote is partial")
	assert.Equal(t, "ontap-prod-01", gu.NodesByID["netapp/ontap-prod/aggr/aggr0"].Labels()["node"],
		"the whole-filer read votes over every volume of aggr0")

	// ...and is invisible in the body, because aggr0 is neither a root nor on
	// a retained unit.
	br, bu := vlrBody(t, gr, scope), vlrBody(t, gu, scope)
	assert.JSONEq(t, planBodyJSON(t, bu), planBodyJSON(t, br))
	assert.NotContains(t, vlrIDs(br), "netapp/ontap-prod/aggr/aggr0")
	assert.NotContains(t, vlrIDs(br), "netapp/ontap-prod/ontap-prod-05")
}

// A cluster-only restriction read those filers WHOLE, so phase 2 has only the
// cross-filer candidates left to find. Excluding them keeps it from re-scanning
// the family it already read with a regex no index can serve.
func TestRootedVolumeLabels_ClusterOnlyPhaseTwoExcludesWhatPhaseOneRead(t *testing.T) {
	fx := vlrEstate(vlrHarvest([]vlrVol{
		{"trident_pvc_orders", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
		{"trident_pvc_orders", "ontap-lab", "ontap-lab-07", "aggr7", "svm_lab", 90},
		{"trident_pvc_redis", "ontap-prod", "ontap-prod-02", "aggr2", "svm_platform", 100},
	}))

	t.Run("cluster only excludes the filer phase 1 read whole", func(t *testing.T) {
		scope := vlrScope(t, []string{"ontap-prod"}, nil, nil, nil, nil)
		restricted, _, _ := vlrParity(t, fx, scope, Options{})
		var token string
		for _, q := range restricted.QueriesFor(promql.QVolumeLabels) {
			if strings.Contains(q, "volume=") {
				token = q
			}
		}
		require.NotEmpty(t, token, "phase 2 still runs — a candidate may be on another filer")
		assert.Contains(t, token, `cluster!~"ontap-prod"`,
			"every candidate on ontap-prod is already in hand from phase 1")
	})

	t.Run("an aggregate root must NOT exclude its cluster", func(t *testing.T) {
		// Phase 1 read only part of ontap-prod, so a candidate on another of
		// its aggregates is exactly what phase 2 has to find.
		scope := vlrScope(t, []string{"ontap-prod"}, nil, []string{"aggr1"}, nil, nil)
		restricted, _, _ := vlrParity(t, fx, scope, Options{})
		for _, q := range restricted.QueriesFor(promql.QVolumeLabels) {
			if strings.Contains(q, "volume=") {
				assert.NotContains(t, q, "cluster!~",
					"excluding the rooted cluster here would hide a same-cluster candidate")
			}
		}
	})
}
