package build

import (
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

// ------------------------------------------------------- 2. candidate extraction

func TestPVCandidates(t *testing.T) {
	cases := []struct {
		name    string
		volumes []string
		want    []string
	}{
		{"trident storage prefix", []string{"trident_pvc_ab12_cd34"}, []string{"pvc-ab12-cd34"}},
		{"empty storage prefix", []string{"pvc_ab12_cd34"}, []string{"pvc-ab12-cd34"}},
		{"every pvc_ boundary is a candidate: a superset", []string{"x_pvc_pool_pvc_ab12"}, []string{"pvc-ab12", "pvc-pool-pvc-ab12"}},
		{"a clone yields a candidate naming no PV", []string{"trident_pvc_ab12_clone"}, []string{"pvc-ab12-clone"}},
		{"an aggregate root volume yields nothing", []string{"vol0"}, nil},
		{"an SVM root volume yields nothing", []string{"svm_shop_root"}, nil},
		{"case-sensitive: a PV name is DNS-1123", []string{"trident_PVC_ab12"}, nil},
		{"pvc_ inside a word is not a boundary", []string{"xpvc_ab12", "trident_mypvc_ab12"}, nil},
		{"sorted and de-duplicated across volumes", []string{"b_pvc_2", "a_pvc_1", "pvc_1"}, []string{"pvc-1", "pvc-2"}},
		{"empty input", nil, nil},
		{"empty names", []string{"", ""}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pvCandidates(tc.volumes)
			if len(tc.want) == 0 {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestClaimTargetsAreClaimScopedQueries(t *testing.T) {
	var v topologyVectors
	targets := claimTargets(&v)
	got := make([]promql.Query, 0, len(targets))
	for _, tg := range targets {
		got = append(got, tg.query)
	}
	assert.Equal(t, promql.ClaimScopedQueries, got)
}

// ------------------------------------------------------------- hub estate

// hubClaim is one claim of a hub estate: bound to pv, mounted by pods, in
// cluster c1 of zone-a / prod unless az overrides it.
type hubClaim struct {
	ns, claim, pv string
	pods          []string
	az            string
}

// hubEstate renders a Kubernetes side over the given claims — pod info, the
// bindings, claim info and kubelet usage — and the Harvest side over vols.
// Every pod runs on worker-1 and has no controller owner.
func hubEstate(vols []vlrVol, claims []hubClaim) map[promql.Query]model.Vector {
	out := vlrHarvest(vols)
	ksm := func(az string, pairs ...string) *model.Sample {
		s := planKSM(pairs...)
		if az != "" {
			s.Metric["az"] = model.LabelValue(az)
		}
		return s
	}
	nodes := map[string]bool{}
	for _, c := range claims {
		out[promql.QPVCInfo] = append(out[promql.QPVCInfo],
			ksm(c.az, "namespace", c.ns, "persistentvolumeclaim", c.claim, "volumename", c.pv, "storageclass", "netapp-nas"))
		out[promql.QKubeletVolumeUsedBytes] = append(out[promql.QKubeletVolumeUsedBytes],
			withValue(ksm(c.az, "namespace", c.ns, "persistentvolumeclaim", c.claim), 1024))
		out[promql.QKubeletVolumeCapacityBytes] = append(out[promql.QKubeletVolumeCapacityBytes],
			withValue(ksm(c.az, "namespace", c.ns, "persistentvolumeclaim", c.claim), 4096))
		for _, p := range c.pods {
			out[promql.QPVCBindings] = append(out[promql.QPVCBindings],
				ksm(c.az, "namespace", c.ns, "pod", p, "persistentvolumeclaim", c.claim, "volume", "data"))
			out[promql.QPodInfo] = append(out[promql.QPodInfo],
				ksm(c.az, "namespace", c.ns, "pod", p, "uid", "uid-"+c.az+"-"+c.ns+"-"+p, "node", "worker-1"))
		}
		if !nodes[c.az] {
			nodes[c.az] = true
			out[promql.QNodeInfo] = append(out[promql.QNodeInfo], ksm(c.az, "node", "worker-1"))
		}
	}
	return out
}

// hubBase: orders-data on aggr1 / svm_shop (two mounters), redis-data on
// aggr2 / svm_platform, and two root volumes on aggr1 that name no claim.
func hubBase() ([]vlrVol, []hubClaim) {
	return []vlrVol{
		{"trident_pvc_ab12_cd34", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
		{"trident_pvc_ef56", "ontap-prod", "ontap-prod-02", "aggr2", "svm_platform", 100},
		{"vol0", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 0},
		{"svm_shop_root", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 0},
	}, []hubClaim{
		{ns: "shop", claim: "orders-data", pv: "pvc-ab12-cd34", pods: []string{"orders-0", "orders-1"}},
		{ns: "platform", claim: "redis-data", pv: "pvc-ef56", pods: []string{"redis-0"}},
	}
}

func hubBuild(t *testing.T, fx map[promql.Query]model.Vector, scope graph.StorageScope, opts Options) (*graph.Graph, *promqlfake.Querier, error) {
	t.Helper()
	q := promqlfake.New(fx)
	g, err := New(q, opts, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	return g, q, err
}

// ------------------------------------------------------- 5. claim-keyed reads

// Spec: "Rooted volumes name their claims".
func TestVolumeHub_RootedVolumesNameTheirClaims(t *testing.T) {
	vols, claims := hubBase()
	scope := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)
	g, q, err := hubBuild(t, hubEstate(vols, claims), scope, Options{})
	require.NoError(t, err)

	assert.Equal(t,
		[]string{`last_over_time(kube_persistentvolumeclaim_info{volumename="pvc-ab12-cd34"}[1m])`},
		q.QueriesFor(promql.QPVCInfo),
		"one query, restricted to the one candidate the rooted rows yield — and no az / env matcher")
	for _, fam := range promql.ClaimScopedQueries[1:] {
		assert.Equal(t, [][]string{{"orders-data"}}, q.ScopeValues(fam, promql.ClaimLabel),
			"%s is restricted to the claim the claim-info read returned", fam)
	}
	assert.Equal(t, [][]string{{"orders-0", "orders-1"}}, q.ScopeValues(promql.QPodInfo, promql.PodLabel))

	ids := vlrIDs(vlrBody(t, g, scope))
	assert.True(t, ids["zone-a-prod-c1/uid--shop-orders-0"], "the claim's complete path is drawn")
	assert.True(t, ids["zone-a-prod-c1/shop/orders-data"])
	assert.False(t, ids["zone-a-prod-c1/platform/redis-data"], "a claim off the rooted aggregate is never loaded")
	assert.NotContains(t, g.NodesByID, "zone-a-prod-c1/platform/redis-data")
}

// Spec: "A same-named claim in another namespace is filtered out".
func TestVolumeHub_SameNamedClaimInAnotherNamespaceIsFiltered(t *testing.T) {
	fx := hubEstate([]vlrVol{
		{"trident_pvc_ab12", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
		{"trident_pvc_zz99", "ontap-prod", "ontap-prod-02", "aggr2", "svm_platform", 100},
	}, []hubClaim{
		{ns: "shop", claim: "data", pv: "pvc-ab12", pods: []string{"app-0"}},
		{ns: "platform", claim: "data", pv: "pvc-zz99", pods: []string{"web-0"}},
	})
	scope := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)
	g, q, err := hubBuild(t, fx, scope, Options{})
	require.NoError(t, err)

	assert.Equal(t, [][]string{{"data"}}, q.ScopeValues(promql.QPVCBindings, promql.ClaimLabel),
		"the name-scoped binding read returns both claims called data")
	assert.Equal(t, [][]string{{"app-0"}}, q.ScopeValues(promql.QPodInfo, promql.PodLabel),
		"platform/data's binding is discarded before the pod scope is computed")
	assert.Contains(t, g.NodesByID, "zone-a-prod-c1/shop/data")
	assert.NotContains(t, g.NodesByID, "zone-a-prod-c1/platform/data", "no PVC node is built for it")

	topo, err := readTopology(t.Context(), promqlfake.New(fx), time.Minute, vlrEnd, Options{}, promql.Selector{},
		storagePlan(scope.Roots))
	require.NoError(t, err)
	assert.Equal(t, 1, topo.RawSeriesCount[string(promql.QPVCBindings)], "the tally counts the rows the reader kept")
	assert.Equal(t, 1, topo.RawSeriesCount[string(promql.QKubeletVolumeUsedBytes)])
}

// Spec: "No candidate is reported".
func TestVolumeHub_NoCandidateIssuesNoClaimQuery(t *testing.T) {
	fx := hubEstate([]vlrVol{
		{"shop_orders_01", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
		{"vol0", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 0},
	}, []hubClaim{{ns: "shop", claim: "orders-data", pv: "pvc-ab12", pods: []string{"orders-0"}}})
	scope := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)

	var (
		g   *graph.Graph
		q   *promqlfake.Querier
		err error
	)
	recs := captureDebugRecords(t, func() { g, q, err = hubBuild(t, fx, scope, Options{}) })
	require.NoError(t, err)

	for _, fam := range append(promql.ClaimScopedQueries, promql.QPodInfo, promql.QPodOwner, promql.QNodeInfo) {
		assert.Empty(t, q.QueriesFor(fam), "%s: no candidate, so nothing downstream is issued", fam)
	}
	assert.True(t, vlrIDs(vlrBody(t, g, scope))["netapp/ontap-prod/aggr/aggr1"], "the root is still drawn")
	miss := claimMisses(recs)
	require.Len(t, miss, 1)
	assert.Equal(t, "no_pv_candidate", miss[0]["reason"])
	assert.InDelta(t, 2.0, miss[0]["volumes"], 1e-9)
}

// Spec: "Candidates naming no claim are reported".
func TestVolumeHub_CandidatesNamingNoClaim(t *testing.T) {
	fx := hubEstate([]vlrVol{
		{"trident_pvc_gone", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
	}, []hubClaim{{ns: "shop", claim: "orders-data", pv: "pvc-ab12", pods: []string{"orders-0"}}})
	scope := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)

	var q *promqlfake.Querier
	var err error
	recs := captureDebugRecords(t, func() { _, q, err = hubBuild(t, fx, scope, Options{}) })
	require.NoError(t, err)

	assert.Len(t, q.QueriesFor(promql.QPVCInfo), 1)
	for _, fam := range append(promql.ClaimScopedQueries[1:], promql.QPodInfo, promql.QNodeInfo) {
		assert.Empty(t, q.QueriesFor(fam), "%s: no claim, so nothing downstream is issued", fam)
	}
	miss := claimMisses(recs)
	require.Len(t, miss, 1)
	assert.Equal(t, "no_claim", miss[0]["reason"])
	assert.Equal(t, "WARN", miss[0]["level"])
	assert.InDelta(t, 1.0, miss[0]["candidates"], 1e-9)

	t.Run("a namespace filter excluding every claim is not a hub miss", func(t *testing.T) {
		vols, claims := hubBase()
		sel := promql.Selector{AZ: vlrSel.AZ, Env: vlrSel.Env, Namespace: []string{"elsewhere"}}
		recs := captureDebugRecords(t, func() {
			_, err := New(promqlfake.New(hubEstate(vols, claims)), Options{}, nil, nil).
				BuildStorage(t.Context(), time.Minute, vlrEnd, sel, scope.Roots)
			require.NoError(t, err)
		})
		miss := claimMisses(recs)
		require.Len(t, miss, 1)
		assert.Equal(t, "DEBUG", miss[0]["level"], "the filter's ordinary outcome stays out of Warn")
	})
}

// Spec: "Root volumes produce no candidate" — and no coverage warning on their
// account while a claim volume beside them yields one.
func TestVolumeHub_RootVolumesDoNotWarn(t *testing.T) {
	vols, claims := hubBase()
	scope := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)
	recs := captureDebugRecords(t, func() {
		_, _, err := hubBuild(t, hubEstate(vols, claims), scope, Options{})
		require.NoError(t, err)
	})
	assert.Empty(t, claimMisses(recs))

	var summary map[string]any
	for _, r := range recs {
		if r["msg"] == "storage graph volume hub" {
			summary = r
		}
	}
	require.NotNil(t, summary, "every hub build logs its Debug summary")
	assert.InDelta(t, 3.0, summary["volumes"], 1e-9, "trident_pvc_ab12_cd34, vol0, svm_shop_root")
	assert.InDelta(t, 1.0, summary["candidates"], 1e-9)
	assert.InDelta(t, 1.0, summary["claims"], 1e-9)
	assert.InDelta(t, 2.0, summary["bindings"], 1e-9)
}

func claimMisses(recs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["msg"] == "storage_root_claim_miss" {
			out = append(out, r)
		}
	}
	return out
}

// Every claim-keyed read fails the build on a query error — including the two
// kubelet families, which /v1/graph degrades — and a failed claim-info read
// neither blocks the waves downstream of it nor loses its family name.
func TestVolumeHub_ClaimReadFailuresFailTheBuild(t *testing.T) {
	vols, claims := hubBase()
	scope := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)
	boom := errors.New("upstream said no")
	for _, fam := range promql.ClaimScopedQueries {
		t.Run(string(fam), func(t *testing.T) {
			q := promqlfake.New(hubEstate(vols, claims))
			q.Fail = func(name, _ string) error {
				if name == string(fam) {
					return boom
				}
				return nil
			}
			_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
			require.Error(t, err)
			be, ok := errors.AsType[*Error](err)
			require.True(t, ok)
			assert.Equal(t, ReasonUpstream, be.Reason)
			assert.Equal(t, string(fam), be.Query)
		})
	}
}

// Spec: "A large claim scope falls back to a filtered read". Twenty claims on
// the rooted aggregate and a budget small enough to put every name in a chunk
// of its own take every claim family past maxHubClaimChunks; each is then read
// once with no scope and filtered to the same rows, and the body is the
// chunked build's byte for byte.
func TestVolumeHub_UnboundedClaimScopeReadsWideAndFilters(t *testing.T) {
	vols := make([]vlrVol, 0, maxHubClaimChunks+5)
	claims := make([]hubClaim, 0, maxHubClaimChunks+5)
	for i := range maxHubClaimChunks + 4 {
		id := fmt.Sprintf("c%02d", i)
		vols = append(vols, vlrVol{"trident_pvc_" + id, "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", float64(i)})
		claims = append(claims, hubClaim{ns: "shop", claim: "data-" + id, pv: "pvc-" + id, pods: []string{"pod-" + id}})
	}
	// Off the rooted aggregate: kept out by the filter of the wide read.
	vols = append(vols, vlrVol{"trident_pvc_off", "ontap-prod", "ontap-prod-02", "aggr2", "svm_shop", 1})
	claims = append(claims, hubClaim{ns: "shop", claim: "data-off", pv: "pvc-off", pods: []string{"pod-off"}})
	fx := hubEstate(vols, claims)
	scope := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)

	wideG, wideQ, err := hubBuild(t, fx, scope, Options{QoSScopeBatchBytes: 6})
	require.NoError(t, err)
	chunkedG, chunkedQ, err := hubBuild(t, fx, scope, Options{})
	require.NoError(t, err)

	for _, fam := range promql.ClaimScopedQueries {
		assert.Equal(t, []string{promql.Render(fam, time.Minute, promql.LabelKeys{}, promql.Selector{})},
			wideQ.QueriesFor(fam), "%s: one unscoped query — no scope and no az / env matcher", fam)
		assert.Len(t, chunkedQ.QueriesFor(fam), 1, "%s: the roomy budget fits the scope in one chunk", fam)
		assert.NotEqual(t, wideQ.QueriesFor(fam), chunkedQ.QueriesFor(fam))
	}
	assert.NotContains(t, wideG.NodesByID, "zone-a-prod-c1/shop/data-off", "the reader filter keeps what the scope would")
	assert.JSONEq(t, planBodyJSON(t, vlrBody(t, chunkedG, scope)), planBodyJSON(t, vlrBody(t, wideG, scope)))

	wideTopo, err := readTopology(t.Context(), promqlfake.New(fx), time.Minute, vlrEnd,
		Options{QoSScopeBatchBytes: 6}, promql.Selector{}, storagePlan(scope.Roots))
	require.NoError(t, err)
	chunkedTopo, err := readTopology(t.Context(), promqlfake.New(fx), time.Minute, vlrEnd,
		Options{}, promql.Selector{}, storagePlan(scope.Roots))
	require.NoError(t, err)
	for _, fam := range promql.ClaimScopedQueries {
		want := maxHubClaimChunks + 4
		if fam == promql.QPVCAnnotations {
			want = 0 // issued, and the estate annotates no claim
		}
		n, ok := wideTopo.RawSeriesCount[string(fam)]
		assert.True(t, ok, "%s: issued, so tallied", fam)
		assert.Equal(t, want, n, "%s: the tally is the filtered count", fam)
		assert.Equal(t, chunkedTopo.RawSeriesCount[string(fam)], n, "%s: the same in either read", fam)
	}
}

// The claim families are withheld from the first wave in hub mode, and only
// in hub mode: a build whose phase 1 fell back — or that never qualified —
// issues them first-wave under the request's own matchers.
func TestVolumeHub_ClaimFamiliesLeaveTheFirstWaveOnlyInHubMode(t *testing.T) {
	suffix := defaultVolumeKeyRewriter()
	hub := storagePlan(vlrRoots(t, nil, nil, []string{"aggr1"}, nil, nil)).
		resolveVolumeLabelRead(suffix, time.Minute, DefaultQoSScopeBatchBytes)
	require.True(t, hub.hub)
	unbounded := storagePlan(vlrRoots(t, nil, nil, manyAggrRoots(5000, 240), nil, nil)).
		resolveVolumeLabelRead(suffix, time.Minute, DefaultQoSScopeBatchBytes)
	require.False(t, unbounded.hub)
	require.True(t, unbounded.phaseOneUnbounded)
	podOnly := storagePlan(vlrRoots(t, nil, nil, nil, nil, []string{"shop/orders-0"})).
		resolveVolumeLabelRead(suffix, time.Minute, DefaultQoSScopeBatchBytes)
	require.False(t, podOnly.hub)
	full := fullPlan.resolveVolumeLabelRead(suffix, time.Minute, DefaultQoSScopeBatchBytes)
	require.False(t, full.hub)

	for _, qy := range promql.ClaimScopedQueries {
		assert.False(t, hub.issuesFirstWave(qy), "%s: read by reference in hub mode", qy)
		assert.True(t, unbounded.issuesFirstWave(qy), "%s: a fallen-back build is today's build", qy)
		assert.True(t, podOnly.issuesFirstWave(qy), "%s", qy)
		assert.True(t, full.issuesFirstWave(qy), "%s", qy)
	}
	assert.True(t, hub.issuesFirstWave(promql.QVolumeLabels), "phase 1 IS a first-wave leg")
	assert.True(t, hub.issuesFirstWave(promql.QAlerts))

	t.Run("a fallen-back build keeps the request's own matchers", func(t *testing.T) {
		vols, claims := hubBase()
		_, q, err := hubBuild(t, hubEstate(vols, claims),
			vlrScope(t, nil, nil, append(manyAggrRoots(5000, 240), "aggr1"), nil, nil), Options{})
		require.NoError(t, err)
		assert.Equal(t, []string{vlrBare}, q.QueriesFor(promql.QVolumeLabels))
		assert.Equal(t,
			[]string{promql.Render(promql.QPVCInfo, time.Minute, promql.LabelKeys{}, vlrSel)},
			q.QueriesFor(promql.QPVCInfo), "whole-zone, first wave, az and env included")
	})
}

// A failed claim-info read closes every done-channel downstream of it, so the
// pod, application and QoS waves compute empty scopes instead of blocking.
// Run under -race: the tail's merge must never overlap the claim-info read.
func TestVolumeHub_FailedClaimInfoDoesNotBlock(t *testing.T) {
	vols, claims := hubBase()
	scope, err := graph.NewStorageScope(nil, nil, nil, nil, []string{"aggr1"}, []string{"svm_shop"}, nil, []string{"beta"})
	require.NoError(t, err)
	for range 20 {
		q := promqlfake.New(hubEstate(vols, claims))
		q.Fail = func(name, _ string) error {
			if name == string(promql.QPVCInfo) {
				return errors.New("boom")
			}
			return nil
		}
		done := make(chan error, 1)
		go func() {
			_, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
			done <- err
		}()
		select {
		case err := <-done:
			require.Error(t, err)
			assert.Contains(t, err.Error(), string(promql.QPVCInfo))
		case <-time.After(5 * time.Second):
			t.Fatal("a failed claim-info read must not leave a wave waiting forever")
		}
	}
}

// Spec: "A statically provisioned volume is not reached from a storage root".
func TestVolumeHub_StaticPVIsNotReachedFromAStorageRoot(t *testing.T) {
	fx := hubEstate([]vlrVol{
		{"mongo_data_01", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
		{"trident_pvc_ab12", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 100},
	}, []hubClaim{
		{ns: "db", claim: "mongo-data", pv: "mongo-data-01", pods: []string{"mongo-0"}},
		{ns: "shop", claim: "orders-data", pv: "pvc-ab12", pods: []string{"orders-0"}},
	})
	scope := vlrScope(t, nil, nil, []string{"aggr1"}, nil, nil)
	g, q, err := hubBuild(t, fx, scope, Options{})
	require.NoError(t, err)

	assert.Equal(t, [][]string{{"pvc-ab12"}}, q.ScopeValues(promql.QPVCInfo, promql.VolumeNameLabel),
		"mongo_data_01 embeds no pvc_, so no candidate names mongo-data-01")
	ids := vlrIDs(vlrBody(t, g, scope))
	assert.False(t, ids["zone-a-prod-c1/db/mongo-data"], "the hub draws no path through the static PV")
	assert.True(t, ids["zone-a-prod-c1/shop/orders-data"])

	// /v1/graph still joins it: the forward derivation is unchanged.
	full, err := New(promqlfake.New(fx), Options{}, nil, nil).Build(t.Context(), time.Minute, vlrEnd, vlrSel)
	require.NoError(t, err)
	var joined bool
	for _, e := range full.Edges {
		if e.Type == graph.EdgeTypePVCToNetAppAggr && e.Source == "zone-a-prod-c1/db/mongo-data" {
			joined = true
			assert.Equal(t, graph.NetAppAggrID("ontap-prod", "aggr1"), e.Target)
		}
	}
	assert.True(t, joined, "GET /v1/graph still draws the static PV's pvc-to-netapp-aggr edge")
}

// Owner completion and phase 2 are merged AFTER the claim read took its
// candidates, so a claim on another SVM of an aggregate the SVM root touched —
// on no rooted component — is never loaded.
func TestVolumeHub_CompletionRowsAreNeverACandidateSource(t *testing.T) {
	fx := hubEstate([]vlrVol{
		{"trident_pvc_shop", "ontap-prod", "ontap-prod-01", "aggr3", "svm_shop", 300},
		{"trident_pvc_other", "ontap-prod", "ontap-prod-01", "aggr3", "svm_other", 100},
	}, []hubClaim{
		{ns: "shop", claim: "shop-data", pv: "pvc-shop", pods: []string{"shop-0"}},
		{ns: "other", claim: "other-data", pv: "pvc-other", pods: []string{"other-0"}},
	})
	scope := vlrScope(t, nil, nil, nil, []string{"svm_shop"}, nil)
	g, q, err := hubBuild(t, fx, scope, Options{})
	require.NoError(t, err)

	var completion []string
	for _, qy := range q.QueriesFor(promql.QVolumeLabels) {
		if strings.Contains(qy, `aggr="aggr3"`) {
			completion = append(completion, qy)
		}
	}
	assert.Equal(t, []string{`last_over_time(volume_labels{cluster="ontap-prod",aggr="aggr3"}[1m])`}, completion,
		"owner completion re-reads the touched aggregate whole")
	assert.Equal(t, [][]string{{"pvc-shop"}}, q.ScopeValues(promql.QPVCInfo, promql.VolumeNameLabel),
		"trident_pvc_other came back through completion, never a candidate")
	assert.NotContains(t, g.NodesByID, "zone-a-prod-c1/other/other-data")
	_, _, _ = vlrParity(t, fx, scope, Options{})
}

// Task 8.1: a body built in hub mode is the pre-change body whenever every
// joined PV is named pvc-…. vlrParity's control clears the storage roots from
// the READ plan only, so it reads the whole filer and every claim family
// first-wave under az / env, exactly as a build did before the hub existed,
// while both bodies are projected over the full request scope. planEstate
// carries every controller kind, a shared (RWX) claim, PVC Application
// inheritance and an application root's own-annotated claim.
func TestVolumeHub_ParityWithThePreChangeRead(t *testing.T) {
	scope := func(ontap, nodes, aggrs, svms, pods, apps []string) graph.StorageScope {
		s, err := graph.NewStorageScope(nil, nil, ontap, nodes, aggrs, svms, pods, apps)
		require.NoError(t, err)
		return s
	}
	cases := map[string]graph.StorageScope{
		"ontap_cluster":                          scope([]string{"ontap-prod"}, nil, nil, nil, nil, nil),
		"aggr":                                   scope(nil, nil, []string{"aggr1"}, nil, nil, nil),
		"svm":                                    scope(nil, nil, nil, []string{"svm_shop"}, nil, nil),
		"aggr and svm":                           scope(nil, nil, []string{"aggr1"}, []string{"svm_platform"}, nil, nil),
		"aggr and a Kubernetes node":             scope(nil, []string{"worker-1"}, []string{"aggr1"}, nil, nil, nil),
		"aggr and an ONTAP controller":           scope(nil, []string{"ontap-prod-01"}, []string{"aggr1"}, nil, nil, nil),
		"aggr and pod":                           scope(nil, nil, []string{"aggr1"}, nil, []string{"shop/orders-0"}, nil),
		"aggr and application":                   scope(nil, nil, []string{"aggr1"}, nil, nil, []string{"beta"}),
		"svm, application and a Kubernetes node": scope(nil, []string{"worker-1"}, nil, []string{"svm_shop"}, nil, []string{"beta"}),
	}
	for name, sc := range cases {
		t.Run(name, func(t *testing.T) {
			hub, pre, body := vlrParity(t, planEstate(), sc, Options{})
			require.NotEmpty(t, vlrPaths(body), "a body with no path would prove nothing about the claim chain")

			assert.NotEmpty(t, hub.ScopeValues(promql.QPVCInfo, promql.VolumeNameLabel), "the hub build read claims by PV name")
			for _, qy := range hub.QueriesFor(promql.QPVCInfo) {
				assert.NotContains(t, qy, `az=`, "hub mode renders no az matcher")
			}
			assert.Equal(t,
				[]string{promql.Render(promql.QPVCInfo, time.Minute, promql.LabelKeys{}, vlrSel)},
				pre.QueriesFor(promql.QPVCInfo), "the control read claims whole-zone")
		})
	}
}

// vlrPaths returns the storage-flow edges of a body.
func vlrPaths(body cytoscape.Body) []string {
	var out []string
	for _, e := range body.Elements.Edges {
		if e.Data.Type == string(graph.EdgeTypeStorageFlow) {
			out = append(out, e.Data.ID)
		}
	}
	return out
}
