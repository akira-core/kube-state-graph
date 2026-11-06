package build

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

const offsetShape = "] offset "

func deltaLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, offsetShape) {
			out = append(out, l)
		}
	}
	return out
}

func withoutLines(lines, drop []string) []string {
	rm := map[string]int{}
	for _, d := range drop {
		rm[d]++
	}
	var out []string
	for _, l := range lines {
		if rm[l] > 0 {
			rm[l]--
			continue
		}
		out = append(out, l)
	}
	return out
}

// expectedDeltaLines renders what io_delta=true adds for scope chunks, in the
// allFanOut `name|query` form, sorted like it.
func expectedDeltaLines(t *testing.T, chunks [][]string) []string {
	t.Helper()
	out := make([]string, 0, len(chunks)*len(promql.QoSWorkloadQueries))
	for _, family := range promql.QoSWorkloadQueries {
		for _, chunk := range chunks {
			rendered, ok := promql.RenderQoSVolumeScopedDelta(family, time.Minute, promql.LabelKeys{}, promql.Selector{}, chunk)
			require.True(t, ok)
			out = append(out, string(family)+"|"+rendered)
		}
	}
	return sortedCopy(out)
}

// Spec: "The queries it adds". io_delta=true issues every query the plain build
// issues, plus one delta query per family per chunk of the same volume scope.
func TestBuildStorageWith_IODeltaAddsOnlyDeltaQueries(t *testing.T) {
	base := promqlfake.New(planEstate())
	_, err := New(base, Options{}, nil, nil).BuildStorageWith(
		t.Context(), time.Minute, vlrEnd, vlrSel, storagePodRoot(t), StorageOptions{})
	require.NoError(t, err)
	plain := allFanOut(base)
	scope := base.ScopeValues(promql.QQoSReadOps, "volume")
	require.Len(t, scope, 1, "the planEstate scope fits one measurement chunk")

	q := promqlfake.New(planEstate())
	_, err = New(q, Options{}, nil, nil).BuildStorageWith(
		t.Context(), time.Minute, vlrEnd, vlrSel, storagePodRoot(t), StorageOptions{IODelta: true})
	require.NoError(t, err)
	withDelta := allFanOut(q)

	added := deltaLines(withDelta)
	assert.Equal(t, expectedDeltaLines(t, scope), added,
		"one delta query per family over the measurement's own volume scope")
	assert.Len(t, added, 6)
	assert.Equal(t, plain, withoutLines(withDelta, added),
		"every other query is byte-identical to the io_delta=false build")
	assert.Empty(t, deltaLines(plain))
}

// Spec: "The form is the same under every io_rollup".
func TestBuildStorageWith_IODeltaIsIndependentOfTheRollup(t *testing.T) {
	run := func(roll promql.Rollup) []string {
		q := promqlfake.New(planEstate())
		_, err := New(q, Options{}, nil, nil).BuildStorageWith(
			t.Context(), time.Minute, vlrEnd, vlrSel, storagePodRoot(t),
			StorageOptions{IORollup: roll, IODelta: true})
		require.NoError(t, err)
		return allFanOut(q)
	}
	last := run(promql.RollupLast)
	maxed := run(promql.RollupMax)

	assert.Equal(t, deltaLines(last), deltaLines(maxed), "the delta form never follows the rollup")
	assert.Equal(t, substituteQoSFunc(withoutLines(last, deltaLines(last)), "max_over_time"),
		withoutLines(maxed, deltaLines(maxed)),
		"the measurement legs keep their max_over_time form beside the delta")
	for _, l := range deltaLines(maxed) {
		assert.NotContains(t, l, "max_over_time")
	}
}

// Spec: "The delta read is chunked within the byte budget". A scope that fits
// one measurement chunk but not one delta chunk splits into two delta chunks
// (budget halved), merged in chunk order, identical to an unchunked read.
func TestReadScopedQoS_DeltaChunksUseHalfTheBudget(t *testing.T) {
	build := func() *scopeFake {
		f := newScopeFake()
		f.claims = model.Vector{claimSample("db", "a", "pvc-aaaaaaaa"), claimSample("db", "b", "pvc-bbbbbbbb")}
		f.volumes = model.Vector{volSample("trident_pvc_aaaaaaaa", "aggr1"), volSample("trident_pvc_bbbbbbbb", "aggr1")}
		f.readOps = map[string]float64{"trident_pvc_aaaaaaaa": 10, "trident_pvc_bbbbbbbb": 20}
		return f
	}
	read := func(f *scopeFake, budget int, delta bool) topologyVectors {
		var v topologyVectors
		v.PVCInfo, v.VolumeLabels = f.claims, f.volumes
		done := make(chan struct{})
		close(done)
		mu := new(sync.Mutex)
		require.NoError(t, readScopedQoS(t.Context(), t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
			Options{QoSScopeBatchBytes: budget}, promql.Selector{}, &v, mu, true, "", delta, done, done))
		return v
	}
	deltaOf := func(f *scopeFake, family promql.Query) []string {
		var out []string
		for _, qs := range f.queriesFor(family) {
			if strings.Contains(qs, offsetShape) {
				out = append(out, qs)
			}
		}
		return out
	}

	// Two 20-byte names: 41 bytes fit one measurement chunk; the delta budget is 20.
	chunked := build()
	vChunked := read(chunked, 41, true)
	measured := len(chunked.queriesFor(promql.QQoSReadOps)) - len(deltaOf(chunked, promql.QQoSReadOps))
	assert.Equal(t, 1, measured, "the measurement scope is one chunk")
	require.Len(t, deltaOf(chunked, promql.QQoSReadOps), 2, "the delta scope is two chunks")
	assert.Contains(t, deltaOf(chunked, promql.QQoSReadOps)[0]+deltaOf(chunked, promql.QQoSReadOps)[1], "trident_pvc_aaaaaaaa")

	whole := build()
	vWhole := read(whole, 1<<20, true)
	require.Len(t, deltaOf(whole, promql.QQoSReadOps), 1)
	// The fake enumerates a map, so order inside one chunk is not meaningful;
	// across chunks it is, and is asserted below.
	assert.ElementsMatch(t, vWhole.QoSReadOpsDelta, vChunked.QoSReadOpsDelta,
		"the merged result is identical to a single unchunked read of the same scope")
	require.Len(t, vChunked.QoSReadOpsDelta, 2)
	assert.Equal(t, model.LabelValue("trident_pvc_aaaaaaaa"), vChunked.QoSReadOpsDelta[0].Metric["volume"],
		"chunk order, not completion order")

	// The per-family series tally keeps describing the measurement read.
	plain := build()
	vPlain := read(plain, 41, false)
	assert.Equal(t, vPlain.ScopeIssued, vChunked.ScopeIssued)
	assert.Equal(t, vPlain.ExtraSeriesCount, vChunked.ExtraSeriesCount)
	assert.Empty(t, deltaOf(plain, promql.QQoSReadOps))
	assert.Empty(t, vPlain.QoSReadOpsDelta)
	assert.ElementsMatch(t, vPlain.QoSReadOps, vChunked.QoSReadOps, "measurement vectors are untouched by the delta read")
}

// An empty scope issues no delta query.
func TestReadScopedQoS_EmptyScopeIssuesNoDeltaQuery(t *testing.T) {
	f := newScopeFake()
	f.claims = model.Vector{claimSample("db", "a", "pvc-aaaaaaaa")}
	// No volume_labels: nothing matched.
	var v topologyVectors
	v.PVCInfo = f.claims
	done := make(chan struct{})
	close(done)
	require.NoError(t, readScopedQoS(t.Context(), t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{}, &v, new(sync.Mutex), true, "", true, done, done))
	for _, family := range promql.QoSWorkloadQueries {
		assert.Empty(t, f.queriesFor(family), family)
	}
}

type deltaFailQuerier struct {
	inner  promql.Querier
	family string
}

func (d deltaFailQuerier) Instant(ctx context.Context, name, query string, at time.Time) (model.Vector, error) {
	if name == d.family && strings.Contains(query, offsetShape) {
		return nil, errors.New("upstream 5xx")
	}
	return d.inner.Instant(ctx, name, query, at)
}

// Spec: "A delta query failure fails the storage request".
func TestBuildStorageWith_FailedDeltaChunkFailsTheBuild(t *testing.T) {
	q := deltaFailQuerier{inner: promqlfake.New(planEstate()), family: string(promql.QQoSReadOps)}
	g, err := New(q, Options{}, nil, nil).BuildStorageWith(
		t.Context(), time.Minute, vlrEnd, vlrSel, storagePodRoot(t), StorageOptions{IODelta: true})
	require.Error(t, err)
	assert.Nil(t, g)
	qe, ok := errors.AsType[*QueryError](err)
	require.True(t, ok)
	assert.Equal(t, string(promql.QQoSReadOps), qe.Query)

	// Without io_delta the same querier never sees a delta query and the build passes.
	_, err = New(q, Options{}, nil, nil).BuildStorageWith(
		t.Context(), time.Minute, vlrEnd, vlrSel, storagePodRoot(t), StorageOptions{})
	require.NoError(t, err)
}

// Spec: "Other builds issue no delta read".
func TestBuild_AndBuildK8s_NeverIssueADeltaQuery(t *testing.T) {
	q := promqlfake.New(planEstate())
	_, err := New(q, Options{}, nil, nil).Build(t.Context(), time.Minute, vlrEnd, vlrSel)
	require.NoError(t, err)
	assert.Empty(t, deltaLines(allFanOut(q)), "/v1/graph")

	k := promqlfake.New(planEstate())
	_, err = New(k, Options{}, nil, nil).BuildK8s(t.Context(), time.Minute, vlrEnd, vlrSel,
		vlrScope(t, graph.StorageRootPod, []string{"shop/orders-0"}).Roots)
	require.NoError(t, err)
	assert.Empty(t, deltaLines(allFanOut(k)), "/v1/k8s-graph")
}

// Design D7: the delta reads are not series-tallied, so RawSeriesCount (and
// with it the build log's largest_leg) describes the measurement read and does
// not move with io_delta.
func TestReadTopology_IODeltaLeavesTheSeriesTallyUnchanged(t *testing.T) {
	read := func(delta bool) (Topology, []string) {
		q := promqlfake.New(planEstate())
		plan := storagePlan(storagePodRoot(t))
		plan.ioDelta = delta
		tp, err := readTopology(t.Context(), q, time.Minute, vlrEnd, Options{}, vlrSel, plan)
		require.NoError(t, err)
		return tp, deltaLines(allFanOut(q))
	}
	plain, plainDeltas := read(false)
	withDelta, deltas := read(true)

	assert.Empty(t, plainDeltas)
	require.Len(t, deltas, len(promql.QoSWorkloadQueries), "the delta reads ran")
	for _, family := range promql.QoSWorkloadQueries {
		require.Contains(t, plain.RawSeriesCount, string(family))
	}
	assert.Equal(t, plain.RawSeriesCount, withDelta.RawSeriesCount)
}
